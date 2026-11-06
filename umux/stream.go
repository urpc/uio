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
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/urpc/uio"
	"github.com/urpc/uio/internal/blockio"
	"github.com/urpc/uio/internal/bytebuf"
	"github.com/urpc/uio/internal/readcontrol"
	"github.com/urpc/uio/internal/writecontrol"
)

// inbound wraps bytebuf.CompositeBuffer with a borrowed transport tail slice,
// mirroring uio's commonConn inbound storage while pooling multi-frame blocks.
type inbound struct {
	buf     bytebuf.CompositeBuffer
	tail    []byte // borrowed transport slice during synchronous OnRead
	borrows int    // active external WriteTo slice borrows
	dropped bool   // deferred Reset while borrows > 0
}

// streamFINState is guarded by the session lock. Closing the writing direction
// first publishes localFIN under the stream lock; until enqueueFin runs, its
// FIN is still finNone. Only a FIN accepted by the transport becomes finSent.
type streamFINState uint8

const (
	finNone streamFINState = iota
	finQueued
	finSent
)

func (b *inbound) len() int {
	if b.dropped {
		return 0
	}
	return b.buf.Len() + len(b.tail)
}

func (b *inbound) setBorrowed(run []byte) {
	b.tail = run
}

func (b *inbound) own() {
	if len(b.tail) > 0 {
		_, _ = b.buf.Write(b.tail)
		b.tail = nil
	}
}

func (b *inbound) append(p []byte) {
	b.own()
	_, _ = b.buf.Write(p)
}

func (b *inbound) peekChunk() []byte {
	if b.dropped {
		return nil
	}
	if !b.buf.Empty() {
		return b.buf.PeekChunk()
	}
	return b.tail
}

func (b *inbound) peek(dst []byte) []byte {
	if len(dst) == 0 || b.len() == 0 {
		return nil
	}
	if b.buf.Empty() {
		return dst[:copy(dst, b.tail)]
	}
	data := b.buf.Peek(dst)
	if len(data) == len(dst) {
		return data
	}
	n := copy(dst, data)
	n += copy(dst[n:], b.tail)
	return dst[:n]
}

func (b *inbound) read(dst []byte) int {
	if b.dropped {
		return 0
	}
	n := 0
	if !b.buf.Empty() {
		n, _ = b.buf.Read(dst)
		if n == len(dst) {
			return n
		}
	}
	if len(b.tail) > 0 {
		k := copy(dst[n:], b.tail)
		b.tail = b.tail[k:]
		if len(b.tail) == 0 {
			b.tail = nil
		}
		n += k
	}
	return n
}

func (b *inbound) consume(n int) {
	if n <= 0 || b.dropped {
		return
	}
	bufLen := b.buf.Len()
	if bufLen > 0 {
		if n <= bufLen {
			b.buf.Discard(n)
			return
		}
		b.buf.Discard(bufLen)
		n -= bufLen
	}
	if n >= len(b.tail) {
		b.tail = nil
	} else {
		b.tail = b.tail[n:]
	}
}

func (b *inbound) drop() {
	b.tail = nil
	if b.borrows > 0 {
		b.dropped = true
		return
	}
	b.buf.Reset()
}

// Stream is one multiplexed byte stream: a uio.Conn without a socket. Read,
// Discard and WriteTo may be used from any goroutine, with one reader consuming
// the stream at a time. Their method calls are synchronized with arrivals;
// slices returned by PeekChunk have the narrower callback lifetime below.
type Stream struct {
	streamReadAccess
	streamWriteAccess
	streamBlockAccess
	sess         *Session
	id           uint32
	readControl  atomic.Pointer[readcontrol.Control]
	writeControl atomic.Pointer[writecontrol.Control]
	blockControl atomic.Pointer[blockio.State]
	pullRead     atomic.Bool
	localClosed  atomic.Bool

	mu sync.Mutex
	// wmu orders a write's credit-taking and frame enqueues against
	// CloseWrite, so a FIN never overtakes data a successful Write accepted.
	wmu sync.Mutex
	// in is the unread inbound payload; a leading run may alias the
	// transport's buffer, and ownInbound copies it before the transport
	// consumes that buffer.
	in inbound
	// callbackActive keeps FIN completion behind the active read/timer callback:
	// draining its last bytes must leave time to publish a pause, yield, or error.
	callbackActive bool
	// peerFIN reports the peer ended its writing direction.
	peerFIN bool
	// localFIN reports this end ended its writing direction.
	localFIN bool
	// opening is set on a locally opened stream until its ACK arrives.
	opening bool
	// openedAt starts the open timeout.
	openedAt time.Time
	// halfSince starts the half-close timeout once one direction ended.
	halfSince time.Time
	// closedLocal reports this end closed or reset the stream.
	closedLocal bool
	// closedErr is the cause reads and writes report after a close.
	closedErr error
	// closeDelivered guards the single OnClose callback.
	closeDelivered bool

	// sendWin is the credit the peer granted this end.
	sendWin sendWindow
	// recvWin is the window this end advertised.
	recvWin recvWindow

	// out is the queue of data frames waiting for the transport.
	out []*uio.Buffer
	// queued reports the stream is on the session's round-robin queue.
	queued bool
	// finState tracks the local FIN independently of the close intent. A
	// stream cannot finish until the transport accepted its FIN.
	finState streamFINState

	// wakeAt and wakeDu are this stream's uio timer.
	wakeAt time.Time
	wakeDu time.Duration
	// readDeadline and writeDeadline are absolute stream deadlines. Unlike
	// wakeAt, they close the stream when they expire, matching uio.Conn's
	// socket deadline contract.
	readDeadline  time.Time
	writeDeadline time.Time

	userdata any
	// wakePending asks the session to run an empty read turn on the next
	// transport turn (uio Wake semantics).
	wakePending bool
}

var _ uio.Conn = (*Stream)(nil)

func newStream(s *Session, id uint32, local bool, ourWindow int64) *Stream {
	// Every sender starts at the yamux v1 floor; the peer's SYN or ACK
	// announces its receive window as a delta above the floor (establish).
	return &Stream{
		sess:     s,
		id:       id,
		opening:  local,
		openedAt: time.Now(),
		sendWin:  newSendWindow(minWindow),
		recvWin:  newRecvWindow(ourWindow),
	}
}

// ID returns the wire stream id.
func (st *Stream) ID() uint32 { return st.id }

// establish completes a locally opened stream: the ACK's length is the
// peer's receive window, a delta above the protocol floor.
func (st *Stream) establish(delta int64) {
	st.mu.Lock()
	st.opening = false
	st.sendWin.reset(st.peerWindow(delta))
	// The peer's announcement is also its own send ceiling: size our grant
	// threshold by it so a sender capped below a quarter of our window still
	// gets its credit back.
	st.recvWin.retune(int64(minWindow) + delta)
	st.mu.Unlock()
}

// peerWindow turns a peer's announced window delta into send credit, capped at
// this end's configured window so a peer cannot inflate the queue it makes us
// buffer. The caller holds st.mu.
func (st *Stream) peerWindow(delta int64) int64 {
	w := int64(minWindow) + delta
	if cap := int64(st.sess.cfg.WindowSize); w > cap {
		w = cap
	}
	return w
}

// ------------------------------------------------------------------ inbound

// pushData accounts an inbound data frame and its FIN flag. It runs on the
// transport turn.
func (st *Stream) pushData(payload []byte, fin bool, borrowed bool) error {
	n := int64(len(payload))
	if n > 0 {
		st.sess.reserveInbound(n)
	}
	st.mu.Lock()
	if st.closedLocal {
		st.mu.Unlock()
		st.sess.releaseInbound(n)
		return nil // late data after a local reset is dropped
	}
	if n > 0 {
		if err := st.recvWin.onData(n); err != nil {
			st.mu.Unlock()
			st.sess.releaseInbound(n)
			return err
		}
		if st.in.len() == 0 && borrowed {
			st.in.setBorrowed(payload)
		} else {
			st.in.append(payload)
		}
	}
	halfStarted := false
	if fin && !st.peerFIN {
		st.peerFIN = true
		if st.localFIN {
			// Both directions ended; the session settles the stream after
			// the read callback sees the end.
			st.halfSince = time.Time{}
		} else {
			st.halfSince = time.Now()
			halfStarted = true
		}
	}
	st.mu.Unlock()
	if halfStarted {
		// Only the half-close deadline changed: data frames leave the timer
		// heap alone, or every message would rescan every stream.
		st.sess.rearmTimer()
	}
	return nil
}

// ownInbound copies a borrowed inbound run into stream-owned memory; the
// session calls it before the transport consumes the buffer it borrowed.
func (st *Stream) ownInbound() {
	st.mu.Lock()
	st.in.own()
	st.mu.Unlock()
}

// grantWindow applies a peer WindowUpdate, reporting whether it granted
// credit.
func (st *Stream) grantWindow(delta int64) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.closedLocal {
		return false
	}
	return st.sendWin.grant(delta) == nil
}

// deliverClose hands OnClose to the session, which runs it on the transport
// turn: an Abort from another goroutine must never run the handler's
// callback concurrently with that stream's OnRead.
func (st *Stream) deliverClose(err error) {
	st.mu.Lock()
	if st.closeDelivered {
		st.mu.Unlock()
		return
	}
	st.closeDelivered = true
	st.mu.Unlock()
	st.notifyBlockClose(err)
	if st.sess.cfg.Handler != nil {
		st.sess.queueClose(st, err)
	}
}

// resetLocal closes the stream from this end.
func (st *Stream) resetLocal(err error) {
	st.mu.Lock()
	if st.closedLocal {
		st.mu.Unlock()
		return
	}
	st.closedLocal = true
	st.closedErr = err
	released := st.dropLocked()
	st.mu.Unlock()
	st.sess.releaseInbound(released)
	st.deliverClose(err)
}

// resetRemote closes the stream because the peer reset it.
func (st *Stream) resetRemote(err error) {
	st.mu.Lock()
	if st.closedLocal {
		st.mu.Unlock()
		return
	}
	st.closedLocal = true
	st.closedErr = err
	released := st.dropLocked()
	st.mu.Unlock()
	st.sess.releaseInbound(released)
	st.deliverClose(err)
}

// dropLocked releases this stream's buffered payload, returning the inbound
// budget it held. The outbound queue belongs to the session and is dropped by
// the session's removal paths.
func (st *Stream) dropLocked() int64 {
	released := int64(st.in.len())
	st.in.drop()
	return released
}

// dropOutLocked releases this stream's queued frames. The caller holds the
// session lock.
func (st *Stream) dropOutLocked() {
	for _, frame := range st.out {
		uio.ReleaseBuffer(frame)
	}
	st.out = nil
	st.queued = false
	st.finState = finNone
}

// Peek returns the next inbound bytes without consuming them.
func (st *Stream) Peek(p []byte) []byte {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.in.peek(p)
}

// PeekChunk borrows the current unread chunk during this stream's Handler
// callback. The slice expires when the callback returns or a Read, Discard or
// WriteTo consumes data. Copy bytes that must survive either event. Pull-mode
// readers use Read or WriteTo instead: arrivals can replace borrowed storage
// between calls outside the transport's callback.
func (st *Stream) PeekChunk() []byte {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.in.peekChunk()
}

// Read copies buffered inbound bytes; it never blocks and reports io.EOF only
// once the peer ended its direction and the buffered bytes were drained.
func (st *Stream) Read(p []byte) (int, error) {
	st.mu.Lock()
	if len(p) == 0 {
		// A zero-length read consumes nothing; the end of the stream is
		// reported by State or by a read with a buffer.
		st.mu.Unlock()
		return 0, nil
	}
	if st.in.len() > 0 {
		n := st.in.read(p)
		grant := st.recvWin.consume(int64(n))
		settle := st.in.len() == 0 && st.peerFIN && st.localFIN && !st.closedLocal
		st.mu.Unlock()
		st.sess.releaseInbound(int64(n))
		if st.sess.cfg.Observer != nil {
			st.sess.cfg.Observer.OnBytesRead(st, n)
		}
		if grant > 0 {
			st.sess.sendWindowUpdate(st.id, grant)
		}
		if settle || st.sess.cfg.Handler == nil {
			st.sess.tryFinish(st)
		}
		return n, nil
	}
	if st.closedLocal {
		err := st.closedErr
		st.mu.Unlock()
		if err == nil {
			err = io.EOF
		}
		return 0, err
	}
	peerFIN := st.peerFIN
	st.mu.Unlock()
	if peerFIN {
		return 0, io.EOF
	}
	return 0, nil
}

// Discard drops n unread bytes.
func (st *Stream) Discard(n int) (int, error) {
	st.mu.Lock()
	if n < 0 || n > st.in.len() {
		n = st.in.len()
	}
	st.in.consume(n)
	grant := st.recvWin.consume(int64(n))
	settle := st.in.len() == 0 && st.peerFIN && st.localFIN && !st.closedLocal
	st.mu.Unlock()
	st.sess.releaseInbound(int64(n))
	if grant > 0 {
		st.sess.sendWindowUpdate(st.id, grant)
	}
	if settle {
		st.sess.tryFinish(st)
	}
	return n, nil
}

// InboundBuffered reports unread inbound bytes.
func (st *Stream) InboundBuffered() int {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.in.len()
}

// ReserveInbound gathers the first n buffered inbound bytes into one contiguous
// chunk when possible.
func (st *Stream) ReserveInbound(n int) error {
	if n <= 0 {
		return nil
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.in.dropped || st.borrowsActive() {
		return nil
	}
	if st.in.buf.Empty() {
		if len(st.in.tail) >= n {
			return nil
		}
	} else if len(st.in.buf.PeekChunk()) >= n {
		return nil
	}
	st.in.tail = st.in.buf.Gather(n, st.in.tail)
	if len(st.in.tail) == 0 {
		st.in.tail = nil
	}
	return nil
}

func (st *Stream) borrowsActive() bool { return st.in.borrows > 0 }

// WriteTo copies unread inbound bytes to w. The slice handed to w stays valid
// for the length of that Write even while the transport keeps delivering.
func (st *Stream) WriteTo(w io.Writer) (int64, error) {
	var total int64
	for {
		st.mu.Lock()
		st.in.own()
		buf := st.in.peekChunk()
		if len(buf) == 0 {
			st.mu.Unlock()
			return total, nil
		}
		st.in.borrows++
		st.mu.Unlock()

		n, err := w.Write(buf)
		total += int64(n)

		st.mu.Lock()
		st.in.borrows--
		if st.in.borrows == 0 && st.in.dropped {
			st.in.buf.Reset()
		}
		st.mu.Unlock()
		if n > 0 {
			_, _ = st.Discard(n)
		}
		if err != nil {
			return total, err
		}
		if n == 0 {
			return total, io.ErrShortWrite
		}
	}
}

// ----------------------------------------------------------------- outbound

// Write frames p into queued data frames, spending the peer's window. When the
// window is exhausted it returns ErrOutboundOverflow; writers retry once the
// peer's WindowUpdate wakes the stream.
func (st *Stream) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	st.wmu.Lock()
	st.mu.Lock()
	if st.closedLocal || st.opening {
		err := st.closedErr
		st.mu.Unlock()
		st.wmu.Unlock()
		if err == nil {
			err = ErrClosed
		}
		return 0, err
	}
	if st.localFIN {
		st.mu.Unlock()
		st.wmu.Unlock()
		return 0, ErrClosed
	}
	avail := st.sendWin.available()
	if avail <= 0 {
		st.mu.Unlock()
		st.wmu.Unlock()
		return 0, uio.ErrOutboundOverflow
	}
	n := len(p)
	if int64(n) > avail {
		n = int(avail)
	}
	st.sendWin.take(int64(n))
	maxFrame := st.sess.cfg.MaxFrame
	st.mu.Unlock()

	if !st.frameData(p[:n], maxFrame) {
		st.wmu.Unlock()
		st.sess.requestFlush()
		return n, ErrClosed
	}
	st.wmu.Unlock()
	st.sess.requestFlush()
	if st.sess.cfg.Observer != nil {
		st.sess.cfg.Observer.OnBytesWritten(st, n)
	}
	if n < len(p) {
		// A short write means the window is spent; io.Writer requires an
		// error alongside it.
		return n, uio.ErrOutboundOverflow
	}
	return n, nil
}

// frameData copies p into queued data frames. It reports false when the
// session no longer holds the stream — an abort raced the write — so the
// caller can leave without queueing onto a dropped stream.
func (st *Stream) frameData(p []byte, maxFrame int) bool {
	for sent := 0; sent < len(p); {
		chunk := len(p) - sent
		if chunk > maxFrame {
			chunk = maxFrame
		}
		frame := uio.AcquireBuffer(headerSize + chunk)
		var hb [headerSize]byte
		encodeHeader(hb[:], frameHeader{typ: typeData, flags: 0, streamID: st.id, length: uint32(chunk)})
		_, _ = frame.Write(hb[:])
		_, _ = frame.Write(p[sent : sent+chunk])
		if !st.sess.enqueue(st, frame) {
			uio.ReleaseBuffer(frame)
			return false
		}
		sent += chunk
	}
	return true
}

// WriteString implements io.StringWriter.
func (st *Stream) WriteString(s string) (int, error) { return st.Write([]byte(s)) }

// WriteByte implements io.ByteWriter.
func (st *Stream) WriteByte(c byte) error {
	_, err := st.Write([]byte{c})
	return err
}

// Writev writes the buffers as one stream segment.
func (st *Stream) Writev(vec [][]byte) (int, error) {
	total := 0
	for _, b := range vec {
		n, err := st.Write(b)
		total += n
		if err != nil {
			return total, err
		}
		if n < len(b) {
			return total, uio.ErrOutboundOverflow
		}
	}
	return total, nil
}

// WriteOwned frames an owned buffer. Streams prepend the frame header, so the
// payload is copied once and the buffer released; it is not a zero-copy path.
// Like uio's contract, an overflow refuses the whole write and leaves the
// buffer with the caller.
func (st *Stream) WriteOwned(buffer *uio.Buffer) (int, error) {
	if buffer == nil {
		return 0, nil
	}
	payload := buffer.Bytes()
	n := len(payload)
	if n == 0 {
		uio.ReleaseBuffer(buffer)
		return 0, nil
	}
	// The credit check and the enqueues share the write lock, so another
	// writer cannot spend the credit between them: the whole buffer is
	// accepted or refused whole. Only ErrOutboundOverflow leaves ownership
	// with the caller; every other outcome consumes it.
	st.wmu.Lock()
	st.mu.Lock()
	open := !st.closedLocal && !st.opening && !st.localFIN
	if !open {
		err := st.closedErr
		st.mu.Unlock()
		st.wmu.Unlock()
		if err == nil {
			err = ErrClosed
		}
		if !errors.Is(err, uio.ErrOutboundOverflow) {
			uio.ReleaseBuffer(buffer)
		}
		return 0, err
	}
	if int64(n) > st.sendWin.available() {
		st.mu.Unlock()
		st.wmu.Unlock()
		return 0, uio.ErrOutboundOverflow
	}
	st.sendWin.take(int64(n))
	maxFrame := st.sess.cfg.MaxFrame
	st.mu.Unlock()

	ok := st.frameData(payload, maxFrame)
	st.wmu.Unlock()
	st.sess.requestFlush()
	uio.ReleaseBuffer(buffer)
	if !ok {
		return n, ErrClosed
	}
	if st.sess.cfg.Observer != nil {
		st.sess.cfg.Observer.OnBytesWritten(st, n)
	}
	return n, nil
}

// OutboundBuffered reports bytes queued but not yet handed to the transport.
// The outbound queue is guarded by the session lock.
func (st *Stream) OutboundBuffered() int {
	s := st.sess
	s.mu.Lock()
	defer s.mu.Unlock()
	total := 0
	for _, f := range st.out {
		total += f.Len()
	}
	return total
}

// Flush pushes queued frames to the transport.
func (st *Stream) Flush() error {
	st.sess.flush()
	return nil
}

// -------------------------------------------------------------------- close

// CloseWrite half-closes this end's writing direction: queued data drains,
// then a FIN follows.
func (st *Stream) CloseWrite() error {
	st.wmu.Lock()
	st.mu.Lock()
	if st.closedLocal {
		st.mu.Unlock()
		st.wmu.Unlock()
		return net.ErrClosed
	}
	if st.localFIN {
		st.mu.Unlock()
		st.wmu.Unlock()
		return nil
	}
	st.localFIN = true
	st.halfSince = time.Now()
	st.mu.Unlock()
	st.sess.enqueueFin(st)
	st.wmu.Unlock()
	st.sess.requestFlush()
	st.sess.tryFinish(st)
	st.sess.rearmTimer()
	return nil
}

// Close closes the stream gracefully, draining queued data first.
func (st *Stream) Close() error {
	st.localClosed.Store(true)
	st.mu.Lock()
	if st.closedLocal {
		st.mu.Unlock()
		return net.ErrClosed
	}
	st.closedErr = nil
	st.mu.Unlock()
	return st.CloseWrite()
}

// Abort immediately resets the stream with cause, dropping unsent data.
func (st *Stream) Abort(cause error) error {
	if cause == nil {
		cause = net.ErrClosed
	}
	st.localClosed.Store(true)
	st.sess.abortStream(st, cause)
	return nil
}

// State reports the stream's directions.
func (st *Stream) State() uio.ConnState {
	st.mu.Lock()
	defer st.mu.Unlock()
	var state uio.ConnState
	if st.peerFIN && st.in.len() == 0 {
		state |= uio.StateReadEnded
	}
	if st.localFIN {
		state |= uio.StateWriteEnded
	}
	if st.closedLocal {
		state |= uio.StateClosed
	}
	return state
}

// ------------------------------------------------------------------- timers

// SetWakeDeadline arms the stream's one-shot timer, replacing any repeating
// interval; a zero time cancels.
func (st *Stream) SetWakeDeadline(when time.Time) error {
	st.mu.Lock()
	st.wakeAt = when
	st.wakeDu = 0
	st.mu.Unlock()
	st.sess.rearmTimer()
	return nil
}

// SetWakeInterval repeats the stream's timer, replacing any pending deadline;
// its first callback comes one interval from now. A zero or negative interval
// cancels.
func (st *Stream) SetWakeInterval(interval time.Duration) error {
	st.mu.Lock()
	if interval <= 0 {
		st.wakeAt = time.Time{}
		st.wakeDu = 0
	} else {
		st.wakeDu = interval
		st.wakeAt = time.Now().Add(interval)
	}
	st.mu.Unlock()
	st.sess.rearmTimer()
	return nil
}

// Wake schedules an empty read turn for the stream. A stream in pull mode
// has no read callback to run.
func (st *Stream) Wake() error {
	s := st.sess
	if s.cfg.Handler == nil && !st.pullRead.Load() {
		return nil
	}
	s.mu.Lock()
	if s.closed {
		// Teardown drops the session queues before closing every stream. A
		// wake in that interval must not recreate work the transport cannot run.
		s.mu.Unlock()
		return nil
	}
	st.mu.Lock()
	if st.closedLocal {
		// A closed stream runs no more read turns: the close is its last
		// callback.
		st.mu.Unlock()
		s.mu.Unlock()
		return nil
	}
	if !st.wakePending {
		st.wakePending = true
		s.wakeQ = append(s.wakeQ, st)
	}
	st.mu.Unlock()
	s.mu.Unlock()
	return s.conn.Wake()
}

// ------------------------------------------------------------------ surface

// LocalAddr mirrors the transport's local address.
func (st *Stream) LocalAddr() net.Addr { return st.sess.conn.LocalAddr() }

// RemoteAddr mirrors the transport's remote address.
func (st *Stream) RemoteAddr() net.Addr { return st.sess.conn.RemoteAddr() }

// Userdata returns this stream's user data.
func (st *Stream) Userdata() any {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.userdata
}

// SetUserdata replaces this stream's user data.
func (st *Stream) SetUserdata(value any) {
	st.mu.Lock()
	st.userdata = value
	st.mu.Unlock()
}

// SetDeadline sets both stream deadlines. A deadline that passes resets the
// stream with os.ErrDeadlineExceeded, matching uio.Conn's socket semantics.
// A zero time clears both deadlines.
func (st *Stream) SetDeadline(t time.Time) error {
	return st.setDeadlines(t, t, true, true)
}

// SetReadDeadline sets the absolute read deadline. Streams do not block in
// Read, so expiry closes the stream rather than interrupting a blocked call.
// A zero time clears the deadline.
func (st *Stream) SetReadDeadline(t time.Time) error {
	return st.setDeadlines(t, time.Time{}, true, false)
}

// SetWriteDeadline sets the absolute write deadline. A zero time clears the
// deadline.
func (st *Stream) SetWriteDeadline(t time.Time) error {
	return st.setDeadlines(time.Time{}, t, false, true)
}

// setDeadlines updates the selected deadline fields while the stream remains
// in its session. The session lock first matches timer scans and prevents a
// deadline from being armed on a stream that has already left the session.
func (st *Stream) setDeadlines(read, write time.Time, setRead, setWrite bool) error {
	s := st.sess
	s.mu.Lock()
	if s.closed || s.streams[st.id] != st {
		s.mu.Unlock()
		return net.ErrClosed
	}
	st.mu.Lock()
	if st.closedLocal {
		st.mu.Unlock()
		s.mu.Unlock()
		return net.ErrClosed
	}
	if setRead {
		st.readDeadline = read
	}
	if setWrite {
		st.writeDeadline = write
	}
	st.mu.Unlock()
	s.mu.Unlock()
	s.rearmTimer()
	return nil
}
