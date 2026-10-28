//go:build (linux || darwin || netbsd || freebsd || openbsd || dragonfly) && !stdio

package uio

import (
	"context"
	"errors"
	"io"
	"net"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
	"unsafe"

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
	funcs(events).Open = func(Conn) { enter("open"); inTurn.Add(-1) }
	funcs(events).Read = func(c Conn) error {
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

	conn := &fdConn{fd: int32(fds[0])}
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
	funcs(events).Start = func(ev *Events) {
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
	funcs(events).Open = func(Conn) { panic("OnOpen panics") }
	funcs(events).Read = func(conn Conn) error {
		_, _ = conn.Discard(-1)
		select {
		case gotData <- struct{}{}:
		default:
		}
		return nil
	}
	funcs(events).Close = func(Conn, error) { closed <- struct{}{} }
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
	funcs(events).Close = func(Conn, error) { panic("OnClose panics") }
	started := make(chan string, 1)
	funcs(events).Start = func(ev *Events) {
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
	funcs(events).Read = func(conn Conn) error {
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

// OnWrite calls for one UDP connection never overlap, though its datagrams
// are sent both from its server's turn and from other goroutines; under -race
// this also covers the send itself, whose destination address every send to
// the peer shares.
func TestUDPOnOutboundCallsDoNotOverlap(t *testing.T) {
	child := make(chan Conn, 1)
	var in, overlap, calls atomic.Int32
	events := &Events{Pollers: 1}
	funcs(events).Open = func(c Conn) {
		select {
		case child <- c:
		default:
		}
	}
	funcs(events).Read = func(c Conn) error {
		_, _ = c.Write([]byte("reply"))
		return nil
	}
	funcs(events).BytesWritten = func(c Conn, n int) {
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
		t.Fatalf("OnWrite overlapped %d times for one UDP connection", n)
	}
	if calls.Load() == 0 {
		t.Fatal("OnWrite never ran")
	}
}

// Two UDP peers whose OnWrite writes to the other, both written to from
// their own goroutines: reporting a datagram never waits for another
// connection's OnWrite, so the writers cannot deadlock.
func TestUDPOnOutboundCrossWritesDoNotDeadlock(t *testing.T) {
	opened := make(chan Conn, 2)
	var mu sync.Mutex
	other := map[Conn]Conn{}
	events := &Events{Pollers: 1}
	funcs(events).Open = func(c Conn) { opened <- c }
	funcs(events).BytesWritten = func(c Conn, n int) {
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
		t.Fatal("writers deadlocked in OnWrite")
	}
}

// acceptorLayoutName labels the two accept layouts the contract tests run
// under.
func acceptorLayoutName(reusePort bool) string {
	if reusePort {
		return "reuseport-listeners"
	}
	return "single-listener"
}

// TestOnOpenRunsInThePoolTurn pins the callback contract on the accept path:
// OnOpen is delivered by the connection's io-pool turn — never on the
// accepting loop — even after accepted-stream registration moved in place on
// platforms whose sockets inherit the listener's options.
func TestOnOpenRunsInThePoolTurn(t *testing.T) {
	for _, reusePort := range []bool{false, true} {
		t.Run(acceptorLayoutName(reusePort), func(t *testing.T) {
			testOnOpenRunsInThePoolTurn(t, reusePort)
		})
	}
}

func testOnOpenRunsInThePoolTurn(t *testing.T, reusePort bool) {
	events := &Events{Pollers: 2, ReusePort: reusePort}
	const dials = 8

	var opens atomic.Int64
	var notOwner atomic.Bool
	funcs(events).Open = func(conn Conn) {
		fdc := conn.(*fdConn)
		// runIOTask stamps the running worker; the open callback must see its
		// own goroutine there, which is what "inside a worker" means.
		if fdc.ioOwner.Load() != currentGoroutineID() {
			notOwner.Store(true)
		}
		opens.Add(1)
	}

	addrCh := make(chan string, 1)
	funcs(events).Start = func(ev *Events) {
		for _, l := range ev.acceptor.listeners {
			addrCh <- l.ln.Addr().String()
			return
		}
	}
	done := make(chan error, 1)
	go func() { done <- events.Serve("tcp://127.0.0.1:0") }()
	t.Cleanup(func() {
		_ = events.Close(nil)
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(3 * time.Second):
			t.Error("Serve did not stop")
		}
	})

	var addr string
	select {
	case addr = <-addrCh:
	case <-time.After(3 * time.Second):
		t.Fatal("Serve did not start")
	}

	var conns []net.Conn
	defer func() {
		for _, c := range conns {
			_ = c.Close()
		}
	}()
	for i := 0; i < dials; i++ {
		c, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		conns = append(conns, c)
	}

	deadline := time.Now().Add(5 * time.Second)
	for opens.Load() < dials && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if got := opens.Load(); got != dials {
		t.Fatalf("OnOpen ran %d times, want %d", got, dials)
	}
	if notOwner.Load() {
		t.Fatal("OnOpen ran outside the connection's io-pool turn")
	}
}

// TestAcceptShutdownRaceClosesEveryOpenedConn closes the server while dials
// are landing: whichever side of the in-place registration a connection is
// on, every connection that reached OnOpen must also reach OnClose before
// Serve returns, and Serve must return.
func TestAcceptShutdownRaceClosesEveryOpenedConn(t *testing.T) {
	for _, reusePort := range []bool{false, true} {
		t.Run(acceptorLayoutName(reusePort), func(t *testing.T) {
			testAcceptShutdownRace(t, reusePort)
		})
	}
}

func testAcceptShutdownRace(t *testing.T, reusePort bool) {
	for round := 0; round < 15; round++ {
		events := &Events{Pollers: 2, ReusePort: reusePort}
		var opened, closed atomic.Int64
		funcs(events).Open = func(Conn) { opened.Add(1) }
		funcs(events).Close = func(Conn, error) { closed.Add(1) }

		addrCh := make(chan string, 1)
		funcs(events).Start = func(ev *Events) {
			for _, l := range ev.acceptor.listeners {
				addrCh <- l.ln.Addr().String()
				return
			}
		}
		done := make(chan error, 1)
		go func() { done <- events.Serve("tcp://127.0.0.1:0") }()

		var addr string
		select {
		case addr = <-addrCh:
		case <-time.After(3 * time.Second):
			t.Fatalf("round %d: Serve did not start", round)
		}

		var wg sync.WaitGroup
		for i := 0; i < 16; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				c, err := net.Dial("tcp", addr)
				if err != nil {
					return
				}
				// Hold the connection so some of them are open when the
				// server starts closing.
				time.Sleep(time.Duration(i%4) * time.Millisecond)
				_ = c.Close()
			}(i)
		}
		// Vary the moment the shutdown lands inside the accept stream.
		time.Sleep(time.Duration(round%4) * time.Millisecond)
		_ = events.Close(nil)
		wg.Wait()

		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("round %d: Serve returned %v", round, err)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("round %d: Serve did not stop", round)
		}
		if o, c := opened.Load(), closed.Load(); o != c {
			t.Fatalf("round %d: %d connections opened but %d closed", round, o, c)
		}
	}
}

func TestConnectedUDPCallbacksStayOnOwningLoop(t *testing.T) {
	remote, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer remote.Close()

	openStarted := make(chan *net.UDPAddr, 1)
	releaseOpen := make(chan struct{})
	dataStarted := make(chan struct{})
	releaseData := make(chan struct{})
	secondData := make(chan struct{})
	var openOnce, dataOnce sync.Once
	releaseOpenFn := func() { openOnce.Do(func() { close(releaseOpen) }) }
	releaseDataFn := func() { dataOnce.Do(func() { close(releaseData) }) }
	defer releaseOpenFn()
	defer releaseDataFn()
	var callbacks atomic.Int32
	started := make(chan struct{}, 1)
	events := &Events{Pollers: 1}
	funcs(events).Start = func(*Events) { started <- struct{}{} }
	funcs(events).Open = func(conn Conn) {
		openStarted <- conn.LocalAddr().(*net.UDPAddr)
		<-releaseOpen
	}
	funcs(events).Read = func(conn Conn) error {
		switch callbacks.Add(1) {
		case 1:
			close(dataStarted)
			<-releaseData
		case 2:
			close(secondData)
		}
		_, _ = conn.Discard(-1)
		return nil
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- events.Serve() }()
	t.Cleanup(func() {
		releaseOpenFn()
		releaseDataFn()
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
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("Events did not start")
	}
	type dialResult struct {
		conn Conn
		err  error
	}
	dialed := make(chan dialResult, 1)
	go func() {
		conn, err := events.DialContext(context.Background(), "udp://"+remote.LocalAddr().String(), nil)
		dialed <- dialResult{conn: conn, err: err}
	}()
	var local *net.UDPAddr
	select {
	case local = <-openStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("UDP OnOpen did not start")
	}
	if _, err := remote.WriteToUDP([]byte("packet"), local); err != nil {
		t.Fatal(err)
	}
	select {
	case <-dataStarted:
		t.Fatal("UDP OnData overlapped OnOpen")
	case <-time.After(50 * time.Millisecond):
	}
	releaseOpenFn()
	var conn Conn
	select {
	case result := <-dialed:
		if result.err != nil {
			t.Fatal(result.err)
		}
		conn = result.conn
	case <-time.After(5 * time.Second):
		t.Fatal("UDP dial did not complete")
	}
	defer conn.Close()
	select {
	case <-dataStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("UDP OnData did not start")
	}
	if err := conn.Wake(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-secondData:
		t.Fatal("UDP Wake overlapped a packet callback")
	case <-time.After(50 * time.Millisecond):
	}
	releaseDataFn()
	select {
	case <-secondData:
	case <-time.After(5 * time.Second):
		t.Fatal("UDP Wake callback was not delivered")
	}
}

func TestUDPCallbackQueuesWriteToSameLoopStream(t *testing.T) {
	udpServer, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer udpServer.Close()

	var target atomic.Pointer[fdConn]
	var tcpSender, udpCallbackGoid atomic.Int64
	var sentInUDPCallback atomic.Bool
	var releaseOnce sync.Once
	releaseTCP := make(chan struct{})
	release := func() { releaseOnce.Do(func() { close(releaseTCP) }) }
	defer release()

	started := make(chan string, 1)
	tcpOpened := make(chan *fdConn, 1)
	udpOpened := make(chan struct{}, 1)
	tcpCallback := make(chan struct{}, 1)
	udpWrite := make(chan error, 1)
	events := &Events{Pollers: 1}
	funcs(events).Start = func(events *Events) {
		for _, listener := range events.acceptor.listeners {
			started <- listener.pair.local.String()
			return
		}
	}
	funcs(events).Open = func(conn Conn) {
		fdc := conn.(*fdConn)
		if fdc.isDatagram() {
			udpOpened <- struct{}{}
		} else {
			tcpOpened <- fdc
		}
	}
	funcs(events).Read = func(conn Conn) error {
		if conn.(*fdConn).isDatagram() {
			udpCallbackGoid.Store(currentGoroutineID())
			_, err := target.Load().Write([]byte("cross"))
			_, _ = conn.Discard(-1)
			udpCallbackGoid.Store(0)
			udpWrite <- err
			return nil
		}
		tcpCallback <- struct{}{}
		<-releaseTCP
		_, _ = conn.Discard(-1)
		return nil
	}
	funcs(events).BytesWritten = func(conn Conn, written int) {
		if fdc := conn.(*fdConn); fdc == target.Load() && written > 0 {
			sender := currentGoroutineID()
			if sender == udpCallbackGoid.Load() {
				sentInUDPCallback.Store(true)
			}
			tcpSender.Store(sender)
		}
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- events.Serve("tcp://127.0.0.1:0") }()
	t.Cleanup(func() {
		release()
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

	var tcpAddr string
	select {
	case tcpAddr = <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("listener did not start")
	}
	tcpClient, err := net.Dial("tcp", tcpAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer tcpClient.Close()
	var tcpConn *fdConn
	select {
	case tcpConn = <-tcpOpened:
	case <-time.After(5 * time.Second):
		t.Fatal("TCP OnOpen was not called")
	}
	target.Store(tcpConn)

	udpConn, err := events.Dial("udp://"+udpServer.LocalAddr().String(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer udpConn.Close()
	select {
	case <-udpOpened:
	case <-time.After(5 * time.Second):
		t.Fatal("UDP OnOpen was not called")
	}
	if _, err = tcpClient.Write([]byte("start")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-tcpCallback:
	case <-time.After(5 * time.Second):
		t.Fatal("TCP task did not enter OnData")
	}
	if _, err = udpServer.WriteToUDP([]byte("trigger"), udpConn.LocalAddr().(*net.UDPAddr)); err != nil {
		t.Fatal(err)
	}
	select {
	case err = <-udpWrite:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("UDP callback did not complete its cross-connection write")
	}
	release()
	if err = tcpClient.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	var response [5]byte
	if _, err = io.ReadFull(tcpClient, response[:]); err != nil || string(response[:]) != "cross" {
		t.Fatalf("TCP response = %q, %v", response, err)
	}
	// The stream's bytes must leave through its own sender, never through a
	// syscall made inside the UDP callback. The worker that ran the callback
	// may run the stream's write turn afterwards, which is a turn of its own.
	// OnWrite follows the send, so it may still be on its way after the
	// client has the bytes.
	deadline := time.Now().Add(5 * time.Second)
	for tcpSender.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if tcpSender.Load() == 0 {
		t.Fatal("stream bytes were not reported sent")
	}
	if sentInUDPCallback.Load() {
		t.Fatal("stream bytes were sent inside the UDP callback")
	}
}

// TestStopRaceKeepsClaimReserved pins the ordering inside noteIO: a turn's
// lifetime reservation is held before its scheduling claim becomes visible, so
// the stop barrier always joins every published claim. With the claim
// published first, a stop landing between the two would let shutdown hand the
// close to a claim that can never become a task, and Serve could return with
// the connection still open.
func TestStopRaceKeepsClaimReserved(t *testing.T) {
	ep, err := poller.New()
	if err != nil {
		t.Fatal(err)
	}
	defer ep.Close(nil)
	sockets, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(sockets[1])

	onClose := make(chan struct{})
	events := &Events{}
	funcs(events).Close = func(Conn, error) { close(onClose) }
	// The final close callback is delivered as a pooled turn, so the loop
	// needs a real pool.
	pool := newIOTaskPool(nil)
	defer pool.stop()
	loop := &eventLoop{
		events: events,
		poller: ep,
		fdMap:  newFdMap(),
		ioPool: pool,
		ioIdle: make(chan struct{}),
	}
	conn := &fdConn{fd: int32(sockets[0]), commonConn: commonConn{events: events, loop: loop}}
	if err := loop.fdMap.Put(int(conn.fd), conn); err != nil {
		t.Fatal(err)
	}

	paused, resume := make(chan struct{}), make(chan struct{})
	testHookTaskClaimed = func(c *fdConn) {
		if c != conn {
			return
		}
		close(paused)
		<-resume
	}
	defer func() { testHookTaskClaimed = nil }()

	claimed := make(chan bool, 1)
	go func() { claimed <- conn.noteIO(ioEventRead) }()
	select {
	case <-paused:
	case <-time.After(time.Second):
		t.Fatal("admission probe did not pause")
	}
	// The claim is visible now; the stop barrier must be able to see this turn.
	if loop.ioTurnCount() == 0 {
		close(resume)
		t.Fatal("scheduling claim was published without a turn reservation")
	}

	shutdownDone := make(chan struct{})
	go func() {
		loop.shutdown(io.EOF)
		close(shutdownDone)
	}()
	select {
	case <-shutdownDone:
		close(resume)
		t.Fatal("shutdown completed while a claimed turn was reserved")
	case <-time.After(50 * time.Millisecond):
	}
	close(resume)
	if !<-claimed {
		t.Fatal("noteIO did not claim the reserved turn")
	}
	// The pooled turn can never run after the stop; the refusal path ends its
	// turn the way a rejected submission does.
	conn.handleIOSubmitFailure(net.ErrClosed)
	select {
	case <-shutdownDone:
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown did not finish after the refused turn")
	}
	if !conn.close.isReleased() {
		t.Fatal("shutdown returned without releasing the connection")
	}
	if _, err := unix.FcntlInt(uintptr(conn.fd), unix.F_GETFD, 0); err != unix.EBADF {
		t.Fatalf("connection fd still open after shutdown: err = %v", err)
	}
	select {
	case <-onClose:
	case <-time.After(5 * time.Second):
		t.Fatal("OnClose was never delivered")
	}
}

// TestShutdownJoinsPendingWake drives the lifecycle end to end: an external
// Wake parked in noteIO's claim window while Events.Close runs. Serve must not
// return before the stop barrier has joined that turn: either the reservation
// holds the barrier until the producer resumes, or the barrier's close pass
// already completed the teardown itself.
func TestShutdownJoinsPendingWake(t *testing.T) {
	ev := &Events{Pollers: 1}
	started := make(chan struct{})
	opened := make(chan *fdConn, 1)
	closed := make(chan struct{})
	funcs(ev).Start = func(*Events) { close(started) }
	funcs(ev).Open = func(conn Conn) { opened <- conn.(*fdConn) }
	funcs(ev).Close = func(Conn, error) { close(closed) }
	serveDone := make(chan error, 1)
	go func() { serveDone <- ev.Serve() }()
	<-started

	client, server := tcpConnectionPair(t)
	defer client.Close()
	if _, err := ev.Adopt(server, nil); err != nil {
		t.Fatal(err)
	}
	conn := <-opened
	for deadline := time.Now().Add(time.Second); conn.taskState.Load()&taskScheduledBit != 0; {
		if time.Now().After(deadline) {
			t.Fatal("OnOpen did not finish")
		}
		time.Sleep(time.Millisecond)
	}

	paused, resume := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(resume) }) }
	wakeDone := make(chan error, 1)
	testHookTaskClaimed = func(c *fdConn) {
		if c == conn {
			close(paused)
			<-resume
		}
	}
	defer func() {
		release()
		<-wakeDone
		testHookTaskClaimed = nil
	}()
	go func() { wakeDone <- conn.Wake() }()
	select {
	case <-paused:
	case <-time.After(time.Second):
		t.Fatal("wake admission did not pause")
	}
	if conn.loop.ioTurnCount() == 0 {
		t.Fatal("claim visible without its turn reservation behind it")
	}

	ev.Close(nil)
	select {
	case err := <-serveDone:
		if err != nil && err != net.ErrClosed {
			t.Error(err)
		}
		select {
		case <-closed:
		default:
			t.Error("Serve returned before OnClose: pending Wake claim was not joined")
		}
		if !conn.close.isReleased() {
			t.Error("Serve returned with the connection fd still owned")
		}
		if _, err := unix.FcntlInt(uintptr(conn.Fd()), unix.F_GETFD, 0); err != unix.EBADF {
			t.Errorf("Serve returned with socket fd still open: F_GETFD error = %v", err)
		}
	case <-time.After(100 * time.Millisecond):
		// The barrier is waiting on the parked turn; releasing it must finish
		// the lifecycle.
		release()
		select {
		case <-serveDone:
		case <-time.After(3 * time.Second):
			t.Fatal("Serve did not finish after Wake admission resumed")
		}
		select {
		case <-closed:
		case <-time.After(3 * time.Second):
			t.Fatal("OnClose was never delivered")
		}
	}
}

// A close with one cause reports that cause itself, not a join wrapping it.
func TestJoinCloseErrorsKeepsALoneCause(t *testing.T) {
	cause := errors.New("cause")
	for _, errs := range [][3]error{{cause, nil, nil}, {nil, cause, nil}, {nil, nil, cause}} {
		if got := joinCloseErrors(errs[0], errs[1], errs[2]); got != cause {
			t.Fatalf("joinCloseErrors(%v) = %v, want the cause itself", errs, got)
		}
	}
	if joinCloseErrors(nil, nil, nil) != nil {
		t.Fatal("no cause joined into an error")
	}
	other := errors.New("other")
	joined := joinCloseErrors(cause, nil, other)
	if !errors.Is(joined, cause) || !errors.Is(joined, other) {
		t.Fatalf("joinCloseErrors with two causes = %v, want both", joined)
	}
}

// The shared requests for common causes are never modified: a later cause
// replaces the request instead.
func TestCloseRequestsForCommonCausesAreShared(t *testing.T) {
	for _, cause := range []error{nil, io.EOF, syscall.ECONNRESET} {
		if newCloseRequest(cause) != newCloseRequest(cause) {
			t.Fatalf("requests for %v are allocated per close", cause)
		}
	}
	var conn fdConn
	conn.setDeferredCloseLocked(nil)
	shared := conn.close.deferred
	cause := errors.New("later")
	conn.setDeferredCloseLocked(cause)
	if shared.cause != nil || conn.close.deferred == shared || conn.takeDeferredCloseCause() != cause {
		t.Fatal("recording a later cause modified the shared request")
	}
	conn.setDeferredCloseLocked(io.EOF)
	conn.setDeferredCloseLocked(errors.New("second"))
	if got := conn.takeDeferredCloseCause(); got != io.EOF || closeAtEOF.cause != io.EOF {
		t.Fatalf("the first cause was replaced: %v", got)
	}
}

func TestUnixConnectionColdStateIsLazy(t *testing.T) {
	if unsafe.Sizeof(uintptr(0)) == 8 {
		// 256 is a size class whose objects start on a cache line, so the
		// fields a turn touches stay within the first lines of every conn.
		// Race-enabled builds add the inbound-scope diagnostics' word.
		maximum := uintptr(256) + (unsafe.Sizeof(inboundScope{})+7)&^7
		if size := unsafe.Sizeof(fdConn{}); size > maximum {
			t.Fatalf("fdConn size = %d bytes, want at most %d", size, maximum)
		}
	}

	conn := &fdConn{}
	if conn.udp != nil || conn.deadlines != nil || conn.isDatagram() {
		t.Fatal("new TCP connection allocated cold state")
	}

	var deadlineErr error
	allocations := testing.AllocsPerRun(1000, func() {
		deadlineErr = conn.applyDeadline(deadlineBoth, time.Time{})
	})
	if deadlineErr != nil {
		t.Fatal(deadlineErr)
	}
	if allocations != 0 || conn.deadlines != nil {
		t.Fatalf("clearing unused deadlines allocated %.2f objects", allocations)
	}

	if err := conn.applyDeadline(deadlineRead, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if conn.deadlines == nil || conn.deadlines.readTimer == nil {
		t.Fatal("nonzero deadline did not initialize deadline state")
	}
	conn.stopDeadlines()

	conn.udp = &unixUDPState{}
	if !conn.isDatagram() {
		t.Fatal("UDP state did not mark connection as datagram")
	}
}

// Serve returns with a UDP listener's address free: the socket's close runs
// off the loop, and Serve waits for it as Listener.Close does.
func TestServeReturnsWithUDPAddressFree(t *testing.T) {
	previous := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(previous)
	for round := range 100 {
		var addr string
		events := &Events{Pollers: 1}
		events.Hooks = FuncHandler{Start: func(ev *Events) {
			listener, err := ev.Listen("udp4://127.0.0.1:0", nil)
			if err != nil {
				t.Error(err)
			} else {
				addr = listener.Addr().String()
			}
			_ = ev.Close(nil)
		}}
		if err := events.Serve(); err != nil {
			t.Fatal(err)
		}
		if addr == "" {
			t.FailNow()
		}
		probe, err := net.ListenPacket("udp4", addr)
		if err != nil {
			t.Fatalf("round %d: Serve returned with %s still bound: %v", round, addr, err)
		}
		_ = probe.Close()
	}
}

// A Listener.Close beside Events.Close: the shutdown finds the socket's close
// already under way and leaves it to that caller, and Serve still waits for
// it — the close is counted before anyone can find it — so Serve returns
// with the address free.
func TestServeWaitsForAListenerCloseUnderWay(t *testing.T) {
	previous := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(previous)
	started := make(chan struct{})
	events := &Events{Pollers: 1, Hooks: FuncHandler{Start: func(*Events) { close(started) }}}
	served := make(chan error, 1)
	go func() { served <- events.Serve() }()
	<-started
	listener, err := events.Listen("udp4://127.0.0.1:0", nil)
	if err != nil {
		t.Fatal(err)
	}
	published, release := make(chan struct{}), make(chan struct{})
	hold := func(*fdConn) { close(published); <-release }
	testHookSocketClosePublished.Store(&hold)
	defer testHookSocketClosePublished.Store(nil)
	closed := make(chan struct{})
	go func() { _ = listener.Close(); close(closed) }()
	select {
	case <-published:
	case <-time.After(5 * time.Second):
		t.Fatal("the Listener.Close never published its close")
	}
	testHookSocketClosePublished.Store(nil)
	_ = events.Close(nil)
	select {
	case <-served:
		close(release)
		t.Fatal("Serve returned while the socket's close was still to run")
	case <-time.After(200 * time.Millisecond):
	}
	close(release)
	<-closed
	if err := <-served; err != nil {
		t.Fatal(err)
	}
	probe, err := net.ListenPacket("udp4", listener.Addr().String())
	if err != nil {
		t.Fatalf("Serve returned with the address still bound: %v", err)
	}
	_ = probe.Close()
}
