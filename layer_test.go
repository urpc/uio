package uio

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"runtime"
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
	return LayerFunc(func(LayerContext) LayerHandler {
		return &mapLayer{key: key, f: f}
	})
}

type mapLayer struct {
	PassThrough
	key byte
	f   func(b, k byte) byte
}

func (m *mapLayer) OnRead(ctx LayerContext) error {
	lower := ctx.Down()
	chunk := lower.PeekChunk()
	if len(chunk) == 0 {
		return nil
	}
	// Transform straight into the delivery space: no buffer of the layer's own.
	out := ctx.Up().Reserve(len(chunk))[:len(chunk)]
	for i, b := range chunk {
		out[i] = m.f(b, m.key)
	}
	ctx.Up().Commit(len(chunk))
	_, err := lower.Discard(len(chunk))
	return err
}

func (m *mapLayer) OnWrite(ctx LayerContext, p []byte) (int, error) {
	// An owned buffer the socket keeps: one copy.
	owned := AcquireBuffer(len(p))
	owned.Write(p)
	bytes := owned.Bytes()
	for i := range bytes {
		bytes[i] = m.f(bytes[i], m.key)
	}
	if _, err := ctx.Down().WriteOwned(owned); err != nil {
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

func (e *layerEcho) OnRead(c Conn) error {
	payload := append([]byte(nil), c.PeekChunk()...)
	_, _ = c.Discard(len(payload))
	e.read <- payload
	_, err := c.Write(payload)
	return err
}

func (e *layerEcho) OnTimer(Conn) error  { return nil }
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
func serveLayered(t *testing.T, layers []Layer, setup func(*Events)) (string, *layerEcho) {
	t.Helper()
	echo := newLayerEcho()
	started := make(chan struct{})
	events := &Events{Pollers: 2, Handler: echo}
	events.Hooks = FuncHandler{Start: func(*Events) { close(started) }}
	if setup != nil {
		setup(events)
	}
	done := make(chan error, 1)
	go func() { done <- events.Serve() }()
	t.Cleanup(func() {
		_ = events.Close(nil)
		<-done
	})
	<-started
	var listener Listener
	var err error
	if len(layers) > 0 {
		listener, err = events.Listen("tcp://127.0.0.1:0", WithLayers(layers...))
	} else {
		listener, err = events.Listen("tcp://127.0.0.1:0")
	}
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
	addr, echo := serveLayered(t, []Layer{mapBytes(xorKey, func(b, k byte) byte { return b ^ k }), mapBytes(addKey, func(b, k byte) byte { return b + k })}, func(ev *Events) {

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
// layeredEvents is dialLayered's Events: its dials carry the helper's chain.
type layeredEvents struct {
	*Events
	layers []Layer
}

func (e *layeredEvents) dialOptions(opts []DialOption) []DialOption {
	if len(e.layers) == 0 {
		return opts
	}
	return append([]DialOption{WithLayers(e.layers...)}, opts...)
}

func (e *layeredEvents) Dial(addr string, opts ...DialOption) (Conn, error) {
	return e.Events.Dial(addr, e.dialOptions(opts)...)
}

func (e *layeredEvents) DialContext(ctx context.Context, addr string, opts ...DialOption) (Conn, error) {
	return e.Events.DialContext(ctx, addr, e.dialOptions(opts)...)
}

func (e *layeredEvents) Listen(addr string, opts ...ListenOption) (Listener, error) {
	if len(e.layers) == 0 {
		return e.Events.Listen(addr, opts...)
	}
	return e.Events.Listen(addr, append([]ListenOption{WithLayers(e.layers...)}, opts...)...)
}

func (e *layeredEvents) Adopt(conn net.Conn, opts ...AdoptOption) (Conn, error) {
	return e.AdoptContext(context.Background(), conn, opts...)
}

func (e *layeredEvents) AdoptContext(ctx context.Context, conn net.Conn, opts ...AdoptOption) (Conn, error) {
	if len(e.layers) == 0 {
		return e.Events.AdoptContext(ctx, conn, opts...)
	}
	return e.Events.AdoptContext(ctx, conn, append([]AdoptOption{WithLayers(e.layers...)}, opts...)...)
}

func dialLayered(t *testing.T, serverAddr string, layers []Layer, setup func(*Events)) *layeredEvents {
	t.Helper()
	started := make(chan struct{})
	events := &Events{Pollers: 1}
	events.Hooks = FuncHandler{Start: func(*Events) { close(started) }}
	events.Handler = FuncHandler{
		Open: func(Conn) {},
		Read: func(c Conn) error {
			_, _ = c.Discard(c.InboundBuffered())
			return nil
		},
		Close: func(Conn, error) {},
	}
	if setup != nil {
		setup(events)
	}
	done := make(chan error, 1)
	go func() { done <- events.Serve() }()
	t.Cleanup(func() {
		_ = events.Close(nil)
		<-done
	})
	<-started
	_ = serverAddr
	return &layeredEvents{Events: events, layers: layers}
}

// A layered dial settles before DialContext returns: the connection it
// returns is the top layer's — its writes leave transformed, its
// userdata is what the dial passed.
func TestUseLayersDialedConnections(t *testing.T) {
	const key byte = 0x5a
	addr, echo := serveLayered(t, []Layer{mapBytes(key, xor)}, func(ev *Events) {})
	events := dialLayered(t, addr, []Layer{mapBytes(key, xor)}, func(ev *Events) {})

	ctx, cancel := context.WithTimeout(context.Background(), layerWait)
	defer cancel()
	conn, err := events.DialContext(ctx, "tcp://"+addr, WithUserdata("dial-userdata"))
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
	events := dialLayered(t, silent.Addr().String(), []Layer{LayerFunc(func(LayerContext) LayerHandler { return refuseLayer{cause: refused} })}, func(ev *Events) {

	})
	events.Handler = FuncHandler{
		Open:  func(Conn) { atomic.AddInt32(&calls, 1) },
		Read:  func(Conn) error { return nil },
		Close: func(Conn, error) { atomic.AddInt32(&calls, 1) },
	}
	ctx, cancel := context.WithTimeout(context.Background(), layerWait)
	defer cancel()
	_, err = events.DialContext(ctx, "tcp://"+silent.Addr().String())
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
	events := dialLayered(t, silent.Addr().String(), []Layer{LayerFunc(func(LayerContext) LayerHandler { return neverOpens{} })}, func(ev *Events) {

	})
	events.Handler = FuncHandler{
		Open:  func(Conn) { atomic.AddInt32(&calls, 1) },
		Read:  func(Conn) error { return nil },
		Close: func(Conn, error) { atomic.AddInt32(&calls, 1) },
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err = events.DialContext(ctx, "tcp://"+silent.Addr().String())
	if err == nil {
		t.Fatal("a canceled dial reported no error")
	}
	if n := atomic.LoadInt32(&calls); n != 0 {
		t.Fatalf("a canceled dial ran %d Handler callbacks", n)
	}
}

// refuseLayer fails its connection as it opens.
type refuseLayer struct {
	PassThrough
	cause error
}

func (r refuseLayer) OnOpen(ctx LayerContext) { _ = ctx.Abort(r.cause) }

// neverOpens negotiates forever: it never opens the layers above.
type neverOpens struct{ PassThrough }

func (neverOpens) OnOpen(LayerContext) {}

// Two layers and the Handler each arm a timer of their own: the lower layer
// at 200ms, the upper at 30ms, the Handler at 80ms. Every one fires: no slot
// replaces another, as the single slot all of them once shared did.
func TestEveryLayerHasItsOwnTimer(t *testing.T) {
	var lowerFired, upperFired, handlerFired atomic.Int32
	lower := LayerFunc(func(LayerContext) LayerHandler {
		return &timerLayer{after: 200 * time.Millisecond, fired: &lowerFired}
	})
	upper := LayerFunc(func(LayerContext) LayerHandler {
		return &timerLayer{after: 30 * time.Millisecond, fired: &upperFired}
	})
	started := make(chan struct{})
	events := &Events{Pollers: 1}
	events.Hooks = FuncHandler{Start: func(*Events) { close(started) }}
	events.Handler = FuncHandler{
		Open: func(c Conn) { _ = c.SetWakeDeadline(time.Now().Add(80 * time.Millisecond)) },
		Read: func(c Conn) error {
			if len(c.PeekChunk()) == 0 {
				handlerFired.Add(1)
			}
			return nil
		},
		Close: func(Conn, error) {},
	}
	done := make(chan error, 1)
	go func() { done <- events.Serve() }()
	t.Cleanup(func() { _ = events.Close(nil); <-done })
	<-started
	addr, _ := serveLayered(t, []Layer{passThroughLayer{}, passThroughLayer{}}, func(ev *Events) {})
	ctx, cancel := context.WithTimeout(context.Background(), layerWait)
	defer cancel()
	if _, err := events.DialContext(ctx, "tcp://"+addr, WithLayers(lower, upper)); err != nil {
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
	ctx.Up().Open()
}

func (l *timerLayer) OnTimer(LayerContext) error {
	l.fired.Add(1)
	return nil
}

// passThroughLayer is a layer that changes nothing.
type passThroughLayer struct{}

func (passThroughLayer) Attach(LayerContext) LayerHandler { return passThroughHandler{} }

// A turn that delivers nothing still reaches the Handler: a wake through a
// stack of layers that pass nothing up arrives all the same.
func TestEmptyTurnsReachTheHandler(t *testing.T) {
	quiet := LayerFunc(func(LayerContext) LayerHandler { return &holdBack{} })
	turns := make(chan struct{}, 8)
	started := make(chan struct{})
	events := &Events{Pollers: 1}
	events.Hooks = FuncHandler{Start: func(*Events) { close(started) }}
	events.Handler = FuncHandler{
		Open: func(Conn) {},
		Read: func(Conn) error {
			select {
			case turns <- struct{}{}:
			default:
			}
			return nil
		},
		Close: func(Conn, error) {},
	}
	done := make(chan error, 1)
	go func() { done <- events.Serve() }()
	t.Cleanup(func() { _ = events.Close(nil); <-done })
	<-started
	addr, _ := serveLayered(t, nil, nil)
	ctx, cancel := context.WithTimeout(context.Background(), layerWait)
	defer cancel()
	conn, err := events.DialContext(ctx, "tcp://"+addr, WithLayers(quiet, quiet))
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

func (*holdBack) OnRead(ctx LayerContext) error {
	lower := ctx.Down()
	_, err := lower.Discard(len(lower.PeekChunk()))
	return err
}

// The Handler's Userdata is the dial's, through every layer, and what a
// layer sets there the Handler reads.
func TestUserdataIsTheConnectionsThroughEveryLayer(t *testing.T) {
	seen := make(chan any, 2)
	marker := LayerFunc(func(LayerContext) LayerHandler { return &userdataLayer{seen: seen} })
	started := make(chan struct{})
	events := &Events{Pollers: 1}
	events.Hooks = FuncHandler{Start: func(*Events) { close(started) }}
	events.Handler = FuncHandler{
		Open:  func(c Conn) { signal(seen, c.Userdata()) },
		Read:  func(Conn) error { return nil },
		Close: func(Conn, error) {},
	}
	done := make(chan error, 1)
	go func() { done <- events.Serve() }()
	t.Cleanup(func() { _ = events.Close(nil); <-done })
	<-started
	addr, _ := serveLayered(t, nil, nil)
	ctx, cancel := context.WithTimeout(context.Background(), layerWait)
	defer cancel()
	conn, err := events.DialContext(ctx, "tcp://"+addr, WithLayers(marker, passThroughLayer{}), WithUserdata("dialed"))
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
	u.seen <- ctx.Down().Userdata()
	ctx.Up().Open()
}

// A connection ends once, top down: the Handler first, then every layer.
func TestCloseReachesEveryLayerOnce(t *testing.T) {
	var order []string
	var mu sync.Mutex
	all := make(chan struct{}) // closed once the Handler and both layers heard
	record := func(name string) {
		mu.Lock()
		order = append(order, name)
		if len(order) == 3 {
			close(all)
		}
		mu.Unlock()
	}
	layer := func(name string) Layer {
		return LayerFunc(func(LayerContext) LayerHandler { return &closeLayer{name: name, record: record} })
	}
	started := make(chan struct{})
	events := &Events{Pollers: 1}
	events.Hooks = FuncHandler{Start: func(*Events) { close(started) }}
	events.Handler = FuncHandler{
		Open:  func(Conn) {},
		Read:  func(Conn) error { return nil },
		Close: func(Conn, error) { record("handler") },
	}
	done := make(chan error, 1)
	go func() { done <- events.Serve() }()
	t.Cleanup(func() { _ = events.Close(nil); <-done })
	<-started
	addr, _ := serveLayered(t, nil, nil)
	ctx, cancel := context.WithTimeout(context.Background(), layerWait)
	defer cancel()
	conn, err := events.DialContext(ctx, "tcp://"+addr, WithLayers(layer("bottom"), layer("top")))
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	layerWaitFor(t, all, "every OnClose")
	time.Sleep(10 * time.Millisecond) // a fourth, were there one, would show
	mu.Lock()
	defer mu.Unlock()
	if got := strings.Join(order, ","); got != "handler,top,bottom" {
		t.Fatalf("close order = %s, want handler,top,bottom", got)
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
	addr, echo := serveLayered(t, []Layer{mapBytes(key, xor)}, func(ev *Events) {})
	events := dialLayered(t, addr, []Layer{mapBytes(key, xor)}, func(ev *Events) {})
	ctx, cancel := context.WithTimeout(context.Background(), layerWait)
	defer cancel()
	conn, err := events.DialContext(ctx, "tcp://"+addr)
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
		Read: func(c Conn) error {
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
	done := make(chan error, 1)
	go func() { done <- events.Serve() }()
	t.Cleanup(func() { _ = events.Close(nil); <-done })
	<-started
	listener, err := events.Listen("tcp://127.0.0.1:0")
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

func (*tinyChunks) OnRead(ctx LayerContext) error {
	lower := ctx.Down()
	chunk := lower.PeekChunk()
	if len(chunk) > 1<<10 {
		chunk = chunk[:1<<10]
	}
	if len(chunk) == 0 {
		return nil
	}
	ctx.Up().Write(chunk)
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
	events := dialLayered(t, silent.Addr().String(), []Layer{passThroughLayer{}}, func(ev *Events) {
		ev.MaxOutboundBuffered = 64 << 10

	})
	ctx, cancel := context.WithTimeout(context.Background(), layerWait)
	defer cancel()
	conn, err := events.DialContext(ctx, "tcp://"+silent.Addr().String())
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

// MaxOutboundBuffered is checked as a write enters the stack, never below
// it: a layer that writes twice for every write it is given — a header and
// a body, two TLS records — never has its second write refused after its
// first went out. Every write from above is taken whole or refused whole.
func TestOutboundLimitAdmitsAtTheTopOfTheStack(t *testing.T) {
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
	twice := LayerFunc(func(LayerContext) LayerHandler { return writeTwice{} })
	events := dialLayered(t, silent.Addr().String(), []Layer{twice, passThroughLayer{}}, func(ev *Events) {
		ev.MaxOutboundBuffered = 64 << 10

	})
	ctx, cancel := context.WithTimeout(context.Background(), layerWait)
	defer cancel()
	conn, err := events.DialContext(ctx, "tcp://"+silent.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	half := make([]byte, 6<<10)
	vec := [][]byte{half, half}
	for i := range 1 << 14 {
		var n int
		if i%2 == 0 {
			n, err = conn.Write(append(half, half...))
		} else {
			n, err = conn.Writev(vec)
		}
		if errors.Is(err, ErrOutboundOverflow) {
			if n != 0 {
				t.Fatalf("a refused write took %d bytes", n)
			}
			return
		}
		if err != nil || n != 2*len(half) {
			t.Fatalf("write %d = %d, %v: want it taken whole", i, n, err)
		}
	}
	t.Fatal("the queue never filled against a peer that reads nothing")
}

// writeTwice writes what it is given in two halves.
type writeTwice struct{ PassThrough }

func (writeTwice) OnWrite(ctx LayerContext, p []byte) (int, error) {
	half := len(p) / 2
	if _, err := ctx.Down().Write(p[:half]); err != nil {
		return 0, err
	}
	if _, err := ctx.Down().Write(p[half:]); err != nil {
		return half, err
	}
	return len(p), nil
}

// A layer that observes sends hears every byte the socket sent, counted on
// the wire — its own and those of the layers above — whether or not an
// Observer listens, and an Observer still hears them all as well.
func TestWireCounterObserverHearsTheWire(t *testing.T) {
	for _, withObserver := range []bool{false, true} {
		t.Run(map[bool]string{false: "alone", true: "beside an Observer"}[withObserver], func(t *testing.T) {
			addr, echo := serveLayered(t, nil, nil)
			var heard, observed atomic.Int64
			counter := LayerFunc(func(LayerContext) LayerHandler { return &sendCounter{heard: &heard} })
			events := dialLayered(t, addr, []Layer{counter, mapBytes(0x00, xor)}, func(ev *Events) {

				if withObserver {
					ev.Observer = FuncHandler{BytesWritten: func(_ Conn, n int) { observed.Add(int64(n)) }}
				}
			})
			ctx, cancel := context.WithTimeout(context.Background(), layerWait)
			defer cancel()
			conn, err := events.DialContext(ctx, "tcp://"+addr)
			if err != nil {
				t.Fatal(err)
			}
			const total = 64 << 10
			chunk := make([]byte, 1<<10)
			for range total / len(chunk) {
				if _, err := conn.Write(chunk); err != nil {
					t.Fatal(err)
				}
			}
			for got := 0; got < total; {
				got += len(layerWaitFor(t, echo.read, "the server's read"))
			}
			deadline := time.Now().Add(layerWait)
			for heard.Load() < total && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			if got := heard.Load(); got != total {
				t.Fatalf("the layer heard %d bytes sent, want %d", got, total)
			}
			if withObserver {
				for observed.Load() < total && time.Now().Before(deadline) {
					time.Sleep(time.Millisecond)
				}
				if got := observed.Load(); got != total {
					t.Fatalf("the Observer heard %d bytes sent, want %d", got, total)
				}
			}
		})
	}
}

// sendCounter adds up what the socket reports sent.
type sendCounter struct {
	PassThrough
	heard *atomic.Int64
}

func (s *sendCounter) OnWireSent(_ LayerContext, n int) { s.heard.Add(int64(n)) }

// A goodbye reaches the peer behind everything queued before it: the socket
// closes once its queue emptied, not as the close is asked for, on either
// backend. A peer that reads nothing more holds the close for the goodbye
// timeout at most.
func TestGoodbyeLeavesBeforeTheSocketCloses(t *testing.T) {
	goodbye := LayerFunc(func(LayerContext) LayerHandler { return sayGoodbye{} })
	run := func(t *testing.T, read bool) {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer listener.Close()
		start := make(chan struct{})
		received := make(chan []byte, 1)
		go func() {
			peer, err := listener.Accept()
			if err != nil {
				return
			}
			defer peer.Close()
			<-start
			if !read {
				time.Sleep(layerWait) // reads nothing while the close waits
				return
			}
			all, _ := io.ReadAll(peer)
			received <- all
		}()
		closed := make(chan error, 1)
		events := dialLayered(t, listener.Addr().String(), []Layer{goodbye}, func(ev *Events) {
			ev.Handler = FuncHandler{
				Open:  func(Conn) {},
				Read:  func(c Conn) error { _, _ = c.Discard(c.InboundBuffered()); return nil },
				Close: func(_ Conn, err error) { signal(closed, err) },
			}

		})
		ctx, cancel := context.WithTimeout(context.Background(), layerWait)
		defer cancel()
		conn, err := events.DialContext(ctx, "tcp://"+listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		const total = 8 << 20
		chunk := bytes.Repeat([]byte{'d'}, 64<<10)
		for sent := 0; sent < total; sent += len(chunk) {
			if _, err := conn.Write(chunk); err != nil {
				t.Fatal(err)
			}
		}
		if err := conn.Close(); err != nil {
			t.Fatal(err)
		}
		close(start)
		began := time.Now()
		if !read {
			if err := layerWaitFor(t, closed, "the close of a peer that reads nothing"); errors.Is(err, os.ErrDeadlineExceeded) {
				t.Fatalf("closed with %v, want the close's own cause", err)
			}
			if waited := time.Since(began); waited > lingerTimeout+time.Second {
				t.Fatalf("the close waited %v, past the goodbye timeout", waited)
			}
			return
		}
		got := layerWaitFor(t, received, "the peer's read")
		if len(got) != total+len("bye") || string(got[total:]) != "bye" {
			t.Fatalf("the peer read %d bytes ending %q, want %d and the goodbye", len(got), got[max(0, len(got)-3):], total+3)
		}
		layerWaitFor(t, closed, "the close")
	}
	t.Run("peer reads", func(t *testing.T) { run(t, true) })
	t.Run("peer reads nothing", func(t *testing.T) {
		saved := lingerTimeout
		lingerTimeout = 200 * time.Millisecond
		t.Cleanup(func() { lingerTimeout = saved })
		run(t, false)
	})
}

// sayGoodbye writes "bye" as the layer above closes.
type sayGoodbye struct{ PassThrough }

func (sayGoodbye) CloseGracefully(ctx LayerContext, _ error) { _, _ = ctx.Down().Write([]byte("bye")) }

// A close with an error aborts: the goodbye is written, but the socket does
// not wait for a peer that reads nothing.
func TestAbortDoesNotWaitForTheGoodbye(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		peer, err := listener.Accept()
		if err == nil {
			time.Sleep(layerWait) // reads nothing
			_ = peer.Close()
		}
	}()
	closed := make(chan error, 1)
	events := dialLayered(t, listener.Addr().String(), []Layer{LayerFunc(func(LayerContext) LayerHandler { return sayGoodbye{} })}, func(ev *Events) {
		ev.Handler = FuncHandler{
			Open:  func(Conn) {},
			Read:  func(Conn) error { return nil },
			Close: func(_ Conn, err error) { signal(closed, err) },
		}

	})
	ctx, cancel := context.WithTimeout(context.Background(), layerWait)
	defer cancel()
	conn, err := events.DialContext(ctx, "tcp://"+listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	chunk := make([]byte, 64<<10)
	for range 128 {
		if _, err := conn.Write(chunk); err != nil {
			t.Fatal(err)
		}
	}
	aborted := errors.New("aborted")
	began := time.Now()
	_ = conn.CloseWith(aborted)
	if err := layerWaitFor(t, closed, "the abort"); !errors.Is(err, aborted) {
		t.Fatalf("closed with %v, want the abort's cause", err)
	}
	if waited := time.Since(began); waited >= LingerTimeout {
		t.Fatalf("the abort waited %v for the goodbye", waited)
	}
}

// Writev through a layer is one write whole, as the socket's is: concurrent
// vectors never interleave their segments.
func TestWritevThroughALayerIsWhole(t *testing.T) {
	const writers, each = 2, 2000
	addr, echo := serveLayered(t, nil, nil)
	events := dialLayered(t, addr, []Layer{passThroughLayer{}}, func(ev *Events) {})
	ctx, cancel := context.WithTimeout(context.Background(), layerWait)
	defer cancel()
	conn, err := events.DialContext(ctx, "tcp://"+addr)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for w := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			mark := byte('A' + w)
			for range each {
				if _, err := conn.Writev([][]byte{{mark}, bytes.Repeat([]byte{mark}, 7)}); err != nil {
					return
				}
			}
		}()
	}
	wg.Wait()
	var got []byte
	deadline := time.After(layerWait)
	for len(got) < writers*each*8 {
		select {
		case chunk := <-echo.read:
			got = append(got, chunk...)
		case <-deadline:
			t.Fatalf("read %d of %d bytes", len(got), writers*each*8)
		}
	}
	for i := 0; i < len(got); i += 8 {
		if !bytes.Equal(got[i:i+8], bytes.Repeat(got[i:i+1], 8)) {
			t.Fatalf("vector at %d torn: %q", i, got[i:i+8])
		}
	}
}

// A dial that gives up while a turn is opening the stack reports its cause,
// and the Handler hears nothing: the turn that would open it finds the dial
// gone.
func TestDialGivingUpMidOpenRunsNoCallbacks(t *testing.T) {
	addr, _ := serveLayered(t, nil, nil)
	var calls atomic.Int32
	events := dialLayered(t, addr, []Layer{LayerFunc(func(LayerContext) LayerHandler { return slowOpen{} })}, func(ev *Events) {
		ev.Handler = FuncHandler{
			Open:  func(Conn) { calls.Add(1) },
			Read:  func(Conn) error { return nil },
			Close: func(Conn, error) { calls.Add(1) },
		}

	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := events.DialContext(ctx, "tcp://"+addr); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("DialContext = %v, want the deadline", err)
	}
	time.Sleep(200 * time.Millisecond) // the slow open finishes meanwhile
	if n := calls.Load(); n != 0 {
		t.Fatalf("a dial that gave up ran %d Handler callbacks", n)
	}
}

// slowOpen opens the layers above after a pause in its OnOpen.
type slowOpen struct{ PassThrough }

func (slowOpen) OnOpen(ctx LayerContext) {
	time.Sleep(100 * time.Millisecond)
	ctx.Up().Open()
}

// While a graceful close waits for its queue, what the peer sends is
// discarded, not kept: a peer that floods and reads nothing cannot grow the
// connection's memory for as long as the close waits.
func TestLingeringCloseDiscardsWhatArrives(t *testing.T) {
	saved := lingerTimeout
	lingerTimeout = time.Second
	t.Cleanup(func() { lingerTimeout = saved })
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	flooding := make(chan struct{})
	go func() {
		peer, err := listener.Accept()
		if err != nil {
			return
		}
		defer peer.Close()
		<-flooding
		chunk := make([]byte, 64<<10)
		for {
			if _, err := peer.Write(chunk); err != nil {
				return
			}
		}
	}()
	closed := make(chan error, 1)
	events := dialLayered(t, listener.Addr().String(), []Layer{passThroughLayer{}}, func(ev *Events) {
		ev.Handler = FuncHandler{
			Open:  func(Conn) {},
			Read:  func(Conn) error { return nil },
			Close: func(_ Conn, err error) { signal(closed, err) },
		}

	})
	ctx, cancel := context.WithTimeout(context.Background(), layerWait)
	defer cancel()
	conn, err := events.DialContext(ctx, "tcp://"+listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	chunk := make([]byte, 64<<10)
	for range 128 { // more than the peer, which reads nothing, takes in
		if _, err := conn.Write(chunk); err != nil {
			t.Fatal(err)
		}
	}
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	_ = conn.Close()
	close(flooding)
	time.Sleep(500 * time.Millisecond)
	var during runtime.MemStats
	runtime.ReadMemStats(&during)
	if grown := int64(during.HeapAlloc) - int64(before.HeapAlloc); grown > 64<<20 {
		t.Fatalf("the heap grew %d MiB while the close waited", grown>>20)
	}
	layerWaitFor(t, closed, "the close")
}

// The end of the stream passes up through PassThrough: with AllowHalfOpen
// the Handler reads io.EOF after what arrived, and goes on writing.
func TestEndOfStreamPassesThroughLayers(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	answer := make(chan []byte, 1)
	go func() {
		peer, err := listener.Accept()
		if err != nil {
			return
		}
		defer peer.Close()
		_, _ = peer.Write([]byte("last words"))
		_ = peer.(*net.TCPConn).CloseWrite()
		all, _ := io.ReadAll(peer)
		answer <- all
	}()
	ended := make(chan []byte, 1)
	events := dialLayered(t, listener.Addr().String(), []Layer{passThroughLayer{}, passThroughLayer{}}, func(ev *Events) {
		ev.AllowHalfOpen = true
		var read []byte
		ev.Handler = FuncHandler{
			Open: func(Conn) {},
			Read: func(c Conn) error {
				buf := make([]byte, 64)
				for {
					n, err := c.Read(buf)
					read = append(read, buf[:n]...)
					if err == io.EOF {
						_, _ = c.Write([]byte("reply"))
						_ = c.CloseWrite()
						ended <- read
						return nil
					}
					if n == 0 {
						return nil
					}
				}
			},
			Close: func(Conn, error) {},
		}

	})
	ctx, cancel := context.WithTimeout(context.Background(), layerWait)
	defer cancel()
	if _, err := events.DialContext(ctx, "tcp://"+listener.Addr().String()); err != nil {
		t.Fatal(err)
	}
	if got := string(layerWaitFor(t, ended, "io.EOF above the layers")); got != "last words" {
		t.Fatalf("read %q before io.EOF", got)
	}
	if got := string(layerWaitFor(t, answer, "the reply after the end")); got != "reply" {
		t.Fatalf("the peer read %q", got)
	}
}

// A layer with no way to end one direction does not pass CloseWrite on to a
// layer below whose protocol would end under it.
func TestCloseWriteNeedsTheLayersSupport(t *testing.T) {
	addr, _ := serveLayered(t, nil, nil)
	events := dialLayered(t, addr, []Layer{passThroughLayer{}, LayerFunc(func(LayerContext) LayerHandler { return &framing{} })}, func(ev *Events) {

	})
	ctx, cancel := context.WithTimeout(context.Background(), layerWait)
	defer cancel()
	conn, err := events.DialContext(ctx, "tcp://"+addr)
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.CloseWrite(); err == nil {
		t.Fatal("CloseWrite passed through a layer that cannot end one direction")
	}
}

// framing is a layer that implements LayerHandler without PassThrough.
type framing struct{}

func (*framing) OnOpen(ctx LayerContext)                         { ctx.Up().Open() }
func (*framing) OnRead(LayerContext) error                       { return nil }
func (*framing) OnWrite(ctx LayerContext, p []byte) (int, error) { return ctx.Down().Write(p) }
func (*framing) OnTimer(LayerContext) error                      { return nil }
func (*framing) OnClose(LayerContext, error)                     {}

// FindLayer looks down the whole stack; what a layer holds counts as queued.
func TestFindLayerAndHeldOutput(t *testing.T) {
	addr, _ := serveLayered(t, nil, nil)
	holder := &holdsOutput{}
	events := dialLayered(t, addr, []Layer{LayerFunc(func(LayerContext) LayerHandler { return holder }), passThroughLayer{}}, func(ev *Events) {
		ev.MaxOutboundBuffered = 100

	})
	ctx, cancel := context.WithTimeout(context.Background(), layerWait)
	defer cancel()
	conn, err := events.DialContext(ctx, "tcp://"+addr)
	if err != nil {
		t.Fatal(err)
	}
	if found, ok := FindLayer[*holdsOutput](conn); !ok || found != holder {
		t.Fatal("FindLayer missed the bottom layer")
	}
	if _, ok := FindLayer[*framing](conn); ok {
		t.Fatal("FindLayer found a layer the stack does not run")
	}
	holder.held.Store(90)
	if got := conn.OutboundBuffered(); got < 90 {
		t.Fatalf("OutboundBuffered = %d, want the 90 bytes the layer holds counted", got)
	}
	if _, err := conn.Write(make([]byte, 20)); !errors.Is(err, ErrOutboundOverflow) {
		t.Fatalf("a write past the limit with held bytes = %v, want ErrOutboundOverflow", err)
	}
}

type holdsOutput struct {
	PassThrough
	held atomic.Int64
}

func (h *holdsOutput) OutboundHeld(LayerContext) int { return int(h.held.Load()) }

func (h *holdsOutput) CloseWrite(ctx LayerContext) error { return ctx.Down().CloseWrite() }

// A graceful close waits for a slow peer that keeps reading past the linger
// bound: the bound is on progress, not on the whole drain.
//
// Native only: the stdio backend reports progress once a whole write batch
// went out, and a batch to a peer this slow outlasts the test's short bound.
func TestLingerWaitsForASlowPeer(t *testing.T) {
	if stdioBackend {
		t.Skip("stdio reports a write batch's progress once it all went out")
	}
	saved := lingerTimeout
	lingerTimeout = 150 * time.Millisecond
	t.Cleanup(func() { lingerTimeout = saved })
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	// More than the kernel's buffers hold, read at a steady pace that takes
	// several times the bound in all: the queue stays busy past the bound,
	// and moves within it.
	const total = 32 << 20
	received := make(chan int, 1)
	go func() {
		peer, err := listener.Accept()
		if err != nil {
			return
		}
		defer peer.Close()
		buf := make([]byte, 1<<20)
		got := 0
		for {
			time.Sleep(20 * time.Millisecond)
			n, err := io.ReadFull(peer, buf)
			got += n
			if err != nil {
				received <- got
				return
			}
		}
	}()
	events := dialLayered(t, listener.Addr().String(), []Layer{LayerFunc(func(LayerContext) LayerHandler { return sayGoodbye{} })}, func(ev *Events) {

	})
	ctx, cancel := context.WithTimeout(context.Background(), layerWait)
	defer cancel()
	conn, err := events.DialContext(ctx, "tcp://"+listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	chunk := make([]byte, 64<<10)
	for sent := 0; sent < total; sent += len(chunk) {
		if _, err := conn.Write(chunk); err != nil {
			t.Fatal(err)
		}
	}
	_ = conn.Close()
	select {
	case got := <-received:
		if got != total+len("bye") {
			t.Fatalf("the slow peer read %d bytes, want %d", got, total+len("bye"))
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the slow peer never finished")
	}
}

// MaxOutboundBuffered holds against concurrent writers through a layer: each
// write reserves its room as it enters the stack, so writers racing past the
// check while another is still inside a layer cannot all fit at once. The
// layer here takes its time, as an encryption does, and adds no bytes, so the
// queue must never pass the limit at all.
func TestOutboundLimitHoldsForConcurrentWriters(t *testing.T) {
	const limit, size, writers = 256 << 10, 16 << 10, 64
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
			defer c.Close() // reads nothing
		}
	}()
	events := dialLayered(t, silent.Addr().String(), []Layer{LayerFunc(func(LayerContext) LayerHandler { return slowEncode{} })}, func(ev *Events) {
		ev.MaxOutboundBuffered = limit

	})
	ctx, cancel := context.WithTimeout(context.Background(), layerWait)
	defer cancel()
	conn, err := events.DialContext(ctx, "tcp://"+silent.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	var most atomic.Int64
	var sampling sync.WaitGroup
	sampling.Add(1)
	go func() {
		defer sampling.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			if q := int64(conn.OutboundBuffered()); q > most.Load() {
				most.Store(q)
			}
		}
	}()
	var writing sync.WaitGroup
	var refused atomic.Int64
	deadline := time.Now().Add(300 * time.Millisecond)
	for range writers {
		writing.Add(1)
		go func() {
			defer writing.Done()
			chunk := make([]byte, size)
			for time.Now().Before(deadline) {
				_, err := conn.Write(chunk)
				if errors.Is(err, ErrOutboundOverflow) {
					refused.Add(1)
				} else if err != nil {
					return
				}
			}
		}()
	}
	writing.Wait()
	close(stop)
	sampling.Wait()
	if got := most.Load(); got > limit {
		t.Fatalf("the queue reached %d bytes, past the %d limit", got, limit)
	}
	if refused.Load() == 0 {
		t.Fatal("no write was refused: the test never reached the limit")
	}
}

// slowEncode takes a moment over every write before it passes it on, as an
// encryption does, and adds no bytes.
type slowEncode struct{ PassThrough }

func (slowEncode) OnWrite(ctx LayerContext, p []byte) (int, error) {
	time.Sleep(200 * time.Microsecond)
	return ctx.Down().Write(p)
}

// Discard(-1) drops everything a layer delivered, as the socket's does —
// FuncHandler's default OnRead calls it — and leaves the connection whole for
// what arrives next.
func TestDiscardAllThroughALayer(t *testing.T) {
	for _, defaulted := range []bool{false, true} {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		next := make(chan struct{})
		got := make(chan []byte, 1)
		go func() {
			c, err := listener.Accept()
			if err != nil {
				return
			}
			defer c.Close()
			_, _ = c.Write([]byte("first"))
			<-next
			_, _ = c.Write([]byte("second"))
			all, _ := io.ReadAll(c)
			got <- all
		}()
		reads := make(chan string, 4)
		var handler FuncHandler
		if defaulted {
			// No Read: the default discards everything, and the reply below
			// shows the connection still works.
			handler = FuncHandler{Open: func(c Conn) {
				go func() { <-time.After(100 * time.Millisecond); _, _ = c.Write([]byte("alive")); _ = c.Close() }()
			}}
		} else {
			handler = FuncHandler{
				Open: func(Conn) {},
				Read: func(c Conn) error {
					if chunk := c.PeekChunk(); len(chunk) > 0 {
						reads <- string(chunk)
						_, _ = c.Discard(-1)
					}
					return nil
				},
				Close: func(Conn, error) {},
			}
		}
		events := dialLayered(t, listener.Addr().String(), []Layer{passThroughLayer{}}, func(ev *Events) {
			ev.Handler = handler

		})
		ctx, cancel := context.WithTimeout(context.Background(), layerWait)
		conn, err := events.DialContext(ctx, "tcp://"+listener.Addr().String())
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		if defaulted {
			close(next)
			if all := string(layerWaitFor(t, got, "the reply")); all != "alive" {
				t.Fatalf("the peer read %q", all)
			}
		} else {
			if s := layerWaitFor(t, reads, "the first read"); s != "first" {
				t.Fatalf("read %q", s)
			}
			close(next)
			if s := layerWaitFor(t, reads, "the second read"); s != "second" {
				t.Fatalf("read %q after Discard(-1)", s)
			}
			_ = conn.Close()
		}
		listener.Close()
	}
}

// MaxInboundBuffered bounds what a stack holds for a Handler that reads
// nothing: the layers stop pulling once they hold it, the socket's own hold
// stops its reads, and once the Handler reads again everything arrives.
func TestInboundLimitCoversTheStack(t *testing.T) {
	const limit, total = 4 << 10, 1 << 20
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		c, err := listener.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		_, _ = c.Write(make([]byte, total))
		time.Sleep(layerWait)
	}()
	var reading atomic.Bool
	var read, most atomic.Int64
	events := dialLayered(t, listener.Addr().String(), []Layer{passThroughLayer{}, passThroughLayer{}}, func(ev *Events) {
		ev.MaxInboundBuffered = limit
		ev.Handler = FuncHandler{
			Open: func(Conn) {},
			Read: func(c Conn) error {
				if held := int64(c.InboundBuffered()); held > most.Load() {
					most.Store(held)
				}
				if reading.Load() {
					n, _ := c.Discard(-1)
					read.Add(int64(n))
				}
				return nil
			},
			Close: func(Conn, error) {},
		}

	})
	ctx, cancel := context.WithTimeout(context.Background(), layerWait)
	defer cancel()
	conn, err := events.DialContext(ctx, "tcp://"+listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	if got := most.Load(); got > 2*limit+16<<10 {
		t.Fatalf("the Handler was handed %d bytes it did not read, past the %d limit", got, limit)
	}
	reading.Store(true)
	_ = conn.Wake()
	deadline := time.Now().Add(layerWait)
	for read.Load() < total {
		if time.Now().After(deadline) {
			t.Fatalf("read %d of %d bytes once the Handler read again", read.Load(), total)
		}
		time.Sleep(time.Millisecond)
	}
}

// A layer that holds its output until flushed still lets the layers above
// it handshake: UIO flushes it at the end of every turn.
func TestHeldOutputIsFlushedAtTheTurnsEnd(t *testing.T) {
	addr, echo := serveLayered(t, nil, nil)
	events := dialLayered(t, addr, []Layer{LayerFunc(func(LayerContext) LayerHandler { return &holdUntilFlush{} }), LayerFunc(func(LayerContext) LayerHandler { return greeter{} })}, func(ev *Events) {

	})
	ctx, cancel := context.WithTimeout(context.Background(), layerWait)
	defer cancel()
	if _, err := events.DialContext(ctx, "tcp://"+addr); err != nil {
		t.Fatal(err)
	}
	if got := string(layerWaitFor(t, echo.read, "the greeting")); got != "hello" {
		t.Fatalf("the server read %q", got)
	}
}

// holdUntilFlush holds what is written to it until Flush.
type holdUntilFlush struct {
	PassThrough
	mu      sync.Mutex
	held    []byte
	counted atomic.Int64 // held, and what a flush is writing below
}

func (h *holdUntilFlush) OnWrite(_ LayerContext, p []byte) (int, error) {
	h.mu.Lock()
	h.held = append(h.held, p...)
	h.counted.Add(int64(len(p)))
	h.mu.Unlock()
	return len(p), nil
}

func (h *holdUntilFlush) OutboundHeld(LayerContext) int { return int(h.counted.Load()) }

func (h *holdUntilFlush) CloseWrite(ctx LayerContext) error { return ctx.Down().CloseWrite() }

func (h *holdUntilFlush) Flush(ctx LayerContext) error {
	h.mu.Lock()
	held := h.held
	h.held = nil
	h.mu.Unlock()
	if len(held) == 0 {
		return nil
	}
	_, err := ctx.Down().Write(held)
	h.counted.Add(-int64(len(held)))
	return err
}

// greeter writes a greeting as it opens, as a handshake's first flight
// does, and opens the layers above at once.
type greeter struct{ PassThrough }

func (greeter) OnOpen(ctx LayerContext) {
	_, _ = ctx.Down().Write([]byte("hello"))
	ctx.Up().Open()
}

// An Events with layers refuses packet connections.
func TestLayersRefusePacketConnections(t *testing.T) {
	events := dialLayered(t, "", []Layer{passThroughLayer{}}, func(ev *Events) {})
	if _, err := events.Listen("udp://127.0.0.1:0"); !errors.Is(err, ErrLayersNeedStream) {
		t.Fatalf("Listen udp = %v, want ErrLayersNeedStream", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), layerWait)
	defer cancel()
	if _, err := events.DialContext(ctx, "udp://127.0.0.1:9"); !errors.Is(err, ErrLayersNeedStream) {
		t.Fatalf("DialContext udp = %v, want ErrLayersNeedStream", err)
	}
	packet, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer packet.Close()
	udp, err := net.Dial("udp", packet.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := events.Adopt(udp); !errors.Is(err, ErrLayersNeedStream) {
		t.Fatalf("Adopt udp = %v, want ErrLayersNeedStream", err)
	}
}

// More layers than the timers' mask holds is refused where the chain is
// given: no endpoint carries it.
func TestTooManyLayers(t *testing.T) {
	events := &Events{Pollers: 1, Handler: FuncHandler{}}
	started := make(chan struct{})
	events.Hooks = FuncHandler{Start: func(*Events) { close(started) }}
	go func() { _ = events.Serve() }()
	t.Cleanup(func() { _ = events.Close(nil) })
	<-started
	chain := make([]Layer, maxLayers+1)
	for i := range chain {
		chain[i] = PassThroughLayer
	}
	if _, err := events.Listen("tcp://127.0.0.1:0", WithLayers(chain...)); err == nil || !strings.Contains(err.Error(), "at most") {
		t.Fatalf("Listen = %v, want the layer cap refused", err)
	}
	if _, err := events.Dialer(WithLayers(chain...)); err == nil || !strings.Contains(err.Error(), "at most") {
		t.Fatalf("Dialer = %v, want the layer cap refused", err)
	}
}

// A timer a layer arms before it opens fires once it has, and does not spin
// the connection meanwhile.
func TestTimerArmedBeforeOpenFiresAfter(t *testing.T) {
	addr, _ := serveLayered(t, nil, nil)
	fired := make(chan time.Time, 1)
	var turns atomic.Int64
	events := dialLayered(t, addr, []Layer{LayerFunc(func(LayerContext) LayerHandler { return &openLate{} }), LayerFunc(func(ctx LayerContext) LayerHandler {
		_ = ctx.SetTimer(time.Now().Add(10 * time.Millisecond)) // before it opens
		return &earlyTimer{fired: fired}
	})}, func(ev *Events) {
		ev.Handler = FuncHandler{Open: func(Conn) {}, Read: func(Conn) error { turns.Add(1); return nil }, Close: func(Conn, error) {}}

	})
	began := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), layerWait)
	defer cancel()
	if _, err := events.DialContext(ctx, "tcp://"+addr); err != nil {
		t.Fatal(err)
	}
	at := layerWaitFor(t, fired, "the early timer")
	if at.Sub(began) < 100*time.Millisecond {
		t.Fatalf("the timer fired %v in, before its layer opened", at.Sub(began))
	}
	if n := turns.Load(); n > 20 {
		t.Fatalf("%d turns while the timer waited: it spun the connection", n)
	}
}

// openLate opens the layers above 150ms after it opens, on its own timer.
type openLate struct{ PassThrough }

func (*openLate) OnOpen(ctx LayerContext) { _ = ctx.SetTimer(time.Now().Add(150 * time.Millisecond)) }
func (*openLate) OnTimer(ctx LayerContext) error {
	ctx.Up().Open()
	return nil
}

type earlyTimer struct {
	PassThrough
	fired chan time.Time
}

func (e *earlyTimer) OnTimer(LayerContext) error {
	select {
	case e.fired <- time.Now():
	default:
	}
	return nil
}

// Closing from several goroutines at once says each goodbye once.
func TestConcurrentClosesSayGoodbyeOnce(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	got := make(chan []byte, 1)
	go func() {
		c, err := listener.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		all, _ := io.ReadAll(c)
		got <- all
	}()
	events := dialLayered(t, listener.Addr().String(), []Layer{LayerFunc(func(LayerContext) LayerHandler { return sayGoodbye{} }), LayerFunc(func(LayerContext) LayerHandler { return sayGoodbye{} })}, func(ev *Events) {

	})
	ctx, cancel := context.WithTimeout(context.Background(), layerWait)
	defer cancel()
	conn, err := events.DialContext(ctx, "tcp://"+listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() { defer wg.Done(); _ = conn.Close() }()
	}
	wg.Wait()
	if all := string(layerWaitFor(t, got, "the goodbyes")); all != "byebye" {
		t.Fatalf("the peer read %q, want each layer's goodbye once", all)
	}
}

// An abort in the Handler's OnRead drops what the layers hold: the turn
// that aborted flushes nothing more.
func TestAbortInOnInboundFlushesNothing(t *testing.T) {
	addr, _ := serveLayered(t, nil, nil)
	holder := &countingHold{}
	aborted := make(chan struct{})
	events := dialLayered(t, addr, []Layer{LayerFunc(func(LayerContext) LayerHandler { return holder })}, func(ev *Events) {
		ev.Handler = FuncHandler{
			Open: func(c Conn) {
				go func() {
					time.Sleep(20 * time.Millisecond)
					_ = c.Wake()
				}()
			},
			Read: func(c Conn) error {
				select {
				case <-aborted:
					return nil
				default:
				}
				_, _ = c.Write([]byte("never sent"))
				holder.aborting.Store(true)
				close(aborted)
				return c.CloseWith(errors.New("abort"))
			},
			Close: func(Conn, error) {},
		}

	})
	ctx, cancel := context.WithTimeout(context.Background(), layerWait)
	defer cancel()
	if _, err := events.DialContext(ctx, "tcp://"+addr); err != nil {
		t.Fatal(err)
	}
	layerWaitFor(t, aborted, "the abort")
	time.Sleep(50 * time.Millisecond)
	if n := holder.flushedAfter.Load(); n != 0 {
		t.Fatalf("the held output was flushed %d times after the abort", n)
	}
}

// countingHold holds what is written and counts the flushes that hand it
// down after the Handler aborted.
type countingHold struct {
	holdUntilFlush
	aborting     atomic.Bool
	flushedAfter atomic.Int32
}

func (h *countingHold) Flush(ctx LayerContext) error {
	if h.aborting.Load() && h.OutboundHeld(ctx) > 0 {
		h.flushedAfter.Add(1)
	}
	return h.holdUntilFlush.Flush(ctx)
}

// A layer that closes the connection in OnRead ends the turn there: the
// layers above and the Handler do not run on what it delivered.
func TestCloseFromALayerEndsTheTurn(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		c, err := listener.Accept()
		if err == nil {
			_, _ = c.Write([]byte("last"))
			time.Sleep(layerWait)
			c.Close()
		}
	}()
	seen := make(chan string, 4)
	closed := make(chan error, 1)
	events := dialLayered(t, listener.Addr().String(), []Layer{LayerFunc(func(LayerContext) LayerHandler { return deliverThenClose{} }), passThroughLayer{}}, func(ev *Events) {
		ev.Handler = FuncHandler{
			Open: func(Conn) {},
			Read: func(c Conn) error {
				if chunk := c.PeekChunk(); len(chunk) > 0 {
					seen <- string(chunk)
					_, _ = c.Discard(len(chunk))
				}
				return nil
			},
			Close: func(_ Conn, err error) { signal(closed, err) },
		}

	})
	ctx, cancel := context.WithTimeout(context.Background(), layerWait)
	defer cancel()
	if _, err := events.DialContext(ctx, "tcp://"+listener.Addr().String()); err != nil {
		t.Fatal(err)
	}
	layerWaitFor(t, closed, "the close")
	select {
	case s := <-seen:
		t.Fatalf("the Handler read %q after the layer below closed", s)
	default:
	}
}

// deliverThenClose hands up what arrives and closes the connection.
type deliverThenClose struct{ PassThrough }

func (deliverThenClose) OnRead(ctx LayerContext) error {
	lower := ctx.Down()
	chunk := lower.PeekChunk()
	if len(chunk) == 0 {
		return nil
	}
	ctx.Up().Write(chunk)
	_, _ = lower.Discard(len(chunk))
	return ctx.Close(io.EOF)
}

// A close as the stack opens — the Handler aborting in its OnOpen — flushes
// nothing more, and a close from a layer as it opens opens nothing above it.
func TestCloseAsTheStackOpens(t *testing.T) {
	t.Run("abort in OnOpen", func(t *testing.T) {
		addr, _ := serveLayered(t, nil, nil)
		holder := &countingHold{}
		closed := make(chan error, 1)
		events := dialLayered(t, addr, []Layer{LayerFunc(func(LayerContext) LayerHandler { return holder })}, func(ev *Events) {
			ev.Handler = FuncHandler{
				Open: func(c Conn) {
					_, _ = c.Write([]byte("never sent"))
					holder.aborting.Store(true)
					_ = c.CloseWith(errors.New("abort"))
				},
				Read:  func(Conn) error { return nil },
				Close: func(_ Conn, err error) { signal(closed, err) },
			}

		})
		ctx, cancel := context.WithTimeout(context.Background(), layerWait)
		defer cancel()
		_, _ = events.DialContext(ctx, "tcp://"+addr)
		layerWaitFor(t, closed, "the close")
		if n := holder.flushedAfter.Load(); n != 0 {
			t.Fatalf("the held output was flushed %d times after the abort", n)
		}
	})
	t.Run("close in a layer's OnOpen", func(t *testing.T) {
		addr, _ := serveLayered(t, nil, nil)
		var opened atomic.Int32
		events := dialLayered(t, addr, []Layer{LayerFunc(func(LayerContext) LayerHandler { return closeThenOpen{} }), LayerFunc(func(LayerContext) LayerHandler { return &countOpens{n: &opened} })}, func(ev *Events) {
			ev.Handler = FuncHandler{Open: func(Conn) {}, Read: func(Conn) error { return nil }, Close: func(Conn, error) {}}

		})
		ctx, cancel := context.WithTimeout(context.Background(), layerWait)
		defer cancel()
		if _, err := events.DialContext(ctx, "tcp://"+addr); err == nil {
			t.Fatal("a dial through a layer that closed as it opened succeeded")
		}
		if n := opened.Load(); n != 0 {
			t.Fatalf("the layer above opened %d times on a closing connection", n)
		}
	})
}

// closeThenOpen closes the connection as it opens, then opens the layers
// above all the same.
type closeThenOpen struct{ PassThrough }

func (closeThenOpen) OnOpen(ctx LayerContext) {
	_ = ctx.Abort(errors.New("refused"))
	ctx.Up().Open()
}

type countOpens struct {
	PassThrough
	n *atomic.Int32
}

func (c *countOpens) OnOpen(ctx LayerContext) {
	c.n.Add(1)
	ctx.Up().Open()
}

// A layer that closes the connection having read only part of what waits
// below is not called again for the rest.
func TestCloseInOnInboundEndsItsCalls(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		c, err := listener.Accept()
		if err == nil {
			_, _ = c.Write([]byte("several bytes"))
			time.Sleep(layerWait)
			c.Close()
		}
	}()
	closer := &byteThenClose{}
	closed := make(chan error, 1)
	events := dialLayered(t, listener.Addr().String(), []Layer{LayerFunc(func(LayerContext) LayerHandler { return closer })}, func(ev *Events) {
		ev.Handler = FuncHandler{Open: func(Conn) {}, Read: func(Conn) error { return nil }, Close: func(_ Conn, err error) { signal(closed, err) }}

	})
	ctx, cancel := context.WithTimeout(context.Background(), layerWait)
	defer cancel()
	if _, err := events.DialContext(ctx, "tcp://"+listener.Addr().String()); err != nil {
		t.Fatal(err)
	}
	layerWaitFor(t, closed, "the close")
	if n := closer.after.Load(); n != 0 {
		t.Fatalf("OnRead ran %d times after the layer closed the connection", n)
	}
}

// byteThenClose reads one byte and closes the connection.
type byteThenClose struct {
	PassThrough
	closed atomic.Bool
	after  atomic.Int32
}

func (b *byteThenClose) OnRead(ctx LayerContext) error {
	if b.closed.Load() {
		b.after.Add(1)
	}
	lower := ctx.Down()
	if len(lower.PeekChunk()) == 0 {
		return nil
	}
	_, _ = lower.Discard(1)
	b.closed.Store(true)
	return ctx.Close(io.EOF)
}

// A reader that always leaves a little unread — a partial header, the start
// of the next message — does not keep what it read alive: the unread rest
// moves to the front as the layer delivers more, so a long connection holds
// what is unread and a delivery's worth, not everything since it began.
func TestPartialReadsDoNotRetainWhatWasRead(t *testing.T) {
	for _, space := range []bool{false, true} {
		t.Run(map[bool]string{false: "Deliver", true: "Space and Commit"}[space], func(t *testing.T) {
			s := &stack{inLimit: 1024}
			l := &layerNode{stack: s}
			l.up = layerUp{l}
			s.layers = []*layerNode{l}
			ctx := layerCtx{l}
			ctx.Up().Write([]byte{1})
			chunk := make([]byte, 8)
			for range 20000 {
				if space {
					copy(ctx.Up().Reserve(len(chunk)), chunk)
					ctx.Up().Commit(len(chunk))
				} else {
					ctx.Up().Write(chunk)
				}
				if _, err := l.Discard(l.held() - 1); err != nil {
					t.Fatal(err)
				}
			}
			if l.held() != 1 {
				t.Fatalf("held %d, want the 1 byte left unread", l.held())
			}
			if len(l.delivered) > 2*len(chunk) || cap(l.delivered) > 64 {
				t.Fatalf("holding %d bytes in %d of capacity for 1 unread", len(l.delivered), cap(l.delivered))
			}
		})
	}
}

// Adopt returns what DialContext returns once layers run: the top layer's
// connection, the Handler's own, whose writes pass the layers and whose
// Userdata is the caller's — not the socket below them.
func TestAdoptReturnsTheTopConnection(t *testing.T) {
	addr, echo := serveLayered(t, nil, nil)
	opened := make(chan Conn, 1)
	events := dialLayered(t, addr, []Layer{LayerFunc(func(LayerContext) LayerHandler { return upperCase{} })}, func(ev *Events) {
		ev.Handler = FuncHandler{Open: func(c Conn) { signal(opened, c) }, Read: func(c Conn) error { _, _ = c.Discard(-1); return nil }, Close: func(Conn, error) {}}

	})
	raw, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := events.Adopt(raw, WithUserdata("adopted"))
	if err != nil {
		t.Fatal(err)
	}
	if handler := layerWaitFor(t, opened, "the Handler's open"); handler != conn {
		t.Fatal("Adopt returned a connection other than the Handler's")
	}
	if got := conn.Userdata(); got != "adopted" {
		t.Fatalf("Userdata = %v, want the caller's", got)
	}
	if _, err := conn.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	if got := string(layerWaitFor(t, echo.read, "the server's read")); got != "HELLO" {
		t.Fatalf("the wire carried %q, want it through the layer", got)
	}

	refusing := dialLayered(t, addr, []Layer{LayerFunc(func(LayerContext) LayerHandler { return refuseLayer{cause: errors.New("refused")} })}, func(ev *Events) {

	})
	raw, err = net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := refusing.Adopt(raw); err == nil || err.Error() != "refused" {
		t.Fatalf("Adopt through a refusing layer = %v, want its refusal", err)
	}
}

// upperCase writes everything upper-cased.
type upperCase struct{ PassThrough }

func (upperCase) OnWrite(ctx LayerContext, p []byte) (int, error) {
	if _, err := ctx.Down().Write(bytes.ToUpper(p)); err != nil {
		return 0, err
	}
	return len(p), nil
}

// An Events with layers and no Handler runs its connections as one without
// layers does: no callbacks, and what arrives discarded, the connection
// open all the while.
func TestLayersWithoutAHandler(t *testing.T) {
	addr, echo := serveLayered(t, nil, nil)
	events := dialLayered(t, addr, []Layer{passThroughLayer{}}, func(ev *Events) {
		ev.Handler = nil

	})
	ctx, cancel := context.WithTimeout(context.Background(), layerWait)
	defer cancel()
	conn, err := events.DialContext(ctx, "tcp://"+addr)
	if err != nil {
		t.Fatal(err)
	}
	for _, message := range []string{"discarded", "still open"} {
		if _, err := conn.Write([]byte(message)); err != nil {
			t.Fatal(err)
		}
		if got := string(layerWaitFor(t, echo.read, "the server's read")); got != message {
			t.Fatalf("the server read %q", got)
		}
		time.Sleep(10 * time.Millisecond) // the echo arrives, and is discarded
	}
}

// A nil *FuncHandler is no Handler under layers too, as without them.
func TestLayersWithANilFuncHandler(t *testing.T) {
	addr, echo := serveLayered(t, nil, nil)
	events := dialLayered(t, addr, []Layer{passThroughLayer{}}, func(ev *Events) {
		ev.Handler = (*FuncHandler)(nil)

	})
	ctx, cancel := context.WithTimeout(context.Background(), layerWait)
	defer cancel()
	conn, err := events.DialContext(ctx, "tcp://"+addr)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write([]byte("no handler")); err != nil {
		t.Fatal(err)
	}
	if got := string(layerWaitFor(t, echo.read, "the server's read")); got != "no handler" {
		t.Fatalf("the server read %q", got)
	}
	time.Sleep(10 * time.Millisecond) // the echo arrives, and is discarded
}

// CloseWrite ends a layered connection's writing half after what was written
// before it, whatever the layers hold: buffered layers flush first, one
// holding output it cannot send yet sends it first, a flush's failure is
// CloseWrite's, and writes after it are refused.
func TestCloseWriteAfterHeldOutput(t *testing.T) {
	type outcome struct {
		read string
		err  error
	}
	peerReads := func(t *testing.T) (string, chan outcome) {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = listener.Close() })
		got := make(chan outcome, 1)
		go func() {
			// A host reusing freed ports may deliver another test's stray
			// connection first; it carries nothing, so the peer reads the
			// next one.
			for {
				peer, err := listener.Accept()
				if err != nil {
					return
				}
				_ = peer.SetReadDeadline(time.Now().Add(layerWait))
				all, err := io.ReadAll(peer)
				_ = peer.Close()
				if len(all) == 0 {
					continue
				}
				got <- outcome{string(all), err}
				return
			}
		}()
		return listener.Addr().String(), got
	}
	holder := func() Layer { return LayerFunc(func(LayerContext) LayerHandler { return &holdUntilFlush{} }) }

	for _, where := range []string{"OnOpen", "OnRead"} {
		for _, layers := range []int{1, 2} {
			t.Run(fmt.Sprintf("%s, %d buffering layers", where, layers), func(t *testing.T) {
				addr, got := peerReads(t)
				results := make(chan error, 2)
				writeAndEnd := func(c Conn) {
					_, err := c.Write([]byte("hello"))
					results <- err
					results <- c.CloseWrite()
				}
				events := dialLayered(t, addr, []Layer{holder()}, func(ev *Events) {
					handler := FuncHandler{Close: func(Conn, error) {}}
					if where == "OnOpen" {
						handler.Open = writeAndEnd
					} else {
						handler.Open = func(c Conn) { _ = c.Wake() }
						handler.Read = func(c Conn) error {
							if c.Userdata() == nil {
								c.SetUserdata(true)
								writeAndEnd(c)
							}
							return nil
						}
					}
					ev.Handler = handler
					for range layers {

					}
				})
				ctx, cancel := context.WithTimeout(context.Background(), layerWait)
				defer cancel()
				if _, err := events.DialContext(ctx, "tcp://"+addr); err != nil {
					t.Fatal(err)
				}
				for range 2 {
					if err := layerWaitFor(t, results, "Write and CloseWrite"); err != nil {
						t.Fatal(err)
					}
				}
				if out := layerWaitFor(t, got, "the peer's read"); out.read != "hello" || out.err != nil {
					t.Fatalf("the peer read %q, %v; want hello, then the end", out.read, out.err)
				}
			})
		}
	}

	t.Run("writes after CloseWrite", func(t *testing.T) {
		addr, _ := peerReads(t)
		events := dialLayered(t, addr, []Layer{holder()}, func(ev *Events) {})
		ctx, cancel := context.WithTimeout(context.Background(), layerWait)
		defer cancel()
		conn, err := events.DialContext(ctx, "tcp://"+addr)
		if err != nil {
			t.Fatal(err)
		}
		if err := conn.CloseWrite(); err != nil {
			t.Fatal(err)
		}
		if _, err := conn.Write([]byte("late")); !errors.Is(err, net.ErrClosed) {
			t.Fatalf("a write after CloseWrite = %v, want net.ErrClosed", err)
		}
		// The peer may have read the end and closed first: the repeat is
		// harmless either way.
		if err := conn.CloseWrite(); err != nil && !errors.Is(err, net.ErrClosed) {
			t.Fatalf("CloseWrite again = %v, want nothing", err)
		}
	})

	t.Run("a flush that fails", func(t *testing.T) {
		addr, _ := peerReads(t)
		failing := &failingFlush{cause: errors.New("flush refused")}
		events := dialLayered(t, addr, []Layer{LayerFunc(func(LayerContext) LayerHandler { return failing })}, func(ev *Events) {

		})
		ctx, cancel := context.WithTimeout(context.Background(), layerWait)
		defer cancel()
		conn, err := events.DialContext(ctx, "tcp://"+addr)
		if err != nil {
			t.Fatal(err)
		}
		failing.armed.Store(true)
		if err := conn.CloseWrite(); !errors.Is(err, failing.cause) {
			t.Fatalf("CloseWrite = %v, want the flush's failure", err)
		}
	})

	t.Run("a holder that sends later", func(t *testing.T) {
		addr, got := peerReads(t)
		window := &sendWindow{}
		events := dialLayered(t, addr, []Layer{LayerFunc(func(LayerContext) LayerHandler { return window })}, func(ev *Events) {

		})
		ctx, cancel := context.WithTimeout(context.Background(), layerWait)
		defer cancel()
		conn, err := events.DialContext(ctx, "tcp://"+addr)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := conn.Write([]byte("hello")); err != nil {
			t.Fatal(err)
		}
		if err := conn.CloseWrite(); err != nil {
			t.Fatal(err)
		}
		select {
		case out := <-got:
			t.Fatalf("the peer read %q, %v before the held bytes went out", out.read, out.err)
		case <-time.After(50 * time.Millisecond):
		}
		window.release()
		if out := layerWaitFor(t, got, "the peer's read"); out.read != "hello" || out.err != nil {
			t.Fatalf("the peer read %q, %v; want hello, then the end", out.read, out.err)
		}
	})
}

// failingFlush is a buffering layer whose Flush fails once armed.
type failingFlush struct {
	PassThrough
	cause error
	armed atomic.Bool
}

func (f *failingFlush) Flush(LayerContext) error {
	if f.armed.Load() {
		return f.cause
	}
	return nil
}

func (f *failingFlush) CloseWrite(ctx LayerContext) error { return ctx.Down().CloseWrite() }

// sendWindow holds what is written until release, as a layer with a closed
// send window does; Flush cannot send it.
type sendWindow struct {
	PassThrough
	mu      sync.Mutex
	ctx     LayerContext
	held    []byte
	counted atomic.Int64
}

func (w *sendWindow) CloseWrite(ctx LayerContext) error { return ctx.Down().CloseWrite() }

func (w *sendWindow) OnOpen(ctx LayerContext) {
	w.mu.Lock()
	w.ctx = ctx
	w.mu.Unlock()
	ctx.Up().Open()
}

func (w *sendWindow) OnWrite(_ LayerContext, p []byte) (int, error) {
	w.mu.Lock()
	w.held = append(w.held, p...)
	w.counted.Add(int64(len(p)))
	w.mu.Unlock()
	return len(p), nil
}

func (w *sendWindow) OutboundHeld(LayerContext) int { return int(w.counted.Load()) }

// release opens the window: the held bytes go below, and a turn sees them
// gone.
func (w *sendWindow) release() {
	w.mu.Lock()
	held, ctx := w.held, w.ctx
	w.held = nil
	w.mu.Unlock()
	_, _ = ctx.Down().Write(held)
	w.counted.Add(-int64(len(held)))
	_ = ctx.Wake()
}

// gatedHold is a holdUntilFlush whose writes stop inside the layer until
// let through.
type gatedHold struct {
	holdUntilFlush
	entered chan struct{}
	proceed chan struct{}
}

func (g *gatedHold) OnWrite(ctx LayerContext, p []byte) (int, error) {
	g.entered <- struct{}{}
	<-g.proceed
	return g.holdUntilFlush.OnWrite(ctx, p)
}

// A CloseWrite beside writes from other goroutines ends the stream after
// every write it did not refuse, and refuses the rest whole.
func TestCloseWriteBesideWrites(t *testing.T) {
	type outcome struct {
		read string
		err  error
	}
	// peerReads reads the connection the test dials, by its address when one
	// is set — a host reusing freed ports may deliver another test's stray
	// first — and by content otherwise.
	peerReads := func(t *testing.T, want *atomic.Pointer[string]) (string, chan outcome) {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = listener.Close() })
		got := make(chan outcome, 1)
		go func() {
			for {
				peer, err := listener.Accept()
				if err != nil {
					return
				}
				_ = peer.SetReadDeadline(time.Now().Add(layerWait))
				all, err := io.ReadAll(peer)
				remote := peer.RemoteAddr().String()
				_ = peer.Close()
				if want != nil {
					if w := want.Load(); w != nil {
						if remote != *w {
							continue // another connection: not the one under test
						}
						got <- outcome{string(all), err}
						return
					}
				}
				if len(all) == 0 {
					continue // a stray: the connection under test has not dialed yet
				}
				got <- outcome{string(all), err}
				return
			}
		}()
		return listener.Addr().String(), got
	}

	t.Run("a write inside the layer", func(t *testing.T) {
		addr, got := peerReads(t, nil)
		gate := &gatedHold{entered: make(chan struct{}, 1), proceed: make(chan struct{})}
		events := dialLayered(t, addr, []Layer{LayerFunc(func(LayerContext) LayerHandler { return gate })}, func(ev *Events) {

		})
		ctx, cancel := context.WithTimeout(context.Background(), layerWait)
		defer cancel()
		conn, err := events.DialContext(ctx, "tcp://"+addr)
		if err != nil {
			t.Fatal(err)
		}
		wrote := make(chan error, 1)
		go func() {
			_, err := conn.Write([]byte("hello"))
			wrote <- err
		}()
		layerWaitFor(t, gate.entered, "the write to enter the layer")
		if err := conn.CloseWrite(); err != nil {
			t.Fatal(err)
		}
		close(gate.proceed)
		if err := layerWaitFor(t, wrote, "the write"); err != nil {
			t.Fatal(err)
		}
		if out := layerWaitFor(t, got, "the peer's read"); out.read != "hello" || out.err != nil {
			t.Fatalf("the peer read %q, %v; want hello, then the end", out.read, out.err)
		}
	})

	t.Run("many writers", func(t *testing.T) {
		for round := range 20 {
			want := &atomic.Pointer[string]{}
			addr, got := peerReads(t, want)
			events := dialLayered(t, addr, []Layer{LayerFunc(func(LayerContext) LayerHandler { return &holdUntilFlush{} })}, func(ev *Events) {

			})
			ctx, cancel := context.WithTimeout(context.Background(), layerWait)
			conn, err := events.DialContext(ctx, "tcp://"+addr)
			cancel()
			if err != nil {
				t.Fatal(err)
			}
			local := conn.LocalAddr().String()
			want.Store(&local)
			const writers, each = 8, 50
			var mu sync.Mutex
			accepted := map[string]bool{}
			var wg sync.WaitGroup
			for w := range writers {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for i := range each {
						message := fmt.Sprintf("<%d.%03d>", w, i)
						_, err := conn.Write([]byte(message))
						if err != nil {
							if !errors.Is(err, net.ErrClosed) {
								t.Errorf("write: %v", err)
							}
							return
						}
						mu.Lock()
						accepted[message] = true
						mu.Unlock()
					}
				}()
			}
			time.Sleep(time.Duration(round%5) * 100 * time.Microsecond)
			if err := conn.CloseWrite(); err != nil {
				t.Fatal(err)
			}
			wg.Wait()
			out := layerWaitFor(t, got, "the peer's read")
			if out.err != nil {
				t.Fatalf("round %d: the peer's read failed: %v", round, out.err)
			}
			received := map[string]bool{}
			for _, part := range strings.SplitAfter(out.read, ">") {
				if part == "" {
					continue
				}
				if !strings.HasPrefix(part, "<") || len(part) != len("<0.000>") {
					t.Fatalf("round %d: the peer read a torn message %q", round, part)
				}
				received[part] = true
			}
			for message := range accepted {
				if !received[message] {
					t.Fatalf("round %d: %s was written, never sent: %d accepted, %d received", round, message, len(accepted), len(received))
				}
			}
			if len(received) != len(accepted) {
				t.Fatalf("round %d: %d received, %d accepted", round, len(received), len(accepted))
			}
		}
	})
}

// windowFlush holds what is written while its send window is shut — Flush
// then sends nothing — and counts the bytes until its write below returns.
type windowFlush struct {
	PassThrough
	mu      sync.Mutex
	open    bool
	held    []byte
	counted atomic.Int64
	flushes atomic.Int32
	// shutFirst holds the first flush whatever the window: the one
	// CloseWrite makes, before OnOpen's own flush sends.
	shutFirst bool
}

func (w *windowFlush) OnWrite(_ LayerContext, p []byte) (int, error) {
	w.mu.Lock()
	w.held = append(w.held, p...)
	w.counted.Add(int64(len(p)))
	w.mu.Unlock()
	return len(p), nil
}

func (w *windowFlush) OutboundHeld(LayerContext) int { return int(w.counted.Load()) }

func (w *windowFlush) CloseWrite(ctx LayerContext) error { return ctx.Down().CloseWrite() }

func (w *windowFlush) Flush(ctx LayerContext) error {
	n := w.flushes.Add(1)
	w.mu.Lock()
	if !w.open && !(w.shutFirst && n > 1) {
		w.mu.Unlock()
		return nil
	}
	held := w.held
	w.held = nil
	w.mu.Unlock()
	if len(held) == 0 {
		return nil
	}
	_, err := ctx.Down().Write(held)
	w.counted.Add(-int64(len(held)))
	return err
}

// A CloseWrite waiting for held output ends the stream once a flush sends
// it — Conn.Flush, or the one closing OnOpen — though the send's report came
// while the holder still counted the bytes.
func TestCloseWriteAfterFlushSendsHeld(t *testing.T) {
	type outcome struct {
		read string
		err  error
	}
	for _, path := range []string{"Conn.Flush", "OnOpen's flush"} {
		t.Run(path, func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			got := make(chan outcome, 1)
			go func() {
				// A host reusing freed ports may deliver another test's stray
				// connection first; it carries nothing, so the peer reads the
				// next one.
				for {
					peer, err := listener.Accept()
					if err != nil {
						return
					}
					_ = peer.SetReadDeadline(time.Now().Add(layerWait))
					all, err := io.ReadAll(peer)
					_ = peer.Close()
					if len(all) == 0 {
						continue
					}
					got <- outcome{string(all), err}
					return
				}
			}()
			window := &windowFlush{shutFirst: path == "OnOpen's flush"}
			results := make(chan error, 2)
			addr := listener.Addr().String()
			events := dialLayered(t, addr, []Layer{LayerFunc(func(LayerContext) LayerHandler { return window })}, func(ev *Events) {
				ev.Handler = FuncHandler{
					Open: func(c Conn) {
						_, err := c.Write([]byte("hello"))
						results <- err
						results <- c.CloseWrite()
					},
					Close: func(Conn, error) {},
				}

			})
			ctx, cancel := context.WithTimeout(context.Background(), layerWait)
			defer cancel()
			conn, err := events.DialContext(ctx, "tcp://"+addr)
			if err != nil {
				t.Fatal(err)
			}
			for range 2 {
				if err := layerWaitFor(t, results, "Write and CloseWrite"); err != nil {
					t.Fatal(err)
				}
			}
			if path == "Conn.Flush" {
				time.Sleep(20 * time.Millisecond) // the opening turns are over
				window.mu.Lock()
				window.open = true
				window.mu.Unlock()
				if err := conn.Flush(); err != nil {
					t.Fatal(err)
				}
			}
			if out := layerWaitFor(t, got, "the peer's read"); out.read != "hello" || out.err != nil {
				t.Fatalf("the peer read %q, %v; want hello, then the end", out.read, out.err)
			}
		})
	}
}

// endedLayer counts the calls it gets once its OnClose ran.
type endedLayer struct {
	PassThrough
	ended atomic.Bool
	late  atomic.Int32
}

func (l *endedLayer) lateCall() {
	if l.ended.Load() {
		l.late.Add(1)
	}
}

func (l *endedLayer) OnWrite(ctx LayerContext, p []byte) (int, error) {
	l.lateCall()
	return ctx.Down().Write(p)
}
func (l *endedLayer) OutboundHeld(LayerContext) int { l.lateCall(); return 0 }
func (l *endedLayer) Flush(LayerContext) error      { l.lateCall(); return nil }
func (l *endedLayer) CloseWrite(ctx LayerContext) error {
	l.lateCall()
	return ctx.Down().CloseWrite()
}
func (l *endedLayer) CloseGracefully(LayerContext, error) { l.lateCall() }
func (l *endedLayer) OnClose(LayerContext, error)         { l.ended.Store(true) }

// Once a connection ended, its writes, flushes and closes are refused
// without reaching the layers, which OnClose may have torn down.
func TestNoLayerCallsAfterOnClose(t *testing.T) {
	for _, end := range []string{"Events.Close", "the peer"} {
		t.Run(end, func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			peers := make(chan net.Conn, 1)
			go func() {
				if peer, err := listener.Accept(); err == nil {
					peers <- peer
				}
			}()
			layers := []*endedLayer{{}, {}}
			closed := make(chan struct{})
			addr := listener.Addr().String()
			chain := make([]Layer, len(layers))
			for i, ended := range layers {
				chain[i] = LayerFunc(func(LayerContext) LayerHandler { return ended })
			}
			events := dialLayered(t, addr, chain, func(ev *Events) {
				ev.Handler = FuncHandler{Close: func(Conn, error) { close(closed) }}
			})
			ctx, cancel := context.WithTimeout(context.Background(), layerWait)
			defer cancel()
			conn, err := events.DialContext(ctx, "tcp://"+addr)
			if err != nil {
				t.Fatal(err)
			}
			peer := layerWaitFor(t, peers, "the peer")
			defer peer.Close()
			if end == "the peer" {
				_ = peer.Close()
			} else {
				_ = events.Close(nil)
			}
			layerWaitFor(t, closed, "OnClose")
			for _, layer := range layers {
				for !layer.ended.Load() {
					time.Sleep(time.Millisecond)
				}
			}
			calls := map[string]error{}
			_, calls["Write"] = conn.Write([]byte("late"))
			_, calls["Writev"] = conn.Writev([][]byte{[]byte("la"), []byte("te")})
			calls["Flush"] = conn.Flush()
			calls["CloseWrite"] = conn.CloseWrite()
			calls["CloseWith"] = conn.CloseWith(errors.New("late"))
			calls["Close"] = conn.Close()
			_ = conn.OutboundBuffered()
			_ = conn.(WireCounter).WireQueued()
			for name, err := range calls {
				if !errors.Is(err, net.ErrClosed) {
					t.Errorf("%s after OnClose = %v, want net.ErrClosed", name, err)
				}
			}
			for i, layer := range layers {
				if n := layer.late.Load(); n != 0 {
					t.Errorf("layer %d got %d calls after its OnClose", i, n)
				}
			}
		})
	}
}

// reentryGuard is a layer's lock that counts a call reaching the layer on a
// goroutine already inside it — UIO calling the layer back from below —
// where a plain mutex would deadlock the test.
type reentryGuard struct {
	mu        sync.Mutex
	owner     atomic.Uint64
	reentered atomic.Int32
}

func goroutineID() uint64 {
	var buf [32]byte
	field := strings.Fields(string(buf[:runtime.Stack(buf[:], false)]))[1]
	var id uint64
	fmt.Sscan(field, &id)
	return id
}

// enter takes the lock, or counts a re-entry and returns false.
func (g *reentryGuard) enter() bool {
	id := goroutineID()
	if g.owner.Load() == id {
		g.reentered.Add(1)
		return false
	}
	g.mu.Lock()
	g.owner.Store(id)
	return true
}

func (g *reentryGuard) exit(entered bool) {
	if entered {
		g.owner.Store(0)
		g.mu.Unlock()
	}
}

// guardedWindow holds what is written while its window is shut and, like a
// protocol serializing its operations, keeps its lock through Flush — the
// write below and the flush below included — and CloseWrite.
type guardedWindow struct {
	PassThrough
	guard   reentryGuard
	dataMu  sync.Mutex
	open    bool
	held    []byte
	counted atomic.Int64
}

func (w *guardedWindow) OnWrite(_ LayerContext, p []byte) (int, error) {
	w.dataMu.Lock()
	w.held = append(w.held, p...)
	w.counted.Add(int64(len(p)))
	w.dataMu.Unlock()
	return len(p), nil
}

func (w *guardedWindow) OutboundHeld(LayerContext) int { return int(w.counted.Load()) }

func (w *guardedWindow) Flush(ctx LayerContext) error {
	entered := w.guard.enter()
	defer w.guard.exit(entered)
	w.dataMu.Lock()
	if !w.open {
		w.dataMu.Unlock()
		return nil
	}
	held := w.held
	w.held = nil
	w.dataMu.Unlock()
	if len(held) > 0 {
		_, err := ctx.Down().Write(held)
		w.counted.Add(-int64(len(held)))
		if err != nil {
			return err
		}
	}
	return ctx.Down().Flush()
}

func (w *guardedWindow) CloseWrite(ctx LayerContext) error {
	entered := w.guard.enter()
	defer w.guard.exit(entered)
	return ctx.Down().CloseWrite()
}

// lockedWriter keeps its lock while it writes below, as crypto/tls keeps its
// record lock.
type lockedWriter struct {
	PassThrough
	guard reentryGuard
}

func (w *lockedWriter) OnWrite(ctx LayerContext, p []byte) (int, error) {
	entered := w.guard.enter()
	defer w.guard.exit(entered)
	return ctx.Down().Write(p)
}

// sentWriter writes once, from OnWireSent, after its first send.
type sentWriter struct {
	PassThrough
	once sync.Once
}

func (s *sentWriter) OnWireSent(ctx LayerContext, _ int) {
	s.once.Do(func() { _, _ = ctx.Down().Write([]byte("!")) })
}

// selfCloser holds output to flush, takes its lock to flush it, and closes
// the connection gracefully, under that lock, as it reads "bye".
type selfCloser struct {
	PassThrough
	guard reentryGuard
}

func (s *selfCloser) Flush(LayerContext) error {
	entered := s.guard.enter()
	s.guard.exit(entered)
	return nil
}

func (s *selfCloser) OnRead(ctx LayerContext) error {
	if !bytes.Contains(ctx.Down().PeekChunk(), []byte("bye")) {
		return s.PassThrough.OnRead(ctx)
	}
	entered := s.guard.enter()
	defer s.guard.exit(entered)
	_, _ = ctx.Down().Discard(-1)
	_ = ctx.Close(nil)
	return nil
}

// UIO calls a layer only on a turn or from above it: never from inside a
// call the layer is making below, where it may hold its lock.
func TestNoLayerCalledBackFromBelow(t *testing.T) {
	type outcome struct {
		read string
		err  error
	}
	peer := func(t *testing.T, say string) (string, chan outcome) {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = listener.Close() })
		got := make(chan outcome, 1)
		go func() {
			peer, err := listener.Accept()
			if err != nil {
				return
			}
			defer peer.Close()
			if say != "" {
				_, _ = peer.Write([]byte(say))
			}
			_ = peer.SetReadDeadline(time.Now().Add(layerWait))
			all, err := io.ReadAll(peer)
			got <- outcome{string(all), err}
		}()
		return listener.Addr().String(), got
	}
	dial := func(t *testing.T, addr string, layers []Layer, setup func(*Events)) Conn {
		events := dialLayered(t, addr, layers, setup)
		ctx, cancel := context.WithTimeout(context.Background(), layerWait)
		defer cancel()
		conn, err := events.DialContext(ctx, "tcp://"+addr)
		if err != nil {
			t.Fatal(err)
		}
		return conn
	}

	t.Run("a flush below ends a CloseWrite above", func(t *testing.T) {
		addr, got := peer(t, "")
		window := &guardedWindow{}
		ended := make(chan error, 1)
		conn := dial(t, addr, []Layer{passThroughLayer{}, LayerFunc(func(LayerContext) LayerHandler { return window })}, func(ev *Events) {
			ev.Handler = FuncHandler{
				Open: func(c Conn) {
					_, err := c.Write([]byte("hello"))
					if err == nil {
						err = c.CloseWrite() // the window is shut: the end waits
					}
					ended <- err
				},
				Close: func(Conn, error) {},
			}
		})
		if err := layerWaitFor(t, ended, "Write and CloseWrite"); err != nil {
			t.Fatal(err)
		}
		window.dataMu.Lock()
		window.open = true
		window.dataMu.Unlock()
		// Opening the window lets a turn flush it too; the explicit flush
		// is the one under test when the connection is still open.
		if err := conn.Flush(); err != nil && !errors.Is(err, net.ErrClosed) {
			t.Fatal(err)
		}
		if out := layerWaitFor(t, got, "the peer's read"); out.read != "hello" || out.err != nil {
			t.Fatalf("the peer read %q, %v; want hello, then the end", out.read, out.err)
		}
		if n := window.guard.reentered.Load(); n != 0 {
			t.Fatalf("the layer was called back %d times inside its own Flush", n)
		}
	})

	t.Run("a send report inside a locked write", func(t *testing.T) {
		addr, got := peer(t, "")
		locked := &lockedWriter{}
		wrote := make(chan error, 1)
		conn := dial(t, addr, []Layer{LayerFunc(func(LayerContext) LayerHandler { return locked }),
			LayerFunc(func(LayerContext) LayerHandler { return &sentWriter{} })}, func(ev *Events) {
			ev.Handler = FuncHandler{
				// On a turn, where the socket sends at once and reports
				// inside the write.
				Open: func(c Conn) {
					_, err := c.Write([]byte("hi"))
					wrote <- err
				},
				Close: func(Conn, error) {},
			}
		})
		if err := layerWaitFor(t, wrote, "the write"); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(layerWait)
		for conn.(WireCounter).WireQueued() < 3 && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		_ = conn.Close()
		if out := layerWaitFor(t, got, "the peer's read"); out.read != "hi!" {
			t.Fatalf("the peer read %q, %v; want hi!", out.read, out.err)
		}
		if n := locked.guard.reentered.Load(); n != 0 {
			t.Fatalf("OnWireSent wrote into the layer %d times inside its own write", n)
		}
	})

	t.Run("a layer closing under its lock", func(t *testing.T) {
		addr, got := peer(t, "bye")
		closer := &selfCloser{}
		dial(t, addr, []Layer{LayerFunc(func(LayerContext) LayerHandler { return closer }), passThroughLayer{}}, nil)
		if out := layerWaitFor(t, got, "the peer's read"); out.err != nil {
			t.Fatalf("the peer's read failed: %v; want the end", out.err)
		}
		if n := closer.guard.reentered.Load(); n != 0 {
			t.Fatalf("the layer's close flushed it %d times inside its own callback", n)
		}
	})
}

// sentCounter counts OnWireSent calls, and those made once the stack was
// closing.
type sentCounter struct {
	PassThrough
	calls, closing atomic.Int32
}

func (s *sentCounter) OnWireSent(ctx LayerContext, _ int) {
	s.calls.Add(1)
	if ctx.(layerCtx).l.stack.over() {
		s.closing.Add(1)
	}
}

// writeOnSent writes once from OnWireSent, into a layer below that holds it
// until a flush.
type writeOnSent struct {
	PassThrough
	armed atomic.Bool
}

func (w *writeOnSent) OnWireSent(ctx LayerContext, _ int) {
	if w.armed.CompareAndSwap(true, false) {
		_, _ = ctx.Down().Write([]byte("X"))
	}
}

// closesOnQ closes the connection gracefully, itself, as it reads 'q'.
type closesOnQ struct{ PassThrough }

func (closesOnQ) OnRead(ctx LayerContext) error {
	if bytes.Contains(ctx.Down().PeekChunk(), []byte("q")) {
		_, _ = ctx.Down().Discard(-1)
		return ctx.Close(nil)
	}
	return PassThrough{}.OnRead(ctx)
}

// What waits for a turn — send reports, a close's linger — keeps to the
// layers' contract: nothing reaches them once the connection is closing,
// what OnWireSent writes is flushed, and a layer closing the connection itself
// waits only for what lies below it.
func TestTurnWorkKeepsTheContract(t *testing.T) {
	type outcome struct {
		read string
		err  error
		at   time.Time
	}
	peer := func(t *testing.T, say string, readDelay time.Duration) (string, chan outcome) {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = listener.Close() })
		got := make(chan outcome, 1)
		go func() {
			peer, err := listener.Accept()
			if err != nil {
				return
			}
			defer peer.Close()
			if say != "" {
				_, _ = peer.Write([]byte(say))
			}
			time.Sleep(readDelay)
			_ = peer.SetReadDeadline(time.Now().Add(layerWait))
			all, err := io.ReadAll(peer)
			got <- outcome{string(all), err, time.Now()}
		}()
		return listener.Addr().String(), got
	}
	dial := func(t *testing.T, addr string, layers []Layer, setup func(*Events)) Conn {
		events := dialLayered(t, addr, layers, setup)
		ctx, cancel := context.WithTimeout(context.Background(), layerWait)
		defer cancel()
		conn, err := events.DialContext(ctx, "tcp://"+addr)
		if err != nil {
			t.Fatal(err)
		}
		return conn
	}

	t.Run("no OnWireSent while closing", func(t *testing.T) {
		const size = 8 << 20
		addr, got := peer(t, "", 100*time.Millisecond)
		counter := &sentCounter{}
		conn := dial(t, addr, []Layer{LayerFunc(func(LayerContext) LayerHandler { return counter })}, nil)
		if _, err := conn.Write(make([]byte, size)); err != nil {
			t.Fatal(err)
		}
		_ = conn.Close() // lingers while the peer is slow to read
		if out := layerWaitFor(t, got, "the peer's read"); len(out.read) != size {
			t.Fatalf("the peer read %d bytes, %v; want %d", len(out.read), out.err, size)
		}
		if n := counter.closing.Load(); n != 0 {
			t.Fatalf("OnWireSent ran %d times on a closing connection", n)
		}
	})

	t.Run("what OnWireSent writes is flushed", func(t *testing.T) {
		addr, got := peer(t, "", 0)
		writer := &writeOnSent{}
		writer.armed.Store(true)
		conn := dial(t, addr, []Layer{LayerFunc(func(LayerContext) LayerHandler { return &holdUntilFlush{} }),
			LayerFunc(func(LayerContext) LayerHandler { return writer })}, nil)
		if _, err := conn.Write([]byte("data")); err != nil {
			t.Fatal(err)
		}
		_ = conn.Wake() // a turn flushes what the write left held
		deadline := time.Now().Add(layerWait)
		for conn.(WireCounter).WireSent() < 5 && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		_ = conn.Close()
		if out := layerWaitFor(t, got, "the peer's read"); out.read != "dataX" {
			t.Fatalf("the peer read %q, %v; want dataX", out.read, out.err)
		}
	})

	t.Run("a layer closing itself", func(t *testing.T) {
		saved := lingerTimeout
		lingerTimeout = 2 * time.Second
		t.Cleanup(func() { lingerTimeout = saved })
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = listener.Close() })
		quit := make(chan struct{})
		got := make(chan outcome, 1)
		go func() {
			peer, err := listener.Accept()
			if err != nil {
				return
			}
			defer peer.Close()
			<-quit
			_, _ = peer.Write([]byte("q"))
			_ = peer.SetReadDeadline(time.Now().Add(layerWait))
			all, err := io.ReadAll(peer)
			got <- outcome{string(all), err, time.Now()}
		}()
		addr := listener.Addr().String()
		conn := dial(t, addr, []Layer{LayerFunc(func(LayerContext) LayerHandler { return closesOnQ{} }),
			LayerFunc(func(LayerContext) LayerHandler { return &holdUntilFlush{} })}, nil)
		// Held above the layer that closes: dropped with the close, which
		// waits for nothing it cannot send.
		if _, err := conn.Write([]byte("held above")); err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		close(quit)
		out := layerWaitFor(t, got, "the peer's read")
		if out.err != nil {
			t.Fatalf("the peer's read failed: %v", out.err)
		}
		if waited := out.at.Sub(start); waited > lingerTimeout/2 {
			t.Fatalf("the close took %v, waiting for output above the layer that closed", waited)
		}
	})
}

// closingProbe counts the calls it gets once its stack is closing.
type closingProbe struct {
	PassThrough
	late atomic.Int32
}

func (p *closingProbe) lateCall(ctx LayerContext) {
	if ctx.(layerCtx).l.stack.over() {
		p.late.Add(1)
	}
}

func (p *closingProbe) OnWireSent(ctx LayerContext, _ int) { p.lateCall(ctx) }

// Flush is not counted: the goodbye walk flushes the layers below the close's
// initiator while the stack is closing, and that flush is the close itself.
func (p *closingProbe) Flush(LayerContext) error       { return nil }
func (p *closingProbe) OnTimer(ctx LayerContext) error { p.lateCall(ctx); return nil }
func (p *closingProbe) CloseWrite(ctx LayerContext) error {
	p.lateCall(ctx)
	return ctx.Down().CloseWrite()
}
func (p *closingProbe) OnWrite(ctx LayerContext, b []byte) (int, error) {
	p.lateCall(ctx)
	return ctx.Down().Write(b)
}

// closesFromSent closes the connection gracefully from its first OnWireSent.
type closesFromSent struct {
	PassThrough
	once sync.Once
}

func (c *closesFromSent) OnWireSent(ctx LayerContext, _ int) {
	c.once.Do(func() { _ = ctx.Close(nil) })
}

// closesFromFlush closes the connection gracefully from a flush once armed.
type closesFromFlush struct {
	PassThrough
	armed atomic.Bool
}

func (c *closesFromFlush) Flush(ctx LayerContext) error {
	if c.armed.CompareAndSwap(true, false) {
		_ = ctx.Close(nil)
	}
	return nil
}

// A layer that closes the connection from OnWireSent or Flush is the last layer
// called: the rest hear nothing more but OnClose, and writes from above are
// refused.
func TestCloseFromALayerCallbackStopsTheRest(t *testing.T) {
	run := func(t *testing.T, layers func(*closingProbe) []Layer, arm func()) {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer listener.Close()
		go func() {
			if peer, err := listener.Accept(); err == nil {
				defer peer.Close()
				_, _ = io.Copy(io.Discard, peer)
			}
		}()
		below, above := &closingProbe{}, &closingProbe{}
		ended := make(chan struct{})
		late := make(chan error, 1)
		addr := listener.Addr().String()
		events := dialLayered(t, addr, append([]Layer{LayerFunc(func(LayerContext) LayerHandler { return below })}, layers(above)...), func(ev *Events) {

			ev.Handler = FuncHandler{
				Open: func(c Conn) {
					arm()
					_, _ = c.Write([]byte("hello"))
				},
				Read: func(c Conn) error {
					_, err := c.Write([]byte("late"))
					select {
					case late <- err:
					default:
					}
					return nil
				},
				Close: func(Conn, error) { close(ended) },
			}
		})
		ctx, cancel := context.WithTimeout(context.Background(), layerWait)
		defer cancel()
		conn, err := events.DialContext(ctx, "tcp://"+addr)
		if err != nil {
			t.Fatal(err)
		}
		layerWaitFor(t, ended, "the close")
		if _, err := conn.Write([]byte("after")); !errors.Is(err, net.ErrClosed) {
			t.Fatalf("a write after the close = %v, want net.ErrClosed", err)
		}
		for name, probe := range map[string]*closingProbe{"below": below, "above": above} {
			if n := probe.late.Load(); n != 0 {
				t.Errorf("the layer %s the closer got %d calls once it was closing", name, n)
			}
		}
	}
	t.Run("from OnWireSent", func(t *testing.T) {
		run(t, func(above *closingProbe) []Layer {
			return []Layer{
				LayerFunc(func(LayerContext) LayerHandler { return &closesFromSent{} }),
				LayerFunc(func(LayerContext) LayerHandler { return above }),
			}
		}, func() {})
	})
	t.Run("from Flush", func(t *testing.T) {
		closer := &closesFromFlush{}
		run(t, func(above *closingProbe) []Layer {
			return []Layer{
				LayerFunc(func(LayerContext) LayerHandler { return closer }),
				LayerFunc(func(LayerContext) LayerHandler { return above }),
			}
		}, func() { closer.armed.Store(true) })
	})
}

// peerAnswersGoodbye closes the socket as it says its goodbye, as a peer
// that answers a close_notify with its own close can before the closing
// call reaches the socket.
type peerAnswersGoodbye struct{ PassThrough }

func (peerAnswersGoodbye) CloseGracefully(ctx LayerContext, _ error) {
	_ = ctx.(layerCtx).l.stack.socket.CloseWith(io.EOF)
}

// A close whose goodbye made the peer close first still succeeds: the
// connection closed as asked.
func TestCloseSucceedsWhenTheGoodbyeIsAnswered(t *testing.T) {
	addr, _ := serveLayered(t, nil, nil)
	events := dialLayered(t, addr, []Layer{LayerFunc(func(LayerContext) LayerHandler { return peerAnswersGoodbye{} })}, func(ev *Events) {

	})
	ctx, cancel := context.WithTimeout(context.Background(), layerWait)
	defer cancel()
	conn, err := events.DialContext(ctx, "tcp://"+addr)
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.Close(); err != nil {
		t.Fatalf("Close = %v, want nothing: the connection closed as asked", err)
	}
	if err := conn.Close(); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("a second Close = %v, want net.ErrClosed", err)
	}
}

// splitWriter writes each write below in two pieces, holding nothing once
// OnWrite returns — a framing layer writing a header and a body — and
// stops between them until let through.
type splitWriter struct {
	PassThrough
	entered chan struct{}
	proceed chan struct{}
}

func (w *splitWriter) CloseWrite(ctx LayerContext) error { return ctx.Down().CloseWrite() }

func (s *splitWriter) OnWrite(ctx LayerContext, p []byte) (int, error) {
	n, err := ctx.Down().Write(p[:2])
	if err != nil {
		return n, err
	}
	s.entered <- struct{}{}
	<-s.proceed
	m, err := ctx.Down().Write(p[2:])
	return n + m, err
}

// closesInShutFlush closes the connection from the flush CloseWrite makes.
type closesInShutFlush struct {
	closingProbe
	armed atomic.Bool
}

func (c *closesInShutFlush) Flush(ctx LayerContext) error {
	if c.armed.CompareAndSwap(true, false) {
		return ctx.Close(nil)
	}
	c.lateCall(ctx)
	return nil
}

func (c *closesInShutFlush) OutboundHeld(ctx LayerContext) int { c.lateCall(ctx); return 0 }

// A CloseWrite waits for a write already inside a layer that holds nothing
// — one writing below in pieces — and stops at a close its own flush made.
func TestCloseWriteInFlightAndClosing(t *testing.T) {
	t.Run("a write in pieces", func(t *testing.T) {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer listener.Close()
		type outcome struct {
			read string
			err  error
		}
		got := make(chan outcome, 1)
		go func() {
			peer, err := listener.Accept()
			if err != nil {
				return
			}
			defer peer.Close()
			_ = peer.SetReadDeadline(time.Now().Add(layerWait))
			all, err := io.ReadAll(peer)
			got <- outcome{string(all), err}
		}()
		split := &splitWriter{entered: make(chan struct{}, 1), proceed: make(chan struct{})}
		addr := listener.Addr().String()
		events := dialLayered(t, addr, []Layer{LayerFunc(func(LayerContext) LayerHandler { return split })}, func(ev *Events) {

		})
		ctx, cancel := context.WithTimeout(context.Background(), layerWait)
		defer cancel()
		conn, err := events.DialContext(ctx, "tcp://"+addr)
		if err != nil {
			t.Fatal(err)
		}
		wrote := make(chan error, 1)
		go func() {
			n, err := conn.Write([]byte("hello"))
			if err == nil && n != 5 {
				err = io.ErrShortWrite
			}
			wrote <- err
		}()
		layerWaitFor(t, split.entered, "the write between its pieces")
		if err := conn.CloseWrite(); err != nil {
			t.Fatal(err)
		}
		close(split.proceed)
		if err := layerWaitFor(t, wrote, "the write"); err != nil {
			t.Fatalf("the write already inside the layer = %v, want it whole", err)
		}
		if out := layerWaitFor(t, got, "the peer's read"); out.read != "hello" || out.err != nil {
			t.Fatalf("the peer read %q, %v; want hello, then the end", out.read, out.err)
		}
	})

	t.Run("a close from the shut's flush", func(t *testing.T) {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer listener.Close()
		go func() {
			if peer, err := listener.Accept(); err == nil {
				defer peer.Close()
				_, _ = io.Copy(io.Discard, peer)
			}
		}()
		layer := &closesInShutFlush{}
		ended := make(chan error, 1)
		closed := make(chan struct{})
		addr := listener.Addr().String()
		events := dialLayered(t, addr, []Layer{LayerFunc(func(LayerContext) LayerHandler { return layer })}, func(ev *Events) {

			ev.Handler = FuncHandler{
				Open: func(c Conn) {
					layer.armed.Store(true)
					ended <- c.CloseWrite()
				},
				Close: func(Conn, error) { close(closed) },
			}
		})
		ctx, cancel := context.WithTimeout(context.Background(), layerWait)
		defer cancel()
		// The flush closes the stack inside the Handler's OnOpen: the dial
		// reports the close rather than a settled connection already closing.
		if _, err := events.DialContext(ctx, "tcp://"+addr); err != nil && !errors.Is(err, net.ErrClosed) {
			t.Fatal(err)
		}
		layerWaitFor(t, ended, "CloseWrite")
		layerWaitFor(t, closed, "the close")
		if n := layer.late.Load(); n != 0 {
			t.Fatalf("the layer got %d calls once its flush closed the connection", n)
		}
	})
}

// wakeCounter is a socket that counts the turns asked of it.
type wakeCounter struct {
	Conn
	wakes atomic.Int32
}

func (w *wakeCounter) Wake() error { w.wakes.Add(1); return nil }

// The turns a stack asks for itself — for a send report, a waiting end —
// are one until that turn begins, however many ask.
func TestRequestedTurnsCoalesce(t *testing.T) {
	socket := &wakeCounter{}
	c := &stack{socket: socket}
	for range 1000 {
		c.requestTurn()
	}
	if n := socket.wakes.Load(); n != 1 {
		t.Fatalf("1000 requests before the turn asked %d wakes, want 1", n)
	}
	_ = c.turn() // the asked-for turn begins
	c.requestTurn()
	if n := socket.wakes.Load(); n != 2 {
		t.Fatalf("a request after the turn began asked %d wakes in all, want 2", n)
	}
}

// blockedGoodbye says its goodbye only when the test lets it, and reports
// that it began.
type blockedGoodbye struct {
	PassThrough
	started chan struct{}
	release chan struct{}
}

func (g *blockedGoodbye) CloseGracefully(LayerContext, error) {
	close(g.started)
	<-g.release
}

// Conn.CloseWith requests the close and returns: the layers say their
// goodbyes on the connection's next turn, never inside the caller — a
// goodbye that blocks holds up the turn it runs on, not the CloseWith that
// asked for it. The blocked goodbye is the assertion: the call returned with
// the goodbye still held, so a CloseWith that ran it inline would never have
// returned.
func TestHandlerCloseRequestsAndReturns(t *testing.T) {
	goodbye := &blockedGoodbye{started: make(chan struct{}), release: make(chan struct{})}
	release := sync.OnceFunc(func() { close(goodbye.release) })
	// A failure before the end must not leave the goodbye blocked: Serve
	// joins the turn it holds.
	t.Cleanup(release)
	opened := make(chan Conn, 1)
	closed := make(chan error, 1)
	addr, _ := serveLayered(t, []Layer{LayerFunc(func(LayerContext) LayerHandler { return goodbye })}, func(ev *Events) {
		ev.Handler = FuncHandler{
			Open:  func(c Conn) { signal(opened, c) },
			Read:  func(c Conn) error { _, _ = c.Discard(c.InboundBuffered()); return nil },
			Close: func(_ Conn, err error) { signal(closed, err) },
		}
	})
	client := dialLayered(t, addr, nil, func(ev *Events) {})
	ctx, cancel := context.WithTimeout(context.Background(), layerWait)
	defer cancel()
	if _, err := client.DialContext(ctx, "tcp://"+addr); err != nil {
		t.Fatal(err)
	}
	var server Conn
	select {
	case server = <-opened:
	case <-time.After(layerWait):
		t.Fatal("the server's handler did not open")
	}
	if err := server.CloseWith(nil); err != nil {
		t.Fatalf("CloseWith = %v, want the request taken", err)
	}
	select {
	case <-goodbye.started:
	case <-time.After(layerWait):
		buf := make([]byte, 1<<20)
		buf = buf[:runtime.Stack(buf, true)]
		t.Fatalf("no the goodbye the turn runs within %v; stacks:\n%s", layerWait, buf)
	}
	release()
	if err := layerWaitFor(t, closed, "the close"); err != nil {
		t.Fatalf("closed with %v, want nil", err)
	}
}

// flushFails is a layer whose Flush always fails and whose goodbye records
// the cause it was told.
type flushFails struct {
	PassThrough
	err     error
	goodbye chan error
}

func (f *flushFails) Flush(LayerContext) error { return f.err }

func (f *flushFails) CloseGracefully(_ LayerContext, cause error) {
	select {
	case f.goodbye <- cause:
	default:
	}
}

// A layer flush that fails as a turn ends aborts the connection: the close
// runs the goodbyes with the flush's cause, rather than the core closing the
// socket with the layers never told.
func TestTurnEndFlushErrorAbortsThroughTheStack(t *testing.T) {
	flushErr := errors.New("the flush failed")
	goodbye := make(chan error, 1)
	closed := make(chan error, 1)
	addr, _ := serveLayered(t, []Layer{LayerFunc(func(LayerContext) LayerHandler {
		return &flushFails{err: flushErr, goodbye: goodbye}
	})}, func(ev *Events) {
		ev.Handler = FuncHandler{
			Open: func(Conn) {},
			Read: func(c Conn) error {
				_, _ = c.Discard(c.InboundBuffered())
				_, err := c.Write([]byte("x"))
				return err
			},
			Close: func(_ Conn, err error) { signal(closed, err) },
		}
	})
	client := dialLayered(t, addr, nil, func(ev *Events) {})
	ctx, cancel := context.WithTimeout(context.Background(), layerWait)
	defer cancel()
	if _, err := client.DialContext(ctx, "tcp://"+addr); err != nil {
		t.Fatal(err)
	}
	if cause := layerWaitFor(t, goodbye, "the goodbye after the flush failed"); !errors.Is(cause, flushErr) {
		t.Fatalf("the goodbye was told %v, want the flush's cause", cause)
	}
	if cause := layerWaitFor(t, closed, "the close"); !errors.Is(cause, flushErr) {
		t.Fatalf("the handler's close was told %v, want the flush's cause", cause)
	}
}

// endAfterRead ends the stream above on the turn it delivers the bytes that
// followed it, as a TLS close_notify does: the peer's end is announced in
// band, not as the socket's FIN.
type endAfterRead struct{ PassThrough }

func (e endAfterRead) OnRead(ctx LayerContext) error {
	err := e.PassThrough.OnRead(ctx)
	ctx.Up().End()
	return err
}

// A stream's own end closes gracefully: what the Handler wrote in the turn
// that carried the end still leaves, whole, instead of the socket closing on
// the queue.
func TestPeerEndDrainsWhatWasQueued(t *testing.T) {
	const payloadSize = 8 << 20
	payload := bytes.Repeat([]byte{'e'}, payloadSize)
	var wrote atomic.Bool
	addr, _ := serveLayered(t, []Layer{LayerFunc(func(LayerContext) LayerHandler { return endAfterRead{} })}, func(ev *Events) {
		ev.Handler = FuncHandler{
			Open: func(Conn) {},
			Read: func(c Conn) error {
				_, _ = c.Discard(c.InboundBuffered())
				if !wrote.CompareAndSwap(false, true) {
					return nil
				}
				_, err := c.Write(payload)
				return err
			},
			Close: func(Conn, error) {},
		}
	})
	peer, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	if _, err := peer.Write([]byte("go")); err != nil {
		t.Fatal(err)
	}
	if err := peer.SetReadDeadline(time.Now().Add(layerWait)); err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(peer)
	if err != nil {
		t.Fatalf("read past the stream's end: %v", err)
	}
	if len(got) != payloadSize {
		t.Fatalf("the peer got %d of %d bytes, want the queue drained", len(got), payloadSize)
	}
	if !bytes.Equal(got, payload[:len(got)]) {
		t.Fatal("the payload the peer got differs from what was written")
	}
}

// One Dialer serves concurrent dials: its defaults are read only, each call
// applies its options to a copy of them, and the chain entry the defaults
// resolved is shared read-only — the per-call WithLayers builds its own.
func TestDialerConcurrentDials(t *testing.T) {
	addr, _ := serveLayered(t, []Layer{PassThroughLayer}, func(ev *Events) {
		ev.Handler = FuncHandler{
			Open: func(Conn) {},
			Read: func(c Conn) error {
				payload := c.PeekChunk()
				_, _ = c.Discard(len(payload))
				_, err := c.Write(payload)
				return err
			},
			Close: func(Conn, error) {},
		}
	})
	client := dialLayered(t, addr, nil, func(ev *Events) {})
	dialer, err := client.Dialer(WithLayers(PassThroughLayer))
	if err != nil {
		t.Fatal(err)
	}
	const workers, dials = 16, 8
	var failures atomic.Int32
	var wg sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := 0; i < dials; i++ {
				ctx, cancel := context.WithTimeout(context.Background(), layerWait)
				var opts []DialOption // half take the Dialer's chain, half their own
				if worker%2 == 1 {
					opts = append(opts, WithLayers(PassThroughLayer))
				}
				conn, err := dialer.DialContext(ctx, "tcp://"+addr, opts...)
				if err == nil {
					_, err = conn.Write([]byte{'x'})
					if closeErr := conn.CloseWith(nil); err == nil {
						err = closeErr
					}
				}
				cancel()
				if err != nil {
					failures.Add(1)
				}
			}
		}(worker)
	}
	wg.Wait()
	if n := failures.Load(); n != 0 {
		t.Fatalf("%d of %d concurrent dials failed", n, workers*dials)
	}
}

// stashedClose keeps its LayerContext for the test and counts its goodbyes.
type stashedClose struct {
	PassThrough
	ctx     chan LayerContext
	goodbye atomic.Int32
}

func (l *stashedClose) OnOpen(ctx LayerContext) {
	l.ctx <- ctx
	ctx.Up().Open()
}

func (l *stashedClose) CloseGracefully(LayerContext, error) { l.goodbye.Add(1) }

// A close from above that finds a walk already pending owns the end: the
// socket closes with its cause, and the pending walk is dropped — its
// goodbyes do not run after the socket began closing.
func TestCloseCutDropsThePendingWalk(t *testing.T) {
	layer := &stashedClose{ctx: make(chan LayerContext, 1)}
	blocked := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	server := make(chan Conn, 1)
	closed := make(chan error, 1)
	// Two layers: the cut arrives at the top one while the bottom one's
	// close walks.
	addr, _ := serveLayered(t, []Layer{LayerFunc(func(LayerContext) LayerHandler { return layer }), PassThroughLayer}, func(ev *Events) {
		ev.Handler = FuncHandler{
			Open: func(c Conn) { signal(server, c) },
			Read: func(c Conn) error {
				_, _ = c.Discard(c.InboundBuffered())
				select {
				case <-blocked:
				default:
					close(blocked)
					<-release
				}
				return nil
			},
			Close: func(_ Conn, err error) { signal(closed, err) },
		}
	})
	client := dialLayered(t, addr, nil, func(ev *Events) {})
	ctx, cancel := context.WithTimeout(context.Background(), layerWait)
	defer cancel()
	conn, err := client.DialContext(ctx, "tcp://"+addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("go")); err != nil { // holds the turn in OnRead
		t.Fatal(err)
	}
	layerWaitFor(t, blocked, "the handler's blocked read")
	layerCtx := layerWaitFor(t, layer.ctx, "the layer's context")
	if err := layerCtx.Close(nil); err != nil { // the close whose walk waits for the turn
		t.Fatal(err)
	}
	cut := errors.New("cut the walk short")
	if err := layerWaitFor(t, server, "the handler's connection").CloseWith(cut); err != nil {
		t.Fatalf("CloseWith = %v, want the end taken", err)
	}
	unblock()
	if cause := layerWaitFor(t, closed, "the close"); !errors.Is(cause, cut) {
		t.Fatalf("closed with %v, want the cut's cause", cause)
	}
	if n := layer.goodbye.Load(); n != 0 {
		t.Fatalf("%d goodbyes ran for a walk the cut dropped", n)
	}
}

// A CloseWith ends a graceful close a CloseGracefully began, at once: the
// drain's LingerTimeout is not waited out. See Conn's documentation.
func TestCloseWithCutsALayeredDrain(t *testing.T) {
	saved := lingerTimeout
	lingerTimeout = 10 * time.Second
	t.Cleanup(func() { lingerTimeout = saved })
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	hold := make(chan struct{})
	peerDone := make(chan struct{})
	go func() {
		defer close(peerDone)
		peer, err := listener.Accept()
		if err != nil {
			return
		}
		defer peer.Close()
		if raw, ok := peer.(*net.TCPConn); ok {
			_ = raw.SetReadBuffer(4 << 10) // the queue cannot leave
		}
		<-hold // and nothing reads it
	}()
	closed := make(chan error, 1)
	client := dialLayered(t, listener.Addr().String(), []Layer{PassThroughLayer}, func(ev *Events) {
		ev.Handler = FuncHandler{
			Open:  func(Conn) {},
			Read:  func(c Conn) error { _, _ = c.Discard(c.InboundBuffered()); return nil },
			Close: func(_ Conn, err error) { signal(closed, err) },
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), layerWait)
	defer cancel()
	conn, err := client.DialContext(ctx, "tcp://"+listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write(make([]byte, 8<<20)); err != nil {
		t.Fatal(err)
	}
	if err := conn.CloseGracefully(nil); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond) // the drain is under way, and cannot progress
	began := time.Now()
	if err := conn.CloseWith(io.EOF); err != nil {
		t.Fatalf("CloseWith = %v, want the cut taken", err)
	}
	select {
	case <-closed:
		if took := time.Since(began); took > 2*time.Second {
			t.Fatalf("the cut took %v, the drain's bound was waited out", took)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("CloseWith never ended the drain")
	}
	close(hold)
	<-peerDone
}

// goodbyeAborts is a layer whose goodbye aborts the connection, as a protocol
// that cannot say goodbye gracefully does. It counts a flush too: one asked
// for after its own abort committed the end must not run.
type goodbyeAborts struct {
	PassThrough
	abort        error
	ran          atomic.Int32
	aborted      atomic.Bool
	flushedAfter atomic.Int32
}

func (g *goodbyeAborts) CloseGracefully(ctx LayerContext, _ error) {
	g.ran.Add(1)
	// Marked before the abort: any flush from here on is the walk's leak.
	g.aborted.Store(true)
	_ = ctx.Abort(g.abort)
}

func (g *goodbyeAborts) Flush(LayerContext) error {
	if g.aborted.Load() {
		g.flushedAfter.Add(1)
	}
	return nil
}

// goodbyeCounts runs no protocol; it counts that the walk reached it.
type goodbyeCounts struct {
	PassThrough
	ran atomic.Int32
}

func (g *goodbyeCounts) CloseGracefully(LayerContext, error) { g.ran.Add(1) }

// An abort from inside a goodbye ends the close then and there: the walk is
// over, and the layers below it are told nothing — the socket's close was
// already committed.
func TestAbortInGoodbyeStopsTheWalk(t *testing.T) {
	abortErr := errors.New("no graceful goodbye")
	top := &goodbyeAborts{abort: abortErr}
	bottom := &goodbyeCounts{}
	closed := make(chan error, 1)
	attached := make(chan Conn, 1)
	addr, _ := serveLayered(t, []Layer{LayerFunc(func(LayerContext) LayerHandler { return bottom }), LayerFunc(func(LayerContext) LayerHandler { return top })}, func(ev *Events) {
		ev.Handler = FuncHandler{
			Open:  func(c Conn) { signal(attached, c) },
			Read:  func(c Conn) error { _, _ = c.Discard(c.InboundBuffered()); return nil },
			Close: func(_ Conn, err error) { signal(closed, err) },
		}
	})
	client := dialLayered(t, addr, nil, func(ev *Events) {})
	ctx, cancel := context.WithTimeout(context.Background(), layerWait)
	defer cancel()
	conn, err := client.DialContext(ctx, "tcp://"+addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	// The close the walk runs is the layered side's: the Handler's Conn.
	attachedConn := layerWaitFor(t, attached, "the server handler")
	if err := attachedConn.CloseWith(nil); err != nil {
		t.Fatal(err)
	}
	layerWaitFor(t, closed, "the close")
	if n := top.ran.Load(); n != 1 {
		t.Fatalf("the top layer's goodbye ran %d times", n)
	}
	if n := bottom.ran.Load(); n != 0 {
		t.Fatalf("%d goodbyes below an abort that ended the walk", n)
	}
	if n := top.flushedAfter.Load(); n != 0 {
		t.Fatalf("the layer was flushed %d times after its goodbye aborted", n)
	}
}

// A close request that yields to the close already under way must leave no
// mark on its layer: the owner's walk still has to pass through it to reach
// the socket. The handler here holds the turn, so both requests land before
// the walk runs — the sequence a real connection hit.
func TestCloseYieldLeavesTheWalkIntact(t *testing.T) {
	bottom := &stashedClose{ctx: make(chan LayerContext, 1)}
	blocked := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	server := make(chan Conn, 1)
	closed := make(chan error, 1)
	addr, _ := serveLayered(t, []Layer{LayerFunc(func(LayerContext) LayerHandler { return bottom }), PassThroughLayer}, func(ev *Events) {
		ev.Handler = FuncHandler{
			Open: func(c Conn) { signal(server, c) },
			Read: func(c Conn) error {
				_, _ = c.Discard(c.InboundBuffered())
				select {
				case <-blocked:
				default:
					close(blocked)
					<-release
				}
				return nil
			},
			Close: func(_ Conn, err error) { signal(closed, err) },
		}
	})
	client := dialLayered(t, addr, nil, func(ev *Events) {})
	ctx, cancel := context.WithTimeout(context.Background(), layerWait)
	defer cancel()
	conn, err := client.DialContext(ctx, "tcp://"+addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("go")); err != nil {
		t.Fatal(err)
	}
	layerWaitFor(t, blocked, "the handler's blocked read")
	layerCtx := layerWaitFor(t, bottom.ctx, "the layer's context")
	srv := layerWaitFor(t, server, "the handler's connection")
	if err := srv.CloseGracefully(nil); err != nil { // the owner: stores the plan
		t.Fatal(err)
	}
	if err := layerCtx.Close(nil); err != nil { // yields; must leave no mark
		t.Fatal(err)
	}
	unblock() // the walk: through both layers, to the socket
	select {
	case cause := <-closed:
		if cause != nil {
			t.Fatalf("closed with %v, want the graceful close", cause)
		}
	case <-time.After(layerWait):
		t.Fatal("the close never reached the socket")
	}
	if n := bottom.goodbye.Load(); n != 1 {
		t.Fatalf("the bottom layer's goodbye ran %d times, want 1", n)
	}
}

// scriptedQueue is a Conn whose OutboundBuffered the test scripts: it takes
// the value on entry, announces the first read, and waits for the test's
// transfer before returning it — the window a held-bytes transfer rides in.
type scriptedQueue struct {
	Conn
	queue atomic.Int64
	read  chan struct{}
	goOn  chan struct{}
	once  sync.Once
}

func (q *scriptedQueue) OutboundBuffered() int {
	v := int(q.queue.Load())
	q.once.Do(func() { close(q.read) })
	<-q.goOn
	return v
}

// OutboundBuffered reads what a layer holds before the socket: a transfer
// moves bytes down and then drops the hold, and reading the socket first
// would lose the bytes in flight between the two reads.
func TestOutboundBufferedReadsTheHoldFirst(t *testing.T) {
	var held atomic.Int64
	held.Store(90)
	lower := &scriptedQueue{read: make(chan struct{}), goOn: make(chan struct{})}
	signalFirstRead := func() { lower.once.Do(func() { close(lower.read) }) }
	node := &layerNode{stack: &stack{}, lower: lower, ops: layerOps{held: func(LayerContext) int {
		v := int(held.Load())
		signalFirstRead()
		<-lower.goOn
		return v
	}}}
	got := make(chan int, 1)
	go func() { got <- node.OutboundBuffered() }()
	<-lower.read // whichever of the two reads ran first
	// The transfer: the layer writes its bytes down, then drops them.
	lower.queue.Store(90)
	held.Store(0)
	close(lower.goOn)
	if buffered := <-got; buffered < 90 {
		t.Fatalf("OutboundBuffered = %d during a transfer, want the bytes counted", buffered)
	}
}

// One Events starts once: a second Serve returns ErrEventsStarted and leaves
// the running one — its loops, its acceptor — alone.
func TestServeTwiceFails(t *testing.T) {
	started := make(chan struct{})
	events := &Events{Pollers: 1, Handler: FuncHandler{}}
	events.Hooks = FuncHandler{Start: func(*Events) { close(started) }}
	done := make(chan error, 1)
	go func() { done <- events.Serve() }()
	<-started
	if err := events.Serve(); !errors.Is(err, ErrEventsStarted) {
		t.Fatalf("second Serve = %v, want ErrEventsStarted", err)
	}
	if err := events.Close(nil); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("first Serve = %v", err)
		}
	case <-time.After(layerWait):
		t.Fatal("the first Serve never returned")
	}
	if err := events.Serve(); !errors.Is(err, ErrEventsStarted) {
		t.Fatalf("Serve after it ended = %v, want ErrEventsStarted", err)
	}
}

// A Serve refused while the first is shutting down must not publish the
// lifecycle's end: Wait keeps waiting until OnStop returns and the loops
// release their connections, which is what the first Serve's return marks.
func TestServeDuringShutdownDoesNotEndTheLifecycle(t *testing.T) {
	stop := make(chan struct{})
	onStop := make(chan struct{})
	started := make(chan struct{})
	events := &Events{Pollers: 1, Handler: FuncHandler{}}
	events.Hooks = FuncHandler{
		Start: func(*Events) { close(started) },
		Stop: func(*Events) {
			close(onStop)
			<-stop
		},
	}
	done := make(chan error, 1)
	go func() { done <- events.Serve() }()
	<-started
	if err := events.Close(nil); err != nil {
		t.Fatal(err)
	}
	if err := events.Serve(); !errors.Is(err, ErrEventsStarted) {
		t.Fatalf("a Serve during shutdown = %v, want ErrEventsStarted", err)
	}
	waited := make(chan error, 1)
	go func() { waited <- events.Wait() }()
	select {
	case <-onStop:
	case <-time.After(layerWait):
		t.Fatal("OnStop never ran")
	}
	select {
	case <-waited:
		t.Fatal("Wait returned while OnStop was still running")
	case <-time.After(100 * time.Millisecond):
	}
	close(stop)
	select {
	case err := <-waited:
		if err != nil {
			t.Fatalf("Wait = %v", err)
		}
	case <-time.After(layerWait):
		t.Fatal("Wait never returned after OnStop")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("first Serve = %v", err)
		}
	case <-time.After(layerWait):
		t.Fatal("the first Serve never returned")
	}
}

// framingHold holds what it is given and, on flush, writes it with a
// five-byte trailer: its output is larger than the input it accepted, so a
// receipt taken at input size would point past bytes not yet written.
type framingHold struct {
	PassThrough
	mu      sync.Mutex
	held    []byte
	counted int        // held plus what a flush is writing below, until it returns
	writeMu sync.Mutex // orders flushes: a turn's and a caller's may run at once
}

func (h *framingHold) OnWrite(_ LayerContext, p []byte) (int, error) {
	h.mu.Lock()
	h.held = append(h.held, p...)
	h.counted += len(p)
	h.mu.Unlock()
	return len(p), nil
}

func (h *framingHold) OutboundHeld(LayerContext) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.counted
}

func (h *framingHold) Flush(ctx LayerContext) error {
	h.writeMu.Lock()
	defer h.writeMu.Unlock()
	h.mu.Lock()
	held := h.held
	h.held = nil
	h.mu.Unlock()
	if len(held) == 0 {
		return nil
	}
	// A trailer far past the socket's send buffer, so the queue genuinely
	// holds unsent bytes after the peer's first read. The bytes count until
	// the write below returned, as the OutboundHolder contract says.
	out := append(held, bytes.Repeat([]byte{'+'}, 8<<20)...)
	_, err := ctx.Down().Write(out)
	h.mu.Lock()
	h.counted -= len(held)
	h.mu.Unlock()
	return err
}

// A tracked write whose path runs through a holder gets no wire position: a
// flush can put more bytes on the wire than the write wrote, so the receipt
// must wait until everything handed down has left, not fire when the input
// size is passed.
func TestTrackedWritePastAnExpandingHolder(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	firstFour := make(chan string, 1)
	rest := make(chan string, 1)
	release := make(chan struct{})
	// The peer stays open until the test ends: a receipt still outstanding
	// when the connection closes is not reported, so a peer that closed as
	// soon as it read the last byte would race the receipt it is here to
	// observe.
	untilDone := make(chan struct{})
	t.Cleanup(func() { close(untilDone) })
	var progress atomic.Int64
	go func() {
		// A host reusing freed ports may deliver another test's stray
		// connection first; it carries nothing, so the peer reads the next
		// one.
		var peer net.Conn
		for {
			var err error
			peer, err = listener.Accept()
			if err != nil {
				return
			}
			head := make([]byte, 4)
			n, _ := io.ReadFull(peer, head)
			if n == 0 {
				_ = peer.Close()
				continue
			}
			firstFour <- string(head[:n])
			break
		}
		defer peer.Close()
		<-release
		trailer := make([]byte, 8<<20)
		for off := 0; off < len(trailer); {
			n, err := peer.Read(trailer[off:])
			off += n
			progress.Store(int64(off))
			if err != nil {
				break
			}
		}
		rest <- string(trailer)
		<-untilDone
	}()
	sent := make(chan SendReceipt, 2)
	events := dialLayered(t, listener.Addr().String(), []Layer{LayerFunc(func(LayerContext) LayerHandler { return &framingHold{} })}, func(ev *Events) {
		ev.Handler = FuncHandler{
			Sent:  func(_ Conn, r SendReceipt) { sent <- r },
			Close: func(Conn, error) {},
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), layerWait)
	defer cancel()
	conn, err := events.DialContext(ctx, "tcp://"+listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, _, err := conn.(TrackedWriter).WriteTracked([]byte("abcd")); err != nil {
		t.Fatal(err)
	}
	if err := conn.Flush(); err != nil { // the holder expands 4 bytes by far more than the socket takes
		t.Fatal(err)
	}
	if got := layerWaitFor(t, firstFour, "the peer's first read"); got != "abcd" {
		t.Fatalf("the peer read %q", got)
	}
	select {
	case r := <-sent:
		node := conn.(*layerNode)
		t.Fatalf("receipt fired early: r=%+v firstHeld=%d held=%d queued=%d", r, node.stack.firstHeld, node.held(), node.OutboundBuffered())
	case <-time.After(300 * time.Millisecond):
	}
	close(release)
	select {
	case got := <-rest:
		if len(got) != 8<<20 {
			t.Fatalf("the peer read %d trailer bytes after the first four", len(got))
		}
	case <-time.After(layerWait):
		node := conn.(*layerNode)
		t.Fatalf("the peer's drain stalled: progress=%d queue=%d state=%b", progress.Load(), node.OutboundBuffered(), conn.State())
	}
	select {
	case <-sent:
	case <-time.After(layerWait):
		node := conn.(*layerNode)
		t.Fatalf("the receipt never fired though the queue emptied: queue=%d progress=%d state=%b", node.OutboundBuffered(), progress.Load(), conn.State())
	}
}

// trackingHold holds nothing itself and starts one tracked write below, in
// its OnOpen. It is a holder above that write's path: the write passes a
// holder below it while another holder sits above, which once made the
// receipt look exact and fire on the input's count alone.
type trackingHold struct {
	PassThrough
	sent chan SendReceipt
}

func (h *trackingHold) OnOpen(ctx LayerContext) {
	_, _, _ = ctx.Down().(TrackedWriter).WriteTracked([]byte("abcd"))
	ctx.Up().Open()
}

func (h *trackingHold) OutboundHeld(LayerContext) int { return 0 }

func (h *trackingHold) Flush(LayerContext) error { return nil }

func (h *trackingHold) OnSent(_ LayerContext, r SendReceipt) {
	select {
	case h.sent <- r:
	default:
	}
}

// A tracked write below a holder keeps its receipt inexact even when another
// holder sits above the write: only the path below the write can expand what
// it accepted.
func TestTrackedWriteHeldBelowAHolderAbove(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	firstFour := make(chan string, 1)
	rest := make(chan string, 1)
	release := make(chan struct{})
	untilDone := make(chan struct{})
	t.Cleanup(func() { close(untilDone) })
	var progress atomic.Int64
	go func() {
		// A host reusing freed ports may deliver another test's stray
		// connection first; it carries nothing, so the peer reads the next
		// one.
		var peer net.Conn
		for {
			var err error
			peer, err = listener.Accept()
			if err != nil {
				return
			}
			head := make([]byte, 4)
			n, _ := io.ReadFull(peer, head)
			if n == 0 {
				_ = peer.Close()
				continue
			}
			firstFour <- string(head[:n])
			break
		}
		defer peer.Close()
		<-release
		trailer := make([]byte, 8<<20)
		for off := 0; off < len(trailer); {
			n, err := peer.Read(trailer[off:])
			off += n
			progress.Store(int64(off))
			if err != nil {
				break
			}
		}
		rest <- string(trailer)
		<-untilDone
	}()
	sent := make(chan SendReceipt, 2)
	events := dialLayered(t, listener.Addr().String(), []Layer{
		LayerFunc(func(LayerContext) LayerHandler { return &framingHold{} }),
		LayerFunc(func(LayerContext) LayerHandler { return &trackingHold{sent: sent} }),
	}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), layerWait)
	defer cancel()
	conn, err := events.DialContext(ctx, "tcp://"+listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if got := layerWaitFor(t, firstFour, "the peer's first read"); got != "abcd" {
		t.Fatalf("the peer read %q", got)
	}
	select {
	case r := <-sent:
		node := conn.(*layerNode)
		t.Fatalf("receipt fired early: r=%+v queued=%d", r, node.OutboundBuffered())
	case <-time.After(300 * time.Millisecond):
	}
	close(release)
	select {
	case got := <-rest:
		if len(got) != 8<<20 {
			t.Fatalf("the peer read %d trailer bytes after the first four", len(got))
		}
	case <-time.After(layerWait):
		t.Fatalf("the peer's drain stalled: progress=%d", progress.Load())
	}
	select {
	case <-sent:
	case <-time.After(layerWait):
		node := conn.(*layerNode)
		t.Fatalf("the receipt never fired though the queue emptied: queued=%d progress=%d state=%b", node.OutboundBuffered(), progress.Load(), conn.State())
	}
}
