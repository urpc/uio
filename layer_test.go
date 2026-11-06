package uio

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"github.com/urpc/uio/internal/readcontrol"
	"github.com/urpc/uio/internal/writecontrol"
	"io"
	"net"
	"os"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type optimizationSocket struct {
	Conn
	userdata any
	driver   any
	deadline time.Time
	queue    int
	mu       sync.Mutex
	written  []byte
	writes   int
	vectors  int
	segments int
	short    bool
}

func (s *optimizationSocket) LocalAddr() net.Addr {
	return nil
}

func (s *optimizationSocket) Userdata() any {
	return s.userdata
}

func (s *optimizationSocket) SetUserdata(v any) {
	s.userdata = v
}

func (s *optimizationSocket) driverData() any {
	return s.driver
}

func (s *optimizationSocket) setDriverData(v any) {
	s.driver = v
}

func (s *optimizationSocket) SetWakeDeadline(t time.Time) error {
	s.deadline = t
	return nil
}

func (s *optimizationSocket) OutboundBuffered() int {
	return s.queue
}

func (s *optimizationSocket) Wake() error {
	return nil
}

func (s *optimizationSocket) Close() error {
	return nil
}

func (s *optimizationSocket) Abort(error) error {
	return nil
}

func (s *optimizationSocket) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.writes++
	if s.short && len(p) > 0 {
		p = p[:len(p)-1]
	}
	s.written = append(s.written, p...)
	return len(p), nil
}

func (s *optimizationSocket) Writev(vec [][]byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.vectors++
	s.segments += len(vec)
	n := 0
	for _, p := range vec {
		n += len(p)
		s.written = append(s.written, p...)
	}
	if s.short && n > 0 {
		s.written = s.written[:len(s.written)-1]
		n--
	}
	return n, nil
}

func optimizationStack(handlers ...LayerHandler) (*stack, *optimizationSocket) {
	socket := &optimizationSocket{}
	layers := make([]Layer, len(handlers))
	for i, handler := range handlers {
		layers[i] = LayerFunc(func(LayerContext) LayerHandler {
			return handler
		})
	}
	(&stackDriver{layers: layers, top: FuncHandler{}}).OnOpen(socket)
	return stackOf(socket), socket
}

func TestLayerTimerActiveLifecycle(t *testing.T) {
	c, socket := optimizationStack(passThroughHandler{})
	past := time.Now().Add(-time.Second)
	future := time.Now().Add(time.Hour)
	for _, slot := range []int{0, 1} {
		t.Run(fmt.Sprintf("slot=%d", slot), func(t *testing.T) {
			if err := c.setTimer(slot, future, 0); err != nil {
				t.Fatal(err)
			}
			if !c.hasTimers.Load() || !socket.deadline.Equal(future) {
				t.Fatal("arming a timer did not reactivate its socket deadline")
			}
			if err := c.setTimer(slot, time.Time{}, 0); err != nil {
				t.Fatal(err)
			}
			if c.hasTimers.Load() || !socket.deadline.IsZero() || c.takeDue() != 0 {
				t.Fatal("cancelling the last timer left the timer path active")
			}
			if err := c.setTimer(slot, past, 0); err != nil {
				t.Fatal(err)
			}
			if due := c.takeDue(); due != 1<<uint(slot) {
				t.Fatalf("rearmed timer due mask = %b, want %b", due, 1<<uint(slot))
			}
			if c.hasTimers.Load() || !socket.deadline.IsZero() {
				t.Fatal("firing the last one-shot left the timer path active")
			}
			if due := c.takeDue(); due != 0 {
				t.Fatalf("one-shot fired again: %b", due)
			}
		})
	}
	if err := c.setTimer(0, past, time.Hour); err != nil {
		t.Fatal(err)
	}
	if due := c.takeDue(); due != 1 || !c.hasTimers.Load() || socket.deadline.IsZero() {
		t.Fatalf("repeating timer lost its next wake: due=%b active=%v deadline=%v", due, c.hasTimers.Load(), socket.deadline)
	}
	c.stopTimers()
	if c.hasTimers.Load() {
		t.Fatal("closed stack retained its active timer flag")
	}
}

type optimizationTransform struct {
	PassThrough
	calls int
	input []byte
}

func (h *optimizationTransform) OnWrite(ctx LayerContext, p []byte) (int, error) {
	h.calls++
	h.input = append(h.input[:0], p...)
	return ctx.Lower().Write(bytes.ToUpper(p))
}

func TestLayerWritevKeepsEmbeddedTransform(t *testing.T) {
	transform := &optimizationTransform{}
	c, socket := optimizationStack(passThroughHandler{}, transform, passThroughHandler{})
	if n, err := c.topConn().Writev([][]byte{[]byte("a"), []byte("bc")}); n != 3 || err != nil {
		t.Fatalf("Writev = %d, %v", n, err)
	}
	if transform.calls != 1 || string(transform.input) != "abc" || string(socket.written) != "ABC" {
		t.Fatalf("transform calls=%d input=%q wire=%q", transform.calls, transform.input, socket.written)
	}
}

func TestLayerBuiltinWritevKeepsSingleAndEmptyCalls(t *testing.T) {
	for _, vec := range [][][]byte{nil, {}, {[]byte("one")}} {
		c, socket := optimizationStack(passThroughHandler{}, passThroughHandler{})
		if _, err := c.topConn().Writev(vec); err != nil {
			t.Fatal(err)
		}
		if socket.writes != 1 || socket.vectors != 0 {
			t.Fatalf("vec=%q writes=%d vectors=%d, want one OnWrite", vec, socket.writes, socket.vectors)
		}
	}
}

func TestLayerBuiltinWritevAdmissionAndShortWrite(t *testing.T) {
	vec := [][]byte{[]byte("a"), []byte("bc")}
	c, socket := optimizationStack(passThroughHandler{}, passThroughHandler{})
	c.limit = 2
	if n, err := c.topConn().Writev(vec); n != 0 || !errors.Is(err, ErrOutboundOverflow) || socket.vectors != 0 {
		t.Fatalf("overflow Writev = %d, %v, vectors=%d", n, err, socket.vectors)
	}
	c.limit = 0
	socket.short = true
	if n, err := c.topConn().Writev(vec); n != 2 || !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("short Writev = %d, %v", n, err)
	}
	c.markClosed()
	if n, err := c.topConn().Writev(vec); n != 0 || !errors.Is(err, net.ErrClosed) || socket.vectors != 1 {
		t.Fatalf("closed Writev = %d, %v, vectors=%d", n, err, socket.vectors)
	}
}

type datagramLayer struct {
	handle  func(ctx LayerContext, chunk []byte)
	onOpen  func(ctx LayerContext)
	onTimer func(ctx LayerContext)
}

func (l *datagramLayer) CarriesPackets() {
}

func (l *datagramLayer) Attach(LayerContext) LayerHandler {
	return &datagramHandler{layer: l}
}

type datagramHandler struct{ layer *datagramLayer }

func (h *datagramHandler) OnOpen(ctx LayerContext) {
	if h.layer.onOpen != nil {
		h.layer.onOpen(ctx)
	}
	ctx.Open()
}

func (h *datagramHandler) OnRead(ctx LayerContext) error {
	if h.layer.handle != nil {
		h.layer.handle(ctx, ctx.Lower().PeekChunk())
	}
	return nil
}

func (h *datagramHandler) OnWrite(ctx LayerContext, p []byte) (int, error) {
	return ctx.Lower().Write(p)
}

func (h *datagramHandler) OnTimer(ctx LayerContext) error {
	if h.layer.onTimer != nil {
		h.layer.onTimer(ctx)
	}
	return nil
}

func (h *datagramHandler) OnClose(LayerContext, error) {
}

func servePacketLayer(t *testing.T, network string, handler FuncHandler, layers ...Layer) (*Events, string) {
	t.Helper()
	addrCh := make(chan string, 1)
	errCh := make(chan error, 1)
	start := handler.Start
	handler.Start = func(ev *Events) {
		if start != nil {
			start(ev)
		}
		ln, err := ev.Listen(network+"://127.0.0.1:0", WithLayers(layers...))
		if err != nil {
			errCh <- err
			return
		}
		addrCh <- ln.Addr().String()
	}
	events := withFuncs(&Events{Pollers: 2}, handler)
	done := make(chan error, 1)
	go func() {
		done <- events.Serve()
	}()
	t.Cleanup(func() {
		_ = events.Close(nil)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("Serve did not return")
		}
	})
	select {
	case addr := <-addrCh:
		return events, addr
	case err := <-errCh:
		t.Fatalf("Listen: %v", err)
	case err := <-done:
		t.Fatalf("Serve: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("server did not start")
	}
	return nil, ""
}

func TestPacketLayerTimerRuns(t *testing.T) {
	fired := make(chan struct{}, 1)
	layer := &datagramLayer{onOpen: func(ctx LayerContext) {
		_ = ctx.SetTimer(time.Now().Add(10 * time.Millisecond))
	}, onTimer: func(LayerContext) {
		select {
		case fired <- struct{}{}:
		default:
		}
	}}
	_, addr := servePacketLayer(t, "udp", FuncHandler{}, layer)
	client, err := net.Dial("udp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = client.Close()
	})
	if _, err := client.Write([]byte("tick")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-fired:
	case <-time.After(2 * time.Second):
		t.Fatal("the layer's timer never ran")
	}
}

type readControlProbe struct {
	PassThrough
	pauseOnOpen bool
	opened      chan LayerContext
	progress    chan int
	timers      chan struct{}
	sent        chan int
	readBytes   atomic.Int64
}

func newReadControlProbe() *readControlProbe {
	return &readControlProbe{opened: make(chan LayerContext, 1), progress: make(chan int, 8), timers: make(chan struct{}, 8), sent: make(chan int, 8)}
}

func (p *readControlProbe) OnOpen(ctx LayerContext) {
	if p.pauseOnOpen {
		if err := ctx.Lower().PauseRead(); err != nil {
			_ = ctx.Abort(err)
			return
		}
	}
	p.opened <- ctx
	ctx.Open()
}

func (p *readControlProbe) OnRead(ctx LayerContext) error {
	n := ctx.Lower().InboundBuffered()
	if err := p.PassThrough.OnRead(ctx); err != nil {
		return err
	}
	if n > 0 {
		p.readBytes.Add(int64(n))
		select {
		case p.progress <- n:
		default:
		}
	}
	return nil
}

func (p *readControlProbe) OnTimer(LayerContext) error {
	select {
	case p.timers <- struct{}{}:
	default:
	}
	return nil
}

func TestReadControlLayerPauseIsScopedAndKeepsControlWorkAlive(t *testing.T) {
	lower, upper := newReadControlProbe(), newReadControlProbe()
	upper.pauseOnOpen = true
	reads := make(chan string, 8)
	sentNotice := make(chan int, 8)
	ev := &Events{Pollers: 1, Handler: FuncHandler{Read: func(c Conn) error {
		if n := c.InboundBuffered(); n != 0 {
			p := make([]byte, n)
			_, _ = c.Read(p)
			reads <- string(p)
		}
		return nil
	}}}
	client, server := readControlServe(t, ev, LayerFunc(func(LayerContext) LayerHandler {
		return lower
	}), LayerFunc(func(LayerContext) LayerHandler {
		return upper
	}))
	server.(*layerNode).connWriteControl().Watch(func(n int) {
		if n > 0 {
			select {
			case sentNotice <- n:
			default:
			}
		}
	})
	lowerCtx := readControlAwait(t, lower.opened, "lower layer open")
	upperCtx := readControlAwait(t, upper.opened, "upper layer open")
	if !upperCtx.Lower().ReadPaused() || lowerCtx.Lower().ReadPaused() || server.ReadPaused() {
		t.Fatal("the upper layer pause escaped its input boundary")
	}
	if _, err := client.Write([]byte("layered")); err != nil {
		t.Fatal(err)
	}
	readControlAwait(t, lower.progress, "lower layer input while upper is paused")
	if upper.readBytes.Load() != 0 {
		t.Fatal("the paused upper layer consumed input")
	}
	if err := lowerCtx.SetTimer(time.Now().Add(10 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if err := upperCtx.SetTimer(time.Now().Add(10 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	readControlAwait(t, lower.timers, "lower layer timer")
	readControlAwait(t, upper.timers, "paused upper layer timer")
	if _, err := server.Write([]byte("reply")); err != nil {
		t.Fatal(err)
	}
	if err := server.Flush(); err != nil {
		t.Fatal(err)
	}
	_ = client.SetReadDeadline(time.Now().Add(5 * time.Second))
	wire := make([]byte, len("reply"))
	if _, err := io.ReadFull(client, wire); err != nil || string(wire) != "reply" {
		t.Fatalf("write through the paused layer = %q, %v", wire, err)
	}
	readControlAwait(t, sentNotice, "outbound notification while paused")
	if err := upperCtx.Wake(); err != nil {
		t.Fatal(err)
	}
	readControlQuiet(t, reads, "input delivery through a paused layer")
	if err := upperCtx.Lower().ResumeRead(); err != nil {
		t.Fatal(err)
	}
	if got := readControlAwait(t, reads, "layer-buffered input after resume"); got != "layered" {
		t.Fatalf("input = %q, want layered", got)
	}
}

func TestReadControlPausedLayerRetainsInputAfterBothSocketHalvesEnd(t *testing.T) {
	for _, halfOpen := range []bool{false, true} {
		name := "default"
		if halfOpen {
			name = "half-open"
		}
		t.Run(name, func(t *testing.T) {
			paused := make(chan error, 1)
			reads := make(chan string, 8)
			closed := make(chan error, 1)
			ev := &Events{Pollers: 1, AllowHalfOpen: halfOpen, Handler: FuncHandler{Open: func(c Conn) {
				err := c.PauseRead()
				if err == nil {
					err = c.CloseWrite()
				}
				paused <- err
			}, Read: func(c Conn) error {
				if n := c.InboundBuffered(); n != 0 {
					p := make([]byte, n)
					_, _ = c.Read(p)
					reads <- string(p)
				}
				return nil
			}, Close: func(_ Conn, err error) {
				closed <- err
			}}}
			client, server := readControlServe(t, ev, PassThroughLayer, PassThroughLayer)
			if err := readControlAwait(t, paused, "pause and CloseWrite"); err != nil {
				t.Fatal(err)
			}
			_ = client.SetReadDeadline(time.Now().Add(5 * time.Second))
			if _, err := io.ReadAll(client); err != nil {
				t.Fatalf("waiting for the server's writing half to end: %v", err)
			}
			if _, err := client.Write([]byte("held-after-local-FIN")); err != nil {
				t.Fatal(err)
			}
			if err := client.(interface{ CloseWrite() error }).CloseWrite(); err != nil {
				t.Fatal(err)
			}
			readControlQuiet(t, closed, "connection close before its paused upper consumer drained")
			if err := server.ResumeRead(); err != nil {
				t.Fatal(err)
			}
			if got := readControlAwait(t, reads, "held input after both socket halves ended"); got != "held-after-local-FIN" {
				t.Fatalf("held input = %q", got)
			}
			readControlAwait(t, closed, "connection close after its upper consumer drained")
		})
	}
}

func TestReadControlLayerCapacityHoldDoesNotPauseItsConsumer(t *testing.T) {
	const limit, total = 1024, 8 << 10
	full := make(chan bool, 1)
	drained := make(chan []byte, 1)
	var consume atomic.Bool
	var announced bool
	var received bytes.Buffer
	var complete bool
	ev := &Events{Pollers: 1, ReadBufferSize: 512, MaxInboundBuffered: limit, Handler: FuncHandler{Read: func(c Conn) error {
		if !consume.Load() {
			if c.InboundBuffered() >= limit && !announced {
				announced = true
				full <- c.ReadPaused()
			}
			return nil
		}
		if _, err := c.WriteTo(&received); err != nil {
			return err
		}
		if received.Len() == total && !complete {
			complete = true
			drained <- append([]byte(nil), received.Bytes()...)
		}
		return nil
	}}}
	client, server := readControlServe(t, ev, PassThroughLayer, PassThroughLayer)
	want := bytes.Repeat([]byte("abcdefgh"), total/8)
	if _, err := client.Write(want); err != nil {
		t.Fatal(err)
	}
	if readControlAwait(t, full, "a full layer delivery") {
		t.Fatal("a full layer delivery paused its consumer")
	}
	if err := server.PauseRead(); err != nil {
		t.Fatal(err)
	}
	consume.Store(true)
	if err := server.ResumeRead(); err != nil {
		t.Fatal(err)
	}
	if got := readControlAwait(t, drained, "layer capacity released by the resumed consumer"); !bytes.Equal(got, want) {
		t.Fatalf("input changed: got %d bytes, want %d", len(got), len(want))
	}
}

const layerWait = 5 * time.Second

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
	lower := ctx.Lower()
	chunk := lower.PeekChunk()
	if len(chunk) == 0 {
		return nil
	}
	out := ctx.Upper().Reserve(len(chunk))[:len(chunk)]
	for i, b := range chunk {
		out[i] = m.f(b, m.key)
	}
	ctx.Upper().Commit(len(chunk))
	_, err := lower.Discard(len(chunk))
	return err
}

func (m *mapLayer) OnWrite(ctx LayerContext, p []byte) (int, error) {
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

type layerEcho struct {
	opened chan Conn
	read   chan []byte
}

func newLayerEcho() *layerEcho {
	return &layerEcho{opened: make(chan Conn, 4), read: make(chan []byte, 16)}
}

func (e *layerEcho) OnOpen(c Conn) {
	e.opened <- c
}

func (e *layerEcho) OnRead(c Conn) error {
	payload := append([]byte(nil), c.PeekChunk()...)
	_, _ = c.Discard(len(payload))
	e.read <- payload
	_, err := c.Write(payload)
	return err
}

func (e *layerEcho) OnTimer(Conn) error {
	return nil
}

func (e *layerEcho) OnClose(Conn, error) {
}

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

func serveLayered(t *testing.T, layers []Layer, setup func(*Events)) (string, *layerEcho) {
	t.Helper()
	echo := newLayerEcho()
	started := make(chan struct{})
	events := &Events{Pollers: 2, Handler: echo}
	events.Hooks = FuncHandler{Start: func(*Events) {
		close(started)
	}}
	if setup != nil {
		setup(events)
	}
	done := make(chan error, 1)
	go func() {
		done <- events.Serve()
	}()
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
	t.Cleanup(func() {
		_ = listener.Close()
	})
	return listener.Addr().String(), echo
}

func TestUseLayersAcceptedConnections(t *testing.T) {
	const (
		xorKey byte = 0x24
		addKey byte = 0x03
		sent        = byte(0x5a)
	)
	addr, echo := serveLayered(t, []Layer{mapBytes(xorKey, func(b, k byte) byte {
		return b ^ k
	}), mapBytes(addKey, func(b, k byte) byte {
		return b + k
	})}, func(ev *Events) {
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
	events.Hooks = FuncHandler{Start: func(*Events) {
		close(started)
	}}
	events.Handler = FuncHandler{Open: func(Conn) {
	}, Read: func(c Conn) error {
		_, _ = c.Discard(c.InboundBuffered())
		return nil
	}, Close: func(Conn, error) {
	}}
	if setup != nil {
		setup(events)
	}
	done := make(chan error, 1)
	go func() {
		done <- events.Serve()
	}()
	t.Cleanup(func() {
		_ = events.Close(nil)
		<-done
	})
	<-started
	_ = serverAddr
	return &layeredEvents{Events: events, layers: layers}
}

func TestUseLayersDialedConnections(t *testing.T) {
	const key byte = 0x5a
	addr, echo := serveLayered(t, []Layer{mapBytes(key, xor)}, func(ev *Events) {
	})
	events := dialLayered(t, addr, []Layer{mapBytes(key, xor)}, func(ev *Events) {
	})
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
	if got := string(layerWaitFor(t, echo.read, "server read")); got != "through" {
		t.Fatalf("server read %q, want through", got)
	}
}

func xor(b, k byte) byte {
	return b ^ k
}

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
	events := dialLayered(t, silent.Addr().String(), []Layer{LayerFunc(func(LayerContext) LayerHandler {
		return neverOpens{}
	})}, func(ev *Events) {
	})
	events.Handler = FuncHandler{Open: func(Conn) {
		atomic.AddInt32(&calls, 1)
	}, Read: func(Conn) error {
		return nil
	}, Close: func(Conn, error) {
		atomic.AddInt32(&calls, 1)
	}}
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

type refuseLayer struct {
	PassThrough
	cause error
}

func (r refuseLayer) OnOpen(ctx LayerContext) {
	_ = ctx.Abort(r.cause)
}

type neverOpens struct{ PassThrough }

func (neverOpens) OnOpen(LayerContext) {
}

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
	events.Hooks = FuncHandler{Start: func(*Events) {
		close(started)
	}}
	events.Handler = FuncHandler{Open: func(c Conn) {
		_ = c.SetWakeDeadline(time.Now().Add(80 * time.Millisecond))
	}, Read: func(c Conn) error {
		if len(c.PeekChunk()) == 0 {
			handlerFired.Add(1)
		}
		return nil
	}, Close: func(Conn, error) {
	}}
	done := make(chan error, 1)
	go func() {
		done <- events.Serve()
	}()
	t.Cleanup(func() {
		_ = events.Close(nil)
		<-done
	})
	<-started
	addr, _ := serveLayered(t, []Layer{passThroughLayer{}, passThroughLayer{}}, func(ev *Events) {
	})
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
		t.Fatalf("lower fired %d, upper %d, Handler %d: want every timer to fire, the layers' once", lowerFired.Load(), upperFired.Load(), handlerFired.Load())
	}
}

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

type passThroughLayer struct{}

func (passThroughLayer) Attach(LayerContext) LayerHandler {
	return passThroughHandler{}
}

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

type sayGoodbye struct{ PassThrough }

func (sayGoodbye) OnClosing(ctx LayerContext, _ error) {
	_, _ = ctx.Lower().Write([]byte("bye"))
}

func TestWritevThroughALayerIsWhole(t *testing.T) {
	const writers, each = 2, 2000
	addr, echo := serveLayered(t, nil, nil)
	events := dialLayered(t, addr, []Layer{passThroughLayer{}}, func(ev *Events) {
	})
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

func TestDialGivingUpMidOpenRunsNoCallbacks(t *testing.T) {
	addr, _ := serveLayered(t, nil, nil)
	var calls atomic.Int32
	events := dialLayered(t, addr, []Layer{LayerFunc(func(LayerContext) LayerHandler {
		return slowOpen{}
	})}, func(ev *Events) {
		ev.Handler = FuncHandler{Open: func(Conn) {
			calls.Add(1)
		}, Read: func(Conn) error {
			return nil
		}, Close: func(Conn, error) {
			calls.Add(1)
		}}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := events.DialContext(ctx, "tcp://"+addr); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("DialContext = %v, want the deadline", err)
	}
	time.Sleep(200 * time.Millisecond)
	if n := calls.Load(); n != 0 {
		t.Fatalf("a dial that gave up ran %d Handler callbacks", n)
	}
}

type slowOpen struct{ PassThrough }

func (slowOpen) OnOpen(ctx LayerContext) {
	time.Sleep(100 * time.Millisecond)
	ctx.Open()
}

func TestLingeringCloseDiscardsWhatArrives(t *testing.T) {
	saved := lingerTimeout
	lingerTimeout = time.Second
	t.Cleanup(func() {
		lingerTimeout = saved
	})
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
		ev.Handler = FuncHandler{Open: func(Conn) {
		}, Read: func(Conn) error {
			return nil
		}, Close: func(_ Conn, err error) {
			signal(closed, err)
		}}
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
		ev.Handler = FuncHandler{Open: func(Conn) {
		}, Read: func(c Conn) error {
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
		}, Close: func(Conn, error) {
		}}
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

func TestCloseWriteNeedsTheLayersSupport(t *testing.T) {
	addr, _ := serveLayered(t, nil, nil)
	events := dialLayered(t, addr, []Layer{passThroughLayer{}, LayerFunc(func(LayerContext) LayerHandler {
		return &framing{}
	})}, func(ev *Events) {
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

type framing struct{}

func (*framing) OnOpen(ctx LayerContext) {
	ctx.Open()
}

func (*framing) OnRead(LayerContext) error {
	return nil
}

func (*framing) OnWrite(ctx LayerContext, p []byte) (int, error) {
	return ctx.Lower().Write(p)
}

func (*framing) OnTimer(LayerContext) error {
	return nil
}

func (*framing) OnClose(LayerContext, error) {
}

func TestFindLayerAndHeldOutput(t *testing.T) {
	addr, _ := serveLayered(t, nil, nil)
	holder := &holdsOutput{}
	events := dialLayered(t, addr, []Layer{LayerFunc(func(LayerContext) LayerHandler {
		return holder
	}), passThroughLayer{}}, func(ev *Events) {
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

func (h *holdsOutput) OutboundHeld(LayerContext) int {
	return int(h.held.Load())
}

func (h *holdsOutput) CloseWrite(ctx LayerContext) error {
	return ctx.Lower().CloseWrite()
}

func TestHeldOutputIsFlushedAtTheTurnsEnd(t *testing.T) {
	addr, echo := serveLayered(t, nil, nil)
	events := dialLayered(t, addr, []Layer{LayerFunc(func(LayerContext) LayerHandler {
		return &holdUntilFlush{}
	}), LayerFunc(func(LayerContext) LayerHandler {
		return greeter{}
	})}, func(ev *Events) {
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

type holdUntilFlush struct {
	PassThrough
	mu      sync.Mutex
	held    []byte
	counted atomic.Int64
} // held, and what a drain is writing below

func (h *holdUntilFlush) OnWrite(ctx LayerContext, p []byte) (int, error) {
	h.mu.Lock()
	h.held = append(h.held, p...)
	h.counted.Add(int64(len(p)))
	h.mu.Unlock()
	_ = ctx.Wake()
	return len(p), nil
}

func (h *holdUntilFlush) OutboundHeld(LayerContext) int {
	return int(h.counted.Load())
}

func (h *holdUntilFlush) CloseWrite(ctx LayerContext) error {
	return ctx.Lower().CloseWrite()
}

func (h *holdUntilFlush) OnRead(ctx LayerContext) error {
	if err := h.PassThrough.OnRead(ctx); err != nil {
		return err
	}
	return h.drain(ctx)
}

func (h *holdUntilFlush) OnClosing(ctx LayerContext, _ error) {
	_ = h.drain(ctx)
}

func (h *holdUntilFlush) drain(ctx LayerContext) error {
	h.mu.Lock()
	held := h.held
	h.held = nil
	h.mu.Unlock()
	if len(held) == 0 {
		return nil
	}
	_, err := ctx.Lower().Write(held)
	h.counted.Add(-int64(len(held)))
	return err
}

type greeter struct{ PassThrough }

func (greeter) OnOpen(ctx LayerContext) {
	_, _ = ctx.Lower().Write([]byte("hello"))
	_ = ctx.Wake()
	ctx.Open()
}

func TestLayersRefusePacketConnections(t *testing.T) {
	events := dialLayered(t, "", []Layer{passThroughLayer{}}, func(ev *Events) {
	})
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

func TestTooManyLayers(t *testing.T) {
	events := &Events{Pollers: 1, Handler: FuncHandler{}}
	started := make(chan struct{})
	events.Hooks = FuncHandler{Start: func(*Events) {
		close(started)
	}}
	go func() {
		_ = events.Serve()
	}()
	t.Cleanup(func() {
		_ = events.Close(nil)
	})
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

func TestTimerArmedBeforeOpenFiresAfter(t *testing.T) {
	addr, _ := serveLayered(t, nil, nil)
	fired := make(chan time.Time, 1)
	var turns atomic.Int64
	events := dialLayered(t, addr, []Layer{LayerFunc(func(LayerContext) LayerHandler {
		return &openLate{}
	}), LayerFunc(func(ctx LayerContext) LayerHandler {
		_ = ctx.SetTimer(time.Now().Add(10 * time.Millisecond))
		return &earlyTimer{fired: fired}
	})}, func(ev *Events) {
		ev.Handler = FuncHandler{Open: func(Conn) {
		}, Read: func(Conn) error {
			turns.Add(1)
			return nil
		}, Close: func(Conn, error) {
		}}
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

type openLate struct{ PassThrough }

func (*openLate) OnOpen(ctx LayerContext) {
	_ = ctx.SetTimer(time.Now().Add(150 * time.Millisecond))
}

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
	events := dialLayered(t, listener.Addr().String(), []Layer{LayerFunc(func(LayerContext) LayerHandler {
		return sayGoodbye{}
	}), LayerFunc(func(LayerContext) LayerHandler {
		return sayGoodbye{}
	})}, func(ev *Events) {
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
		go func() {
			defer wg.Done()
			_ = conn.Close()
		}()
	}
	wg.Wait()
	if all := string(layerWaitFor(t, got, "the goodbyes")); all != "byebye" {
		t.Fatalf("the peer read %q, want each layer's goodbye once", all)
	}
}

type countingHold struct {
	holdUntilFlush
	aborting     atomic.Bool
	flushedAfter atomic.Int32
}

func (h *countingHold) OnRead(ctx LayerContext) error {
	if h.aborting.Load() && h.OutboundHeld(ctx) > 0 {
		h.flushedAfter.Add(1)
	}
	return h.holdUntilFlush.OnRead(ctx)
}

func TestCloseAsTheStackOpens(t *testing.T) {
	t.Run("abort in OnOpen", func(t *testing.T) {
		addr, _ := serveLayered(t, nil, nil)
		holder := &countingHold{}
		closed := make(chan error, 1)
		events := dialLayered(t, addr, []Layer{LayerFunc(func(LayerContext) LayerHandler {
			return holder
		})}, func(ev *Events) {
			ev.Handler = FuncHandler{Open: func(c Conn) {
				_, _ = c.Write([]byte("never sent"))
				holder.aborting.Store(true)
				_ = c.Abort(errors.New("abort"))
			}, Read: func(Conn) error {
				return nil
			}, Close: func(_ Conn, err error) {
				signal(closed, err)
			}}
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
		events := dialLayered(t, addr, []Layer{LayerFunc(func(LayerContext) LayerHandler {
			return closeThenOpen{}
		}), LayerFunc(func(LayerContext) LayerHandler {
			return &countOpens{n: &opened}
		})}, func(ev *Events) {
			ev.Handler = FuncHandler{Open: func(Conn) {
			}, Read: func(Conn) error {
				return nil
			}, Close: func(Conn, error) {
			}}
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

type closeThenOpen struct{ PassThrough }

func (closeThenOpen) OnOpen(ctx LayerContext) {
	_ = ctx.Abort(errors.New("refused"))
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
	events := dialLayered(t, listener.Addr().String(), []Layer{LayerFunc(func(LayerContext) LayerHandler {
		return closer
	})}, func(ev *Events) {
		ev.Handler = FuncHandler{Open: func(Conn) {
		}, Read: func(Conn) error {
			return nil
		}, Close: func(_ Conn, err error) {
			signal(closed, err)
		}}
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

type byteThenClose struct {
	PassThrough
	closed atomic.Bool
	after  atomic.Int32
}

func (b *byteThenClose) OnRead(ctx LayerContext) error {
	if b.closed.Load() {
		b.after.Add(1)
	}
	lower := ctx.Lower()
	if len(lower.PeekChunk()) == 0 {
		return nil
	}
	_, _ = lower.Discard(1)
	b.closed.Store(true)
	return ctx.Close()
}

func TestAdoptReturnsTheTopConnection(t *testing.T) {
	addr, echo := serveLayered(t, nil, nil)
	opened := make(chan Conn, 1)
	events := dialLayered(t, addr, []Layer{LayerFunc(func(LayerContext) LayerHandler {
		return upperCase{}
	})}, func(ev *Events) {
		ev.Handler = FuncHandler{Open: func(c Conn) {
			signal(opened, c)
		}, Read: func(c Conn) error {
			_, _ = c.Discard(-1)
			return nil
		}, Close: func(Conn, error) {
		}}
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
	refusing := dialLayered(t, addr, []Layer{LayerFunc(func(LayerContext) LayerHandler {
		return refuseLayer{cause: errors.New("refused")}
	})}, func(ev *Events) {
	})
	raw, err = net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := refusing.Adopt(raw); err == nil || err.Error() != "refused" {
		t.Fatalf("Adopt through a refusing layer = %v, want its refusal", err)
	}
}

type upperCase struct{ PassThrough }

func (upperCase) OnWrite(ctx LayerContext, p []byte) (int, error) {
	if _, err := ctx.Lower().Write(bytes.ToUpper(p)); err != nil {
		return 0, err
	}
	return len(p), nil
}

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
		t.Cleanup(func() {
			_ = listener.Close()
		})
		got := make(chan outcome, 1)
		go func() {
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
	holder := func() Layer {
		return LayerFunc(func(LayerContext) LayerHandler {
			return &holdUntilFlush{}
		})
	}
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
					handler := FuncHandler{Close: func(Conn, error) {
					}}
					if where == "OnOpen" {
						handler.Open = writeAndEnd
					} else {
						handler.Open = func(c Conn) {
							_ = c.Wake()
						}
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
		events := dialLayered(t, addr, []Layer{holder()}, func(ev *Events) {
		})
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
		if err := conn.CloseWrite(); err != nil && !errors.Is(err, net.ErrClosed) {
			t.Fatalf("CloseWrite again = %v, want nothing", err)
		}
	})
	t.Run("a holder that sends later", func(t *testing.T) {
		addr, got := peerReads(t)
		window := &sendWindow{}
		events := dialLayered(t, addr, []Layer{LayerFunc(func(LayerContext) LayerHandler {
			return window
		})}, func(ev *Events) {
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

type sendWindow struct {
	PassThrough
	mu      sync.Mutex
	ctx     LayerContext
	held    []byte
	counted atomic.Int64
}

func (w *sendWindow) CloseWrite(ctx LayerContext) error {
	return ctx.Lower().CloseWrite()
}

func (w *sendWindow) OnOpen(ctx LayerContext) {
	w.mu.Lock()
	w.ctx = ctx
	w.mu.Unlock()
	ctx.Open()
}

func (w *sendWindow) OnWrite(_ LayerContext, p []byte) (int, error) {
	w.mu.Lock()
	w.held = append(w.held, p...)
	w.counted.Add(int64(len(p)))
	w.mu.Unlock()
	return len(p), nil
}

func (w *sendWindow) OutboundHeld(LayerContext) int {
	return int(w.counted.Load())
}

func (w *sendWindow) release() {
	w.mu.Lock()
	held, ctx := w.held, w.ctx
	w.held = nil
	w.mu.Unlock()
	_, _ = ctx.Lower().Write(held)
	w.counted.Add(-int64(len(held)))
	_ = ctx.Wake()
}

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
	return ctx.Lower().Write(p)
}

func (l *endedLayer) OutboundHeld(LayerContext) int {
	l.lateCall()
	return 0
}

func (l *endedLayer) CloseWrite(ctx LayerContext) error {
	l.lateCall()
	return ctx.Lower().CloseWrite()
}

func (l *endedLayer) OnClosing(LayerContext, error) {
	l.lateCall()
}

func (l *endedLayer) OnClose(LayerContext, error) {
	l.ended.Store(true)
}

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
				chain[i] = LayerFunc(func(LayerContext) LayerHandler {
					return ended
				})
			}
			events := dialLayered(t, addr, chain, func(ev *Events) {
				ev.Handler = FuncHandler{Close: func(Conn, error) {
					close(closed)
				}}
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
			calls["CloseWith"] = conn.Abort(errors.New("late"))
			calls["Close"] = conn.Close()
			_ = conn.OutboundBuffered()
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

type peerAnswersGoodbye struct{ PassThrough }

func (peerAnswersGoodbye) OnClosing(ctx LayerContext, _ error) {
	_ = ctx.(layerCtx).l.stack.socket.Abort(io.EOF)
}

func TestCloseSucceedsWhenTheGoodbyeIsAnswered(t *testing.T) {
	addr, _ := serveLayered(t, nil, nil)
	events := dialLayered(t, addr, []Layer{LayerFunc(func(LayerContext) LayerHandler {
		return peerAnswersGoodbye{}
	})}, func(ev *Events) {
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

type splitWriter struct {
	PassThrough
	entered chan struct{}
	proceed chan struct{}
}

func (w *splitWriter) CloseWrite(ctx LayerContext) error {
	return ctx.Lower().CloseWrite()
}

func (s *splitWriter) OnWrite(ctx LayerContext, p []byte) (int, error) {
	n, err := ctx.Lower().Write(p[:2])
	if err != nil {
		return n, err
	}
	s.entered <- struct{}{}
	<-s.proceed
	m, err := ctx.Lower().Write(p[2:])
	return n + m, err
}

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
		events := dialLayered(t, addr, []Layer{LayerFunc(func(LayerContext) LayerHandler {
			return split
		})}, func(ev *Events) {
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
}

type wakeCounter struct {
	Conn
	wakes atomic.Int32
}

func (w *wakeCounter) Wake() error {
	w.wakes.Add(1)
	return nil
}

func TestRequestedTurnsCoalesce(t *testing.T) {
	socket := &wakeCounter{}
	c := &stack{socket: socket}
	for range 1000 {
		c.requestTurn()
	}
	if n := socket.wakes.Load(); n != 1 {
		t.Fatalf("1000 requests before the turn asked %d wakes, want 1", n)
	}
	_ = c.turn()
	c.requestTurn()
	if n := socket.wakes.Load(); n != 2 {
		t.Fatalf("a request after the turn began asked %d wakes in all, want 2", n)
	}
}

func TestDialerConcurrentDials(t *testing.T) {
	addr, _ := serveLayered(t, []Layer{PassThroughLayer}, func(ev *Events) {
		ev.Handler = FuncHandler{Open: func(Conn) {
		}, Read: func(c Conn) error {
			payload := c.PeekChunk()
			_, _ = c.Discard(len(payload))
			_, err := c.Write(payload)
			return err
		}, Close: func(Conn, error) {
		}}
	})
	client := dialLayered(t, addr, nil, func(ev *Events) {
	})
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
				var opts []DialOption
				if worker%2 == 1 {
					opts = append(opts, WithLayers(PassThroughLayer))
				}
				conn, err := dialer.DialContext(ctx, "tcp://"+addr, opts...)
				if err == nil {
					_, err = conn.Write([]byte{ // half take the Dialer's chain, half their own
						'x'})
					if closeErr := conn.Close(); err == nil {
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

type stashedClose struct {
	PassThrough
	ctx     chan LayerContext
	goodbye atomic.Int32
}

func (l *stashedClose) OnOpen(ctx LayerContext) {
	l.ctx <- ctx
	ctx.Open()
}

func (l *stashedClose) OnClosing(LayerContext, error) {
	l.goodbye.Add(1)
}

func TestCloseWithCutsALayeredDrain(t *testing.T) {
	saved := lingerTimeout
	lingerTimeout = 10 * time.Second
	t.Cleanup(func() {
		lingerTimeout = saved
	})
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = listener.Close()
	})
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
			_ = raw.SetReadBuffer(4 << 10)
		}
		<-hold
	}()
	closed := make(chan error, 1)
	client := dialLayered(t, listener.Addr().String(), []Layer{PassThroughLayer}, func(ev *Events) {
		ev.Handler = FuncHandler{Open: func(Conn) {
		}, Read: func(c Conn) error {
			_, _ = c.Discard(c.InboundBuffered())
			return nil
		}, Close: func(_ Conn, err error) {
			signal(closed, err)
		}}
	})
	ctx, cancel := context.WithTimeout(context.Background(), layerWait)
	defer cancel()
	conn, err := client.DialContext(ctx, "tcp://"+listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write(make([]byte, 32<<20)); err != nil {
		t.Fatal(err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if conn.OutboundBuffered() == 0 {
		t.Skip("the platform drained the write: no stalled queue to cut")
	}
	began := time.Now()
	if err := conn.Abort(io.EOF); err != nil {
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

type goodbyeAborts struct {
	PassThrough
	abort error
	ran   atomic.Int32
}

func (g *goodbyeAborts) OnClosing(ctx LayerContext, _ error) {
	g.ran.Add(1)
	_ = ctx.Abort(g.abort)
}

type goodbyeCounts struct {
	PassThrough
	ran atomic.Int32
}

func (g *goodbyeCounts) OnClosing(LayerContext, error) {
	g.ran.Add(1)
}

func TestAbortInGoodbyeStopsTheWalk(t *testing.T) {
	abortErr := errors.New("no graceful goodbye")
	top := &goodbyeAborts{abort: abortErr}
	bottom := &goodbyeCounts{}
	closed := make(chan error, 1)
	attached := make(chan Conn, 1)
	addr, _ := serveLayered(t, []Layer{LayerFunc(func(LayerContext) LayerHandler {
		return bottom
	}), LayerFunc(func(LayerContext) LayerHandler {
		return top
	})}, func(ev *Events) {
		ev.Handler = FuncHandler{Open: func(c Conn) {
			signal(attached, c)
		}, Read: func(c Conn) error {
			_, _ = c.Discard(c.InboundBuffered())
			return nil
		}, Close: func(_ Conn, err error) {
			signal(closed, err)
		}}
	})
	client := dialLayered(t, addr, nil, func(ev *Events) {
	})
	ctx, cancel := context.WithTimeout(context.Background(), layerWait)
	defer cancel()
	conn, err := client.DialContext(ctx, "tcp://"+addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	attachedConn := layerWaitFor(t, attached, "the server handler")
	if err := attachedConn.Close(); err != nil {
		t.Fatal(err)
	}
	layerWaitFor(t, closed, "the close")
	if n := top.ran.Load(); n != 1 {
		t.Fatalf("the top layer's goodbye ran %d times", n)
	}
	if n := bottom.ran.Load(); n != 0 {
		t.Fatalf("%d goodbyes below an abort that ended the walk", n)
	}
}

func TestCloseYieldLeavesTheWalkIntact(t *testing.T) {
	bottom := &stashedClose{ctx: make(chan LayerContext, 1)}
	blocked := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() {
		releaseOnce.Do(func() {
			close(release)
		})
	}
	defer unblock()
	server := make(chan Conn, 1)
	closed := make(chan error, 1)
	addr, _ := serveLayered(t, []Layer{LayerFunc(func(LayerContext) LayerHandler {
		return bottom
	}), PassThroughLayer}, func(ev *Events) {
		ev.Handler = FuncHandler{Open: func(c Conn) {
			signal(server, c)
		}, Read: func(c Conn) error {
			_, _ = c.Discard(c.InboundBuffered())
			select {
			case <-blocked:
			default:
				close(blocked)
				<-release
			}
			return nil
		}, Close: func(_ Conn, err error) {
			signal(closed, err)
		}}
	})
	client := dialLayered(t, addr, nil, func(ev *Events) {
	})
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
	if err := srv.Close(); err != nil {
		t.Fatal(err)
	}
	if err := layerCtx.Close(); err != nil {
		t.Fatal(err)
	}
	unblock()
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

func TestServeTwiceFails(t *testing.T) {
	started := make(chan struct{})
	events := &Events{Pollers: 1, Handler: FuncHandler{}}
	events.Hooks = FuncHandler{Start: func(*Events) {
		close(started)
	}}
	done := make(chan error, 1)
	go func() {
		done <- events.Serve()
	}()
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

type contractTransport struct {
	optimizationSocket
	SocketConn
	options      []string
	local        net.Addr
	remote       net.Addr
	state        ConnState
	writeError   error
	reserveError error
	wakeError    error
	closeCause   error
	graceful     bool
	wakes        int
	input        []byte
	reserved     []byte
}

func (c *contractTransport) LocalAddr() net.Addr {
	return c.local
}

func (c *contractTransport) RemoteAddr() net.Addr {
	return c.remote
}

func (c *contractTransport) ReadPaused() bool {
	return false
}

func (c *contractTransport) State() ConnState {
	return c.state
}

func (c *contractTransport) Wake() error {
	c.wakes++
	return c.wakeError
}

func (c *contractTransport) Write(p []byte) (int, error) {
	if c.writeError != nil {
		return 0, c.writeError
	}
	return c.optimizationSocket.Write(p)
}

func (c *contractTransport) WriteString(p string) (int, error) {
	return c.Write([]byte(p))
}

func (c *contractTransport) WriteByte(b byte) error {
	_, err := c.Write([]byte{b})
	return err
}

func (c *contractTransport) Abort(cause error) error {
	c.closeCause = cause
	c.state |= StateClosing | StateClosed
	return nil
}

func (c *contractTransport) Close() error {
	c.graceful = true
	return c.Abort(nil)
}

func (c *contractTransport) CloseWrite() error {
	c.state |= StateWriteEnded
	return nil
}

func (c *contractTransport) InboundBuffered() int {
	return len(c.input)
}

func (c *contractTransport) PeekChunk() []byte {
	return c.input
}

func (c *contractTransport) Discard(n int) (int, error) {
	if n < 0 || n > len(c.input) {
		n = len(c.input)
	}
	c.input = c.input[n:]
	return n, nil
}

func (c *contractTransport) SetNoDelay(b bool) error {
	c.options = append(c.options, fmt.Sprint("delay:", b))
	return nil
}

func (c *contractTransport) SetKeepAlive(b bool) error {
	c.options = append(c.options, fmt.Sprint("keep:", b))
	return nil
}

func (c *contractTransport) SetKeepAlivePeriod(n int) error {
	c.options = append(c.options, fmt.Sprint("period:", n))
	return nil
}

func (c *contractTransport) SetReadBuffer(n int) error {
	c.options = append(c.options, fmt.Sprint("read:", n))
	return nil
}

func (c *contractTransport) SetWriteBuffer(n int) error {
	c.options = append(c.options, fmt.Sprint("write:", n))
	return nil
}

func (c *contractTransport) SetDeadline(v time.Time) error {
	c.options = append(c.options, fmt.Sprint("both:", v.Unix()))
	return nil
}

func (c *contractTransport) SetReadDeadline(v time.Time) error {
	c.options = append(c.options, fmt.Sprint("rd:", v.Unix()))
	return nil
}

func (c *contractTransport) SetWriteDeadline(v time.Time) error {
	c.options = append(c.options, fmt.Sprint("wd:", v.Unix()))
	return nil
}

func (c *contractTransport) Control(fn func(uintptr)) error {
	fn(42)
	return nil
}

func contractStack(transport *contractTransport, top Handler, handlers ...LayerHandler) *stack {
	layers := make([]Layer, len(handlers))
	for i, handler := range handlers {
		layers[i] = LayerFunc(func(LayerContext) LayerHandler {
			return handler
		})
	}
	(&stackDriver{layers: layers, top: top}).OnOpen(transport)
	return stackOf(transport)
}

func TestLayerTransportOptionsAndInputStayAtTheirBoundary(t *testing.T) {
	transport := &contractTransport{local: &net.TCPAddr{Port: 12}, remote: &net.TCPAddr{Port: 34}}
	c := contractStack(transport, FuncHandler{}, passThroughHandler{}, passThroughHandler{})
	top := c.topConn().(*layerNode)
	if top.RemoteAddr().String() != transport.remote.String() || top.LocalAddr().String() != transport.local.String() {
		t.Fatal("layer connection changed transport addresses")
	}
	sc, ok := SocketOf(top)
	if !ok {
		t.Fatal("SocketOf did not unwrap protocol layers to the underlying socket")
	}
	for _, set := range []func() error{func() error {
		return sc.SetNoDelay(true)
	}, func() error {
		return sc.SetKeepAlive(false)
	}, func() error {
		return sc.SetKeepAlivePeriod(7)
	}, func() error {
		return sc.SetReadBuffer(11)
	}, func() error {
		return sc.SetWriteBuffer(13)
	}, func() error {
		return top.SetDeadline(time.Unix(17, 0))
	}, func() error {
		return top.SetReadDeadline(time.Unix(19, 0))
	}, func() error {
		return top.SetWriteDeadline(time.Unix(23, 0))
	}} {
		if err := set(); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{"delay:true", "keep:false", "period:7", "read:11", "write:13", "both:17", "rd:19", "wd:23"}
	if !reflect.DeepEqual(transport.options, want) {
		t.Fatalf("transport options = %v, want %v", transport.options, want)
	}
	if err := sc.Control(func(fd uintptr) {
		if fd != 42 {
			t.Errorf("Control fd = %d", fd)
		}
	}); err != nil {
		t.Fatal(err)
	}
	top.SetUserdata("application")
	if top.Userdata() != "application" || c.layers[0].Userdata() != "application" || transport.Userdata() != "application" || transport.driverData() != c {
		t.Fatal("application userdata was not shared cleanly with the transport while keeping the stack separate")
	}
	c.layers[0].SetUserdata(nil)
	if top.Userdata() != nil {
		t.Fatal("nil userdata was not preserved")
	}
	if LayerOf(top) != top.conn || LayerOf(transport) != nil {
		t.Fatal("LayerOf reported the wrong input producer")
	}
	if err := top.WriteByte('a'); err != nil {
		t.Fatal(err)
	}
	if _, err := top.WriteString("bc"); err != nil {
		t.Fatal(err)
	}
	if string(transport.written) != "abc" {
		t.Fatalf("layer writes = %q", transport.written)
	}
	c.inLimit = 3
	if err := top.ReserveInbound(3); err != nil {
		t.Fatal(err)
	}
	if err := top.ReserveInbound(4); !errors.Is(err, ErrInboundOverflow) {
		t.Fatalf("inbound reservation = %v", err)
	}
	up := layerUpper{top}
	if n, err := up.Write(nil); n != 0 || err != nil || up.Buffered() != 0 {
		t.Fatal("empty layer delivery changed input")
	}
	if _, err := up.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	if got := top.Peek(make([]byte, 3)); string(got) != "hel" || up.Buffered() != 5 {
		t.Fatalf("layer peek = %q, buffered = %d", got, up.Buffered())
	}
	up.Yield()
	before := transport.wakes
	if _, err := top.Discard(2); err != nil {
		t.Fatal(err)
	}
	if top.stalled.Load() || transport.wakes != before+1 || string(top.PeekChunk()) != "llo" {
		t.Fatal("consumer progress did not resume a yielded producer")
	}
}

func TestSocketLayerCountsOnlyAcceptedOutput(t *testing.T) {
	transport := &contractTransport{}
	c := contractStack(transport, FuncHandler{}, passThroughHandler{})
	lower := c.layers[0].lower.(socketLower)
	if lower.Userdata() != nil {
		t.Fatal("new stack has unexpected userdata")
	}
	lower.SetUserdata("lower application")
	if c.topConn().Userdata() != "lower application" {
		t.Fatal("lower userdata did not share the application's slot")
	}
	if lower.SocketReadSize() != 0 || lower.PacketSource() {
		t.Fatal("custom stream transport claimed socket-specific metadata")
	}
	transport.local = &net.UDPAddr{Port: 7}
	if !lower.PacketSource() {
		t.Fatal("packet source was not reported")
	}
	transport.local = &net.TCPAddr{Port: 7}
	if lower.PacketSource() {
		t.Fatal("stream source was reported as packet")
	}
	if n, err := lower.WriteString("ab"); n != 2 || err != nil {
		t.Fatalf("WriteString = %d, %v", n, err)
	}
	if err := lower.WriteByte('c'); err != nil {
		t.Fatal(err)
	}
	failure := errors.New("transport refused")
	transport.writeError = failure
	if !errors.Is(lower.WriteByte('x'), failure) {
		t.Fatal("byte-write error disappeared")
	}
	fd := &fdConn{commonConn: commonConn{entry: (&Events{readBufSize: 8192}).plainEntry()}}
	if got := (socketLower{Conn: fd}).SocketReadSize(); got != 8192 {
		t.Fatalf("socket read bound = %d", got)
	}
}

type callbackLayer struct {
	PassThrough
	read, timer func(LayerContext) error
}

func (h callbackLayer) OnRead(c LayerContext) error {
	if h.read != nil {
		return h.read(c)
	}
	return h.PassThrough.OnRead(c)
}

func (h callbackLayer) OnTimer(c LayerContext) error {
	if h.timer != nil {
		return h.timer(c)
	}
	return nil
}

func TestLayerCallbackFailureStopsLaterDelivery(t *testing.T) {
	for _, phase := range []string{"layer-timer-error", "layer-timer-close", "layer-read-error", "layer-read-close", "top-timer-error", "top-timer-close", "top-read-error"} {
		t.Run(phase, func(t *testing.T) {
			cause := errors.New(phase)
			var calls []string
			layer := callbackLayer{}
			layer.timer = func(ctx LayerContext) error {
				calls = append(calls, "layer-timer")
				if phase == "layer-timer-close" {
					return ctx.Abort(cause)
				}
				if phase == "layer-timer-error" {
					return cause
				}
				return nil
			}
			layer.read = func(ctx LayerContext) error {
				calls = append(calls, "layer-read")
				if phase == "layer-read-close" {
					return ctx.Abort(cause)
				}
				if phase == "layer-read-error" {
					return cause
				}
				return nil
			}
			top := FuncHandler{Timer: func(c Conn) error {
				calls = append(calls, "top-timer")
				if phase == "top-timer-close" {
					return c.Abort(cause)
				}
				return cause
			}, Read: func(Conn) error {
				calls = append(calls, "top-read")
				return cause
			}}
			transport := &contractTransport{}
			c := contractStack(transport, top, layer)
			if strings.HasPrefix(phase, "layer-timer") {
				if err := (layerCtx{c.layers[0]}).SetTimer(time.Now().Add(-time.Second)); err != nil {
					t.Fatal(err)
				}
			}
			if strings.HasPrefix(phase, "top-timer") {
				if err := c.topConn().SetWakeDeadline(time.Now().Add(-time.Second)); err != nil {
					t.Fatal(err)
				}
			}
			err := c.turn()
			if strings.HasSuffix(phase, "-error") && !errors.Is(err, cause) {
				t.Fatalf("run error = %v, want %v", err, cause)
			}
			if strings.HasSuffix(phase, "-close") && (err != nil || !c.over()) {
				t.Fatalf("callback close = %v, closing = %v", err, c.over())
			}
			last := calls[len(calls)-1]
			want := strings.TrimSuffix(strings.TrimSuffix(phase, "-error"), "-close")
			if last != want {
				t.Fatalf("callbacks continued after %s: %v", want, calls)
			}
			if !errors.Is(transport.closeCause, cause) || transport.graceful {
				t.Fatalf("callback failure did not abort the transport with its cause: %v", transport.closeCause)
			}
		})
	}
}

func TestLayerTimersAreScopedAndClosedStackRejectsRearming(t *testing.T) {
	transport := &contractTransport{}
	c := contractStack(transport, FuncHandler{}, passThroughHandler{})
	top := c.topConn().(*layerNode)
	lower := c.layers[0].lower.(socketLower)
	if err := lower.SetWakeInterval(0); err != nil || c.timers != nil {
		t.Fatal("canceling an unused timer allocated/failed")
	}
	for _, set := range []func() error{func() error {
		return top.SetWakeInterval(time.Hour)
	}, func() error {
		return lower.SetWakeInterval(2 * time.Hour)
	}, func() error {
		return lower.SetWakeDeadline(time.Now().Add(3 * time.Hour))
	}} {
		if err := set(); err != nil {
			t.Fatal(err)
		}
	}
	if c.timers[0].every != 0 || c.timers[top.slot()].every != time.Hour {
		t.Fatal("lower timer changed the top repeating timer")
	}
	if err := top.SetWakeInterval(-1); err != nil || !c.timers[top.slot()].when.IsZero() {
		t.Fatal("top timer cancellation failed")
	}
	if err := lower.SetWakeInterval(-1); err != nil || c.hasTimers.Load() {
		t.Fatal("lower timer cancellation failed")
	}
	c.markClosed()
	if err := top.SetWakeInterval(time.Second); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("closed timer = %v", err)
	}
	if err := lower.SetWakeDeadline(time.Now()); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("closed lower timer = %v", err)
	}
}

func TestLayerInputReservationAndCloseState(t *testing.T) {
	transport := &contractTransport{}
	c := contractStack(transport, FuncHandler{}, passThroughHandler{})
	top := c.topConn().(*layerNode)
	if top.State() != 0 {
		t.Fatal("new layer reported a terminal state")
	}
	if _, err := (layerUpper{top}).Write([]byte("remaining")); err != nil {
		t.Fatal(err)
	}
	top.ended.Store(true)
	if top.State()&StateReadEnded != 0 {
		t.Fatal("buffered layer reported a consumed end")
	}
	if _, err := top.Discard(-1); err != nil {
		t.Fatal(err)
	}
	if top.State()&StateReadEnded == 0 {
		t.Fatal("drained layer hid its end")
	}
	c.beginClosing(len(c.layers), false)
	if top.State()&StateClosing == 0 {
		t.Fatal("layer hid a closing stack")
	}
	c.markClosed()
	if top.State()&(StateClosing|StateClosed) != StateClosing|StateClosed {
		t.Fatal("layer hid a closed stack")
	}
	if n, err := top.Write([]byte("late")); n != 0 || !errors.Is(err, net.ErrClosed) {
		t.Fatalf("late application write = %d, %v", n, err)
	}
	c.markOpen()
	if c.markClosed() || top.State()&StateClosed == 0 {
		t.Fatal("repeated lifecycle operations reopened a closed stack")
	}
	if err := c.turn(); err != nil {
		t.Fatal(err)
	}
	if err := top.PauseRead(); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("closed pause = %v", err)
	}
	if err := top.ResumeRead(); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("closed resume = %v", err)
	}
}

func TestLargeLayerDeliveryReleasesConsumedStorage(t *testing.T) {
	c := contractStack(&contractTransport{}, FuncHandler{}, passThroughHandler{})
	top := c.topConn().(*layerNode)
	up := layerUpper{top}
	room := up.Reserve(maxRetainedDelivery + 1)
	copy(room, bytes.Repeat([]byte{'x'}, len(room)))
	up.Commit(len(room))
	if _, err := top.WriteTo(io.Discard); err != nil {
		t.Fatal(err)
	}
	if top.delivered != nil || top.InboundBuffered() != 0 {
		t.Fatal("fully consumed large delivery retained its allocation")
	}
}

func TestReadControlOwnedPausesDoNotReleaseEachOther(t *testing.T) {
	reads := make(chan string, 8)
	ev := &Events{Pollers: 1, Handler: FuncHandler{Read: func(c Conn) error {
		if n := c.InboundBuffered(); n != 0 {
			p := make([]byte, n)
			_, _ = c.Read(p)
			reads <- string(p)
		}
		return nil
	}}}
	client, server := readControlServe(t, ev)
	a, b := readcontrol.NewPause(server), readcontrol.NewPause(server)
	if a == nil || b == nil || a.Paused() || b.Paused() {
		t.Fatal("new pause handles must exist and start unpaused")
	}
	for _, pause := range []func() error{server.PauseRead, server.PauseRead, a.Pause, a.Pause, b.Pause} {
		if err := pause(); err != nil {
			t.Fatal(err)
		}
	}
	if !server.ReadPaused() || !a.Paused() || !b.Paused() {
		t.Fatal("the pause owners were not all recorded")
	}
	if _, err := client.Write([]byte("owned")); err != nil {
		t.Fatal(err)
	}
	for _, resume := range []func() error{server.ResumeRead, server.ResumeRead, a.Resume, a.Resume} {
		if err := resume(); err != nil {
			t.Fatal(err)
		}
	}
	if a.Paused() || !b.Paused() || !server.ReadPaused() {
		t.Fatal("resuming the application or one owner released another owner")
	}
	if err := server.Wake(); err != nil {
		t.Fatal(err)
	}
	readControlQuiet(t, reads, "input delivery with an outstanding owner")
	if err := b.Resume(); err != nil {
		t.Fatal(err)
	}
	if server.ReadPaused() || b.Paused() {
		t.Fatal("the last owner did not release the pause")
	}
	if got := readControlAwait(t, reads, "input after the final owner resumed"); got != "owned" {
		t.Fatalf("input = %q, want owned", got)
	}
}

func TestReadControlResumeDrainsAnInboundCapacityHold(t *testing.T) {
	const limit, total = 1024, 8 << 10
	var consume atomic.Bool
	var announced atomic.Bool
	var consumed atomic.Int64
	full := make(chan bool, 1)
	drained := make(chan []byte, 1)
	var received bytes.Buffer
	var completed bool
	ev := &Events{Pollers: 1, ReadBufferSize: 512, MaxInboundBuffered: limit, Handler: FuncHandler{Read: func(c Conn) error {
		if !consume.Load() {
			if c.InboundBuffered() >= limit && announced.CompareAndSwap(false, true) {
				full <- c.ReadPaused()
			}
			return nil
		}
		if _, err := c.WriteTo(&received); err != nil {
			return err
		}
		consumed.Store(int64(received.Len()))
		if received.Len() == total && !completed {
			completed = true
			drained <- append([]byte( // used only by the serialized read callback
				nil), received.Bytes()...)
		}
		return nil
	}}}
	client, server := readControlServe(t, ev)
	want := bytes.Repeat([]byte("01234567"), total/8)
	if _, err := client.Write(want); err != nil {
		t.Fatal(err)
	}
	if readControlAwait(t, full, "the inbound capacity threshold") {
		t.Fatal("inbound capacity was reported as a consumer pause")
	}
	if err := server.PauseRead(); err != nil {
		t.Fatal(err)
	}
	consume.Store(true)
	if err := server.Wake(); err != nil {
		t.Fatal(err)
	}
	readControlQuiet(t, drained, "consumption before the manual pause resumed")
	if got := consumed.Load(); got != 0 {
		t.Fatalf("consumed %d bytes while manually paused", got)
	}
	if err := server.ResumeRead(); err != nil {
		t.Fatal(err)
	}
	if got := readControlAwait(t, drained, "capacity-held input after resume"); !bytes.Equal(got, want) {
		t.Fatalf("input changed: got %d bytes, want %d", len(got), len(want))
	}
}

func TestReadControlWakeReevaluatesCapacityHeldInput(t *testing.T) {
	for _, layered := range []bool{false, true} {
		name := "plain"
		if layered {
			name = "layered"
		}
		t.Run(name, func(t *testing.T) {
			const limit, total = 128, 1024
			var consume atomic.Bool
			var announced atomic.Bool
			var received bytes.Buffer
			var complete bool
			full := make(chan bool, 1)
			drained := make(chan []byte, 1)
			events := &Events{Pollers: 1, ReadBufferSize: 64, MaxInboundBuffered: limit, Handler: FuncHandler{Read: func(c Conn) error {
				if !consume.Load() {
					if c.InboundBuffered() >= limit && announced.CompareAndSwap(false, true) {
						full <- c.ReadPaused()
					}
					return nil
				}
				if _, err := c.WriteTo(&received); err != nil {
					return err
				}
				if received.Len() == total && !complete {
					complete = true
					drained <- append([]byte(nil), received.Bytes()...)
				}
				return nil
			}}}
			var layers []Layer
			if layered {
				layers = []Layer{PassThroughLayer}
			}
			peer, conn := readControlServe(t, events, layers...)
			want := bytes.Repeat([]byte("input"), total/5)
			want = append(want, bytes.Repeat([]byte("x"), total-len(want))...)
			if _, err := peer.Write(want); err != nil {
				t.Fatal(err)
			}
			if readControlAwait(t, full, "capacity hold without application pause") {
				t.Fatal("capacity hold was reported as an application pause")
			}
			consume.Store(true)
			if err := conn.ResumeRead(); err != nil {
				t.Fatal(err)
			}
			if err := conn.Wake(); err != nil {
				t.Fatal(err)
			}
			if got := readControlAwait(t, drained, "a wake after downstream drain"); !bytes.Equal(got, want) {
				t.Fatalf("received %d bytes, want %d unchanged", len(got), len(want))
			}
		})
	}
}

func TestReadControlPauseFromBytesReadPrecedesApplicationDelivery(t *testing.T) {
	paused := make(chan error, 1)
	reads := make(chan string, 8)
	var first atomic.Bool
	observer := FuncHandler{BytesRead: func(c Conn, n int) {
		if n > 0 && first.CompareAndSwap(false, true) {
			paused <- c.PauseRead()
		}
	}}
	ev := &Events{Pollers: 1, Observer: observer, Handler: FuncHandler{Read: func(c Conn) error {
		if n := c.InboundBuffered(); n != 0 {
			p := make([]byte, n)
			_, _ = c.Read(p)
			reads <- string(p)
		}
		return nil
	}}}
	client, server := readControlServe(t, ev)
	if _, err := client.Write([]byte("observed")); err != nil {
		t.Fatal(err)
	}
	if err := readControlAwait(t, paused, "the read observer's pause"); err != nil {
		t.Fatal(err)
	}
	if err := server.Wake(); err != nil {
		t.Fatal(err)
	}
	readControlQuiet(t, reads, "input delivery after the observer paused")
	if err := server.ResumeRead(); err != nil {
		t.Fatal(err)
	}
	if got := readControlAwait(t, reads, "observer-held input after resume"); got != "observed" {
		t.Fatalf("input = %q, want observed", got)
	}
}

func TestReadControlPausedConnectionsStillCloseAndExpire(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		name := "Close"
		if deadline {
			name = "ReadDeadline"
		}
		t.Run(name, func(t *testing.T) {
			paused := make(chan error, 1)
			closed := make(chan error, 1)
			ev := &Events{Pollers: 1, Handler: FuncHandler{Open: func(c Conn) {
				paused <- c.PauseRead()
			}, Close: func(_ Conn, err error) {
				closed <- err
			}}}
			_, server := readControlServe(t, ev)
			if err := readControlAwait(t, paused, "OnOpen pause"); err != nil {
				t.Fatal(err)
			}
			var err error
			if deadline {
				err = server.SetReadDeadline(time.Now().Add(30 * time.Millisecond))
			} else {
				err = server.Close()
			}
			if err != nil {
				t.Fatal(err)
			}
			err = readControlAwait(t, closed, "OnClose while paused")
			if deadline && !errors.Is(err, os.ErrDeadlineExceeded) {
				t.Fatalf("deadline close = %v, want deadline exceeded", err)
			}
			if err := server.ResumeRead(); !errors.Is(err, net.ErrClosed) {
				t.Fatalf("ResumeRead after OnClose = %v, want net.ErrClosed", err)
			}
		})
	}
}

func readControlAwait[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
		return *new(T)
	}
}

func readControlQuiet[T any](t *testing.T, ch <-chan T, what string) {
	t.Helper()
	select {
	case value := <-ch:
		t.Fatalf("%s while reads were paused: %v", what, value)
	case <-time.After(30 * time.Millisecond):
	}
}

func readControlServe(t *testing.T, ev *Events, layers ...Layer) (net.Conn, Conn) {
	t.Helper()
	opened := make(chan Conn, 1)
	handler, ok := asFuncs(ev.Handler)
	if !ok || handler == nil {
		t.Fatal("readControlServe requires a FuncHandler")
	}
	onOpen := handler.Open
	handler.Open = func(c Conn) {
		if onOpen != nil {
			onOpen(c)
		}
		opened <- c
	}
	ev.Handler = handler
	started := make(chan struct{})
	ev.Hooks = FuncHandler{Start: func(*Events) {
		close(started)
	}}
	done := make(chan error, 1)
	go func() {
		done <- ev.Serve()
	}()
	t.Cleanup(func() {
		_ = ev.Close(nil)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("Serve did not return")
		}
	})
	readControlAwait(t, started, "server initialization")
	var opts []ListenOption
	if len(layers) != 0 {
		opts = append(opts, WithLayers(layers...))
	}
	listener, err := ev.Listen("tcp://127.0.0.1:0", opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = listener.Close()
	})
	client, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = client.Close()
	})
	return client, readControlAwait(t, opened, "OnOpen")
}

func writeUntilStalled(t *testing.T, client net.Conn, total int) int {
	t.Helper()
	chunk := make([]byte, 16<<10)
	sent := 0
	for sent < total {
		_ = client.SetWriteDeadline(time.Now().Add(300 * time.Millisecond))
		n, err := client.Write(chunk[:min(len(chunk), total-sent)])
		sent += n
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				break
			}
			if isPeerGoneWrite(err) {
				break
			}
			t.Fatal(err)
		}
	}
	_ = client.SetWriteDeadline(time.Time{})
	return sent
}

func TestReadControlResumeDuringLastReadKeepsLayeredInput(t *testing.T) {
	const total = 100000
	var got, seen atomic.Int64
	var top atomic.Pointer[Conn]
	closed := make(chan error, 1)
	ev := &Events{Pollers: 1, Handler: FuncHandler{Open: func(c Conn) {
		_ = c.PauseRead()
		top.Store(&c)
	}, Read: func(c Conn) error {
		n, _ := c.Discard(-1)
		got.Add(int64(n))
		return nil
	}, Close: func(_ Conn, err error) {
		closed <- err
	}}, Observer: FuncHandler{BytesRead: func(_ Conn, n int) {
		if seen.Add(int64(n)) == total {
			_ = (*top.Load()).ResumeRead()
		}
	}}}
	client, _ := readControlServe(t, ev, PassThroughLayer)
	if _, err := client.Write(make([]byte, total)); err != nil {
		t.Fatal(err)
	}
	if err := client.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	if err := readControlAwait(t, closed, "the end after resumed input"); err != io.EOF {
		t.Fatalf("OnClose = %v, want EOF", err)
	}
	if got.Load() != total {
		t.Fatalf("closed after delivering %d of %d bytes", got.Load(), total)
	}
}

func TestReadControlBudgetYieldDeliversAllBeforeEOF(t *testing.T) {
	for _, layered := range []bool{false, true} {
		t.Run(map[bool]string{false: "plain", true: "layered"}[layered], func(t *testing.T) {
			const total, slice = 300000, 1000
			var got atomic.Int64
			closed := make(chan error, 1)
			ev := &Events{Pollers: 1, Handler: FuncHandler{Read: func(c Conn) error {
				n, _ := c.Discard(min(slice, c.InboundBuffered()))
				got.Add(int64(n))
				if c.InboundBuffered() > 0 {
					return c.Wake()
				}
				return nil
			}, Close: func(_ Conn, err error) {
				closed <- err
			}}}
			var layers []Layer
			if layered {
				layers = []Layer{PassThroughLayer}
			}
			client, _ := readControlServe(t, ev, layers...)
			if _, err := client.Write(make([]byte, total)); err != nil {
				t.Fatal(err)
			}
			_ = client.(*net.TCPConn).CloseWrite()
			readControlAwait(t, closed, "the end after the yielded input")
			if got.Load() != total {
				t.Fatalf("closed after delivering %d of %d bytes", got.Load(), total)
			}
		})
	}
}

type noEndLayer struct{}

type noEndHandler struct{}

func (noEndLayer) Attach(LayerContext) LayerHandler {
	return noEndHandler{}
}

func (noEndHandler) OnOpen(ctx LayerContext) {
	ctx.Open()
}

func (noEndHandler) OnRead(ctx LayerContext) error {
	lower := ctx.Lower()
	if chunk := lower.PeekChunk(); len(chunk) > 0 {
		_, _ = ctx.Upper().Write(chunk)
		_, _ = lower.Discard(len(chunk))
	}
	return nil
}

func (noEndHandler) OnWrite(ctx LayerContext, p []byte) (int, error) {
	return ctx.Lower().Write(p)
}

func (noEndHandler) OnTimer(LayerContext) error {
	return nil
}

func (noEndHandler) OnClose(LayerContext, error) {
}

func (noEndHandler) CloseWrite(ctx LayerContext) error {
	return ctx.Lower().CloseWrite()
}

func TestStackWithoutEndStillCloses(t *testing.T) {
	for _, halfOpen := range []bool{true, false} {
		t.Run(map[bool]string{true: "half-open", false: "closing"}[halfOpen], func(t *testing.T) {
			closed := make(chan error, 1)
			var got atomic.Int64
			ev := &Events{Pollers: 1, AllowHalfOpen: halfOpen, Handler: FuncHandler{Read: func(c Conn) error {
				n, _ := c.Discard(-1)
				if got.Add(int64(n)) >= 3 && halfOpen {
					return c.CloseWrite()
				}
				return nil
			}, Close: func(_ Conn, err error) {
				closed <- err
			}}}
			client, _ := readControlServe(t, ev, noEndLayer{})
			if _, err := client.Write([]byte("bye")); err != nil {
				t.Fatal(err)
			}
			_ = client.(*net.TCPConn).CloseWrite()
			readControlAwait(t, closed, "the close after the peer's end")
			if got.Load() != 3 {
				t.Fatalf("delivered %d of 3 bytes", got.Load())
			}
		})
	}
}

type holdingLayer struct {
	PassThrough
	held int
	full bool
}

func (h *holdingLayer) OnWrite(_ LayerContext, p []byte) (int, error) {
	if h.full {
		return 0, ErrOutboundOverflow
	}
	h.held += len(p)
	return len(p), nil
}

func (h *holdingLayer) OutboundHeld(LayerContext) int {
	return h.held
}

func TestStackReportsHeldOutputDrains(t *testing.T) {
	transport := new(contractTransport)
	holder := &holdingLayer{}
	var drained int
	driver := &stackDriver{layers: []Layer{LayerFunc(func(LayerContext) LayerHandler {
		return holder
	})}, top: FuncHandler{}}
	driver.OnOpen(transport)
	c := stackOf(transport)
	top := c.topConn()
	top.(*layerNode).connWriteControl().Watch(func(sent int) {
		if sent == 0 {
			drained++
		}
	})
	if _, err := top.Write([]byte("queued")); err != nil {
		t.Fatal(err)
	}
	holder.held = 0
	if err := c.turn(); err != nil {
		t.Fatal(err)
	}
	if drained != 1 {
		t.Fatalf("drains reported = %d, want one for the top connection", drained)
	}
	if err := c.turn(); err != nil {
		t.Fatal(err)
	}
	if drained != 1 {
		t.Fatal("a turn without a drain reported one")
	}
	if _, err := top.Write([]byte("more")); err != nil {
		t.Fatal(err)
	}
	holder.held = 0
	if err := c.turn(); err != nil {
		t.Fatal(err)
	}
	if drained != 2 {
		t.Fatalf("drains reported = %d, want two", drained)
	}
	holder.full = true
	if _, err := top.Write([]byte("refused")); !errors.Is(err, ErrOutboundOverflow) {
		t.Fatalf("write = %v, want overflow", err)
	}
	holder.full = false
	if err := c.turn(); err != nil {
		t.Fatal(err)
	}
	if drained != 3 {
		t.Fatalf("drains reported = %d, want a third for the refusal", drained)
	}
	if err := c.turn(); err != nil {
		t.Fatal(err)
	}
	if drained != 3 {
		t.Fatal("a refusal was reported twice")
	}
}

func TestPacedLayerAtSocketEndStillCloses(t *testing.T) {
	closed := make(chan error, 1)
	ev := &Events{Pollers: 1, Handler: FuncHandler{Read: func(Conn) error {
		return nil
	}, Close: func(_ Conn, err error) {
		closed <- err
	}}}
	client, _ := readControlServe(t, ev, PassThroughLayer)
	if _, err := client.Write(make([]byte, DefaultDeliverBound+4096)); err != nil {
		t.Fatal(err)
	}
	_ = client.(*net.TCPConn).CloseWrite()
	if err := readControlAwait(t, closed, "the close at the socket's end"); err != io.ErrUnexpectedEOF {
		t.Fatalf("OnClose = %v, want unexpected EOF", err)
	}
}

func TestStackEndWaitsForEveryLayerConsumer(t *testing.T) {
	transport := new(contractTransport)
	c := contractStack(transport, FuncHandler{}, passThroughHandler{}, passThroughHandler{})
	raw := new(fdConn)
	raw.entry = &entry{ev: new(Events), directConsumer: false}
	setStackState(raw, c)
	top := c.layers[len(c.layers)-1]
	if c.ended() {
		t.Fatal("a stack ended before its socket did")
	}
	transport.state = StateReadEnded | StateWriteEnded
	top.markWriteShut()
	c.halfOpen = true
	if !c.ended() || !errors.Is(c.endOfStream(true), io.EOF) {
		t.Fatal("an empty half-open stack behind an ended socket did not end")
	}
	c.layers[0].delivered = []byte("held by the intermediate consumer")
	if !c.ended() || c.endOfStream(true) != nil {
		t.Fatal("an intermediate layer's retained data was passed over on half-open")
	}
	c.halfOpen = false
	if err := c.endOfStream(true); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("a truncated stack ended with %v", err)
	}
	c.layers[0].delivered = nil
	c.layers[0].stalled.Store(true)
	if c.ended() {
		t.Fatal("a layer that stopped short was passed over")
	}
	c.layers[0].stalled.Store(false)
	top.delivered = []byte("unread by the Handler")
	if !c.ended() {
		t.Fatal("the Handler's own unread delivery held the end back")
	}
	c.halfOpen = true
	if c.endOfStream(true) != nil {
		t.Fatal("a half-open close discarded the Handler's unread delivery")
	}
	c.halfOpen = false
	top.delivered = nil
	transport.state = 0
	top.ended.Store(true)
	if !c.ended() {
		t.Fatal("the top layer's end did not end the stack")
	}
	if err := top.PauseRead(); err != nil {
		t.Fatal(err)
	}
	if err := c.endOfStream(true); err != nil {
		t.Fatal("a paused Handler did not hold the end back")
	}
	if got := raw.inputLimit(false); got != DefaultDeliverBound {
		t.Fatalf("non-direct stack input bound=%d", got)
	}
	if err := top.ResumeRead(); err != nil {
		t.Fatal(err)
	}
	if err := c.endOfStream(true); err != nil {
		t.Fatal("the end overtook the resume turn the release owes")
	}
	top.readControl.Load().ConsumerRuns()
	if err := c.endOfStream(true); err == nil {
		t.Fatal("an unpaused Handler kept the ended stack open")
	}
	plain := &fdConn{commonConn: commonConn{entry: (&Events{}).plainEntry()}}
	if got := plain.inputLimit(false); got != 0 {
		t.Fatalf("unpaused default plain input bound=%d", got)
	}
	c.halfOpen = true
	top.gate.Store(gateOpenUp)
	if err := c.endOfStream(true); err != nil {
		t.Fatal("a half-open stack closed at its peer's end")
	}
}

func TestReadControllerLookupAndClosedBoundaryContracts(t *testing.T) {
	getter := readControlGetter{}
	if getter.GetReadControl(new(contractTransport)) != nil {
		t.Fatal("unsupported transport supplied a private controller")
	}
	var absent *layerNode
	if absent.connReadControl() != nil {
		t.Fatal("nil layer supplied controller")
	}
	transport := new(contractTransport)
	c := contractStack(transport, FuncHandler{}, passThroughHandler{})
	top := c.topConn().(*layerNode)
	if got := top.connReadControl(); got == nil || got != readcontrol.ControlOf(top) {
		t.Fatal("controller lookup lost boundary identity")
	}
	if (socketLower{Conn: top}).connReadControl() != top.connReadControl() {
		t.Fatal("socket wrapper supplied a separate pause owner count")
	}
	c.state.Store(stackClosed)
	if err := top.ResumeRead(); !errors.Is(err, net.ErrClosed) {
		t.Fatal(err)
	}
}

func TestWriteControlAccessOnConnAndStack(t *testing.T) {
	var nilFD *fdConn
	if nilFD.connWriteControl() != nil {
		t.Fatal("nil fdConn returned write control")
	}
	var nilNode *layerNode
	if nilNode.connWriteControl() != nil {
		t.Fatal("nil layerNode returned write control")
	}
	var getter writeControlGetter
	if getter.GetWriteControl(123) != nil {
		t.Fatal("non-connection returned write control")
	}
	fd := &fdConn{commonConn: commonConn{entry: (&Events{}).plainEntry()}}
	if fd.hasWriteControl() {
		t.Fatal("fresh fdConn already allocated write control")
	}
	var fdHits int
	if !writecontrol.Watch(fd, func(n int) {
		fdHits += n
	}) {
		t.Fatal("fdConn did not expose write control")
	}
	if !fd.hasWriteControl() {
		t.Fatal("watched fdConn did not report write control")
	}
	if writecontrol.ControlOf(fd) != fd.ensureWriteControl() {
		t.Fatal("ensureWriteControl allocated a second instance")
	}
	sl := socketLower{Conn: fd}
	if writecontrol.ControlOf(sl) != writecontrol.ControlOf(fd) {
		t.Fatal("socketLower did not forward write control")
	}
	holder := &holdingLayer{}
	driver := &stackDriver{layers: []Layer{LayerFunc(func(LayerContext) LayerHandler {
		return PassThrough{}
	}), LayerFunc(func(LayerContext) LayerHandler {
		return holder
	}), PassThroughLayer}, top: FuncHandler{}}
	driver.OnOpen(fd)
	var intervalCtx LayerContext
	contractStack(&contractTransport{}, FuncHandler{}, callbackLayer{timer: nil})
	(&stackDriver{layers: []Layer{LayerFunc(func(ctx LayerContext) LayerHandler {
		intervalCtx = ctx
		return PassThrough{}
	})}, top: FuncHandler{}}).OnOpen(&contractTransport{})
	if err := intervalCtx.SetTimerInterval(10 * time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if err := intervalCtx.SetTimerInterval(0); err != nil {
		t.Fatal(err)
	}
	st := stackOf(fd)
	bottomCtrl := writecontrol.ControlOf(st.layers[0])
	midCtrl := writecontrol.ControlOf(st.layers[1])
	topCtrl := writecontrol.ControlOf(st.layers[2])
	if bottomCtrl.Inexact() {
		t.Fatal("layer below first holder marked inexact")
	}
	if !midCtrl.Inexact() || !topCtrl.Inexact() {
		t.Fatal("layer at or above holder not marked inexact")
	}
	if writecontrol.ControlOf(st.layers[2]) != topCtrl {
		t.Fatal("layerNode allocated duplicate write control")
	}
	var stackHits int
	topCtrl.Watch(func(n int) {
		stackHits += n
	})
	fd.events().onSocketBytesWrite(fd, 5)
	if fdHits != 5 || stackHits != 5 {
		t.Fatalf("hits = (%d, %d), want (5, 5)", fdHits, stackHits)
	}
	st.closed(nil)
	fd.events().onSocketBytesWrite(fd, 3)
	if stackHits != 5 {
		t.Fatal("closed stack forwarded socket write notification")
	}
}

type connDecorator struct{ Conn }

func (d *connDecorator) Unwrap() Conn {
	return d.Conn
}

type lowerDecorator struct{ Lower }

func (d *lowerDecorator) Unwrap() Lower {
	return d.Lower
}

type virtualNonSocketConn struct{ Conn }

func TestUnwrapAndSocketOfContracts(t *testing.T) {
	if _, ok := SocketOf(nil); ok {
		t.Fatal("SocketOf(nil) succeeded")
	}
	if LayerOf(nil) != nil {
		t.Fatal("LayerOf(nil) returned non-nil")
	}
	if _, ok := FindLayer[LayerHandler](nil); ok {
		t.Fatal("FindLayer(nil) succeeded")
	}
	virt := &virtualNonSocketConn{}
	if _, ok := SocketOf(virt); ok {
		t.Fatal("SocketOf succeeded on virtual non-socket Conn")
	}
	selfWrap := &connDecorator{}
	selfWrap.Conn = selfWrap
	if _, ok := SocketOf(selfWrap); ok {
		t.Fatal("SocketOf looped on self-wrapping decorator")
	}
	fd := &fdConn{commonConn: commonConn{entry: (&Events{}).plainEntry()}}
	holder := &holdingLayer{}
	driver := &stackDriver{layers: []Layer{LayerFunc(func(LayerContext) LayerHandler {
		return holder
	})}, top: FuncHandler{}}
	driver.OnOpen(fd)
	top := stackOf(fd).topConn()
	wrapped := &connDecorator{Conn: top}
	lowerWrapped := &lowerDecorator{Lower: wrapped}
	if sc, ok := SocketOf(lowerWrapped); !ok || sc != fd {
		t.Fatalf("SocketOf(lowerWrapped) = (%v, %v), want (%v, true)", sc, ok, fd)
	}
	if sc, ok := SocketOf(stackOf(fd).layers[0].lower); !ok || sc != fd {
		t.Fatalf("SocketOf(socketLower) = (%v, %v), want (%v, true)", sc, ok, fd)
	}
	if LayerOf(lowerWrapped) != holder {
		t.Fatal("LayerOf did not unwrap decorators")
	}
	if got, ok := FindLayer[*holdingLayer](lowerWrapped); !ok || got != holder {
		t.Fatal("FindLayer did not unwrap decorators")
	}
}

func TestStackCloseAndAbortRepeatContracts(t *testing.T) {
	t1 := &contractTransport{}
	var ctx1 LayerContext
	(&stackDriver{layers: []Layer{LayerFunc(func(ctx LayerContext) LayerHandler {
		ctx1 = ctx
		return PassThrough{}
	})}, top: FuncHandler{}}).OnOpen(t1)
	top1 := stackOf(t1).topConn()
	if err := top1.Abort(nil); err != nil {
		t.Fatal(err)
	}
	if err := top1.Abort(nil); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("second Conn.Abort = %v, want net.ErrClosed", err)
	}
	if err := ctx1.Abort(nil); err != nil {
		t.Fatalf("LayerContext.Abort while aborting = %v, want nil", err)
	}
	t2 := &closedAbortTransport{}
	var ctx2 LayerContext
	(&stackDriver{layers: []Layer{LayerFunc(func(ctx LayerContext) LayerHandler {
		ctx2 = ctx
		return PassThrough{}
	})}, top: FuncHandler{}}).OnOpen(t2)
	top2 := stackOf(t2).topConn()
	if err := top2.Close(); err != nil {
		t.Fatal(err)
	}
	if err := ctx2.Abort(nil); err != nil {
		t.Fatalf("LayerContext.Abort cutting closed socket = %v, want nil", err)
	}
	t3 := &contractTransport{}
	s3 := contractStack(t3, FuncHandler{}, passThroughHandler{})
	sl := s3.layers[0].lower.(socketLower)
	if err := sl.Close(); err != nil {
		t.Fatal(err)
	}
	t4 := &contractTransport{}
	s4 := contractStack(t4, FuncHandler{}, passThroughHandler{})
	if err := s4.layers[0].lower.(socketLower).Abort(nil); err != nil {
		t.Fatal(err)
	}
}

type closedAbortTransport struct{ contractTransport }

func (c *closedAbortTransport) Abort(error) error {
	return net.ErrClosed
}
