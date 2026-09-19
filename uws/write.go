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

	"github.com/petermattis/goid"
	"github.com/urpc/uio"
	"github.com/urpc/uio/uws/internal/frame"
)

type serverFrameScratch struct {
	header [14]byte
	vec    [2][]byte
}

const (
	dispatchWriteBatchSmallBytes = 8 << 10
	dispatchWriteBatchMaxBytes   = 64 << 10
)

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

func (state *connWriteState) requestDrain() { state.drainRequested.Store(true) }

func (state *connWriteState) drainIsRequested() bool { return state.drainRequested.Load() }

func (state *connWriteState) completeDrain() { state.drainRequested.Store(false) }

func (state *connWriteState) closeFrameWasSent() bool { return state.closeFrameSent }

func (state *connWriteState) markCloseFrameSent() { state.closeFrameSent = true }

func (batch *dispatchWriteBatch) ownerID() int64 { return batch.owner.Load() }

func (batch *dispatchWriteBatch) begin(owner int64) { batch.owner.Store(owner) }

func (batch *dispatchWriteBatch) requestFinish() { batch.finishRequested.Store(true) }

func (batch *dispatchWriteBatch) finishIsRequested() bool { return batch.finishRequested.Load() }

func (batch *dispatchWriteBatch) complete() {
	batch.owner.Store(0)
	batch.finishRequested.Store(false)
}

func (batch *dispatchWriteBatch) takeBuffer() *uio.Buffer {
	buffer := batch.buffer
	batch.buffer = nil
	return buffer
}

// SendText queues payload as one text message. The transport flushes accepted
// data at its callback or task boundary.
func (c *Conn) SendText(payload []byte) error {
	return c.send(MessageType(TextMessage), payload)
}

// SendBinary queues payload as one binary message. The transport flushes
// accepted data at its callback or task boundary.
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

func (c *Conn) tryHeartbeatPing(now time.Time) (bool, error) {
	if c.heartbeat == nil || !c.opened.Load() || c.closed.Load() || c.closing.Load() {
		return false, ErrClosed
	}
	if !c.writes.mu.TryLock() {
		return false, nil
	}
	if err := c.flushForeignDispatchWritesLocked(); err != nil {
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
	err := c.sendFrameLocked(frame.Frame{Fin: true, Opcode: frame.Ping, Payload: payload[:]})
	if err == nil {
		err = c.flush()
	}
	if err != nil {
		heartbeat.cancelPing(nonce)
	}
	c.unlockWrite()
	return true, err
}

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
	err := c.flushForeignDispatchWritesLocked()
	if err == nil && !c.closed.Load() && !c.closing.Load() {
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
		_ = c.raw.CloseWith(err)
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
	c.lockWrite()
	if err := c.flushForeignDispatchWritesLocked(); err != nil {
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
		_ = c.raw.CloseWith(err)
		return err
	}
	c.closing.Store(true)
	c.unlockWrite()
	c.startCloseTimer()
	return nil
}

func (c *Conn) send(typ MessageType, payload []byte) error {
	opcode := frame.Text
	if typ == BinaryMessage {
		opcode = frame.Binary
	} else if typ != TextMessage {
		return frame.ErrProtocol
	}
	c.lockWrite()
	defer c.unlockWrite()
	if err := c.flushForeignDispatchWritesLocked(); err != nil {
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
	c.lockWrite()
	defer c.unlockWrite()
	if err := c.flushForeignDispatchWritesLocked(); err != nil {
		return err
	}
	if err := c.sendFrameLocked(f); err != nil {
		return err
	}
	return c.flush()
}

func (c *Conn) sendFrameLocked(f frame.Frame) error {
	if c.closed.Load() || (c.closing.Load() && f.Opcode != frame.Close) {
		return ErrClosed
	}
	if maxPayload := c.maxFramePayload(); maxPayload > 0 && uint64(len(f.Payload)) > maxPayload {
		return frame.ErrMessageTooBig
	}
	if f.Opcode == frame.Close {
		if c.writes.closeFrameWasSent() {
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
	if !c.reserveOutbound(wireSize) {
		return ErrBackpressure
	}
	c.markHeartbeatPingTarget(f)
	state := c.dispatch
	owner := int64(0)
	if state != nil {
		owner = state.writeBatch.ownerID()
	}
	if owner != 0 && owner == goid.Get() &&
		!f.Masked && isDataFrame(f.Opcode) && wireSize <= dispatchWriteBatchMaxBytes {
		if err := c.appendDispatchFrameLocked(f, wireSize, maskKey); err != nil {
			c.releaseOutbound(wireSize)
			return err
		}
		return nil
	}
	if err := c.flushDispatchWritesLocked(); err != nil {
		c.releaseOutbound(wireSize)
		return err
	}
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

func isDataFrame(opcode frame.OpCode) bool {
	return opcode == frame.Text || opcode == frame.Binary || opcode == frame.Continuation
}

func (c *Conn) appendDispatchFrameLocked(f frame.Frame, wireSize int, maskKey [4]byte) error {
	batch := &c.dispatch.writeBatch
	if batch.buffer != nil && batch.buffer.Available() < wireSize {
		if err := c.flushDispatchWritesLocked(); err != nil {
			return err
		}
	}
	if batch.buffer == nil {
		capacity := dispatchWriteBatchSmallBytes
		if wireSize > dispatchWriteBatchSmallBytes {
			capacity = dispatchWriteBatchMaxBytes
		}
		batch.buffer = uio.AcquireBuffer(capacity)
	}
	dst := batch.buffer.AvailableBuffer()[:wireSize]
	wire := frame.Append(dst[:0], f, maskKey)
	batch.buffer.CommitWrite(len(wire))
	return nil
}

func (c *Conn) beginDispatchWrites() (bool, error) {
	if !c.writes.mu.TryLock() {
		return false, nil
	}
	if err := c.finishWriteResponsibilitiesLocked(); err != nil {
		c.writes.mu.Unlock()
		return false, err
	}
	if c.closed.Load() || c.closing.Load() {
		c.writes.mu.Unlock()
		return false, nil
	}
	c.dispatch.writeBatch.begin(goid.Get())
	c.writes.mu.Unlock()
	return true, nil
}

func (c *Conn) endDispatchWrites() error {
	owner := goid.Get()
	if c.dispatch == nil || c.dispatch.writeBatch.ownerID() != owner {
		return nil
	}
	c.dispatch.writeBatch.requestFinish()
	return c.tryFinishWriteResponsibilities()
}

func (c *Conn) flushDispatchBatch() error {
	return c.endDispatchWrites()
}

func (c *Conn) flushForeignDispatchWritesLocked() error {
	if c.dispatch == nil {
		if c.writes.drainIsRequested() {
			return c.finishWriteResponsibilitiesLocked()
		}
		return nil
	}
	if c.dispatch.writeBatch.finishIsRequested() || c.writes.drainIsRequested() {
		return c.finishWriteResponsibilitiesLocked()
	}
	owner := c.dispatch.writeBatch.ownerID()
	if owner != 0 && owner == goid.Get() {
		return nil
	}
	return c.flushDispatchWritesLocked()
}

func (c *Conn) finishDispatchWritesLocked() error {
	if c.dispatch == nil || !c.dispatch.writeBatch.finishIsRequested() {
		return nil
	}
	err := c.flushDispatchWritesLocked()
	c.dispatch.writeBatch.complete()
	return err
}

func (c *Conn) finishWriteResponsibilitiesLocked() error {
	err := c.finishDispatchWritesLocked()
	c.writes.completeDrain()
	return err
}

func (c *Conn) tryFinishWriteResponsibilities() error {
	finishDispatch := c.dispatch != nil && c.dispatch.writeBatch.finishIsRequested()
	if !finishDispatch && !c.writes.drainIsRequested() {
		return nil
	}
	if !c.writes.mu.TryLock() {
		return nil
	}
	err := c.finishWriteResponsibilitiesLocked()
	c.writes.mu.Unlock()
	return err
}

func (c *Conn) unlockWrite() {
	c.writes.mu.Unlock()
	finishDispatch := c.dispatch != nil && c.dispatch.writeBatch.finishIsRequested()
	if finishDispatch || c.writes.drainIsRequested() {
		if err := c.tryFinishWriteResponsibilities(); err != nil {
			c.reportDispatchWriteError(err)
		}
	}
	_, _ = c.tryCloseTransport()
}

func (c *Conn) lockWrite() {
	c.writes.mu.Lock()
}

func (c *Conn) flushDispatchWritesLocked() error {
	if c.dispatch == nil {
		return nil
	}
	buffer := c.dispatch.writeBatch.takeBuffer()
	if buffer == nil {
		return nil
	}
	want := buffer.Len()
	n, err := c.raw.WriteOwned(buffer)
	return c.finishFrameWrite(0, n, want, err)
}

func (c *Conn) finishFrameWrite(opcode frame.OpCode, n, want int, err error) error {
	if err != nil {
		c.releaseOutbound(want - n)
		if errors.Is(err, net.ErrClosed) {
			return ErrClosed
		}
		return err
	}
	if n != want {
		c.releaseOutbound(want - n)
		return io.ErrShortWrite
	}
	if opcode == frame.Close {
		c.writes.markCloseFrameSent()
	}
	return nil
}

func (c *Conn) writeTransportOwned(buffer *uio.Buffer) error {
	want := buffer.Len()
	// Handshake bytes are not subject to message backpressure, but they still
	// participate in graceful-close ordering on asynchronous transports.
	c.pendingBytes.Add(int64(want))
	if heartbeat := c.heartbeat; heartbeat != nil {
		heartbeat.outboundAccepted.Add(uint64(want))
	}
	n, err := c.raw.WriteOwned(buffer)
	if err != nil {
		c.releaseOutbound(want - n)
		return err
	}
	if n != want {
		c.releaseOutbound(want - n)
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

func (c *Conn) reserveOutbound(n int) bool {
	limit := c.maxOutboundBytes()
	for {
		current := c.pendingBytes.Load()
		if limit > 0 && current > int64(limit)-int64(n) {
			return false
		}
		if c.pendingBytes.CompareAndSwap(current, current+int64(n)) {
			if heartbeat := c.heartbeat; heartbeat != nil {
				heartbeat.outboundAccepted.Add(uint64(n))
			}
			return true
		}
	}
}

func (c *Conn) releaseOutbound(n int) {
	if n <= 0 {
		return
	}
	for {
		current := c.pendingBytes.Load()
		if current == 0 {
			return
		}
		remaining := current - int64(n)
		if remaining < 0 {
			remaining = 0
		}
		if c.pendingBytes.CompareAndSwap(current, remaining) {
			if heartbeat := c.heartbeat; heartbeat != nil {
				retired := heartbeat.outboundRetired.Add(uint64(current - remaining))
				heartbeat.markPingSent(retired)
			}
			return
		}
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
	return true
}

func (heartbeat *heartbeatState) cancelPing(nonce uint64) {
	heartbeat.mu.Lock()
	if heartbeat.pingOutstanding.Load() && heartbeat.pingNonce == nonce {
		heartbeat.clearPingLocked()
	}
	heartbeat.mu.Unlock()
}

func (heartbeat *heartbeatState) expirePing(now time.Time, timeout time.Duration) bool {
	heartbeat.mu.Lock()
	defer heartbeat.mu.Unlock()
	if !heartbeat.pingOutstanding.Load() {
		return false
	}
	startedAt := heartbeat.pingQueuedAt
	if heartbeat.pingSentAt != 0 {
		startedAt = heartbeat.pingSentAt
	}
	if startedAt == 0 || now.Sub(time.Unix(0, startedAt)) < timeout {
		return false
	}
	heartbeat.clearPingLocked()
	return true
}

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
