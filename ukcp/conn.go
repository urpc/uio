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
	// consumed: half a header, part of a payload.
	pending []byte

	// out and outEnds stage encoded segments, coalescing adjacent control/data
	// pieces into MTU-sized datagrams before sending in one Writev.
	out     []byte
	outEnds []int
	outVec  [][]byte

	needFullFlush bool
	turnDirty     bool

	finQueued  bool // the end of this stream is queued in the engine
	peerFin    bool // the peer's end arrived and was delivered up
	lowerEnded bool // the stream below ended; its end goes up once drained
	ended      bool // no further output or delivery, or the close ran
	closed     bool // OnClose ran

	debugNotes    []string
	debugSawInput bool
	debugSawData  bool
	writeErr      error // the first write below that failed

	lastInbound time.Time // the last valid segment heard
	lastProbe   time.Time // the last keepalive probe sent
	armedAt     time.Time // the timer this session armed, zero once it fired

	held atomic.Int64

	// Lock-free, zero-allocation published SessionState fields.
	stateLive  atomic.Bool
	stateConv  atomic.Uint32
	stateRTT   atomic.Uint64 // low 32: srtt ms, high 32: rto ms
	stateWnd   atomic.Uint64 // low 32: cwnd, high 32: rmt_wnd
	stateQueue atomic.Uint64 // low 32: queued, high 32: inflight
}

var (
	_ uio.LayerHandler   = (*conn)(nil)
	_ uio.CloseWriter    = (*conn)(nil)
	_ uio.LayerCloser    = (*conn)(nil)
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

func packetLower(lower uio.Lower) bool {
	source, ok := lower.(interface{ PacketSource() bool })
	return ok && source.PacketSource()
}

func morePacketsInBatch(lower uio.Lower) bool {
	source, ok := lower.(interface{ MorePacketsInBatch() bool })
	return ok && source.MorePacketsInBatch()
}

func deferTurnFlush(ctx uio.LayerContext) bool {
	d, ok := ctx.(interface{ DeferTurnFlush() bool })
	return ok && d.DeferTurnFlush()
}

func (c *conn) OnOpen(uio.LayerContext) {
	if err := c.layer.err; err != nil {
		c.closed = true
		_ = c.ctx.Abort(err)
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.packet = packetLower(c.ctx.Lower())
	if c.packet {
		if source, ok := c.ctx.Lower().(interface{ SocketReadSize() int }); ok {
			if size := source.SocketReadSize(); size > 0 && c.opts.MTU > size {
				c.ended = true
				_ = c.ctx.Abort(fmt.Errorf("%w: MTU %d does not fit the Events' read buffer of %d bytes",
					ErrInvalidOptions, c.opts.MTU, size))
				return
			}
		}
	}
	if !c.layer.client {
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
	c.ctx.Open()
}

func (c *conn) establish(conv uint32) {
	k := kcp.NewKCP(conv, c.output)
	k.SetStreamMode(true)
	k.NoDelay(boolInt(c.opts.NoDelay), int(c.opts.Interval/time.Millisecond), c.opts.Resend, boolInt(!c.opts.CongestionControl))
	k.WndSize(c.opts.SendWindow, c.opts.RecvWindow)
	_ = k.SetMtu(c.opts.MTU)
	k.SetDeadLink(c.opts.DeadLink)
	c.kcp, c.conv = k, conv
	c.mss = c.opts.MTU - kcpOverhead
	c.lastInbound = time.Now()
	c.debugNote("open")
	c.stateConv.Store(conv)
	c.publishState()
	c.arm(0)
}

// output stages encoded segments and coalesces adjacent pieces that fit inside
// one MTU datagram so control frames (ACK, WINS) and small writes share a
// single packet instead of amplifying datagram and syscall counts.
func (c *conn) output(buf []byte, size int) {
	if c.ended || c.closed || size <= 0 {
		return
	}
	if n := len(c.outEnds); n > 0 {
		prevStart := 0
		if n > 1 {
			prevStart = c.outEnds[n-2]
		}
		if (c.outEnds[n-1]-prevStart)+size <= c.opts.MTU {
			c.out = append(c.out, buf[:size]...)
			c.outEnds[n-1] = len(c.out)
			return
		}
	}
	c.out = append(c.out, buf[:size]...)
	c.outEnds = append(c.outEnds, len(c.out))
}

func (c *conn) sendBatch() {
	n := len(c.outEnds)
	if n == 0 {
		return
	}
	if cap(c.outVec) < n {
		c.outVec = make([][]byte, n)
	} else {
		c.outVec = c.outVec[:n]
	}
	start := 0
	for i, end := range c.outEnds {
		c.outVec[i] = c.out[start:end]
		start = end
	}
	_, err := c.ctx.Lower().Writev(c.outVec)
	c.releaseStaging()
	if err != nil && !(c.packet && !errors.Is(err, net.ErrClosed) && !errors.Is(err, io.ErrClosedPipe)) {
		c.fatal(err)
	}
}

func (c *conn) releaseStaging() {
	clear(c.outVec)
	c.out, c.outEnds, c.outVec = c.out[:0], c.outEnds[:0], c.outVec[:0]
	maxKeep := 64 << 10
	if c.kcp != nil && c.kcp.WaitSnd() > 0 {
		maxKeep = 512 << 10
	}
	if cap(c.out) > maxKeep {
		c.out, c.outEnds, c.outVec = nil, nil, nil
	}
}

func (c *conn) armFor(deadline time.Time) {
	if !c.armedAt.IsZero() && !deadline.Before(c.armedAt) {
		return
	}
	if err := c.ctx.SetTimer(deadline); err == nil {
		c.armedAt = deadline
	}
}

func (c *conn) fatal(err error) {
	c.debugNote("fatal: " + err.Error())
	if c.writeErr == nil {
		c.writeErr = err
	}
	c.ended = true
	c.held.Store(0)
	c.stateLive.Store(false)
	_ = c.ctx.Abort(err)
}

func (c *conn) OnRead(ctx uio.LayerContext) error {
	lower := c.ctx.Lower()
	chunk := lower.PeekChunk()
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
		c.lastInbound = time.Now()
	}
	if c.kcp == nil || c.ended {
		return nil
	}
	c.deliver()
	if c.ended {
		return nil
	}
	if !c.packet {
		c.streamLowerEnded(lower)
		if c.ended {
			return nil
		}
	}
	if deferTurnFlush(ctx) {
		c.turnDirty = true
		return nil
	}
	c.flushReadTurnLocked()
	return nil
}

// OnTurnEnd runs after Handler.OnRead completes in stack.turn(): if more
// datagrams for this peer are already queued in the active recvmmsg batch, we
// keep accumulating; otherwise any remaining ACKs, window updates, or
// ACK-released data leave now in a single coalesced Writev.
func (c *conn) OnTurnEnd(uio.LayerContext) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.turnDirty || c.closed || c.ended || c.kcp == nil {
		return
	}
	if c.packet && morePacketsInBatch(c.ctx.Lower()) && c.kcp.PendingAckCount() < 128 && c.kcp.Queued() == 0 {
		return
	}
	c.turnDirty = false
	c.flushReadTurnLocked()
}

func (c *conn) flushReadTurnLocked() {
	if c.needFullFlush || c.kcp.Queued() > 0 {
		c.needFullFlush = false
		next := c.kcp.Flush()
		c.sendBatch()
		c.noteHeld()
		if c.kcp.IsDead() {
			c.fatal(ErrDeadLink)
			return
		}
		c.arm(next)
		return
	}
	if c.kcp.HasPendingAckOrProbe() || len(c.outEnds) > 0 {
		c.kcp.FlushAckOnly()
		c.sendBatch()
	}
	c.noteHeld()
	c.arm(c.opts.Interval)
}

func (c *conn) OnTimer(uio.LayerContext) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.armedAt = time.Time{}
	if c.closed || c.ended {
		return nil
	}
	if c.kcp == nil {
		c.fatal(ErrPeerSilent)
		return nil
	}
	c.maintain()
	c.deliver()
	c.keepAlive(time.Now())
	return nil
}

func (c *conn) maintain() {
	c.needFullFlush = false
	c.turnDirty = false
	next := c.kcp.Flush()
	c.sendBatch()
	c.noteHeld()
	if c.kcp.IsDead() {
		c.fatal(ErrDeadLink)
		return
	}
	c.arm(next)
}

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

func (c *conn) inputPacket(packet []byte) bool {
	// Fast validation pass: a datagram holds whole segments only; one that does
	// not is dropped whole (matching previous framing validation).
	for scan := packet; len(scan) > 0; {
		length, ok := segmentLength(scan)
		if !ok || len(scan) < kcpOverhead+length {
			return false
		}
		scan = scan[kcpOverhead+length:]
	}

	fed := false
	defer func() {
		if fed && !c.debugSawInput {
			c.debugSawInput = true
			c.debugNote("first input")
		}
	}()

	// Fast path when session is already established and every segment in the
	// datagram belongs to c.conv with frg == 0: feed the entire datagram in one
	// InputBatch call!
	if c.kcp != nil {
		allClean := true
		for scan := packet; len(scan) > 0; {
			length := int(binary.LittleEndian.Uint32(scan[20:24]))
			if scan[5] != 0 || binary.LittleEndian.Uint32(scan[0:4]) != c.conv || !knownCommand(scan[4]) {
				allClean = false
				break
			}
			scan = scan[kcpOverhead+length:]
		}
		if allClean {
			fastTrigger, ret := c.kcp.InputBatch(packet)
			if ret == 0 {
				if fastTrigger {
					c.needFullFlush = true
				}
				return true
			}
		}
	}

	for rest := packet; len(rest) > 0; {
		length := int(binary.LittleEndian.Uint32(rest[20:24]))
		total := kcpOverhead + length
		if c.feed(rest[:total]) {
			fed = true
		}
		if c.ended {
			return fed
		}
		rest = rest[total:]
	}
	return fed
}

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
		if c.ended {
			break
		}
	}
	if consumed > 0 {
		c.pending = append(c.pending[:0], c.pending[consumed:]...)
	}
	return fed
}

func (c *conn) feed(seg []byte) bool {
	if seg[5] != 0 {
		if c.kcp == nil || binary.LittleEndian.Uint32(seg[0:4]) == c.conv {
			c.fatal(fmt.Errorf("%w: a segment in message mode (frg=%d)", ErrProtocol, seg[5]))
		}
		return false
	}
	if c.kcp == nil {
		if c.layer.client || !knownCommand(seg[4]) {
			return false
		}
		c.establish(binary.LittleEndian.Uint32(seg[0:4]))
		c.ctx.Open()
	}
	fastTrigger, ret := c.kcp.InputBatch(seg)
	if ret == 0 {
		if fastTrigger {
			c.needFullFlush = true
		}
		return true
	}
	return false
}

func (c *conn) deliver() {
	if c.ended || c.peerFin || c.kcp == nil {
		return
	}
	up := c.ctx.Upper()
	limit := c.opts.RecvWindow * c.mss
	for {
		if up.Buffered() >= limit {
			up.Yield()
			return
		}
		payload, ok := c.kcp.PopStreamSegment()
		if !ok {
			c.endLower()
			return
		}
		if len(payload) == 0 {
			kcp.ReleasePayload(payload)
			c.peerFin = true
			c.debugNote("peer-fin")
			up.End()
			return
		}
		if len(payload) > maxSegmentPayload {
			kcp.ReleasePayload(payload)
			c.fatal(errMessageTooLarge)
			return
		}
		if !c.debugSawData {
			c.debugSawData = true
			c.debugNote("first data")
		}
		_, _ = up.Write(payload)
		kcp.ReleasePayload(payload)
	}
}

func (c *conn) streamLowerEnded(lower uio.Lower) {
	if c.ended || c.peerFin || c.closed || c.kcp == nil {
		return
	}
	if lower.State()&uio.StateReadEnded == 0 {
		return
	}
	if len(c.pending) > 0 {
		c.fatal(io.ErrUnexpectedEOF)
		return
	}
	c.lowerEnded = true
	c.debugNote("lower-end")
	c.deliver()
}

func (c *conn) endLower() {
	if !c.lowerEnded || c.peerFin {
		return
	}
	c.peerFin = true
	c.debugNote("lower-eof")
	c.ctx.Upper().End()
}

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
	if deferTurnFlush(c.ctx) && c.kcp.Queued() < 32 {
		c.turnDirty = true
		c.needFullFlush = true
		c.noteHeld()
		return len(p), nil
	}
	c.maintain()
	if c.ended {
		return 0, c.writeErr
	}
	return len(p), nil
}

func (c *conn) CloseWrite(uio.LayerContext) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ended || c.closed || c.finQueued || c.kcp == nil {
		return nil
	}
	c.finQueued = true
	c.debugNote("closewrite")
	if ret := c.kcp.Send(nil); ret != 0 {
		return nil
	}
	c.maintain()
	return nil
}

func (c *conn) OnClosing(uio.LayerContext, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.ended && !c.closed && !c.finQueued && c.kcp != nil {
		c.finQueued = true
		c.debugNote("graceful close")
		if c.kcp.Send(nil) == 0 {
			c.kcp.Flush()
			c.sendBatch()
		}
	}
	c.ended = true
	c.held.Store(0)
	c.stateLive.Store(false)
}

func (c *conn) OnClose(_ uio.LayerContext, cause error) {
	c.mu.Lock()
	c.debugNote("onclose: " + errorText(cause))
	c.closed, c.ended = true, true
	c.kcp = nil
	c.pending = nil
	c.out, c.outEnds, c.outVec = nil, nil, nil
	c.held.Store(0)
	c.stateLive.Store(false)
	c.mu.Unlock()
}

func (c *conn) OutboundHeld(uio.LayerContext) int { return int(c.held.Load()) }

func (c *conn) State() SessionState {
	if !c.stateLive.Load() {
		return SessionState{}
	}
	rtt := c.stateRTT.Load()
	wnd := c.stateWnd.Load()
	q := c.stateQueue.Load()
	return SessionState{
		Conv:      c.stateConv.Load(),
		SRTT:      time.Duration(int32(uint32(rtt))) * time.Millisecond,
		RTO:       time.Duration(uint32(rtt>>32)) * time.Millisecond,
		CWnd:      uint32(wnd),
		RemoteWnd: uint32(wnd >> 32),
		Queued:    int(uint32(q)),
		Inflight:  int(uint32(q >> 32)),
	}
}

func (c *conn) publishState() {
	if c.kcp == nil || c.ended {
		c.stateLive.Store(false)
		return
	}
	srtt, rto := c.kcp.RTT()
	srttMs := uint32(int32(srtt / time.Millisecond))
	rtoMs := uint32(rto / time.Millisecond)
	c.stateRTT.Store(uint64(srttMs) | uint64(rtoMs)<<32)
	c.stateWnd.Store(uint64(c.kcp.CWnd()) | uint64(c.kcp.RemoteWnd())<<32)
	c.stateQueue.Store(uint64(uint32(c.kcp.Queued())) | uint64(uint32(c.kcp.Inflight()))<<32)
	c.stateLive.Store(true)
}

func (c *conn) noteHeld() {
	if c.kcp == nil || c.ended {
		c.held.Store(0)
		c.stateLive.Store(false)
		return
	}
	held := int64(c.kcp.QueuedBytes())
	start := 0
	for _, end := range c.outEnds {
		held += pushBytes(c.out[start:end])
		start = end
	}
	c.held.Store(held)
	c.publishState()
}

func pushBytes(datagram []byte) int64 {
	var n int64
	for rest := datagram; len(rest) >= kcpOverhead; {
		length, ok := segmentLength(rest)
		if !ok || len(rest) < kcpOverhead+length {
			break
		}
		if rest[4] == kcp.IKCP_CMD_PUSH {
			n += int64(length)
		}
		rest = rest[kcpOverhead+length:]
	}
	return n
}

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
	if !c.armedAt.IsZero() && c.armedAt.Sub(deadline) < time.Millisecond {
		return
	}
	if err := c.ctx.SetTimer(deadline); err == nil {
		c.armedAt = deadline
	}
}

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

func (c *conn) probe() {
	c.kcp.AskWindow()
	c.kcp.FlushAckOnly()
	c.sendBatch()
}

func knownCommand(cmd byte) bool {
	switch cmd {
	case kcp.IKCP_CMD_PUSH, kcp.IKCP_CMD_ACK, kcp.IKCP_CMD_WASK, kcp.IKCP_CMD_WINS:
		return true
	}
	return false
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func (c *conn) debugNote(event string) {
	if len(c.debugNotes) < 24 {
		c.debugNotes = append(c.debugNotes, event)
	}
}

func (c *conn) debugLifecycle() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return strings.Join(c.debugNotes, " | ")
}

func errorText(err error) string {
	if err == nil {
		return "<nil>"
	}
	return err.Error()
}
