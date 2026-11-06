package ubridge

import (
	"bytes"
	"context"
	"errors"
	"github.com/urpc/uio"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/urpc/uio/internal/readcontrol"
)

// bridgeReadConn lacks the private controller capability, as a custom uio.Conn
// does: the bridge relays it without a pause of its own. Its public pause
// methods let the tests detect accidental use of the application's slot.
// bridgeOwnedConn adds only the trusted internal adapter.
type bridgeReadConn struct {
	uio.Conn
	control         *readcontrol.Control
	mu              sync.Mutex
	inbound         []byte
	written         bytes.Buffer
	userdata        any
	queue           atomic.Int64
	writtenBytes    atomic.Int64
	autoDrain       atomic.Bool
	creditBlocked   atomic.Bool
	state           atomic.Uint32
	wakes           atomic.Int64
	defaultPauses   atomic.Int64
	defaultResumes  atomic.Int64
	writeCloses     atomic.Int64
	gracefulCloses  atomic.Int64
	stateHook       func()
	accessHook      func()
	writeHook       func()
	writeError      error
	discardError    error
	writeCloseError error
}

func newBridgeReadConn(check func() error) *bridgeReadConn {
	c := &bridgeReadConn{}
	c.control = readcontrol.New(func() error {
		if check != nil {
			if err := check(); err != nil {
				return err
			}
		}
		if c.stateHook != nil {
			c.stateHook()
		}
		if c.state.Load()&uint32(uio.StateClosing|uio.StateClosed) != 0 {
			return net.ErrClosed
		}
		return nil
	}, func() error { c.wakes.Add(1); return nil })
	return c
}

type bridgeOwnedAccess struct{}

func (bridgeOwnedAccess) GetReadControl(source any) *readcontrol.Control {
	c := source.(*bridgeOwnedConn)
	if c.accessHook != nil {
		c.accessHook()
	}
	return c.control
}

type bridgeOwnedConn struct {
	readcontrol.Access[bridgeOwnedAccess]
	*bridgeReadConn
}

func newBridgeOwnedConn(check func() error) *bridgeOwnedConn {
	return &bridgeOwnedConn{bridgeReadConn: newBridgeReadConn(check)}
}

func (c *bridgeReadConn) PauseRead() error {
	c.defaultPauses.Add(1)
	return c.control.PauseRead()
}
func (c *bridgeReadConn) ResumeRead() error {
	c.defaultResumes.Add(1)
	return c.control.ResumeRead()
}
func (c *bridgeReadConn) ReadPaused() bool      { return c.control.ReadPaused() }
func (c *bridgeReadConn) OutboundBuffered() int { return int(c.queue.Load()) }
func (c *bridgeReadConn) InboundBuffered() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.inbound)
}
func (c *bridgeReadConn) setInbound(p string) {
	c.mu.Lock()
	c.inbound = []byte(p)
	c.mu.Unlock()
}
func (c *bridgeReadConn) writtenString() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.written.String()
}
func (c *bridgeReadConn) Write(p []byte) (int, error) {
	if hook := c.writeHook; hook != nil {
		hook()
	}
	if c.writeError != nil {
		return 0, c.writeError
	}
	if c.creditBlocked.Load() {
		return 0, uio.ErrOutboundOverflow
	}
	c.mu.Lock()
	c.written.Write(p)
	c.mu.Unlock()
	c.writtenBytes.Add(int64(len(p)))
	if !c.autoDrain.Load() {
		c.queue.Add(int64(len(p)))
	}
	return len(p), nil
}
func (c *bridgeReadConn) WriteTo(w io.Writer) (int64, error) {
	c.mu.Lock()
	p := append([]byte(nil), c.inbound...)
	c.mu.Unlock()
	if len(p) == 0 {
		return 0, nil
	}
	n, err := w.Write(p)
	_, _ = c.Discard(n)
	return int64(n), err
}
func (c *bridgeReadConn) Discard(n int) (int, error) {
	if c.discardError != nil {
		return 0, c.discardError
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if n < 0 || n > len(c.inbound) {
		n = len(c.inbound)
	}
	c.inbound = c.inbound[n:]
	return n, nil
}
func (c *bridgeReadConn) State() uio.ConnState {
	if c.stateHook != nil {
		c.stateHook()
	}
	return uio.ConnState(c.state.Load())
}
func (c *bridgeReadConn) Wake() error { c.wakes.Add(1); return nil }
func (c *bridgeReadConn) CloseWrite() error {
	c.writeCloses.Add(1)
	if c.writeCloseError != nil {
		return c.writeCloseError
	}
	c.state.Or(uint32(uio.StateWriteEnded))
	return nil
}
func (c *bridgeReadConn) Close() error {
	c.gracefulCloses.Add(1)
	c.state.Or(uint32(uio.StateClosing | uio.StateClosed))
	return nil
}
func (c *bridgeReadConn) SetLinger(int) error { return nil }
func (c *bridgeReadConn) Abort(error) error {
	c.state.Or(uint32(uio.StateClosing | uio.StateClosed))
	return nil
}
func (c *bridgeReadConn) Userdata() any {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.userdata
}

func TestBridgePairIsFixedAndIdempotent(t *testing.T) {
	a, b, c := newBridgeOwnedConn(nil), newBridgeOwnedConn(nil), newBridgeOwnedConn(nil)
	var paired atomic.Int64
	br := &Bridge{OnPair: func(uio.Conn, uio.Conn) { paired.Add(1) }}
	if err := br.Pair(a, b); err != nil {
		t.Fatal(err)
	}
	p := br.pairOf(a)
	if err := br.Pair(b, a); err != nil {
		t.Fatal(err)
	}
	if paired.Load() != 1 || br.pairOf(b) != p {
		t.Fatal("same-pair call was not idempotent")
	}
	if err := br.Pair(a, c); err == nil {
		t.Fatal("a live pair was replaced")
	}
	if br.pairOf(a) != p || br.pairOf(c) != nil {
		t.Fatal("a conflicting pair changed the registry")
	}
	c.state.Store(uint32(uio.StateClosed))
	if err := br.Pair(a, c); !errors.Is(err, ErrAlreadyPaired) || a.State()&uio.StateClosed != 0 {
		t.Fatal("rejecting a conflicting closed peer aborted the existing pair")
	}
	if err := br.Pair(a, a); !errors.Is(err, uio.ErrUnsupported) {
		t.Fatalf("self pair: %v", err)
	}
	if err := br.Pair(nil, a); !errors.Is(err, uio.ErrUnsupported) {
		t.Fatalf("nil pair: %v", err)
	}
	custom, owned := newBridgeReadConn(nil), newBridgeOwnedConn(nil)
	if err := br.Pair(custom, owned); err != nil {
		t.Fatalf("custom connection: %v", err)
	}
	if p := br.pairOf(custom); p == nil || p.a.pause != nil || p.b.pause == nil {
		t.Fatal("a custom connection got a bridge pause it cannot hold")
	}
}

func TestBridgePairDoesNotRetainAConnectionClosedBeforePublication(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var first atomic.Bool
	a, b := newBridgeOwnedConn(nil), newBridgeOwnedConn(nil)
	a.accessHook = func() {
		if first.CompareAndSwap(false, true) {
			close(entered) // owner acquisition happens before registry publication
			<-release
		}
	}
	br := &Bridge{}
	done := make(chan error, 1)
	go func() { done <- br.Pair(a, b) }()
	bridgeAwait(t, entered, "owner acquisition before pair publication")
	a.state.Store(uint32(uio.StateClosed))
	br.OnClose(a, io.EOF) // no registry entry exists yet and this will not repeat
	close(release)
	if err := bridgeAwait(t, done, "pair publication racing close"); !errors.Is(err, ErrPeerGone) {
		t.Fatalf("Pair = %v, want closed peer", err)
	}
	if len(br.pairs) != 0 || b.State()&uio.StateClosed != 0 {
		t.Fatal("failed publication retained entries or closed a caller-owned live endpoint")
	}
}

// A source whose destination is behind keeps its input and waits; the
// destination draining to half the limit lets it forward again. Only a
// source whose waiting input fills its own bound is paused as well.
func TestBridgeDestinationWatermarkHoldsItsSource(t *testing.T) {
	for _, high := range []int{2, 8} {
		src, dst := newBridgeOwnedConn(nil), newBridgeOwnedConn(nil)
		br := &Bridge{InboundLimit: high}
		if err := br.Pair(src, dst); err != nil {
			t.Fatal(err)
		}
		p := br.pairOf(src)
		src.setInbound(string(bytes.Repeat([]byte{'x'}, high)))
		if err := br.OnRead(src); err != nil {
			t.Fatal(err)
		}
		if src.InboundBuffered() != 0 || p.a.waiting.Load() || src.ReadPaused() {
			t.Fatal("a destination with room did not take the whole input")
		}
		src.setInbound("y")
		if err := br.OnRead(src); err != nil {
			t.Fatal(err)
		}
		if src.InboundBuffered() != 1 || !p.a.waiting.Load() || src.ReadPaused() {
			t.Fatal("input behind a full destination was not kept waiting, unpaused")
		}
		src.setInbound(string(bytes.Repeat([]byte{'y'}, high)))
		if err := br.OnRead(src); err != nil {
			t.Fatal(err)
		}
		if !src.ReadPaused() {
			t.Fatal("waiting input that filled the bound did not pause its source")
		}
		dst.queue.Store(int64(high/2 + 1))
		br.OnBytesWritten(dst, 1)
		if !src.ReadPaused() || !p.a.waiting.Load() {
			t.Fatal("source went on above low water")
		}
		wakes := src.wakes.Load()
		dst.queue.Store(int64(high / 2))
		br.OnBytesWritten(dst, 0)
		if src.ReadPaused() || p.a.waiting.Load() || src.wakes.Load() != wakes+1 {
			t.Fatal("source was not released at low water")
		}
		if err := br.OnRead(src); err != nil {
			t.Fatal(err)
		}
		if src.InboundBuffered() != 0 {
			t.Fatal("released source did not forward")
		}
		// Waiting without a pause goes on through a wake.
		dst.queue.Store(int64(high))
		src.setInbound("z")
		if err := br.OnRead(src); err != nil {
			t.Fatal(err)
		}
		wakes = src.wakes.Load()
		dst.queue.Store(0)
		br.OnBytesWritten(dst, 1)
		br.OnBytesWritten(dst, 1)
		if src.wakes.Load() != wakes+1 || p.a.waiting.Load() {
			t.Fatal("a waiting source got no single wake on the drain")
		}
		if src.defaultPauses.Load() != 0 || src.defaultResumes.Load() != 0 {
			t.Fatal("bridge used application's public pause slot")
		}
	}
}

// A drain that lands while the source forwards, before it marks itself
// waiting, would see no waiting source: the source notices it and goes on.
func TestBridgeDrainDuringForwardIsNotLost(t *testing.T) {
	src := newBridgeOwnedConn(nil)
	dst := newBridgeOwnedConn(nil)
	br := &Bridge{InboundLimit: 8}
	if err := br.Pair(src, dst); err != nil {
		t.Fatal(err)
	}
	src.setInbound("held")
	dst.queue.Store(1)
	dst.creditBlocked.Store(true)
	dst.writeHook = func() {
		// The forward is refused, and the queue drains before the source
		// marks itself waiting.
		dst.writeHook = nil
		dst.queue.Store(0)
		br.OnBytesWritten(dst, 1)
	}
	wakes := src.wakes.Load()
	if err := br.OnRead(src); err != nil {
		t.Fatal(err)
	}
	if src.wakes.Load() != wakes+1 || br.pairOf(src).a.waiting.Load() {
		t.Fatal("a drain racing the forward stranded the source")
	}
}

func TestBridgeDrainDoesNotReleaseApplicationPause(t *testing.T) {
	src, dst := newBridgeOwnedConn(nil), newBridgeOwnedConn(nil)
	br := &Bridge{InboundLimit: 8}
	if err := br.Pair(src, dst); err != nil {
		t.Fatal(err)
	}
	if err := src.PauseRead(); err != nil {
		t.Fatal(err)
	}
	src.setInbound("held")
	dst.queue.Store(8)
	// A callback already in flight may finish after the application pauses.
	if err := br.OnRead(src); err != nil {
		t.Fatal(err)
	}
	dst.queue.Store(0)
	br.OnBytesWritten(dst, 8)
	if !src.ReadPaused() {
		t.Fatal("destination drain cleared the application pause")
	}
	if err := src.ResumeRead(); err != nil {
		t.Fatal(err)
	}
	if src.ReadPaused() || src.defaultPauses.Load() != 1 || src.defaultResumes.Load() != 1 {
		t.Fatal("bridge changed application pause ownership")
	}
}

func TestBridgeDrainBeforePauseIsNotLost(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var armed, first atomic.Bool
	src := newBridgeOwnedConn(func() error {
		if armed.Load() && first.CompareAndSwap(false, true) {
			close(entered)
			<-release
		}
		return nil
	})
	dst := newBridgeOwnedConn(nil)
	br := &Bridge{InboundLimit: 8}
	if err := br.Pair(src, dst); err != nil {
		t.Fatal(err)
	}
	armed.Store(true)
	src.setInbound("heldheld")
	dst.queue.Store(8)
	done := make(chan error, 1)
	go func() { done <- br.OnRead(src) }()
	bridgeAwait(t, entered, "pause before publication")
	dst.queue.Store(0)
	br.OnBytesWritten(dst, 8)
	close(release)
	if err := bridgeAwait(t, done, "pause/drain race"); err != nil {
		t.Fatal(err)
	}
	if src.ReadPaused() {
		t.Fatal("drain before pause stranded source")
	}
	if err := br.OnRead(src); err != nil {
		t.Fatal(err)
	}
	if dst.writtenString() != "heldheld" {
		t.Fatal("retained input was not forwarded")
	}
}

func TestBridgeCloseCannotLeaveALatePauseHeld(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var armed, first atomic.Bool
	src := newBridgeOwnedConn(func() error {
		if armed.Load() && first.CompareAndSwap(false, true) {
			close(entered)
			<-release
		}
		return nil
	})
	dst := newBridgeOwnedConn(nil)
	br := &Bridge{InboundLimit: 8}
	if err := br.Pair(src, dst); err != nil {
		t.Fatal(err)
	}
	armed.Store(true)
	src.setInbound("orphaned")
	dst.queue.Store(8)
	done := make(chan error, 1)
	go func() { done <- br.OnRead(src) }()
	bridgeAwait(t, entered, "in-flight pause")
	br.OnClose(dst, io.EOF)
	close(release)
	if err := bridgeAwait(t, done, "read racing close"); err != nil {
		t.Fatal(err)
	}
	if src.ReadPaused() {
		t.Fatal("late pause survived pair closure")
	}
	if err := br.OnRead(src); err != nil {
		t.Fatal(err)
	}
	if src.InboundBuffered() != 0 {
		t.Fatal("orphan could not discard its input")
	}
	src.state.Or(uint32(uio.StateReadEnded))
	if err := br.OnRead(src); err != nil {
		t.Fatal(err)
	}
	if src.gracefulCloses.Load() != 1 {
		t.Fatal("orphan did not finish after peer EOF")
	}
	br.OnClose(src, io.EOF)
	if len(br.pairs) != 0 {
		t.Fatal("closed pair remained registered")
	}
}

type bridgeCloseRecorder struct {
	*Bridge
	closed chan error
}

func (r *bridgeCloseRecorder) OnClose(c uio.Conn, err error) {
	r.Bridge.OnClose(c, err)
	select {
	case r.closed <- err:
	default:
	}
}

func TestBridgeDialValidationErrorClosesUncommittedUpstream(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	accepted := make(chan net.Conn, 1)
	go func() {
		if c, err := ln.Accept(); err == nil {
			accepted <- c
		}
	}()
	cause := errors.New("test: validation after helper dial failed")
	var pairs atomic.Int64
	br := &Bridge{OnPair: func(uio.Conn, uio.Conn) { pairs.Add(1) }, Dial: func(ctx context.Context, _ uio.Conn, dial DialFunc) (uio.Conn, error) {
		up, err := dial(ctx, "tcp://"+ln.Addr().String(), uio.WithLayers(uio.PassThroughLayer))
		if err != nil {
			return nil, err
		}
		if pairs.Load() != 0 {
			return up, errors.New("test: paired before Dial succeeded")
		}
		return up, cause
	}}
	closed := make(chan error, 4)
	ev := br.Events()
	ev.Handler = &bridgeCloseRecorder{Bridge: br, closed: closed}
	listenAddr := bridgeServe(t, ev)
	legs := []net.Conn{}
	// The bridge can reset the client before connect has reported success
	// — the very close this test waits for — and connect then fails.
	client, err := net.Dial("tcp", listenAddr)
	if err == nil {
		t.Cleanup(func() { _ = client.Close() })
		legs = append(legs, client)
	} else if !errors.Is(err, syscall.ECONNRESET) {
		t.Fatal(err)
	}
	peer := bridgeAwait(t, accepted, "helper upstream")
	t.Cleanup(func() { _ = peer.Close() })
	legs = append(legs, peer)
	for _, c := range legs {
		_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
		if _, err := c.Read(make([]byte, 1)); err == nil {
			t.Fatal("validation failure left a leg open")
		} else if failure, ok := err.(net.Error); ok && failure.Timeout() {
			t.Fatal(err)
		}
	}
	for range 2 {
		if err := bridgeAwait(t, closed, "failed leg close"); !errors.Is(err, cause) {
			t.Fatalf("close=%v, want validation error", err)
		}
	}
	if pairs.Load() != 0 || len(br.pairs) != 0 {
		t.Fatal("failed Dial committed a pair")
	}
}

func TestBridgeZeroCreditProgressReevaluatesAWaitingSource(t *testing.T) {
	const size = 1024
	dst := newBridgeOwnedConn(nil)
	dst.creditBlocked.Store(true)
	dst.autoDrain.Store(true)
	br := &Bridge{InboundLimit: 2 * size}
	held, delivered := make(chan struct{}, 1), make(chan struct{}, 1)
	var announced, complete bool
	ev := &uio.Events{Pollers: 1, ReadBufferSize: size, MaxInboundBuffered: size, AllowHalfOpen: true, Handler: uio.FuncHandler{
		Open: func(c uio.Conn) {
			if err := br.Pair(c, dst); err != nil {
				_ = c.Abort(err)
			}
		},
		Read: func(c uio.Conn) error {
			if err := br.OnRead(c); err != nil {
				return err
			}
			if c.InboundBuffered() >= size && !announced {
				announced = true
				held <- struct{}{}
			}
			if dst.writtenBytes.Load() == size && !complete {
				complete = true
				delivered <- struct{}{}
			}
			return nil
		}, Close: br.OnClose,
	}}
	client, src := bridgeServeConn(t, ev)
	if _, err := client.Write(make([]byte, size)); err != nil {
		t.Fatal(err)
	}
	bridgeAwait(t, held, "input held by zero write credit")
	if src.ReadPaused() || dst.OutboundBuffered() != 0 {
		t.Fatal("test needs capacity hold without explicit pause or queued output")
	}
	dst.creditBlocked.Store(false)
	br.OnBytesWritten(dst, 0)
	bridgeAwait(t, delivered, "retained input after window credit reopened")
}

func TestBridgeBackpressureBoundsAndResumesASlowDestination(t *testing.T) {
	const high = 1024
	dst := newBridgeOwnedConn(nil)
	br := &Bridge{InboundLimit: high}
	paused, drained := make(chan struct{}, 1), make(chan int64, 1)
	var target, socketBytes atomic.Int64
	var announced, complete bool
	ev := &uio.Events{Pollers: 1, ReadBufferSize: high, MaxInboundBuffered: high, AllowHalfOpen: true,
		Observer: uio.FuncHandler{BytesRead: func(_ uio.Conn, n int) { socketBytes.Add(int64(n)) }},
		Handler: uio.FuncHandler{
			Open: func(c uio.Conn) {
				if err := br.Pair(c, dst); err != nil {
					_ = c.Abort(err)
				}
			},
			Read: func(c uio.Conn) error {
				if err := br.OnRead(c); err != nil {
					return err
				}
				if c.ReadPaused() && !announced {
					announced = true
					paused <- struct{}{}
				}
				if want := target.Load(); want != 0 && dst.writtenBytes.Load() == want && !complete {
					complete = true
					drained <- want
				}
				return nil
			}, Close: br.OnClose,
		}}
	client, src := bridgeServeConn(t, ev)
	// The first chunk fills the destination's queue; what follows waits in
	// the source until its bound is full, which pauses it.
	const attempted = 32 << 20
	sent := writeUntilStalled(t, client, attempted)
	if sent == attempted {
		t.Fatal("source never applied TCP backpressure")
	}
	bridgeAwait(t, paused, "pause of a source whose waiting input filled its bound")
	if socketBytes.Load() > 3*high || !src.ReadPaused() {
		t.Fatalf("source read %d bytes behind a full destination", socketBytes.Load())
	}
	target.Store(int64(sent))
	dst.autoDrain.Store(true)
	dst.queue.Store(0)
	br.OnBytesWritten(dst, 0)
	if got := bridgeAwait(t, drained, "complete relay after destination drain"); got != int64(sent) {
		t.Fatalf("forwarded=%d", got)
	}
	if src.ReadPaused() {
		t.Fatal("source did not resume")
	}
}

// A Dial override returning a connection another pair already relays fails
// the pairing; OnOpen resets the client and leaves that other pair alone.
func TestBridgeOnOpenLeavesAForeignPairAlone(t *testing.T) {
	x, shared, client := newBridgeOwnedConn(nil), newBridgeOwnedConn(nil), newBridgeOwnedConn(nil)
	br := &Bridge{}
	if err := br.Pair(x, shared); err != nil {
		t.Fatal(err)
	}
	br.Dial = func(context.Context, uio.Conn, DialFunc) (uio.Conn, error) { return shared, nil }
	br.OnOpen(client)
	if client.State()&uio.StateClosed == 0 {
		t.Fatal("the client of a failed pairing stayed open")
	}
	if shared.State()&uio.StateClosed != 0 || x.State()&uio.StateClosed != 0 || br.pairOf(x) == nil {
		t.Fatal("OnOpen aborted a leg that belongs to another live pair")
	}
}

func bridgeAwait[T any](t *testing.T, ch <-chan T, desc string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", desc)
		var zero T
		return zero
	}
}

func bridgeServeConn(t *testing.T, ev *uio.Events) (net.Conn, uio.Conn) {
	t.Helper()
	opened := make(chan uio.Conn, 1)
	orig := ev.Handler
	ev.Handler = uio.FuncHandler{
		Open: func(c uio.Conn) {
			opened <- c
			if orig != nil {
				orig.OnOpen(c)
			}
		},
		Read: func(c uio.Conn) error {
			if orig != nil {
				return orig.OnRead(c)
			}
			return nil
		},
		Timer: func(c uio.Conn) error {
			if orig != nil {
				return orig.OnTimer(c)
			}
			return nil
		},
		Close: func(c uio.Conn, err error) {
			if orig != nil {
				orig.OnClose(c, err)
			}
		},
	}
	addr := bridgeServe(t, ev)
	client := bridgeDial(t, addr)
	t.Cleanup(func() { _ = client.Close() })
	return client, bridgeAwait(t, opened, "accepted server connection")
}

func writeUntilStalled(t *testing.T, conn net.Conn, total int) int {
	t.Helper()
	chunk := make([]byte, 32<<10)
	sent := 0
	for sent < total {
		_ = conn.SetWriteDeadline(time.Now().Add(80 * time.Millisecond))
		n, err := conn.Write(chunk[:min(len(chunk), total-sent)])
		sent += n
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				_ = conn.SetWriteDeadline(time.Time{})
				return sent
			}
			t.Fatal(err)
		}
	}
	_ = conn.SetWriteDeadline(time.Time{})
	return sent
}

func TestBridgeUnsupportedCloseWriteKeepsReverseAndClosesOnEnd(t *testing.T) {
	a := newBridgeOwnedConn(nil)
	a.inbound = []byte("req")
	a.state.Store(uint32(uio.StateReadEnded))
	b := newBridgeOwnedConn(nil)
	b.writeCloseError = uio.ErrUnsupported

	br := &Bridge{}
	if err := br.Pair(a, b); err != nil {
		t.Fatal(err)
	}
	// OnRead forwarding EOF to a non-CloseWriter leg must NOT abort with ErrUnsupported!
	if err := br.OnRead(a); err != nil {
		t.Fatalf("OnRead with ErrUnsupported CloseWrite = %v, want nil", err)
	}
	if b.gracefulCloses.Load() != 0 {
		t.Fatal("non-CloseWriter leg closed prematurely during half-open OnRead")
	}
	// OnClose(nil) falls back to peer.Close() when CloseWrite returns ErrUnsupported!
	br.OnClose(a, io.EOF)
	if b.gracefulCloses.Load() != 1 {
		t.Fatal("OnClose did not call Close() when CloseWrite returned ErrUnsupported")
	}

	// Pairing with an already-closed peer returns ErrPeerGone cleanly!
	dead := newBridgeOwnedConn(nil)
	dead.state.Store(uint32(uio.StateClosed))
	live := newBridgeOwnedConn(nil)
	if err := br.Pair(live, dead); !errors.Is(err, ErrPeerGone) {
		t.Fatalf("Pair with closed conn = %v, want ErrPeerGone", err)
	}
}
