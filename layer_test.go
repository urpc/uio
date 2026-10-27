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
		return LayerFunc(func(LayerContext) LayerConn { return &closeLayer{name: name, record: record} })
	}
	started := make(chan struct{})
	events := &Events{Pollers: 1}
	events.Hooks = FuncHandler{Start: func(*Events) { close(started) }}
	events.Handler = FuncHandler{
		Open:  func(Conn) {},
		Data:  func(Conn) error { return nil },
		Close: func(Conn, error) { record("handler") },
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
	twice := LayerFunc(func(LayerContext) LayerConn { return writeTwice{} })
	events := dialLayered(t, silent.Addr().String(), func(ev *Events) {
		ev.MaxOutboundBuffered = 64 << 10
		ev.Use(twice, passThroughLayer{})
	})
	ctx, cancel := context.WithTimeout(context.Background(), layerWait)
	defer cancel()
	conn, err := events.DialContext(ctx, "tcp://"+silent.Addr().String(), nil)
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

func (writeTwice) OnOutbound(ctx LayerContext, p []byte) (int, error) {
	half := len(p) / 2
	if _, err := ctx.Write(p[:half]); err != nil {
		return 0, err
	}
	if _, err := ctx.Write(p[half:]); err != nil {
		return half, err
	}
	return len(p), nil
}

// A layer that observes sends hears every byte the socket sent, counted on
// the wire — its own and those of the layers above — whether or not an
// Observer listens, and an Observer still hears them all as well.
func TestSendObserverHearsTheWire(t *testing.T) {
	for _, withObserver := range []bool{false, true} {
		t.Run(map[bool]string{false: "alone", true: "beside an Observer"}[withObserver], func(t *testing.T) {
			addr, echo := serveLayered(t, nil)
			var heard, observed atomic.Int64
			counter := LayerFunc(func(LayerContext) LayerConn { return &sendCounter{heard: &heard} })
			events := dialLayered(t, addr, func(ev *Events) {
				ev.Use(counter, mapBytes(0x00, xor))
				if withObserver {
					ev.Observer = FuncHandler{Outbound: func(_ Conn, n int) { observed.Add(int64(n)) }}
				}
			})
			ctx, cancel := context.WithTimeout(context.Background(), layerWait)
			defer cancel()
			conn, err := events.DialContext(ctx, "tcp://"+addr, nil)
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

func (s *sendCounter) OnSent(_ LayerContext, n int) { s.heard.Add(int64(n)) }

// A goodbye reaches the peer behind everything queued before it: the socket
// closes once its queue emptied, not as the close is asked for, on either
// backend. A peer that reads nothing more holds the close for the goodbye
// timeout at most.
func TestGoodbyeLeavesBeforeTheSocketCloses(t *testing.T) {
	goodbye := LayerFunc(func(LayerContext) LayerConn { return sayGoodbye{} })
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
		events := dialLayered(t, listener.Addr().String(), func(ev *Events) {
			ev.Handler = FuncHandler{
				Open:  func(Conn) {},
				Data:  func(c Conn) error { _, _ = c.Discard(c.InboundBuffered()); return nil },
				Close: func(_ Conn, err error) { closed <- err },
			}
			ev.Use(goodbye)
		})
		ctx, cancel := context.WithTimeout(context.Background(), layerWait)
		defer cancel()
		conn, err := events.DialContext(ctx, "tcp://"+listener.Addr().String(), nil)
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
		defer func() { lingerTimeout = saved }()
		run(t, false)
	})
}

// sayGoodbye writes "bye" as the layer above closes.
type sayGoodbye struct{ PassThrough }

func (sayGoodbye) CloseGracefully(ctx LayerContext, _ error) { _, _ = ctx.Write([]byte("bye")) }

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
	events := dialLayered(t, listener.Addr().String(), func(ev *Events) {
		ev.Handler = FuncHandler{
			Open:  func(Conn) {},
			Data:  func(Conn) error { return nil },
			Close: func(_ Conn, err error) { closed <- err },
		}
		ev.Use(LayerFunc(func(LayerContext) LayerConn { return sayGoodbye{} }))
	})
	ctx, cancel := context.WithTimeout(context.Background(), layerWait)
	defer cancel()
	conn, err := events.DialContext(ctx, "tcp://"+listener.Addr().String(), nil)
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
	addr, echo := serveLayered(t, nil)
	events := dialLayered(t, addr, func(ev *Events) { ev.Use(passThroughLayer{}) })
	ctx, cancel := context.WithTimeout(context.Background(), layerWait)
	defer cancel()
	conn, err := events.DialContext(ctx, "tcp://"+addr, nil)
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
	addr, _ := serveLayered(t, nil)
	var calls atomic.Int32
	events := dialLayered(t, addr, func(ev *Events) {
		ev.Handler = FuncHandler{
			Open:  func(Conn) { calls.Add(1) },
			Data:  func(Conn) error { return nil },
			Close: func(Conn, error) { calls.Add(1) },
		}
		ev.Use(LayerFunc(func(LayerContext) LayerConn { return slowOpen{} }))
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := events.DialContext(ctx, "tcp://"+addr, nil); !errors.Is(err, context.DeadlineExceeded) {
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
	ctx.Open()
}

// While a graceful close waits for its queue, what the peer sends is
// discarded, not kept: a peer that floods and reads nothing cannot grow the
// connection's memory for as long as the close waits.
func TestLingeringCloseDiscardsWhatArrives(t *testing.T) {
	saved := lingerTimeout
	lingerTimeout = time.Second
	defer func() { lingerTimeout = saved }()
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
	events := dialLayered(t, listener.Addr().String(), func(ev *Events) {
		ev.Handler = FuncHandler{
			Open:  func(Conn) {},
			Data:  func(Conn) error { return nil },
			Close: func(_ Conn, err error) { closed <- err },
		}
		ev.Use(passThroughLayer{})
	})
	ctx, cancel := context.WithTimeout(context.Background(), layerWait)
	defer cancel()
	conn, err := events.DialContext(ctx, "tcp://"+listener.Addr().String(), nil)
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
	events := dialLayered(t, listener.Addr().String(), func(ev *Events) {
		ev.AllowHalfOpen = true
		var read []byte
		ev.Handler = FuncHandler{
			Open: func(Conn) {},
			Data: func(c Conn) error {
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
		ev.Use(passThroughLayer{}, passThroughLayer{})
	})
	ctx, cancel := context.WithTimeout(context.Background(), layerWait)
	defer cancel()
	if _, err := events.DialContext(ctx, "tcp://"+listener.Addr().String(), nil); err != nil {
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
	addr, _ := serveLayered(t, nil)
	events := dialLayered(t, addr, func(ev *Events) {
		ev.Use(passThroughLayer{}, LayerFunc(func(LayerContext) LayerConn { return &framing{} }))
	})
	ctx, cancel := context.WithTimeout(context.Background(), layerWait)
	defer cancel()
	conn, err := events.DialContext(ctx, "tcp://"+addr, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.CloseWrite(); err == nil {
		t.Fatal("CloseWrite passed through a layer that cannot end one direction")
	}
}

// framing is a layer that implements LayerConn without PassThrough.
type framing struct{}

func (*framing) OnOpen(ctx LayerContext)                            { ctx.Open() }
func (*framing) OnInbound(LayerContext) error                       { return nil }
func (*framing) OnOutbound(ctx LayerContext, p []byte) (int, error) { return ctx.Write(p) }
func (*framing) OnTimer(LayerContext) error                         { return nil }
func (*framing) OnClose(LayerContext, error)                        {}

// FindLayer looks down the whole stack; what a layer holds counts as queued.
func TestFindLayerAndHeldOutput(t *testing.T) {
	addr, _ := serveLayered(t, nil)
	holder := &holdsOutput{}
	events := dialLayered(t, addr, func(ev *Events) {
		ev.MaxOutboundBuffered = 100
		ev.Use(LayerFunc(func(LayerContext) LayerConn { return holder }), passThroughLayer{})
	})
	ctx, cancel := context.WithTimeout(context.Background(), layerWait)
	defer cancel()
	conn, err := events.DialContext(ctx, "tcp://"+addr, nil)
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
	defer func() { lingerTimeout = saved }()
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
	events := dialLayered(t, listener.Addr().String(), func(ev *Events) {
		ev.Use(LayerFunc(func(LayerContext) LayerConn { return sayGoodbye{} }))
	})
	ctx, cancel := context.WithTimeout(context.Background(), layerWait)
	defer cancel()
	conn, err := events.DialContext(ctx, "tcp://"+listener.Addr().String(), nil)
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
	events := dialLayered(t, silent.Addr().String(), func(ev *Events) {
		ev.MaxOutboundBuffered = limit
		ev.Use(LayerFunc(func(LayerContext) LayerConn { return slowEncode{} }))
	})
	ctx, cancel := context.WithTimeout(context.Background(), layerWait)
	defer cancel()
	conn, err := events.DialContext(ctx, "tcp://"+silent.Addr().String(), nil)
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

func (slowEncode) OnOutbound(ctx LayerContext, p []byte) (int, error) {
	time.Sleep(200 * time.Microsecond)
	return ctx.Write(p)
}

// Discard(-1) drops everything a layer delivered, as the socket's does —
// FuncHandler's default OnData calls it — and leaves the connection whole for
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
			// No Data: the default discards everything, and the reply below
			// shows the connection still works.
			handler = FuncHandler{Open: func(c Conn) {
				go func() { <-time.After(100 * time.Millisecond); _, _ = c.Write([]byte("alive")); _ = c.Close() }()
			}}
		} else {
			handler = FuncHandler{
				Open: func(Conn) {},
				Data: func(c Conn) error {
					if chunk := c.PeekChunk(); len(chunk) > 0 {
						reads <- string(chunk)
						_, _ = c.Discard(-1)
					}
					return nil
				},
				Close: func(Conn, error) {},
			}
		}
		events := dialLayered(t, listener.Addr().String(), func(ev *Events) {
			ev.Handler = handler
			ev.Use(passThroughLayer{})
		})
		ctx, cancel := context.WithTimeout(context.Background(), layerWait)
		conn, err := events.DialContext(ctx, "tcp://"+listener.Addr().String(), nil)
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
	events := dialLayered(t, listener.Addr().String(), func(ev *Events) {
		ev.MaxInboundBuffered = limit
		ev.Handler = FuncHandler{
			Open: func(Conn) {},
			Data: func(c Conn) error {
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
		ev.Use(passThroughLayer{}, passThroughLayer{})
	})
	ctx, cancel := context.WithTimeout(context.Background(), layerWait)
	defer cancel()
	conn, err := events.DialContext(ctx, "tcp://"+listener.Addr().String(), nil)
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
	addr, echo := serveLayered(t, nil)
	events := dialLayered(t, addr, func(ev *Events) {
		ev.Use(LayerFunc(func(LayerContext) LayerConn { return &holdUntilFlush{} }),
			LayerFunc(func(LayerContext) LayerConn { return greeter{} }))
	})
	ctx, cancel := context.WithTimeout(context.Background(), layerWait)
	defer cancel()
	if _, err := events.DialContext(ctx, "tcp://"+addr, nil); err != nil {
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

func (h *holdUntilFlush) OnOutbound(_ LayerContext, p []byte) (int, error) {
	h.mu.Lock()
	h.held = append(h.held, p...)
	h.counted.Add(int64(len(p)))
	h.mu.Unlock()
	return len(p), nil
}

func (h *holdUntilFlush) OutboundHeld(LayerContext) int { return int(h.counted.Load()) }

func (h *holdUntilFlush) Flush(ctx LayerContext) error {
	h.mu.Lock()
	held := h.held
	h.held = nil
	h.mu.Unlock()
	if len(held) == 0 {
		return nil
	}
	_, err := ctx.Write(held)
	h.counted.Add(-int64(len(held)))
	return err
}

// greeter writes a greeting as it opens, as a handshake's first flight
// does, and opens the layers above at once.
type greeter struct{ PassThrough }

func (greeter) OnOpen(ctx LayerContext) {
	_, _ = ctx.Write([]byte("hello"))
	ctx.Open()
}

// An Events with layers refuses packet connections.
func TestLayersRefusePacketConnections(t *testing.T) {
	events := dialLayered(t, "", func(ev *Events) { ev.Use(passThroughLayer{}) })
	if _, err := events.Listen("udp://127.0.0.1:0", nil); !errors.Is(err, ErrLayersNeedStream) {
		t.Fatalf("Listen udp = %v, want ErrLayersNeedStream", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), layerWait)
	defer cancel()
	if _, err := events.DialContext(ctx, "udp://127.0.0.1:9", nil); !errors.Is(err, ErrLayersNeedStream) {
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
	if _, err := events.Adopt(udp, nil); !errors.Is(err, ErrLayersNeedStream) {
		t.Fatalf("Adopt udp = %v, want ErrLayersNeedStream", err)
	}
}

// More layers than the timers' mask holds is refused when Serve starts.
func TestTooManyLayers(t *testing.T) {
	events := &Events{Pollers: 1, Handler: FuncHandler{}}
	for range maxLayers + 1 {
		events.Use(passThroughLayer{})
	}
	if err := events.Serve(); err == nil {
		_ = events.Close(nil)
		t.Fatal("Serve took more layers than the timers can tell apart")
	}
}

// A timer a layer arms before it opens fires once it has, and does not spin
// the connection meanwhile.
func TestTimerArmedBeforeOpenFiresAfter(t *testing.T) {
	addr, _ := serveLayered(t, nil)
	fired := make(chan time.Time, 1)
	var turns atomic.Int64
	events := dialLayered(t, addr, func(ev *Events) {
		ev.Handler = FuncHandler{Open: func(Conn) {}, Data: func(Conn) error { turns.Add(1); return nil }, Close: func(Conn, error) {}}
		ev.Use(LayerFunc(func(LayerContext) LayerConn { return &openLate{} }),
			LayerFunc(func(ctx LayerContext) LayerConn {
				_ = ctx.SetTimer(time.Now().Add(10 * time.Millisecond)) // before it opens
				return &earlyTimer{fired: fired}
			}))
	})
	began := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), layerWait)
	defer cancel()
	if _, err := events.DialContext(ctx, "tcp://"+addr, nil); err != nil {
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
	ctx.Open()
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
	events := dialLayered(t, listener.Addr().String(), func(ev *Events) {
		ev.Use(LayerFunc(func(LayerContext) LayerConn { return sayGoodbye{} }),
			LayerFunc(func(LayerContext) LayerConn { return sayGoodbye{} }))
	})
	ctx, cancel := context.WithTimeout(context.Background(), layerWait)
	defer cancel()
	conn, err := events.DialContext(ctx, "tcp://"+listener.Addr().String(), nil)
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

// An abort in the Handler's OnData drops what the layers hold: the turn
// that aborted flushes nothing more.
func TestAbortInOnDataFlushesNothing(t *testing.T) {
	addr, _ := serveLayered(t, nil)
	holder := &countingHold{}
	aborted := make(chan struct{})
	events := dialLayered(t, addr, func(ev *Events) {
		ev.Handler = FuncHandler{
			Open: func(c Conn) {
				go func() {
					time.Sleep(20 * time.Millisecond)
					_ = c.Wake()
				}()
			},
			Data: func(c Conn) error {
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
		ev.Use(LayerFunc(func(LayerContext) LayerConn { return holder }))
	})
	ctx, cancel := context.WithTimeout(context.Background(), layerWait)
	defer cancel()
	if _, err := events.DialContext(ctx, "tcp://"+addr, nil); err != nil {
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

// A layer that closes the connection in OnInbound ends the turn there: the
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
	events := dialLayered(t, listener.Addr().String(), func(ev *Events) {
		ev.Handler = FuncHandler{
			Open: func(Conn) {},
			Data: func(c Conn) error {
				if chunk := c.PeekChunk(); len(chunk) > 0 {
					seen <- string(chunk)
					_, _ = c.Discard(len(chunk))
				}
				return nil
			},
			Close: func(_ Conn, err error) { closed <- err },
		}
		ev.Use(LayerFunc(func(LayerContext) LayerConn { return deliverThenClose{} }), passThroughLayer{})
	})
	ctx, cancel := context.WithTimeout(context.Background(), layerWait)
	defer cancel()
	if _, err := events.DialContext(ctx, "tcp://"+listener.Addr().String(), nil); err != nil {
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

func (deliverThenClose) OnInbound(ctx LayerContext) error {
	lower := ctx.Lower()
	chunk := lower.PeekChunk()
	if len(chunk) == 0 {
		return nil
	}
	ctx.Deliver(chunk)
	_, _ = lower.Discard(len(chunk))
	return ctx.CloseWith(io.EOF)
}

// A close as the stack opens — the Handler aborting in its OnOpen — flushes
// nothing more, and a close from a layer as it opens opens nothing above it.
func TestCloseAsTheStackOpens(t *testing.T) {
	t.Run("abort in OnOpen", func(t *testing.T) {
		addr, _ := serveLayered(t, nil)
		holder := &countingHold{}
		closed := make(chan error, 1)
		events := dialLayered(t, addr, func(ev *Events) {
			ev.Handler = FuncHandler{
				Open: func(c Conn) {
					_, _ = c.Write([]byte("never sent"))
					holder.aborting.Store(true)
					_ = c.CloseWith(errors.New("abort"))
				},
				Data:  func(Conn) error { return nil },
				Close: func(_ Conn, err error) { closed <- err },
			}
			ev.Use(LayerFunc(func(LayerContext) LayerConn { return holder }))
		})
		ctx, cancel := context.WithTimeout(context.Background(), layerWait)
		defer cancel()
		_, _ = events.DialContext(ctx, "tcp://"+addr, nil)
		layerWaitFor(t, closed, "the close")
		if n := holder.flushedAfter.Load(); n != 0 {
			t.Fatalf("the held output was flushed %d times after the abort", n)
		}
	})
	t.Run("close in a layer's OnOpen", func(t *testing.T) {
		addr, _ := serveLayered(t, nil)
		var opened atomic.Int32
		events := dialLayered(t, addr, func(ev *Events) {
			ev.Handler = FuncHandler{Open: func(Conn) {}, Data: func(Conn) error { return nil }, Close: func(Conn, error) {}}
			ev.Use(LayerFunc(func(LayerContext) LayerConn { return closeThenOpen{} }),
				LayerFunc(func(LayerContext) LayerConn { return &countOpens{n: &opened} }))
		})
		ctx, cancel := context.WithTimeout(context.Background(), layerWait)
		defer cancel()
		if _, err := events.DialContext(ctx, "tcp://"+addr, nil); err == nil {
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
	_ = ctx.CloseWith(errors.New("refused"))
	ctx.Open()
}

type countOpens struct {
	PassThrough
	n *atomic.Int32
}

func (c *countOpens) OnOpen(ctx LayerContext) {
	c.n.Add(1)
	ctx.Open()
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
	events := dialLayered(t, listener.Addr().String(), func(ev *Events) {
		ev.Handler = FuncHandler{Open: func(Conn) {}, Data: func(Conn) error { return nil }, Close: func(_ Conn, err error) { closed <- err }}
		ev.Use(LayerFunc(func(LayerContext) LayerConn { return closer }))
	})
	ctx, cancel := context.WithTimeout(context.Background(), layerWait)
	defer cancel()
	if _, err := events.DialContext(ctx, "tcp://"+listener.Addr().String(), nil); err != nil {
		t.Fatal(err)
	}
	layerWaitFor(t, closed, "the close")
	if n := closer.after.Load(); n != 0 {
		t.Fatalf("OnInbound ran %d times after the layer closed the connection", n)
	}
}

// byteThenClose reads one byte and closes the connection.
type byteThenClose struct {
	PassThrough
	closed atomic.Bool
	after  atomic.Int32
}

func (b *byteThenClose) OnInbound(ctx LayerContext) error {
	if b.closed.Load() {
		b.after.Add(1)
	}
	lower := ctx.Lower()
	if len(lower.PeekChunk()) == 0 {
		return nil
	}
	_, _ = lower.Discard(1)
	b.closed.Store(true)
	return ctx.CloseWith(io.EOF)
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
			s.layers = []*layerNode{l}
			ctx := layerCtx{l}
			ctx.Deliver([]byte{1})
			chunk := make([]byte, 8)
			for range 20000 {
				if space {
					copy(ctx.Space(len(chunk)), chunk)
					ctx.Commit(len(chunk))
				} else {
					ctx.Deliver(chunk)
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
	addr, echo := serveLayered(t, nil)
	opened := make(chan Conn, 1)
	events := dialLayered(t, addr, func(ev *Events) {
		ev.Handler = FuncHandler{Open: func(c Conn) { opened <- c }, Data: func(c Conn) error { _, _ = c.Discard(-1); return nil }, Close: func(Conn, error) {}}
		ev.Use(LayerFunc(func(LayerContext) LayerConn { return upperCase{} }))
	})
	raw, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := events.Adopt(raw, "adopted")
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

	refusing := dialLayered(t, addr, func(ev *Events) {
		ev.Use(LayerFunc(func(LayerContext) LayerConn { return refuseLayer{cause: errors.New("refused")} }))
	})
	raw, err = net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := refusing.Adopt(raw, nil); err == nil || err.Error() != "refused" {
		t.Fatalf("Adopt through a refusing layer = %v, want its refusal", err)
	}
}

// upperCase writes everything upper-cased.
type upperCase struct{ PassThrough }

func (upperCase) OnOutbound(ctx LayerContext, p []byte) (int, error) {
	if _, err := ctx.Write(bytes.ToUpper(p)); err != nil {
		return 0, err
	}
	return len(p), nil
}

// An Events with layers and no Handler runs its connections as one without
// layers does: no callbacks, and what arrives discarded, the connection
// open all the while.
func TestLayersWithoutAHandler(t *testing.T) {
	addr, echo := serveLayered(t, nil)
	events := dialLayered(t, addr, func(ev *Events) {
		ev.Handler = nil
		ev.Use(passThroughLayer{})
	})
	ctx, cancel := context.WithTimeout(context.Background(), layerWait)
	defer cancel()
	conn, err := events.DialContext(ctx, "tcp://"+addr, nil)
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
	addr, echo := serveLayered(t, nil)
	events := dialLayered(t, addr, func(ev *Events) {
		ev.Handler = (*FuncHandler)(nil)
		ev.Use(passThroughLayer{})
	})
	ctx, cancel := context.WithTimeout(context.Background(), layerWait)
	defer cancel()
	conn, err := events.DialContext(ctx, "tcp://"+addr, nil)
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
			peer, err := listener.Accept()
			if err != nil {
				return
			}
			defer peer.Close()
			_ = peer.SetReadDeadline(time.Now().Add(layerWait))
			all, err := io.ReadAll(peer)
			got <- outcome{string(all), err}
		}()
		return listener.Addr().String(), got
	}
	holder := func() Layer { return LayerFunc(func(LayerContext) LayerConn { return &holdUntilFlush{} }) }

	for _, where := range []string{"OnOpen", "OnData"} {
		for _, layers := range []int{1, 2} {
			t.Run(fmt.Sprintf("%s, %d buffering layers", where, layers), func(t *testing.T) {
				addr, got := peerReads(t)
				results := make(chan error, 2)
				writeAndEnd := func(c Conn) {
					_, err := c.Write([]byte("hello"))
					results <- err
					results <- c.CloseWrite()
				}
				events := dialLayered(t, addr, func(ev *Events) {
					handler := FuncHandler{Close: func(Conn, error) {}}
					if where == "OnOpen" {
						handler.Open = writeAndEnd
					} else {
						handler.Open = func(c Conn) { _ = c.Wake() }
						handler.Data = func(c Conn) error {
							if c.Userdata() == nil {
								c.SetUserdata(true)
								writeAndEnd(c)
							}
							return nil
						}
					}
					ev.Handler = handler
					for range layers {
						ev.Use(holder())
					}
				})
				ctx, cancel := context.WithTimeout(context.Background(), layerWait)
				defer cancel()
				if _, err := events.DialContext(ctx, "tcp://"+addr, nil); err != nil {
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
		events := dialLayered(t, addr, func(ev *Events) { ev.Use(holder()) })
		ctx, cancel := context.WithTimeout(context.Background(), layerWait)
		defer cancel()
		conn, err := events.DialContext(ctx, "tcp://"+addr, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := conn.CloseWrite(); err != nil {
			t.Fatal(err)
		}
		if _, err := conn.Write([]byte("late")); !errors.Is(err, net.ErrClosed) {
			t.Fatalf("a write after CloseWrite = %v, want net.ErrClosed", err)
		}
		if err := conn.CloseWrite(); err != nil {
			t.Fatalf("CloseWrite again = %v, want nothing", err)
		}
	})

	t.Run("a flush that fails", func(t *testing.T) {
		addr, _ := peerReads(t)
		failing := &failingFlush{cause: errors.New("flush refused")}
		events := dialLayered(t, addr, func(ev *Events) {
			ev.Use(LayerFunc(func(LayerContext) LayerConn { return failing }))
		})
		ctx, cancel := context.WithTimeout(context.Background(), layerWait)
		defer cancel()
		conn, err := events.DialContext(ctx, "tcp://"+addr, nil)
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
		events := dialLayered(t, addr, func(ev *Events) {
			ev.Use(LayerFunc(func(LayerContext) LayerConn { return window }))
		})
		ctx, cancel := context.WithTimeout(context.Background(), layerWait)
		defer cancel()
		conn, err := events.DialContext(ctx, "tcp://"+addr, nil)
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

// sendWindow holds what is written until release, as a layer with a closed
// send window does; Flush cannot send it.
type sendWindow struct {
	PassThrough
	mu      sync.Mutex
	ctx     LayerContext
	held    []byte
	counted atomic.Int64
}

func (w *sendWindow) OnOpen(ctx LayerContext) {
	w.mu.Lock()
	w.ctx = ctx
	w.mu.Unlock()
	ctx.Open()
}

func (w *sendWindow) OnOutbound(_ LayerContext, p []byte) (int, error) {
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
	_, _ = ctx.Write(held)
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

func (g *gatedHold) OnOutbound(ctx LayerContext, p []byte) (int, error) {
	g.entered <- struct{}{}
	<-g.proceed
	return g.holdUntilFlush.OnOutbound(ctx, p)
}

// A CloseWrite beside writes from other goroutines ends the stream after
// every write it did not refuse, and refuses the rest whole.
func TestCloseWriteBesideWrites(t *testing.T) {
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
			peer, err := listener.Accept()
			if err != nil {
				return
			}
			defer peer.Close()
			_ = peer.SetReadDeadline(time.Now().Add(layerWait))
			all, err := io.ReadAll(peer)
			got <- outcome{string(all), err}
		}()
		return listener.Addr().String(), got
	}

	t.Run("a write inside the layer", func(t *testing.T) {
		addr, got := peerReads(t)
		gate := &gatedHold{entered: make(chan struct{}, 1), proceed: make(chan struct{})}
		events := dialLayered(t, addr, func(ev *Events) {
			ev.Use(LayerFunc(func(LayerContext) LayerConn { return gate }))
		})
		ctx, cancel := context.WithTimeout(context.Background(), layerWait)
		defer cancel()
		conn, err := events.DialContext(ctx, "tcp://"+addr, nil)
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
			addr, got := peerReads(t)
			events := dialLayered(t, addr, func(ev *Events) {
				ev.Use(LayerFunc(func(LayerContext) LayerConn { return &holdUntilFlush{} }))
			})
			ctx, cancel := context.WithTimeout(context.Background(), layerWait)
			conn, err := events.DialContext(ctx, "tcp://"+addr, nil)
			cancel()
			if err != nil {
				t.Fatal(err)
			}
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

func (w *windowFlush) OnOutbound(_ LayerContext, p []byte) (int, error) {
	w.mu.Lock()
	w.held = append(w.held, p...)
	w.counted.Add(int64(len(p)))
	w.mu.Unlock()
	return len(p), nil
}

func (w *windowFlush) OutboundHeld(LayerContext) int { return int(w.counted.Load()) }

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
	_, err := ctx.Write(held)
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
				peer, err := listener.Accept()
				if err != nil {
					return
				}
				defer peer.Close()
				_ = peer.SetReadDeadline(time.Now().Add(layerWait))
				all, err := io.ReadAll(peer)
				got <- outcome{string(all), err}
			}()
			window := &windowFlush{shutFirst: path == "OnOpen's flush"}
			results := make(chan error, 2)
			addr := listener.Addr().String()
			events := dialLayered(t, addr, func(ev *Events) {
				ev.Handler = FuncHandler{
					Open: func(c Conn) {
						_, err := c.Write([]byte("hello"))
						results <- err
						results <- c.CloseWrite()
					},
					Close: func(Conn, error) {},
				}
				ev.Use(LayerFunc(func(LayerContext) LayerConn { return window }))
			})
			ctx, cancel := context.WithTimeout(context.Background(), layerWait)
			defer cancel()
			conn, err := events.DialContext(ctx, "tcp://"+addr, nil)
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

func (l *endedLayer) OnOutbound(ctx LayerContext, p []byte) (int, error) {
	l.lateCall()
	return ctx.Write(p)
}
func (l *endedLayer) OutboundHeld(LayerContext) int { l.lateCall(); return 0 }
func (l *endedLayer) Flush(LayerContext) error      { l.lateCall(); return nil }
func (l *endedLayer) CloseWrite(ctx LayerContext) error {
	l.lateCall()
	return ctx.Lower().CloseWrite()
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
			events := dialLayered(t, addr, func(ev *Events) {
				ev.Handler = FuncHandler{Close: func(Conn, error) { close(closed) }}
				for _, layer := range layers {
					ev.Use(LayerFunc(func(LayerContext) LayerConn { return layer }))
				}
			})
			ctx, cancel := context.WithTimeout(context.Background(), layerWait)
			defer cancel()
			conn, err := events.DialContext(ctx, "tcp://"+addr, nil)
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

func (w *guardedWindow) OnOutbound(_ LayerContext, p []byte) (int, error) {
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
		_, err := ctx.Write(held)
		w.counted.Add(-int64(len(held)))
		if err != nil {
			return err
		}
	}
	return ctx.Lower().Flush()
}

func (w *guardedWindow) CloseWrite(ctx LayerContext) error {
	entered := w.guard.enter()
	defer w.guard.exit(entered)
	return ctx.Lower().CloseWrite()
}

// lockedWriter keeps its lock while it writes below, as crypto/tls keeps its
// record lock.
type lockedWriter struct {
	PassThrough
	guard reentryGuard
}

func (w *lockedWriter) OnOutbound(ctx LayerContext, p []byte) (int, error) {
	entered := w.guard.enter()
	defer w.guard.exit(entered)
	return ctx.Write(p)
}

// sentWriter writes once, from OnSent, after its first send.
type sentWriter struct {
	PassThrough
	once sync.Once
}

func (s *sentWriter) OnSent(ctx LayerContext, _ int) {
	s.once.Do(func() { _, _ = ctx.Write([]byte("!")) })
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

func (s *selfCloser) OnInbound(ctx LayerContext) error {
	if !bytes.Contains(ctx.Lower().PeekChunk(), []byte("bye")) {
		return s.PassThrough.OnInbound(ctx)
	}
	entered := s.guard.enter()
	defer s.guard.exit(entered)
	_, _ = ctx.Lower().Discard(-1)
	_ = ctx.CloseWith(nil)
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
	dial := func(t *testing.T, addr string, setup func(*Events)) Conn {
		events := dialLayered(t, addr, setup)
		ctx, cancel := context.WithTimeout(context.Background(), layerWait)
		defer cancel()
		conn, err := events.DialContext(ctx, "tcp://"+addr, nil)
		if err != nil {
			t.Fatal(err)
		}
		return conn
	}

	t.Run("a flush below ends a CloseWrite above", func(t *testing.T) {
		addr, got := peer(t, "")
		window := &guardedWindow{}
		ended := make(chan error, 1)
		conn := dial(t, addr, func(ev *Events) {
			ev.Use(passThroughLayer{}, LayerFunc(func(LayerContext) LayerConn { return window }))
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
		if err := conn.Flush(); err != nil {
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
		conn := dial(t, addr, func(ev *Events) {
			ev.Use(LayerFunc(func(LayerContext) LayerConn { return locked }),
				LayerFunc(func(LayerContext) LayerConn { return &sentWriter{} }))
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
			t.Fatalf("OnSent wrote into the layer %d times inside its own write", n)
		}
	})

	t.Run("a layer closing under its lock", func(t *testing.T) {
		addr, got := peer(t, "bye")
		closer := &selfCloser{}
		dial(t, addr, func(ev *Events) {
			ev.Use(LayerFunc(func(LayerContext) LayerConn { return closer }), passThroughLayer{})
		})
		if out := layerWaitFor(t, got, "the peer's read"); out.err != nil {
			t.Fatalf("the peer's read failed: %v; want the end", out.err)
		}
		if n := closer.guard.reentered.Load(); n != 0 {
			t.Fatalf("the layer's close flushed it %d times inside its own callback", n)
		}
	})
}

// sentCounter counts OnSent calls, and those made once the stack was
// closing.
type sentCounter struct {
	PassThrough
	calls, closing atomic.Int32
}

func (s *sentCounter) OnSent(ctx LayerContext, _ int) {
	s.calls.Add(1)
	if ctx.(layerCtx).l.stack.closing.Load() {
		s.closing.Add(1)
	}
}

// writeOnSent writes once from OnSent, into a layer below that holds it
// until a flush.
type writeOnSent struct {
	PassThrough
	armed atomic.Bool
}

func (w *writeOnSent) OnSent(ctx LayerContext, _ int) {
	if w.armed.CompareAndSwap(true, false) {
		_, _ = ctx.Write([]byte("X"))
	}
}

// closesOnQ closes the connection gracefully, itself, as it reads 'q'.
type closesOnQ struct{ PassThrough }

func (closesOnQ) OnInbound(ctx LayerContext) error {
	if bytes.Contains(ctx.Lower().PeekChunk(), []byte("q")) {
		_, _ = ctx.Lower().Discard(-1)
		return ctx.CloseWith(nil)
	}
	return PassThrough{}.OnInbound(ctx)
}

// What waits for a turn — send reports, a close's linger — keeps to the
// layers' contract: nothing reaches them once the connection is closing,
// what OnSent writes is flushed, and a layer closing the connection itself
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
	dial := func(t *testing.T, addr string, setup func(*Events)) Conn {
		events := dialLayered(t, addr, setup)
		ctx, cancel := context.WithTimeout(context.Background(), layerWait)
		defer cancel()
		conn, err := events.DialContext(ctx, "tcp://"+addr, nil)
		if err != nil {
			t.Fatal(err)
		}
		return conn
	}

	t.Run("no OnSent while closing", func(t *testing.T) {
		const size = 8 << 20
		addr, got := peer(t, "", 100*time.Millisecond)
		counter := &sentCounter{}
		conn := dial(t, addr, func(ev *Events) {
			ev.Use(LayerFunc(func(LayerContext) LayerConn { return counter }))
		})
		if _, err := conn.Write(make([]byte, size)); err != nil {
			t.Fatal(err)
		}
		_ = conn.Close() // lingers while the peer is slow to read
		if out := layerWaitFor(t, got, "the peer's read"); len(out.read) != size {
			t.Fatalf("the peer read %d bytes, %v; want %d", len(out.read), out.err, size)
		}
		if n := counter.closing.Load(); n != 0 {
			t.Fatalf("OnSent ran %d times on a closing connection", n)
		}
	})

	t.Run("what OnSent writes is flushed", func(t *testing.T) {
		addr, got := peer(t, "", 0)
		writer := &writeOnSent{}
		writer.armed.Store(true)
		conn := dial(t, addr, func(ev *Events) {
			ev.Use(LayerFunc(func(LayerContext) LayerConn { return &holdUntilFlush{} }),
				LayerFunc(func(LayerContext) LayerConn { return writer }))
		})
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
		conn := dial(t, addr, func(ev *Events) {
			ev.Use(LayerFunc(func(LayerContext) LayerConn { return closesOnQ{} }),
				LayerFunc(func(LayerContext) LayerConn { return &holdUntilFlush{} }))
		})
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
	if ctx.(layerCtx).l.stack.closing.Load() {
		p.late.Add(1)
	}
}

func (p *closingProbe) OnSent(ctx LayerContext, _ int) { p.lateCall(ctx) }
func (p *closingProbe) Flush(ctx LayerContext) error   { p.lateCall(ctx); return nil }
func (p *closingProbe) OnTimer(ctx LayerContext) error { p.lateCall(ctx); return nil }
func (p *closingProbe) CloseWrite(ctx LayerContext) error {
	p.lateCall(ctx)
	return ctx.Lower().CloseWrite()
}
func (p *closingProbe) OnOutbound(ctx LayerContext, b []byte) (int, error) {
	p.lateCall(ctx)
	return ctx.Write(b)
}

// closesFromSent closes the connection gracefully from its first OnSent.
type closesFromSent struct {
	PassThrough
	once sync.Once
}

func (c *closesFromSent) OnSent(ctx LayerContext, _ int) {
	c.once.Do(func() { _ = ctx.CloseWith(nil) })
}

// closesFromFlush closes the connection gracefully from a flush once armed.
type closesFromFlush struct {
	PassThrough
	armed atomic.Bool
}

func (c *closesFromFlush) Flush(ctx LayerContext) error {
	if c.armed.CompareAndSwap(true, false) {
		_ = ctx.CloseWith(nil)
	}
	return nil
}

// A layer that closes the connection from OnSent or Flush is the last layer
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
		events := dialLayered(t, addr, func(ev *Events) {
			ev.Use(LayerFunc(func(LayerContext) LayerConn { return below }))
			ev.Use(layers(above)...)
			ev.Handler = FuncHandler{
				Open: func(c Conn) {
					arm()
					_, _ = c.Write([]byte("hello"))
				},
				Data: func(c Conn) error {
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
		conn, err := events.DialContext(ctx, "tcp://"+addr, nil)
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
	t.Run("from OnSent", func(t *testing.T) {
		run(t, func(above *closingProbe) []Layer {
			return []Layer{
				LayerFunc(func(LayerContext) LayerConn { return &closesFromSent{} }),
				LayerFunc(func(LayerContext) LayerConn { return above }),
			}
		}, func() {})
	})
	t.Run("from Flush", func(t *testing.T) {
		closer := &closesFromFlush{}
		run(t, func(above *closingProbe) []Layer {
			return []Layer{
				LayerFunc(func(LayerContext) LayerConn { return closer }),
				LayerFunc(func(LayerContext) LayerConn { return above }),
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
	addr, _ := serveLayered(t, nil)
	events := dialLayered(t, addr, func(ev *Events) {
		ev.Use(LayerFunc(func(LayerContext) LayerConn { return peerAnswersGoodbye{} }))
	})
	ctx, cancel := context.WithTimeout(context.Background(), layerWait)
	defer cancel()
	conn, err := events.DialContext(ctx, "tcp://"+addr, nil)
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
// OnOutbound returns — a framing layer writing a header and a body — and
// stops between them until let through.
type splitWriter struct {
	PassThrough
	entered chan struct{}
	proceed chan struct{}
}

func (s *splitWriter) OnOutbound(ctx LayerContext, p []byte) (int, error) {
	n, err := ctx.Write(p[:2])
	if err != nil {
		return n, err
	}
	s.entered <- struct{}{}
	<-s.proceed
	m, err := ctx.Write(p[2:])
	return n + m, err
}

// closesInShutFlush closes the connection from the flush CloseWrite makes.
type closesInShutFlush struct {
	closingProbe
	armed atomic.Bool
}

func (c *closesInShutFlush) Flush(ctx LayerContext) error {
	if c.armed.CompareAndSwap(true, false) {
		return ctx.CloseWith(nil)
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
		events := dialLayered(t, addr, func(ev *Events) {
			ev.Use(LayerFunc(func(LayerContext) LayerConn { return split }))
		})
		ctx, cancel := context.WithTimeout(context.Background(), layerWait)
		defer cancel()
		conn, err := events.DialContext(ctx, "tcp://"+addr, nil)
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
		events := dialLayered(t, addr, func(ev *Events) {
			ev.Use(LayerFunc(func(LayerContext) LayerConn { return layer }))
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
		if _, err := events.DialContext(ctx, "tcp://"+addr, nil); err != nil {
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
