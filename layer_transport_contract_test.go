package uio

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"reflect"
	"strings"
	"testing"
	"time"
)

// contractTransport isolates the layer-facing connection contract: requests
// reach the underlying transport, while userdata, input, timers and receipts
// remain owned by the stack. Errors are injected at that transport boundary.
type contractTransport struct {
	optimizationSocket
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

func (c *contractTransport) LocalAddr() net.Addr  { return c.local }
func (c *contractTransport) RemoteAddr() net.Addr { return c.remote }
func (c *contractTransport) ReadsPaused() bool    { return false }
func (c *contractTransport) State() ConnState     { return c.state }
func (c *contractTransport) Wake() error          { c.wakes++; return c.wakeError }
func (c *contractTransport) Write(p []byte) (int, error) {
	if c.writeError != nil {
		return 0, c.writeError
	}
	return c.optimizationSocket.Write(p)
}
func (c *contractTransport) WriteString(p string) (int, error) { return c.Write([]byte(p)) }
func (c *contractTransport) WriteByte(b byte) error            { _, err := c.Write([]byte{b}); return err }
func (c *contractTransport) ReserveOutbound(n int) ([]byte, error) {
	if c.reserveError != nil {
		return nil, c.reserveError
	}
	c.reserved = make([]byte, n)
	return c.reserved, nil
}
func (c *contractTransport) CloseWith(cause error) error {
	c.closeCause = cause
	c.state |= StateClosing | StateClosed
	return nil
}
func (c *contractTransport) CloseGracefully(cause error) error {
	c.graceful = true
	return c.CloseWith(cause)
}
func (c *contractTransport) CloseWrite() error    { c.state |= StateWriteShut; return nil }
func (c *contractTransport) InboundBuffered() int { return len(c.input) }
func (c *contractTransport) PeekChunk() []byte    { return c.input }
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
func (c *contractTransport) Control(fn func(uintptr)) error { fn(42); return nil }

func contractStack(transport *contractTransport, top Handler, handlers ...LayerHandler) *stack {
	layers := make([]Layer, len(handlers))
	for i, handler := range handlers {
		layers[i] = LayerFunc(func(LayerContext) LayerHandler { return handler })
	}
	(&stackDriver{layers: layers, top: top, topSent: sentNoticeOf(top)}).OnOpen(transport)
	return transport.userdata.(*stack)
}

func TestLayerTransportOptionsAndInputStayAtTheirBoundary(t *testing.T) {
	transport := &contractTransport{local: &net.TCPAddr{Port: 12}, remote: &net.TCPAddr{Port: 34}}
	c := contractStack(transport, FuncHandler{}, passThroughHandler{}, passThroughHandler{})
	top := c.topConn().(*layerNode)
	if top.RemoteAddr().String() != transport.remote.String() || top.LocalAddr().String() != transport.local.String() {
		t.Fatal("layer connection changed transport addresses")
	}
	for _, set := range []func() error{
		func() error { return top.SetNoDelay(true) }, func() error { return top.SetKeepAlive(false) },
		func() error { return top.SetKeepAlivePeriod(7) }, func() error { return top.SetReadBuffer(11) },
		func() error { return top.SetWriteBuffer(13) }, func() error { return top.SetDeadline(time.Unix(17, 0)) },
		func() error { return top.SetReadDeadline(time.Unix(19, 0)) }, func() error { return top.SetWriteDeadline(time.Unix(23, 0)) },
	} {
		if err := set(); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{"delay:true", "keep:false", "period:7", "read:11", "write:13", "both:17", "rd:19", "wd:23"}
	if !reflect.DeepEqual(transport.options, want) {
		t.Fatalf("transport options = %v, want %v", transport.options, want)
	}
	if err := top.Control(func(fd uintptr) {
		if fd != 42 {
			t.Errorf("Control fd = %d", fd)
		}
	}); err != nil {
		t.Fatal(err)
	}
	top.SetUserdata("application")
	if top.Userdata() != "application" || c.layers[0].Userdata() != "application" || transport.Userdata() != c {
		t.Fatal("application userdata escaped into the transport's stack slot")
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
	if _, err := top.ReserveOutbound(1); !errors.Is(err, ErrReserveUnsupported) {
		t.Fatalf("layer outbound reservation = %v", err)
	}
	c.inLimit = 3
	if err := top.ReserveInbound(3); err != nil {
		t.Fatal(err)
	}
	if err := top.ReserveInbound(4); !errors.Is(err, ErrInboundOverflow) {
		t.Fatalf("inbound reservation = %v", err)
	}
	up := layerUp{top}
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
	room, err := lower.ReserveOutbound(4)
	if err != nil || len(room) != 4 {
		t.Fatalf("ReserveOutbound = %d, %v", len(room), err)
	}
	copy(room, "defg")
	if c.wireQueued.Load() != 7 {
		t.Fatalf("wire queue position = %d, want 7", c.wireQueued.Load())
	}
	failure := errors.New("transport refused")
	transport.writeError, transport.reserveError = failure, failure
	if !errors.Is(lower.WriteByte('x'), failure) {
		t.Fatal("byte-write error disappeared")
	}
	if _, err := lower.ReserveOutbound(5); !errors.Is(err, failure) {
		t.Fatal("reservation error disappeared")
	}
	if n, r, err := lower.WriteTracked([]byte("x")); n != 0 || r != (SendReceipt{}) || !errors.Is(err, failure) {
		t.Fatalf("failed receipt = %d, %+v, %v", n, r, err)
	}
	if c.wireQueued.Load() != 7 {
		t.Fatal("refused bytes advanced the wire position")
	}
	transport.writeError = nil
	if n, r, err := lower.WriteTracked(nil); n != 0 || r != (SendReceipt{}) || err != nil {
		t.Fatal("empty write created a receipt")
	}
	if n, r, err := lower.WriteTracked([]byte("h")); n != 1 || r.wire != 8 || err != nil {
		t.Fatalf("tracked byte = %d, %+v, %v", n, r, err)
	}
	if c.receiptCount.Load() != 1 {
		t.Fatal("successful tracked output lost its receipt")
	}
	c.dropReceipts()
	if c.receiptCount.Load() != 0 || len(c.receipts) != 0 {
		t.Fatal("close retained an outstanding receipt")
	}
	fd := &fdConn{commonConn: commonConn{entry: (&Events{readBufferSize: 8192}).plainEntry()}}
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
			top := FuncHandler{
				Timer: func(c Conn) error {
					calls = append(calls, "top-timer")
					if phase == "top-timer-close" {
						return c.CloseWith(cause)
					}
					return cause
				},
				Read: func(Conn) error { calls = append(calls, "top-read"); return cause },
			}
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
	for _, set := range []func() error{
		func() error { return top.SetWakeInterval(time.Hour) },
		func() error { return lower.SetWakeInterval(2 * time.Hour) },
		func() error { return lower.SetWakeDeadline(time.Now().Add(3 * time.Hour)) },
	} {
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
	if _, err := (layerUp{top}).Write([]byte("remaining")); err != nil {
		t.Fatal(err)
	}
	top.ended = true
	if top.State()&StatePeerEnded != 0 {
		t.Fatal("buffered layer reported a consumed end")
	}
	if _, err := top.Discard(-1); err != nil {
		t.Fatal(err)
	}
	if top.State()&StatePeerEnded == 0 {
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
	if err := top.PauseReads(); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("closed pause = %v", err)
	}
	if err := top.ResumeReads(); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("closed resume = %v", err)
	}
}

func TestLargeLayerDeliveryReleasesConsumedStorage(t *testing.T) {
	c := contractStack(&contractTransport{}, FuncHandler{}, passThroughHandler{})
	top := c.topConn().(*layerNode)
	up := layerUp{top}
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
