package ulayer

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urpc/uio"
)

const testWait = 5 * time.Second

// echo is the handler at the top of a stack: it records the connections it
// sees and answers what it reads.
type echo struct {
	opened chan uio.Conn
	data   chan []byte
	closed chan uio.Conn
}

func newEcho() *echo {
	return &echo{
		opened: make(chan uio.Conn, 8),
		data:   make(chan []byte, 64),
		closed: make(chan uio.Conn, 8),
	}
}

func (e *echo) OnOpen(c uio.Conn) { e.opened <- c }

func (e *echo) OnData(c uio.Conn) error {
	payload := append([]byte(nil), c.PeekChunk()...)
	_, _ = c.Discard(len(payload))
	e.data <- payload
	_, err := c.Write(payload)
	return err
}

func (e *echo) OnClose(c uio.Conn, _ error) { e.closed <- c }

// record is a handler that only reports: a dialing side reads its answers
// from its channels.
type record struct {
	opened chan uio.Conn
	data   chan []byte
	closed chan error
}

func newRecord() *record {
	return &record{
		opened: make(chan uio.Conn, 4),
		data:   make(chan []byte, 64),
		closed: make(chan error, 4),
	}
}

func (r *record) OnOpen(c uio.Conn) { r.opened <- c }

func (r *record) OnData(c uio.Conn) error {
	payload := append([]byte(nil), c.PeekChunk()...)
	_, _ = c.Discard(len(payload))
	r.data <- payload
	return nil
}

func (r *record) OnClose(_ uio.Conn, err error) { r.closed <- err }

func wait[T any](t *testing.T, c chan T, what string) T {
	t.Helper()
	select {
	case v := <-c:
		return v
	case <-time.After(testWait):
		t.Fatalf("no %s within %v", what, testWait)
		return *new(T)
	}
}

// serveStack serves a stack over its own events, with nothing listening yet.
func serveStack(t *testing.T, top uio.Handler, layers ...Layer) Transport {
	t.Helper()
	return serveOver(t, &uio.Events{Pollers: 2}, top, layers...)
}

// dialStack serves a stack of its own and dials addr through it; the handler
// above the stack sees the connection.
func dialStack(t *testing.T, top uio.Handler, addr string, layers ...Layer) uio.Conn {
	t.Helper()
	transport := serveOver(t, &uio.Events{Pollers: 1}, top, layers...)
	ctx, cancel := context.WithTimeout(context.Background(), testWait)
	defer cancel()
	conn, err := transport.DialContext(ctx, addr, nil)
	if err != nil {
		t.Fatal(err)
	}
	return conn
}

// serveOver runs a stack over fresh events and waits for them to serve.
func serveOver(t *testing.T, events *uio.Events, top uio.Handler, layers ...Layer) Transport {
	t.Helper()
	started := make(chan struct{})
	events.Hooks = uio.FuncHandler{Start: func(*uio.Events) { close(started) }}
	transport, err := Over(events, top, layers...)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- events.Serve() }()
	t.Cleanup(func() {
		_ = events.Close(nil)
		select {
		case <-done:
		case <-time.After(testWait):
			t.Error("Serve did not return")
		}
	})
	select {
	case <-started:
	case <-time.After(testWait):
		t.Fatal("Serve did not start")
	}
	return transport
}

// A stack's connection works like any connection: the transport dials it,
// the stack's handler echoes through it, and the wire between carries the
// layers' bytes — a plain client to the same address sees them.
func TestTransportDialAndListenThroughLayers(t *testing.T) {
	const key = 0x5a
	server := newEcho()
	transport := serveStack(t, server, xorLayer{key: key})
	listener, err := transport.Listen("tcp://127.0.0.1:0", nil)
	if err != nil {
		t.Fatal(err)
	}

	client := newRecord()
	conn := dialStack(t, client, "tcp://"+listener.Addr().String(), xorLayer{key: key})
	if _, err := conn.Write([]byte("hello stack")); err != nil {
		t.Fatal(err)
	}
	serverConn := wait(t, server.opened, "server open")
	if serverConn.RemoteAddr() == nil {
		t.Fatal("a wrapped connection has no remote address")
	}
	if got := string(wait(t, server.data, "server data")); got != "hello stack" {
		t.Fatalf("server read %q through its stack", got)
	}
	if got := string(wait(t, client.data, "client answer")); got != "hello stack" {
		t.Fatalf("client read %q back", got)
	}

	// A plain client speaks the wire's bytes, not the stack's: what the
	// stack reads below arrives transformed, and its echo, transformed
	// again on the way out, comes back as it went.
	plain, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer plain.Close()
	if _, err := plain.Write([]byte("plain")); err != nil {
		t.Fatal(err)
	}
	want := make([]byte, len("plain"))
	xorInto(want, []byte("plain"), key)
	if got := wait(t, server.data, "server plain data"); string(got) != string(want) {
		t.Fatalf("server read %q from a plain client, want %q transformed", got, want)
	}
	plain.SetReadDeadline(time.Now().Add(testWait))
	echoed := make([]byte, 32)
	n, err := plain.Read(echoed)
	if err != nil {
		t.Fatal(err)
	}
	if string(echoed[:n]) != "plain" {
		t.Fatalf("plain client read %q, want its own bytes back through two transforms", echoed[:n])
	}

	_ = conn.Close()
	wait(t, server.closed, "server close")
}

// Stacks nest: two handshakes and two transforms, one over the other, and
// every layer's connection still behaves.
func TestNestedStacks(t *testing.T) {
	server := newEcho()
	transport := serveStack(t, server,
		xorLayer{key: 0x01}, &gateLayer{},
		xorLayer{key: 0x02}, &gateLayer{},
	)
	listener, err := transport.Listen("tcp://127.0.0.1:0", nil)
	if err != nil {
		t.Fatal(err)
	}
	client := newRecord()
	conn := dialStack(t, client, "tcp://"+listener.Addr().String(),
		xorLayer{key: 0x01}, &gateLayer{},
		xorLayer{key: 0x02}, &gateLayer{},
	)
	for i, message := range []string{"first", "second", "third"} {
		if _, err := conn.Write([]byte(message)); err != nil {
			t.Fatal(err)
		}
		if got := string(wait(t, server.data, "server data")); got != message {
			t.Fatalf("message %d = %q, want %q", i, got, message)
		}
	}
	_ = conn.Close()
	wait(t, server.closed, "server close")
}

// A layer that hands its connection over before the handshake holds what is
// sent until the handshake settles, and then sends it, in order.
func TestEarlyOpenHoldsWritesUntilSettled(t *testing.T) {
	server := newEcho()
	transport := serveStack(t, server, &gateLayer{EarlyOpen: true, MaxPending: 1 << 16})
	listener, err := transport.Listen("tcp://127.0.0.1:0", nil)
	if err != nil {
		t.Fatal(err)
	}

	early := make(chan struct{})
	clientEvents := &uio.Events{Pollers: 1}
	serveOver(t, clientEvents, uio.FuncHandler{
		Open: func(c uio.Conn) {
			if _, err := c.Write([]byte("held-1")); err != nil {
				t.Error(err)
			}
			if _, err := c.Write([]byte("held-2")); err != nil {
				t.Error(err)
			}
			close(early)
		},
		Data:  func(uio.Conn) error { return nil },
		Close: func(uio.Conn, error) {},
	}, &gateLayer{EarlyOpen: true, MaxPending: 1 << 16})
	if _, err := clientEvents.Dial("tcp://"+listener.Addr().String(), nil); err != nil {
		t.Fatal(err)
	}
	wait(t, early, "the open above a settling handshake")
	// A stream carries no message boundaries: the held writes arrive in
	// order, as few or many rounds as the transport made of them.
	var arrived []byte
	for !strings.Contains(string(arrived), "held-2") {
		select {
		case got := <-server.data:
			arrived = append(arrived, got...)
		case <-time.After(testWait):
			t.Fatalf("held writes arrived as %q, want both in order", arrived)
		}
	}
	if string(arrived) != "held-1held-2" {
		t.Fatalf("held writes arrived as %q, out of order", arrived)
	}
}

// The held writes of a layer that never settles are bounded: past the bound
// the connection ends, and the layer above hears why.
func TestHeldWritesAreBounded(t *testing.T) {
	server := newEcho()
	transport := serveStack(t, server, &gateLayer{EarlyOpen: true, MaxPending: 8, Stall: true})
	listener, err := transport.Listen("tcp://127.0.0.1:0", nil)
	if err != nil {
		t.Fatal(err)
	}

	closed := make(chan error, 1)
	clientEvents := &uio.Events{Pollers: 1}
	serveOver(t, clientEvents, uio.FuncHandler{
		Open: func(c uio.Conn) {
			if _, err := c.Write(make([]byte, 32)); err == nil {
				t.Error("a write past the held bound was taken")
			}
		},
		Data:  func(uio.Conn) error { return nil },
		Close: func(_ uio.Conn, err error) { closed <- err },
	}, &gateLayer{EarlyOpen: true, MaxPending: 8, Stall: true})
	if _, err := clientEvents.Dial("tcp://"+listener.Addr().String(), nil); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-closed:
		if !errors.Is(err, errGatePending) {
			t.Fatalf("closed with %v, want the held-write bound", err)
		}
	case <-time.After(testWait):
		t.Fatal("the held-write bound never closed the connection")
	}
}

// DialContext returns a settled connection: its stack's handler has opened.
// A handshake that fails reports the error and runs no callback of the
// stack's handler; so does one cut short by the context.
func TestDialSettlesOrFails(t *testing.T) {
	t.Run("settled", func(t *testing.T) {
		server := newEcho()
		transport := serveStack(t, server, &gateLayer{})
		listener, err := transport.Listen("tcp://127.0.0.1:0", nil)
		if err != nil {
			t.Fatal(err)
		}
		client := newRecord()
		conn := dialStack(t, client, "tcp://"+listener.Addr().String(), &gateLayer{})
		select {
		case <-client.opened: // the stack settled before DialContext returned
		default:
			t.Fatal("DialContext returned before its stack settled")
		}
		_ = conn.Close()
	})
	t.Run("userdata", func(t *testing.T) {
		server := newEcho()
		transport := serveStack(t, server, &gateLayer{})
		listener, err := transport.Listen("tcp://127.0.0.1:0", nil)
		if err != nil {
			t.Fatal(err)
		}
		transport = serveOver(t, &uio.Events{Pollers: 1}, newRecord(), &gateLayer{})
		ctx, cancel := context.WithTimeout(context.Background(), testWait)
		defer cancel()
		conn, err := transport.DialContext(ctx, "tcp://"+listener.Addr().String(), "dial-userdata")
		if err != nil {
			t.Fatal(err)
		}
		if got := conn.Userdata(); got != "dial-userdata" {
			t.Fatalf("dialed connection userdata = %v, want what the dial passed", got)
		}
		_ = conn.Close()
	})
	t.Run("refused", func(t *testing.T) {
		// The dialer's own stack counts: a dial the server refuses reports
		// its error and nothing else.
		var mu sync.Mutex
		calls := 0
		counting := uio.FuncHandler{
			Open:  func(uio.Conn) { mu.Lock(); calls++; mu.Unlock() },
			Data:  func(uio.Conn) error { return nil },
			Close: func(uio.Conn, error) { mu.Lock(); calls++; mu.Unlock() },
		}
		server := serveStack(t, newEcho(), &gateLayer{Refuse: true})
		listener, err := server.Listen("tcp://127.0.0.1:0", nil)
		if err != nil {
			t.Fatal(err)
		}
		events := &uio.Events{Pollers: 1}
		transport := serveOver(t, events, counting, &gateLayer{})
		ctx, cancel := context.WithTimeout(context.Background(), testWait)
		defer cancel()
		_, err = transport.DialContext(ctx, "tcp://"+listener.Addr().String(), nil)
		if err == nil {
			t.Fatal("DialContext of a refused handshake returned no error")
		}
		mu.Lock()
		got := calls
		mu.Unlock()
		if got != 0 {
			t.Fatalf("a failed dial ran %d callbacks of the stack's handler", got)
		}
	})
	t.Run("canceled", func(t *testing.T) {
		var mu sync.Mutex
		calls := 0
		counting := uio.FuncHandler{
			Open:  func(uio.Conn) { mu.Lock(); calls++; mu.Unlock() },
			Data:  func(uio.Conn) error { return nil },
			Close: func(uio.Conn, error) { mu.Lock(); calls++; mu.Unlock() },
		}
		transport := serveStack(t, counting, &gateLayer{Stall: true})
		listener, err := transport.Listen("tcp://127.0.0.1:0", nil)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
		defer cancel()
		_, err = transport.DialContext(ctx, "tcp://"+listener.Addr().String(), nil)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("DialContext error = %v, want the deadline", err)
		}
		mu.Lock()
		got := calls
		mu.Unlock()
		if got != 0 {
			t.Fatalf("a canceled dial ran %d callbacks of the stack's handler", got)
		}
	})
}

// The close cause a stack's handler gives OnClose is the one its connection
// was closed with, kept through the layers.
func TestCloseCauseThroughLayers(t *testing.T) {
	sentinel := errors.New("stack close cause")
	server := newEcho()
	transport := serveStack(t, server, xorLayer{key: 0x33}, &gateLayer{})
	listener, err := transport.Listen("tcp://127.0.0.1:0", nil)
	if err != nil {
		t.Fatal(err)
	}
	client := newRecord()
	clientEvents := &uio.Events{Pollers: 1}
	serveOver(t, clientEvents, uio.Handler(client), xorLayer{key: 0x33}, &gateLayer{})
	raw, err := clientEvents.Dial("tcp://"+listener.Addr().String(), nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = raw.CloseWith(sentinel)
	select {
	case err := <-client.closed:
		if !errors.Is(err, sentinel) {
			t.Fatalf("closed with %v, want the cause the close named", err)
		}
	case <-time.After(testWait):
		t.Fatal("the stack never closed")
	}
	wait(t, server.closed, "server close")
}

// zeroes reads as zeros, forever.
type zeroes struct{}

func (zeroes) Read(b []byte) (int, error) {
	for i := range b {
		b[i] = 0
	}
	return len(b), nil
}

// A layer's staging bound stills the wire: once it holds its bound and stops
// consuming, the unread bytes below reach the transport's inbound limit, the
// socket reads stop, and the sender stops completing writes — until the
// layer above reads again.
func TestStagingBoundStillsTheWire(t *testing.T) {
	const (
		capacity = 32 << 10
		inbound  = 64 << 10
	)
	release := make(chan struct{})
	hold := &holdLayer{Capacity: capacity}
	var stalledStaged, stalledBelow atomic.Int64
	transport := serveOver(t, &uio.Events{Pollers: 2, MaxInboundBuffered: inbound}, uio.FuncHandler{
		Open: func(uio.Conn) {},
		Data: func(c uio.Conn) error {
			select {
			case <-release:
				_, _ = c.Discard(c.InboundBuffered())
			default: // staged above, unread
				if held, ok := c.(*holdConn); ok {
					staged, below := held.snapshot()
					stalledStaged.Store(int64(staged))
					stalledBelow.Store(int64(below))
				}
			}
			return nil
		},
		Close: func(uio.Conn, error) {},
	}, hold)
	listener, err := transport.Listen("tcp://127.0.0.1:0", nil)
	if err != nil {
		t.Fatal(err)
	}
	sender, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer sender.Close()

	const total = 4 << 20
	written := make(chan error, 1)
	go func() {
		_, err := io.CopyN(sender, zeroes{}, total)
		written <- err
	}()
	// Wait until the stack is stalled against its bound with bytes left
	// below: the sender keeps writing while that settles.
	deadline := time.Now().Add(testWait)
	for stalledBelow.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if stalledBelow.Load() == 0 {
		t.Fatal("the stack never held bytes below its layer")
	}
	select {
	case err := <-written:
		t.Fatalf("the sender finished with the stack stalled: %v", err)
	case <-time.After(time.Second):
	}
	staged := int(stalledStaged.Load())
	below := int(stalledBelow.Load())
	if staged > capacity+(128<<10) {
		t.Fatalf("the layer staged %d bytes, far past its bound", staged)
	}
	if below == 0 {
		t.Fatal("the connection below was drained while the layer held its bound")
	}

	close(release) // the reader above reads again, and its round needs a wake
	hold.mu.Lock()
	top := hold.conns[0]
	hold.mu.Unlock()
	if err := top.Wake(); err != nil {
		t.Fatalf("wake the stalled stack: %v", err)
	}
	select {
	case err := <-written:
		if err != nil {
			t.Fatalf("the sender failed after the release: %v", err)
		}
	case <-time.After(testWait):
		t.Fatal("the sender never finished after the release")
	}
}

// Kind rules: a layer cannot stack over what the layer under it serves, and
// a transport's addresses are checked against its bottom layer.
func TestKindRules(t *testing.T) {
	var kindErr *KindError
	err := Validate(datagramLayer{}, xorLayer{key: 1})
	if !errors.As(err, &kindErr) || kindErr.Layer != 0 {
		t.Fatalf("Validate error = %v, want a KindError on layer 0", err)
	}
	if err := Validate(xorLayer{key: 1}, xorLayer{key: 2}); err != nil {
		t.Fatalf("two stream layers do not stack: %v", err)
	}
	func() {
		defer func() { recover() }()
		Wrap(newEcho(), datagramLayer{}, xorLayer{key: 1})
		t.Fatal("Wrap stacked what Validate refuses")
	}()
	transport := serveStack(t, newEcho(), datagramLayer{})
	if _, err := transport.Listen("tcp://127.0.0.1:0", nil); err == nil {
		t.Fatal("a datagram layer over a tcp address was allowed")
	}
	if _, err := transport.Dial("tcp://127.0.0.1:1", nil); err == nil {
		t.Fatal("a datagram layer dialing tcp was allowed")
	}
	for addr, want := range map[string]Kind{
		"tcp://x:1": StreamKind, "tcp4://x:1": StreamKind,
		"unix:///p": StreamKind, "udp://x:1": DatagramKind,
		"udp6://x:1": DatagramKind, "unixgram:///p": DatagramKind,
		"x:1": StreamKind,
	} {
		if got := kindOfAddr(addr); got != want {
			t.Errorf("kindOfAddr(%q) = %v, want %v", addr, got, want)
		}
	}
}
