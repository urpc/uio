package uws

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/urpc/uio"
)

// openSendHandler answers the handshake and then sends texts of its choosing.
type openSendHandler struct {
	send func(*Conn)
}

func (h openSendHandler) OnOpen(conn *Conn) {
	if h.send != nil {
		h.send(conn)
	}
}
func (openSendHandler) OnMessage(*Conn, Message)  {}
func (openSendHandler) OnClose(*Conn, CloseEvent) {}

func startClientTestServer(t *testing.T, send func(*Conn)) string {
	t.Helper()
	server, addr, serveDone := startTestServer(t, func() *Server {
		return NewServer(Options{Handler: openSendHandler{send: send}})
	})
	t.Cleanup(func() { stopTestServer(t, server, serveDone) })
	return addr
}

// A client sends and receives through the package-level Dial.
func TestClientEchoRoundTrip(t *testing.T) {
	server, addr, serveDone := startTestServer(t, func() *Server {
		return NewServer(Options{Handler: lifecycleEchoHandler{}})
	})
	t.Cleanup(func() { stopTestServer(t, server, serveDone) })
	ctx, cancel := context.WithTimeout(context.Background(), testIOTimeout())
	defer cancel()
	client, err := Dial(ctx, "ws://"+addr+"/")
	if err != nil {
		t.Fatal(err)
	}
	if err := client.SendBinary([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	message, err := client.ReadMessage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if message.Type != BinaryMessage || string(message.Payload) != "hello" {
		t.Fatalf("message = %d/%q", message.Type, message.Payload)
	}
	if err := client.Close(1000, "done"); err != nil {
		t.Fatal(err)
	}
}

// Messages the server sends right after the handshake wait in the queue;
// Dial returning and the first ReadMessage need not race them.
func TestClientReceivesMessagesSentRightAfterOpen(t *testing.T) {
	const messages = 3
	addr := startClientTestServer(t, func(conn *Conn) {
		for i := range messages {
			_ = conn.SendText([]byte(fmt.Sprintf("m%d", i)))
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), testIOTimeout())
	defer cancel()
	client, err := Dial(ctx, "ws://"+addr+"/")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close(1000, "") }()
	for i := range messages {
		message, err := client.ReadMessage(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if want := fmt.Sprintf("m%d", i); string(message.Payload) != want {
			t.Fatalf("message %d = %q, want %q", i, message.Payload, want)
		}
	}
}

// A refused handshake reports a CloseError, and no Client comes back.
func TestClientHandshakeFailureReturnsCloseError(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	rejected := make(chan struct{})
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		buf := make([]byte, 4096)
		for {
			n, err := conn.Read(buf)
			if err != nil {
				return
			}
			if string(buf[n-4:n]) == "\r\n\r\n" {
				break
			}
		}
		_, _ = conn.Write([]byte("HTTP/1.1 403 Forbidden\r\nContent-Length: 0\r\n\r\n"))
		close(rejected)
		_, _ = conn.Read(buf) // hold the connection until the test ends
	}()
	ctx, cancel := context.WithTimeout(context.Background(), testIOTimeout())
	defer cancel()
	_, err = Dial(ctx, "ws://"+listener.Addr().String()+"/")
	if err == nil {
		t.Fatal("a rejected handshake returned a client")
	}
	var closeErr *CloseError
	if !errors.As(err, &closeErr) {
		t.Fatalf("Dial error = %v, want a CloseError", err)
	}
}

// A server that answers the upgrade and then goes silent leaves Dial to the
// context's deadline.
func TestClientDialDeadlineOnSilentServer(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				buf := make([]byte, 4096)
				for {
					_, err := conn.Read(buf)
					if err != nil {
						return
					}
				}
			}()
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := Dial(ctx, "ws://"+listener.Addr().String()+"/"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Dial error = %v, want %v", err, context.DeadlineExceeded)
	}
}

// A ReadMessage context only bounds its own wait: the connection stays.
func TestClientReadMessageContextTimesOutIndependently(t *testing.T) {
	trigger := make(chan struct{})
	addr := startClientTestServer(t, func(conn *Conn) {
		go func() {
			<-trigger
			_ = conn.SendText([]byte("late"))
		}()
	})
	ctx, cancel := context.WithTimeout(context.Background(), testIOTimeout())
	defer cancel()
	client, err := Dial(ctx, "ws://"+addr+"/")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close(1000, "") }()
	quick, quickCancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	if _, err := client.ReadMessage(quick); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("ReadMessage error = %v, want %v", err, context.DeadlineExceeded)
	}
	quickCancel()
	close(trigger)
	message, err := client.ReadMessage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if string(message.Payload) != "late" {
		t.Fatalf("message = %q", message.Payload)
	}
}

// The peer's close reaches ReadMessage as a CloseError once the queue drains.
func TestClientReportsPeerCloseAfterDrain(t *testing.T) {
	addr := startClientTestServer(t, func(conn *Conn) {
		_ = conn.SendText([]byte("one"))
		_ = conn.SendText([]byte("two"))
		_ = conn.Close(1000, "bye")
	})
	ctx, cancel := context.WithTimeout(context.Background(), testIOTimeout())
	defer cancel()
	client, err := Dial(ctx, "ws://"+addr+"/")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"one", "two"} {
		message, err := client.ReadMessage(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if string(message.Payload) != want {
			t.Fatalf("message = %q, want %q", message.Payload, want)
		}
	}
	_, err = client.ReadMessage(ctx)
	var closeErr *CloseError
	if !errors.As(err, &closeErr) {
		t.Fatalf("ReadMessage error = %v, want a CloseError", err)
	}
	if closeErr.Code != 1000 || closeErr.Reason != "bye" {
		t.Fatalf("close event = %d/%q, want 1000/bye", closeErr.Code, closeErr.Reason)
	}
	if info := client.CloseEventInfo(); info.Code != 1000 {
		t.Fatalf("CloseEventInfo = %+v", info)
	}
}

// Close works on a client that stopped reading with a full queue: the stuck
// message intake gives up, the connection ends, and a later ReadMessage walks
// the queue into the close.
func TestClientCloseEndsFullQueue(t *testing.T) {
	const messages = 200 // well past the queue bound
	addr := startClientTestServer(t, func(conn *Conn) {
		go func() {
			for i := range messages {
				if err := conn.SendText([]byte(fmt.Sprintf("m%d", i))); err != nil {
					return
				}
			}
		}()
	})
	ctx, cancel := context.WithTimeout(context.Background(), testIOTimeout())
	defer cancel()
	client, err := Dial(ctx, "ws://"+addr+"/")
	if err != nil {
		t.Fatal(err)
	}
	// No ReadMessage: the queue fills and OnMessage blocks.
	time.Sleep(50 * time.Millisecond)
	done := make(chan error, 1)
	go func() { done <- client.Close(1000, "") }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(testIOTimeout()):
		t.Fatal("Close never returned from under a full queue")
	}
	deadline := time.Now().Add(testIOTimeout())
	for {
		_, err := client.ReadMessage(context.Background())
		if err == nil {
			continue
		}
		var closeErr *CloseError
		if !errors.As(err, &closeErr) {
			t.Fatalf("ReadMessage error = %v, want a CloseError", err)
		}
		break
	}
	if time.Now().After(deadline) {
		t.Fatal("the queue never drained to the close")
	}
}

// A backlog larger than the queue still delivers every message once the
// application reads.
func TestClientQueueBackpressureDeliversEverything(t *testing.T) {
	const messages = 200
	addr := startClientTestServer(t, func(conn *Conn) {
		go func() {
			for i := range messages {
				if err := conn.SendText([]byte(fmt.Sprintf("m%d", i))); err != nil {
					return
				}
			}
		}()
	})
	ctx, cancel := context.WithTimeout(context.Background(), testIOTimeout())
	defer cancel()
	client, err := Dial(ctx, "ws://"+addr+"/")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close(1000, "") }()
	for i := range messages {
		message, err := client.ReadMessage(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if want := fmt.Sprintf("m%d", i); string(message.Payload) != want {
			t.Fatalf("message %d = %q, want %q", i, message.Payload, want)
		}
	}
}

// The pull mode owns the callbacks: a configured Handler is refused.
func TestDialClientRejectsConfiguredHandler(t *testing.T) {
	dialer := NewDialer(Options{Handler: lifecycleEchoHandler{}})
	if _, err := dialer.DialClient(context.Background(), "ws://127.0.0.1:1/"); !errors.Is(err, ErrInvalidOptions) {
		t.Fatalf("DialClient error = %v, want %v", err, ErrInvalidOptions)
	}
	if dialer.started != nil {
		t.Fatal("a refused DialClient started the dialer event loop")
	}
}

// A Close whose code or reason fails validation changes nothing: the
// connection stays open, still reads, and a valid Close still works after.
func TestClientCloseInvalidReasonKeepsReading(t *testing.T) {
	server, addr, serveDone := startTestServer(t, func() *Server {
		return NewServer(Options{Handler: lifecycleEchoHandler{}})
	})
	t.Cleanup(func() { stopTestServer(t, server, serveDone) })
	ctx, cancel := context.WithTimeout(context.Background(), testIOTimeout())
	defer cancel()
	client, err := Dial(ctx, "ws://"+addr+"/")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close(1000, "") }()
	if err := client.Close(1000, "\xff"); err == nil {
		t.Fatal("an invalid close reason was accepted")
	}
	if client.IsClosed() {
		t.Fatal("an invalid Close closed the connection")
	}
	if err := client.SendText([]byte("still here")); err != nil {
		t.Fatal(err)
	}
	message, err := client.ReadMessage(ctx)
	if err != nil {
		t.Fatalf("reading stopped after a rejected Close: %v", err)
	}
	if string(message.Payload) != "still here" {
		t.Fatalf("message = %q", message.Payload)
	}
}

// After Close, ReadMessage drains the queue and then reports the close event
// OnClose recorded, not an empty one.
func TestClientCloseErrorCarriesCloseEvent(t *testing.T) {
	server, addr, serveDone := startTestServer(t, func() *Server {
		return NewServer(Options{Handler: lifecycleEchoHandler{}})
	})
	t.Cleanup(func() { stopTestServer(t, server, serveDone) })
	ctx, cancel := context.WithTimeout(context.Background(), testIOTimeout())
	defer cancel()
	client, err := Dial(ctx, "ws://"+addr+"/")
	if err != nil {
		t.Fatal(err)
	}
	if err := client.SendText([]byte("last")); err != nil {
		t.Fatal(err)
	}
	message, err := client.ReadMessage(ctx)
	if err != nil || string(message.Payload) != "last" {
		t.Fatalf("echo = %q, %v", message.Payload, err)
	}
	if err := client.Close(1000, "done"); err != nil {
		t.Fatal(err)
	}
	_, err = client.ReadMessage(ctx)
	var closeErr *CloseError
	if !errors.As(err, &closeErr) {
		t.Fatalf("ReadMessage error = %v, want a CloseError", err)
	}
	if closeErr.Code != 1000 {
		t.Fatalf("close event = %d/%q, want the peer's 1000 close answer", closeErr.Code, closeErr.Reason)
	}
}

type refusingExecutor struct{}

func (refusingExecutor) Submit(uio.IOTask) bool       { return false }
func (refusingExecutor) SubmitBatch([]uio.IOTask) int { return 0 }

// An Executor that refuses every turn fails the dial outright instead of
// leaving a connection no callback will ever report.
func TestDialClientFirstTurnRefusalFailsDial(t *testing.T) {
	if !executorRunsTurns {
		t.Skip("the std backend ignores the Executor")
	}
	addr := startClientTestServer(t, nil)
	dialer := NewDialer(Options{Executor: refusingExecutor{}})
	ctx, cancel := context.WithTimeout(context.Background(), testIOTimeout())
	defer cancel()
	if _, err := dialer.DialClient(ctx, "ws://"+addr+"/"); !errors.Is(err, uio.ErrFirstTurnRefused) {
		t.Fatalf("DialClient error = %v, want %v", err, uio.ErrFirstTurnRefused)
	}
	// The callback form fails the same way instead of returning a dead
	// connection.
	if err := dialer.Dial(ctx, "ws://"+addr+"/", nil); !errors.Is(err, uio.ErrFirstTurnRefused) {
		t.Fatalf("Dial error = %v, want %v", err, uio.ErrFirstTurnRefused)
	}
}
