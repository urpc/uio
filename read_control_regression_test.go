package uio

import (
	"errors"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

// Regression tests for the read-control review of 2026-11-04: each one
// failed on the tree before its fix.

// A layered stack keeps reading its socket while its Handler is paused, so
// part of the peer's stream can wait below the Handler — in the socket's
// buffer behind a paced first layer — when the Handler resumes. A resume
// landing in the read round that reads the stream's last bytes must not let
// the end of the stream that round then reads close the connection before
// that input climbed the stack: the end belongs to the stack, which hands
// it up last.
func TestReadControlResumeDuringLastReadKeepsLayeredInput(t *testing.T) {
	const total = 100000
	var got, seen atomic.Int64
	var top atomic.Pointer[Conn]
	closed := make(chan error, 1)
	ev := &Events{Pollers: 1, Handler: FuncHandler{
		Open: func(c Conn) { _ = c.PauseReads(); top.Store(&c) },
		Read: func(c Conn) error {
			n, _ := c.Discard(-1)
			got.Add(int64(n))
			return nil
		},
		Close: func(_ Conn, err error) { closed <- err },
	}, Observer: FuncHandler{BytesRead: func(_ Conn, n int) {
		// Inside the round that read the last chunk, as a goroutine
		// resuming at that moment would.
		if seen.Add(int64(n)) == total {
			_ = (*top.Load()).ResumeReads()
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

// A consumer handling a bounded slice per callback and yielding while more
// remains gets everything the peer sent before the end closes the
// connection, plain or layered.
func TestReadControlBudgetYieldDeliversAllBeforeEOF(t *testing.T) {
	for _, layered := range []bool{false, true} {
		t.Run(map[bool]string{false: "plain", true: "layered"}[layered], func(t *testing.T) {
			const total, slice = 300000, 1000
			var got atomic.Int64
			closed := make(chan error, 1)
			ev := &Events{Pollers: 1, Handler: FuncHandler{
				Read: func(c Conn) error {
					n, _ := c.Discard(min(slice, c.InboundBuffered()))
					got.Add(int64(n))
					if c.InboundBuffered() > 0 {
						return c.YieldRead()
					}
					return nil
				},
				Close: func(_ Conn, err error) { closed <- err },
			}}
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

// A consumer that yields while waiting for more bytes than it has — moving
// nothing — still gets those bytes when they arrive: its continuation reads
// the socket. Yielding without progress spins, as documented, but it no
// longer starves the consumer.
func TestReadControlYieldWithoutProgressStillReads(t *testing.T) {
	for _, layered := range []bool{false, true} {
		t.Run(map[bool]string{false: "plain", true: "layered"}[layered], func(t *testing.T) {
			got := make(chan string, 1)
			ev := &Events{Pollers: 1, Handler: FuncHandler{Read: func(c Conn) error {
				if c.InboundBuffered() < 8 {
					return c.YieldRead()
				}
				p := make([]byte, 8)
				_, _ = io.ReadFull(c, p)
				select {
				case got <- string(p):
				default:
				}
				return nil
			}}}
			var layers []Layer
			if layered {
				layers = []Layer{PassThroughLayer}
			}
			client, _ := readControlServe(t, ev, layers...)
			if _, err := client.Write([]byte("abcd")); err != nil {
				t.Fatal(err)
			}
			time.Sleep(20 * time.Millisecond) // the consumer yields on half a message meanwhile
			if _, err := client.Write([]byte("efgh")); err != nil {
				t.Fatal(err)
			}
			if s := readControlAwait(t, got, "the message completed by a later write"); s != "abcdefgh" {
				t.Fatalf("got %q", s)
			}
		})
	}
}

// noEndLayer forwards bytes up but never calls Up().End().
type noEndLayer struct{}
type noEndHandler struct{}

func (noEndLayer) Attach(LayerContext) LayerHandler { return noEndHandler{} }
func (noEndHandler) OnOpen(ctx LayerContext)        { ctx.Up().Open() }
func (noEndHandler) OnRead(ctx LayerContext) error {
	lower := ctx.Down()
	if chunk := lower.PeekChunk(); len(chunk) > 0 {
		_, _ = ctx.Up().Write(chunk)
		_, _ = lower.Discard(len(chunk))
	}
	return nil
}
func (noEndHandler) OnWrite(ctx LayerContext, p []byte) (int, error) { return ctx.Down().Write(p) }
func (noEndHandler) OnTimer(LayerContext) error                      { return nil }
func (noEndHandler) OnClose(LayerContext, error)                     {}
func (noEndHandler) CloseWrite(ctx LayerContext) error               { return ctx.Down().CloseWrite() }

// A stack whose layers never pass the peer's end up still closes: half-open
// once both halves ended and its layers hold nothing, and without half-open
// once everything below the Handler was handed up.
func TestStackWithoutEndStillCloses(t *testing.T) {
	for _, halfOpen := range []bool{true, false} {
		t.Run(map[bool]string{true: "half-open", false: "closing"}[halfOpen], func(t *testing.T) {
			closed := make(chan error, 1)
			var got atomic.Int64
			ev := &Events{Pollers: 1, AllowHalfOpen: halfOpen, Handler: FuncHandler{
				Read: func(c Conn) error {
					n, _ := c.Discard(-1)
					if got.Add(int64(n)) >= 3 && halfOpen {
						return c.CloseWrite()
					}
					return nil
				},
				Close: func(_ Conn, err error) { closed <- err },
			}}
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

// holdingLayer keeps what is written through it until acknowledged, as a
// reliable transport's send queue does, and reports it as held output. With
// full set it refuses writes, as a full send window does, holding nothing.
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
func (h *holdingLayer) OutboundHeld(LayerContext) int { return h.held }

// Output a layer holds can drain with no socket write to report it: the
// stack tells a QueueDrainedObserver, once per drain, whether the output was
// written between turns or during one.
func TestStackReportsHeldOutputDrains(t *testing.T) {
	transport := new(contractTransport)
	holder := &holdingLayer{}
	var drained []Conn
	driver := &stackDriver{
		layers:  []Layer{LayerFunc(func(LayerContext) LayerHandler { return holder })},
		top:     FuncHandler{},
		drained: func(c Conn) { drained = append(drained, c) },
	}
	driver.OnOpen(transport)
	c := transport.userdata.(*stack)
	top := c.topConn()
	if _, err := top.Write([]byte("queued")); err != nil {
		t.Fatal(err)
	}
	holder.held = 0 // the peer acknowledged it
	if err := c.turn(); err != nil {
		t.Fatal(err)
	}
	if len(drained) != 1 || drained[0] != top {
		t.Fatalf("drains reported = %d, want one for the top connection", len(drained))
	}
	if err := c.turn(); err != nil {
		t.Fatal(err)
	}
	if len(drained) != 1 {
		t.Fatal("a turn without a drain reported one")
	}
	// Written and acknowledged while one turn runs: the turn still sees the
	// peak the write recorded.
	if _, err := top.Write([]byte("more")); err != nil {
		t.Fatal(err)
	}
	holder.held = 0
	if err := c.turn(); err != nil {
		t.Fatal(err)
	}
	if len(drained) != 2 {
		t.Fatalf("drains reported = %d, want two", len(drained))
	}
	// A refusal holding nothing — a full window — is reported on the next
	// turn, which is where an acknowledgement can open the window.
	holder.full = true
	if _, err := top.Write([]byte("refused")); !errors.Is(err, ErrOutboundOverflow) {
		t.Fatalf("write = %v, want overflow", err)
	}
	holder.full = false
	if err := c.turn(); err != nil {
		t.Fatal(err)
	}
	if len(drained) != 3 {
		t.Fatalf("drains reported = %d, want a third for the refusal", len(drained))
	}
	if err := c.turn(); err != nil {
		t.Fatal(err)
	}
	if len(drained) != 3 {
		t.Fatal("a refusal was reported twice")
	}
}

// A plain connection without half-open that left bytes unconsumed when its
// peer ended gets one more OnRead with them, then closes with io.EOF.
func TestUnconsumedInputGetsAFinalCallbackBeforeEOF(t *testing.T) {
	closed := make(chan error, 1)
	var calls, sawFive atomic.Int64
	ev := &Events{Pollers: 1, Handler: FuncHandler{
		Read: func(c Conn) error {
			calls.Add(1)
			if c.InboundBuffered() == 5 {
				sawFive.Add(1)
			}
			return nil // waits for ten bytes that never come
		},
		Close: func(_ Conn, err error) { closed <- err },
	}}
	client, _ := readControlServe(t, ev)
	if _, err := client.Write([]byte("12345")); err != nil {
		t.Fatal(err)
	}
	_ = client.(*net.TCPConn).CloseWrite()
	if err := readControlAwait(t, closed, "the close at the peer's end"); err != io.EOF {
		t.Fatalf("OnClose = %v, want EOF", err)
	}
	if sawFive.Load() == 0 {
		t.Fatal("the retained bytes were never handed to OnRead")
	}
}

// A delivery bound reached (the Handler reads little) does not hold the end
// of the stream: at the socket's end no turn can come to unpace the layer,
// so its undelivered bytes end the stream as a truncation does.
func TestPacedLayerAtSocketEndStillCloses(t *testing.T) {
	closed := make(chan error, 1)
	ev := &Events{Pollers: 1, Handler: FuncHandler{
		Read:  func(Conn) error { return nil }, // declines everything
		Close: func(_ Conn, err error) { closed <- err },
	}}
	client, _ := readControlServe(t, ev, PassThroughLayer)
	if _, err := client.Write(make([]byte, DefaultDeliverBound+4096)); err != nil {
		t.Fatal(err)
	}
	_ = client.(*net.TCPConn).CloseWrite()
	if err := readControlAwait(t, closed, "the close at the socket's end"); err != io.ErrUnexpectedEOF {
		t.Fatalf("OnClose = %v, want unexpected EOF", err)
	}
}
