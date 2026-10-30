/*
 * Copyright 2024 the urpc project
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *      https://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package umux

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/urpc/uio"
)

// maxFlushFrames bounds one flush pass, so a busy sender cannot hold the
// session lock for long.
const maxFlushFrames = 256

// closeItem is one queued OnClose delivery.
type closeItem struct {
	st  *Stream
	err error
}

// Session multiplexes streams over one transport connection. It is the
// transport's uio.Handler: frames arrive in the transport turn and stream
// callbacks run there, synchronously.
type Session struct {
	cfg    Config
	client bool

	conn uio.Conn
	done chan struct{}

	mu sync.Mutex
	// timerMu serializes computing and submitting the transport's wake
	// deadline, so concurrent arms cannot let a stale value land last.
	timerMu sync.Mutex
	// armedAt is the deadline last submitted to the transport; armNotLaterThan
	// skips resubmitting when it is already soon enough.
	armedAt time.Time
	streams map[uint32]*Stream
	nextID  uint32
	// openWait carries the ACK of a locally opened stream; the Open caller
	// waits on it.
	openWait map[uint32]chan error
	// acceptCh receives accepted streams when Config.Handler is nil.
	acceptCh chan Accepted
	// control holds control frames waiting for the transport.
	control [][]byte
	// pending lists stream ids with queued frames, in round-robin order.
	pending []uint32
	// pendingBytes is the payload buffered across this session's streams; the
	// session window bound.
	pendingBytes int64
	// readHeld reports the session buffered more inbound than its budget
	// allows and stopped parsing transport frames; consumption resumes it.
	readHeld bool

	goAway     bool
	goAwaySent bool
	// goAwayFlushed reports the transport accepted the GoAway frame, not
	// merely that it was queued.
	goAwayFlushed bool
	closed        bool
	// closeErr is why the transport went away, for streams closed by it.
	closeErr error
	// draining reports a Shutdown in progress: new streams are refused while
	// the existing ones get to finish.
	draining bool
	// drainDone closes once the drain ends, cleanly or not; drainErr then
	// says how, and nil means every stream settled first.
	drainDone   chan struct{}
	drainErr    error
	drainClosed bool

	lastPing time.Time
	pingSent time.Time
	pingSeq  uint32

	// flushReq coalesces write-progress notifications: the transport observer
	// may run inside a write this session just made, so it only asks for a
	// later flush through a transport Wake.
	flushReq atomic.Bool
	// finFlushed collects streams whose FIN left the wire in a flush; they
	// settle outside the session lock.
	finFlushed []*Stream
	// drainObs, when the observer asks for it, receives one event per flush
	// that emptied a stream's outbound queue — the stream's version of a
	// socket leg reporting its write progress. It is resolved once: an
	// observer not implementing the optional method pays nothing.
	drainObs QueueDrainedObserver
	// drained collects streams whose outbound queue a flush emptied; they are
	// reported outside the session lock.
	drained []*Stream
	// closeQ carries stream closes waiting for the transport turn; the
	// handler's callbacks for one stream never run concurrently, so a
	// CloseWith from another goroutine is delivered there.
	closeQ []closeItem
	// wakeQ holds streams that asked for an empty read turn, so a turn does
	// not scan the whole table.
	wakeQ []*Stream
}

func newSession(conn uio.Conn, cfg Config, client bool) *Session {
	s := &Session{
		cfg:      cfg,
		client:   client,
		conn:     conn,
		done:     make(chan struct{}),
		streams:  make(map[uint32]*Stream),
		openWait: make(map[uint32]chan error),
	}
	if cfg.Handler == nil {
		s.acceptCh = make(chan Accepted, cfg.AcceptBacklog)
	}
	if client {
		s.nextID = 1 // client streams are odd
	} else {
		s.nextID = 2 // server streams are even
	}
	s.drainObs, _ = cfg.Observer.(QueueDrainedObserver)
	if cfg.KeepAlive > 0 {
		s.lastPing = time.Now()
	}
	s.rearmTimer()
	return s
}

// Accepted is one inbound stream handed over by Session.Accept, together with
// the metadata its opener attached to the SYN (nil for a plain open). Nothing
// in the session refers to the value once it is queued: a caller that wants
// the metadata beyond dequeuing it keeps it on its own account.
type Accepted struct {
	Stream   *Stream
	Metadata []byte
}

// Accept returns the queue of accepted streams; it is nil when Config.Handler
// is set. A full queue refuses the peer's stream with RST. The transport's
// close closes this queue; buffered streams can still be received, but their
// transport has ended. Done and Err report that session lifecycle directly.
func (s *Session) Accept() <-chan Accepted { return s.acceptCh }

// Done closes when the transport ends. Err then reports the transport's cause.
func (s *Session) Done() <-chan struct{} { return s.done }

// Err reports why the transport ended, or nil while the session is open.
func (s *Session) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closeErr
}

// Closed reports whether the session has closed.
func (s *Session) Closed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

// Close sends a GoAway, then closes the transport; every stream closes with
// cause. It is idempotent.
func (s *Session) Close(cause error) error {
	if cause == nil {
		cause = ErrClosed
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return net.ErrClosed
	}
	s.goAway = true
	if !s.goAwaySent {
		s.goAwaySent = true
		s.sendControlLocked(typeGoAway, 0, 0, goAwayNormal)
	}
	err := s.flushLocked()
	s.finishDrainLocked(cause)
	s.mu.Unlock()
	s.emitDrained()
	if err != nil {
		_ = s.conn.CloseWith(err)
		return nil
	}
	return s.conn.CloseWith(cause)
}

// Open starts a stream and waits for the peer's ACK. The ACK names the peer's
// initial window.
func (s *Session) Open(ctx context.Context) (*Stream, error) {
	return s.open(ctx, nil)
}

// OpenWith starts a stream like Open and attaches metadata to its SYN: opaque
// bytes the acceptor receives once, through MetadataHandler.OnMetadata or the
// Accepted value of Session.Accept. umux neither interprets the bytes nor
// bounds their meaning beyond Config.MaxMetadata; it is the place for a
// caller's own routing or identification data. The extension frame is outside
// yamux v1, so both ends must be umux when metadata is used.
func (s *Session) OpenWith(ctx context.Context, metadata []byte) (*Stream, error) {
	if len(metadata) > s.cfg.MaxMetadata {
		return nil, ErrMetadataTooLarge
	}
	if len(metadata) == 0 {
		return s.open(ctx, nil)
	}
	return s.open(ctx, metadata)
}

func (s *Session) open(ctx context.Context, metadata []byte) (*Stream, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, ErrClosed
	}
	if s.goAway {
		s.mu.Unlock()
		return nil, ErrGoAway
	}
	if len(s.streams) >= s.cfg.MaxStreams {
		s.mu.Unlock()
		return nil, ErrMaxStreams
	}
	id := s.nextID
	s.nextID += 2
	st := newStream(s, id, true, int64(s.cfg.WindowSize))
	s.streams[id] = st
	wait := make(chan error, 1)
	s.openWait[id] = wait
	// SYN opens the stream and announces this end's receive window as a delta
	// above the yamux v1 floor. Metadata rides in an extension frame whose
	// payload is the same delta followed by the opener's opaque bytes.
	if len(metadata) == 0 {
		s.sendControlLocked(typeWindowUpdate, flagSYN, id, uint32(s.cfg.WindowSize-minWindow))
	} else {
		frame := make([]byte, headerSize+4+len(metadata))
		encodeHeader(frame, frameHeader{
			typ:      typeSynMetadata,
			flags:    flagSYN,
			streamID: id,
			length:   uint32(4 + len(metadata)),
		})
		binary.BigEndian.PutUint32(frame[headerSize:], uint32(s.cfg.WindowSize-minWindow))
		copy(frame[headerSize+4:], metadata)
		s.control = append(s.control, frame)
	}
	err := s.flushLocked()
	s.mu.Unlock()
	st.mu.Lock()
	openDeadline := st.openedAt.Add(s.cfg.StreamOpenTimeout)
	st.mu.Unlock()
	s.armNotLaterThan(openDeadline)
	s.emitDrained()
	if err != nil {
		s.removeStream(st)
		st.resetLocal(err)
		_ = s.conn.CloseWith(err)
		return nil, err
	}
	select {
	case err := <-wait:
		if err != nil {
			s.removeStream(st)
			st.resetLocal(err)
			return nil, err
		}
		return st, nil
	case <-ctx.Done():
		s.abortStream(st, ctx.Err())
		return nil, ctx.Err()
	}
}

// ------------------------------------------------------------------ inbound

// onRead parses every complete frame buffered on the transport and dispatches
// it. It runs on the transport's turn.
func (s *Session) onRead(c uio.Conn) error {
	s.flushReq.Store(false)
	if s.heldReads() {
		// The session is over its receive budget: the transport's bytes stay
		// buffered, and a consumption below the low-water mark wakes this
		// turn to parse them. Closes queued meanwhile still run: a held
		// session with no other timer would otherwise never deliver them.
		s.flush()
		s.runWakes()
		s.drainCloses()
		return nil
	}
	for {
		fr, ok, err := readFrame(c, s.cfg.WindowSize, s.cfg.MaxMetadata)
		if err != nil {
			_ = c.CloseWith(err)
			return err
		}
		if !ok {
			break
		}
		herr := s.handleFrame(fr.hdr, fr.payload, fr.borrowed)
		if !fr.consumed {
			if _, err := c.Discard(fr.total); err != nil {
				_ = c.CloseWith(err)
				return err
			}
		}
		if herr != nil {
			_ = c.CloseWith(herr)
			return herr
		}
		if s.exhaustedBudget() {
			break
		}
	}
	s.flush()
	s.runWakes()
	s.drainCloses()
	return nil
}

// runWakes runs the empty read turns streams scheduled with Wake.
func (s *Session) runWakes() {
	handler := s.cfg.Handler
	if handler == nil {
		return
	}
	s.mu.Lock()
	wakes := s.wakeQ
	s.wakeQ = nil
	live := wakes[:0]
	for _, st := range wakes {
		st.mu.Lock()
		st.wakePending = false
		closed := st.closedLocal
		st.mu.Unlock()
		if closed {
			// Closed while the wake was queued: the close callback was its
			// last turn.
			continue
		}
		live = append(live, st)
	}
	s.mu.Unlock()
	for _, st := range live {
		if err := handler.OnRead(st); err != nil {
			s.abortStream(st, err)
			continue
		}
		// A wake turn drains like an arrival turn: a stream whose FIN arrived
		// while it was backed up settles here, once the callback consumed the
		// rest.
		s.tryFinish(st)
	}
}

// readFrame pulls one frame from the transport's inbound buffer. ok=false
// means the buffered bytes hold no complete frame yet.
// frame is one decoded frame. A data payload may borrow the transport's
// inbound buffer: it stays valid until the session consumed the frame, after
// which any leftover held by a stream was copied aside.
type frame struct {
	hdr      frameHeader
	payload  []byte
	total    int
	borrowed bool
	// consumed marks a frame whose bytes were read off the inbound already.
	consumed bool
}

func readFrame(c uio.Conn, maxFrame, maxMeta int) (frame, bool, error) {
	var hb [headerSize]byte
	got := c.Peek(hb[:])
	if len(got) < headerSize {
		return frame{}, false, nil
	}
	h, err := decodeHeader(got)
	if err != nil {
		return frame{}, false, err
	}
	if err := validateFrame(h, uint32(maxFrame), uint32(maxMeta)); err != nil {
		return frame{}, false, err
	}
	total := headerSize
	if h.typ == typeData || h.typ == typeSynMetadata {
		total += int(h.length)
	}
	if c.InboundBuffered() < total {
		return frame{}, false, nil
	}
	if (h.typ != typeData && h.typ != typeSynMetadata) || h.length == 0 {
		return frame{hdr: h, total: total}, true, nil
	}
	// A contiguous chunk serves the payload in place; a frame split across
	// chunks falls back to a copy.
	if chunk := c.PeekChunk(); len(chunk) >= total {
		return frame{hdr: h, payload: chunk[headerSize:total], total: total, borrowed: true}, true, nil
	}
	payload := make([]byte, total)
	if _, err := c.Read(payload); err != nil {
		return frame{}, false, err
	}
	return frame{hdr: h, payload: payload[headerSize:], total: total, consumed: true}, true, nil
}

func (s *Session) handleFrame(h frameHeader, payload []byte, borrowed bool) error {
	switch h.typ {
	case typeData:
		return s.handleData(h, payload, borrowed)
	case typeWindowUpdate:
		return s.handleWindowUpdate(h)
	case typePing:
		return s.handlePing(h)
	case typeSynMetadata:
		return s.handleSynMetadata(h, payload)
	case typeGoAway:
		return s.handleGoAway(h)
	default:
		s.mu.Lock()
		s.goAway = true
		s.mu.Unlock()
		return nil
	}
}

func (s *Session) handleData(h frameHeader, payload []byte, borrowed bool) error {
	if h.flags&(flagSYN|flagACK) != 0 {
		return ErrProtocol // open and acknowledge travel on WindowUpdate frames
	}
	s.mu.Lock()
	st := s.streams[h.streamID]
	if st == nil {
		// Data for a stream this end does not know: reset it so the peer
		// stops sending.
		s.sendControlLocked(typeWindowUpdate, flagRST, h.streamID, 0)
		err := s.flushLocked()
		s.mu.Unlock()
		s.emitDrained()
		return err
	}
	s.mu.Unlock()

	if err := st.pushData(payload, h.flags&flagFIN != 0, borrowed); err != nil {
		return err
	}
	if h.flags&flagRST != 0 {
		s.abortRemote(st, ErrStreamReset)
		return nil
	}
	s.dispatchRead(st)
	if borrowed {
		// The transport reuses its buffer after this turn: copy whatever the
		// callback left unread, for this stream only.
		st.ownInbound()
	}
	return nil
}

func (s *Session) handleWindowUpdate(h frameHeader) error {
	switch {
	case h.flags&flagSYN != 0:
		return s.handleSyn(h)
	case h.flags&flagACK != 0:
		return s.handleAck(h)
	case h.flags&flagRST != 0:
		s.mu.Lock()
		st := s.streams[h.streamID]
		s.mu.Unlock()
		if st != nil {
			s.abortRemote(st, ErrStreamReset)
		}
		return nil
	}
	s.mu.Lock()
	st := s.streams[h.streamID]
	s.mu.Unlock()
	if st == nil {
		return nil // a grant for a finished stream is harmless
	}
	if h.flags&flagFIN != 0 {
		// yamux sends FIN on a WindowUpdate frame; the read side hears the
		// end exactly as it would from a FIN-bearing data frame.
		if err := st.pushData(nil, true, false); err != nil {
			return err
		}
		s.dispatchRead(st)
		if h.length == 0 {
			return nil
		}
	}
	if h.length == 0 {
		return nil
	}
	if granted := st.grantWindow(int64(h.length)); granted {
		// The peer's credit unblocks writers: report the reopened window
		// (a zero-byte write) and run an empty read turn, as a Wake would on
		// a socket connection.
		if obs := s.cfg.Observer; obs != nil {
			obs.OnBytesWritten(st, 0)
		}
		s.dispatchRead(st)
	}
	return nil
}

func (s *Session) handlePing(h frameHeader) error {
	if h.flags&flagSYN != 0 {
		s.mu.Lock()
		s.sendControlLocked(typePing, flagACK, h.streamID, h.length)
		err := s.flushLocked()
		s.mu.Unlock()
		s.emitDrained()
		return err
	}
	s.mu.Lock()
	// Only the outstanding ping's ACK proves liveness. A delayed response to
	// an earlier ping must not mask a peer that stopped answering this one.
	if h.flags&flagACK != 0 && h.length == s.pingSeq {
		s.pingSent = time.Time{}
	}
	s.mu.Unlock()
	return nil
}

// handleSyn accepts a peer stream. The SYN's length is the peer's initial
// receive window.
func (s *Session) handleSyn(h frameHeader) error {
	return s.acceptStream(h.streamID, int64(h.length), nil)
}

// handleSynMetadata accepts a metadata-bearing SYN: the payload's first four
// bytes are the opener's window delta, the rest are its opaque metadata,
// copied out before the transport reuses the buffer.
func (s *Session) handleSynMetadata(h frameHeader, payload []byte) error {
	if len(payload) < 4 {
		return ErrProtocol
	}
	delta := int64(binary.BigEndian.Uint32(payload[:4]))
	meta := append([]byte(nil), payload[4:]...)
	return s.acceptStream(h.streamID, delta, meta)
}

// acceptStream admits a peer-opened stream whose SYN announced delta as its
// receive window and metadata as its opener's opaque bytes.
func (s *Session) acceptStream(id uint32, delta int64, meta []byte) error {
	if err := s.checkPeerStreamID(id); err != nil {
		return err
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	if s.goAway {
		// The peer opened against our GoAway: reset it at once, so its Open
		// fails now instead of waiting for its own open timeout.
		return s.refuseLocked(id)
	}
	if len(s.streams) >= s.cfg.MaxStreams {
		s.sendControlLocked(typeWindowUpdate, flagRST, id, 0)
		err := s.flushLocked()
		s.mu.Unlock()
		s.emitDrained()
		return err
	}
	if _, ok := s.streams[id]; ok {
		s.mu.Unlock()
		return ErrProtocol
	}
	accept := s.cfg.Accept
	handler := s.cfg.Handler
	ch := s.acceptCh
	if ch != nil && len(ch) >= cap(ch) {
		// The accept queue is full: refuse before acknowledging, so the
		// opener sees a failed Open instead of a stream reset after it.
		s.sendControlLocked(typeWindowUpdate, flagRST, id, 0)
		err := s.flushLocked()
		s.mu.Unlock()
		s.emitDrained()
		return err
	}
	s.mu.Unlock()

	if accept != nil {
		if err := accept(); err != nil {
			s.mu.Lock()
			s.sendControlLocked(typeWindowUpdate, flagRST, id, 0)
			err := s.flushLocked()
			s.mu.Unlock()
			s.emitDrained()
			return err
		}
	}

	st := newStream(s, id, false, int64(s.cfg.WindowSize))
	st.sendWin.reset(st.peerWindow(delta))
	st.recvWin.retune(minWindow + delta)
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		// The stream has not been published or opened to the handler. There is
		// no application state to release and no OnClose to deliver for it.
		return nil
	}
	if s.goAway {
		// A Shutdown began while Accept vetted this stream: refuse it like
		// any other open the session will not take.
		return s.refuseLocked(id)
	}
	s.streams[id] = st
	// The ACK announces this end's receive window the same way the SYN did.
	s.sendControlLocked(typeWindowUpdate, flagACK, id, uint32(s.cfg.WindowSize-minWindow))
	err := s.flushLocked()
	s.mu.Unlock()
	s.emitDrained()
	if err != nil {
		// An ACK failure happened before OnOpen. Remove this unexposed stream
		// so transport teardown cannot deliver an unmatched OnClose.
		s.removeStream(st)
		return err
	}

	if handler != nil {
		// The metadata is handed over once, ahead of OnOpen, and the session
		// keeps no reference: a handler that wants it later must copy it.
		if meta != nil {
			if mh, ok := handler.(MetadataHandler); ok {
				mh.OnMetadata(st, meta)
			}
		}
		handler.OnOpen(st)
		return nil
	}
	// The channel is closed under the same lock by onTransportClose. Real
	// callbacks share a transport turn; this also makes an overlapping close
	// safe when the session is driven by a custom transport.
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	select {
	case ch <- Accepted{Stream: st, Metadata: meta}:
		s.mu.Unlock()
	default:
		s.mu.Unlock()
		// Backlog full: refuse the stream instead of blocking the turn.
		s.abortStream(st, ErrMaxStreams)
	}
	return nil
}

// handleAck completes a local Open. The opener holds the stream from Open
// itself, so no OnOpen callback follows: its later callbacks still reach the
// handler.
func (s *Session) handleAck(h frameHeader) error {
	s.mu.Lock()
	st := s.streams[h.streamID]
	wait := s.openWait[h.streamID]
	delete(s.openWait, h.streamID)
	s.mu.Unlock()
	if st == nil || wait == nil {
		return nil
	}
	st.establish(int64(h.length))
	if wait != nil {
		wait <- nil
	}
	return nil
}

// checkPeerStreamID rejects a stream id of the wrong parity for the peer, or
// one that would collide with the local sequence.
func (s *Session) checkPeerStreamID(id uint32) error {
	peerOdd := s.client // the peer of a client session is the server: even ids
	if peerOdd {
		if id%2 != 0 {
			return ErrProtocol
		}
	} else if id%2 == 0 {
		return ErrProtocol
	}
	if id == 0 {
		return ErrProtocol
	}
	return nil
}

// ---------------------------------------------------------------- outbound

// sendControlLocked queues one control frame. The caller holds s.mu.
func (s *Session) sendControlLocked(typ uint8, flags uint16, sid uint32, length uint32) {
	if s.closed {
		return
	}
	frame := make([]byte, headerSize)
	encodeHeader(frame, frameHeader{typ: typ, flags: flags, streamID: sid, length: length})
	s.control = append(s.control, frame)
}

// enqueueFin puts a stream's FIN behind its queued data.
func (s *Session) enqueueFin(st *Stream) {
	s.mu.Lock()
	if st.finState != finNone || s.streams[st.id] != st {
		s.mu.Unlock()
		return
	}
	st.finState = finQueued
	if !st.queued {
		st.queued = true
		s.pending = append(s.pending, st.id)
	}
	s.mu.Unlock()
}

// enqueue appends one owned data frame to a stream's outbound queue. The
// flush is coalesced: the caller asks for one through the transport's Wake,
// so many writers of one turn share a single flush pass instead of each
// taking the session lock through a write of its own.
func (s *Session) enqueue(st *Stream, frame *uio.Buffer) bool {
	s.mu.Lock()
	if s.streams[st.id] != st {
		// The stream left the session (an abort raced this write): queueing
		// now would strand the frame on a stream nothing flushes.
		s.mu.Unlock()
		return false
	}
	st.out = append(st.out, frame)
	if !st.queued {
		st.queued = true
		s.pending = append(s.pending, st.id)
	}
	s.mu.Unlock()
	return true
}

// flush writes queued control frames first, then one frame per stream in
// round-robin order. It is safe from any goroutine.
func (s *Session) flush() {
	s.mu.Lock()
	err := s.flushLocked()
	finFlushed := s.finFlushed
	s.finFlushed = nil
	s.mu.Unlock()
	for _, st := range finFlushed {
		s.tryFinish(st)
	}
	s.emitDrained()
	if err != nil {
		_ = s.conn.CloseWith(err)
	}
	s.maybeFinishDrain()
}

// queueClose schedules a stream's OnClose on the transport turn and wakes the
// session so a turn runs; once the transport is gone, no turn will, so the
// close is delivered in place (the turn is over by then).
func (s *Session) queueClose(st *Stream, err error) {
	s.mu.Lock()
	s.closeQ = append(s.closeQ, closeItem{st: st, err: err})
	closed := s.closed
	s.mu.Unlock()
	if closed {
		s.drainCloses()
		return
	}
	_ = s.conn.Wake()
}

// drainCloses delivers queued closes in order, outside the session lock. A
// handler may close another stream from OnClose, which queues again; the loop
// runs until the queue is empty.
func (s *Session) drainCloses() {
	handler := s.cfg.Handler
	if handler == nil {
		s.mu.Lock()
		s.closeQ = nil
		s.mu.Unlock()
		return
	}
	for {
		s.mu.Lock()
		if len(s.closeQ) == 0 {
			s.mu.Unlock()
			return
		}
		item := s.closeQ[0]
		s.closeQ = s.closeQ[1:]
		s.mu.Unlock()
		handler.OnClose(item.st, item.err)
	}
}

// emitDrained reports, outside the session lock, every stream whose queued
// bytes left for the transport in a flush. A socket leg reports each drained
// write to its observer, and the bridge's resume of a leg it paused depends on
// exactly that signal; streams must report it too, or a leg paused on this
// stream's queue waits for a window grant that a flushed queue never needs.
func (s *Session) emitDrained() {
	if s.drainObs == nil {
		return
	}
	s.mu.Lock()
	drained := s.drained
	s.drained = nil
	s.mu.Unlock()
	for _, st := range drained {
		s.drainObs.OnQueueDrained(st)
	}
}

// tryFinish closes a stream whose directions both ended, whose FIN was accepted
// by the transport and whose queues ran dry. It takes the session lock first, so the
// outbound queue's guards hold while it decides.
func (s *Session) tryFinish(st *Stream) {
	s.mu.Lock()
	if s.streams[st.id] != st {
		s.mu.Unlock()
		return
	}
	st.mu.Lock()
	if st.closedLocal || !st.peerFIN || !st.localFIN || st.finState != finSent || len(st.out) != 0 || st.in.len() != 0 {
		st.mu.Unlock()
		s.mu.Unlock()
		return
	}
	st.closedLocal = true
	cause := st.closedErr
	if cause == nil {
		cause = io.EOF
	}
	st.closedErr = cause
	st.halfSince = time.Time{}
	st.mu.Unlock()
	if s.streams[st.id] == st {
		delete(s.streams, st.id)
	}
	delete(s.openWait, st.id)
	st.dropOutLocked()
	s.mu.Unlock()
	st.deliverClose(cause)
	s.maybeFinishDrain()
}

func (s *Session) flushLocked() error {
	if s.conn == nil || s.closed {
		return nil
	}
	for budget := 0; budget < maxFlushFrames; budget++ {
		if s.goAwaySent && len(s.control) == 0 {
			s.goAwayFlushed = true
		}
		if len(s.control) > 0 {
			frame := s.control[0]
			if _, err := s.conn.Write(frame); err != nil {
				if errors.Is(err, uio.ErrOutboundOverflow) {
					return nil // retried when the transport reports progress
				}
				return err
			}
			s.control = s.control[1:]
			continue
		}
		if len(s.pending) == 0 {
			return nil
		}
		id := s.pending[0]
		st := s.streams[id]
		if st == nil {
			s.pending = s.pending[1:]
			continue
		}
		if len(st.out) == 0 {
			if st.finState != finQueued {
				// A stale queue entry has neither data nor a FIN to submit.
				s.pending = s.pending[1:]
				st.queued = false
				continue
			}
			// Keep the FIN queued until the transport accepts it. In particular,
			// overflow must neither expose a sent FIN nor allocate a retry queue.
			frame := uio.AcquireBuffer(headerSize)
			var hb [headerSize]byte
			encodeHeader(hb[:], frameHeader{typ: typeWindowUpdate, flags: flagFIN, streamID: id})
			_, _ = frame.Write(hb[:])
			if _, err := s.conn.WriteOwned(frame); err != nil {
				if errors.Is(err, uio.ErrOutboundOverflow) {
					uio.ReleaseBuffer(frame)
					return nil
				}
				// Not our closed state: the transport's close path releases
				// the streams, and skipping it would leave them live and the
				// Open waiters asleep.
				return err
			}
			s.pending = s.pending[1:]
			st.queued = false
			st.finState = finSent
			s.finFlushed = append(s.finFlushed, st)
			continue
		}
		frame := st.out[0]
		st.out = st.out[1:]
		if _, err := s.conn.WriteOwned(frame); err != nil {
			if errors.Is(err, uio.ErrOutboundOverflow) {
				st.out = append([]*uio.Buffer{frame}, st.out...)
				return nil
			}
			return err
		}
		if len(st.out) == 0 && st.finState != finQueued {
			s.pending = s.pending[1:]
			st.queued = false
			if s.drainObs != nil {
				s.drained = append(s.drained, st)
			}
		} else {
			// Rotate: move this stream to the back of the queue.
			s.pending = append(s.pending[1:], id)
		}
	}
	return nil
}

// requestFlush schedules a flush for a later turn. Safe to call from a
// transport write callback, where taking the session lock could recurse.
func (s *Session) requestFlush() {
	if s.flushReq.CompareAndSwap(false, true) {
		_ = s.conn.Wake()
	}
}

// onTransportWritable retries frames the transport refused for overflow.
func (s *Session) onTransportWritable() { s.requestFlush() }

// sendWindowUpdate grants the peer credit for consumed bytes.
func (s *Session) sendWindowUpdate(id uint32, delta int64) {
	if delta <= 0 {
		return
	}
	s.mu.Lock()
	s.sendControlLocked(typeWindowUpdate, 0, id, uint32(delta))
	err := s.flushLocked()
	s.mu.Unlock()
	s.emitDrained()
	if err != nil {
		_ = s.conn.CloseWith(err)
	}
}

// reserveInbound charges the session's receive budget. Data the peer was
// granted is never refused: whatever the budget, an arriving frame is
// buffered, and a session over its budget stops parsing instead (see
// exhaustedBudget).
func (s *Session) reserveInbound(n int64) {
	s.mu.Lock()
	s.pendingBytes += n
	s.mu.Unlock()
}

// releaseInbound returns consumed bytes to the session budget and, when
// consumption brings the session back under its low-water mark, wakes the
// transport so the held frames are parsed.
func (s *Session) releaseInbound(n int64) {
	s.mu.Lock()
	s.pendingBytes -= n
	if s.pendingBytes < 0 {
		s.pendingBytes = 0
	}
	resume := s.readHeld && s.pendingBytes <= s.lowWater()
	if resume {
		s.readHeld = false
	}
	s.mu.Unlock()
	if resume {
		_ = s.conn.Wake()
	}
}

// lowWater is the budget consumption must fall to before a held session
// parses again: one stream window of headroom, so a held read does not
// release a few bytes and immediately trip again.
func (s *Session) lowWater() int64 {
	budget := int64(s.cfg.SessionWindow)
	low := budget - int64(s.cfg.WindowSize)
	if low < budget/2 {
		low = budget / 2
	}
	return low
}

// exhaustedBudget reports whether the session buffered more than its budget
// and must hold its transport reads.
func (s *Session) exhaustedBudget() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pendingBytes <= int64(s.cfg.SessionWindow) {
		return false
	}
	s.readHeld = true
	return true
}

// heldReads reports whether parsing stays suspended for lack of budget.
func (s *Session) heldReads() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.readHeld {
		return false
	}
	if s.pendingBytes <= s.lowWater() {
		s.readHeld = false
	}
	return s.readHeld
}

// ------------------------------------------------------------------- state

// refuseLocked resets a peer stream this session will not accept, releases the
// lock and reports the flush error. The caller holds s.mu.
func (s *Session) refuseLocked(id uint32) error {
	s.sendControlLocked(typeWindowUpdate, flagRST, id, 0)
	err := s.flushLocked()
	s.mu.Unlock()
	s.emitDrained()
	return err
}

// removeStream drops a stream from the table and releases its queued frames.
func (s *Session) removeStream(st *Stream) {
	s.mu.Lock()
	if s.streams[st.id] == st {
		delete(s.streams, st.id)
	}
	delete(s.openWait, st.id)
	st.dropOutLocked()
	// The round-robin queue may still name this id; flush skips ids it no
	// longer finds.
	s.mu.Unlock()
	s.maybeFinishDrain()
}

// abortStream resets a stream from the local side: pending data is dropped and
// the peer is told with RST.
func (s *Session) abortStream(st *Stream, cause error) {
	s.mu.Lock()
	if s.streams[st.id] == st {
		delete(s.streams, st.id)
	}
	wait := s.openWait[st.id]
	delete(s.openWait, st.id)
	st.dropOutLocked()
	s.sendControlLocked(typeWindowUpdate, flagRST, st.id, 0)
	err := s.flushLocked()
	s.mu.Unlock()
	s.emitDrained()
	if wait != nil {
		select {
		case wait <- cause:
		default:
		}
	}
	st.resetLocal(cause)
	if err != nil {
		_ = s.conn.CloseWith(err)
	}
	s.maybeFinishDrain()
}

// abortRemote resets a stream the peer reset.
func (s *Session) abortRemote(st *Stream, cause error) {
	s.mu.Lock()
	if s.streams[st.id] == st {
		delete(s.streams, st.id)
	}
	wait := s.openWait[st.id]
	delete(s.openWait, st.id)
	st.dropOutLocked()
	s.mu.Unlock()
	if wait != nil {
		select {
		case wait <- cause:
		default:
		}
	}
	st.resetRemote(cause)
	s.maybeFinishDrain()
}

// dispatchRead runs the stream's read callback: data arrived, the peer ended
// its direction, or a window update woke writers.
func (s *Session) dispatchRead(st *Stream) {
	handler := s.cfg.Handler
	if handler == nil {
		// Pull mode: the Accept owner reads directly.
		s.tryFinish(st)
		return
	}
	if err := handler.OnRead(st); err != nil {
		s.abortStream(st, err)
		return
	}
	s.tryFinish(st)
}
