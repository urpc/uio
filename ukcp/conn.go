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

package ukcp

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/urpc/uio"
	"github.com/urpc/uio/ukcp/internal/kcp"
)

// conn is one KCP session: the engine, and the transport adapter between it
// and the connection below — datagrams one per turn for a packet connection,
// self-framed segments for a stream one.
//
// Everything that touches the engine runs under mu: OnRead and OnTimer on
// the connection's turns, OnWrite on a writer's goroutine. Writes below
// happen under it too — the framework lets a layer hold its lock while it
// writes below, and nothing calls back in meanwhile — so what leaves keeps
// the engine's order.
type conn struct {
	layer *kcpLayer
	ctx   uio.LayerContext
	opts  Options

	mu     sync.Mutex
	kcp    *kcp.KCP
	conv   uint32
	packet bool // the connection below carries datagrams
	mss    int  // the largest payload one segment can carry

	// pending holds what a stream lower delivered beyond the whole segments
	// consumed: half a header, part of a payload. A packet lower leaves
	// nothing here — one turn carries one datagram, consumed whole.
	pending []byte
	// recv is the scratch the engine reads in-order bytes into.
	recv []byte
	// out and outVec stage one engine flush's output: every segment the
	// engine emits is copied into out and recorded as a slice, so the flush
	// leaves below in one Writev — one sendmmsg on a packet connection.
	out    []byte
	outVec [][]byte

	finQueued  bool // the end of this stream is queued in the engine
	peerFin    bool // the peer's end arrived and was delivered up
	lowerEnded bool // the stream below ended; its end goes up once drained
	ended      bool // no further output or delivery, or the close ran
	closed     bool // OnClose ran

	// debugNotes records the session's lifecycle for tests: what arrived,
	// what ended it and in which order. Every write is under mu.
	debugNotes    []string
	debugSawInput bool
	debugSawData  bool
	writeErr      error // the first write below that failed

	lastInbound time.Time // the last valid segment heard
	lastProbe   time.Time // the last keepalive probe sent
	armedAt     time.Time // the timer this session armed, zero once it fired

	// held is what the session took from its writer and has not yet handed
	// to the socket: the send queue plus what a flush staged but did not
	// write, in bytes. It is published for OutboundHeld, which runs off the
	// connection's turns.
	held atomic.Int64
	// state is the session's last published state. State() serves it without
	// the engine's lock: an OnBytesWritten observer runs inside a write below
	// the layer, under mu, and may legitimately read the state there.
	state atomic.Pointer[SessionState]
}

var (
	_ uio.LayerHandler   = (*conn)(nil)
	_ uio.CloseWriter    = (*conn)(nil)
	_ uio.GracefulCloser = (*conn)(nil)
	_ uio.OutboundHolder = (*conn)(nil)
	_ interface {
		State() SessionState
	} = (*conn)(nil)
)

// SessionState reports where one session stands; a connection's layer state
// is read through uio.FindLayer:
//
//	state, ok := uio.FindLayer[interface{ State() ukcp.SessionState }](conn)
type SessionState struct {
	Conv      uint32        // the conversation id the session runs with
	SRTT      time.Duration // the smoothed round-trip time
	RTO       time.Duration // the retransmission timeout in force
	CWnd      uint32        // the congestion window, in segments
	RemoteWnd uint32        // the window the peer last advertised, in segments
	Queued    int           // segments taken from the writer, not yet sent
	Inflight  int           // segments sent, not yet acknowledged
}

// packetDown reports whether the connection directly below carries datagrams:
// only the socket of a packet connection does. What a layer builds above
// another one is a byte stream, whatever the socket beneath it carries, so
// the address's network — which passes through a stack — says nothing about
// the direct lower; the framework answers for it: PacketSource.
func packetDown(down uio.LayerDown) bool {
	source, ok := down.(interface{ PacketSource() bool })
	return ok && source.PacketSource()
}

// OnOpen starts the session: a client picks a conversation id and opens the
// layers above at once; a server waits for the peer's first segment to name
// it, so the layers above open from OnRead.
func (c *conn) OnOpen(uio.LayerContext) {
	if err := c.layer.err; err != nil {
		c.closed = true
		_ = c.ctx.Abort(err)
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.packet = packetDown(c.ctx.Down())
	if c.packet {
		// A datagram the socket's read buffer cannot hold is truncated below
		// this layer: refuse the session rather than lose segments quietly.
		if source, ok := c.ctx.Down().(interface{ SocketReadSize() int }); ok {
			if size := source.SocketReadSize(); size > 0 && c.opts.MTU > size {
				c.ended = true
				_ = c.ctx.Abort(fmt.Errorf("%w: MTU %d does not fit the Events' read buffer of %d bytes",
					ErrInvalidOptions, c.opts.MTU, size))
				return
			}
		}
	}
	if !c.layer.client {
		// KCP has no handshake, but a peer that never speaks must not hold
		// the connection for ever: the first valid segment has a deadline.
		if timeout := c.opts.HandshakeTimeout; timeout > 0 {
			c.armFor(time.Now().Add(timeout))
		}
		return
	}
	var seed [4]byte
	if _, err := rand.Read(seed[:]); err != nil {
		c.closed = true
		_ = c.ctx.Abort(err)
		return
	}
	c.establish(binary.LittleEndian.Uint32(seed[:]))
	c.ctx.Up().Open()
}

// establish builds the engine a conversation runs on.
func (c *conn) establish(conv uint32) {
	k := kcp.NewKCP(conv, c.output)
	k.SetStreamMode(true)
	k.NoDelay(boolInt(c.opts.NoDelay), int(c.opts.Interval/time.Millisecond), c.opts.Resend, boolInt(!c.opts.CongestionControl))
	k.WndSize(c.opts.SendWindow, c.opts.RecvWindow)
	_ = k.SetMtu(c.opts.MTU)
	k.SetDeadLink(c.opts.DeadLink)
	c.kcp, c.conv = k, conv
	c.mss = c.opts.MTU - kcpOverhead
	// The scratch holds the largest segment the peer may send, which may be
	// bigger than ours: Recv refuses (-2) a segment that does not fit.
	c.recv = make([]byte, maxSegmentPayload)
	c.lastInbound = time.Now()
	c.debugNote("open")
	// The state is readable from OnOpen on: publish it with the session.
	c.publishState()
	c.arm(0)
}

// output stages one encoded segment, or a batch of ACKs, for the Writev that
// ends the flush. It runs inside the engine, under mu; the engine reuses its
// own buffer between output calls, so the bytes are copied here once.
func (c *conn) output(buf []byte, size int) {
	if c.ended || c.closed {
		return
	}
	start := len(c.out)
	c.out = append(c.out, buf[:size]...)
	c.outVec = append(c.outVec, c.out[start:len(c.out)])
}

// ackHoldDelay is how long a read turn's small flush waits for company. A
// window at its bound is acknowledged segment by segment, so the engine emits
// one to three segments per arriving ACK; sending each at once costs one
// sendmmsg per segment. The hold gathers them — ACKs, window updates, and the
// trickle the acknowledgements released — into one Writev, and an answer
// travelling back usually carries it at no cost. App writes never wait: they
// leave from OnWrite.
const ackHoldDelay = time.Millisecond

// holdBatch is the most segments a read turn holds back for the delay; a
// larger flush has company enough and leaves at once.
const holdBatch = 16

// sendBatch hands the staged segments to the connection below in one Writev.
// With hold set, a small staging — ACKs, window updates, the trickle a
// window's bound released — waits a moment for the traffic that usually
// follows; the timer bounds the wait. It requires mu.
func (c *conn) sendBatch(hold bool) {
	if len(c.outVec) == 0 {
		return
	}
	if hold && len(c.outVec) <= holdBatch {
		c.armFor(time.Now().Add(ackHoldDelay))
		return
	}
	_, err := c.ctx.Down().Writev(c.outVec)
	c.releaseStaging()
	if err != nil && !(c.packet && !errors.Is(err, net.ErrClosed) && !errors.Is(err, io.ErrClosedPipe)) {
		c.fatal(err)
	}
}

// releaseStaging drops what the write below took. The vector's backing array
// holds slices of the staging buffer, so it is cleared before it is shortened:
// references left in it would keep a burst-sized buffer alive with an idle
// session. A burst-sized staging is not worth keeping per session at all.
func (c *conn) releaseStaging() {
	clear(c.outVec)
	c.out, c.outVec = c.out[:0], c.outVec[:0]
	if cap(c.out) > 64<<10 {
		c.out, c.outVec = nil, nil
	}
}

// armFor aims the session's timer at deadline unless one is already aimed
// sooner. It requires mu.
func (c *conn) armFor(deadline time.Time) {
	if !c.armedAt.IsZero() && !deadline.Before(c.armedAt) {
		return
	}
	if err := c.ctx.SetTimer(deadline); err == nil {
		c.armedAt = deadline
	}
}

// fatal ends the session from under the engine's or a writer's call.
func (c *conn) fatal(err error) {
	c.debugNote("fatal: " + err.Error())
	if c.writeErr == nil {
		c.writeErr = err
	}
	c.ended = true
	c.held.Store(0)
	c.state.Store(nil)   // the session is ending: no live state to read
	_ = c.ctx.Abort(err) // on a turn, never inside the callback that asked
}

// OnRead is a turn: what arrived goes into the engine — one datagram on a
// packet connection, the whole segments a stream connection completed — then
// in-order bytes go up, and what the engine has to answer leaves at once.
func (c *conn) OnRead(uio.LayerContext) error {
	lower := c.ctx.Down()
	chunk := lower.PeekChunk()
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || c.ended {
		return nil
	}
	fed := false
	switch {
	case c.packet:
		if len(chunk) > 0 {
			fed = c.inputPacket(chunk)
			_, _ = lower.Discard(len(chunk))
		}
	case len(chunk) > 0:
		c.pending = append(c.pending, chunk...)
		_, _ = lower.Discard(len(chunk))
		fed = c.inputStream()
	}
	if fed {
		c.lastInbound = now
	}
	if c.kcp == nil || c.ended {
		return nil
	}
	// Deliver first — draining the receive queue is what owes the peer a
	// window update — then answer. What a round produced is held a moment,
	// so the answer travelling back carries it; data and retransmissions
	// leave from the writes and the timer.
	c.deliver()
	if c.kcp.Queued() > 0 {
		// The peer's window reopened while data waited in the send queue: a
		// full flush moves it into the window and sends it now, so no byte
		// counts as handed down before the socket sees it.
		next := c.kcp.Flush()
		c.sendBatch(true)
		c.noteHeld()
		if c.kcp.IsDead() {
			c.fatal(ErrDeadLink)
			return nil
		}
		c.arm(next)
	} else {
		c.kcp.FlushAckOnly()
		c.sendBatch(true)
		c.noteHeld()
		c.arm(c.opts.Interval)
	}
	if !c.packet {
		c.streamLowerEnded(lower)
	}
	return nil
}

// OnTimer is the session's clock: retransmissions, window probes, delayed
// ACKs and the keepalive, then whatever waits is delivered.
func (c *conn) OnTimer(uio.LayerContext) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.armedAt = time.Time{}
	if c.closed || c.ended {
		return nil
	}
	if c.kcp == nil {
		// The deadline for the peer's first segment came due and none
		// arrived.
		c.fatal(ErrPeerSilent)
		return nil
	}
	c.maintain()
	c.deliver()
	c.keepAlive(time.Now())
	return nil
}

// maintain flushes the engine — retransmissions, probes, delayed ACKs, new
// segments — sends what it staged below in one Writev, publishes what it
// still holds and re-arms the timer. Only turns deliver; a writer's flush
// stops here. It requires mu.
func (c *conn) maintain() {
	next := c.kcp.Flush()
	c.sendBatch(false)
	c.noteHeld()
	if c.kcp.IsDead() {
		c.fatal(ErrDeadLink)
		return
	}
	c.arm(next)
}

// segmentLength reports the payload length a segment's header claims. The
// comparison is on the wire's uint32, before any conversion to int: on a
// 32-bit platform int(0x80000000) goes negative and would slip past an
// int-side check into a slice bound panic. A header shorter than a segment,
// or one claiming more payload than the engine's buffers hold, reports false.
func segmentLength(seg []byte) (int, bool) {
	if len(seg) < kcpOverhead {
		return 0, false
	}
	length := binary.LittleEndian.Uint32(seg[20:24])
	if length > maxSegmentPayload {
		return 0, false
	}
	return int(length), true
}

// inputPacket feeds one datagram's segments to the engine. A datagram holds
// whole segments only; one that does not is dropped whole. It requires mu.
func (c *conn) inputPacket(packet []byte) bool {
	fed := false
	defer func() {
		if fed && !c.debugSawInput {
			c.debugSawInput = true
			c.debugNote("first input")
		}
	}()
	for rest := packet; len(rest) > 0; {
		length, ok := segmentLength(rest)
		if !ok {
			return fed
		}
		total := kcpOverhead + length
		if len(rest) < total {
			return fed
		}
		if c.feed(rest[:total]) {
			fed = true
		}
		rest = rest[total:]
	}
	return fed
}

// inputStream feeds the engine the whole segments the stream below completed,
// keeping a partial one for later. It requires mu.
func (c *conn) inputStream() bool {
	fed := false
	defer func() {
		if fed && !c.debugSawInput {
			c.debugSawInput = true
			c.debugNote("first input")
		}
	}()
	consumed := 0
	for {
		rest := c.pending[consumed:]
		if len(rest) < kcpOverhead {
			break
		}
		length, ok := segmentLength(rest)
		if !ok {
			c.fatal(errSegmentTooLarge)
			break
		}
		total := kcpOverhead + length
		if len(rest) < total {
			break
		}
		if c.feed(rest[:total]) {
			fed = true
		}
		consumed += total
	}
	if consumed > 0 {
		c.pending = append(c.pending[:0], c.pending[consumed:]...)
	}
	return fed
}

// feed hands one whole segment to the engine, building it first if this is
// the server's first sight of the conversation. It requires mu.
func (c *conn) feed(seg []byte) bool {
	if seg[5] != 0 {
		// The stream dialect frames with frg=0. A segment in message mode
		// would take a sequence number this end cannot deliver — a hole
		// every later segment waits behind — so it ends the session instead
		// of being dropped. Before a session exists the segment is the one
		// that would found it, so it is this connection's to judge too; once
		// one does, only its own conversation is.
		if c.kcp == nil || binary.LittleEndian.Uint32(seg[0:4]) == c.conv {
			c.fatal(fmt.Errorf("%w: a segment in message mode (frg=%d)", ErrProtocol, seg[5]))
		}
		return false
	}
	if c.kcp == nil {
		// Only a segment that names a command can found a session: noise on
		// the port must not lock the peer's conversation id to garbage.
		if c.layer.client || !knownCommand(seg[4]) {
			return false
		}
		c.establish(binary.LittleEndian.Uint32(seg[0:4]))
		c.ctx.Up().Open()
	}
	return c.kcp.Input(seg, kcp.IKCP_PACKET_REGULAR, true) == 0
}

// deliver hands the engine's in-order bytes up, to the layer's own bound and
// no further; the peer's end of stream follows the last byte. It requires mu.
func (c *conn) deliver() {
	if c.ended || c.peerFin || c.kcp == nil {
		return
	}
	up := c.ctx.Up()
	limit := c.opts.RecvWindow * c.mss
	for {
		if up.Buffered() >= limit {
			// The layer above is behind: what waits keeps the receive window
			// closed, and the turn its read runs takes the rest.
			up.Yield()
			return
		}
		switch n := c.kcp.Recv(c.recv); {
		case n == -2:
			// A queued message the scratch cannot hold: the queue would never
			// drain behind it.
			c.fatal(errMessageTooLarge)
			return
		case n < 0:
			c.endLower()
			return
		case n == 0:
			// An empty push: the peer's end of the stream, behind every byte.
			c.peerFin = true
			c.debugNote("peer-fin")
			up.End()
			return
		default:
			if !c.debugSawData {
				c.debugSawData = true
				c.debugNote("first data")
			}
			_, _ = up.Write(c.recv[:n])
		}
	}
}

// streamLowerEnded notices a stream connection below that ended: at a segment
// boundary the peer's stream is over and the end travels up once the receive
// queue drained, inside one it is a truncation the connection aborts. The
// keepalive would otherwise wait its periods out. It requires mu, on a turn.
func (c *conn) streamLowerEnded(lower uio.LayerDown) {
	if c.ended || c.peerFin || c.closed || c.kcp == nil {
		return
	}
	if lower.State()&uio.StatePeerEnded == 0 {
		return
	}
	if len(c.pending) > 0 {
		// Half a segment arrived and the stream ended: the segments cannot be
		// trusted to be whole again.
		c.fatal(io.ErrUnexpectedEOF)
		return
	}
	// Bytes the peer sent before its end may still wait in the receive queue:
	// a layer above that is behind keeps it full, and the end must follow the
	// last byte, not overtake it. Deliver what can go and let the turns the
	// layer above's reads ask for finish the drain; endLower ends the stream
	// once nothing is left.
	c.lowerEnded = true
	c.debugNote("lower-end")
	c.deliver()
}

// endLower ends the layer above once the stream below ended and the engine
// holds nothing more to deliver. It requires mu.
func (c *conn) endLower() {
	if !c.lowerEnded || c.peerFin {
		return
	}
	c.peerFin = true
	c.debugNote("lower-eof")
	c.ctx.Up().End()
}

// OnWrite takes what the writer wrote into the engine and puts it on the
// wire at once. It runs on the writer's goroutine; the engine's lock keeps it
// from interleaving with the turns' flushes.
func (c *conn) OnWrite(_ uio.LayerContext, p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ended || c.closed || c.kcp == nil {
		return 0, net.ErrClosed
	}
	if c.kcp.WaitSnd() >= c.opts.SendWindow || c.held.Load()+int64(len(p)) > int64(c.opts.MaxBuffered) {
		return 0, uio.ErrOutboundOverflow
	}
	// The engine takes at most 255 segments per call; hand a large write over
	// in pieces it accepts.
	for rest := p; len(rest) > 0; {
		n := len(rest)
		if most := 255 * c.mss; n > most {
			n = most
		}
		if ret := c.kcp.Send(rest[:n]); ret != 0 {
			c.fatal(errors.New("ukcp: the engine refused a write it sized"))
			return 0, c.writeErr
		}
		rest = rest[n:]
	}
	c.maintain()
	if c.ended {
		// The flush that carried the write ended the session: the bytes sit in
		// a queue that will never be retransmitted, so the write did not take.
		return 0, c.writeErr
	}
	return len(p), nil
}

// CloseWrite queues the end of this stream behind what was written before it
// and puts it on the wire: an empty push, which the peer reads back as its
// end of the stream once every byte before it arrived. The session goes on
// receiving.
func (c *conn) CloseWrite(uio.LayerContext) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ended || c.closed || c.finQueued || c.kcp == nil {
		return nil
	}
	c.finQueued = true
	c.debugNote("closewrite")
	if ret := c.kcp.Send(nil); ret != 0 {
		return nil // nothing in the queue and stream mode: cannot happen
	}
	c.maintain()
	return nil
}

// CloseGracefully queues the end of the stream as the whole connection closes
// and sends it best-effort: the connection below closes with it, so the end
// retransmits no further.
func (c *conn) CloseGracefully(uio.LayerContext, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.ended && !c.closed && !c.finQueued && c.kcp != nil {
		c.finQueued = true
		c.debugNote("graceful close")
		if c.kcp.Send(nil) == 0 {
			c.kcp.Flush()
			c.sendBatch(false)
		}
	}
	c.ended = true
	c.held.Store(0)
	c.state.Store(nil)
}

// OnClose is the connection's end.
func (c *conn) OnClose(_ uio.LayerContext, cause error) {
	c.mu.Lock()
	c.debugNote("onclose: " + errorText(cause))
	c.closed, c.ended = true, true
	c.kcp = nil
	c.pending = nil
	c.out, c.outVec = nil, nil
	c.held.Store(0)
	c.state.Store(nil)
	c.mu.Unlock()
}

// OutboundHeld reports the bytes the session took from its writer and has not
// yet handed to the socket. It runs off the turns, so it reads the published
// count and takes no lock.
func (c *conn) OutboundHeld(uio.LayerContext) int { return int(c.held.Load()) }

// State reports the session; see SessionState. It reads the last published
// snapshot without taking the engine's lock, so an OnBytesWritten observer —
// which runs inside a write below the layer, under that lock — may call it.
func (c *conn) State() SessionState {
	if state := c.state.Load(); state != nil {
		return *state
	}
	return SessionState{}
}

// publishState snapshots the session for State; it requires mu.
func (c *conn) publishState() {
	if c.kcp == nil || c.ended {
		// No session, or one that is ending: there is no live state to read.
		c.state.Store(nil)
		return
	}
	srtt, rto := c.kcp.RTT()
	c.state.Store(&SessionState{
		Conv:      c.conv,
		SRTT:      srtt,
		RTO:       rto,
		CWnd:      c.kcp.CWnd(),
		RemoteWnd: c.kcp.RemoteWnd(),
		Queued:    c.kcp.Queued(),
		Inflight:  c.kcp.Inflight(),
	})
}

// noteHeld publishes what the session still holds for its writer — queued
// segments and the application bytes staged but not written — and refreshes
// the state snapshot. Control segments staged beside them (ACKs, window
// probes and updates) are not the writer's bytes and count nothing. A session
// that ended holds nothing, whichever path ended it. It requires mu.
func (c *conn) noteHeld() {
	if c.kcp == nil || c.ended {
		c.held.Store(0)
		c.state.Store(nil)
		return
	}
	// The queue is charged its segments' own bytes: a one-byte write holds
	// one byte, not a whole MTU of them, so a small MaxBuffered admits what
	// it says it does.
	held := int64(c.kcp.QueuedBytes())
	for _, datagram := range c.outVec {
		held += pushBytes(datagram)
	}
	c.held.Store(held)
	c.publishState()
}

// pushBytes counts the application bytes one staged datagram carries. A
// datagram may hold several segments — a window update ahead of the data it
// released, acknowledgements behind it — so every segment is read: only a
// PUSH carries the writer's bytes, and only its payload counts.
func pushBytes(datagram []byte) int64 {
	var n int64
	for rest := datagram; len(rest) >= kcpOverhead; {
		length, ok := segmentLength(rest)
		if !ok || len(rest) < kcpOverhead+length {
			break // an engine never stages half a segment; count nothing more
		}
		if rest[4] == kcp.IKCP_CMD_PUSH {
			n += int64(length)
		}
		rest = rest[kcpOverhead+length:]
	}
	return n
}

// arm aims the session's timer: at the engine's next due when something
// waits, at the keepalive when one runs. It takes no later time when a turn
// is already aimed sooner, and makes no turn at all for an idle session. It
// requires mu.
func (c *conn) arm(next time.Duration) {
	if c.closed || c.ended || c.kcp == nil {
		return
	}
	var deadline time.Time
	if c.kcp.WaitSnd() > 0 {
		if next <= 0 {
			next = time.Millisecond
		}
		deadline = time.Now().Add(next)
	}
	if ka := c.keepAliveDeadline(); !ka.IsZero() && (deadline.IsZero() || ka.Before(deadline)) {
		deadline = ka
	}
	if deadline.IsZero() {
		return
	}
	if !c.armedAt.IsZero() && !deadline.Before(c.armedAt) {
		return // the turn already aimed comes no later, and looks again
	}
	if err := c.ctx.SetTimer(deadline); err == nil {
		c.armedAt = deadline
	}
}

// keepAliveDeadline is when the keepalive next needs a turn: a period after
// what was last heard, or after the last probe when the peer stayed silent.
// It requires mu.
func (c *conn) keepAliveDeadline() time.Time {
	if c.opts.KeepAlive <= 0 || c.kcp == nil {
		return time.Time{}
	}
	base := c.lastInbound
	if c.lastProbe.After(base) {
		base = c.lastProbe
	}
	return base.Add(c.opts.KeepAlive)
}

// keepAlive probes a peer that has gone silent and ends the session when it
// stays silent for three periods. It requires mu.
func (c *conn) keepAlive(now time.Time) {
	period := c.opts.KeepAlive
	if period <= 0 || c.kcp == nil || c.ended {
		return
	}
	if now.Sub(c.lastInbound) >= 3*period {
		c.fatal(ErrKeepAliveTimeout)
		return
	}
	if now.Sub(c.lastInbound) >= period && now.Sub(c.lastProbe) >= period {
		c.lastProbe = now
		c.probe()
	}
}

// probe asks the peer for its window: the engine's own WASK segment, carrying
// this end's window and position, which a live peer answers with its window —
// the keepalive's sign of life — and which never disturbs data flow control.
// It requires mu.
func (c *conn) probe() {
	c.kcp.AskWindow()
	c.kcp.FlushAckOnly()
	c.sendBatch(false)
}

// knownCommand reports a segment command the engine knows: only such a
// segment may found a server's session.
func knownCommand(cmd byte) bool {
	switch cmd {
	case kcp.IKCP_CMD_PUSH, kcp.IKCP_CMD_ACK, kcp.IKCP_CMD_WASK, kcp.IKCP_CMD_WINS:
		return true
	}
	return false
}

// boolInt maps a flag onto the engine's 0/1 argument.
func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// debugNote records one lifecycle event for the tests; it requires mu.
func (c *conn) debugNote(event string) {
	if len(c.debugNotes) < 24 {
		c.debugNotes = append(c.debugNotes, event)
	}
}

// debugLifecycle reports the session's lifecycle events, for a failing test
// to say what happened where none of it should have.
func (c *conn) debugLifecycle() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return strings.Join(c.debugNotes, " | ")
}

// errorText is an error's message, or the empty outcome for nil.
func errorText(err error) string {
	if err == nil {
		return "<nil>"
	}
	return err.Error()
}
