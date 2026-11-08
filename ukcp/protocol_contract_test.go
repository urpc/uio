package ukcp

import (
	"bytes"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/urpc/uio"
	"github.com/urpc/uio/ukcp/internal/kcp"
)

type contractLower struct {
	uio.Lower
	inbound []byte
	wire    [][]byte
}

func (l *contractLower) PeekChunk() []byte { return l.inbound }
func (l *contractLower) Discard(n int) (int, error) {
	l.inbound = l.inbound[n:]
	return n, nil
}
func (l *contractLower) Writev(p [][]byte) (int, error) {
	n := 0
	for _, packet := range p {
		l.wire = append(l.wire, bytes.Clone(packet))
		n += len(packet)
	}
	return n, nil
}
func (*contractLower) PacketSource() bool   { return true }
func (*contractLower) State() uio.ConnState { return 0 }

type contractUp struct {
	uio.Upper
	data   []byte
	opened bool
}

func (u *contractUp) Buffered() int               { return len(u.data) }
func (u *contractUp) Write(p []byte) (int, error) { u.data = append(u.data, p...); return len(p), nil }

type contractContext struct {
	uio.LayerContext
	lower    *contractLower
	up       *contractUp
	cause    error
	deadline time.Time
}

func (c *contractContext) Lower() uio.Lower              { return c.lower }
func (c *contractContext) Upper() uio.Upper              { return c.up }
func (c *contractContext) Open()                         { c.up.opened = true }
func (c *contractContext) Abort(err error) error         { c.cause = err; return nil }
func (c *contractContext) SetTimer(when time.Time) error { c.deadline = when; return nil }

func newContractSession(t *testing.T, opts Options) (*conn, *contractContext) {
	t.Helper()
	ctx := &contractContext{lower: &contractLower{}, up: &contractUp{}}
	engine := newLayer(opts, false).Attach(ctx).(*conn)
	engine.OnOpen(ctx)
	return engine, ctx
}

func TestProtocolRejectsNoiseAndClosedSessionOperations(t *testing.T) {
	c, ctx := newContractSession(t, Options{KeepAlive: -1})
	ctx.lower.inbound = kcpSegment(7, 255, 0, 32, 0, 0, nil)
	if err := c.OnRead(ctx); err != nil || c.kcp != nil || ctx.up.opened {
		t.Fatal("unknown command founded a conversation")
	}
	ctx.lower.inbound = kcpSegment(7, kcp.IKCP_CMD_PUSH, 0, 32, 0, 0, []byte("x"))
	if err := c.OnRead(ctx); err != nil || !ctx.up.opened || string(ctx.up.data) != "x" {
		t.Fatal("a valid initial segment did not establish the stream")
	}
	c.OnClose(ctx, nil)
	ctx.lower.inbound = []byte("late packet")
	if err := c.OnRead(ctx); err != nil {
		t.Fatal(err)
	}
	if err := c.OnTimer(ctx); err != nil {
		t.Fatal(err)
	}
	if n, err := c.OnWrite(ctx, []byte("late output")); n != 0 || !errors.Is(err, net.ErrClosed) {
		t.Fatalf("closed write=%d,%v", n, err)
	}
	if err := c.CloseWrite(ctx); err != nil {
		t.Fatal(err)
	}
	c.output([]byte("late flush"), 10)
	c.deliver()
	if c.State() != (SessionState{}) || c.OutboundHeld(ctx) != 0 || len(c.outVec) != 0 {
		t.Fatal("late callbacks resurrected ended protocol state")
	}
}

func TestProtocolSplitsLargeWritesAtEngineLimit(t *testing.T) {
	c, ctx := newContractSession(t, Options{KeepAlive: -1, SendWindow: 1024, MaxBuffered: 2 << 20})
	c.establish(7)
	payload := bytes.Repeat([]byte("x"), c.mss*255+7)
	if n, err := c.OnWrite(ctx, payload); n != len(payload) || err != nil {
		t.Fatalf("large valid write=%d,%v", n, err)
	}
	if c.State().Queued == 0 || c.OutboundHeld(ctx) == 0 {
		t.Fatal("accepted bytes outside the peer's send window were not accounted")
	}
	if len(ctx.lower.wire) == 0 {
		t.Fatal("large application output never reached its transport")
	}
	c.OnClose(ctx, nil)
}

func TestReopenedWindowPropagatesDeadLinkAndClearsHeldState(t *testing.T) {
	c, ctx := newContractSession(t, Options{KeepAlive: -1, DeadLink: 1})
	c.establish(7)
	ctx.lower.inbound = kcpSegment(7, kcp.IKCP_CMD_WINS, 0, 0, 0, 0, nil)
	_ = c.OnRead(ctx)
	if n, err := c.OnWrite(ctx, []byte("queued")); n != 6 || err != nil {
		t.Fatalf("queued write=%d,%v", n, err)
	}
	ctx.lower.inbound = kcpSegment(7, kcp.IKCP_CMD_WINS, 0, 32, 0, 0, nil)
	_ = c.OnRead(ctx)
	if !errors.Is(ctx.cause, ErrDeadLink) || c.OutboundHeld(ctx) != 0 || c.State() != (SessionState{}) {
		t.Fatal("engine failure during a reopened window was not propagated")
	}
}

func TestOptionsValidationAndOnClosingContracts(t *testing.T) {
	for _, opts := range []Options{
		{MTU: -1},
		{MTU: 20},
		{MTU: 1600},
		{SendWindow: -1},
		{SendWindow: 70000},
		{Interval: -time.Second},
		{Interval: 5 * time.Millisecond},
		{DeadLink: -1},
		{MaxBuffered: -1},
	} {
		if err := opts.Validate(); !errors.Is(err, ErrInvalidOptions) {
			t.Fatalf("opts %+v Validate = %v, want ErrInvalidOptions", opts, err)
		}
	}
	if err := (Options{Resend: -1, HandshakeTimeout: -1, KeepAlive: -1}).Validate(); err != nil {
		t.Fatal(err)
	}
	ServerLayer(Options{}).(uio.PacketLayer).CarriesPackets()
	badCtx := &contractContext{lower: &contractLower{}, up: &contractUp{}}
	ServerLayer(Options{MTU: -1}).Attach(badCtx).OnOpen(badCtx)
	if !errors.Is(badCtx.cause, ErrInvalidOptions) {
		t.Fatalf("invalid layer OnOpen cause = %v, want ErrInvalidOptions", badCtx.cause)
	}

	// Verify OnClosing flushes deferred ACKs when finQueued is already true, and skips FIN on Abort!
	cFin, ctxFin := newContractSession(t, Options{KeepAlive: -1})
	cFin.establish(9)
	_ = cFin.CloseWrite(ctxFin)
	ctxFin.lower.wire = nil
	cFin.kcp.AskWindow()
	cFin.OnClosing(ctxFin, nil)
	if len(ctxFin.lower.wire) == 0 {
		t.Fatal("OnClosing after CloseWrite dropped pending control flush")
	}
	cAbort, ctxAbort := newContractSession(t, Options{KeepAlive: -1})
	cAbort.establish(10)
	ctxAbort.lower.wire = nil
	cAbort.OnClosing(ctxAbort, errors.New("abort"))
	if len(ctxAbort.lower.wire) != 0 {
		t.Fatal("OnClosing with abort cause emitted clean FIN segment")
	}

	// Verify SelfPaced, full-window sentinel OutboundHeld, and OnTurnEnd batch deferral!
	cFull, ctxFull := newContractSession(t, Options{KeepAlive: -1, SendWindow: 1})
	cFull.establish(11)
	if !cFull.SelfPaced() {
		t.Fatal("SelfPaced returned false")
	}
	_, _ = cFull.OnWrite(ctxFull, []byte("fill"))
	if cFull.OutboundHeld(ctxFull) == 0 {
		t.Fatal("full SendWindow reported OutboundHeld == 0")
	}
	cFull.turnDirty = true
	cFull.OnTurnEnd(ctxFull)
	// Test output multi-datagram prevStart (n > 1), publishState on ended, and BSD socket buffer clamping!
	cFull.output(make([]byte, cFull.opts.MTU), cFull.opts.MTU)
	cFull.output([]byte("d2a"), 3)
	cFull.output([]byte("d2b"), 3) // n == 2 -> prevStart = outEnds[0], coalesces into 2nd datagram!
	cFull.noteHeld()               // iterates outEnds in noteHeld!
	cFull.OnClose(ctxFull, nil)
	cFull.publishState()
}
