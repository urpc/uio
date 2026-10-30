package umux

import (
	"bytes"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/urpc/uio"
)

type productionStreamWire struct {
	uio.Conn
	mu       sync.Mutex
	frames   []frameHeader
	writeErr error
}

func (*productionStreamWire) Wake() error                     { return nil }
func (*productionStreamWire) SetWakeDeadline(time.Time) error { return nil }
func (*productionStreamWire) CloseWith(error) error           { return nil }
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

func (*productionStreamHandler) OnOpen(uio.Conn)        {}
func (*productionStreamHandler) OnRead(uio.Conn) error  { return nil }
func (*productionStreamHandler) OnTimer(uio.Conn) error { return nil }
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
	s := &Session{
		cfg:      Config{Handler: h, KeepAlive: -1}.resolve(),
		conn:     wire,
		streams:  make(map[uint32]*Stream),
		openWait: make(map[uint32]chan error),
	}
	st := newStream(s, 1, false, int64(s.cfg.WindowSize))
	s.streams[st.id] = st
	t.Cleanup(func() {
		s.mu.Lock()
		st.dropOutLocked()
		s.mu.Unlock()
	})
	return s, st, wire, h
}

// The gap between publishing the close intent and queueing FIN is a valid
// concurrent state. Neither it nor a transport refusal may settle the stream.
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
	// Subsequent progress or close requests do not duplicate FIN or OnClose.
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
	cause := errors.New("application close")
	for _, peerFirst := range []bool{false, true} {
		for _, inbound := range []string{"", "inbound"} {
			for _, outbound := range []string{"", "outbound"} {
				for _, closeCause := range []error{nil, cause} {
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
					if closeCause != nil {
						name += "/cause"
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
						if err := st.CloseGracefully(closeCause); err != nil {
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
						want := closeCause
						if want == nil {
							want = io.EOF
						}
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
}

func TestProductionStreamAbortWinsAfterRemoval(t *testing.T) {
	s, st, _, h := productionStreamPair(t)
	// An abort removes the member before setting its closed flag. A concurrent
	// settlement must leave its cause and callback to the abort that owns it.
	st.mu.Lock()
	st.localFIN, st.peerFIN = true, true
	st.mu.Unlock()
	s.mu.Lock()
	st.finState = finSent
	s.mu.Unlock()
	s.removeStream(st)
	s.tryFinish(st)
	cause := errors.New("abort wins")
	st.resetLocal(cause)
	s.drainCloses()
	closes := h.closed()
	if len(closes) != 1 || !errors.Is(closes[0], cause) {
		t.Fatalf("OnClose = %v, want the abort cause", closes)
	}
}

func TestProductionStreamWakeCannotReviveClosedSession(t *testing.T) {
	s, st, _, _ := productionStreamPair(t)
	// Teardown has removed the stream and its queued work, but resetRemote
	// has not yet published this stream's own closed flag.
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
	}{
		{name: "accepted", wantN: 4},
		{name: "overflow", prepare: func(_ *Session, st *Stream) { st.sendWin.reset(3) }, wantErr: uio.ErrOutboundOverflow},
		{name: "opening", prepare: func(_ *Session, st *Stream) { st.opening = true }, wantErr: ErrClosed},
		{name: "half-closed", prepare: func(_ *Session, st *Stream) { _ = st.CloseWrite() }, wantErr: ErrClosed},
		{name: "reset", prepare: func(s *Session, st *Stream) { s.abortStream(st, boom) }, wantErr: boom},
		{name: "removed", prepare: func(s *Session, st *Stream) { s.removeStream(st) }, wantN: 4, wantErr: ErrClosed},
	}
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
