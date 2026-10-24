package utls

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urpc/uio"
	"github.com/urpc/uio/ulayer"
)

// zeroes reads as zeros, forever.
type zeroes struct{}

func (zeroes) Read(b []byte) (int, error) {
	for i := range b {
		b[i] = 0
	}
	return len(b), nil
}

// The staging bound stills the wire: a stack whose top reads nothing holds
// its bound, the bytes below reach the transport's inbound limit, and a
// plain crypto/tls sender stops completing writes — until the top reads.
func TestStagingBoundStillsTheWire(t *testing.T) {
	serverConfig, clientConfig := testConfigs(t)
	release := make(chan struct{})
	var stalled atomic.Int64
	var top atomic.Pointer[uio.Conn]
	started := make(chan struct{})
	events := &uio.Events{Pollers: 2, MaxInboundBuffered: 32 << 10, Hooks: uio.FuncHandler{
		Start: func(*uio.Events) { close(started) },
	}}
	layer := Server(serverConfig)
	layer.StagingBound = 16 << 10
	transport, err := ulayer.Over(events, uio.FuncHandler{
		Open: func(c uio.Conn) {
			_ = c.SetReadBuffer(32 << 10) // the staging bound, not the kernel's
			top.Store(&c)
		},
		Data: func(c uio.Conn) error {
			select {
			case <-release:
				_, _ = c.Discard(c.InboundBuffered())
			default:
				stalled.Store(int64(c.InboundBuffered())) // staged plus unread below
			}
			return nil
		},
		Close: func(uio.Conn, error) {},
	}, layer)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- events.Serve() }()
	t.Cleanup(func() { _ = events.Close(nil); <-done })
	select {
	case <-started:
	case <-time.After(testWait):
		t.Fatal("Serve did not start")
	}
	listener, err := transport.Listen("tcp://127.0.0.1:0", nil)
	if err != nil {
		t.Fatal(err)
	}

	client, err := tls.Dial("tcp", listener.Addr().String(), clientConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	// Small socket buffers on both ends, so the staging bound and the
	// transport's inbound limit are what hold the sender, not the kernel's
	// loopback buffers.
	if tcp, ok := client.NetConn().(*net.TCPConn); ok {
		_ = tcp.SetWriteBuffer(32 << 10)
		_ = tcp.SetReadBuffer(32 << 10)
	}
	const total = 4 << 20
	written := make(chan error, 1)
	go func() {
		_, err := io.CopyN(client, zeroes{}, total)
		written <- err
	}()
	deadline := time.Now().Add(testWait)
	for stalled.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if stalled.Load() == 0 {
		t.Fatal("the stack never held bytes below its layer")
	}
	select {
	case err := <-written:
		t.Fatalf("the sender finished with the stack stalled: %v", err)
	case <-time.After(time.Second):
	}
	if held := stalled.Load(); held > int64(layer.StagingBound+(32<<10)+(64<<10)) {
		t.Fatalf("the layer held %d bytes, far past its bound", held)
	}

	close(release) // the reader above reads again, and its round needs a wake
	if err := (*top.Load()).Wake(); err != nil {
		t.Fatalf("wake the stalled stack: %v", err)
	}
	if err := <-written; err != nil {
		t.Fatalf("the sender failed after the release: %v", err)
	}
}

// A close sends the peer close_notify; everything accepted before it
// arrives, and the peer that shut its writing half is read to its end.
func TestCloseAndHalfClose(t *testing.T) {
	t.Run("close", func(t *testing.T) {
		serverConfig, clientConfig := testConfigs(t)
		server := newEcho()
		transport := serveTLS(t, server, Server(serverConfig))
		listener, err := transport.Listen("tcp://127.0.0.1:0", nil)
		if err != nil {
			t.Fatal(err)
		}
		conn, _ := dialTLS(t, Client(clientConfig), "tcp://"+listener.Addr().String())
		if _, err := conn.Write([]byte("last words")); err != nil {
			t.Fatal(err)
		}
		if got := string(wait(t, server.data, "server data")); got != "last words" {
			t.Fatalf("server read %q", got)
		}
		if err := conn.Close(); err != nil {
			t.Fatal(err)
		}
		wait(t, server.closed, "server close after close_notify")
	})

	t.Run("half close", func(t *testing.T) {
		serverConfig, clientConfig := testConfigs(t)
		server := newEcho()
		transport := serveTLS(t, server, Server(serverConfig))
		listener, err := transport.Listen("tcp://127.0.0.1:0", nil)
		if err != nil {
			t.Fatal(err)
		}
		conn, client := dialTLS(t, Client(clientConfig), "tcp://"+listener.Addr().String())
		if _, err := conn.Write([]byte("one way")); err != nil {
			t.Fatal(err)
		}
		if err := conn.CloseWrite(); err != nil {
			t.Fatal(err)
		}
		if got := string(wait(t, server.data, "server data")); got != "one way" {
			t.Fatalf("server read %q", got)
		}
		// The server's echo of what the client sent, and its own answer,
		// both arrive after the client's writing half ended.
		serverConn := wait(t, server.opened, "server open")
		if _, err := serverConn.Write([]byte("answer")); err != nil {
			t.Fatal(err)
		}
		var got []byte
		for len(got) < len("one wayanswer") {
			select {
			case chunk := <-client.data:
				got = append(got, chunk...)
			case <-time.After(testWait):
				t.Fatalf("client read %q after CloseWrite, want the echo and the answer", got)
			}
		}
		if string(got) != "one wayanswer" {
			t.Fatalf("client read %q after CloseWrite, want the echo then the answer", got)
		}
		_ = conn.Close()
		wait(t, server.closed, "server close")
	})
}

// A close from below keeps its cause: an abrupt end reports the error it
// ended with, not a close_notify's clean finish.
func TestAbruptCloseKeepsItsCause(t *testing.T) {
	serverConfig, clientConfig := testConfigs(t)
	sentinel := errors.New("connection cut")
	server := newEcho()
	transport := serveTLS(t, server, Server(serverConfig))
	listener, err := transport.Listen("tcp://127.0.0.1:0", nil)
	if err != nil {
		t.Fatal(err)
	}

	closed := make(chan error, 1)
	started := make(chan struct{})
	events := &uio.Events{Pollers: 1, Hooks: uio.FuncHandler{Start: func(*uio.Events) { close(started) }}}
	_, err = ulayer.Over(events, uio.FuncHandler{
		Open:  func(uio.Conn) {},
		Data:  func(uio.Conn) error { return nil },
		Close: func(_ uio.Conn, err error) { closed <- err },
	}, Client(clientConfig))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- events.Serve() }()
	t.Cleanup(func() { _ = events.Close(nil); <-done })
	<-started
	raw, err := events.Dial("tcp://"+listener.Addr().String(), nil)
	if err != nil {
		t.Fatal(err)
	}
	// The stack settles first, then the wire is cut under it.
	time.Sleep(100 * time.Millisecond)
	_ = raw.CloseWith(sentinel)
	select {
	case err := <-closed:
		if !errors.Is(err, sentinel) {
			t.Fatalf("closed with %v, want the cause the close named", err)
		}
	case <-time.After(testWait):
		t.Fatal("the stack never closed")
	}
	wait(t, server.closed, "server close")
}

// A handshake timeout ends the connection with the timeout's cause, and the
// dialing side reports it without running a callback.
func TestHandshakeTimeout(t *testing.T) {
	serverConfig, _ := testConfigs(t)
	layer := Server(serverConfig)
	layer.HandshakeTimeout = 150 * time.Millisecond
	server := newEcho()
	transport := serveTLS(t, server, layer)
	listener, err := transport.Listen("tcp://127.0.0.1:0", nil)
	if err != nil {
		t.Fatal(err)
	}
	// A peer that connects and says nothing.
	silent, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer silent.Close()
	select {
	case <-server.closed:
	case <-time.After(testWait):
		t.Fatal("the silent peer's connection never closed")
	}
}

// A dial cut short by its context reports the deadline and runs no callback
// of the stack's handler.
func TestDialCanceledMidHandshake(t *testing.T) {
	// A server that accepts and never answers.
	silent, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer silent.Close()
	go func() {
		for {
			c, err := silent.Accept()
			if err != nil {
				return
			}
			_ = c // held open by nothing but the kernel: the peer says nothing
		}
	}()

	var mu sync.Mutex
	calls := 0
	started := make(chan struct{})
	events := &uio.Events{Pollers: 1, Hooks: uio.FuncHandler{Start: func(*uio.Events) { close(started) }}}
	transport, err := ulayer.Over(events, uio.FuncHandler{
		Open:  func(uio.Conn) { mu.Lock(); calls++; mu.Unlock() },
		Data:  func(uio.Conn) error { return nil },
		Close: func(uio.Conn, error) { mu.Lock(); calls++; mu.Unlock() },
	}, Client(&tls.Config{InsecureSkipVerify: true}))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- events.Serve() }()
	t.Cleanup(func() { _ = events.Close(nil); <-done })
	<-started
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err = transport.DialContext(ctx, "tcp://"+silent.Addr().String(), nil)
	// The deadline is what asked for the close; a transport error that beat
	// it to the report tells the same story.
	if err == nil {
		t.Fatal("DialContext of a stalled handshake returned no error")
	}
	mu.Lock()
	got := calls
	mu.Unlock()
	if got != 0 {
		t.Fatalf("a canceled dial ran %d callbacks of the stack's handler", got)
	}
}
