package uio

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const layerWait = 5 * time.Second

// mapBytes transforms every byte with f both ways: the shape of a protocol
// that changes the wire's bytes. It writes only what it transforms.
func mapBytes(key byte, f func(b, k byte) byte) Layer {
	return LayerFunc(func(LayerContext) LayerConn {
		return &mapLayer{key: key, f: f}
	})
}

type mapLayer struct {
	PassThrough
	key byte
	f   func(b, k byte) byte
}

func (m *mapLayer) OnInbound(ctx LayerContext) error {
	lower := ctx.Lower()
	chunk := lower.PeekChunk()
	if len(chunk) == 0 {
		return nil
	}
	// Transform straight into the delivery space: no buffer of the layer's own.
	out := ctx.Space(len(chunk))[:len(chunk)]
	for i, b := range chunk {
		out[i] = m.f(b, m.key)
	}
	ctx.Commit(len(chunk))
	_, err := lower.Discard(len(chunk))
	return err
}

func (m *mapLayer) OnOutbound(ctx LayerContext, p []byte) (int, error) {
	// An owned buffer the socket keeps: one copy.
	owned := AcquireBuffer(len(p))
	owned.Write(p)
	bytes := owned.Bytes()
	for i := range bytes {
		bytes[i] = m.f(bytes[i], m.key)
	}
	if _, err := ctx.Lower().WriteOwned(owned); err != nil {
		if errors.Is(err, ErrOutboundOverflow) {
			ReleaseBuffer(owned)
		}
		return 0, err
	}
	return len(p), nil
}

// layerEcho records what it read and answers.
type layerEcho struct {
	opened chan Conn
	read   chan []byte
}

func newLayerEcho() *layerEcho {
	return &layerEcho{opened: make(chan Conn, 4), read: make(chan []byte, 16)}
}

func (e *layerEcho) OnOpen(c Conn) { e.opened <- c }

func (e *layerEcho) OnData(c Conn) error {
	payload := append([]byte(nil), c.PeekChunk()...)
	_, _ = c.Discard(len(payload))
	e.read <- payload
	_, err := c.Write(payload)
	return err
}

func (e *layerEcho) OnClose(Conn, error) {}

func layerWaitFor[T any](t *testing.T, c chan T, what string) T {
	t.Helper()
	select {
	case v := <-c:
		return v
	case <-time.After(layerWait):
		t.Fatalf("no %s within %v", what, layerWait)
		return *new(T)
	}
}

// serveLayered serves echo through the given layers on its own Events.
func serveLayered(t *testing.T, use func(*Events)) (string, *layerEcho) {
	t.Helper()
	echo := newLayerEcho()
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

// What the Events accepts passes every layer in install order: the first
// installed transforms the wire's bytes first on the way up, and the
// Handler's writes leave through them in reverse.
func TestUseLayersAcceptedConnections(t *testing.T) {
	const (
		xorKey byte = 0x24
		addKey byte = 0x03
		sent        = byte(0x5a)
	)
	addr, echo := serveLayered(t, func(ev *Events) {
		ev.Use(mapBytes(xorKey, func(b, k byte) byte { return b ^ k })).
			Use(mapBytes(addKey, func(b, k byte) byte { return b + k }))
	})
	plain, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer plain.Close()
	if _, err := plain.Write([]byte{sent}); err != nil {
		t.Fatal(err)
	}
	got := layerWaitFor(t, echo.read, "layered read")
	want := (sent ^ xorKey) + addKey
	if len(got) != 1 || got[0] != want {
		t.Fatalf("handler read %v, want [%#x]: the first Use must transform first", got, want)
	}
}

// dialLayered serves echo through layers of its own and dials it through
// the same layers, reporting the client's events.
func dialLayered(t *testing.T, serverAddr string, use func(*Events)) *Events {
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

// A layered dial settles before DialContext returns: the connection it
// returns is the top layer's — its writes leave transformed, its
// userdata is what the dial passed.
func TestUseLayersDialedConnections(t *testing.T) {
	const key byte = 0x5a
	addr, echo := serveLayered(t, func(ev *Events) { ev.Use(mapBytes(key, xor)) })
	events := dialLayered(t, addr, func(ev *Events) { ev.Use(mapBytes(key, xor)) })

	ctx, cancel := context.WithTimeout(context.Background(), layerWait)
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
	// The server's own layer already undid the transform: what its echo
	// read is the plaintext the client wrote through its layer.
	if got := string(layerWaitFor(t, echo.read, "server read")); got != "through" {
		t.Fatalf("server read %q, want through", got)
	}
}

func xor(b, k byte) byte { return b ^ k }

// A layer that fails before the dial settles reports its error from
// DialContext, and the Handler hears nothing of the connection.
func TestUseFailedDialReportsWithoutCallbacks(t *testing.T) {
	refused := errors.New("layer refused")
	// A server that answers nothing: the layer below fails at once.
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
	events := dialLayered(t, silent.Addr().String(), func(ev *Events) {
		ev.Use(LayerFunc(func(LayerContext) LayerConn { return refuseLayer{cause: refused} }))
	})
	events.Handler = FuncHandler{
		Open:  func(Conn) { atomic.AddInt32(&calls, 1) },
		Data:  func(Conn) error { return nil },
		Close: func(Conn, error) { atomic.AddInt32(&calls, 1) },
	}
	ctx, cancel := context.WithTimeout(context.Background(), layerWait)
	defer cancel()
	_, err = events.DialContext(ctx, "tcp://"+silent.Addr().String(), nil)
	if !errors.Is(err, refused) {
		t.Fatalf("DialContext error = %v, want the layer's refusal", err)
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
	events := dialLayered(t, silent.Addr().String(), func(ev *Events) {
		ev.Use(LayerFunc(func(LayerContext) LayerConn { return neverOpens{} }))
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

// Use after Serve installs nothing: Serve read the layers already.
func TestUseAfterServeInstallsNothing(t *testing.T) {
	addr, echo := serveLayered(t, nil)
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
	events.Use(mapBytes(0x01, xor))
	if len(events.use) != 0 {
		t.Fatalf("Use after Serve installed %d layers", len(events.use))
	}
	// The wire without layers still answers, plainly.
	plain, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer plain.Close()
	if _, err := plain.Write([]byte("plain")); err != nil {
		t.Fatal(err)
	}
	if got := string(layerWaitFor(t, echo.read, "plain read")); got != "plain" {
		t.Fatalf("read %q without layers", got)
	}
}

// refuseLayer fails its connection as it opens.
type refuseLayer struct {
	PassThrough
	cause error
}

func (r refuseLayer) OnOpen(ctx LayerContext) { _ = ctx.CloseWith(r.cause) }

// neverOpens negotiates forever: it never opens the layers above.
type neverOpens struct{ PassThrough }

func (neverOpens) OnOpen(LayerContext) {}

// Two layers and the Handler each arm a timer of their own: the lower layer
// at 200ms, the upper at 30ms, the Handler at 80ms. Every one fires: no slot
// replaces another, as the single slot all of them once shared did.
func TestEveryLayerHasItsOwnTimer(t *testing.T) {
	var lowerFired, upperFired, handlerFired atomic.Int32
	lower := LayerFunc(func(LayerContext) LayerConn {
		return &timerLayer{after: 200 * time.Millisecond, fired: &lowerFired}
	})
	upper := LayerFunc(func(LayerContext) LayerConn {
		return &timerLayer{after: 30 * time.Millisecond, fired: &upperFired}
	})
	started := make(chan struct{})
	events := &Events{Pollers: 1}
	events.Hooks = FuncHandler{Start: func(*Events) { close(started) }}
	events.Handler = FuncHandler{
		Open: func(c Conn) { _ = c.SetWakeDeadline(time.Now().Add(80 * time.Millisecond)) },
		Data: func(c Conn) error {
			if len(c.PeekChunk()) == 0 {
				handlerFired.Add(1)
			}
			return nil
		},
		Close: func(Conn, error) {},
	}
	events.Use(lower, upper)
	done := make(chan error, 1)
	go func() { done <- events.Serve() }()
	t.Cleanup(func() { _ = events.Close(nil); <-done })
	<-started
	addr, _ := serveLayered(t, func(ev *Events) { ev.Use(passThroughLayer{}, passThroughLayer{}) })
	ctx, cancel := context.WithTimeout(context.Background(), layerWait)
	defer cancel()
	if _, err := events.DialContext(ctx, "tcp://"+addr, nil); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(layerWait)
	for (lowerFired.Load() == 0 || upperFired.Load() == 0 || handlerFired.Load() == 0) && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if lowerFired.Load() != 1 || upperFired.Load() != 1 || handlerFired.Load() == 0 {
		t.Fatalf("lower fired %d, upper %d, Handler %d: want every timer to fire, the layers' once",
			lowerFired.Load(), upperFired.Load(), handlerFired.Load())
	}
}

// timerLayer arms one timer of its own as it opens.
type timerLayer struct {
	PassThrough
	after time.Duration
	fired *atomic.Int32
}

func (l *timerLayer) OnOpen(ctx LayerContext) {
	_ = ctx.SetTimer(time.Now().Add(l.after))
	ctx.Open()
}

func (l *timerLayer) OnTimer(LayerContext) error {
	l.fired.Add(1)
	return nil
}

// passThroughLayer is a layer that changes nothing.
type passThroughLayer struct{}

func (passThroughLayer) Attach(LayerContext) LayerConn { return PassThrough{} }

// A turn that delivers nothing still reaches the Handler: a wake through a
// stack of layers that pass nothing up arrives all the same.
func TestEmptyTurnsReachTheHandler(t *testing.T) {
	quiet := LayerFunc(func(LayerContext) LayerConn { return &holdBack{} })
	turns := make(chan struct{}, 8)
	started := make(chan struct{})
	events := &Events{Pollers: 1}
	events.Hooks = FuncHandler{Start: func(*Events) { close(started) }}
	events.Handler = FuncHandler{
		Open: func(Conn) {},
		Data: func(Conn) error {
			select {
			case turns <- struct{}{}:
			default:
			}
			return nil
		},
		Close: func(Conn, error) {},
	}
	events.Use(quiet, quiet)
	done := make(chan error, 1)
	go func() { done <- events.Serve() }()
	t.Cleanup(func() { _ = events.Close(nil); <-done })
	<-started
	addr, _ := serveLayered(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), layerWait)
	defer cancel()
	conn, err := events.DialContext(ctx, "tcp://"+addr, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.Wake(); err != nil {
		t.Fatal(err)
	}
	layerWaitFor(t, turns, "the woken turn")
}

// holdBack swallows everything below: it never delivers.
type holdBack struct{ PassThrough }

func (*holdBack) OnInbound(ctx LayerContext) error {
	lower := ctx.Lower()
	_, err := lower.Discard(len(lower.PeekChunk()))
	return err
}

// The Handler's Userdata is the dial's, through every layer, and what a
// layer sets there the Handler reads.
func TestUserdataIsTheConnectionsThroughEveryLayer(t *testing.T) {
	seen := make(chan any, 2)
	marker := LayerFunc(func(LayerContext) LayerConn { return &userdataLayer{seen: seen} })
	started := make(chan struct{})
	events := &Events{Pollers: 1}
	events.Hooks = FuncHandler{Start: func(*Events) { close(started) }}
	events.Handler = FuncHandler{
		Open:  func(c Conn) { seen <- c.Userdata() },
		Data:  func(Conn) error { return nil },
		Close: func(Conn, error) {},
	}
	events.Use(marker, passThroughLayer{})
	done := make(chan error, 1)
	go func() { done <- events.Serve() }()
	t.Cleanup(func() { _ = events.Close(nil); <-done })
	<-started
	addr, _ := serveLayered(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), layerWait)
	defer cancel()
	conn, err := events.DialContext(ctx, "tcp://"+addr, "dialed")
	if err != nil {
		t.Fatal(err)
	}
	if got := layerWaitFor(t, seen, "the layer's view"); got != "dialed" {
		t.Fatalf("the bottom layer saw userdata %v, want the dial's", got)
	}
	if got := layerWaitFor(t, seen, "the Handler's view"); got != "dialed" {
		t.Fatalf("the Handler saw userdata %v, want the dial's", got)
	}
	conn.SetUserdata("changed")
	if got := conn.Userdata(); got != "changed" {
		t.Fatalf("Userdata after SetUserdata = %v", got)
	}
}

// userdataLayer reports the Userdata its Lower carries as it opens.
type userdataLayer struct {
	PassThrough
	seen chan any
}

func (u *userdataLayer) OnOpen(ctx LayerContext) {
	u.seen <- ctx.Lower().Userdata()
	ctx.Open()
}

// Every layer hears OnClose once, top down, and the Handler last.
func TestCloseReachesEveryLayerOnce(t *testing.T) {
	var order []string
	var mu sync.Mutex
	record := func(name string) {
		mu.Lock()
		order = append(order, name)
		mu.Unlock()
	}
	layer := func(name string) Layer {
		return LayerFunc(func(LayerContext) LayerConn { return &closeLayer{name: name, record: record} })
	}
	closed := make(chan struct{})
	started := make(chan struct{})
	events := &Events{Pollers: 1}
	events.Hooks = FuncHandler{Start: func(*Events) { close(started) }}
	events.Handler = FuncHandler{
		Open:  func(Conn) {},
		Data:  func(Conn) error { return nil },
		Close: func(Conn, error) { record("handler"); close(closed) },
	}
	events.Use(layer("bottom"), layer("top"))
	done := make(chan error, 1)
	go func() { done <- events.Serve() }()
	t.Cleanup(func() { _ = events.Close(nil); <-done })
	<-started
	addr, _ := serveLayered(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), layerWait)
	defer cancel()
	conn, err := events.DialContext(ctx, "tcp://"+addr, nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	layerWaitFor(t, closed, "the Handler's OnClose")
	mu.Lock()
	defer mu.Unlock()
	if got := strings.Join(order, ","); got != "top,bottom,handler" {
		t.Fatalf("close order = %s, want top,bottom,handler", got)
	}
}

type closeLayer struct {
	PassThrough
	name   string
	record func(string)
}

func (c *closeLayer) OnClose(LayerContext, error) { c.record(c.name) }

// Writes from many goroutines at once all leave through the layer, whole.
func TestConcurrentWritesThroughALayer(t *testing.T) {
	const key byte = 0x33
	const writers, each = 8, 50
	addr, echo := serveLayered(t, func(ev *Events) { ev.Use(mapBytes(key, xor)) })
	events := dialLayered(t, addr, func(ev *Events) { ev.Use(mapBytes(key, xor)) })
	ctx, cancel := context.WithTimeout(context.Background(), layerWait)
	defer cancel()
	conn, err := events.DialContext(ctx, "tcp://"+addr, nil)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range each {
				_, _ = conn.Write([]byte("abcd"))
			}
		}()
	}
	wg.Wait()
	got := 0
	deadline := time.After(layerWait)
	for got < writers*each*4 {
		select {
		case chunk := <-echo.read:
			for _, b := range chunk {
				if b < 'a' || b > 'd' {
					t.Fatalf("the server read %q: a write was torn or mistransformed", chunk)
				}
			}
			got += len(chunk)
		case <-deadline:
			t.Fatalf("the server read %d of %d bytes", got, writers*each*4)
		}
	}
}

// A layer that takes one chunk per call is called again while bytes still
// wait below: a turn that brings no new bytes — a wake after the reader
// above made room — still reaches what earlier reads left behind.
func TestOneChunkLayerDrainsEverythingBelow(t *testing.T) {
	const total = 256 << 10
	got := make(chan int, 1)
	var read atomic.Int64
	started := make(chan struct{})
	events := &Events{Pollers: 1}
	events.Hooks = FuncHandler{Start: func(*Events) { close(started) }}
	events.Handler = FuncHandler{
		Open: func(Conn) {},
		Data: func(c Conn) error {
			n, _ := c.Discard(len(c.PeekChunk()))
			if read.Add(int64(n)) >= total {
				select {
				case got <- int(read.Load()):
				default:
				}
			}
			return nil
		},
		Close: func(Conn, error) {},
	}
	events.Use(LayerFunc(func(LayerContext) LayerConn { return &tinyChunks{} }))
	done := make(chan error, 1)
	go func() { done <- events.Serve() }()
	t.Cleanup(func() { _ = events.Close(nil); <-done })
	<-started
	listener, err := events.Listen("tcp://127.0.0.1:0", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	peer, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	if _, err := peer.Write(make([]byte, total)); err != nil {
		t.Fatal(err)
	}
	select {
	case n := <-got:
		if n < total {
			t.Fatalf("read %d of %d bytes", n, total)
		}
	case <-time.After(layerWait):
		t.Fatalf("read %d of %d bytes: the layer was not called again for what waited below", read.Load(), total)
	}
}

// tinyChunks delivers at most 1 KiB per call, the way a record-at-a-time
// protocol takes what arrived.
type tinyChunks struct{ PassThrough }

func (*tinyChunks) OnInbound(ctx LayerContext) error {
	lower := ctx.Lower()
	chunk := lower.PeekChunk()
	if len(chunk) > 1<<10 {
		chunk = chunk[:1<<10]
	}
	if len(chunk) == 0 {
		return nil
	}
	ctx.Deliver(chunk)
	_, err := lower.Discard(len(chunk))
	return err
}

// A write a layer refuses whole for a full queue leaves an owned buffer the
// caller's, as the socket's WriteOwned does: the caller sends it again or
// releases it, and releasing it here as well put it in the pool twice.
func TestLayerWriteOwnedOverflowKeepsTheBuffer(t *testing.T) {
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
			defer c.Close()
		}
	}()
	events := dialLayered(t, silent.Addr().String(), func(ev *Events) {
		ev.MaxOutboundBuffered = 64 << 10
		ev.Use(passThroughLayer{})
	})
	ctx, cancel := context.WithTimeout(context.Background(), layerWait)
	defer cancel()
	conn, err := events.DialContext(ctx, "tcp://"+silent.Addr().String(), nil)
	if err != nil {
		t.Fatal(err)
	}
	const size = 16 << 10
	payload := make([]byte, size)
	for range 1 << 14 {
		buffer := AcquireBuffer(size)
		buffer.Write(payload)
		_, err := conn.WriteOwned(buffer)
		if err == nil {
			continue
		}
		if !errors.Is(err, ErrOutboundOverflow) {
			t.Fatalf("WriteOwned = %v, want ErrOutboundOverflow once the queue is full", err)
		}
		if buffer.Len() != size {
			t.Fatalf("the refused buffer holds %d bytes, want %d: it went back to the pool", buffer.Len(), size)
		}
		ReleaseBuffer(buffer)
		return
	}
	t.Fatal("the queue never filled against a peer that reads nothing")
}
