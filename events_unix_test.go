//go:build (linux || darwin || netbsd || freebsd || openbsd || dragonfly) && !stdio

package uio

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"github.com/urpc/uio/internal/fdmap"
	"github.com/urpc/uio/internal/socket"
	"golang.org/x/sys/unix"
	"io"
	"net"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func newContractNativeLoop(t *testing.T) (*Events, *eventLoop) {
	t.Helper()
	ev := &Events{Pollers: 1}
	if err := ev.initConfig(); err != nil {
		t.Fatal(err)
	}
	loop, err := newEventLoop(ev)
	if err != nil {
		t.Fatal(err)
	}
	ev.loops = []*eventLoop{loop}
	t.Cleanup(func() {
		loop.closePoller(nil)
		loop.shutdown(net.ErrClosed)
	})
	return ev, loop
}

func contractNativeSocket(t *testing.T, ev *Events, loop *eventLoop) *fdConn {
	t.Helper()
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = unix.Close(fds[1])
	})
	c := &fdConn{fd: int32(fds[0])}
	c.entry = ev.plainEntry()
	c.loop = loop
	t.Cleanup(func() {
		c.closeUnregistered()
	})
	return c
}

func assertNativeDescriptorClosed(t *testing.T, fd int) {
	t.Helper()
	if _, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0); !errors.Is(err, unix.EBADF) {
		t.Fatalf("rejected descriptor remains open: %v", err)
	}
}

func TestNativeRegistrationWithdrawsAfterShutdownPublication(t *testing.T) {
	ev, loop := newContractNativeLoop(t)
	conn := contractNativeSocket(t, ev, loop)
	fd := conn.Fd()
	loop.stopping.Store(true)
	if err := loop.registerConn(conn); !errors.Is(err, net.ErrClosed) {
		t.Fatal(err)
	}
	if loop.fdMap.Get(fd) != nil || loop.ioTurnCount() != 0 {
		t.Fatal("registration left a mapping or a claimed turn after shutdown")
	}
	assertNativeDescriptorClosed(t, fd)
}

func TestNativeUDPAdmissionAndWatchFailuresReleaseOwnership(t *testing.T) {
	for _, mode := range []string{"stopped", "stopping", "closed-server", "retired-socket", "fd-range", "closed-poller"} {
		t.Run(mode, func(t *testing.T) {
			ev, loop := newContractNativeLoop(t)
			c := contractNativeSocket(t, ev, loop)
			c.udp = &unixUDPState{}
			fd := c.Fd()
			switch mode {
			case "stopped":
				loop.stopIO()
			case "stopping":
				loop.stopping.Store(true)
			case "closed-server":
				c.closeUnregistered()
			case "retired-socket":
				c.udp.gate.retire()
			case "fd-range":
				old := fdmap.MaxOpenFiles
				fdmap.MaxOpenFiles = 1
				loop.fdMap = fdmap.NewMap[fdConn]()
				fdmap.MaxOpenFiles = old
			case "closed-poller":
				_ = loop.poller.Close(nil)
			}
			var err error
			if mode == "stopped" || mode == "stopping" {
				err = loop.trackListener(c)
			} else {
				err = loop.registerListener(c)
			}
			if err == nil {
				t.Fatal("invalid UDP registration succeeded")
			}
			if mode != "fd-range" && mode != "closed-poller" && !errors.Is(err, net.ErrClosed) {
				t.Fatalf("registration error=%v", err)
			}
			if loop.fdMap.Get(fd) != nil || loop.ioTurnCount() != 0 || len(loop.udpServers) != 0 {
				t.Fatal("failed UDP admission retained a mapping, turn, or lifetime owner")
			}
			if mode == "stopped" || mode == "stopping" {
				assertNativeDescriptorClosed(t, fd)
			}
		})
	}
}

func TestNativeListenerWatchFailureRollsBackRegistry(t *testing.T) {
	ev, loop := newContractNativeLoop(t)
	l := &listener{fd: -1, loop: loop}
	if err := loop.addListener(l); err == nil {
		t.Fatal("invalid descriptor was registered")
	}
	if loop.listener(l.fd) != nil {
		t.Fatal("failed listener remained published")
	}
	a := &acceptor{events: ev, listeners: map[int]*listener{-1: l}}
	if err := a.startAccepting(); err == nil || l.started {
		t.Fatalf("start=%v started=%v", err, l.started)
	}
	l.descriptorGate.retire()
	if more, err := loop.accept(l, new(loopWaiter)); more || err != nil {
		t.Fatalf("closed listener accept=%v,%v", more, err)
	}
	loop.resumeAccept(l)
	loop.wakeTimer = time.AfterFunc(time.Hour, func() {
		t.Error("shutdown retained its fallback timer")
	})
	loop.shutdown(net.ErrClosed)
	if loop.wakeTimer != nil {
		t.Fatal("shutdown retained wake timer")
	}
	standalone := new(eventLoop)
	standalone.stopIO()
	if standalone.acquireIO() {
		t.Fatal("uninitialized stopped loop admitted a turn")
	}
}

func TestNativeAdoptionRejectsUnsupportedAndInvalidLayers(t *testing.T) {
	ev := new(Events)
	ev.ready.Store(true)
	if _, err := ev.Adopt(nil); !errors.Is(err, ErrUnsupported) {
		t.Fatal(err)
	}
	for _, mode := range []string{"unsupported-transport", "invalid-layer", "event-loop"} {
		t.Run(mode, func(t *testing.T) {
			a, b := net.Pipe()
			defer b.Close()
			var opts []AdoptOption
			if mode == "invalid-layer" {
				opts = []AdoptOption{WithLayers(nil)}
			}
			if mode == "event-loop" {
				opts = []AdoptOption{WithLayers(PassThroughLayer)}
				id := currentGoroutineID()
				activeEventLoops.Store(id, struct{}{})
				defer activeEventLoops.Delete(id)
			}
			_, err := ev.AdoptContext(context.Background(), a, opts...)
			if err == nil {
				t.Fatal("invalid adoption succeeded")
			}
			if mode == "event-loop" && !errors.Is(err, ErrDialOnEventLoop) {
				t.Fatal(err)
			}
			if _, err := b.Read(make([]byte, 1)); !errors.Is(err, net.ErrClosed) && !errors.Is(err, io.EOF) {
				t.Fatalf("rejected adoption kept transport open: %v", err)
			}
		})
	}
}

func TestNativeStartupResourceFailureRollsBackEarlierLoops(t *testing.T) {
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(max(2, runtime.GOMAXPROCS(0))))
	ev := &Events{Pollers: 2}
	if err := ev.initConfig(); err != nil {
		t.Fatal(err)
	}
	if ev.Pollers != 2 {
		t.Fatalf("Pollers = %d, want two loops", ev.Pollers)
	}
	var original unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_NOFILE, &original); err != nil {
		t.Fatal(err)
	}
	limit := original
	limit.Cur = min(limit.Cur, 64)
	if err := unix.Setrlimit(unix.RLIMIT_NOFILE, &limit); err != nil {
		t.Fatal(err)
	}
	var opened []int
	defer func() {
		_ = unix.Setrlimit(unix.RLIMIT_NOFILE, &original)
		for _, fd := range opened {
			_ = unix.Close(fd)
		}
	}()
	fill := func() (int, error) {
		n := 0
		for {
			fd, err := unix.Open("/dev/null", unix.O_RDONLY|unix.O_CLOEXEC, 0)
			if errors.Is(err, unix.EMFILE) {
				return n, nil
			}
			if err != nil {
				return n, err
			}
			opened = append(opened, fd)
			n++
		}
	}
	var failure, initErr error
	for attempt := 0; attempt < 10; attempt++ {
		var unexpected bool
		unexpected, initErr, failure = startupRollbackAttempt(ev, fill, &opened)
		if !unexpected {
			break
		}
		failure = errors.New("startup unexpectedly exceeded the descriptor limit")
	}
	restoreErr := unix.Setrlimit(unix.RLIMIT_NOFILE, &original)
	for _, fd := range // Finalizers and closes left from earlier tests can free a descriptor
	// between the fill and the startup, which then fits; that attempt proves
	// nothing either way, so it is repeated.
	opened {
		_ = unix.Close(fd)
	}
	opened = nil
	if restoreErr != nil {
		t.Fatal(restoreErr)
	}
	if failure != nil {
		t.Fatalf("resource rollback: %v (startup error %v)", failure, initErr)
	}
}

func startupRollbackAttempt(ev *Events, fill func() (int, error), opened *[]int) (unexpected bool, initErr, failure error) {
	if _, err := fill(); err != nil {
		return false, nil, err
	}
	if len(*opened) < 3 {
		return false, nil, errors.New("not enough descriptors to exercise partial startup")
	}
	var released []int // One poller needs three descriptors on both epoll and kqueue. No waiter
	// starts until all pollers exist, so the second allocation must fail.

	for range 3 {
		last := len(*opened) - 1
		released = append(released, (*opened)[last])
		_ = unix.Close((*opened)[last])
		*opened = (*opened)[:last]
	}
	initErr = ev.initLoops()
	if initErr == nil {
		ev.shutdownLoops(nil)
		ev.loops = nil
		return true, nil, nil
	}
	if !errors.Is(initErr, unix.EMFILE) || ev.loops != nil {
		return true, initErr, nil
	}
	for _, fd := range released {
		if _, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0); !errors.Is(err, unix.EBADF) {
			failure = fmt.Errorf("startup retained released descriptor %d: %v", fd, err)
		}
	}
	reclaimed, err := fill()
	if err != nil || reclaimed < len(released) {
		failure = fmt.Errorf("startup reclaimed %d slots, want at least %d: %v", reclaimed, len(released), err)
	}
	return false, initErr, failure
}

func TestNativeListenerGroupRollsBackASecondBindFailure(t *testing.T) {
	cause := errors.New("second listener refused by socket policy")
	a := &acceptor{events: &Events{ReusePort: true}}
	calls := 0
	firstFD := -1
	var bound string
	group, err := a.bindGroup("tcp4://127.0.0.1:0", func(_, addr string, fd uintptr) error {
		calls++
		if calls == 1 {
			firstFD = int(fd)
			return nil
		}
		bound = addr
		return cause
	})
	if !errors.Is(err, cause) || group != nil || calls < 2 {
		t.Fatalf("group=%v calls=%d err=%v", group, calls, err)
	}
	assertNativeDescriptorClosed(t, firstFD)
	probe, err := net.Listen("tcp4", bound)
	if err != nil {
		t.Fatalf("failed group retained its port: %v", err)
	}
	probe.Close()
}

func TestNativeDynamicListenerFailureDoesNotPublishSockets(t *testing.T) {
	ev, loop := newContractNativeLoop(t)
	_ = loop.poller.Close(nil)
	a := &acceptor{events: ev, accepting: true}
	if _, err := a.listenGroup("tcp4://127.0.0.1:0", listenConfig{}); err == nil {
		t.Fatal("listener started on a closed poller")
	}
	if len(a.listeners) != 0 || len(*loop.listeners.Load()) != 0 {
		t.Fatal("failed dynamic listen left a published socket")
	}
}

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
	t.Cleanup(func() {
		registeredForTest = nil
	})
	started := make(chan struct{})
	events := withFuncs(&Events{Pollers: 2}, FuncHandler{Start: func(*Events) {
		close(started)
	}})
	funcs(events).Open = func(conn Conn) {
		conn.SetUserdata(true)
	}
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
	go func() {
		serveDone <- events.Serve()
	}()
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

func TestLoopWaitModeFollowsTheMeasuredRange(t *testing.T) {
	names := map[waitMode]string{waitAdaptive: "adaptive", waitPark: "park", waitBlock: "block"}
	cases := []struct {
		procs, pollers int
		want           waitMode
	}{{1, 0, waitPark}, {2, 0, waitPark}, {4, 0, waitPark}, {6, 0, waitPark}, {8, 0, waitAdaptive}, {24, 0, waitAdaptive}, {32, 0, waitAdaptive}, {40, 0, waitBlock}, {48, 0, waitBlock}, {64, 0, waitBlock}, {128, 0, waitBlock}, {48, 16, waitBlock}, {4, 4, waitPark}, {24, 1, waitPark}}
	for _, c := range // pollers 0: the default
	cases {
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

var testLoopStops sync.Map // *eventLoop -> *testLoopStop

type testLoopStop struct {
	once sync.Once
	done chan struct{}
}

func startTestLoop(events *Events, loop *eventLoop) {
	events.loops = []*eventLoop{loop}
	loop.start(1, waitBlock, false)
}

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

func TestWideHostGeometry(t *testing.T) {
	cases := []struct {
		procs, loops       int
		waiters, listeners int
	}{{8, 2, 1, 2}, {24, 6, 1, 2}, {32, 8, 1, 2}, {33, 8, 1, 8}, {48, 8, 1, 8}, {64, 8, 1, 8}, {64, 16, 1, 16}, {128, 8, 1, 8}}
	for _, c := range cases {
		if got := loopWaiters(c.procs, c.loops); c.procs >= wideHostProcs && got != c.waiters {
			t.Errorf("%d Ps, %d loops: %d waiters per loop, want %d", c.procs, c.loops, got, c.waiters)
		}
		if got := reusePortListeners(c.procs, c.loops); got != c.listeners {
			t.Errorf("%d Ps, %d loops: %d listeners, want %d", c.procs, c.loops, got, c.listeners)
		}
	}
}

func TestListenReusePortGroup(t *testing.T) {
	listenersOverride = 3
	t.Cleanup(func() {
		listenersOverride = 0
	})
	h := newListenEcho()
	events := h.events()
	events.ReusePort = true
	serveLoopback(t, events)
	ln, err := events.Listen("tcp://127.0.0.1:0", WithUserdata("group"))
	if err != nil {
		t.Fatal(err)
	}
	if members := len(ln.(*listenerGroup).members); members != 3 {
		t.Fatalf("ReusePort listener has %d members, want 3", members)
	}
	addr := ln.Addr().String()
	for range 6 {
		c, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		echoOnce(t, c, "group")
		_ = c.Close()
	}
	for _, ud := range h.userdata() {
		if ud != "group" {
			t.Fatalf("a connection of the group opened with %v", ud)
		}
	}
	_ = ln.Close()
	waitRefused(t, addr)
}

func eachWaitMode(t *testing.T, test func(t *testing.T)) {
	t.Run("sized", test)
	t.Run("blocking", func(t *testing.T) {
		blockingLoops(t)
		test(t)
	})
}

func TestAcceptRunnerTakesEveryConnectionOfABurst(t *testing.T) {
	eachWaitMode(t, testAcceptRunnerTakesEveryConnectionOfABurst)
}

func testAcceptRunnerTakesEveryConnectionOfABurst(t *testing.T) {
	var opened atomic.Int32
	events := &Events{Pollers: 1, Handler: FuncHandler{Open: func(Conn) {
		opened.Add(1)
	}}}
	addr, _ := serveLoopback(t, events)
	if events.loops[0].accepts == nil {
		t.Fatal("a loop has no accept runner")
	}
	const n = 256
	clients := make([]net.Conn, n)
	var wg sync.WaitGroup
	for i := range clients {
		wg.Go(func() {
			c, err := net.Dial("tcp", addr)
			if err != nil {
				t.Error(err)
				return
			}
			clients[i] = c
		})
	}
	wg.Wait()
	defer func() {
		for _, c := range clients {
			if c != nil {
				_ = c.Close()
			}
		}
	}()
	deadline := time.Now().Add(5 * time.Second)
	for opened.Load() < n && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := opened.Load(); got != n {
		t.Fatalf("%d of %d connections opened", got, n)
	}
}

func TestAcceptRunnerDrainsAgainForALateEdge(t *testing.T) {
	eachWaitMode(t, testAcceptRunnerDrainsAgainForALateEdge)
}

func testAcceptRunnerDrainsAgainForALateEdge(t *testing.T) {
	var (
		once     atomic.Bool
		target   atomic.Pointer[string]
		late     net.Conn
		lateEdge = make(chan bool, 1)
	)
	testHookAcceptDrained = func(l *listener) {
		if !once.CompareAndSwap(false, true) {
			return
		}
		c, err := net.Dial("tcp", *target.Load())
		if err != nil {
			lateEdge <- false
			return
		}
		late = c
		deadline := time.Now().Add(5 * time.Second)
		for l.acceptState.Load() != acceptAgain && time.Now().Before(deadline) {
			time.Sleep(100 * time.Microsecond)
		}
		lateEdge <- l.acceptState.Load() == acceptAgain
	}
	t.Cleanup(func() {
		testHookAcceptDrained = nil
	})
	opened := make(chan Conn, 4)
	events := &Events{Pollers: 1, Handler: FuncHandler{Open: func(c Conn) {
		signal(opened, c)
	}}}
	addr, _ := serveLoopback(t, events)
	target.Store(&addr)
	first, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	for i := range 2 {
		select {
		case <-opened:
		case <-time.After(5 * time.Second):
			t.Fatalf("connection %d of 2 never opened", i+1)
		}
	}
	if !<-lateEdge {
		t.Fatal("the waiter did not record the late edge")
	}
	_ = late.Close()
}

func TestAcceptRunnerResumesAPausedListener(t *testing.T) {
	eachWaitMode(t, testAcceptRunnerResumesAPausedListener)
}

func testAcceptRunnerResumesAPausedListener(t *testing.T) {
	opened := make(chan Conn, 4)
	events := &Events{Pollers: 1, Handler: FuncHandler{Open: func(c Conn) {
		signal(opened, c)
	}}}
	addr, _ := serveLoopback(t, events)
	var l *listener
	for _, member := range events.acceptor.listeners {
		l = member
	}
	l.loop.pauseAccept(l)
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	select {
	case <-opened:
	case <-time.After(5 * time.Second):
		t.Fatal("the connection queued during the pause never opened")
	}
}

func blockingLoops(t *testing.T) {
	t.Helper()
	loopWaitersOverride = parkedWaiters + 1
	t.Cleanup(func() {
		loopWaitersOverride = 0
	})
}

func TestNativeSchedulerTerminalBatchOwnership(t *testing.T) {
	for _, mode := range []string{"typed-batch", "external-allocate", "external-reuse", "external-refused"} {
		t.Run(mode, func(t *testing.T) {
			var executor Executor
			if mode == "external-refused" {
				executor = rejectingNativeExecutor{}
			} else if mode != "typed-batch" {
				executor = nativeTaskExecutor{}
			}
			pool := newIOTaskPool(executor)
			t.Cleanup(pool.stop)
			loop := &eventLoop{ioPool: pool}
			finished := make(chan Conn, 3)
			cause := errors.New("transport already released")
			events := withFuncs(&Events{}, FuncHandler{Close: func(c Conn, err error) {
				if !errors.Is(err, cause) {
					t.Errorf("terminal close cause = %v", err)
				}
				finished <- c
			}})
			conns := make([]*fdConn, 3)
			tasks := make([]IOTask, len(conns))
			for i := range conns {
				c := &fdConn{fd: -1}
				c.loop, c.entry = loop, events.plainEntry()
				c.close.phase.Store(closeResourcesReleased)
				c.setDeferredCloseLocked(cause)
				c.taskState.Store(ioEventClose | taskScheduledBit)
				loop.acquireCloseIO()
				conns[i], tasks[i] = c, c
			}
			var args []IOTask
			if mode == "external-reuse" {
				args = make([]IOTask, len(conns)+2)
			}
			if mode == "typed-batch" {
				if !pool.submitBatch(tasks) {
					t.Fatal("owned scheduler refused terminal callbacks")
				}
			} else if !pool.submitConnBatch(conns, args) {
				t.Fatal("scheduler refused its completion handoff")
			}
			for _, item := range args {
				if item != nil {
					t.Fatal("caller scratch retained a connection after submission")
				}
			}
			seen := make(map[Conn]bool)
			for range conns {
				c := readControlAwait(t, finished, "terminal callback")
				if seen[c] {
					t.Fatal("terminal callback ran more than once")
				}
				seen[c] = true
			}
			loop.waitIODrained()
			pool.stop()
			if n := loop.ioTurnCount(); n != 0 {
				t.Fatalf("completion handoff retained %d turns", n)
			}
			for _, c := range conns {
				if c.close.phase.Load() != closeCallbackDelivered || c.taskState.Load()&taskScheduledBit != 0 {
					t.Fatal("terminal callback left its claim or close phase unfinished")
				}
			}
		})
	}
}

func TestNativeSchedulerStopRejectsFutureWorkAndFinishesLateRefusal(t *testing.T) {
	var absent *ioTaskPool
	absent.stop()
	if absent.submit(nil) || absent.submitWrite(nil) || absent.submitBatch(nil) || absent.submitConnBatch(nil, nil) {
		t.Fatal("absent scheduler accepted work")
	}
	(&fdConn{}).RunTask()
	(*writeTurn)(&fdConn{}).RunTask()
	(*writeTurn)(&fdConn{commonConn: commonConn{loop: &eventLoop{}}}).RunTask()
	for _, external := range []bool{false, true} {
		var executor Executor
		if external {
			executor = rejectingNativeExecutor{}
		}
		pool := newIOTaskPool(executor)
		if pool.submit(nil) || pool.submitWrite(nil) || !pool.submitBatch(nil) || !pool.submitConnBatch(nil, nil) {
			t.Fatal("empty submission changed scheduler ownership")
		}
		if external {
			opening := &fdConn{}
			opening.taskState.Store(ioEventOpen | taskScheduledBit)
			if pool.submit(opening) {
				t.Fatal("refused initial open was reported as accepted")
			}
		}
		pool.stop()
		unaccepted := &fdConn{}
		if pool.submit(unaccepted) || pool.submitWrite(unaccepted) || pool.submitBatch([]IOTask{unaccepted}) || pool.submitConnBatch([]*fdConn{unaccepted}, nil) {
			t.Fatal("stopped scheduler accepted future work")
		}
		closed := make(chan error, 1)
		loop := &eventLoop{ioPool: pool}
		events := withFuncs(&Events{}, FuncHandler{Close: func(_ Conn, err error) {
			closed <- err
		}})
		conn := &fdConn{fd: -1}
		conn.loop, conn.entry = loop, events.plainEntry()
		conn.close.phase.Store(closeResourcesReleased)
		conn.setDeferredCloseLocked(io.EOF)
		conn.taskState.Store(ioEventClose | taskScheduledBit)
		loop.acquireCloseIO()
		pool.rejectBatch([]IOTask{conn})
		if err := readControlAwait(t, closed, "late refused callback"); !errors.Is(err, io.EOF) {
			t.Fatalf("late refusal lost its terminal cause: %v", err)
		}
		if loop.ioTurnCount() != 0 || conn.close.phase.Load() != closeCallbackDelivered {
			t.Fatal("late refused callback retained its turn")
		}
	}
}

func TestNativeTerminalCallbackSurvivesLostScheduler(t *testing.T) {
	for _, mode := range []string{"no-loop", "no-scheduler", "stopped-scheduler", "claimed-turn"} {
		t.Run(mode, func(t *testing.T) {
			closed := make(chan error, 2)
			events := withFuncs(&Events{}, FuncHandler{Close: func(_ Conn, err error) {
				closed <- err
			}})
			conn := &fdConn{fd: -1}
			conn.entry = events.plainEntry()
			conn.close.phase.Store(closeResourcesReleased)
			if mode != "no-loop" {
				conn.loop = &eventLoop{}
			}
			if mode == "stopped-scheduler" || mode == "claimed-turn" {
				pool := newIOTaskPool(nativeTaskExecutor{})
				t.Cleanup(pool.stop)
				conn.loop.ioPool = pool
				if mode == "stopped-scheduler" {
					pool.stop()
				} else {
					conn.taskState.Store(taskScheduledBit)
					conn.loop.acquireCloseIO()
				}
			}
			conn.scheduleCloseCallback(io.EOF)
			if mode == "claimed-turn" {
				select {
				case <-closed:
					t.Fatal("terminal delivery overtook the existing turn")
				default:
				}
				if !conn.loop.ioPool.submit(conn) {
					t.Fatal("executor refused its terminal turn")
				}
			}
			if err := readControlAwait(t, closed, "terminal callback after scheduler removal"); !errors.Is(err, io.EOF) {
				t.Fatalf("terminal cause=%v", err)
			}
			if conn.loop != nil {
				conn.loop.waitIODrained()
				if conn.loop.ioTurnCount() != 0 {
					t.Fatal("terminal fallback retained its reservation")
				}
			}
			conn.fireCloseCallback()
			select {
			case <-closed:
				t.Fatal("terminal callback was delivered twice")
			default:
			}
		})
	}
}

func nativeDatagramFixture(t *testing.T, events *Events) (*fdConn, [2]int) {
	t.Helper()
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_DGRAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, fd := range fds {
		if err := unix.SetNonblock(fd, true); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		_ = unix.Close(fds[0])
		_ = unix.Close(fds[1])
	})
	conn := &fdConn{fd: int32(fds[0]), udp: &unixUDPState{}}
	conn.loop, conn.entry = &eventLoop{}, events.plainEntry()
	return conn, fds
}

func TestNativeDatagramVectorAtomicityAndRefusal(t *testing.T) {
	for _, mode := range []string{"complete", "per-packet-limit", "blocked", "peer-closed"} {
		t.Run(mode, func(t *testing.T) {
			written := 0
			events := withFuncs(&Events{}, FuncHandler{BytesWritten: func(_ Conn, n int) {
				written += n
			}})
			if mode == "per-packet-limit" {
				events.MaxOutboundBuffered = 3
			}
			conn, fds := nativeDatagramFixture(t, events)
			vec := [][]byte{nil, []byte("ab"), nil, []byte("cd")}
			switch mode {
			case "per-packet-limit":
				vec = [][]byte{[]byte("ab"), []byte("four")}
			case "blocked":
				vec = [][]byte{fillDatagramSendBuffer(t, fds[0])}
			case "peer-closed":
				replacement, err := unix.Open("/dev/null", unix.O_RDONLY, 0)
				if err != nil {
					t.Fatal(err)
				}
				defer unix.Close(replacement)
				if err := unix.Dup2(replacement, fds[1]); err != nil {
					t.Fatal(err)
				}
			}
			n, err := conn.Writev(vec)
			switch mode {
			case "complete":
				if n != 4 || err != nil || written != n {
					t.Fatalf("vector write = %d,%v; reported=%d", n, err, written)
				}
				for _, want := range []string{"ab", "cd"} {
					if got := readPeer(t, fds[1], len(want)); string(got) != want {
						t.Fatalf("datagram=%q, want %q", got, want)
					}
				}
			case "per-packet-limit":
				if n != 0 || !errors.Is(err, ErrOutboundOverflow) || written != 0 {
					t.Fatalf("oversized vector=%d,%v; reported=%d", n, err, written)
				}
				var packet [16]byte
				if _, err := unix.Read(fds[1], packet[:]); !isWouldBlock(err) {
					t.Fatalf("budget rejection emitted a prefix packet: %v", err)
				}
			case "blocked":
				if n != 0 || !isUDPSendBlocked(err) || conn.isClosing() || written != 0 {
					t.Fatalf("blocked vector=%d,%v; closing=%v reported=%d", n, err, conn.isClosing(), written)
				}
			case "peer-closed":
				if n != 0 || err == nil || isUDPSendBlocked(err) || !conn.isClosing() || written != 0 {
					t.Fatalf("failed vector=%d,%v; closing=%v reported=%d", n, err, conn.isClosing(), written)
				}
				if cause := conn.takeDeferredCloseCause(); !errors.Is(cause, err) {
					t.Fatalf("send failure %v lost as close cause %v", err, cause)
				}
			}
			if conn.OutboundBuffered() != 0 {
				t.Fatal("datagram send retained a stream suffix")
			}
		})
	}
}

func TestNativeDatagramRetirementStopsEveryDescriptorEntry(t *testing.T) {
	for _, direct := range []bool{false, true} {
		conn, _ := nativeDatagramFixture(t, &Events{})
		server := &fdConn{fd: conn.fd, internal: true, udp: &unixUDPState{peers: make(map[socket.UDPAddress]*fdConn)}}
		server.loop, server.entry = conn.loop, conn.entry
		conn.udp.server = server
		if direct {
			server.ioOwner.Store(currentGoroutineID())
		}
		server.udp.gate.retire()
		for _, write := range []func() (int, error){func() (int, error) {
			return conn.Write([]byte("packet"))
		}, func() (int, error) {
			return conn.Writev([][]byte{[]byte("packet")})
		}, func() (int, error) {
			owned := AcquireBuffer(6)
			_, _ = owned.WriteString("packet")
			return conn.WriteOwned(owned)
		}} {
			if n, err := write(); n != 0 || !errors.Is(err, net.ErrClosed) {
				t.Fatalf("retired datagram write=%d,%v (direct=%v)", n, err, direct)
			}
		}
		called := false
		if err := conn.Control(func(uintptr) {
			called = true
		}); !errors.Is(err, net.ErrClosed) || called {
			t.Fatalf("retired Control=%v, invoked=%v (direct=%v)", err, called, direct)
		}
		if n := conn.loop.ioTurnCount(); n != 0 {
			t.Fatalf("retired descriptor entry retained %d turns", n)
		}
		if err := conn.ReserveInbound(10); err != nil {
			t.Fatal(err)
		}
		if err := conn.Flush(); err != nil {
			t.Fatalf("datagram flush=%v", err)
		}
	}
}

func TestNativeDatagramShutdownRejectsVectorsBeforeSending(t *testing.T) {
	for _, reason := range []string{"loop-stopped", "events-closing", "owner-released"} {
		t.Run(reason, func(t *testing.T) {
			events := &Events{}
			conn, fds := nativeDatagramFixture(t, events)
			switch reason {
			case "loop-stopped":
				conn.loop.stopping.Store(true)
			case "events-closing":
				events.closing.Store(true)
			case "owner-released":
				server := &fdConn{fd: conn.fd, udp: &unixUDPState{}}
				server.loop, server.entry = conn.loop, conn.entry
				server.close.phase.Store(closeResourcesReleased)
				conn.udp.server = server
			}
			if n, err := conn.Writev([][]byte{[]byte("packet")}); n != 0 || !errors.Is(err, net.ErrClosed) {
				t.Fatalf("shutdown vector=%d,%v", n, err)
			}
			var packet [16]byte
			if n, err := unix.Read(fds[1], packet[:]); n > 0 || !isWouldBlock(err) {
				t.Fatalf("shutdown emitted a packet: %q,%v", bytes.TrimRight(packet[:], "\x00"), err)
			}
		})
	}
}

func TestNativeDatagramScalarFailureRetainsTransportCause(t *testing.T) {
	for _, owned := range []bool{false, true} {
		conn, fds := nativeDatagramFixture(t, &Events{})
		replacement, err := unix.Open("/dev/null", unix.O_RDONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		if err := unix.Dup2(replacement, fds[1]); err != nil {
			t.Fatal(err)
		}
		_ = unix.Close(replacement)
		var n int
		if owned {
			buffer := AcquireBuffer(6)
			_, _ = buffer.WriteString("packet")
			n, err = conn.WriteOwned(buffer)
		} else {
			n, err = conn.Write([]byte("packet"))
		}
		if n != 0 || err == nil || isUDPSendBlocked(err) || !conn.isClosing() || conn.OutboundBuffered() != 0 {
			t.Fatalf("failed scalar datagram=%d,%v; closing=%v pending=%d", n, err, conn.isClosing(), conn.OutboundBuffered())
		}
		if cause := conn.takeDeferredCloseCause(); !errors.Is(cause, err) {
			t.Fatalf("scalar send error %v lost as close cause %v", err, cause)
		}
	}
}

func TestWakeIntervalFallsBackAndRepeats(t *testing.T) {
	rec := &wakeRecorder{}
	events := &Events{Pollers: 1, Handler: rec}
	started := make(chan struct{})
	events.Hooks = FuncHandler{Start: func(*Events) {
		close(started)
	}}
	done := make(chan error, 1)
	go func() {
		done <- events.Serve()
	}()
	t.Cleanup(func() {
		_ = events.Close(nil)
		<-done
	})
	<-started
	sinkStarted := make(chan struct{})
	sink := &Events{Pollers: 1, Handler: FuncHandler{Open: func(Conn) {
	}, Read: func(Conn) error {
		return nil
	}, Close: func(Conn, error) {
	}}, Hooks: FuncHandler{Start: func(*Events) {
		close(sinkStarted)
	}}}
	sinkDone := make(chan error, 1)
	go func() {
		sinkDone <- sink.Serve()
	}()
	t.Cleanup(func() {
		_ = sink.Close(nil)
		<-sinkDone
	})
	<-sinkStarted
	listener, err := sink.Listen("tcp://127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = listener.Close()
	})
	conn, err := events.Dial("tcp://" + listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = conn.Close()
	})
	fail := func(*eventLoop, time.Duration) error {
		return errors.New("no kernel timer")
	}
	armKernelTimerOverride.Store(&fail)
	t.Cleanup(func() {
		armKernelTimerOverride.Store(nil)
	})
	if err := conn.SetWakeInterval(20 * time.Millisecond); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 3*time.Second, func() bool {
		return rec.fires() >= 3
	})
	if rec.fires() < 3 {
		t.Fatalf("the fallback interval fired %d times, want at least 3", rec.fires())
	}
	if err := conn.SetWakeInterval(0); err != nil {
		t.Fatal(err)
	}
	fires := rec.fires()
	time.Sleep(150 * time.Millisecond)
	if got := rec.fires(); got != fires {
		t.Fatalf("a stopped fallback interval kept firing: %d then %d", fires, got)
	}
}

type recoveringUDPExec struct{ recovered chan struct{} }

func (r recoveringUDPExec) Submit(t IOTask) bool {
	go func() {
		defer func() {
			if recover() != nil {
				select {
				case r.recovered <- struct{}{}:
				default:
				}
			}
		}()
		t.RunTask()
	}()
	return true
}

func (r recoveringUDPExec) SubmitBatch(ts []IOTask) int {
	for _, t := range ts {
		r.Submit(t)
	}
	return len(ts)
}

func TestUDPPanicRecoveryClearsBatchState(t *testing.T) {
	panicRecovered := make(chan struct{}, 1)
	panicEv := &Events{
		Pollers:        1,
		ReadBufferSize: 2048,
		Executor:       recoveringUDPExec{recovered: panicRecovered},
		Handler: FuncHandler{
			Read: func(Conn) error {
				panic("udp-callback-panic")
			},
		},
	}
	pStarted := make(chan struct{})
	panicEv.Hooks = FuncHandler{Start: func(*Events) { close(pStarted) }}
	pDone := make(chan error, 1)
	go func() { pDone <- panicEv.Serve() }()
	<-pStarted
	defer func() {
		_ = panicEv.Close(nil)
		<-pDone
	}()
	pLn, err := panicEv.Listen("udp://127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pLn.Close()
	pCli, err := net.Dial("udp", pLn.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer pCli.Close()
	_, _ = pCli.Write([]byte("boom1"))
	_, _ = pCli.Write([]byte("boom2"))
	select {
	case <-panicRecovered:
	case <-time.After(2 * time.Second):
	}
}
