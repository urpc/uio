package uws

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/urpc/uio"
	"github.com/urpc/uio/uws/internal/frame"
)

func TestPublicErrorsPreserveCauseAndDescribeRefusal(t *testing.T) {
	cause := errors.New("permission denied")
	for _, tc := range []struct {
		err  error
		text string
	}{
		{(*RejectError)(nil), "403"}, {&RejectError{Status: 429}, "429"},
		{&RejectError{Status: 503, Reason: "busy"}, "busy"},
		{&HandshakeError{StatusCode: 401}, "401"},
		{&HandshakeError{StatusCode: 401, Reason: "credentials expired"}, "credentials expired"},
		{&CloseError{}, "connection closed"},
		{&CloseError{CloseEvent: CloseEvent{Err: cause}}, "permission denied"},
		{&authorizeError{cause}, "permission denied"}, {&ownedProtocolClose{cause}, "permission denied"},
	} {
		if got := tc.err.Error(); !strings.Contains(got, tc.text) {
			t.Errorf("%T: %q does not contain %q", tc.err, got, tc.text)
		}
	}
	for _, err := range []error{&CloseError{CloseEvent: CloseEvent{Err: cause}}, &authorizeError{cause}, &ownedProtocolClose{cause}} {
		if !errors.Is(err, cause) {
			t.Fatalf("%T lost cause", err)
		}
	}
}

func TestUnopenedConnectionObservationsAndLateDeadlines(t *testing.T) {
	var absent *Conn
	if absent.Request() != nil {
		t.Fatal("nil connection has request")
	}
	c := new(Conn)
	if c.Request() != nil || c.Subprotocol() != "" || c.frameParserConfig() != nil || !c.utf8ValidationEnabled() || c.maxFramePayload() != DefaultMaxFramePayload || c.maxMessageSize() != DefaultMaxMessageSize || c.maxOutbound() != 0 || c.writeBufferedThreshold() != defaultWriteBufferedThreshold {
		t.Fatal("unopened connection defaults differ")
	}
	if _, ok := c.TLSState(); ok {
		t.Fatal("unopened connection has TLS")
	}
	c.raw = newScriptedConn()
	if _, ok := c.TLSState(); ok {
		t.Fatal("plain transport has TLS")
	}
	c.fireDeadline(deadlineKindHandshake, 1)
	c.closed.Store(true)
	c.fireDeadline(deadlineKindClose, 0)
	if c.raw.(*scriptedConn).closes != 0 {
		t.Fatal("late deadline closed an already closed connection")
	}
	state := &handshakeState{}
	c.handshake.Store(&handshakeState{})
	if c.handshakeHTTPRequest(state) != nil {
		t.Fatal("retired handshake returned a request")
	}
	var counters *statsCounters
	if counters.snapshot() != (Stats{}) {
		t.Fatal("nil counters are not zero")
	}
	d := new(Dialer)
	d.stats.paused.Add(2)
	d.stats.resumed.Add(1)
	if got := d.Stats(); got != (Stats{ReadsPaused: 2, ReadsResumed: 1}) {
		t.Fatal(got)
	}
}

func TestDialRequestFailuresRollbackAndPreserveCloseCause(t *testing.T) {
	cause := errors.New("transport failure")
	for _, tc := range []struct {
		name               string
		writeErr, flushErr error
		short              bool
		closes             int
		want               error
	}{
		{"write", cause, nil, false, 1, cause}, {"short", nil, nil, true, 1, io.ErrShortWrite},
		{"already-closed", net.ErrClosed, nil, false, 0, nil}, {"flush", nil, cause, false, 1, cause},
		{"closed-flush", nil, net.ErrClosed, false, 0, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := newScriptedConn()
			raw.writeErr = tc.writeErr
			raw.flushErr = tc.flushErr
			raw.short = tc.short
			raw.closeCause = make(chan error, 1)
			c := testServerConn(raw)
			new(Dialer).sendRequest(raw, &dialSetup{conn: c, request: []byte("upgrade")})
			if raw.closes != tc.closes {
				t.Fatalf("closes=%d", raw.closes)
			}
			if tc.want != nil {
				select {
				case got := <-raw.closeCause:
					if !errors.Is(got, tc.want) {
						t.Fatal(got)
					}
				default:
					t.Fatal("missing close cause")
				}
			}
			wantPending := int64(len("upgrade"))
			if tc.writeErr != nil {
				wantPending = 0
			}
			if tc.short {
				wantPending--
			}
			if got := c.writes.close.pendingBytes.Load(); got != wantPending {
				t.Fatalf("pending=%d want %d", got, wantPending)
			}
		})
	}
	new(Dialer).sendRequest(nil, &dialSetup{})
}

func TestPullClientCloseAndDeadlineLifecycle(t *testing.T) {
	client := newClient()
	if client.CloseEventInfo() != (CloseEvent{}) || client.closedConn() {
		t.Fatal("new client already closed")
	}
	client.queue <- Message{Type: BinaryMessage, Payload: []byte("queued")}
	if got, err := client.ReadMessage(nil); err != nil || string(got.Payload) != "queued" {
		t.Fatalf("message=%v err=%v", got, err)
	}
	client.readDeadline = time.Now().Add(time.Hour)
	client.expireReadDeadline()
	client.deadlineTimer = time.AfterFunc(time.Hour, func() {})
	client.stopDeadlines()
	if client.deadlineTimer != nil || !client.readDeadline.IsZero() {
		t.Fatal("deadline retained on shutdown")
	}
	close(client.ended)
	if !client.closedConn() {
		t.Fatal("closed client not observed")
	}
	d := new(Dialer)
	if err := d.Close(io.EOF); err != nil {
		t.Fatal(err)
	}
	d.trackClient(client)
	select {
	case <-client.stop:
	default:
		t.Fatal("registration after Dialer.Close did not release client")
	}
	d.clients.Delete(client)
}

type contractLayerDown struct {
	*scriptedConn
	state uio.ConnState
}

func (d *contractLayerDown) State() uio.ConnState { return d.state }

type contractLayerUp struct {
	uio.LayerUp
	bytes.Buffer
	opened int
}

func (u *contractLayerUp) Write(p []byte) (int, error) { return u.Buffer.Write(p) }
func (u *contractLayerUp) Open()                       { u.opened++ }

type contractLayerContext struct {
	uio.LayerContext
	down    *contractLayerDown
	up      *contractLayerUp
	aborted error
	wakes   int
}

func (c *contractLayerContext) Down() uio.LayerDown   { return c.down }
func (c *contractLayerContext) Up() uio.LayerUp       { return c.up }
func (c *contractLayerContext) Abort(err error) error { c.aborted = err; return nil }
func (c *contractLayerContext) Wake() error           { c.wakes++; return nil }

func TestWebSocketLayerStagedFramesAndTransportEnd(t *testing.T) {
	for _, tc := range []struct {
		name    string
		wire    []byte
		closing bool
		want    error
	}{
		{"staged-message", frame.Append(nil, frame.Frame{Fin: true, Opcode: frame.Binary, Masked: true, Payload: []byte("staged")}, [4]byte{1, 2, 3, 4}), false, nil},
		{"staged-invalid", []byte{0x83, 0}, false, frame.ErrProtocol},
		{"peer-ended", nil, false, io.ErrUnexpectedEOF}, {"peer-ended-after-close", nil, true, io.EOF},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := &contractLayerContext{down: &contractLayerDown{scriptedConn: newScriptedConn()}, up: new(contractLayerUp)}
			w := &wsLayerHandler{ctx: ctx, staged: tc.wire}
			c := testServerConn(ctx.down.scriptedConn)
			c.raw = ctx.down
			c.handler = layerSink{w}
			c.closing.Store(tc.closing)
			w.conn.Store(c)
			if tc.wire == nil {
				ctx.down.state = uio.StatePeerEnded
			}
			got := w.OnRead(ctx)
			if !errors.Is(got, tc.want) {
				t.Fatalf("read=%v want=%v", got, tc.want)
			}
			if tc.name == "staged-message" && ctx.up.String() != "staged" {
				t.Fatal(ctx.up.String())
			}
			if tc.name == "staged-invalid" && !protocolCloseOwnsTransport(got) {
				t.Fatal("protocol close lost transport ownership")
			}
			c.stopCloseTimer()
			c.releaseReadState()
		})
	}
	w := new(wsLayerHandler)
	if err := w.OnRead(nil); err != nil || w.Userdata() != nil || w.Subprotocol() != "" {
		t.Fatal("unopened layer observations differ")
	}
}

func TestClientLayerRequestWriteFailures(t *testing.T) {
	for _, tc := range []struct {
		name  string
		err   error
		short bool
		want  error
	}{
		{"failure", io.ErrClosedPipe, false, io.ErrClosedPipe}, {"short", nil, true, io.ErrShortWrite}, {"closed", net.ErrClosed, false, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := newScriptedConn()
			raw.writeErr = tc.err
			raw.short = tc.short
			ctx := &contractLayerContext{down: &contractLayerDown{scriptedConn: raw}, up: new(contractLayerUp)}
			layer := ClientLayer(LayerOptions{Target: "ws://localhost/"})
			handler := layer.Attach(ctx).(*wsLayerHandler)
			handler.OnOpen(ctx)
			if !errors.Is(ctx.aborted, tc.want) {
				t.Fatalf("abort=%v want=%v", ctx.aborted, tc.want)
			}
			handler.OnClose(ctx, io.EOF)
		})
	}
	if got := ServerLayer(LayerOptions{Target: "ws://localhost"}).(*wsLayer).err; !errors.Is(got, ErrInvalidOptions) {
		t.Fatal(got)
	}
	invalid := ClientLayer(LayerOptions{Target: "ws://localhost", Header: http.Header{"Upgrade": {"forbidden"}}}).(*wsLayer)
	if !errors.Is(invalid.err, ErrInvalidOptions) {
		t.Fatal(invalid.err)
	}
}

func TestTrackedFrameTransportErrors(t *testing.T) {
	raw := newScriptedConn()
	c := testServerConn(raw)
	if _, err := c.writeTrackedFrame(frame.Frame{Fin: true, Opcode: frame.Ping}); !errors.Is(err, uio.ErrUnsupported) {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		err, want error
		short     bool
	}{
		{uio.ErrOutboundOverflow, ErrBackpressure, false}, {net.ErrClosed, ErrClosed, false}, {io.ErrClosedPipe, io.ErrClosedPipe, false}, {nil, io.ErrShortWrite, true},
	} {
		raw := newScriptedConn()
		raw.writeErr = tc.err
		raw.short = tc.short
		c := testServerConn(raw)
		c.setRaw(&contractTrackedWire{scriptedConn: raw})
		if _, err := c.writeTrackedFrame(frame.Frame{Fin: true, Opcode: frame.Ping}); !errors.Is(err, tc.want) {
			t.Fatalf("error=%v want=%v", err, tc.want)
		}
	}
}

type contractTrackedWire struct{ *scriptedConn }

func (w *contractTrackedWire) WriteTracked(p []byte) (int, uio.SendReceipt, error) {
	n, err := w.Write(p)
	return n, uio.SendReceipt{}, err
}

// A pull reader awaiting the final close event must still obey cancellation.
func TestPullReadCancellationWhileCloseFinishes(t *testing.T) {
	client := newClient()
	client.finish()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := client.ReadMessage(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestLifecycleOptionsAndClosedControlContracts(t *testing.T) {
	for _, opts := range []Options{{HeartbeatTimeout: -1}, {MaxOutboundBuffered: -1}, {MaxOutboundBuffered: 10, OutboundHighWatermark: 11}} {
		if _, err := opts.resolve(); !errors.Is(err, ErrInvalidOptions) {
			t.Fatalf("options=%+v err=%v", opts, err)
		}
	}
	if got := tlsLayerOptions(nil, Options{HandshakeTimeout: -1}).HandshakeTimeout; got != 0 {
		t.Fatal(got)
	}
	if readControlError(net.ErrClosed) != ErrClosed {
		t.Fatal("transport close not translated")
	}
	var absent *Conn
	if absent.OutboundBuffered() != 0 || new(Conn).OutboundBuffered() != 0 {
		t.Fatal("unattached output")
	}
	c := testServerConn(newScriptedConn())
	if c.setInternalReadPause(false) {
		t.Fatal("unpaused control changed")
	}
	c.closed.Store(true)
	if !errors.Is(c.notifyMessage(Message{}), ErrClosed) {
		t.Fatal("message delivered to closed connection")
	}
	if !errors.Is(c.acceptFrame(frame.Frame{}), ErrClosed) {
		t.Fatal("closed connection accepted frame")
	}
	c.closing.Store(true)
	c.ensureReadPauses()
	if c.setInternalReadPause(true) {
		t.Fatal("closing connection acquired a pause")
	}
	if err := c.Close(1000, ""); err != nil {
		t.Fatal(err)
	}
	if err := new(Conn).sendPong(nil); !errors.Is(err, ErrNotReady) {
		t.Fatal(err)
	}
	if err := new(Conn).sendCloseFrame(nil); !errors.Is(err, ErrNotReady) {
		t.Fatal(err)
	}
	if got := new(Conn).batchBlockSize(); got != (Options{}).batchBlockSize() {
		t.Fatal(got)
	}
}

func TestDeferredCloseErrorTerminatesExactlyOnce(t *testing.T) {
	for _, tracked := range []bool{false, true} {
		for _, cause := range []error{io.ErrClosedPipe, net.ErrClosed, nil} {
			raw := newScriptedConn()
			raw.writeErr = cause
			c := testServerConn(raw)
			if tracked {
				c.setRaw(&contractTrackedWire{raw})
			}
			if !c.writes.close.deferClose([]byte{3, 232}) {
				t.Fatal("deferred close refused")
			}
			c.sendDeferredClose()
			c.sendDeferredClose()
			if cause == io.ErrClosedPipe && raw.closes != 1 {
				t.Fatalf("tracked=%v closes=%d", tracked, raw.closes)
			}
			if cause == nil && !c.writes.close.closeFrameWasSent() {
				t.Fatal("successful deferred close not marked sent")
			}
		}
	}
}

func TestCloseReceiptCanArriveBeforeRegistration(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	events, _ := layerEvents(t, newStreamPeer(false), false, uio.PassThroughLayer)
	conn := dialLayer(t, events, listener.Addr().String())
	peer, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	n, receipt, err := conn.(uio.TrackedWriter).WriteTracked([]byte("receipt"))
	if err != nil || n != 7 || receipt == (uio.SendReceipt{}) {
		t.Fatalf("write=%d receipt=%v err=%v", n, receipt, err)
	}
	state := new(connCloseProgress)
	if state.claimReceipt(receipt) {
		t.Fatal("unregistered receipt claimed a close")
	}
	state.requestTransportClose()
	if state.claimPendingTransportClose(true) {
		t.Fatal("unregistered close completed")
	}
	state.setReceipt(receipt)
	if !state.claimPendingTransportClose(true) || state.claimPendingTransportClose(true) {
		t.Fatal("early receipt did not complete close exactly once")
	}
	empty := new(connCloseProgress)
	empty.setReceipt(uio.SendReceipt{})
	empty.requestTransportClose()
	if !empty.claimPendingTransportClose(true) {
		t.Fatal("empty tracked write waited for a receipt")
	}
}

func TestDeferredClientClosePreservesMaskAndTracking(t *testing.T) {
	raw := newScriptedConn()
	c := &Conn{raw: raw, config: testDialerConfig(nil)}
	c.setRaw(&contractTrackedWire{raw})
	c.opened.Store(true)
	c.writes.close.deferClose([]byte{3, 232})
	c.sendDeferredClose()
	if len(raw.written) != 1 {
		t.Fatalf("writes=%d", len(raw.written))
	}
	f, _, complete, err := frame.ParseFrame(raw.written[0], &frame.ParserConfig{ExpectMask: true})
	if err != nil || !complete || !f.Masked || f.Opcode != frame.Close || !bytes.Equal(f.Payload, []byte{3, 232}) {
		t.Fatalf("frame=%+v complete=%v err=%v", f, complete, err)
	}
	if !c.writes.close.closeFrameWasSent() {
		t.Fatal("close receipt not registered")
	}
}
