package uio

import (
	"bytes"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urpc/uio/internal/readcontrol"
)

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
	return &readControlProbe{
		opened:   make(chan LayerContext, 1),
		progress: make(chan int, 8),
		timers:   make(chan struct{}, 8),
		sent:     make(chan int, 8),
	}
}

func (p *readControlProbe) OnOpen(ctx LayerContext) {
	if p.pauseOnOpen {
		if err := ctx.Down().PauseReads(); err != nil {
			_ = ctx.Abort(err)
			return
		}
	}
	p.opened <- ctx
	ctx.Up().Open()
}

func (p *readControlProbe) OnRead(ctx LayerContext) error {
	n := ctx.Down().InboundBuffered()
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

func (p *readControlProbe) OnWireSent(_ LayerContext, n int) {
	select {
	case p.sent <- n:
	default:
	}
}

// Pausing one layer's consumer must not pause the lower producer, another
// boundary's controller, timers, or the stack work that retires sent writes.
func TestReadControlLayerPauseIsScopedAndKeepsControlWorkAlive(t *testing.T) {
	lower, upper := newReadControlProbe(), newReadControlProbe()
	upper.pauseOnOpen = true
	reads := make(chan string, 8)
	receipts := make(chan SendReceipt, 8)
	ev := &Events{Pollers: 1, Handler: FuncHandler{
		Read: func(c Conn) error {
			if n := c.InboundBuffered(); n != 0 {
				p := make([]byte, n)
				_, _ = c.Read(p)
				reads <- string(p)
			}
			return nil
		},
		Sent: func(_ Conn, r SendReceipt) { receipts <- r },
	}}
	client, server := readControlServe(t, ev,
		LayerFunc(func(LayerContext) LayerHandler { return lower }),
		LayerFunc(func(LayerContext) LayerHandler { return upper }))
	lowerCtx := readControlAwait(t, lower.opened, "lower layer open")
	upperCtx := readControlAwait(t, upper.opened, "upper layer open")
	if !upperCtx.Down().ReadsPaused() || lowerCtx.Down().ReadsPaused() || server.ReadsPaused() {
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
	if _, _, err := server.(TrackedWriter).WriteTracked([]byte("reply")); err != nil {
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
	readControlAwait(t, lower.sent, "lower send report")
	readControlAwait(t, upper.sent, "paused upper layer send report")
	readControlAwait(t, receipts, "tracked write receipt while paused")
	if err := upperCtx.Wake(); err != nil {
		t.Fatal(err)
	}
	readControlQuiet(t, reads, "input delivery through a paused layer")
	if err := upperCtx.Down().ResumeReads(); err != nil {
		t.Fatal(err)
	}
	if got := readControlAwait(t, reads, "layer-buffered input after resume"); got != "layered" {
		t.Fatalf("input = %q, want layered", got)
	}
}

// The first layer's Down face wraps the socket. It must clear exactly the
// same resume barrier as later layer boundaries, for both application and
// independently owned pauses, then continue reading future socket input.
func TestReadControlBottomLayerResumeContinuesSocketReads(t *testing.T) {
	probe := newReadControlProbe()
	probe.pauseOnOpen = true
	reads := make(chan string, 8)
	ev := &Events{Pollers: 1, Handler: FuncHandler{Read: func(c Conn) error {
		if n := c.InboundBuffered(); n != 0 {
			p := make([]byte, n)
			_, _ = c.Read(p)
			reads <- string(p)
		}
		return nil
	}}}
	client, server := readControlServe(t, ev, LayerFunc(func(LayerContext) LayerHandler { return probe }))
	ctx := readControlAwait(t, probe.opened, "bottom layer open")
	if !ctx.Down().ReadsPaused() || server.ReadsPaused() {
		t.Fatal("the bottom input pause was not scoped to the bottom consumer")
	}
	if _, err := client.Write([]byte("first")); err != nil {
		t.Fatal(err)
	}
	if err := ctx.Down().ResumeReads(); err != nil {
		t.Fatal(err)
	}
	if got := readControlAwait(t, reads, "socket input after bottom application resume"); got != "first" {
		t.Fatalf("first input = %q, want first", got)
	}
	owner := readcontrol.NewPause(ctx.Down())
	if err := owner.Pause(); err != nil {
		t.Fatal(err)
	}
	if err := ctx.Down().ResumeReads(); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Write([]byte("second")); err != nil {
		t.Fatal(err)
	}
	if err := ctx.Wake(); err != nil {
		t.Fatal(err)
	}
	readControlQuiet(t, reads, "socket input during bottom owner pause")
	if err := owner.Resume(); err != nil {
		t.Fatal(err)
	}
	if got := readControlAwait(t, reads, "socket input after bottom owner resume"); got != "second" {
		t.Fatalf("second input = %q, want second", got)
	}
	if _, err := client.Write([]byte("third")); err != nil {
		t.Fatal(err)
	}
	if got := readControlAwait(t, reads, "future socket input after both resumes"); got != "third" {
		t.Fatalf("third input = %q, want third", got)
	}
}

// A resume must deliver retained data before a later socket read can discover
// EOF. Otherwise a peer's FIN can close the stream and discard the held tail.
func TestReadControlResumeDeliversBufferedInputBeforeEOF(t *testing.T) {
	for _, layered := range []bool{false, true} {
		name := "plain"
		var layers []Layer
		if layered {
			name = "layered"
			layers = []Layer{PassThroughLayer, PassThroughLayer}
		}
		t.Run(name, func(t *testing.T) {
			const payload = "retained-before-peer-FIN"
			paused := make(chan error, 1)
			timers := make(chan struct{}, 1)
			reads := make(chan string, 8)
			closed := make(chan error, 1)
			var held bool
			ev := &Events{Pollers: 1, MaxBufferSize: 64, Handler: FuncHandler{
				Read: func(c Conn) error {
					if !held {
						if c.InboundBuffered() < len(payload) {
							return nil
						}
						_, _ = c.Discard(1)
						held = true
						paused <- c.PauseReads()
						return nil
					}
					if n := c.InboundBuffered(); n != 0 {
						p := make([]byte, n)
						_, _ = c.Read(p)
						reads <- string(p)
					}
					return nil
				},
				Timer: func(Conn) error { timers <- struct{}{}; return nil },
				Close: func(_ Conn, err error) { closed <- err },
			}}
			client, server := readControlServe(t, ev, layers...)
			if _, err := client.Write([]byte(payload)); err != nil {
				t.Fatal(err)
			}
			if err := readControlAwait(t, paused, "pause retaining input"); err != nil {
				t.Fatal(err)
			}
			if err := server.SetWakeDeadline(time.Now().Add(10 * time.Millisecond)); err != nil {
				t.Fatal(err)
			}
			readControlAwait(t, timers, "control turn after the paused callback returned")
			if err := client.(interface{ CloseWrite() error }).CloseWrite(); err != nil {
				t.Fatal(err)
			}
			if err := server.Wake(); err != nil {
				t.Fatal(err)
			}
			readControlQuiet(t, reads, "retained input before resume")
			select {
			case err := <-closed:
				t.Fatalf("closed with retained input still paused: %v", err)
			default:
			}
			if err := server.ResumeReads(); err != nil {
				t.Fatal(err)
			}
			if got := readControlAwait(t, reads, "retained input before EOF"); got != payload[1:] {
				t.Fatalf("retained input = %q, want %q", got, payload[1:])
			}
			readControlAwait(t, closed, "EOF after the retained input was delivered")
		})
	}
}

// The transport's two halves can end before a paused upper consumer drains.
// Socket-level emptiness is then insufficient to close the entire stack.
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
			ev := &Events{Pollers: 1, AllowHalfOpen: halfOpen, Handler: FuncHandler{
				Open: func(c Conn) {
					err := c.PauseReads()
					if err == nil {
						err = c.CloseWrite()
					}
					paused <- err
				},
				Read: func(c Conn) error {
					if n := c.InboundBuffered(); n != 0 {
						p := make([]byte, n)
						_, _ = c.Read(p)
						reads <- string(p)
					}
					return nil
				},
				Close: func(_ Conn, err error) { closed <- err },
			}}
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
			if err := server.ResumeReads(); err != nil {
				t.Fatal(err)
			}
			if got := readControlAwait(t, reads, "held input after both socket halves ended"); got != "held-after-local-FIN" {
				t.Fatalf("held input = %q", got)
			}
			readControlAwait(t, closed, "connection close after its upper consumer drained")
		})
	}
}

// Explicitly yielded processing remains owed even after the peer's EOF. A
// resumed parser may need several budgeted callbacks to drain retained frames.
func TestReadControlResumeHonorsYieldedInputBeforeEOF(t *testing.T) {
	for _, layered := range []bool{false, true} {
		name := "plain"
		var layers []Layer
		if layered {
			name = "layered"
			layers = []Layer{PassThroughLayer, PassThroughLayer}
		}
		t.Run(name, func(t *testing.T) {
			const payload = "retained-parser-batches"
			paused := make(chan error, 1)
			reads := make(chan string, 1)
			closed := make(chan error, 1)
			var initial = true
			var received bytes.Buffer
			ev := &Events{Pollers: 1, Handler: FuncHandler{
				Read: func(c Conn) error {
					if initial {
						if c.InboundBuffered() < len(payload) {
							return nil
						}
						initial = false
						paused <- c.PauseReads()
						return nil
					}
					if c.InboundBuffered() == 0 {
						return nil
					}
					p := make([]byte, 1)
					_, _ = c.Read(p)
					received.Write(p)
					if c.InboundBuffered() != 0 {
						return c.YieldRead()
					}
					reads <- received.String()
					return nil
				},
				Close: func(_ Conn, err error) { closed <- err },
			}}
			client, server := readControlServe(t, ev, layers...)
			if _, err := client.Write([]byte(payload)); err != nil {
				t.Fatal(err)
			}
			if err := readControlAwait(t, paused, "pause before parser batches"); err != nil {
				t.Fatal(err)
			}
			if err := client.(interface{ CloseWrite() error }).CloseWrite(); err != nil {
				t.Fatal(err)
			}
			readControlQuiet(t, reads, "parser delivery before resume")
			if err := server.ResumeReads(); err != nil {
				t.Fatal(err)
			}
			select {
			case got := <-reads:
				if got != payload {
					t.Fatalf("parser batches = %q, want %q", got, payload)
				}
			case err := <-closed:
				// Both channels can be ready after a correct final callback.
				select {
				case got := <-reads:
					if got != payload {
						t.Fatalf("parser batches = %q, want %q", got, payload)
					}
				default:
					t.Fatalf("EOF overtook explicitly yielded input: %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("yielded parser batches did not finish")
			}
		})
	}
}

// Even with the ordinary inbound cap disabled, an explicitly paused layered
// consumer must eventually exert TCP backpressure rather than moving an
// unlimited stream into the raw socket's user-space buffer.
func TestReadControlPausedLayerHasABoundedReceiveBacklog(t *testing.T) {
	paused := make(chan error, 1)
	drained := make(chan int64, 1)
	var socketBytes atomic.Int64
	var want atomic.Int64
	var received int64
	var complete bool
	ev := &Events{Pollers: 1, MaxBufferSize: 4 << 10, Observer: FuncHandler{
		BytesRead: func(_ Conn, n int) { socketBytes.Add(int64(n)) },
	}, Handler: FuncHandler{
		Open: func(c Conn) { paused <- c.PauseReads() },
		Read: func(c Conn) error {
			n, err := c.Discard(-1)
			received += int64(n)
			if received == want.Load() && received != 0 && !complete {
				complete = true
				drained <- received
			}
			return err
		},
	}}
	client, server := readControlServe(t, ev, PassThroughLayer, PassThroughLayer)
	if err := readControlAwait(t, paused, "top consumer pause"); err != nil {
		t.Fatal(err)
	}
	const attempted = 32 << 20
	sent := writeUntilStalled(t, client, attempted)
	if sent == attempted {
		t.Fatal("the paused stack never applied TCP backpressure")
	}
	if got := socketBytes.Load(); got > 4*DefaultDeliverBound {
		t.Fatalf("the paused stack read %d bytes into user space with a %d-byte default bound", got, DefaultDeliverBound)
	}
	want.Store(int64(sent))
	if err := server.ResumeReads(); err != nil {
		t.Fatal(err)
	}
	if got := readControlAwait(t, drained, "all accepted input after the paused layer resumed"); got != int64(sent) {
		t.Fatalf("read %d bytes, want %d", got, sent)
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
	ev := &Events{Pollers: 1, MaxBufferSize: 512, MaxInboundBuffered: limit, Handler: FuncHandler{
		Read: func(c Conn) error {
			if !consume.Load() {
				if c.InboundBuffered() >= limit && !announced {
					announced = true
					full <- c.ReadsPaused()
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
		},
	}}
	client, server := readControlServe(t, ev, PassThroughLayer, PassThroughLayer)
	want := bytes.Repeat([]byte("abcdefgh"), total/8)
	if _, err := client.Write(want); err != nil {
		t.Fatal(err)
	}
	if readControlAwait(t, full, "a full layer delivery") {
		t.Fatal("a full layer delivery paused its consumer")
	}
	if err := server.PauseReads(); err != nil {
		t.Fatal(err)
	}
	consume.Store(true)
	if err := server.ResumeReads(); err != nil {
		t.Fatal(err)
	}
	if got := readControlAwait(t, drained, "layer capacity released by the resumed consumer"); !bytes.Equal(got, want) {
		t.Fatalf("input changed: got %d bytes, want %d", len(got), len(want))
	}
}
