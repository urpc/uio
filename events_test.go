package uio

import (
	"context"
	"errors"
	"io"
	"net"
	"runtime"
	"testing"
	"time"
)

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

func TestAdoptTransfersTCPConnection(t *testing.T) {
	events := &Events{Pollers: 1}
	started := make(chan struct{})
	opened := make(chan Conn, 1)
	received := make(chan string, 1)
	funcs(events).Start = func(*Events) { close(started) }
	funcs(events).Open = func(conn Conn) { opened <- conn }
	funcs(events).Data = func(conn Conn) error {
		payload := make([]byte, conn.InboundBuffered())
		_, err := conn.Read(payload)
		if err == nil {
			received <- string(payload)
		}
		return err
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- events.Serve() }()
	t.Cleanup(func() {
		_ = events.Close(nil)
		select {
		case <-serveDone:
		case <-time.After(time.Second):
			t.Error("Events.Serve did not stop")
		}
	})
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("Events.Serve did not start")
	}

	client, server := tcpConnectionPair(t)
	defer client.Close()
	const userdata = "adopted"
	adopted, err := events.Adopt(server, userdata)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case conn := <-opened:
		if conn != adopted || conn.Userdata() != userdata {
			t.Fatalf("OnOpen connection = %v/%v, want adopted userdata", conn, conn.Userdata())
		}
	case <-time.After(time.Second):
		t.Fatal("Adopt did not call OnOpen")
	}

	if _, err = client.Write([]byte("payload")); err != nil {
		t.Fatal(err)
	}
	select {
	case payload := <-received:
		if payload != "payload" {
			t.Fatalf("received payload = %q", payload)
		}
	case <-time.After(time.Second):
		t.Fatal("adopted connection did not receive data")
	}
}

func TestAdoptClosesConnectionWhenEventsNotServing(t *testing.T) {
	client, server := tcpConnectionPair(t)
	defer client.Close()
	if conn, err := (&Events{}).Adopt(server, nil); conn != nil || !errors.Is(err, net.ErrClosed) {
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

func TestPollerCountDefaultsAndLimits(t *testing.T) {
	procs := runtime.GOMAXPROCS(0)
	for _, test := range []struct {
		name       string
		configured int
		want       int
	}{
		{name: "zero", want: min(defaultPollers(procs), procs)},
		{name: "negative", configured: -1, want: min(defaultPollers(procs), procs)},
		{name: "explicit", configured: 1, want: 1},
		{name: "above CPU count", configured: procs + 1, want: procs},
	} {
		t.Run(test.name, func(t *testing.T) {
			events := &Events{Pollers: test.configured}
			if err := events.initConfig(); err != nil {
				t.Fatal(err)
			}
			if events.Pollers != test.want {
				t.Fatalf("Pollers = %d, want %d", events.Pollers, test.want)
			}
		})
	}
}

func TestDialRejectsAnotherEventsLoop(t *testing.T) {
	owner := currentGoroutineID()
	activeEventLoops.Store(owner, struct{}{})
	defer activeEventLoops.Delete(owner)
	events := &Events{}
	events.ready.Store(true)
	if _, err := events.DialContext(context.Background(), "tcp://127.0.0.1:1", nil); !errors.Is(err, ErrDialOnEventLoop) {
		t.Fatalf("DialContext from another Events loop = %v, want ErrDialOnEventLoop", err)
	}
}

func TestEventLoopShutdownClosesIOAdmission(t *testing.T) {
	loop := &eventLoop{ioIdle: make(chan struct{})}
	if !loop.acquireIO() {
		t.Fatal("loop rejected I/O before shutdown")
	}
	released := false
	defer func() {
		if !released {
			loop.releaseIO()
		}
	}()
	stopped := make(chan struct{})
	go func() {
		loop.stopIO()
		close(stopped)
	}()
	deadline := time.Now().Add(time.Second)
	for !loop.ioStopped() {
		if time.Now().After(deadline) {
			t.Fatal("loop did not close I/O admission")
		}
		runtime.Gosched()
	}
	if loop.acquireIO() {
		loop.releaseIO()
		t.Fatal("loop accepted I/O after stop")
	}
	select {
	case <-stopped:
		t.Fatal("shutdown returned before active I/O completed")
	default:
	}
	loop.releaseIO()
	released = true
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not complete after I/O returned")
	}
	loop.acquireCloseIO()
	loop.releaseIO()
}

func TestReadBufferPoolDoesNotAllocateOnReuse(t *testing.T) {
	events := &Events{MaxBufferSize: 16 * 1024}
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
	go func() { waitDone <- events.Wait() }()
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
	funcs(events).Start = func(*Events) { close(started) }
	funcs(events).Stop = func(*Events) {
		close(stopStarted)
		<-releaseStop
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- events.Serve() }()
	<-started
	shutdownErr := errors.New("wait for on-stop")
	if err := events.Close(shutdownErr); err != nil {
		t.Fatal(err)
	}
	<-stopStarted
	waitDone := make(chan error, 1)
	go func() { waitDone <- events.Wait() }()
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

func TestDialContextCancellationBeforeNetworkDial(t *testing.T) {
	started := make(chan struct{})
	events := withFuncs(&Events{Pollers: 1}, FuncHandler{Start: func(*Events) { close(started) }})
	serveDone := make(chan error, 1)
	go func() { serveDone <- events.Serve() }()
	defer func() {
		_ = events.Close(nil)
		select {
		case <-serveDone:
		case <-time.After(5 * time.Second):
			t.Error("Events.Serve did not stop")
		}
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("Events.Serve did not start")
	}

	cause := errors.New("dial canceled")
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(cause)
	conn, err := events.DialContext(ctx, "tcp://127.0.0.1:1", nil)
	if conn != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("DialContext() = %v, %v; want nil, cancellation", conn, err)
	}
}

func TestServeInitializesListeners(t *testing.T) {
	for _, test := range []struct {
		name  string
		addrs []string
		want  int
	}{
		{name: "dial only", want: 0},
		{name: "empty address", addrs: []string{""}, want: 0},
		{name: "listener", addrs: []string{"tcp://127.0.0.1:0"}, want: 1},
		{name: "listeners", addrs: []string{"tcp://127.0.0.1:0", "tcp://127.0.0.1:0"}, want: 2},
	} {
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

func TestServeAcceptsConnectionsOnAllAddresses(t *testing.T) {
	events := &Events{Pollers: 1}
	started := make(chan []string, 1)
	funcs(events).Start = func(events *Events) {
		addrs := make([]string, 0, len(events.acceptor.listeners))
		for _, listener := range events.acceptor.listeners {
			addrs = append(addrs, listener.ln.Addr().String())
		}
		started <- addrs
	}
	funcs(events).Data = func(conn Conn) error {
		_, err := conn.WriteTo(conn)
		return err
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- events.Serve("tcp://127.0.0.1:0", "tcp://127.0.0.1:0") }()
	t.Cleanup(func() {
		_ = events.Close(nil)
		select {
		case err := <-serveDone:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(2 * time.Second):
			t.Error("Serve did not stop")
		}
	})

	var addrs []string
	select {
	case addrs = <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not start")
	}
	if len(addrs) != 2 || addrs[0] == addrs[1] {
		t.Fatalf("listening addresses = %v, want two distinct addresses", addrs)
	}
	for _, addr := range addrs {
		conn, err := net.DialTimeout("tcp", addr, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if err = conn.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
			conn.Close()
			t.Fatal(err)
		}
		if _, err = conn.Write([]byte("ping")); err != nil {
			conn.Close()
			t.Fatal(err)
		}
		var response [4]byte
		_, err = io.ReadFull(conn, response[:])
		conn.Close()
		if err != nil || string(response[:]) != "ping" {
			t.Fatalf("echo on %s = %q, %v", addr, response, err)
		}
	}
}

func TestServeRollsBackListenersWhenLaterAddressFails(t *testing.T) {
	// The port is free between the probe and Serve and again after the
	// rollback, so another process may bind it meanwhile; a listener the
	// rollback leaked fails every attempt.
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
