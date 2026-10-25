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

const wrapWait = 5 * time.Second

// mapWrap transforms every byte with f both ways: the shape of a protocol
// that changes the wire's bytes.
func mapWrap(key byte, f func(b, k byte) byte) Middleware {
	return PerConn(func(lower Conn, next Handler) Handler {
		conn := &mapWrapConn{InnerConn: InnerConn{Conn: lower}, like: mapFn{key: key, f: f}}
		return FuncHandler{
			Open:  func(Conn) { next.OnOpen(conn) },
			Data:  func(Conn) error { conn.pull(); return next.OnData(conn) },
			Close: func(_ Conn, err error) { next.OnClose(conn, err) },
		}
	})
}

type mapFn struct {
	key byte
	f   func(b, k byte) byte
}

type mapWrapConn struct {
	InnerConn
	like    mapFn
	pending []byte
}

var _ Conn = (*mapWrapConn)(nil)

func (c *mapWrapConn) pull() {
	chunk := c.Conn.PeekChunk()
	for i := range chunk {
		chunk[i] = c.like.f(chunk[i], c.like.key)
	}
	_, _ = c.Conn.Discard(len(chunk))
	c.pending = append(c.pending, chunk...)
}

func (c *mapWrapConn) Peek(b []byte) []byte { n := copy(b, c.pending); return b[:n] }
func (c *mapWrapConn) PeekChunk() []byte    { return c.pending }
func (c *mapWrapConn) Discard(n int) (int, error) {
	if n > len(c.pending) {
		n = len(c.pending)
	}
	c.pending = c.pending[n:]
	return n, nil
}
func (c *mapWrapConn) Read(b []byte) (int, error) {
	n := copy(b, c.pending)
	c.pending = c.pending[n:]
	return n, nil
}
func (c *mapWrapConn) InboundBuffered() int     { return len(c.pending) + c.Conn.InboundBuffered() }
func (c *mapWrapConn) ReserveInbound(int) error { return nil }
func (c *mapWrapConn) OutboundBuffered() int    { return c.Conn.OutboundBuffered() }
func (c *mapWrapConn) Write(p []byte) (int, error) {
	owned := AcquireBuffer(len(p))
	owned.Write(p)
	bytes := owned.Bytes()
	for i := range bytes {
		bytes[i] = c.like.f(bytes[i], c.like.key)
	}
	if _, err := c.Conn.WriteOwned(owned); err != nil {
		return 0, err
	}
	return len(p), nil
}
func (c *mapWrapConn) Writev(v [][]byte) (int, error) { return writevViaWrite(c, v) }

func (c *mapWrapConn) WriteByte(b byte) error {
	_, err := c.Write([]byte{b})
	return err
}

func (c *mapWrapConn) WriteString(s string) (int, error) { return c.Write([]byte(s)) }

func (c *mapWrapConn) WriteTo(w io.Writer) (int64, error) {
	n, err := w.Write(c.pending)
	c.pending = c.pending[n:]
	return int64(n), err
}
func (c *mapWrapConn) WriteOwned(b *Buffer) (int, error) {
	n, err := c.Write(b.Bytes())
	ReleaseBuffer(b)
	if err != nil {
		return 0, err
	}
	return n, nil
}
func (c *mapWrapConn) ReserveOutbound(int) ([]byte, error) { return nil, ErrReserveUnsupported }
func (c *mapWrapConn) Flush() error                        { return c.Conn.Flush() }
func (c *mapWrapConn) YieldRead() error                    { return c.Conn.YieldRead() }
func (c *mapWrapConn) CloseWith(err error) error           { return c.Conn.CloseWith(err) }
func (c *mapWrapConn) Close() error                        { return c.Conn.Close() }
func (c *mapWrapConn) CloseWrite() error                   { return c.Conn.CloseWrite() }

func writevViaWrite(c Conn, vec [][]byte) (int, error) {
	total := 0
	for _, segment := range vec {
		n, err := c.Write(segment)
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

// wrapEcho records what it read and answers.
type wrapEcho struct {
	opened chan Conn
	read   chan []byte
}

func newWrapEcho() *wrapEcho {
	return &wrapEcho{opened: make(chan Conn, 4), read: make(chan []byte, 16)}
}

func (e *wrapEcho) OnOpen(c Conn) { e.opened <- c }

func (e *wrapEcho) OnData(c Conn) error {
	payload := append([]byte(nil), c.PeekChunk()...)
	_, _ = c.Discard(len(payload))
	e.read <- payload
	_, err := c.Write(payload)
	return err
}

func (e *wrapEcho) OnClose(Conn, error) {}

func wrapWaitFor[T any](t *testing.T, c chan T, what string) T {
	t.Helper()
	select {
	case v := <-c:
		return v
	case <-time.After(wrapWait):
		t.Fatalf("no %s within %v", what, wrapWait)
		return *new(T)
	}
}

// serveWrapped serves echo through the given wrappers on its own Events.
func serveWrapped(t *testing.T, use func(*Events)) (string, *wrapEcho) {
	t.Helper()
	echo := newWrapEcho()
	started := make(chan struct{})
	events := &Events{Pollers: 2, Handler: echo}
	events.Hooks = FuncHandler{Start: func(*Events) { close(started) }}
	if use != nil {
		use(events)
	}
	done := make(chan error, 1)
	go func() { done <- events.Serve() }()
	t.Cleanup(func() {
		_ = events.Close(nil)
		<-done
	})
	<-started
	listener, err := events.Listen("tcp://127.0.0.1:0", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	return listener.Addr().String(), echo
}

// What the Events accepts passes every wrapper in install order: the first
// installed transforms the wire's bytes first on the way up, and the
// Handler's writes leave through them in reverse.
func TestUseWrapsAcceptedConnections(t *testing.T) {
	const (
		xorKey byte = 0x24
		addKey byte = 0x03
		sent        = byte(0x5a)
	)
	addr, echo := serveWrapped(t, func(ev *Events) {
		ev.Use(mapWrap(xorKey, func(b, k byte) byte { return b ^ k })).
			Use(mapWrap(addKey, func(b, k byte) byte { return b + k }))
	})
	plain, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer plain.Close()
	if _, err := plain.Write([]byte{sent}); err != nil {
		t.Fatal(err)
	}
	got := wrapWaitFor(t, echo.read, "wrapped read")
	want := (sent ^ xorKey) + addKey
	if len(got) != 1 || got[0] != want {
		t.Fatalf("handler read %v, want [%#x]: the first Use must transform first", got, want)
	}
}

// dialWrapped serves echo through wrappers of its own and dials it through
// the same wrappers, reporting the client's events.
func dialWrapped(t *testing.T, serverAddr string, use func(*Events)) *Events {
	t.Helper()
	started := make(chan struct{})
	events := &Events{Pollers: 1}
	events.Hooks = FuncHandler{Start: func(*Events) { close(started) }}
	events.Handler = FuncHandler{
		Open: func(Conn) {},
		Data: func(c Conn) error {
			_, _ = c.Discard(c.InboundBuffered())
			return nil
		},
		Close: func(Conn, error) {},
	}
	if use != nil {
		use(events)
	}
	done := make(chan error, 1)
	go func() { done <- events.Serve() }()
	t.Cleanup(func() {
		_ = events.Close(nil)
		<-done
	})
	<-started
	_ = serverAddr
	return events
}

// A wrapped dial settles before DialContext returns: the connection it
// returns is the topmost wrapper's — its writes leave transformed, its
// userdata is what the dial passed.
func TestUseWrapsDialedConnections(t *testing.T) {
	const key byte = 0x5a
	addr, echo := serveWrapped(t, func(ev *Events) { ev.Use(mapWrap(key, xor)) })
	events := dialWrapped(t, addr, func(ev *Events) { ev.Use(mapWrap(key, xor)) })

	ctx, cancel := context.WithTimeout(context.Background(), wrapWait)
	defer cancel()
	conn, err := events.DialContext(ctx, "tcp://"+addr, "dial-userdata")
	if err != nil {
		t.Fatal(err)
	}
	if got := conn.Userdata(); got != "dial-userdata" {
		t.Fatalf("dialed connection userdata = %v, want what the dial passed", got)
	}
	if _, err := conn.Write([]byte("through")); err != nil {
		t.Fatal(err)
	}
	// The server's own wrapper already undid the transform: what its echo
	// read is the plaintext the client wrote through its wrapper.
	if got := string(wrapWaitFor(t, echo.read, "server read")); got != "through" {
		t.Fatalf("server read %q, want through", got)
	}
}

func xor(b, k byte) byte { return b ^ k }

// A wrapper that fails before the dial settles reports its error from
// DialContext, and the Handler hears nothing of the connection.
func TestUseFailedDialReportsWithoutCallbacks(t *testing.T) {
	refused := errors.New("wrapper refused")
	// A server that answers nothing: the wrapper below fails at once.
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
			_ = c
		}
	}()

	var calls int32
	events := dialWrapped(t, silent.Addr().String(), func(ev *Events) {
		ev.Use(func(next Handler) Handler {
			return FuncHandler{
				Open: func(c Conn) { _ = c.CloseWith(refused) },
				Data: func(Conn) error { return nil },
				// OnClose always reaches above, by the wrapper contract.
				Close: func(c Conn, _ error) { next.OnClose(c, refused) },
			}
		})
	})
	events.Handler = FuncHandler{
		Open:  func(Conn) { atomic.AddInt32(&calls, 1) },
		Data:  func(Conn) error { return nil },
		Close: func(Conn, error) { atomic.AddInt32(&calls, 1) },
	}
	ctx, cancel := context.WithTimeout(context.Background(), wrapWait)
	defer cancel()
	_, err = events.DialContext(ctx, "tcp://"+silent.Addr().String(), nil)
	if !errors.Is(err, refused) {
		t.Fatalf("DialContext error = %v, want the wrapper's refusal", err)
	}
	if n := atomic.LoadInt32(&calls); n != 0 {
		t.Fatalf("a failed dial ran %d Handler callbacks", n)
	}
}

// A dial canceled mid-settle reports the cancellation and runs no callback.
func TestUseCanceledDialRunsNoCallbacks(t *testing.T) {
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
			_ = c
		}
	}()

	var calls int32
	events := dialWrapped(t, silent.Addr().String(), func(ev *Events) {
		ev.Use(func(next Handler) Handler { // never settles, never fails
			return FuncHandler{
				Open:  func(Conn) {},
				Data:  func(Conn) error { return nil },
				Close: func(c Conn, err error) { next.OnClose(c, err) },
			}
		})
	})
	events.Handler = FuncHandler{
		Open:  func(Conn) { atomic.AddInt32(&calls, 1) },
		Data:  func(Conn) error { return nil },
		Close: func(Conn, error) { atomic.AddInt32(&calls, 1) },
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err = events.DialContext(ctx, "tcp://"+silent.Addr().String(), nil)
	if err == nil {
		t.Fatal("a canceled dial reported no error")
	}
	if n := atomic.LoadInt32(&calls); n != 0 {
		t.Fatalf("a canceled dial ran %d Handler callbacks", n)
	}
}

// Use after Serve installs nothing: Serve read the wrappers already.
func TestUseAfterServeInstallsNothing(t *testing.T) {
	addr, echo := serveWrapped(t, nil)
	events := &Events{Pollers: 1, Handler: echo}
	started := make(chan struct{})
	events.Hooks = FuncHandler{Start: func(*Events) { close(started) }}
	done := make(chan error, 1)
	go func() { done <- events.Serve() }()
	t.Cleanup(func() {
		_ = events.Close(nil)
		<-done
	})
	<-started
	events.Use(mapWrap(0x01, xor))
	if len(events.use) != 0 {
		t.Fatalf("Use after Serve installed %d wrappers", len(events.use))
	}
	// The unwrapped wire still answers, plainly.
	plain, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer plain.Close()
	if _, err := plain.Write([]byte("plain")); err != nil {
		t.Fatal(err)
	}
	if got := string(wrapWaitFor(t, echo.read, "plain read")); got != "plain" {
		t.Fatalf("read %q unwrapped", got)
	}
}
