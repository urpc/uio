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
// it sees (LayerContext, LayerUp, the socket below), and the Conn surface the
// top of the stack hands the Handler — with the send-receipt bookkeeping the
// node's tracked writes feed.

package uio

import (
	"errors"
	"io"
	"net"
	"sync/atomic"

	"time"

	"github.com/urpc/uio/internal/readcontrol"
)

// layerOps is the set of optional interfaces a LayerHandler implements,
// resolved once when the stack attaches it: the close and flush paths read
// the table instead of asserting at every call, and a layer without a
// capability costs one nil check.
type layerOps struct {
	graceful    func(LayerContext, error)       // GracefulCloser
	closeWriter func(LayerContext) error        // CloseWriter
	flush       func(LayerContext) error        // Flusher
	held        func(LayerContext) int          // OutboundHolder
	wireSent    func(LayerContext, int)         // WireCounterObserver
	sent        func(LayerContext, SendReceipt) // ReceiptObserver
}

// opsOf resolves a LayerHandler's optional interfaces.
func opsOf(h LayerHandler) layerOps {
	var ops layerOps
	if graceful, ok := h.(GracefulCloser); ok {
		ops.graceful = graceful.CloseGracefully
	}
	if closeWriter, ok := h.(CloseWriter); ok {
		ops.closeWriter = closeWriter.CloseWrite
	}
	if flusher, ok := h.(Flusher); ok {
		ops.flush = flusher.Flush
	}
	if holder, ok := h.(OutboundHolder); ok {
		ops.held = holder.OutboundHeld
	}
	if observer, ok := h.(WireCounterObserver); ok {
		ops.wireSent = observer.OnWireSent
	}
	if observer, ok := h.(ReceiptObserver); ok {
		ops.sent = observer.OnSent
	}
	return ops
}

// layerNode is one layer of one connection: the state the layer works
// on, and the Conn the layer above reads and writes through.
type layerNode struct {
	inputReadControlAccess
	readControl atomic.Pointer[readcontrol.Control] // this node's delivered input consumer gate
	stack       *stack
	index       int
	lower       Conn
	conn        LayerHandler // the layer's instance on this connection
	up          LayerUp      // the face the layer writes up, cached to allocate nothing

	ops    layerOps
	opened atomic.Bool // the layer's OnOpen ran; set on a turn
	ended  bool        // the layer ended its stream up; only turns touch it
	// gate packs the two facts a write is admitted by: openUp, the layer
	// opened the layers above it, and writeShut, its writing half ended. One
	// load on the write path admits or refuses.
	gate    atomic.Uint32
	closing atomic.Bool // this layer's close walk began; see CloseWith
	// shutPending marks a CloseWrite end not yet passed below, shutFlushed
	// its flush made, and writers counts the writes in flight into the layer —
	// the Handler's through the top, a layer's own through every layer it
	// writes below — which the end waits for; see shutDown.
	shutPending atomic.Bool
	shutFlushed atomic.Bool
	writers     atomic.Int32
	stalled     atomic.Bool // the layer stopped short; see LayerUp.Yield
	paced       atomic.Bool // a full delivery kept the layer from running; the read above wakes it

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

// layerCtx is the LayerContext a layer holds: the connection below as it
// reads it, the layer above as it writes to it, and the close verbs that
// begin the stack's close from here.
type layerCtx struct{ l *layerNode }

var (
	_ LayerContext = layerCtx{}
	_ LayerUp      = layerUp{}
	_ Conn         = (*layerNode)(nil)
	_ WireCounter  = (*layerNode)(nil)
)

func (lc layerCtx) Down() LayerDown { return lc.l.lower }

// Up returns the node's cached face, so writing up allocates nothing.
func (lc layerCtx) Up() LayerUp { return lc.l.up }

func (lc layerCtx) SetTimer(when time.Time) error {
	return lc.l.stack.setTimer(lc.l.index, when, 0)
}

func (lc layerCtx) Close(cause error) error { return lc.l.endClose(cause, true, closeByLayer) }

func (lc layerCtx) Abort(cause error) error { return lc.l.endClose(cause, false, closeByLayer) }

func (lc layerCtx) Wake() error { return lc.l.stack.socket.Wake() }

// layerUp is the layer above's input, as the layer below writes to it.
type layerUp struct{ l *layerNode }

func (u layerUp) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	l := u.l
	l.compact()
	l.delivered = append(l.delivered, p...)
	return len(p), nil
}

func (u layerUp) End() { u.l.ended = true }

// Open records that the layers above may open: their OnOpen runs on this
// turn once this callback returned — never inside it — so this layer's lock,
// if it holds one, stays its own.
func (u layerUp) Open() {
	u.l.markOpenUp()
}

func (u layerUp) Reserve(n int) []byte {
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

func (u layerUp) Commit(n int) {
	l := u.l
	l.delivered = l.delivered[:len(l.delivered)+n]
}

func (u layerUp) Buffered() int { return len(u.l.delivered) - u.l.at }

func (u layerUp) Yield() { u.l.stalled.Store(true) }

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

// socketLower is the first layer's Lower: the socket, with the
// connection's userdata — the socket's own slot holds the stack — and the
// first layer's timer in place of the socket's single wake slot.
type socketLower struct {
	inputReadControlAccess
	Conn
	stack *stack
}

func (s socketLower) Userdata() any {
	if held := s.stack.userdata.Load(); held != nil {
		return *held
	}
	return nil
}

func (s socketLower) SetUserdata(value any) { s.stack.userdata.Store(&value) }

func (s socketLower) CloseWith(cause error) error { return s.stack.closeSocket(cause) }

// Close and CloseGracefully follow the same road as CloseWith, so the first
// layer cannot end the stack under itself: the close a layer asks for runs
// the stack's goodbyes and waits, not the socket's own one-shot flush.
func (s socketLower) Close() error { return s.stack.closeSocket(io.ErrUnexpectedEOF) }

func (s socketLower) CloseGracefully(cause error) error { return s.stack.closeSocket(cause) }

// The writes that hand the socket bytes count them; see WireCounter.

func (s socketLower) Write(p []byte) (int, error) {
	n, err := s.Conn.Write(p)
	s.stack.wireQueued.Add(uint64(n))
	return n, err
}

func (s socketLower) WriteString(p string) (int, error) {
	n, err := s.Conn.WriteString(p)
	s.stack.wireQueued.Add(uint64(n))
	return n, err
}

func (s socketLower) WriteByte(b byte) error {
	err := s.Conn.WriteByte(b)
	if err == nil {
		s.stack.wireQueued.Add(1)
	}
	return err
}

func (s socketLower) Writev(vec [][]byte) (int, error) {
	n, err := s.Conn.Writev(vec)
	s.stack.wireQueued.Add(uint64(n))
	return n, err
}

func (s socketLower) WriteOwned(buffer *Buffer) (int, error) {
	n, err := s.Conn.WriteOwned(buffer)
	s.stack.wireQueued.Add(uint64(n))
	return n, err
}

func (s socketLower) ReserveOutbound(n int) ([]byte, error) {
	room, err := s.Conn.ReserveOutbound(n)
	if err == nil {
		s.stack.wireQueued.Add(uint64(n))
	}
	return room, err
}

var _ TrackedWriter = socketLower{}

// WriteTracked writes p to the socket and takes its wire position; the first
// layer hears the receipt once it leaves. See TrackedWriter.
func (s socketLower) WriteTracked(p []byte) (int, SendReceipt, error) {
	n, err := s.Write(p)
	if err != nil || n == 0 {
		return n, SendReceipt{}, err
	}
	// Exact: nothing below the socket can transform these bytes, and a
	// holder above it holds other writes, not this one's position.
	r := SendReceipt{wire: s.stack.wireQueued.Load()}
	if r.wire == 0 || len(s.stack.layers) == 0 {
		return n, SendReceipt{}, nil
	}
	s.stack.addReceipt(pendingReceipt{conn: s, above: s.stack.layers[0], r: r})
	return n, r, nil
}

// PacketSource reports the socket's datagrams. Only the first layer's Down —
// the socket itself — can answer for them: what a layer builds above another
// one is a byte stream, whatever the socket beneath it carries. A layer that
// cares about the shape of its direct lower asserts this interface on Down:
//
//	if source, ok := ctx.Down().(interface{ PacketSource() bool }); ok && source.PacketSource() { … }
func (s socketLower) PacketSource() bool {
	addr := s.Conn.LocalAddr()
	return addr != nil && packetNetwork(addr.Network())
}

// SocketReadSize reports the Events' read buffer: the most bytes one socket
// read, or one datagram, carries. A layer whose units must fit inside a read
// — a segment, a record — checks it at OnOpen and refuses what cannot fit:
//
//	if size, ok := ctx.Down().(interface{ SocketReadSize() int }); ok { … }
func (s socketLower) SocketReadSize() int {
	if c, ok := s.Conn.(*fdConn); ok {
		return c.events().readBufferSize
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

func (l *layerNode) Userdata() any {
	if held := l.stack.userdata.Load(); held != nil {
		return *held
	}
	return nil
}

func (l *layerNode) SetUserdata(value any) { l.stack.userdata.Store(&value) }

func (l *layerNode) SetLinger(secs int) error           { return l.lower.SetLinger(secs) }
func (l *layerNode) SetNoDelay(noDelay bool) error      { return l.lower.SetNoDelay(noDelay) }
func (l *layerNode) SetKeepAlive(keepAlive bool) error  { return l.lower.SetKeepAlive(keepAlive) }
func (l *layerNode) SetKeepAlivePeriod(secs int) error  { return l.lower.SetKeepAlivePeriod(secs) }
func (l *layerNode) Control(fn func(fd uintptr)) error  { return l.lower.Control(fn) }
func (l *layerNode) SetReadBuffer(size int) error       { return l.lower.SetReadBuffer(size) }
func (l *layerNode) SetWriteBuffer(size int) error      { return l.lower.SetWriteBuffer(size) }
func (l *layerNode) SetDeadline(t time.Time) error      { return l.lower.SetDeadline(t) }
func (l *layerNode) SetReadDeadline(t time.Time) error  { return l.lower.SetReadDeadline(t) }
func (l *layerNode) SetWriteDeadline(t time.Time) error { return l.lower.SetWriteDeadline(t) }

func (l *layerNode) Wake() error                          { return l.stack.socket.Wake() }
func (l *layerNode) YieldRead() error                     { return l.lower.YieldRead() }
func (l *layerNode) InboundBuffered() int                 { return l.held() }
func (l *layerNode) ReserveOutbound(int) ([]byte, error)  { return nil, ErrReserveUnsupported }
func (l *layerNode) Close() error                         { return l.CloseWith(io.ErrUnexpectedEOF) }
func (l *layerNode) held() int                            { return len(l.delivered) - l.at }
func (l *layerNode) slot() int                            { return l.index + 1 }
func (l *layerNode) Peek(b []byte) []byte                 { return b[:copy(b, l.delivered[l.at:])] }
func (l *layerNode) PeekChunk() []byte                    { return l.delivered[l.at:] }
func (l *layerNode) WriteByte(b byte) error               { _, err := l.Write([]byte{b}); return err }
func (l *layerNode) WriteString(s string) (int, error)    { return l.Write([]byte(s)) }
func (l *layerNode) SetWakeDeadline(when time.Time) error { return l.stack.setTimer(l.slot(), when, 0) }

// WireQueued is what the socket was handed, with what this layer and the
// ones below it hold — not the layers above, whose bytes have not reached
// this Conn. The layers' part is read first, from the layers themselves: a
// layer that hands bytes down between the two readings is counted twice,
// never missed.
func (l *layerNode) WireQueued() uint64 {
	held := 0
	if l.stack.holders && !l.stack.closedStack() {
		for _, below := range l.stack.layers[:l.index+1] {
			if below.ops.held != nil {
				held += below.ops.held(layerCtx{below})
			}
		}
	}
	return uint64(held) + l.stack.wireQueued.Load()
}

// WireSent is what the socket reported sent.
func (l *layerNode) WireSent() uint64 { return l.stack.wireSent.Load() }

// State reports where the connection stands; see ConnState.
func (l *layerNode) State() ConnState {
	var state ConnState
	if l.ended && l.held() == 0 {
		state |= StatePeerEnded
	}
	if l.gate.Load()&gateWriteShut != 0 {
		state |= StateWriteShut
	}
	switch l.stack.state.Load() & stackPhaseMask {
	case stackClosing, stackEnding:
		state |= StateClosing
	case stackClosed:
		state |= StateClosing | StateClosed
	}
	return state
}

// pendingReceipt is one tracked write the wire has not passed yet.
type pendingReceipt struct {
	conn  Conn       // the connection the write entered, for its queue
	above *layerNode // the layer above it, to hear; nil past the top
	r     SendReceipt
}

var _ TrackedWriter = (*layerNode)(nil)

// WriteTracked writes p into the layer and takes the position its output
// holds on the wire once the write returned; see TrackedWriter. The layer
// above this connection hears the receipt on a turn once it leaves.
func (l *layerNode) WriteTracked(p []byte) (int, SendReceipt, error) {
	n, err := l.Write(p)
	if err != nil || n == 0 {
		return n, SendReceipt{}, err
	}
	r := SendReceipt{wire: l.WireQueued()}
	if r.wire == 0 {
		return n, SendReceipt{}, nil
	}
	r.inexact = l.stack.firstHeld >= 0 && l.stack.firstHeld <= l.index
	c := l.stack
	var above *layerNode
	if l.index+1 < len(c.layers) {
		above = c.layers[l.index+1]
	}
	c.addReceipt(pendingReceipt{conn: l, above: above, r: r})
	return n, r, nil
}

// addReceipt queues a tracked write, for the turn that sees the wire pass it.
func (c *stack) addReceipt(pr pendingReceipt) {
	c.receiptCount.Add(1)
	c.receiptMu.Lock()
	c.receipts = append(c.receipts, pr)
	c.receiptMu.Unlock()
	c.requestTurn()
}

// retireReceipts reports the tracked writes the wire passed, and keeps the
// rest. It runs as a turn ends: a receipt may not fire from inside the write
// that moved the wire — the layer holding its lock must not be called back.
func (c *stack) retireReceipts() {
	if c.receiptCount.Load() == 0 {
		return
	}
	c.receiptMu.Lock()
	out := c.receipts
	c.receipts = nil
	c.receiptMu.Unlock()
	sent := c.wireSent.Load()
	var keep []pendingReceipt
	for _, pr := range out {
		if c.over() {
			// A receipt reported began the close: the rest wait for OnClose's
			// notice, as any receipt outstanding at a close does.
			pr.r = SendReceipt{}
		}
		if pr.r.wire == 0 {
			c.receiptCount.Add(-1)
			continue
		}
		if pr.r.done(sent) || pr.conn.OutboundBuffered() == 0 {
			c.receiptCount.Add(-1)
			switch {
			case pr.above != nil:
				if pr.above.ops.sent != nil {
					held := pr.above.held()
					pr.above.ops.sent(layerCtx{pr.above}, pr.r)
					if pr.above.held() != held {
						// A layer that wrote up in OnSent gets a turn to carry it,
						// as one does in OnWireSent.
						c.upPending.Store(true)
					}
				}
			case c.topSent != nil:
				// The Handler's own tracked write: its Conn is the top layer's.
				c.topSent(c.topConn(), pr.r)
			}
			continue
		}
		keep = append(keep, pr)
	}
	if len(keep) > 0 {
		c.receiptMu.Lock()
		c.receipts = append(keep, c.receipts...) // kept writes entered first
		c.receiptMu.Unlock()
	}
}

// dropReceipts forgets the tracked writes the connection ended under: the
// layers hear OnClose instead.
func (c *stack) dropReceipts() {
	if c.receiptCount.Load() == 0 {
		return
	}
	c.receiptMu.Lock()
	c.receipts = nil
	c.receiptMu.Unlock()
	c.receiptCount.Store(0)
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
	if n == 0 && l.ended && l.held() == 0 {
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

// Flush sends what this layer holds, if it holds any, then passes below.
func (l *layerNode) Flush() error {
	if l.stack.over() {
		return net.ErrClosed
	}
	var err error
	if l.ops.flush != nil {
		err = l.ops.flush(layerCtx{l})
	}
	if err == nil && !l.stack.over() {
		err = l.lower.Flush()
	}
	// What the flush sent may be what an end waits for: a turn looks, since
	// this flush may run inside a layer above's.
	if c := l.stack; c.shutsPending.Load() > 0 {
		c.requestTurn()
	}
	return err
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
	l.stack.moved++
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
	n, err := l.Write(buffer.Bytes())
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
