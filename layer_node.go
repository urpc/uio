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

// One connection's layers: the node each layer instance works on, the faces
// it sees (LayerContext, Lower, Upper), and the Conn surface the top of the
// stack hands the Handler.

package uio

import (
	"errors"
	"io"
	"net"
	"sync/atomic"

	"time"

	"github.com/urpc/uio/internal/blockio"
	"github.com/urpc/uio/internal/readcontrol"
	"github.com/urpc/uio/internal/writecontrol"
)

// layerOps is the set of optional interfaces a LayerHandler implements,
// resolved once when the stack attaches it: the close paths read the table
// instead of asserting at every call, and a layer without a capability costs
// one nil check.
type layerOps struct {
	closing     func(LayerContext, error) // LayerCloser
	closeWriter func(LayerContext) error  // CloseWriter
	held        func(LayerContext) int    // OutboundHolder
}

// opsOf resolves a LayerHandler's optional interfaces.
func opsOf(h LayerHandler) layerOps {
	var ops layerOps
	if closer, ok := h.(LayerCloser); ok {
		ops.closing = closer.OnClosing
	}
	if closeWriter, ok := h.(CloseWriter); ok {
		ops.closeWriter = closeWriter.CloseWrite
	}
	if holder, ok := h.(OutboundHolder); ok {
		ops.held = holder.OutboundHeld
	}
	return ops
}

// layerNode is one layer of one connection: the state the layer works
// on, and the Conn the layer above reads and writes through.
type layerNode struct {
	readControlAccess
	writeControlAccess
	blockControlAccess
	readControl  atomic.Pointer[readcontrol.Control]  // this node's delivered input consumer gate
	writeControl atomic.Pointer[writecontrol.Control] // optional write-progress watchers
	stack        *stack
	index        int
	lower        Conn
	conn         LayerHandler // the layer's instance on this connection
	upper        Upper        // the face the layer delivers up, cached to allocate nothing

	ops    layerOps
	opened atomic.Bool // the layer's OnOpen ran; set on a turn
	ended  atomic.Bool // the layer ended its stream up; set on a turn, read by CloseWrite/State
	// gate packs the two facts a write is admitted by: openUp, the layer
	// opened the layers above it, and writeShut, its writing half ended. One
	// load on the write path admits or refuses.
	gate    atomic.Uint32
	closing atomic.Bool // this layer's close walk began; see endClose
	// shutPending marks a CloseWrite end not yet passed below, and writers
	// counts the writes in flight into the layer — the Handler's through the
	// top, a layer's own through every layer it writes below — which the end
	// waits for; see shutDown.
	shutPending atomic.Bool
	writers     atomic.Int32
	stalled     atomic.Bool // the layer stopped short; see Upper.Yield
	paced       atomic.Bool // a full delivery kept the layer from running; the read above wakes it
	woken       atomic.Bool // the consumer above scheduled a Wake; holds EOF while delivered > 0

	// delivered is what the layer handed up that the layer above has not
	// read, from at on; only the connection's turns touch it.
	delivered []byte
	at        int
}

// The two facts a write into a layer is admitted by; see layerNode.gate.
const (
	gateOpenUp    uint32 = 1 << iota // the layer opened the layers above it
	gateWriteShut                    // the layer above ended its writing half
)

func (l *layerNode) openUp() bool { return l.gate.Load()&gateOpenUp != 0 }

func (l *layerNode) markOpenUp() { l.gate.Or(gateOpenUp) }

// markWriteShut ends the layer's writing half, reporting whether this call
// ended it: a second CloseWrite does nothing.
func (l *layerNode) markWriteShut() bool {
	for {
		gate := l.gate.Load()
		if gate&gateWriteShut != 0 {
			return false
		}
		if l.gate.CompareAndSwap(gate, gate|gateWriteShut) {
			return true
		}
	}
}

// layerCtx is the LayerContext a layer holds: the connection below as Lower,
// the delivery buffer above as Upper, and this layer's lifecycle and timers.
type layerCtx struct{ l *layerNode }

var (
	_ LayerContext = layerCtx{}
	_ Upper        = layerUpper{}
	_ Conn         = (*layerNode)(nil)
)

func (lc layerCtx) Lower() Lower { return lc.l.lower }

// Upper returns the node's cached face, so delivering up allocates nothing.
func (lc layerCtx) Upper() Upper { return lc.l.upper }

// Open records that the layers above may open: their OnOpen runs on this
// turn once this callback returned — never inside it — so this layer's lock,
// if it holds one, stays its own.
func (lc layerCtx) Open() { lc.l.markOpenUp() }

func (lc layerCtx) SetTimer(when time.Time) error {
	return lc.l.stack.setTimer(lc.l.index, when, 0)
}

func (lc layerCtx) SetTimerInterval(interval time.Duration) error {
	if interval <= 0 {
		return lc.l.stack.setTimer(lc.l.index, time.Time{}, 0)
	}
	return lc.l.stack.setTimer(lc.l.index, time.Now().Add(interval), interval)
}

func (lc layerCtx) Close() error            { return lc.l.endClose(nil, true, false) }
func (lc layerCtx) Abort(cause error) error { return lc.l.endClose(cause, false, false) }

func (lc layerCtx) Wake() error { return lc.l.stack.socket.Wake() }

// layerUpper is the layer above's inbound delivery buffer.
type layerUpper struct{ l *layerNode }

func (u layerUpper) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	l := u.l
	l.compact()
	l.delivered = append(l.delivered, p...)
	return len(p), nil
}

func (u layerUpper) End() { u.l.ended.Store(true) }

func (u layerUpper) Reserve(n int) []byte {
	l := u.l
	l.compact()
	if spare := cap(l.delivered) - len(l.delivered); spare < n {
		// Grow by doubling, as append does: a layer that commits a little at
		// a time copies what it holds a logarithmic number of times.
		grown := make([]byte, len(l.delivered), max(len(l.delivered)+n, 2*cap(l.delivered)))
		copy(grown, l.delivered)
		l.delivered = grown
	}
	return l.delivered[len(l.delivered):cap(l.delivered)]
}

func (u layerUpper) Commit(n int) {
	l := u.l
	l.delivered = l.delivered[:len(l.delivered)+n]
}

func (u layerUpper) Buffered() int { return len(u.l.delivered) - u.l.at }

func (u layerUpper) Yield() { u.l.stalled.Store(true) }

// waitingBelow is what the layer can still read from the one below: the
// socket's buffered bytes for the first layer, the delivered bytes of the
// layer below for the others.
func (l *layerNode) waitingBelow() int {
	if below, ok := l.lower.(*layerNode); ok {
		return below.held()
	}
	return l.lower.InboundBuffered()
}

// maxRetainedDelivery is the most capacity a layer keeps for its next
// delivery once everything delivered was read; a larger buffer, grown for a
// burst, goes back to the heap.
const maxRetainedDelivery = 256 << 10

// reclaim drops a fully read delivery, so the next one reuses its capacity,
// or frees it when a burst grew it large.
func (l *layerNode) reclaim() {
	if l.at > 0 && l.at == len(l.delivered) {
		if cap(l.delivered) > maxRetainedDelivery {
			l.delivered, l.at = nil, 0
			return
		}
		l.delivered, l.at = l.delivered[:0], 0
	}
}

// compact moves what the layer above has not read to the front before the
// layer delivers more, so a reader that always leaves a little unread — a
// partial header, the start of a message — does not keep everything read
// before it alive. It runs as the layer delivers, on its OnRead or
// OnTimer, when no slice the layer above was lent is in use any more.
func (l *layerNode) compact() {
	if l.at == 0 {
		return
	}
	n := copy(l.delivered, l.delivered[l.at:])
	l.delivered, l.at = l.delivered[:n], 0
	if n == 0 && cap(l.delivered) > maxRetainedDelivery {
		l.delivered = nil
	}
}

// socketLower is the first layer's Lower: the socket, with the first layer's
// timer in place of the socket's single wake slot.
type socketLower struct {
	readControlAccess
	writeControlAccess
	Conn
	stack *stack
}

func (s socketLower) Unwrap() Conn { return s.Conn }

func (s socketLower) Close() error            { return s.stack.closeSocket(nil, true) }
func (s socketLower) Abort(cause error) error { return s.stack.closeSocket(cause, false) }

// PacketSource reports the socket's datagrams. Only the first layer's Lower —
// the socket itself — can answer for them: what a layer builds above another
// one is a byte stream, whatever the socket beneath it carries. A layer that
// cares about the shape of its direct lower asserts this interface on Lower():
//
//	if source, ok := ctx.Lower().(interface{ PacketSource() bool }); ok && source.PacketSource() { … }
func (s socketLower) PacketSource() bool {
	addr := s.Conn.LocalAddr()
	return addr != nil && packetNetwork(addr.Network())
}

// SocketReadSize reports the Events' read buffer: the most bytes one socket
// read, or one datagram, carries. A layer whose units must fit inside a read
// — a segment, a record — checks it at OnOpen and refuses what cannot fit:
//
//	if size, ok := ctx.Lower().(interface{ SocketReadSize() int }); ok { … }
func (s socketLower) SocketReadSize() int {
	if c, ok := s.Conn.(*fdConn); ok {
		return c.events().readBufSize
	}
	return 0
}

func (s socketLower) SetWakeDeadline(when time.Time) error { return s.stack.setTimer(0, when, 0) }

func (s socketLower) SetWakeInterval(interval time.Duration) error {
	if interval <= 0 {
		return s.stack.setTimer(0, time.Time{}, 0)
	}
	return s.stack.setTimer(0, time.Now().Add(interval), interval)
}

// The Conn the layer above holds. Its reads are what the layer delivered;
// its writes enter the layer; the rest is the connection's own.

func (l *layerNode) LocalAddr() net.Addr  { return l.lower.LocalAddr() }
func (l *layerNode) RemoteAddr() net.Addr { return l.lower.RemoteAddr() }

func (l *layerNode) Unwrap() Conn                       { return l.lower }
func (l *layerNode) Userdata() any                      { return l.stack.socket.Userdata() }
func (l *layerNode) SetUserdata(value any)              { l.stack.socket.SetUserdata(value) }
func (l *layerNode) SetDeadline(t time.Time) error      { return l.lower.SetDeadline(t) }
func (l *layerNode) SetReadDeadline(t time.Time) error  { return l.lower.SetReadDeadline(t) }
func (l *layerNode) SetWriteDeadline(t time.Time) error { return l.lower.SetWriteDeadline(t) }

func (l *layerNode) Wake() error {
	if err := l.stack.socket.Wake(); err != nil {
		return err
	}
	l.woken.Store(true)
	return nil
}
func (l *layerNode) InboundBuffered() int                 { return l.held() }
func (l *layerNode) Close() error                         { return l.endClose(nil, true, true) }
func (l *layerNode) Abort(cause error) error              { return l.endClose(cause, false, true) }
func (l *layerNode) held() int                            { return len(l.delivered) - l.at }
func (l *layerNode) slot() int                            { return l.index + 1 }
func (l *layerNode) Peek(b []byte) []byte                 { return b[:copy(b, l.delivered[l.at:])] }
func (l *layerNode) PeekChunk() []byte                    { return l.delivered[l.at:] }
func (l *layerNode) WriteByte(b byte) error               { _, err := l.Write([]byte{b}); return err }
func (l *layerNode) WriteString(s string) (int, error)    { return l.Write([]byte(s)) }
func (l *layerNode) SetWakeDeadline(when time.Time) error { return l.stack.setTimer(l.slot(), when, 0) }

// State reports where the connection stands; see ConnState.
func (l *layerNode) State() ConnState {
	var state ConnState
	if l.ended.Load() && l.held() == 0 {
		state |= StateReadEnded
	}
	if l.gate.Load()&gateWriteShut != 0 {
		state |= StateWriteEnded
	}
	switch l.stack.state.Load() & stackPhaseMask {
	case stackDraining, stackAborting, stackEnding:
		state |= StateClosing
	case stackClosed:
		state |= StateClosing | StateClosed
	}
	return state
}

// SetWakeInterval arms the layer above's timer to repeat; see Conn.
func (l *layerNode) SetWakeInterval(interval time.Duration) error {
	if interval <= 0 {
		return l.stack.setTimer(l.slot(), time.Time{}, 0)
	}
	return l.stack.setTimer(l.slot(), time.Now().Add(interval), interval)
}

// ReserveInbound succeeds without copying: what a layer delivers is one
// chunk already. The limit is the connection's, as the socket's is.
func (l *layerNode) ReserveInbound(n int) error {
	if limit := l.stack.inLimit; limit > 0 && n > limit {
		return ErrInboundOverflow
	}
	return nil
}

// Discard drops the next n bytes delivered, all of them when n is negative
// or past what is held, as the socket's does.
func (l *layerNode) Discard(n int) (int, error) {
	if held := l.held(); n < 0 || n > held {
		n = held
	}
	l.at += n
	l.reclaim()
	l.roomMade()
	return n, nil
}

// Read is non-blocking, as the socket's is, and reports io.EOF once the
// layer ended its stream and everything it delivered was read.
func (l *layerNode) Read(b []byte) (int, error) {
	n := copy(b, l.delivered[l.at:])
	l.at += n
	l.reclaim()
	l.roomMade()
	if n == 0 && l.ended.Load() && l.held() == 0 {
		return 0, io.EOF
	}
	return n, nil
}

// OutboundBuffered is what waits below the layer above: what this layer
// holds, if it holds any, and what the layers below and the socket queue.
func (l *layerNode) OutboundBuffered() int {
	// What this layer holds is read before what the socket does: a layer
	// moves bytes down with Down().Write and then drops its hold, so the
	// reverse order can read the socket before the move and the hold after
	// it, and miss the bytes in flight between the two reads. Counting a
	// byte twice is safe for admission and completion checks; a hole is
	// not. The recursion into lower reads its own hold first the same way.
	held := 0
	if l.ops.held != nil && !l.stack.closedStack() {
		held = l.ops.held(layerCtx{l})
	}
	return held + l.lower.OutboundBuffered()
}

// Flush schedules buffered transport output for writing.
func (l *layerNode) Flush() error {
	if l.stack.over() {
		return net.ErrClosed
	}
	return l.lower.Flush()
}

func (l *layerNode) WriteTo(w io.Writer) (int64, error) {
	n, err := w.Write(l.delivered[l.at:])
	l.at += n
	l.reclaim()
	l.roomMade()
	return int64(n), err
}

// roomMade runs another turn after the layer above read, when the layer
// stalled short of what it could deliver or the stack stopped pulling at its
// inbound limit.
func (l *layerNode) roomMade() {
	// Loads first: every read runs this, and a compare-and-swap costs a
	// locked instruction even when it fails.
	if l.stalled.Load() && l.stalled.CompareAndSwap(true, false) {
		_ = l.stack.socket.Wake()
	}
	if l.paced.Load() && l.paced.Swap(false) {
		// The layer feeding this one was paused on a full delivery; the read
		// that just ran makes room, so a turn calls it again.
		_ = l.stack.socket.Wake()
	}
	if l.stack.inHeld.Load() {
		l.stack.inboundRoom()
	}
}

// Write enters the layer, once it opened the layers above; the Handler's
// writes are admitted against MaxOutboundBuffered first.
func (l *layerNode) Write(p []byte) (int, error) {
	if l.isTop() && l.stack.blockingWrite {
		if l.hasWaitingBlockWriter() && !blockio.OnTurn() {
			return l.ensureBlockWriter().Write(p)
		}
		n, err := l.writeNonBlocking(p)
		if errors.Is(err, ErrOutboundOverflow) && !blockio.OnTurn() {
			n2, err2 := l.ensureBlockWriter().Write(p[n:])
			return n + n2, err2
		}
		return n, err
	}
	return l.writeNonBlocking(p)
}

func (l *layerNode) writeNonBlocking(p []byte) (int, error) {
	if err := l.enter(len(p)); err != nil {
		return 0, err
	}
	n, err := l.conn.OnWrite(layerCtx{l}, p)
	if err != nil && l.isTop() {
		l.stack.noteRefused(err)
	}
	l.leave(len(p))
	if err == nil && n < len(p) {
		// A layer took part of the write: io.Writer reports that as a short
		// write, never as a complete one.
		err = io.ErrShortWrite
	}
	return n, err
}

func (l *layerNode) Writev(vec [][]byte) (int, error) {
	if l.isTop() && l.stack.blockingWrite && !blockio.OnTurn() {
		return l.ensureBlockWriter().Writev(vec)
	}
	return l.writevNonBlocking(vec)
}

func (l *layerNode) writevNonBlocking(vec [][]byte) (int, error) {
	size := 0
	for _, segment := range vec {
		size += len(segment)
	}
	if err := l.enter(size); err != nil {
		return 0, err
	}
	var n int
	var err error
	if len(vec) == 1 {
		n, err = l.conn.OnWrite(layerCtx{l}, vec[0])
	} else if _, transparent := l.conn.(passThroughHandler); transparent && len(vec) > 1 &&
		(l.index > 0 || !l.stack.packetBottom) {
		// Only the built-in transparent handler can skip OnWrite: a user
		// layer may embed PassThrough and override its write transform.
		// A packet bottom must join: socket Writev sends separate datagrams.
		n, err = l.lower.Writev(vec)
	} else {
		// One write whole, as the socket's Writev is: segments passed one by
		// one would interleave with another writer's.
		joined := AcquireBuffer(size)
		for _, segment := range vec {
			_, _ = joined.Write(segment)
		}
		n, err = l.conn.OnWrite(layerCtx{l}, joined.Bytes())
		ReleaseBuffer(joined)
	}
	if err != nil && l.isTop() {
		l.stack.noteRefused(err)
	}
	l.leave(size)
	if err == nil && n < size {
		err = io.ErrShortWrite
	}
	return n, err
}

// enter checks that a write may enter the layer — it opened the layers above
// and is not closing — and, at the top of the stack, reserves its room; leave
// returns the room once the write is through.
//
// The write is counted at every layer, not only the top: a layer writing
// below from its OnRead passes through the layer below it, and a CloseWrite
// reaching that layer must not pass under a write still in flight — the top's
// count covers the Handler's writes only.
func (l *layerNode) enter(n int) error {
	if l.refuses() {
		return net.ErrClosed // refused before it counts: it asks for no turn
	}
	// Counted, then checked again, so a CloseWrite that has not seen this
	// write leaves its end to the write's leave.
	l.writers.Add(1)
	if l.refuses() {
		l.exit()
		return net.ErrClosed
	}
	if l.isTop() {
		if err := l.stack.reserve(n); err != nil {
			l.stack.noteRefused(err)
			l.exit()
			return err
		}
	}
	return nil
}

// refuses reports a layer writes no longer reach: not open above yet, its
// writing half ended, or the stack closing — where only the goodbyes below
// the close's initiator still write down. The stack reads one state word, so
// a write racing the close is refused at once, not when the walk reaches its
// layer.
func (l *layerNode) refuses() bool {
	// One load: either the layer opened the layers above it and its writing
	// half is open, or the write is refused.
	return l.gate.Load() != gateOpenUp || !l.stack.mayWriteDown(l.index)
}

func (l *layerNode) leave(n int) {
	if l.isTop() {
		l.stack.release(n)
		l.stack.noteHeldPeak()
	}
	l.exit()
}

// exit ends a write into the layer; the last one out asks a turn to pass on
// an end that waited for it.
func (l *layerNode) exit() {
	if l.writers.Add(-1) == 0 && l.shutPending.Load() {
		l.stack.requestTurn()
	}
}

func (l *layerNode) isTop() bool { return l.index == len(l.stack.layers)-1 }

// WriteOwned keeps the Conn contract: a write refused whole with
// ErrOutboundOverflow leaves the buffer the caller's; any other outcome
// consumes it, and a partial one reports io.ErrShortWrite.
func (l *layerNode) WriteOwned(buffer *Buffer) (int, error) {
	if buffer != nil && l.isTop() && l.stack.blockingWrite && !blockio.OnTurn() {
		return l.ensureBlockWriter().WriteOwned(buffer)
	}
	return l.writeOwnedNonBlocking(buffer)
}

func (l *layerNode) writeOwnedNonBlocking(buffer *Buffer) (int, error) {
	if buffer == nil {
		return 0, nil
	}
	n, err := l.writeNonBlocking(buffer.Bytes())
	if err == nil {
		n = buffer.Len()
	} else if errors.Is(err, ErrOutboundOverflow) {
		if n == 0 {
			return 0, err
		}
		err = io.ErrShortWrite
	}
	ReleaseBuffer(buffer)
	return n, err
}
