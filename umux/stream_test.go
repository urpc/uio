package umux

import (
	"bytes"
	"context"
	"errors"
	"github.com/urpc/uio"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func waitStreamError(t *testing.T, st *Stream) error {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	buf := make([]byte, 1)
	for time.Now().Before(deadline) {
		_, err := st.Read(buf)
		if err != nil {
			return err
		}
		time.Sleep(200 * time.Microsecond)
	}
	t.Fatal("stream did not close before the deadline")
	return nil
}

func TestStreamReadDeadlineResetsStream(t *testing.T) {
	sess, echo := startPair(t, Config{KeepAlive: -1}, Config{KeepAlive: -1})
	st, err := sess.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetReadDeadline(time.Now().Add(40 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	readErr := waitStreamError(t, st)
	if !errors.Is(readErr, os.ErrDeadlineExceeded) {
		t.Fatalf("Read error = %v, want deadline exceeded", readErr)
	}
	if err := st.SetReadDeadline(time.Time{}); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("SetReadDeadline after expiry = %v, want net.ErrClosed", err)
	}
	select {
	case closeErr := <-echo.closed:
		if !errors.Is(closeErr, ErrStreamReset) {
			t.Fatalf("remote close = %v, want ErrStreamReset", closeErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("remote stream did not observe the reset")
	}
}

func TestStreamWriteDeadlineResetsStream(t *testing.T) {
	sess, _ := startPair(t, Config{KeepAlive: -1}, Config{KeepAlive: -1})
	st, err := sess.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetWriteDeadline(time.Now().Add(40 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	writeErr := waitStreamError(t, st)
	if !errors.Is(writeErr, os.ErrDeadlineExceeded) {
		t.Fatalf("Read after write deadline = %v, want deadline exceeded", writeErr)
	}
	if !strings.Contains(writeErr.Error(), "write deadline") {
		t.Fatalf("Read error = %v, want write deadline cause", writeErr)
	}
}

func TestStreamDeadlineDropsQueuedDataBeforeReset(t *testing.T) {
	s, st, wire, h := productionStreamPair(t)
	if _, err := st.Write([]byte("queued")); err != nil {
		t.Fatal(err)
	}
	if err := st.SetDeadline(time.Unix(0, 0)); err != nil {
		t.Fatal(err)
	}
	if err := s.onTimer(wire); err != nil {
		t.Fatal(err)
	}
	frames := wire.sent()
	if len(frames) != 0 {
		t.Fatalf("frames = %+v, want no data frame after deadline reset", frames)
	}
	if _, ok := s.streams[st.id]; ok {
		t.Fatal("deadline reset left the stream in the session")
	}
	closes := h.closed()
	if len(closes) != 1 || !errors.Is(closes[0], os.ErrDeadlineExceeded) {
		t.Fatalf("OnClose = %v, want one deadline error", closes)
	}
}

func TestInboundAppendReusesPooledBuffer(t *testing.T) {
	payload := []byte(strings.Repeat("x", 32))
	var b inbound
	allocs := testing.AllocsPerRun(100, func() {
		b.append(payload)
		b.consume(b.len())
	})
	if allocs != 0 {
		t.Fatalf("append/consume cycle allocated %v times, want 0", allocs)
	}
}

func TestInboundBorrowedRunIsCopiedBeforeAppend(t *testing.T) {
	src := []byte("borrowed")
	var b inbound
	b.setBorrowed(src)
	b.append([]byte("-tail"))
	for i := range src {
		src[i] = 'X'
	}
	got := b.peek(make([]byte, b.len()))
	if string(got) != "borrowed-tail" {
		t.Fatalf("queue = %q, want %q", got, "borrowed-tail")
	}
	b.drop()
}

func TestInboundMultiBlockPeekReadAndConsume(t *testing.T) {
	var b inbound
	b.append(make([]byte, 64))
	b.consume(48)
	b.append(make([]byte, 40))
	if b.len() != 56 {
		t.Fatalf("len = %d, want 56", b.len())
	}
	b.setBorrowed([]byte("tail"))
	if b.len() != 60 {
		t.Fatalf("len with borrowed tail = %d, want 60", b.len())
	}
	peeked := b.peek(make([]byte, 60))
	if string(peeked[56:]) != "tail" {
		t.Fatalf("peek tail = %q, want tail", peeked[56:])
	}
	out := make([]byte, 58)
	if n := b.read(out); n != 58 || b.len() != 2 {
		t.Fatalf("read = %d, remaining = %d, want 58 and 2", n, b.len())
	}
	b.consume(10)
	if b.len() != 0 {
		t.Fatalf("remaining after consume = %d, want 0", b.len())
	}
	b.drop()
}

func TestInboundDropDeferredDuringActiveBorrow(t *testing.T) {
	var b inbound
	b.append([]byte("payload"))
	b.borrows = 1
	b.drop()
	if b.len() != 0 || b.peekChunk() != nil || b.read(make([]byte, 4)) != 0 {
		t.Fatal("dropped inbound still exposed data while borrowed")
	}
	b.consume(4)
	b.borrows = 0
	b.buf.Reset()
	b.dropped = false
	b.append([]byte("next"))
	if got := string(b.peekChunk()); got != "next" {
		t.Fatalf("queue after drop = %q, want %q", got, "next")
	}
	b.drop()
}

func TestStreamReserveInboundAndNilAbort(t *testing.T) {
	s, st, _, _ := productionStreamPair(t)
	_ = s
	if err := st.ReserveInbound(0); err != nil {
		t.Fatal(err)
	}
	if err := st.ReserveInbound(16); err != nil {
		t.Fatal(err)
	}
	st.in.setBorrowed([]byte("already-long-enough"))
	if err := st.ReserveInbound(8); err != nil {
		t.Fatal(err)
	}
	st.in.append([]byte("-more"))
	if err := st.ReserveInbound(24); err != nil {
		t.Fatal(err)
	}
	if len(st.PeekChunk()) < 24 {
		t.Fatalf("PeekChunk len = %d, want >= 24", len(st.PeekChunk()))
	}
	st.in.borrows = 1
	if err := st.ReserveInbound(64); err != nil {
		t.Fatal(err)
	}
	st.in.borrows = 0
	if err := st.Abort(nil); err != nil {
		t.Fatal(err)
	}
}

type metaHandler struct {
	mu     sync.Mutex
	calls  int
	meta   []byte
	opened chan *Stream
}

func (h *metaHandler) OnMetadata(_ uio.Conn, meta []byte) {
	h.mu.Lock()
	h.calls++
	h.meta = meta
	h.mu.Unlock()
}

func (h *metaHandler) OnOpen(c uio.Conn) {
	h.opened <- c.(*Stream)
}

func (h *metaHandler) OnRead(c uio.Conn) error {
	st := c.(*Stream)
	buf := make([]byte, 4<<10)
	n, err := st.Read(buf)
	if n > 0 {
		_, _ = st.Write(buf[:n])
	}
	return err
}

func (h *metaHandler) OnTimer(uio.Conn) error {
	return nil
}

func (h *metaHandler) OnClose(uio.Conn, error) {
}

func (h *metaHandler) lastMeta() ([]byte, int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.meta, h.calls
}

func TestMetadataPullMode(t *testing.T) {
	s, _ := recPair(Config{})
	s.acceptCh = make(chan Accepted, 1)
	payload := append([]byte{0, 0, 0x10, 0}, []byte("route=a")...)
	if err := s.handleSynMetadata(frameHeader{typ: typeSynMetadata, flags: flagSYN, streamID: 3, length: uint32(len(payload))}, payload); err != nil {
		t.Fatal(err)
	}
	var in Accepted
	select {
	case in = <-s.acceptCh:
	default:
		t.Fatal("stream never reached the accept queue")
	}
	if in.Stream == nil || string(in.Metadata) != "route=a" {
		t.Fatalf("accepted %+v, want the stream with metadata %q", in, "route=a")
	}
}

func TestMetadataCopiedOutOfTransport(t *testing.T) {
	h := &metaHandler{opened: make(chan *Stream, 1)}
	s, _ := recPair(Config{Handler: h})
	payload := append([]byte{0, 0, 0x10, 0}, []byte("route=a")...)
	if err := s.handleSynMetadata(frameHeader{typ: typeSynMetadata, flags: flagSYN, streamID: 3, length: uint32(len(payload))}, payload); err != nil {
		t.Fatal(err)
	}
	<-h.opened
	copy(payload[4:], "xxxxxxx")
	if got, _ := h.lastMeta(); string(got) != "route=a" {
		t.Fatalf("metadata = %q after the transport reused its buffer, want %q", got, "route=a")
	}
}

func TestMetadataTooLargeLocally(t *testing.T) {
	s, wire := recPair(Config{MaxMetadata: 64})
	before := wire.deadlineCount()
	if _, err := s.OpenWith(context.Background(), make([]byte, 65)); !errors.Is(err, ErrMetadataTooLarge) {
		t.Fatalf("open = %v, want ErrMetadataTooLarge", err)
	}
	if wire.deadlineCount() != before || s.Closed() {
		t.Fatal("a refused OpenWith touched the session")
	}
}

func TestMetadataRejectedByPeer(t *testing.T) {
	sess, _ := startPair(t, Config{Handler: &metaHandler{opened: make(chan *Stream, 4)}, MaxMetadata: 64}, Config{MaxMetadata: 1024})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := sess.OpenWith(ctx, make([]byte, 100)); err == nil {
		t.Fatal("OpenWith succeeded past the receiver's MaxMetadata")
	}
}

func TestTransportLimitFitsMetadataFrame(t *testing.T) {
	cfg := Config{ReadBufferSize: 4 << 10, SessionWindow: 64 << 10, WindowSize: 256 << 10, MaxMetadata: 1 << 20}.resolve()
	if got, want := transportReadLimit(cfg), (1<<20)+headerSize+4; got < want {
		t.Fatalf("transport read limit = %d, want at least one %d-byte metadata frame", got, want)
	}
}

type quietWire struct {
	uio.Conn
	mu     sync.Mutex
	frames []frameHeader
}

func (*quietWire) Wake() error {
	return nil
}

func (*quietWire) SetWakeDeadline(time.Time) error {
	return nil
}

func (*quietWire) Write(p []byte) (int, error) {
	return len(p), nil
}

func (c *quietWire) WriteOwned(b *uio.Buffer) (int, error) {
	h, err := decodeHeader(b.Bytes()[:headerSize])
	if err != nil {
		panic(err)
	}
	c.mu.Lock()
	c.frames = append(c.frames, h)
	c.mu.Unlock()
	n := b.Len()
	uio.ReleaseBuffer(b)
	return n, nil
}

func (c *quietWire) seen() []frameHeader {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]frameHeader(nil), c.frames...)
}

func quietPair() (*Session, *Stream, *quietWire) {
	wire := &quietWire{}
	s := &Session{cfg: Config{KeepAlive: -1}.resolve(), conn: wire, streams: make(map[uint32]*Stream), openWait: make(map[uint32]chan error)}
	st := newStream(s, 1, false, int64(s.cfg.WindowSize))
	s.streams[1] = st
	return s, st, wire
}

func TestStreamDiscardSettlesFullyEnded(t *testing.T) {
	s, st, _ := quietPair()
	if err := st.pushData([]byte("x"), true, false); err != nil {
		t.Fatal(err)
	}
	if err := st.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	s.flush()
	if _, err := st.Discard(-1); err != nil {
		t.Fatal(err)
	}
	if s.streams[st.id] != nil {
		t.Fatalf("discard left fully ended stream in map; state=%v", st.State())
	}
}

type productionStreamWire struct {
	uio.Conn
	mu       sync.Mutex
	frames   []frameHeader
	writeErr error
}

func (*productionStreamWire) Wake() error {
	return nil
}

func (*productionStreamWire) SetWakeDeadline(time.Time) error {
	return nil
}

func (*productionStreamWire) Close() error {
	return nil
}

func (*productionStreamWire) Abort(error) error {
	return nil
}

func (w *productionStreamWire) Write(p []byte) (int, error) {
	return len(p), nil
}

func (w *productionStreamWire) WriteOwned(b *uio.Buffer) (int, error) {
	w.mu.Lock()
	err := w.writeErr
	if err == nil {
		h, decErr := decodeHeader(b.Bytes())
		if decErr != nil {
			w.mu.Unlock()
			panic(decErr)
		}
		w.frames = append(w.frames, h)
	}
	w.mu.Unlock()
	if errors.Is(err, uio.ErrOutboundOverflow) {
		return 0, err
	}
	n := b.Len()
	uio.ReleaseBuffer(b)
	if err != nil {
		return 0, err
	}
	return n, nil
}

func (w *productionStreamWire) failWith(err error) {
	w.mu.Lock()
	w.writeErr = err
	w.mu.Unlock()
}

func (w *productionStreamWire) sent() []frameHeader {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]frameHeader(nil), w.frames...)
}

type productionStreamHandler struct {
	mu     sync.Mutex
	closes []error
}

func (*productionStreamHandler) OnOpen(uio.Conn) {
}

func (*productionStreamHandler) OnRead(uio.Conn) error {
	return nil
}

func (*productionStreamHandler) OnTimer(uio.Conn) error {
	return nil
}

func (h *productionStreamHandler) OnClose(_ uio.Conn, cause error) {
	h.mu.Lock()
	h.closes = append(h.closes, cause)
	h.mu.Unlock()
}

func (h *productionStreamHandler) closed() []error {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]error(nil), h.closes...)
}

func productionStreamPair(t *testing.T) (*Session, *Stream, *productionStreamWire, *productionStreamHandler) {
	t.Helper()
	wire := &productionStreamWire{}
	h := &productionStreamHandler{}
	s := &Session{cfg: Config{Handler: h, KeepAlive: -1}.resolve(), conn: wire, streams: make(map[uint32]*Stream), openWait: make(map[uint32]chan error)}
	st := newStream(s, 1, false, int64(s.cfg.WindowSize))
	s.streams[st.id] = st
	t.Cleanup(func() {
		s.mu.Lock()
		st.dropOutLocked()
		s.mu.Unlock()
	})
	return s, st, wire, h
}

func TestProductionStreamFINRequiresTransportAcceptance(t *testing.T) {
	s, st, wire, h := productionStreamPair(t)
	if err := st.pushData(nil, true, false); err != nil {
		t.Fatal(err)
	}
	st.mu.Lock()
	st.localFIN = true
	st.mu.Unlock()
	s.tryFinish(st)
	s.drainCloses()
	if st.State()&uio.StateClosed != 0 || len(h.closed()) != 0 {
		t.Fatal("stream settled before its FIN was queued")
	}
	s.enqueueFin(st)
	wire.failWith(uio.ErrOutboundOverflow)
	s.flush()
	s.drainCloses()
	if st.State()&uio.StateClosed != 0 || len(h.closed()) != 0 || len(wire.sent()) != 0 {
		t.Fatal("stream settled after the transport refused its FIN")
	}
	wire.failWith(nil)
	s.flush()
	s.drainCloses()
	s.enqueueFin(st)
	s.flush()
	s.tryFinish(st)
	s.drainCloses()
	frames, closes := wire.sent(), h.closed()
	if len(frames) != 1 || frames[0].flags != flagFIN {
		t.Fatalf("frames = %+v, want exactly one FIN", frames)
	}
	if len(closes) != 1 || !errors.Is(closes[0], io.EOF) {
		t.Fatalf("OnClose = %v, want one EOF", closes)
	}
}

func TestProductionStreamFINTransportErrorUsesTeardownCause(t *testing.T) {
	s, st, wire, h := productionStreamPair(t)
	if err := st.pushData(nil, true, false); err != nil {
		t.Fatal(err)
	}
	if err := st.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	boom := errors.New("transport FIN failed")
	wire.failWith(boom)
	s.flush()
	s.tryFinish(st)
	s.drainCloses()
	if st.State()&uio.StateClosed != 0 || len(h.closed()) != 0 {
		t.Fatal("failed FIN was treated as a successful close")
	}
	s.onTransportClose(boom)
	closes := h.closed()
	if len(closes) != 1 || !errors.Is(closes[0], boom) {
		t.Fatalf("OnClose = %v, want the transport error", closes)
	}
}

func TestProductionStreamGracefulCloseStates(t *testing.T) {
	for _, peerFirst := range []bool{false, true} {
		for _, inbound := range []string{"", "inbound"} {
			for _, outbound := range []string{"", "outbound"} {
				name := "local-first"
				if peerFirst {
					name = "peer-first"
				}
				if inbound != "" {
					name += "/inbound"
				}
				if outbound != "" {
					name += "/outbound"
				}
				t.Run(name, func(t *testing.T) {
					s, st, wire, h := productionStreamPair(t)
					peerEnd := func() {
						if err := st.pushData([]byte(inbound), true, false); err != nil {
							t.Fatal(err)
						}
						s.tryFinish(st)
					}
					if peerFirst {
						peerEnd()
					}
					if _, err := st.Write([]byte(outbound)); err != nil {
						t.Fatal(err)
					}
					if err := st.Close(); err != nil {
						t.Fatal(err)
					}
					s.flush()
					if !peerFirst {
						peerEnd()
					}
					if inbound != "" {
						if st.State()&uio.StateClosed != 0 {
							t.Fatal("unread inbound was dropped")
						}
						if _, err := st.Discard(-1); err != nil {
							t.Fatal(err)
						}
					}
					s.drainCloses()
					want := io.EOF
					closes := h.closed()
					if len(closes) != 1 || !errors.Is(closes[0], want) {
						t.Fatalf("OnClose = %v, want %v", closes, want)
					}
					if st.State()&uio.StateClosed == 0 {
						t.Fatal("fully ended stream remained open")
					}
					frames := wire.sent()
					wantFrames := 1
					if outbound != "" {
						wantFrames++
					}
					if len(frames) != wantFrames || frames[len(frames)-1].flags != flagFIN {
						t.Fatalf("data/FIN order = %+v", frames)
					}
				})
			}
		}
	}
}

func TestProductionStreamWakeCannotReviveClosedSession(t *testing.T) {
	s, st, _, _ := productionStreamPair(t)
	s.mu.Lock()
	s.closed = true
	delete(s.streams, st.id)
	s.wakeQ = nil
	s.mu.Unlock()
	if err := st.Wake(); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	queued := len(s.wakeQ)
	s.mu.Unlock()
	st.mu.Lock()
	pending := st.wakePending
	st.mu.Unlock()
	if queued != 0 || pending {
		t.Fatalf("closed session regained wake work: queued=%d pending=%v", queued, pending)
	}
}

func TestProductionStreamWriteOwnedOwnership(t *testing.T) {
	boom := errors.New("reset cause")
	cases := []struct {
		name    string
		prepare func(*Session, *Stream)
		wantN   int
		wantErr error
	}{{name: "accepted", wantN: 4}, {name: "overflow", prepare: func(_ *Session, st *Stream) {
		st.sendWin.reset(3)
	}, wantErr: uio.ErrOutboundOverflow}, {name: "opening", prepare: func(_ *Session, st *Stream) {
		st.opening = true
	}, wantErr: ErrClosed}, {name: "half-closed", prepare: func(_ *Session, st *Stream) {
		_ = st.CloseWrite()
	}, wantErr: ErrClosed}, {name: "reset", prepare: func(s *Session, st *Stream) {
		s.abortStream(st, boom)
	}, wantErr: boom}, {name: "removed", prepare: func(s *Session, st *Stream) {
		s.removeStream(st)
	}, wantN: 4, wantErr: ErrClosed}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, st, _, _ := productionStreamPair(t)
			if tc.prepare != nil {
				tc.prepare(s, st)
			}
			b := uio.AcquireBuffer(4)
			_, _ = b.Write([]byte("data"))
			n, err := st.WriteOwned(b)
			if n != tc.wantN || !errors.Is(err, tc.wantErr) {
				t.Fatalf("WriteOwned = %d,%v; want %d,%v", n, err, tc.wantN, tc.wantErr)
			}
			if errors.Is(err, uio.ErrOutboundOverflow) {
				if !bytes.Equal(b.Bytes(), []byte("data")) {
					t.Fatal("refused buffer was consumed")
				}
				uio.ReleaseBuffer(b)
			} else if b.Len() != 0 {
				uio.ReleaseBuffer(b)
				t.Fatal("non-overflow result left buffer ownership with caller")
			}
		})
	}
}
