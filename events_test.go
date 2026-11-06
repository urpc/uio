package uio

import (
	"context"
	"errors"
	"github.com/urpc/uio/internal/readcontrol"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

func TestPublishedDialResultWinsConcurrentCancellation(t *testing.T) {
	for _, failure := range []error{nil, errors.New("handshake failed")} {
		for range 64 {
			ctx, cancel := context.WithCancel(context.Background())
			probe := &dialProbe{done: make(chan struct{})}
			transport := &contractTransport{}
			if failure == nil {
				probe.settle(transport)
			} else {
				probe.fail(failure)
			}
			cancel()
			conn, err := awaitDial(ctx, transport, probe)
			if !errors.Is(err, failure) || (failure == nil && conn != transport) {
				t.Fatalf("published dial = %v, %v, want %v", conn, err, failure)
			}
			if transport.state&StateClosed != 0 {
				t.Fatal("cancellation aborted an already published dial result")
			}
		}
	}
}

func TestCanceledDialAbortsTransportAndCannotPublishALateOpen(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	cause := errors.New("dial owner stopped waiting")
	cancel(cause)
	transport := &contractTransport{}
	probe := &dialProbe{done: make(chan struct{})}
	conn, err := awaitDial(ctx, transport, probe)
	if conn != nil || !errors.Is(err, cause) || !errors.Is(transport.closeCause, cause) {
		t.Fatalf("canceled dial = %v, %v, transport cause %v", conn, err, transport.closeCause)
	}
	if probe.settle(transport) {
		t.Fatal("late handshake opened a dial already canceled by its owner")
	}
}

func TestLayerDefaultsAcceptTypedNilHandlerAndDiscardInput(t *testing.T) {
	var handler *FuncHandler
	events := &Events{Handler: handler}
	entry, err := events.newEntry([]Layer{PassThroughLayer})
	if err != nil {
		t.Fatal(err)
	}
	transport := &contractTransport{input: []byte("unhandled input")}
	entry.cb.onOpen(transport)
	if err := entry.cb.onRead(transport); err != nil {
		t.Fatal(err)
	}
	c := stackOf(transport)
	if !c.topOpen.Load() || len(transport.input) != 0 || c.topConn().InboundBuffered() != 0 {
		t.Fatal("typed nil Handler did not use the default consuming handler")
	}
}

func TestDefaultReadHandlerConsumesEachBorrowedSocketChunk(t *testing.T) {
	observed := make(chan int, 2)
	events := &Events{Pollers: 1, ReadBufferSize: 1, Handler: FuncHandler{}, Observer: FuncHandler{BytesRead: func(c Conn, _ int) {
		observed <- c.InboundBuffered()
	}}}
	peer, _ := readControlServe(t, events)
	for _, payload := range []string{"a", "b"} {
		if _, err := peer.Write([]byte(payload)); err != nil {
			t.Fatal(err)
		}
		if buffered := readControlAwait(t, observed, "default handler input"); buffered != 1 {
			t.Fatalf("default handler retained an earlier chunk: %d bytes", buffered)
		}
	}
}

func TestPacketInputRejectsConsumerPauseWithoutInstallingAnOwner(t *testing.T) {
	started := make(chan struct{})
	events := &Events{Pollers: 1, Hooks: FuncHandler{Start: func(*Events) {
		close(started)
	}}}
	done := make(chan error, 1)
	go func() {
		done <- events.Serve()
	}()
	t.Cleanup(func() {
		_ = events.Close(nil)
		readControlAwait(t, done, "packet Events stopped")
	})
	readControlAwait(t, started, "packet Events started")
	conn, err := events.Dial("udp://127.0.0.1:9")
	if err != nil {
		t.Fatal(err)
	}
	if !errors.Is(conn.PauseRead(), ErrUnsupported) || !errors.Is(conn.ResumeRead(), ErrUnsupported) {
		t.Fatal("raw packet transport acquired stream-style pause semantics")
	}
	if conn.ReadPaused() || readcontrol.ControlOf(conn) != nil || readcontrol.NewPause(conn) != nil {
		t.Fatal("unsupported packet input retained a consumer owner")
	}
}

func expectPeerClosed(t *testing.T, conn net.Conn) {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	var buffer [1]byte
	_, err := conn.Read(buffer[:])
	if err == nil {
		t.Fatal("peer remained open")
	}
	if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
		t.Fatalf("peer was not closed: %v", err)
	}
	if !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
		var operationError *net.OpError
		if !errors.As(err, &operationError) {
			t.Fatalf("peer read = %v, want closed connection", err)
		}
	}
}

func TestAdoptClosesConnectionWhenEventsNotServing(t *testing.T) {
	client, server := tcpConnectionPair(t)
	defer client.Close()
	if conn, err := (&Events{}).Adopt(server); conn != nil || !errors.Is(err, net.ErrClosed) {
		t.Fatalf("Adopt() = %v, %v; want nil, net.ErrClosed", conn, err)
	}
	expectPeerClosed(t, client)
}

func tcpConnectionPair(t *testing.T) (net.Conn, net.Conn) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	accepted := make(chan net.Conn, 1)
	acceptErr := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			acceptErr <- err
			return
		}
		accepted <- conn
	}()
	client, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	select {
	case server := <-accepted:
		return client, server
	case err := <-acceptErr:
		client.Close()
		t.Fatal(err)
	case <-time.After(time.Second):
		client.Close()
		t.Fatal("listener did not accept connection")
	}
	return nil, nil
}

func TestDialRejectsAnotherEventsLoop(t *testing.T) {
	owner := currentGoroutineID()
	activeEventLoops.Store(owner, struct{}{})
	defer activeEventLoops.Delete(owner)
	events := &Events{}
	events.ready.Store(true)
	if _, err := events.DialContext(context.Background(), "tcp://127.0.0.1:1"); !errors.Is(err, ErrDialOnEventLoop) {
		t.Fatalf("DialContext from another Events loop = %v, want ErrDialOnEventLoop", err)
	}
}

func TestReadBufferPoolDoesNotAllocateOnReuse(t *testing.T) {
	events := &Events{ReadBufferSize: 16 * 1024}
	if err := events.initConfig(); err != nil {
		t.Fatal(err)
	}
	holder := events.readPool.Get()
	events.readPool.Put(holder)
	allocations := testing.AllocsPerRun(1000, func() {
		holder = events.readPool.Get()
		events.readPool.Put(holder)
	})
	if allocations != 0 {
		t.Fatalf("read buffer pool allocations = %.2f, want 0", allocations)
	}
}

func TestEventLifecycleHelpers(t *testing.T) {
	events := &Events{}
	waitDone := make(chan error, 1)
	go func() {
		waitDone <- events.Wait()
	}()
	select {
	case <-waitDone:
		t.Fatal("Wait returned before Close")
	case <-time.After(20 * time.Millisecond):
	}
	shutdownErr := errors.New("shutdown")
	if err := events.Close(shutdownErr); err != nil {
		t.Fatal(err)
	}
	if err := <-waitDone; !errors.Is(err, shutdownErr) {
		t.Fatalf("Wait error = %v, want %v", err, shutdownErr)
	}
	if err := events.Serve(); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("Serve after close = %v, want net.ErrClosed", err)
	}
}

func TestWaitIncludesOnStop(t *testing.T) {
	events := &Events{Pollers: 1}
	started := make(chan struct{})
	stopStarted := make(chan struct{})
	releaseStop := make(chan struct{})
	funcs(events).Start = func(*Events) {
		close(started)
	}
	funcs(events).Stop = func(*Events) {
		close(stopStarted)
		<-releaseStop
	}
	serveDone := make(chan error, 1)
	go func() {
		serveDone <- events.Serve()
	}()
	<-started
	shutdownErr := errors.New("wait for on-stop")
	if err := events.Close(shutdownErr); err != nil {
		t.Fatal(err)
	}
	<-stopStarted
	waitDone := make(chan error, 1)
	go func() {
		waitDone <- events.Wait()
	}()
	select {
	case <-waitDone:
		t.Fatal("Wait returned before OnStop")
	case <-time.After(20 * time.Millisecond):
	}
	close(releaseStop)
	if err := <-waitDone; !errors.Is(err, shutdownErr) {
		t.Fatalf("Wait error = %v, want %v", err, shutdownErr)
	}
	if err := <-serveDone; !errors.Is(err, shutdownErr) {
		t.Fatalf("Serve error = %v, want %v", err, shutdownErr)
	}
}

func TestServeInitializesListeners(t *testing.T) {
	for _, test := range []struct {
		name  string
		addrs []string
		want  int
	}{{name: "dial only", want: 0}, {name: "empty address", addrs: []string{""}, want: 0}, {name: "listener", addrs: []string{"tcp://127.0.0.1:0"}, want: 1}, {name: "listeners", addrs: []string{"tcp://127.0.0.1:0", "tcp://127.0.0.1:0"}, want: 2}} {
		t.Run(test.name, func(t *testing.T) {
			events := &Events{Pollers: 1}
			funcs(events).Start = func(events *Events) {
				if got := len(events.acceptor.listeners); got != test.want {
					t.Errorf("listeners = %d, want %d", got, test.want)
				}
				_ = events.Close(nil)
			}
			if err := events.Serve(test.addrs...); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestServeRollsBackListenersWhenLaterAddressFails(t *testing.T) {
	for attempt := 1; ; attempt++ {
		reserved, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr := reserved.Addr().String()
		if err = reserved.Close(); err != nil {
			t.Fatal(err)
		}
		events := &Events{Pollers: 1}
		if err = events.Serve(addr, "unsupported://address"); err == nil {
			t.Fatal("Serve accepted an unsupported second address")
		}
		if events.ready.Load() || !events.closing.Load() {
			t.Fatal("failed Serve left event loops serving")
		}
		listener, err := net.Listen("tcp", addr)
		if err == nil {
			_ = listener.Close()
			return
		}
		if attempt == 5 {
			t.Fatalf("first listener was not released: %v", err)
		}
	}
}

type liveFuncs struct{ fh *FuncHandler }

func (l *liveFuncs) OnOpen(c Conn) {
	l.fh.OnOpen(c)
}

func (l *liveFuncs) OnRead(c Conn) error {
	return l.fh.OnRead(c)
}

func (l *liveFuncs) OnTimer(c Conn) error {
	return l.fh.OnTimer(c)
}

func (l *liveFuncs) OnClose(c Conn, err error) {
	l.fh.OnClose(c, err)
}

func (l *liveFuncs) OnBytesRead(c Conn, n int) {
	l.fh.OnBytesRead(c, n)
}

func (l *liveFuncs) OnBytesWritten(c Conn, n int) {
	l.fh.OnBytesWritten(c, n)
}

func (l *liveFuncs) OnStart(ev *Events) {
	l.fh.OnStart(ev)
}

func (l *liveFuncs) OnStop(ev *Events) {
	l.fh.OnStop(ev)
}

func bareEntry() *entry {
	e, err := (&Events{}).newEntry(nil)
	if err != nil {
		panic(err)
	}
	return e
}

func funcs(ev *Events) *FuncHandler {
	if live, ok := ev.Handler.(*liveFuncs); ok {
		return live.fh
	}
	fh, ok := asFuncs(ev.Handler)
	if !ok || fh == nil {
		fh = &FuncHandler{}
	}
	live := &liveFuncs{fh: fh}
	ev.Handler, ev.Observer, ev.Hooks = live, live, live
	ev.cb = resolveCallbacks(live, live, live)
	return fh
}

func withFuncs(ev *Events, fh FuncHandler) *Events {
	ev.Handler, ev.Observer, ev.Hooks = fh, fh, fh
	ev.cb = resolveCallbacks(fh, fh, fh)
	return ev
}

func signal[T any](ch chan<- T, v T) {
	select {
	case ch <- v:
	default:
	}
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
}

type timerRecorder struct {
	mu    sync.Mutex
	order []string
}

func (r *timerRecorder) note(what string) {
	r.mu.Lock()
	r.order = append(r.order, what)
	r.mu.Unlock()
}

func (r *timerRecorder) sequence() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.order...)
}

func (r *timerRecorder) count(what string) int {
	n := 0
	for _, got := range r.sequence() {
		if got == what {
			n++
		}
	}
	return n
}

func waitForNote(t *testing.T, rec *timerRecorder, what string, want int, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		if got := rec.count(what); got >= want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s fired %d times within %v, want at least %d; ran %v", what, rec.count(what), within, want, rec.sequence())
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func TestHandlerTimerErrorCloses(t *testing.T) {
	wantErr := errors.New("timer boom")
	closed := make(chan struct{}, 1)
	started := make(chan struct{})
	events := &Events{Pollers: 1, Handler: FuncHandler{Read: func(Conn) error {
		return nil
	}, Timer: func(Conn) error {
		return wantErr
	}, Close: func(Conn, error) {
		select {
		case closed <- struct{}{}:
		default:
		}
	}}, Hooks: FuncHandler{Start: func(*Events) {
		close(started)
	}}}
	done := make(chan error, 1)
	go func() {
		done <- events.Serve()
	}()
	t.Cleanup(func() {
		_ = events.Close(nil)
		<-done
	})
	<-started
	listener, err := events.Listen("tcp://127.0.0.1:0")
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
	if err := conn.SetWakeDeadline(time.Now().Add(20 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("OnClose did not run after the OnTimer error")
	}
}

func TestHandlerTimerThroughLayers(t *testing.T) {
	rec := &timerRecorder{}
	started := make(chan struct{})
	events := &Events{Pollers: 1, Handler: FuncHandler{Read: func(Conn) error {
		rec.note("read")
		return nil
	}, Timer: func(Conn) error {
		rec.note("timer")
		return nil
	}, Close: func(Conn, error) {
	}}, Hooks: FuncHandler{Start: func(*Events) {
		close(started)
	}}}
	done := make(chan error, 1)
	go func() {
		done <- events.Serve()
	}()
	t.Cleanup(func() {
		_ = events.Close(nil)
		<-done
	})
	<-started
	listener, err := events.Listen("tcp://127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = listener.Close()
	})
	conn, err := events.Dial("tcp://"+listener.Addr().String(), WithLayers(PassThroughLayer))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = conn.Close()
	})
	if err := conn.SetWakeDeadline(time.Now().Add(30 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	waitForNote(t, rec, "timer", 1, 2*time.Second)
	if n := rec.count("timer"); n != 1 {
		t.Fatalf("OnTimer ran %d times through a stack, want 1", n)
	}
}

func serveAndDial(t *testing.T, handler Handler) Conn {
	t.Helper()
	started := make(chan struct{})
	events := &Events{Pollers: 1, Handler: handler, Hooks: FuncHandler{Start: func(*Events) {
		close(started)
	}}}
	done := make(chan error, 1)
	go func() {
		done <- events.Serve()
	}()
	t.Cleanup(func() {
		_ = events.Close(nil)
		<-done
	})
	<-started
	listener, err := events.Listen("tcp://127.0.0.1:0")
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
	return conn
}
