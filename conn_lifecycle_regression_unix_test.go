//go:build (linux || darwin || netbsd || freebsd || openbsd || dragonfly) && !stdio

package uio

import (
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urpc/uio/internal/poller"
	"golang.org/x/sys/unix"
)

// A readiness event for an admitted connection's descriptor number, collected
// for its previous owner where the backend reports no registration tag, can
// reach the loop before the connection's first turn is submitted. It must
// fold into that claimed turn, which runs OnOpen first and alone, and its
// hangup must not stick to the new connection.
func TestStaleEventDuringAdmissionFoldsIntoTheOpenTurn(t *testing.T) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fds[1])
	for _, fd := range fds {
		if err := unix.SetNonblock(fd, true); err != nil {
			t.Fatal(err)
		}
	}
	var mu sync.Mutex
	var order []string
	var inTurn, overlap atomic.Int32
	enter := func(name string) {
		if inTurn.Add(1) > 1 {
			overlap.Add(1)
		}
		mu.Lock()
		order = append(order, name)
		mu.Unlock()
	}
	gotData := make(chan struct{}, 1)
	events := &Events{Pollers: 1}
	events.OnOpen = func(Conn) { enter("open"); inTurn.Add(-1) }
	events.OnData = func(c Conn) error {
		enter("data")
		defer inTurn.Add(-1)
		_, _ = c.Discard(-1)
		select {
		case gotData <- struct{}{}:
		default:
		}
		return nil
	}
	if err := events.initConfig(); err != nil {
		t.Fatal(err)
	}
	loop, err := newEventLoop(events)
	if err != nil {
		t.Fatal(err)
	}
	events.loops = []*eventLoop{loop}
	t.Cleanup(func() { <-stopTestLoop(loop, nil) })
	if _, err := unix.Write(fds[1], []byte("hello")); err != nil {
		t.Fatal(err)
	}

	conn := &fdConn{fd: fds[0]}
	conn.events, conn.loop = events, loop
	if !conn.admitAccepted(false) {
		t.Fatal("connection was not admitted")
	}
	waiter := loop.newWaiter()
	loop.dispatch(waiter, []poller.Event{{FD: fds[0], Events: poller.ReadEvents | poller.HangupEvents}})
	if len(waiter.ready) != 0 {
		t.Fatal("a stale event claimed a second turn for an admitted connection")
	}
	if !loop.ioPool.submit(conn) {
		t.Fatal("open turn was refused")
	}
	select {
	case <-gotData:
	case <-time.After(5 * time.Second):
		t.Fatal("the open turn read nothing")
	}
	for deadline := time.Now().Add(5 * time.Second); conn.taskState.Load()&taskScheduledBit != 0; {
		if time.Now().After(deadline) {
			t.Fatal("turn did not finish")
		}
		time.Sleep(time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(order) < 2 || order[0] != "open" || order[1] != "data" || overlap.Load() != 0 {
		t.Fatalf("callbacks = %v, overlap = %d", order, overlap.Load())
	}
	if conn.turn&turnHangup != 0 {
		t.Fatal("a stale hangup stuck to the new connection")
	}
}

// startRecoveringServer serves addr with an Executor that recovers callback
// panics, and returns the listening address.
func startRecoveringServer(t *testing.T, events *Events, network string) string {
	t.Helper()
	started := make(chan string, 1)
	events.OnStart = func(ev *Events) {
		for _, listener := range ev.acceptor.listeners {
			started <- listener.pair.local.String()
			return
		}
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- events.Serve(network + "://127.0.0.1:0") }()
	t.Cleanup(func() {
		_ = events.Close(nil)
		select {
		case <-serveDone:
		case <-time.After(5 * time.Second):
			t.Error("Serve did not return")
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

// An accepted connection whose OnOpen panics, under an Executor that recovers
// the panic, still starts watching its socket: it reads, and notices its peer
// closing.
func TestRecoveredOnOpenPanicStillWatchesTheSocket(t *testing.T) {
	executor := &recoveringExecutor{}
	gotData := make(chan struct{}, 1)
	closed := make(chan struct{}, 1)
	events := &Events{Pollers: 1, Executor: executor}
	events.OnOpen = func(Conn) { panic("OnOpen panics") }
	events.OnData = func(conn Conn) error {
		_, _ = conn.Discard(-1)
		select {
		case gotData <- struct{}{}:
		default:
		}
		return nil
	}
	events.OnClose = func(Conn, error) { closed <- struct{}{} }
	addr := startRecoveringServer(t, events, "tcp")
	client, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(5 * time.Second); executor.recovered.Load() == 0; {
		if time.Now().After(deadline) {
			t.Fatal("OnOpen did not panic")
		}
		time.Sleep(time.Millisecond)
	}
	if _, err := client.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-gotData:
	case <-time.After(5 * time.Second):
		t.Fatal("no OnData after a recovered OnOpen panic")
	}
	_ = client.Close()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("no OnClose after the peer closed")
	}
}

// A panic in OnClose, delivered at the end of the turn that released the
// connection and recovered by the Executor, still ends that turn, so Serve
// returns.
func TestRecoveredOnClosePanicLetsServeReturn(t *testing.T) {
	executor := &recoveringExecutor{}
	events := &Events{Pollers: 1, Executor: executor}
	events.OnClose = func(Conn, error) { panic("OnClose panics") }
	started := make(chan string, 1)
	events.OnStart = func(ev *Events) {
		for _, listener := range ev.acceptor.listeners {
			started <- listener.pair.local.String()
			return
		}
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- events.Serve("tcp://127.0.0.1:0") }()
	client, err := net.Dial("tcp", <-started)
	if err != nil {
		t.Fatal(err)
	}
	_ = client.Close()
	for deadline := time.Now().Add(5 * time.Second); executor.recovered.Load() == 0; {
		if time.Now().After(deadline) {
			t.Fatal("OnClose did not panic")
		}
		time.Sleep(time.Millisecond)
	}
	_ = events.Close(nil)
	select {
	case <-serveDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after a recovered OnClose panic")
	}
}

// A UDP callback may request another callback with Wake. That request must be
// handed to a later turn; otherwise a callback that keeps waking itself drains
// the queue forever and prevents Events.Close from joining the server turn.
func TestUDPSelfWakeDoesNotPinTheServerTurn(t *testing.T) {
	firstData := make(chan struct{}, 1)
	var callbacks atomic.Int32
	events := &Events{Pollers: 1}
	events.OnData = func(conn Conn) error {
		_, _ = conn.Discard(-1)
		if callbacks.Add(1) == 1 {
			firstData <- struct{}{}
		}
		_ = conn.Wake()
		return nil
	}
	addr := startRecoveringServer(t, events, "udp")
	client, err := net.Dial("udp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if _, err := client.Write([]byte("wake")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-firstData:
	case <-time.After(5 * time.Second):
		t.Fatal("UDP OnData did not run")
	}
	deadline := time.Now().Add(5 * time.Second)
	for callbacks.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if callbacks.Load() < 2 {
		t.Fatal("UDP Wake did not schedule a later turn")
	}
	_ = events.Close(nil)
}

// OnOutbound calls for one UDP connection never overlap, though its datagrams
// are sent both from its server's turn and from other goroutines; under -race
// this also covers the send itself, whose destination address every send to
// the peer shares.
func TestUDPOnOutboundCallsDoNotOverlap(t *testing.T) {
	child := make(chan Conn, 1)
	var in, overlap, calls atomic.Int32
	events := &Events{Pollers: 1}
	events.OnOpen = func(c Conn) {
		select {
		case child <- c:
		default:
		}
	}
	events.OnData = func(c Conn) error {
		_, _ = c.Write([]byte("reply"))
		return nil
	}
	events.OnOutbound = func(c Conn, n int) {
		if in.Add(1) > 1 {
			overlap.Add(1)
		}
		calls.Add(1)
		if calls.Load()%16 == 0 {
			// A send made here is reported once this call returns.
			_, _ = c.Write([]byte("nested"))
		}
		time.Sleep(50 * time.Microsecond)
		in.Add(-1)
	}
	addr := startRecoveringServer(t, events, "udp")
	client, err := net.Dial("udp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	go func() {
		buf := make([]byte, 64)
		for {
			if _, err := client.Read(buf); err != nil {
				return
			}
		}
	}()
	if _, err := client.Write([]byte("hi")); err != nil {
		t.Fatal(err)
	}
	var c Conn
	select {
	case c = <-child:
	case <-time.After(5 * time.Second):
		t.Fatal("UDP peer did not open")
	}
	var wg sync.WaitGroup
	stop := time.Now().Add(200 * time.Millisecond)
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for time.Now().Before(stop) {
				_, _ = c.Write([]byte("ext"))
			}
		}()
	}
	for time.Now().Before(stop) {
		_, _ = client.Write([]byte("hi"))
		time.Sleep(100 * time.Microsecond)
	}
	wg.Wait()
	if n := overlap.Load(); n != 0 {
		t.Fatalf("OnOutbound overlapped %d times for one UDP connection", n)
	}
	if calls.Load() == 0 {
		t.Fatal("OnOutbound never ran")
	}
}

// Two UDP peers whose OnOutbound writes to the other, both written to from
// their own goroutines: reporting a datagram never waits for another
// connection's OnOutbound, so the writers cannot deadlock.
func TestUDPOnOutboundCrossWritesDoNotDeadlock(t *testing.T) {
	opened := make(chan Conn, 2)
	var mu sync.Mutex
	other := map[Conn]Conn{}
	events := &Events{Pollers: 1}
	events.OnOpen = func(c Conn) { opened <- c }
	events.OnOutbound = func(c Conn, n int) {
		if n != 3 {
			return
		}
		mu.Lock()
		o := other[c]
		mu.Unlock()
		time.Sleep(time.Millisecond)
		_, _ = o.Write([]byte("x"))
	}
	addr := startRecoveringServer(t, events, "udp")
	c1, err := net.Dial("udp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c1.Close()
	c2, err := net.Dial("udp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	_, _ = c1.Write([]byte("a"))
	_, _ = c2.Write([]byte("b"))
	var a, b Conn
	for _, peer := range []*Conn{&a, &b} {
		select {
		case *peer = <-opened:
		case <-time.After(5 * time.Second):
			t.Fatal("UDP peers did not open")
		}
	}
	mu.Lock()
	other[a], other[b] = b, a
	mu.Unlock()
	done := make(chan struct{})
	go func() {
		var wg sync.WaitGroup
		for _, c := range []Conn{a, b} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; i < 200; i++ {
					_, _ = c.Write([]byte("ext"))
				}
			}()
		}
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("writers deadlocked in OnOutbound")
	}
}
