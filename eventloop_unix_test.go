//go:build (linux || darwin || netbsd || freebsd || openbsd || dragonfly) && !stdio

package uio

import (
	"bytes"
	"errors"
	"io"
	"net"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urpc/uio/internal/poller"
)

// startEchoEvents serves an echo handler and returns the listening address.
func startEchoEvents(t *testing.T, events *Events) string {
	t.Helper()
	started := make(chan string, 1)
	funcs(events).Start = func(events *Events) {
		for _, listener := range events.acceptor.listeners {
			started <- listener.ln.Addr().String()
			return
		}
	}
	if funcs(events).Read == nil {
		funcs(events).Read = func(conn Conn) error {
			_, err := conn.WriteTo(conn)
			return err
		}
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- events.Serve("tcp://127.0.0.1:0") }()
	t.Cleanup(func() {
		_ = events.Close(nil)
		select {
		case err := <-serveDone:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(5 * time.Second):
			t.Error("Serve did not stop")
		}
	})
	select {
	case addr := <-started:
		return addr
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not start")
		return ""
	}
}

func readFull(conn net.Conn, buf []byte) (int, error) {
	total := 0
	for total < len(buf) {
		n, err := conn.Read(buf[total:])
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

// TestEchoWithSeveralLoops drives concurrent echo traffic through several
// loops, each waited on by several goroutines, then checks that Serve joins
// all of them on shutdown.
func TestEchoWithSeveralLoops(t *testing.T) {
	loopWaitersOverride = 2
	t.Cleanup(func() { loopWaitersOverride = 0 })

	events := &Events{Pollers: 3}
	addr := startEchoEvents(t, events)
	// Pollers is capped by GOMAXPROCS.
	if want := min(3, runtime.GOMAXPROCS(0)); len(events.loops) != want {
		t.Fatalf("loops = %d, want %d", len(events.loops), want)
	}

	const clients, rounds = 16, 200
	var wg sync.WaitGroup
	errs := make(chan error, clients)
	for c := 0; c < clients; c++ {
		wg.Add(1)
		go func(c int) {
			defer wg.Done()
			conn, err := net.DialTimeout("tcp", addr, time.Second)
			if err != nil {
				errs <- err
				return
			}
			defer conn.Close()
			_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
			message := bytes.Repeat([]byte{byte('a' + c)}, 512)
			reply := make([]byte, len(message))
			for r := 0; r < rounds; r++ {
				if _, err := conn.Write(message); err != nil {
					errs <- err
					return
				}
				if _, err := readFull(conn, reply); err != nil {
					errs <- err
					return
				}
				if !bytes.Equal(reply, message) {
					errs <- net.UnknownNetworkError("echo mismatch")
					return
				}
			}
		}(c)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}

// TestLoopIgnoresStaleTags feeds a loop an event whose tag belongs to an
// earlier registration of the same descriptor number. It must not schedule
// the current connection.
func TestLoopIgnoresStaleTags(t *testing.T) {
	if !poller.Tagged {
		t.Skip("the backend reports no registration tags")
	}
	events := &Events{Pollers: 1}
	opened := make(chan *fdConn, 1)
	funcs(events).Open = func(conn Conn) { opened <- conn.(*fdConn) }
	addr := startEchoEvents(t, events)

	client, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	var conn *fdConn
	select {
	case conn = <-opened:
	case <-time.After(5 * time.Second):
		t.Fatal("connection did not open")
	}
	deadline := time.Now().Add(5 * time.Second)
	for conn.taskState.Load()&taskScheduledBit != 0 || conn.pollTag.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("open turn did not finish")
		}
		time.Sleep(time.Millisecond)
	}

	loop := conn.loop
	waiter := loop.newWaiter()
	stale := poller.Event{FD: conn.Fd(), Events: poller.ReadEvents, Tag: conn.pollTag.Load() + 1}
	loop.dispatch(waiter, []poller.Event{stale})
	if len(waiter.ready) != 0 || conn.taskState.Load()&taskScheduledBit != 0 {
		t.Fatal("stale readiness scheduled the current connection")
	}

	current := stale
	current.Tag = conn.pollTag.Load()
	loop.dispatch(waiter, []poller.Event{current})
	if len(waiter.ready) != 1 || waiter.ready[0] != conn {
		t.Fatal("current readiness did not schedule the connection")
	}
	loop.submit(waiter)
}

// TestDialedConnectionRunsOpenBeforeRead holds each dialed connection after it
// joins its poller until a waiter has submitted its turn for the peer's first
// bytes, before registerConn schedules the open event. The turn must still run
// OnOpen before OnData.
func TestDialedConnectionRunsOpenBeforeRead(t *testing.T) {
	peer, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	go func() {
		for {
			conn, err := peer.Accept()
			if err != nil {
				return
			}
			_, _ = conn.Write([]byte("ping"))
			go func() {
				_, _ = io.Copy(io.Discard, conn)
				_ = conn.Close()
			}()
		}
	}()

	var held, reads, early atomic.Int32
	registeredForTest = func(conn *fdConn) {
		start := reads.Load()
		deadline := time.Now().Add(2 * time.Second)
		for conn.taskState.Load()&taskScheduledBit == 0 && reads.Load() == start {
			if time.Now().After(deadline) {
				return
			}
			time.Sleep(50 * time.Microsecond)
		}
		held.Add(1)
	}
	t.Cleanup(func() { registeredForTest = nil })

	started := make(chan struct{})
	events := withFuncs(&Events{Pollers: 2}, FuncHandler{Start: func(*Events) { close(started) }})
	funcs(events).Open = func(conn Conn) { conn.SetUserdata(true) }
	gotData := make(chan struct{}, 32)
	funcs(events).Read = func(conn Conn) error {
		if conn.Userdata() == nil {
			early.Add(1)
		}
		reads.Add(1)
		_, _ = conn.Discard(-1)
		gotData <- struct{}{}
		return conn.Close()
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- events.Serve() }()
	t.Cleanup(func() {
		_ = events.Close(nil)
		<-serveDone
	})
	<-started

	const dials = 20
	for i := 0; i < dials; i++ {
		if _, err := events.Dial("tcp://" + peer.Addr().String()); err != nil {
			t.Fatal(err)
		}
		select {
		case <-gotData:
		case <-time.After(5 * time.Second):
			t.Fatal("dialed connection read nothing")
		}
	}
	if held.Load() == 0 {
		t.Fatal("no waiter ever submitted a dialed connection before its open event")
	}
	if n := early.Load(); n != 0 {
		t.Fatalf("OnData ran before OnOpen on %d of %d connections", n, dials)
	}
}

// TestAcceptedOpenTurnReadsAlreadySentBytes pins the open turn of an accepted
// connection: OnOpen runs first, and whatever the peer sent before the
// connection was accepted is read in the same turn, although the socket
// already joined its poller and reported those bytes.
func TestAcceptedOpenTurnReadsAlreadySentBytes(t *testing.T) {
	opened := make(chan int64, 1)
	data := make(chan int64, 1)
	events := &Events{Pollers: 1}
	funcs(events).Open = func(Conn) { opened <- currentGoroutineID() }
	funcs(events).Read = func(conn Conn) error {
		select {
		case data <- currentGoroutineID():
		default:
		}
		_, err := conn.WriteTo(conn)
		return err
	}
	started := make(chan string, 1)
	accepting := make(chan struct{})
	funcs(events).Start = func(ev *Events) {
		for _, l := range ev.acceptor.listeners {
			started <- l.ln.Addr().String()
		}
		// Hold accepting back until the client's request is in the backlog.
		<-accepting
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- events.Serve("tcp://127.0.0.1:0") }()
	t.Cleanup(func() {
		_ = events.Close(nil)
		<-serveDone
	})
	client, err := net.DialTimeout("tcp", <-started, time.Second)
	if err != nil {
		close(accepting)
		t.Fatal(err)
	}
	defer client.Close()
	if _, err := client.Write([]byte("hello")); err != nil {
		close(accepting)
		t.Fatal(err)
	}
	close(accepting)
	var open, first int64
	select {
	case open = <-opened:
	case <-time.After(5 * time.Second):
		t.Fatal("OnOpen did not run")
	}
	select {
	case first = <-data:
	case <-time.After(5 * time.Second):
		t.Fatal("OnData did not run")
	}
	if open != first {
		t.Fatal("the first OnData ran in another turn than OnOpen")
	}
	_ = client.SetReadDeadline(time.Now().Add(5 * time.Second))
	reply := make([]byte, 5)
	if _, err := readFull(client, reply); err != nil || string(reply) != "hello" {
		t.Fatalf("echo = %q, %v", reply, err)
	}
	// Later bytes reach the connection through its poller.
	if _, err := client.Write([]byte("again")); err != nil {
		t.Fatal(err)
	}
	if _, err := readFull(client, reply); err != nil || string(reply) != "again" {
		t.Fatalf("second echo = %q, %v", reply, err)
	}
}

// TestCloseInOnOpenSendsQueuedOutput pins the close of a connection whose
// OnOpen writes and then closes it: the bytes leave, OnClose runs once, in the
// same turn, and the descriptor is closed.
func TestCloseInOnOpenSendsQueuedOutput(t *testing.T) {
	var closes atomic.Int32
	var openTurn, closeTurn atomic.Int64
	closed := make(chan *fdConn, 1)
	events := &Events{Pollers: 1}
	funcs(events).Open = func(conn Conn) {
		openTurn.Store(currentGoroutineID())
		_, _ = conn.Write([]byte("bye"))
		_ = conn.Close()
	}
	funcs(events).Close = func(conn Conn, _ error) {
		closeTurn.Store(currentGoroutineID())
		if closes.Add(1) == 1 {
			closed <- conn.(*fdConn)
		}
	}
	addr := startEchoEvents(t, events)
	client, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	_ = client.SetReadDeadline(time.Now().Add(5 * time.Second))
	reply, err := io.ReadAll(client)
	if err != nil || string(reply) != "bye" {
		t.Fatalf("peer read %q, %v; want the bytes then EOF", reply, err)
	}
	var conn *fdConn
	select {
	case conn = <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("OnClose did not run")
	}
	time.Sleep(20 * time.Millisecond)
	if n := closes.Load(); n != 1 {
		t.Fatalf("OnClose ran %d times", n)
	}
	if openTurn.Load() != closeTurn.Load() {
		t.Fatal("OnClose ran in another turn than the OnOpen that closed")
	}
	// The peer's EOF above shows the descriptor closed; its number may
	// already belong to another descriptor.
	if !conn.isReleased() {
		t.Fatal("connection not released after OnClose")
	}
}

// TestExternalCloseRacingTheOpenTurn closes a connection from another
// goroutine while its open turn runs. The close request must not be lost, and
// OnClose carries its cause.
func TestExternalCloseRacingTheOpenTurn(t *testing.T) {
	inOpen := make(chan *fdConn, 1)
	release := make(chan struct{})
	closed := make(chan error, 1)
	events := &Events{Pollers: 1}
	funcs(events).Open = func(conn Conn) {
		inOpen <- conn.(*fdConn)
		<-release
	}
	funcs(events).Close = func(_ Conn, err error) { closed <- err }
	addr := startEchoEvents(t, events)
	client, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	var conn *fdConn
	select {
	case conn = <-inOpen:
	case <-time.After(5 * time.Second):
		t.Fatal("OnOpen did not run")
	}
	cause := errors.New("closed from outside")
	if err := conn.CloseWith(cause); err != nil {
		t.Fatal(err)
	}
	close(release)
	select {
	case err := <-closed:
		if !errors.Is(err, cause) {
			t.Fatalf("OnClose error = %v, want %v", err, cause)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the close requested during OnOpen was lost")
	}
}

// TestSocketOptionsRaceClose sets options from another goroutine while the
// connection closes: each call reaches the connection's own socket or reports
// it closed.
func TestSocketOptionsRaceClose(t *testing.T) {
	for i := 0; i < 50; i++ {
		opened := make(chan Conn, 1)
		closed := make(chan struct{})
		events := &Events{Pollers: 1}
		funcs(events).Open = func(conn Conn) { opened <- conn }
		funcs(events).Close = func(Conn, error) { close(closed) }
		addr := startEchoEvents(t, events)
		client, err := net.DialTimeout("tcp", addr, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		conn := <-opened
		done := make(chan error, 1)
		go func() {
			for {
				err := conn.SetNoDelay(true)
				if err != nil {
					if !errors.Is(err, net.ErrClosed) {
						done <- err
						return
					}
					done <- nil
					return
				}
			}
		}()
		_ = conn.Close()
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("option setter never saw the close")
		}
		<-closed
		_ = client.Close()
	}
}

// TestCloseStopsAllWaiters checks that Close joins every waiter promptly when
// no traffic ever wakes them.
func TestCloseStopsAllWaiters(t *testing.T) {
	loopWaitersOverride = 3
	t.Cleanup(func() { loopWaitersOverride = 0 })

	for i := 0; i < 50; i++ {
		events := &Events{Pollers: 2}
		started := make(chan struct{})
		funcs(events).Start = func(*Events) { close(started) }
		serveDone := make(chan error, 1)
		go func() { serveDone <- events.Serve("") }()
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatal("loops did not start")
		}
		// Give the waiters a moment to reach their wait.
		time.Sleep(time.Millisecond)
		if err := events.Close(nil); err != nil {
			t.Fatal(err)
		}
		select {
		case err := <-serveDone:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("iteration %d: Serve did not stop after Close", i)
		}
	}
}

// flushBatchRecorder records every batch hand-off a loop makes.
type flushBatchRecorder struct {
	batches atomic.Int64
	tasks   atomic.Int64
}

func (c *flushBatchRecorder) Submit(IOTask) bool { return true }
func (c *flushBatchRecorder) SubmitBatch(tasks []IOTask) int {
	c.batches.Add(1)
	c.tasks.Add(int64(len(tasks)))
	return len(tasks)
}

// TestFlushCountsFilteredEvents pins the unit the flush interval counts:
// every event the batch walk examines, not only the ones that turn into
// connections. Under churn most of a batch can be stale descriptors, and a
// live connection behind them must still reach the executor within the same
// bound — with the interval applied after filtering, one live connection in a
// batch of stale events would wait for the whole batch.
func TestFlushCountsFilteredEvents(t *testing.T) {
	exec := &flushBatchRecorder{}
	pool := &ioTaskPool{executor: exec}
	loop := &eventLoop{fdMap: newFdMap(), ioPool: pool, events: &Events{Executor: exec}}
	conn := &fdConn{}
	conn.entry = (&Events{}).plainEntry()
	conn.loop = loop
	conn.pollTag.Store(7)
	const liveFD = 42
	if err := loop.fdMap.Put(liveFD, conn); err != nil {
		t.Fatal(err)
	}
	defer loop.fdMap.DeleteValue(liveFD, conn)

	waiter := loop.newWaiter()
	var events []poller.Event
	for i := 0; i < flushEvery-1; i++ {
		events = append(events, poller.Event{FD: 1000 + i, Tag: 1})
	}
	events = append(events, poller.Event{FD: liveFD, Tag: 7})
	for i := 0; i < 10; i++ {
		events = append(events, poller.Event{FD: 2000 + i, Tag: 1})
	}

	if !loop.dispatch(waiter, events) {
		t.Fatal("the live connection did not reach the executor before the batch ended")
	}
	if got := exec.batches.Load(); got != 1 {
		t.Fatalf("batch hand-offs = %d, want 1", got)
	}
	if got := exec.tasks.Load(); got != 1 {
		t.Fatalf("tasks = %d, want 1", got)
	}
	if len(waiter.ready) != 0 {
		t.Fatalf("ready list = %d, want flushed at the mark", len(waiter.ready))
	}
}

// TestWaiterNoteLoadSwitchesOnQuietAndLoad pins the adaptive wait's
// thresholds: a lone waiter parks while its rounds carry parkBatch events or
// more, blocks after quietWaits rounds of quietBatch events or fewer, and a
// round in between only breaks the quiet streak.
func TestWaiterNoteLoadSwitchesOnQuietAndLoad(t *testing.T) {
	waiter := &loopWaiter{}
	quiet := func(rounds int) {
		t.Helper()
		for range rounds {
			waiter.noteLoad(quietBatch)
		}
	}

	quiet(quietWaits - 1)
	if waiter.block {
		t.Fatalf("a waiter blocked short of %d quiet rounds", quietWaits)
	}
	waiter.noteLoad(quietBatch + 1)
	quiet(quietWaits - 1)
	if waiter.block {
		t.Fatalf("a round of %d events did not break the quiet streak", quietBatch+1)
	}
	quiet(1)
	if !waiter.block {
		t.Fatalf("a waiter did not block after %d quiet rounds", quietWaits)
	}

	quiet(quietWaits)
	if !waiter.block {
		t.Fatal("a quiet round took a blocked waiter out of its wait")
	}
	waiter.noteLoad(parkBatch)
	if waiter.block {
		t.Fatalf("a round of %d events did not send a blocked waiter back to park", parkBatch)
	}
	waiter.noteLoad(quietBatch)
	if waiter.block {
		t.Fatal("one quiet round blocked a waiter that had just parked")
	}
}

// TestAcceptedConnectionsGoRoundTheLoops pins how accepted connections are
// spread: in turn, whatever their descriptor numbers. Clients dialing from
// this process take every other descriptor, so the connections the server
// accepts all have numbers of one parity, and placing them by number would put
// every one of them on the same one of two loops.
func TestAcceptedConnectionsGoRoundTheLoops(t *testing.T) {
	loopWaitersOverride = 1
	t.Cleanup(func() { loopWaitersOverride = 0 })
	// Events runs no more loops than Ps.
	if procs := runtime.GOMAXPROCS(0); procs < 2 {
		runtime.GOMAXPROCS(2)
		t.Cleanup(func() { runtime.GOMAXPROCS(procs) })
	}

	const conns = 20
	opened := make(chan *fdConn, conns)
	events := &Events{Pollers: 2}
	funcs(events).Open = func(c Conn) { opened <- c.(*fdConn) }
	addr := startEchoEvents(t, events)

	perLoop := map[*eventLoop]int{}
	fdParity := map[int32]int{}
	for i := 0; i < conns; i++ {
		client, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		defer client.Close()
		select {
		case conn := <-opened:
			perLoop[conn.loop]++
			fdParity[conn.fd%2]++
		case <-time.After(5 * time.Second):
			t.Fatal("connection was not opened")
		}
	}
	if len(fdParity) != 1 {
		t.Logf("accepted descriptors were not all of one parity (%v); the spread still holds", fdParity)
	}
	if len(perLoop) != 2 {
		t.Fatalf("accepted connections used %d loops, want 2: %v", len(perLoop), perLoop)
	}
	for loop, n := range perLoop {
		if n != conns/2 {
			t.Fatalf("loop %p took %d of %d connections, want %d", loop, n, conns, conns/2)
		}
	}
}

// TestSelectLoopGoesRoundTheLoops pins the same spread for connections that
// are dialed or adopted rather than accepted.
func TestSelectLoopGoesRoundTheLoops(t *testing.T) {
	loops := []*eventLoop{{}, {}, {}}
	events := &Events{}
	events.loops = loops
	got := map[*eventLoop]int{}
	for range 3 * len(loops) {
		got[events.selectLoop()]++
	}
	for i, loop := range loops {
		if got[loop] != 3 {
			t.Fatalf("loop %d picked %d times out of %d, want 3", i, got[loop], 3*len(loops))
		}
	}
}

// TestTurnKeyKeepsALoopOnItsOwnShards pins how turns are keyed: where the
// shards are a multiple of the loops, every connection of a loop lands on the
// loop's own subset of shards, and the loop's connections spread over all of
// that subset, whatever descriptors they have.
func TestTurnKeyKeepsALoopOnItsOwnShards(t *testing.T) {
	const loops, shards = 6, 48
	for index := uint32(0); index < loops; index++ {
		loop := &eventLoop{index: index, count: loops}
		seen := map[uint32]bool{}
		for fd := int32(3); fd < 3+4*shards; fd++ {
			conn := &fdConn{fd: fd}
			conn.entry = (&Events{}).plainEntry()
			conn.loop = loop
			shard := conn.turnKey() % shards
			if shard%loops != index {
				t.Fatalf("fd %d of loop %d went to shard %d, outside the loop's subset", fd, index, shard)
			}
			if (*writeTurn)(conn).ShardKey() != conn.ShardKey() {
				t.Fatalf("fd %d: write turn key differs from the connection's", fd)
			}
			seen[shard] = true
		}
		if len(seen) != shards/loops {
			t.Fatalf("loop %d used %d shards, want its %d", index, len(seen), shards/loops)
		}
	}
	if got := (&fdConn{fd: 9}).turnKey(); got != 9 {
		t.Fatalf("a connection without a loop is keyed %d, want its descriptor", got)
	}
}

// TestLoopWaitModeFollowsTheMeasuredRange pins where each wait is used under
// the default sizing and with Pollers set: the adaptive wait only where a
// loop has a single waiter on a host of adaptiveMinProcs Ps or more, the
// blocking wait past parkedWaiters waiters, and parking everywhere else.
func TestLoopWaitModeFollowsTheMeasuredRange(t *testing.T) {
	names := map[waitMode]string{waitAdaptive: "adaptive", waitPark: "park", waitBlock: "block"}
	cases := []struct {
		procs, pollers int // pollers 0: the default
		want           waitMode
	}{
		{1, 0, waitPark}, {2, 0, waitPark}, {4, 0, waitPark}, {6, 0, waitPark},
		{8, 0, waitAdaptive}, {24, 0, waitAdaptive}, {32, 0, waitAdaptive}, {48, 0, waitAdaptive},
		{64, 0, waitBlock}, {128, 0, waitBlock},
		{4, 4, waitPark},  // one waiter per loop, but too few Ps to block
		{24, 1, waitPark}, // six waiters sharing one loop
	}
	for _, c := range cases {
		loops := c.pollers
		if loops == 0 {
			loops = defaultPollers(c.procs)
		}
		loops = min(loops, c.procs)
		waiters := loopWaiters(c.procs, loops)
		if got := loopWaitMode(c.procs, loops, waiters); got != c.want {
			t.Errorf("%d Ps, %d loops × %d waiters: %s, want %s", c.procs, loops, waiters, names[got], names[c.want])
		}
	}
}

func TestIOAdmissionRaceWithShutdown(t *testing.T) {
	for range 1000 {
		loop := &eventLoop{ioIdle: make(chan struct{}), ioPool: &ioTaskPool{}}
		conn := &fdConn{commonConn: commonConn{loop: loop}}
		conn.entry = (&Events{}).plainEntry()
		start := make(chan struct{})
		producerDone := make(chan struct{})
		stopDone := make(chan struct{})
		go func() {
			<-start
			if conn.noteIO(ioEventRead) {
				conn.taskState.And(^taskScheduledBit)
				loop.releaseIO()
			}
			close(producerDone)
		}()
		go func() {
			<-start
			loop.stopIO()
			close(stopDone)
		}()
		close(start)
		select {
		case <-producerDone:
		case <-time.After(time.Second):
			t.Fatal("I/O producer did not finish during shutdown")
		}
		select {
		case <-stopDone:
		case <-time.After(time.Second):
			t.Fatal("shutdown lost an in-flight I/O reservation")
		}
		if conn.taskState.Load()&taskScheduledBit != 0 {
			t.Fatal("shutdown left a connection scheduled without an owner")
		}
	}
}

// testLoopStops makes stopTestLoop idempotent per loop.
var testLoopStops sync.Map // *eventLoop -> *testLoopStop

type testLoopStop struct {
	once sync.Once
	done chan struct{}
}

// startTestLoop runs one waiter on loop, which a test publishes as the only
// loop of its Events, so a white-box test drives a loop the way Serve does.
func startTestLoop(events *Events, loop *eventLoop) {
	events.loops = []*eventLoop{loop}
	loop.start(1, waitBlock, false)
}

// stopTestLoop shuts loop down in the order Events does and returns a channel
// closed once it has. Later calls return the same channel.
func stopTestLoop(loop *eventLoop, err error) <-chan struct{} {
	value, _ := testLoopStops.LoadOrStore(loop, &testLoopStop{done: make(chan struct{})})
	stop := value.(*testLoopStop)
	stop.once.Do(func() {
		go func() {
			loop.stopping.Store(true)
			loop.closePoller(err)
			loop.shutdown(err)
			if loop.ioPool != nil && !loop.ioPoolOwner {
				loop.waitIODrained()
			}
			close(stop.done)
		}()
	})
	return stop.done
}
