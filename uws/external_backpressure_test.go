package uws

import (
	"bytes"
	"errors"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urpc/uio"
	"github.com/urpc/uio/uws/internal/compress"
	"github.com/urpc/uio/uws/internal/frame"
	"github.com/urpc/uio/uws/internal/handshake"
)

type externalPauseHandler struct {
	open    func(*Conn)
	message func(*Conn, Message)
}

func (h *externalPauseHandler) OnOpen(c *Conn) {
	if h.open != nil {
		h.open(c)
	}
}

func (h *externalPauseHandler) OnMessage(c *Conn, m Message) {
	if h.message != nil {
		h.message(c, m)
	}
}

func (*externalPauseHandler) OnClose(*Conn, CloseEvent) {}

func externalPauseFrame(f frame.Frame, client bool) []byte {
	f.Masked = !client
	return frame.Append(nil, f, [4]byte{9, 8, 7, 6})
}

// Every decoder path must stop at the message whose callback paused it,
// leaving later frames in the same chunk intact for the resumed round.
func TestPauseReadsStopsEveryMessagePathAtTheCurrentFrame(t *testing.T) {
	for _, mode := range []string{"short", "16-bit", "64-bit", "client", "fragmented", "compressed"} {
		t.Run(mode, func(t *testing.T) {
			client := mode == "client"
			payload := []byte("first message")
			if mode == "16-bit" {
				payload = bytes.Repeat([]byte("s"), 512)
			} else if mode == "64-bit" {
				payload = bytes.Repeat([]byte("l"), 70000)
			}
			raw := &limitedWire{}
			cfg := testServerConfig(NewServer(Options{EnableCompression: mode == "compressed"}))
			if client {
				cfg = testDialerConfig(NewDialer(Options{}))
			}
			c := &Conn{raw: raw, config: cfg}
			c.opened.Store(true)
			var got [][]byte
			c.handler = &externalPauseHandler{message: func(c *Conn, m Message) {
				got = append(got, append([]byte(nil), m.Payload...))
				if len(got) == 1 {
					if err := c.PauseReads(); err != nil {
						t.Fatal(err)
					}
				}
			}}
			var first []byte
			switch mode {
			case "fragmented":
				first = externalPauseFrame(frame.Frame{Opcode: frame.Binary, Payload: payload[:5]}, client)
				first = append(first, externalPauseFrame(frame.Frame{Fin: true, Opcode: frame.Continuation, Payload: payload[5:]}, client)...)
			case "compressed":
				encoded, err := compress.Compress(payload, -1)
				if err != nil {
					t.Fatal(err)
				}
				c.compression = &compressionState{decoder: compress.NewDecoder(true)}
				first = externalPauseFrame(frame.Frame{Fin: true, RSV1: true, Opcode: frame.Binary, Payload: encoded}, client)
			default:
				first = externalPauseFrame(frame.Frame{Fin: true, Opcode: frame.Binary, Payload: payload}, client)
			}
			tail := externalPauseFrame(frame.Frame{Fin: true, Opcode: frame.Binary, Payload: []byte("second")}, client)
			tail = append(tail, externalPauseFrame(frame.Frame{Fin: true, Opcode: frame.Binary, Payload: []byte("third")}, client)...)
			raw.inbound = append(first, tail...)
			if err := c.readAvailable(); err != nil {
				t.Fatal(err)
			}
			if len(got) != 1 || !bytes.Equal(got[0], payload) {
				t.Fatalf("before resume: %d messages, first payload matches %v", len(got), len(got) > 0 && bytes.Equal(got[0], payload))
			}
			if !bytes.Equal(raw.inbound, tail) {
				t.Fatalf("paused round changed the unread tail: %d bytes left, want %d", len(raw.inbound), len(tail))
			}
			if c.parser != nil || c.assembler != nil {
				t.Fatal("pause retained a completed parser or assembler")
			}
			if err := c.readAvailable(); err != nil {
				t.Fatal(err)
			}
			if len(got) != 1 {
				t.Fatal("a subsequent read round delivered while manually paused")
			}
			if err := c.ResumeReads(); err != nil {
				t.Fatal(err)
			}
			if err := c.readAvailable(); err != nil {
				t.Fatal(err)
			}
			if len(got) != 3 || string(got[1]) != "second" || string(got[2]) != "third" || len(raw.inbound) != 0 {
				t.Fatalf("after resume: messages=%q, unread=%d", got, len(raw.inbound))
			}
		})
	}
}

func TestPauseReadsAtIncrementalFrameBoundaryPreservesFollowingFrames(t *testing.T) {
	for _, client := range []bool{false, true} {
		name := "server"
		if client {
			name = "client"
		}
		t.Run(name, func(t *testing.T) {
			raw := &limitedWire{}
			cfg := testServerConfig(NewServer(Options{}))
			if client {
				cfg = testDialerConfig(NewDialer(Options{}))
			}
			c := &Conn{raw: raw, config: cfg}
			c.opened.Store(true)
			var got []string
			c.handler = &externalPauseHandler{message: func(c *Conn, m Message) {
				got = append(got, string(m.Payload))
				if len(got) == 1 {
					if err := c.PauseReads(); err != nil {
						t.Fatal(err)
					}
				}
			}}
			payload := strings.Repeat("partial-", 64)
			wire := externalPauseFrame(frame.Frame{Fin: true, Opcode: frame.Binary, Payload: []byte(payload)}, client)
			raw.inbound = append([]byte(nil), wire[:3]...)
			if err := c.readAvailable(); err != nil {
				t.Fatal(err)
			}
			if len(got) != 0 || c.parser == nil {
				t.Fatal("fixture did not leave an incremental parser")
			}
			tail := externalPauseFrame(frame.Frame{Fin: true, Opcode: frame.Binary, Payload: []byte("after partial")}, client)
			raw.inbound = append(append([]byte(nil), wire[3:]...), tail...)
			if err := c.readAvailable(); err != nil {
				t.Fatal(err)
			}
			if len(got) != 1 || got[0] != payload || c.parser != nil || !bytes.Equal(raw.inbound, tail) {
				t.Fatalf("incremental pause: messages %d, parser retained %v, unread %d", len(got), c.parser != nil, len(raw.inbound))
			}
			if err := c.ResumeReads(); err != nil {
				t.Fatal(err)
			}
			if err := c.readAvailable(); err != nil {
				t.Fatal(err)
			}
			if len(got) != 2 || got[1] != "after partial" || len(raw.inbound) != 0 {
				t.Fatalf("incremental resume: messages %q, unread %d", got, len(raw.inbound))
			}
		})
	}
}

func TestManualAndAutomaticReadPausesDoNotReleaseEachOther(t *testing.T) {
	raw, c, _ := newBackpressureConn(64, 16)
	delivered := 0
	c.handler = &externalPauseHandler{message: func(*Conn, Message) { delivered++ }}
	queueReplies(raw, c, 64)
	raw.inbound = maskedFrames("one", "two")
	if err := c.readAvailable(); err != nil {
		t.Fatal(err)
	}
	if !c.internalReadsPaused() {
		t.Fatal("fixture did not enter automatic backpressure")
	}
	if err := c.PauseReads(); err != nil {
		t.Fatal(err)
	}
	if err := c.ResumeReads(); err != nil {
		t.Fatal(err)
	}
	if err := c.readAvailable(); err != nil {
		t.Fatal(err)
	}
	if !c.internalReadsPaused() || delivered != 0 {
		t.Fatal("manual resume released the automatic pause")
	}
	if err := c.PauseReads(); err != nil {
		t.Fatal(err)
	}
	drainTo(raw, c, 0)
	c.resumeReadsAfterDrain()
	if err := c.readAvailable(); err != nil {
		t.Fatal(err)
	}
	if c.internalReadsPaused() || delivered != 0 {
		t.Fatal("automatic drain released the manual pause")
	}
	if err := c.ResumeReads(); err != nil {
		t.Fatal(err)
	}
	if err := c.readAvailable(); err != nil {
		t.Fatal(err)
	}
	if delivered != 2 || len(raw.inbound) != 0 {
		t.Fatalf("resumed delivery = %d, unread %d", delivered, len(raw.inbound))
	}
}

func TestPauseAndResumeReadsAreIdempotentAndResumeWakesBufferedInput(t *testing.T) {
	raw, c, _ := newBackpressureConn(64, 16)
	delivered := 0
	c.handler = &externalPauseHandler{message: func(*Conn, Message) { delivered++ }}
	if err := c.ResumeReads(); err != nil {
		t.Fatal(err)
	}
	if raw.wakes != 0 {
		t.Fatal("resuming an active connection scheduled a wake")
	}
	for range 2 {
		if err := c.PauseReads(); err != nil {
			t.Fatal(err)
		}
	}
	raw.inbound = maskedFrames("buffered")
	if err := c.readAvailable(); err != nil {
		t.Fatal(err)
	}
	if delivered != 0 || len(raw.inbound) == 0 {
		t.Fatal("paused reads consumed input")
	}
	for range 2 {
		if err := c.ResumeReads(); err != nil {
			t.Fatal(err)
		}
	}
	if raw.wakes != 1 {
		t.Fatalf("resume wakes = %d, want one", raw.wakes)
	}
	if err := c.readAvailable(); err != nil {
		t.Fatal(err)
	}
	if delivered != 1 {
		t.Fatalf("resume delivered %d messages, want one", delivered)
	}
}

func TestResumeReadsFromAnotherGoroutineBeforeThePausedCallbackReturns(t *testing.T) {
	raw, c, _ := newBackpressureConn(64, 16)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	var got []string
	c.handler = &externalPauseHandler{message: func(c *Conn, m Message) {
		got = append(got, string(m.Payload))
		if len(got) == 1 {
			if err := c.PauseReads(); err != nil {
				t.Error(err)
			}
			close(entered)
			<-release
		}
	}}
	raw.inbound = maskedFrames("one", "two")
	done := make(chan error, 1)
	go func() { done <- c.readAvailable() }()
	select {
	case <-entered:
	case <-time.After(testIOTimeout()):
		t.Fatal("the first callback never paused")
	}
	if err := c.ResumeReads(); err != nil {
		t.Fatal(err)
	}
	once.Do(func() { close(release) })
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(testIOTimeout()):
		t.Fatal("read round did not finish after concurrent resume")
	}
	if strings.Join(got, ",") != "one,two" || raw.wakes != 1 || len(raw.inbound) != 0 {
		t.Fatalf("concurrent resume: messages %q, wakes %d, unread %d", got, raw.wakes, len(raw.inbound))
	}
}

func TestPauseReadsDoesNotPreventTheCloseHandshake(t *testing.T) {
	raw, c, _ := newBackpressureConn(64, 16)
	t.Cleanup(c.stopCloseTimer)
	if err := c.PauseReads(); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(1000, ""); err != nil {
		t.Fatal(err)
	}
	raw.inbound = externalPauseFrame(frame.Frame{Fin: true, Opcode: frame.Close, Payload: []byte{3, 232}}, false)
	if err := c.readAvailable(); err != nil {
		t.Fatal(err)
	}
	if len(raw.inbound) != 0 {
		t.Fatal("manual pause prevented reading the peer's Close")
	}
	drainTo(raw, c, 0)
	_, _ = c.tryCloseTransport()
	if raw.closeCount() != 1 {
		t.Fatalf("transport close calls = %d, want one", raw.closeCount())
	}
}

func TestReadPauseAPIsRejectUnavailableConnections(t *testing.T) {
	var nilConn *Conn
	for _, c := range []*Conn{nilConn, {}, {raw: &limitedWire{}}} {
		if err := c.PauseReads(); !errors.Is(err, ErrNotReady) {
			t.Fatalf("PauseReads on unavailable connection = %v", err)
		}
		if err := c.ResumeReads(); !errors.Is(err, ErrNotReady) {
			t.Fatalf("ResumeReads on unavailable connection = %v", err)
		}
	}
	_, c, _ := newBackpressureConn(64, 16)
	c.closed.Store(true)
	if err := c.PauseReads(); !errors.Is(err, ErrClosed) {
		t.Fatalf("PauseReads after close = %v", err)
	}
	if err := c.ResumeReads(); !errors.Is(err, ErrClosed) {
		t.Fatalf("ResumeReads after close = %v", err)
	}
	_, closing, _ := newBackpressureConn(64, 16)
	closing.closing.Store(true)
	if err := closing.PauseReads(); !errors.Is(err, ErrClosed) {
		t.Fatalf("PauseReads while closing = %v", err)
	}
	if err := closing.ResumeReads(); !errors.Is(err, ErrClosed) {
		t.Fatalf("ResumeReads while closing = %v", err)
	}
}

// OnOpen can pause frames coalesced behind the HTTP header, and a callback
// for the first of those frames can pause its siblings just like normal input.
func TestReadPausePreservesFramesCoalescedWithTheHandshake(t *testing.T) {
	for _, client := range []bool{false, true} {
		for _, where := range []string{"open", "message"} {
			name := "server/" + where
			if client {
				name = "client/" + where
			}
			t.Run(name, func(t *testing.T) {
				raw := newScriptedConn()
				cfg := testServerConfig(NewServer(Options{}))
				if client {
					cfg = testDialerConfig(NewDialer(Options{}))
				}
				c := &Conn{raw: raw, config: cfg}
				c.handshake.Store(&handshakeState{clientKey: testKey})
				var got []string
				c.handler = &externalPauseHandler{
					open: func(c *Conn) {
						if where == "open" {
							if err := c.PauseReads(); err != nil {
								t.Fatal(err)
							}
						}
					},
					message: func(c *Conn, m Message) {
						got = append(got, string(m.Payload))
						if where == "message" && len(got) == 1 {
							if err := c.PauseReads(); err != nil {
								t.Fatal(err)
							}
						}
					},
				}
				if client {
					raw.inbound = handshake.BuildServerResponse(handshake.Request{Key: testKey}, "", "")
				} else {
					raw.inbound = []byte("GET / HTTP/1.1\r\nHost: example.com\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: " + testKey + "\r\n\r\n")
				}
				for _, payload := range []string{"one", "two", "three"} {
					raw.inbound = append(raw.inbound, externalPauseFrame(frame.Frame{Fin: true, Opcode: frame.Binary, Payload: []byte(payload)}, client)...)
				}
				if err := c.readAvailable(); err != nil {
					t.Fatal(err)
				}
				wantBefore := 0
				if where == "message" {
					wantBefore = 1
				}
				if len(got) != wantBefore || !c.opened.Load() {
					t.Fatalf("handshake pause: opened %v, messages %q", c.opened.Load(), got)
				}
				if err := c.ResumeReads(); err != nil {
					t.Fatal(err)
				}
				if err := c.readAvailable(); err != nil {
					t.Fatal(err)
				}
				if strings.Join(got, ",") != "one,two,three" {
					t.Fatalf("resumed handshake frames = %q", got)
				}
			})
		}
	}
}

func TestOutboundBufferedIncludesTheCurrentReadBatch(t *testing.T) {
	raw, c, _ := newBackpressureConn(64, 16)
	c.round.Store(roundBatching)
	if err := c.SendBinary([]byte("small")); err != nil {
		t.Fatal(err)
	}
	if got := c.OutboundBuffered(); got != 7 || raw.queuedBytes() != 0 {
		t.Fatalf("buffered=%d, transport=%d; want 7 bytes still batched", got, raw.queuedBytes())
	}
	done := make(chan error, 1)
	go func() { done <- c.SendBinary([]byte("external")) }()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := c.OutboundBuffered(); got != 17 {
		t.Fatalf("mixed buffered = %d, want 17", got)
	}
	c.endRound(c.round.Load())
	if got := c.OutboundBuffered(); got != 17 || raw.queuedBytes() != 17 {
		t.Fatalf("after handoff buffered=%d, transport=%d; want 17", got, raw.queuedBytes())
	}
	drainTo(raw, c, 0)
	if got := c.OutboundBuffered(); got != 0 {
		t.Fatalf("after drain buffered=%d, want zero", got)
	}
}

type externalObserveWriteWire struct {
	*limitedWire
	observe func()
}

func (w *externalObserveWriteWire) WriteOwned(buffer *uio.Buffer) (int, error) {
	n, err := w.limitedWire.WriteOwned(buffer)
	if w.observe != nil {
		w.observe()
	}
	return n, err
}

// A transport can report completion before the caller releases writes.mu.
// Both the public queue snapshot and manual resume must be safe there.
func TestOutboundBufferedAndResumeReadsAreSafeInASynchronousWriteReport(t *testing.T) {
	raw, c, _ := newBackpressureConn(64, 16)
	wire := &externalObserveWriteWire{limitedWire: raw}
	c.raw = wire
	c.round.Store(roundBatching)
	if err := c.PauseReads(); err != nil {
		t.Fatal(err)
	}
	if err := c.SendBinary([]byte("report")); err != nil {
		t.Fatal(err)
	}
	var observed int
	var resumeErr error
	wire.observe = func() {
		observed = c.OutboundBuffered()
		resumeErr = c.ResumeReads()
	}
	done := make(chan struct{})
	go func() {
		c.endRound(c.round.Load())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(testIOTimeout()):
		t.Fatal("queue snapshot or resume deadlocked in a synchronous write report")
	}
	if observed < 8 || resumeErr != nil || raw.wakes != 1 {
		t.Fatalf("report observed=%d, resume=%v, wakes=%d", observed, resumeErr, raw.wakes)
	}
	if got := c.OutboundBuffered(); got != 8 {
		t.Fatalf("settled queue snapshot = %d, want 8", got)
	}
}

// A controller must observe the final drain inside WriteOwned, while the
// sender still holds its write lock, or a watermark-based resume can be lost.
func TestOutboundBufferedFinalSynchronousDrainResumesReads(t *testing.T) {
	for _, tracked := range []bool{false, true} {
		name := "plain"
		if tracked {
			name = "tracked"
		}
		t.Run(name, func(t *testing.T) {
			raw, c, _ := newBackpressureConn(64, 16)
			wire := &externalObserveWriteWire{limitedWire: raw}
			if tracked {
				c.setRaw(&externalTrackedObserveWriteWire{wire})
			} else {
				c.setRaw(wire)
			}
			c.round.Store(roundBatching)
			if err := c.PauseReads(); err != nil {
				t.Fatal(err)
			}
			if err := c.SendBinary([]byte("report")); err != nil {
				t.Fatal(err)
			}
			var observed int
			var reported int
			observer := &externalQueueObserver{observe: func(c *Conn, n int) {
				reported += n
				observed = c.OutboundBuffered()
				if observed <= 0 {
					if err := c.ResumeReads(); err != nil {
						t.Error(err)
					}
				}
			}}
			c.handler = observer
			c.config.observeTraffic = true
			wire.observe = func() {
				drainTo(raw, c, 0)
				// The real Server/Dialer observer runs after protocol retirement.
				c.observeOutbound(3)
				c.observeOutbound(5)
			}
			c.endRound(c.round.Load())
			if c.OutboundBuffered() != 0 {
				t.Fatalf("transport did not drain: %d", c.OutboundBuffered())
			}
			if c.externalReadsPaused() {
				t.Fatalf("missed final drain: callback observed %d queued bytes, after handoff %d; reads remain paused", observed, c.OutboundBuffered())
			}
			if reported != 8 {
				t.Fatalf("reported %d bytes, want all 8 sent bytes", reported)
			}
		})
	}
}

type externalTrackedObserveWriteWire struct{ *externalObserveWriteWire }

func (w *externalTrackedObserveWriteWire) WriteTracked(p []byte) (int, uio.SendReceipt, error) {
	n, err := w.Write(p)
	return n, uio.SendReceipt{}, err
}

type externalQueueObserver struct {
	batchEchoHandler
	observe func(*Conn, int)
}

func (*externalQueueObserver) OnInbound(*Conn, int) {}

func (o *externalQueueObserver) OnOutbound(c *Conn, n int) { o.observe(c, n) }

func TestDeferredOutboundReportsKeepTheirTotalAndDoNotOverlap(t *testing.T) {
	_, c, _ := newBackpressureConn(64, 16)
	var active atomic.Int32
	var overlap atomic.Bool
	var total atomic.Int64
	c.handler = &externalQueueObserver{observe: func(c *Conn, n int) {
		if active.Add(1) != 1 {
			overlap.Store(true)
		}
		_ = c.OutboundBuffered()
		runtime.Gosched()
		total.Add(int64(n))
		active.Add(-1)
	}}
	c.config.observeTraffic = true
	var reports sync.WaitGroup
	reports.Add(1)
	go func() {
		defer reports.Done()
		for range 1000 {
			c.observeOutbound(1)
		}
	}()
	for range 1000 {
		c.batchBuffered.Store(^int64(4))
		c.observeOutbound(2)
		c.batchBuffered.Store(0)
		c.flushOutboundObservation()
	}
	reports.Wait()
	c.flushOutboundObservation()
	if total.Load() != 3000 || overlap.Load() {
		t.Fatalf("reported bytes=%d, overlapping callbacks=%v; want 3000 serialized bytes", total.Load(), overlap.Load())
	}
}

type externalHandoffGate struct {
	*limitedWire
	entered chan struct{}
	release chan struct{}
}

func (w *externalHandoffGate) WriteOwned(buffer *uio.Buffer) (int, error) {
	close(w.entered)
	<-w.release
	return w.limitedWire.WriteOwned(buffer)
}

type externalTrackedHandoffGate struct{ *externalHandoffGate }

func (w *externalTrackedHandoffGate) WriteTracked(p []byte) (int, uio.SendReceipt, error) {
	n, err := w.Write(p)
	return n, uio.SendReceipt{}, err
}

func TestOutboundBufferedKeepsAcceptedBatchVisibleDuringHandoff(t *testing.T) {
	for _, tracked := range []bool{false, true} {
		name := "plain"
		if tracked {
			name = "tracked"
		}
		t.Run(name, func(t *testing.T) {
			raw, c, _ := newBackpressureConn(64, 16)
			gate := &externalHandoffGate{limitedWire: raw, entered: make(chan struct{}), release: make(chan struct{})}
			if tracked {
				c.setRaw(&externalTrackedHandoffGate{gate})
			} else {
				c.setRaw(gate)
			}
			c.round.Store(roundBatching)
			if err := c.SendBinary([]byte("report")); err != nil {
				t.Fatal(err)
			}
			done := make(chan struct{})
			go func() {
				c.endRound(c.round.Load())
				close(done)
			}()
			select {
			case <-gate.entered:
			case <-time.After(testIOTimeout()):
				close(gate.release)
				t.Fatal("batch never reached handoff")
			}
			buffered := c.OutboundBuffered()
			close(gate.release)
			select {
			case <-done:
			case <-time.After(testIOTimeout()):
				t.Fatal("batch handoff did not finish")
			}
			if buffered < 8 {
				t.Fatalf("accepted 8-byte batch disappeared during handoff: %d", buffered)
			}
		})
	}
}
