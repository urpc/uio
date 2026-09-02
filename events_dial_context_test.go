package uio

import (
	"context"
	"errors"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

func TestDialContextCancelsQueuedRegistration(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	events := &Events{Pollers: 1}
	started := make(chan struct{})
	firstOpen := make(chan struct{})
	releaseOpen := make(chan struct{})
	var opens atomic.Int32
	events.OnStart = func(*Events) { close(started) }
	events.OnOpen = func(Conn) {
		if opens.Add(1) == 1 {
			close(firstOpen)
			<-releaseOpen
		}
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- events.Serve() }()
	t.Cleanup(func() {
		select {
		case <-releaseOpen:
		default:
			close(releaseOpen)
		}
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

	accepted := make(chan net.Conn, 2)
	acceptErr := make(chan error, 1)
	go func() {
		for range 2 {
			conn, acceptError := listener.Accept()
			if acceptError != nil {
				acceptErr <- acceptError
				return
			}
			accepted <- conn
		}
	}()

	firstResult := make(chan struct {
		conn Conn
		err  error
	}, 1)
	firstCtx, cancelFirst := context.WithCancelCause(context.Background())
	defer cancelFirst(context.Canceled)
	go func() {
		conn, dialErr := events.DialContext(firstCtx, "tcp://"+listener.Addr().String(), nil)
		firstResult <- struct {
			conn Conn
			err  error
		}{conn: conn, err: dialErr}
	}()
	serverFirst := waitAccepted(t, accepted, acceptErr)
	defer serverFirst.Close()
	select {
	case <-firstOpen:
	case <-time.After(time.Second):
		t.Fatal("first OnOpen did not block the event loop")
	}
	firstCancelErr := errors.New("cancel during OnOpen")
	cancelFirst(firstCancelErr)
	select {
	case result := <-firstResult:
		if result.conn != nil || !errors.Is(result.err, firstCancelErr) {
			t.Fatalf("first DialContext() = %v, %v; want nil, cancellation", result.conn, result.err)
		}
	case <-time.After(time.Second):
		t.Fatal("first DialContext did not return while OnOpen was blocked")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	second, err := events.DialContext(ctx, "tcp://"+listener.Addr().String(), nil)
	if second != nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second DialContext() = %v, %v; want nil, deadline exceeded", second, err)
	}
	serverSecond := waitAccepted(t, accepted, acceptErr)
	defer serverSecond.Close()
	close(releaseOpen)
	expectPeerClosed(t, serverFirst)
	expectPeerClosed(t, serverSecond)
	if got := opens.Load(); got != 1 {
		t.Fatalf("OnOpen calls = %d, want 1", got)
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
		t.Fatal("canceled registration peer remained open")
	}
	if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
		t.Fatalf("canceled registration peer was not closed: %v", err)
	}
	if !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
		var operationError *net.OpError
		if !errors.As(err, &operationError) {
			t.Fatalf("canceled registration peer read = %v, want closed connection", err)
		}
	}
}

func waitAccepted(t *testing.T, accepted <-chan net.Conn, acceptErr <-chan error) net.Conn {
	t.Helper()
	select {
	case conn := <-accepted:
		return conn
	case err := <-acceptErr:
		t.Fatal(err)
	case <-time.After(time.Second):
		t.Fatal("listener did not accept connection")
	}
	return nil
}
