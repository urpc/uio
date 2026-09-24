package uws

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/urpc/uio"
	"github.com/urpc/uio/uws/internal/frame"
)

// serverFrameScratch keeps the maximum server header and its two writev slices
// off the heap. Client frames cannot use it because masking mutates payload.
type serverFrameScratch struct {
	header [14]byte
	vec    [2][]byte
}

var serverFrameScratchPool sync.Pool

func acquireServerFrameScratch() *serverFrameScratch {
	scratch, _ := serverFrameScratchPool.Get().(*serverFrameScratch)
	if scratch == nil {
		scratch = &serverFrameScratch{}
	}
	return scratch
}

func releaseServerFrameScratch(scratch *serverFrameScratch) {
	clear(scratch.vec[:])
	serverFrameScratchPool.Put(scratch)
}

func (state *connCloseProgress) drainIsRequested() bool {
	return state.flags.Load()&closeDrainRequested != 0
}

func (state *connCloseProgress) phase() uint32 {
	return (state.flags.Load() & transportPhaseMask) >> transportPhaseShift
}

// setPhaseLocked changes only the transport claim bits. All flag writes hold
// transitionMu; reads on the normal send path need just one atomic load.
func (state *connCloseProgress) setPhaseLocked(phase uint32) {
	flags := state.flags.Load()
	state.flags.Store((flags &^ transportPhaseMask) | phase<<transportPhaseShift)
}

// completeDrain preserves a Close frame queued while the previous frame was
// being written. The caller must keep draining while it returns false.
func (state *connCloseProgress) completeDrain() bool {
	state.transitionMu.Lock()
	defer state.transitionMu.Unlock()
	if state.pendingClose != nil {
		return false
	}
	state.flags.Store(state.flags.Load() &^ closeDrainRequested)
	return true
}

func (state *connCloseProgress) closeFrameWasSent() bool {
	return state.flags.Load()&closeFrameSent != 0
}

func (state *connCloseProgress) markCloseFrameSent() {
	state.transitionMu.Lock()
	state.flags.Store(state.flags.Load() | closeFrameSent)
	state.transitionMu.Unlock()
}

// queueClose copies a protocol Close frame when a streaming Writer owns mu.
// A second request can briefly queue while the first is being written, but
// sendFrameLocked emits only the first successful Close frame.
func (state *connCloseProgress) queueClose(payload []byte) bool {
	state.transitionMu.Lock()
	defer state.transitionMu.Unlock()
	flags := state.flags.Load()
	if flags&closeWriterAborted != 0 || state.phase() == transportCloseClaimed {
		return false
	}
	if flags&closeFrameSent != 0 || state.pendingClose != nil {
		return true
	}
	pending := &pendingCloseFrame{payload: make([]byte, len(payload))}
	copy(pending.payload, payload)
	state.pendingClose = pending
	state.flags.Store(flags | closeDrainRequested)
	return true
}

func (state *connCloseProgress) takeClose() []byte {
	state.transitionMu.Lock()
	pending := state.pendingClose
	state.pendingClose = nil
	state.transitionMu.Unlock()
	if pending == nil {
		return nil
	}
	return pending.payload
}

func (state *connCloseProgress) hasPendingClose() bool {
	state.transitionMu.Lock()
	pending := state.pendingClose != nil
	state.transitionMu.Unlock()
	return pending
}

func (state *connCloseProgress) abortDrain() {
	state.transitionMu.Lock()
	state.pendingClose = nil
	state.flags.Store(state.flags.Load() &^ closeDrainRequested)
	state.transitionMu.Unlock()
}

// writerFailed decides the handoff before releasing the long-lived write lock.
// A Close request that wins first is drained by unlockWrite; otherwise later
// protocol Close requests cannot overtake the failed partial message.
func (state *connCloseProgress) writerFailed() bool {
	state.transitionMu.Lock()
	defer state.transitionMu.Unlock()
	if state.pendingClose != nil {
		return true
	}
	state.flags.Store((state.flags.Load() | closeWriterAborted) &^ closeDrainRequested)
	return false
}

func (state *connCloseProgress) writerIsAborted() bool {
	return state.flags.Load()&closeWriterAborted != 0
}

func (state *connCloseProgress) claimAbort() bool {
	state.transitionMu.Lock()
	defer state.transitionMu.Unlock()
	if state.phase() == transportCloseClaimed {
		return false
	}
	state.pendingClose = nil
	state.flags.Store((state.flags.Load() | closeWriterAborted) &^ closeDrainRequested)
	state.setPhaseLocked(transportCloseClaimed)
	return true
}

func (state *connCloseProgress) requestTransportClose() bool {
	state.transitionMu.Lock()
	defer state.transitionMu.Unlock()
	if state.writerIsAborted() {
		return false
	}
	switch state.phase() {
	case transportCloseIdle:
		state.flags.Store(state.flags.Load() | closeDrainRequested)
		state.setPhaseLocked(transportClosePending)
		return true
	case transportClosePending:
		return true
	default:
		return false
	}
}

func (state *connCloseProgress) claimTransportClose() bool {
	if state.phase() != transportClosePending || state.pendingBytes.Load() != 0 {
		return false
	}
	state.transitionMu.Lock()
	defer state.transitionMu.Unlock()
	if state.phase() != transportClosePending || state.writerIsAborted() || state.drainIsRequested() || state.pendingBytes.Load() != 0 {
		return false
	}
	state.setPhaseLocked(transportCloseClaimed)
	return true
}

// SendText queues payload as one text message. The transport flushes accepted
// data in the connection's current or next I/O task.
func (c *Conn) SendText(payload []byte) error {
	return c.send(MessageType(TextMessage), payload)
}

// SendBinary queues payload as one binary message. The transport flushes
// accepted data in the connection's current or next I/O task.
func (c *Conn) SendBinary(payload []byte) error {
	return c.send(BinaryMessage, payload)
}

// Ping sends a ping control frame with up to 125 bytes of payload.
func (c *Conn) Ping(payload []byte) error {
	if len(payload) > 125 {
		return frame.ErrProtocol
	}
	return c.sendFrame(frame.Frame{Fin: true, Opcode: frame.Ping, Payload: payload})
}

// tryHeartbeatPing never waits for a streaming Writer. It records queue time
// now, while markPingSent starts the Pong timeout only after OnOutbound proves
// that the complete Ping frame left UIO's queue.
func (c *Conn) tryHeartbeatPing(now time.Time) (bool, error) {
	if c.heartbeat == nil || !c.opened.Load() || c.closed.Load() || c.closing.Load() {
		return false, ErrClosed
	}
	if !c.writes.mu.TryLock() {
		return false, nil
	}
	if err := c.completeWriteDrainLocked(); err != nil {
		c.unlockWrite()
		return true, err
	}
	if c.closed.Load() || c.closing.Load() {
		c.unlockWrite()
		return false, ErrClosed
	}
	heartbeat := c.heartbeat
	if heartbeat.pingOutstanding.Load() {
		c.unlockWrite()
		return false, nil
	}
	var payload [8]byte
	if _, err := rand.Read(payload[:]); err != nil {
		c.unlockWrite()
		return true, err
	}
	nonce := binary.BigEndian.Uint64(payload[:])
	if nonce == 0 {
		nonce = 1
		binary.BigEndian.PutUint64(payload[:], nonce)
	}
	if !heartbeat.beginPing(now, nonce) {
		c.unlockWrite()
		return false, nil
	}
	if !heartbeat.submitMu.TryLock() {
		heartbeat.cancelPing(nonce)
		c.unlockWrite()
		return false, nil
	}
	err := c.sendFrameSubmitted(frame.Frame{Fin: true, Opcode: frame.Ping, Payload: payload[:]})
	heartbeat.submitMu.Unlock()
	if err == nil {
		err = c.flush()
	}
	if err != nil {
		heartbeat.cancelPing(nonce)
	}
	c.unlockWrite()
	return true, err
}

// tryHeartbeatClose is the non-blocking timeout close path. false means a
// streaming Writer still owns the connection and a later scan must retry.
func (c *Conn) tryHeartbeatClose(code uint16, reason string) bool {
	if c.closed.Load() || c.closing.Load() {
		return true
	}
	if !c.writes.mu.TryLock() {
		return false
	}
	payload := make([]byte, 2+len(reason))
	payload[0] = byte(code >> 8)
	payload[1] = byte(code)
	copy(payload[2:], reason)
	startedClose := false
	if err := c.completeWriteDrainLocked(); err != nil {
		c.unlockWrite()
		return true
	}
	var err error
	if !c.closed.Load() && !c.closing.Load() {
		c.setCloseReason(code, reason)
		err = c.sendFrameLocked(frame.Frame{Fin: true, Opcode: frame.Close, Payload: payload})
		if err == nil {
			err = c.flush()
		}
		if err == nil {
			c.closing.Store(true)
			startedClose = true
		}
	}
	c.unlockWrite()
	if err != nil {
		c.setCloseError(err)
		c.closing.Store(true)
		c.abortTransport(err)
		return true
	}
	if startedClose {
		_ = c.closeTransport()
	}
	return true
}

// Close starts a graceful WebSocket close handshake.
func (c *Conn) Close(code uint16, reason string) error {
	if !c.opened.Load() {
		return c.raw.CloseWith(io.EOF)
	}
	if len(reason) > 123 || !utf8.ValidString(reason) {
		return frame.ErrInvalidUTF8
	}
	payload := make([]byte, 2+len(reason))
	payload[0] = byte(code >> 8)
	payload[1] = byte(code)
	copy(payload[2:], reason)
	if err := frame.ValidateClosePayload(payload); err != nil {
		return err
	}
	if c.closing.Load() {
		return nil
	}
	if !c.tryLockWrite() {
		return ErrWriteBusy
	}
	if err := c.completeWriteDrainLocked(); err != nil {
		c.unlockWrite()
		return err
	}
	if c.closed.Load() {
		c.unlockWrite()
		return ErrClosed
	}
	if c.closing.Load() {
		c.unlockWrite()
		return nil
	}
	c.setCloseReason(code, reason)
	if err := c.sendFrameLocked(frame.Frame{Fin: true, Opcode: frame.Close, Payload: payload}); err != nil {
		c.unlockWrite()
		return err
	}
	if err := c.flush(); err != nil {
		c.unlockWrite()
		c.abortTransport(err)
		return err
	}
	c.closing.Store(true)
	c.unlockWrite()
	c.startCloseTimer()
	return nil
}

// send validates one complete application message while holding write
// ownership, optionally compresses it, and queues exactly one logical message.
func (c *Conn) send(typ MessageType, payload []byte) error {
	opcode := frame.Text
	if typ == BinaryMessage {
		opcode = frame.Binary
	} else if typ != TextMessage {
		return frame.ErrProtocol
	}
	if !c.tryLockWrite() {
		return ErrWriteBusy
	}
	defer c.unlockWrite()
	if err := c.completeWriteDrainLocked(); err != nil {
		return err
	}
	if !c.opened.Load() {
		return ErrNotReady
	}
	if c.closed.Load() || c.closing.Load() {
		return ErrClosed
	}
	if uint64(len(payload)) > c.maxMessageSize() {
		return frame.ErrMessageTooBig
	}
	if typ == TextMessage && c.utf8ValidationEnabled() && !utf8.Valid(payload) {
		return frame.ErrInvalidUTF8
	}
	if c.compression != nil && len(payload) > 0 {
		compressed := false
		err := c.compression.encoder.EncodeBorrowed(payload, func(encoded []byte) error {
			framePayload := payload
			if len(encoded) < len(payload) {
				framePayload = encoded
				compressed = true
			}
			return c.sendFrameLocked(frame.Frame{
				Fin: true, RSV1: compressed, Opcode: opcode, Payload: framePayload,
			})
		})
		if err == nil && compressed {
			c.compression.encoder.Commit(payload)
		}
		return err
	}
	return c.sendFrameLocked(frame.Frame{Fin: true, Opcode: opcode, Payload: payload})
}

func (c *Conn) sendFrame(f frame.Frame) error {
	if !c.opened.Load() {
		return ErrNotReady
	}
	if c.closed.Load() || c.closing.Load() {
		return ErrClosed
	}
	if !c.tryLockWrite() {
		return ErrWriteBusy
	}
	defer c.unlockWrite()
	if err := c.completeWriteDrainLocked(); err != nil {
		return err
	}
	if err := c.sendFrameLocked(f); err != nil {
		return err
	}
	return c.flush()
}

// sendProtocolControlFrame preserves protocol progress when a Writer owns the
// normal write lock: Close is handed off, while Ping/Pong use an owned frame
// that can be safely queued concurrently by UIO.
func (c *Conn) sendProtocolControlFrame(f frame.Frame) error {
	if !c.opened.Load() {
		return ErrNotReady
	}
	if c.closed.Load() || c.writes.close.writerIsAborted() || (c.closing.Load() && f.Opcode != frame.Close) {
		return ErrClosed
	}
	if c.tryLockWrite() {
		defer c.unlockWrite()
		if c.writes.close.writerIsAborted() {
			return ErrClosed
		}
		if err := c.completeWriteDrainLocked(); err != nil {
			return err
		}
		if err := c.sendFrameLocked(f); err != nil {
			return err
		}
		return c.flush()
	}
	if f.Opcode == frame.Close {
		c.closing.Store(true)
		if !c.writes.close.queueClose(f.Payload) {
			return ErrClosed
		}
		return c.tryCompleteWriteDrain()
	}
	return c.sendConcurrentControlFrame(f)
}

// sendConcurrentControlFrame materializes the complete frame because it queues
// through UIO without holding the normal write lock or borrowing caller memory.
func (c *Conn) sendConcurrentControlFrame(f frame.Frame) error {
	if !frame.IsControl(f.Opcode) || len(f.Payload) > 125 {
		return frame.ErrProtocol
	}
	var maskKey [4]byte
	if c.isClient() {
		if _, err := rand.Read(maskKey[:]); err != nil {
			return err
		}
		f.Masked = true
	}
	wireSize := frameWireSize(len(f.Payload), f.Masked)
	owned := uio.AcquireBuffer(wireSize)
	dst := owned.AvailableBuffer()[:wireSize]
	wire := frame.Append(dst[:0], f, maskKey)
	owned.CommitWrite(len(wire))
	heartbeat := c.heartbeat
	if heartbeat != nil {
		heartbeat.submitMu.Lock()
	}
	c.trackOutbound(wireSize)
	n, err := c.raw.WriteOwned(owned)
	err = c.finishFrameWrite(f.Opcode, n, wireSize, err)
	if heartbeat != nil {
		heartbeat.submitMu.Unlock()
	}
	if err != nil {
		return err
	}
	return c.flush()
}

// sendFrameLocked requires connWriteState.mu. Server frames use header+borrowed
// payload writev above the coalescing threshold; client frames are materialized
// because RFC masking must not mutate caller-owned payload.
func (c *Conn) sendFrameLocked(f frame.Frame) error {
	if heartbeat := c.heartbeat; heartbeat != nil {
		heartbeat.submitMu.Lock()
		defer heartbeat.submitMu.Unlock()
	}
	return c.sendFrameSubmitted(f)
}

// sendFrameSubmitted requires heartbeat.submitMu when heartbeat is enabled, so
// accepted byte positions follow the actual UIO submission order.
func (c *Conn) sendFrameSubmitted(f frame.Frame) error {
	if c.closed.Load() || (c.closing.Load() && f.Opcode != frame.Close) {
		return ErrClosed
	}
	if maxPayload := c.maxFramePayload(); maxPayload > 0 && uint64(len(f.Payload)) > maxPayload {
		return frame.ErrMessageTooBig
	}
	if f.Opcode == frame.Close {
		if c.writes.close.closeFrameWasSent() {
			return nil
		}
	}
	var maskKey [4]byte
	if c.isClient() {
		if _, err := rand.Read(maskKey[:]); err != nil {
			return err
		}
		f.Masked = true
	}
	wireSize := frameWireSize(len(f.Payload), f.Masked)
	c.trackOutbound(wireSize)
	c.markHeartbeatPingTarget(f)
	threshold := c.writeBufferedThreshold()
	if f.Masked || (threshold > 0 && wireSize < threshold) {
		owned := uio.AcquireBuffer(wireSize)
		dst := owned.AvailableBuffer()[:wireSize]
		wire := frame.Append(dst[:0], f, maskKey)
		owned.CommitWrite(len(wire))
		n, err := c.raw.WriteOwned(owned)
		return c.finishFrameWrite(f.Opcode, n, wireSize, err)
	}
	scratch := acquireServerFrameScratch()
	header := frame.AppendHeader(scratch.header[:0], f, maskKey)
	scratch.vec[0] = header
	scratch.vec[1] = f.Payload
	n, err := c.raw.Writev(scratch.vec[:])
	releaseServerFrameScratch(scratch)
	return c.finishFrameWrite(f.Opcode, n, wireSize, err)
}

func (c *Conn) completeWriteDrainLocked() error {
	if !c.writes.close.drainIsRequested() {
		return nil
	}
	for {
		payload := c.writes.close.takeClose()
		if payload != nil {
			err := c.sendFrameLocked(frame.Frame{Fin: true, Opcode: frame.Close, Payload: payload})
			if err == nil {
				err = c.flush()
			}
			if err != nil {
				c.writes.close.abortDrain()
				return err
			}
		}
		if c.writes.close.completeDrain() {
			return nil
		}
	}
}

func (c *Conn) tryCompleteWriteDrain() error {
	if !c.writes.close.drainIsRequested() {
		return nil
	}
	if !c.writes.mu.TryLock() {
		return nil
	}
	err := c.completeWriteDrainLocked()
	c.writes.mu.Unlock()
	return err
}

func (c *Conn) unlockWrite() {
	c.writes.mu.Unlock()
	if c.writes.close.drainIsRequested() {
		if err := c.tryCompleteWriteDrain(); err != nil {
			c.setCloseError(err)
			c.closing.Store(true)
			c.abortTransport(err)
		}
	}
	_, _ = c.tryCloseTransport()
}

func (c *Conn) tryLockWrite() bool { return c.writes.mu.TryLock() }

// finishFrameWrite reconciles progress accounting with a rejected or partial
// transport write and translates UIO's connection-level limit to the UWS API.
func (c *Conn) finishFrameWrite(opcode frame.OpCode, n, want int, err error) error {
	if err != nil {
		c.rollbackOutbound(want - n)
		if errors.Is(err, uio.ErrOutboundOverflow) {
			return ErrBackpressure
		}
		if errors.Is(err, net.ErrClosed) {
			return ErrClosed
		}
		return err
	}
	if n != want {
		c.rollbackOutbound(want - n)
		return io.ErrShortWrite
	}
	if opcode == frame.Close {
		c.writes.close.markCloseFrameSent()
	}
	return nil
}

func (c *Conn) writeTransportOwned(buffer *uio.Buffer) error {
	if heartbeat := c.heartbeat; heartbeat != nil {
		heartbeat.submitMu.Lock()
		defer heartbeat.submitMu.Unlock()
	}
	want := buffer.Len()
	// Handshake bytes participate in graceful-close ordering just like frames.
	c.writes.close.pendingBytes.Add(int64(want))
	if heartbeat := c.heartbeat; heartbeat != nil {
		heartbeat.outboundAccepted.Add(uint64(want))
	}
	n, err := c.raw.WriteOwned(buffer)
	if err != nil {
		c.rollbackOutbound(want - n)
		return err
	}
	if n != want {
		c.rollbackOutbound(want - n)
		return io.ErrShortWrite
	}
	return nil
}

func frameWireSize(payload int, masked bool) int {
	size := payload + 2
	if payload >= 126 && payload <= 0xffff {
		size += 2
	} else if payload > 0xffff {
		size += 8
	}
	if masked {
		size += 4
	}
	return size
}

// trackOutbound records accepted wire positions used by graceful close and
// heartbeat send detection. It intentionally performs no limit check.
func (c *Conn) trackOutbound(n int) {
	c.writes.close.pendingBytes.Add(int64(n))
	if heartbeat := c.heartbeat; heartbeat != nil {
		heartbeat.outboundAccepted.Add(uint64(n))
	}
}

// reducePendingOutbound reconciles either transport retirement or a rejected
// write against the same pending count without allowing a concurrent callback
// to make that count negative.
func (c *Conn) reducePendingOutbound(n int) int64 {
	if n <= 0 {
		return 0
	}
	for {
		current := c.writes.close.pendingBytes.Load()
		if current == 0 {
			return 0
		}
		remaining := current - int64(n)
		if remaining < 0 {
			remaining = 0
		}
		if c.writes.close.pendingBytes.CompareAndSwap(current, remaining) {
			return current - remaining
		}
	}
}

// rollbackOutbound removes bytes not accepted by UIO. They did not leave the
// socket queue, so they must not advance heartbeat's retired byte position.
func (c *Conn) rollbackOutbound(n int) {
	if rolledBack := c.reducePendingOutbound(n); rolledBack != 0 && c.heartbeat != nil {
		c.heartbeat.outboundAccepted.Add(^uint64(rolledBack - 1))
	}
}

// releaseOutbound advances protocol send progress only for UIO's OnOutbound
// callback, which confirms bytes actually left its queue.
func (c *Conn) releaseOutbound(n int) {
	if retiredBytes := c.reducePendingOutbound(n); retiredBytes != 0 && c.heartbeat != nil {
		retired := c.heartbeat.outboundRetired.Add(uint64(retiredBytes))
		c.heartbeat.markPingSent(retired)
	}
}

func (c *Conn) markHeartbeatPingTarget(f frame.Frame) {
	heartbeat := c.heartbeat
	if heartbeat == nil || f.Opcode != frame.Ping || len(f.Payload) != 8 {
		return
	}
	heartbeat.markPingTarget(binary.BigEndian.Uint64(f.Payload))
}

func (heartbeat *heartbeatState) markPingSent(retired uint64) {
	if !heartbeat.pingOutstanding.Load() {
		return
	}
	heartbeat.mu.Lock()
	if heartbeat.pingOutstanding.Load() && heartbeat.pingSentAt == 0 &&
		heartbeat.pingTarget != 0 && retired >= heartbeat.pingTarget {
		heartbeat.pingSentAt = time.Now().UnixNano()
		heartbeat.sendStalledAt = 0
	}
	heartbeat.mu.Unlock()
}

func (heartbeat *heartbeatState) beginPing(now time.Time, nonce uint64) bool {
	heartbeat.mu.Lock()
	defer heartbeat.mu.Unlock()
	if heartbeat.pingOutstanding.Load() {
		return false
	}
	heartbeat.pingQueuedAt = now.UnixNano()
	if heartbeat.sendStalledAt != 0 {
		heartbeat.pingQueuedAt = heartbeat.sendStalledAt
	}
	heartbeat.pingSentAt = 0
	heartbeat.pingNonce = nonce
	heartbeat.pingTarget = 0
	heartbeat.pingOutstanding.Store(true)
	return true
}

func (heartbeat *heartbeatState) markPingTarget(nonce uint64) {
	heartbeat.mu.Lock()
	if heartbeat.pingOutstanding.Load() && heartbeat.pingNonce == nonce {
		heartbeat.pingTarget = heartbeat.outboundAccepted.Load()
	}
	heartbeat.mu.Unlock()
}

func (heartbeat *heartbeatState) acknowledgePing(nonce uint64) bool {
	heartbeat.mu.Lock()
	defer heartbeat.mu.Unlock()
	if nonce == 0 || !heartbeat.pingOutstanding.Load() || heartbeat.pingNonce != nonce {
		return false
	}
	heartbeat.clearPingLocked()
	heartbeat.sendStalledAt = 0
	return true
}

func (heartbeat *heartbeatState) cancelPing(nonce uint64) {
	heartbeat.mu.Lock()
	if heartbeat.pingOutstanding.Load() && heartbeat.pingNonce == nonce {
		heartbeat.clearPingLocked()
	}
	heartbeat.mu.Unlock()
}

// noteSendStall preserves the first failed or busy send attempt across retries.
// A later accepted Ping inherits this queue deadline until OnOutbound proves
// that it was actually sent.
func (heartbeat *heartbeatState) noteSendStall(now time.Time) {
	heartbeat.mu.Lock()
	if !heartbeat.pingOutstanding.Load() && heartbeat.sendStalledAt == 0 {
		heartbeat.sendStalledAt = now.UnixNano()
	}
	heartbeat.mu.Unlock()
}

func (heartbeat *heartbeatState) expirePing(now time.Time, timeout time.Duration) bool {
	heartbeat.mu.Lock()
	defer heartbeat.mu.Unlock()
	if !heartbeat.pingOutstanding.Load() {
		if heartbeat.sendStalledAt == 0 || now.Sub(time.Unix(0, heartbeat.sendStalledAt)) < timeout {
			return false
		}
		heartbeat.sendStalledAt = 0
		return true
	}
	startedAt := heartbeat.pingQueuedAt
	if heartbeat.pingSentAt != 0 {
		startedAt = heartbeat.pingSentAt
	}
	if startedAt == 0 || now.Sub(time.Unix(0, startedAt)) < timeout {
		return false
	}
	heartbeat.clearPingLocked()
	heartbeat.sendStalledAt = 0
	return true
}

// clearPingLocked only cancels the current Ping attempt. A failed enqueue must
// retain sendStalledAt so the next attempt cannot restart the send deadline.
func (heartbeat *heartbeatState) clearPingLocked() {
	heartbeat.pingOutstanding.Store(false)
	heartbeat.pingQueuedAt = 0
	heartbeat.pingSentAt = 0
	heartbeat.pingNonce = 0
	heartbeat.pingTarget = 0
}

func (c *Conn) flush() error {
	return c.raw.Flush()
}
