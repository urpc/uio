package ukcp

import (
	"bytes"
	"context"
	"errors"
	"net"
	"runtime"
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
	resolved, err := opts.resolve()
	if err != nil {
		t.Fatal(err)
	}
	ctx := &contractContext{lower: &contractLower{}, up: &contractUp{}}
	engine := newResolvedLayer(resolved, false).Attach(ctx).(*conn)
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

func TestStandaloneValidationAndFailedListenCleanup(t *testing.T) {
	for _, opts := range []Options{{MTU: -1}, {Interval: -time.Second}} {
		if err := opts.Validate(); !errors.Is(err, ErrInvalidOptions) {
			t.Fatal(err)
		}
	}
	saved := DefaultDialer
	DefaultDialer = NewDialer(Options{})
	defer func() { DefaultDialer = saved }()
	if _, err := Dial("127.0.0.1:1"); !errors.Is(err, ErrInvalidOptions) {
		t.Fatal(err)
	}
	for _, opts := range []Options{{Handler: echoHandler, Transport: TransportOptions{ReadBufferSize: 1}}, {Handler: echoHandler, MTU: -1}} {
		if _, err := NewDialer(opts).DialContext(context.Background(), "127.0.0.1:1"); !errors.Is(err, ErrInvalidOptions) {
			t.Fatal(err)
		}
	}
	d := NewDialer(Options{Handler: echoHandler})
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Dial("127.0.0.1:1"); !errors.Is(err, net.ErrClosed) {
		t.Fatal(err)
	}
	s := NewServer(Options{Handler: echoHandler, SocketReadBuffer: -1, SocketWriteBuffer: -1})
	if err := s.Serve("tcp://not-an-address"); err == nil {
		t.Fatal("invalid listener unexpectedly started")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	empty := NewServer(Options{})
	if err := empty.Close(); err != nil {
		t.Fatal(err)
	}
	if control := (Options{SocketReadBuffer: -1, SocketWriteBuffer: -1}).socketControl(); control != nil {
		t.Fatal("negative buffer settings did not leave the socket unchanged")
	}
	control := (Options{SocketReadBuffer: 1024, SocketWriteBuffer: -1}).socketControl()
	if runtime.GOOS != "windows" {
		// Windows applies no kernel socket buffers, so it takes any
		// descriptor and reports nothing to apply.
		if err := control("udp", "", ^uintptr(0)); err == nil {
			t.Fatal("invalid socket descriptor was accepted")
		}
	}
}
