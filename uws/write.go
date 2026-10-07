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

// errBatchRefused reports that the transport refused frames already accepted
// into a write batch. Each batched frame is admitted against the outbound limit
// and nothing else writes to the transport before the batch is handed over, so
// the limit alone cannot cause it.
var errBatchRefused = errors.New("uws: transport refused accepted frames")

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

func (state *connCloseProgress) phase() uint32 {
	return (state.flags.Load() & transportPhaseMask) >> transportPhaseShift
}

// setPhaseLocked changes only the transport claim bits. All flag writes hold
// transitionMu; reads on the normal send path need just one atomic load.
func (state *connCloseProgress) setPhaseLocked(phase uint32) {
	flags := state.flags.Load()
	state.flags.Store((flags &^ transportPhaseMask) | phase<<transportPhaseShift)
}

func (state *connCloseProgress) closeFrameWasSent() bool {
	return state.flags.Load()&closeFrameSent != 0
}

func (state *connCloseProgress) closeIsDeferred() bool {
	return state.flags.Load()&closeFrameDeferred != 0
}

// closeFrameQueued reports whether a Close frame was accepted or waits for
// outbound room; either way no second Close frame may follow it.
func (state *connCloseProgress) closeFrameQueued() bool {
	return state.flags.Load()&(closeFrameSent|closeFrameDeferred) != 0
}

func (state *connCloseProgress) markCloseFrameSent() {
	state.transitionMu.Lock()
	state.deferredClose = nil
	state.flags.Store((state.flags.Load() | closeFrameSent) &^ closeFrameDeferred)
	state.transitionMu.Unlock()
}

// deferClose keeps a copy of a Close frame the outbound limit had no room for.
// It reports false when the transport was claimed or a Close frame was queued
// first.
func (state *connCloseProgress) deferClose(payload []byte) bool {
	state.transitionMu.Lock()
	defer state.transitionMu.Unlock()
	flags := state.flags.Load()
	if flags&(closeFrameSent|closeFrameDeferred) != 0 || state.phase() == transportCloseClaimed {
		return false
	}
	state.deferredClose = &deferredCloseFrame{payload: append([]byte(nil), payload...)}
	state.flags.Store(flags | closeFrameDeferred)
	return true
}

// takeDeferredClose claims the deferred payload for one submission attempt.
// closeFrameDeferred stays set meanwhile, so no other Close frame can pass it.
func (state *connCloseProgress) takeDeferredClose() []byte {
	state.transitionMu.Lock()
	deferred := state.deferredClose
	state.deferredClose = nil
	state.transitionMu.Unlock()
	if deferred == nil {
		return nil
	}
	return deferred.payload
}

// restoreDeferredClose returns a payload the outbound limit refused again,
// unless an abort discarded the deferred frame meanwhile.
func (state *connCloseProgress) restoreDeferredClose(payload []byte) {
	state.transitionMu.Lock()
	if state.flags.Load()&closeFrameDeferred != 0 {
		state.deferredClose = &deferredCloseFrame{payload: payload}
	}
	state.transitionMu.Unlock()
}

func (state *connCloseProgress) claimAbort() bool {
	state.transitionMu.Lock()
	defer state.transitionMu.Unlock()
	if state.phase() == transportCloseClaimed {
		return false
	}
	state.deferredClose = nil
	state.flags.Store(state.flags.Load() &^ closeFrameDeferred)
	state.setPhaseLocked(transportCloseClaimed)
	return true
}

func (state *connCloseProgress) requestTransportClose() bool {
	state.transitionMu.Lock()
	defer state.transitionMu.Unlock()
	switch state.phase() {
	case transportCloseIdle:
		state.setPhaseLocked(transportClosePending)
		return true
	case transportClosePending:
		return true
	default:
		return false
	}
}

// claimTransportClose claims shutdown once no Close frame waits for outbound
// room and every accepted byte has retired.
func (state *connCloseProgress) claimTransportClose() bool {
	if state.phase() != transportClosePending || state.pendingBytes.Load() != 0 || state.closeIsDeferred() {
		return false
	}
	state.transitionMu.Lock()
	defer state.transitionMu.Unlock()
	if state.phase() != transportClosePending || state.closeIsDeferred() || state.pendingBytes.Load() != 0 {
		return false
	}
	state.setPhaseLocked(transportCloseClaimed)
	return true
}

// SendText queues payload as one text message. The transport flushes accepted
// data in the connection's current or next I/O task. payload must be valid
// UTF-8: it is sent as given, and only received text is validated.
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

// sendHeartbeatPing queues a heartbeat Ping unless one is outstanding. It
// records queue time now, while markPingSent starts the Pong timeout only after
// OnOutbound proves that the complete Ping frame left UIO's queue.
func (c *Conn) sendHeartbeatPing(now time.Time) error {
	heartbeat := c.heartbeat
	if heartbeat == nil || !c.opened.Load() || c.closed.Load() || c.closing.Load() {
		return ErrClosed
	}
	var payload [8]byte
	if _, err := rand.Read(payload[:]); err != nil {
		return err
	}
	nonce := binary.BigEndian.Uint64(payload[:])
	if nonce == 0 {
		nonce = 1
		binary.BigEndian.PutUint64(payload[:], nonce)
	}
	c.writes.mu.Lock()
	defer c.unlockWrite()
	if c.closed.Load() || c.closing.Load() {
		return ErrClosed
	}
	if !heartbeat.beginPing(now, nonce) {
		return nil
	}
	err := c.sendFrameLocked(frame.Frame{Fin: true, Opcode: frame.Ping, Payload: payload[:]})
	if err == nil {
		err = c.flushLocked()
	}
	if err != nil {
		heartbeat.cancelPing(nonce)
	}
	return err
}

// sendHeartbeatClose starts the close handshake with a peer that missed its
// heartbeat deadline. Such a peer is not waited for: a Close frame that cannot
// be queued aborts the transport instead.
func (c *Conn) sendHeartbeatClose(code uint16, reason string) {
	payload := make([]byte, 2+len(reason))
	payload[0] = byte(code >> 8)
	payload[1] = byte(code)
	copy(payload[2:], reason)
	c.writes.mu.Lock()
	if c.closed.Load() || c.closing.Load() {
		c.unlockWrite()
		return
	}
	c.setCloseReason(code, reason)
	err := c.sendFrameLocked(frame.Frame{Fin: true, Opcode: frame.Close, Payload: payload})
	if err == nil {
		err = c.flushLocked()
	}
	c.closing.Store(true)
	c.unlockWrite()
	if err != nil {
		c.setCloseError(err)
		c.abortTransport(err)
		return
	}
	_ = c.closeTransport()
}

// Close starts a graceful WebSocket close handshake. The Close frame follows
// every message accepted before it. When the outbound limit has no room for
// the frame, it waits for the transport to drain instead of failing; either
// way the connection stops accepting messages, and CloseTimeout bounds the
// handshake.
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
	c.writes.mu.Lock()
	if c.closed.Load() {
		c.unlockWrite()
		return ErrClosed
	}
	if c.closing.Load() {
		c.unlockWrite()
		return nil
	}
	c.setCloseReason(code, reason)
	deferred, err := c.sendCloseFrameLocked(payload)
	c.unlockWrite()
	if err != nil {
		if !errors.Is(err, ErrClosed) {
			c.abortTransport(err)
		}
		return err
	}
	if deferred {
		c.sendDeferredClose()
	}
	c.startCloseTimer()
	return nil
}

// sendCloseFrame queues a protocol Close frame; see sendCloseFrameLocked.
func (c *Conn) sendCloseFrame(payload []byte) error {
	if !c.opened.Load() {
		return ErrNotReady
	}
	c.writes.mu.Lock()
	deferred, err := c.sendCloseFrameLocked(payload)
	c.unlockWrite()
	if deferred {
		c.sendDeferredClose()
	}
	return err
}

// sendCloseFrameLocked queues a Close frame behind every accepted byte and
// marks the connection closing. It requires writes.mu. A frame the outbound
// limit has no room for is deferred rather than failed; the caller must then
// call sendDeferredClose after releasing the lock.
func (c *Conn) sendCloseFrameLocked(payload []byte) (deferred bool, err error) {
	err = c.sendFrameLocked(frame.Frame{Fin: true, Opcode: frame.Close, Payload: payload})
	if errors.Is(err, ErrBackpressure) {
		deferred, err = c.writes.close.deferClose(payload), nil
	}
	c.closing.Store(true)
	if err == nil {
		err = c.flushLocked()
	}
	return deferred, err
}

// sendDeferredClose submits a deferred Close frame once the transport has room
// for it. No frame is accepted after closing starts, so it still follows every
// accepted byte without the write lock.
func (c *Conn) sendDeferredClose() {
	state := &c.writes.close
	for {
		payload := state.takeDeferredClose()
		if payload == nil {
			return
		}
		f := frame.Frame{Fin: true, Opcode: frame.Close, Payload: payload}
		var maskKey [4]byte
		if c.isClient() {
			if _, err := rand.Read(maskKey[:]); err != nil {
				c.failStream(err)
				return
			}
			f.Masked = true
		}
		wireSize := frameWireSize(len(payload), f.Masked)
		c.trackOutbound(wireSize)
		err := c.writeFrameOwned(f, maskKey, wireSize)
		if err == nil {
			_ = c.flush()
			return
		}
		if errors.Is(err, ErrClosed) {
			return
		}
		if !errors.Is(err, ErrBackpressure) {
			c.failStream(err)
			return
		}
		state.restoreDeferredClose(payload)
		// A retirement between the refusal and the restore found nothing to
		// send, so the room it made must be noticed here.
		if limit := c.maxOutbound(); limit <= 0 || c.raw.OutboundBuffered()+wireSize > limit {
			return
		}
	}
}

// send validates one complete application message, optionally compresses it,
// and queues it as one logical message.
func (c *Conn) send(typ MessageType, payload []byte) error {
	opcode := frame.Text
	if typ == BinaryMessage {
		opcode = frame.Binary
	} else if typ != TextMessage {
		return frame.ErrProtocol
	}
	c.writes.mu.Lock()
	if c.batching.Load() {
		if batched, err := c.batchMessageLocked(opcode, payload); batched {
			c.unlockWrite()
			return err
		}
	}
	defer c.unlockWrite()
	if !c.opened.Load() {
		return ErrNotReady
	}
	if c.closed.Load() || c.closing.Load() {
		return ErrClosed
	}
	if uint64(len(payload)) > c.maxMessageSize() {
		return frame.ErrMessageTooBig
	}
	// Text payloads are not revalidated here: the RFC's valid-UTF-8 rule for
	// outgoing text is the sender's data contract, and what the peer sent has
	// already been validated on read. Checking every send measured 0.5% of an
	// echo host's whole CPU.
	if c.compression != nil && len(payload) > 0 {
		compressed := false
		err := c.compression.encoder.EncodeBorrowed(payload, func(encoded []byte) error {
			if len(encoded) < len(payload) {
				compressed = true
				return c.sendMessageLocked(opcode, true, encoded)
			}
			return c.sendMessageLocked(opcode, false, payload)
		})
		if err == nil && compressed {
			c.compression.encoder.Commit(payload)
		}
		return err
	}
	return c.sendMessageLocked(opcode, false, payload)
}

// sendMessageLocked queues one message payload, split into fragments of at most
// MaxFramePayload. It requires writes.mu. A fragmented message is admitted
// whole or not at all: once its first fragment is accepted, the rest must
// follow before any other data frame.
func (c *Conn) sendMessageLocked(opcode frame.OpCode, compressed bool, payload []byte) error {
	maxFrame := c.maxFramePayload()
	if uint64(len(payload)) <= maxFrame {
		return c.sendFrameLocked(frame.Frame{Fin: true, RSV1: compressed, Opcode: opcode, Payload: payload})
	}
	if !c.outboundHasRoom(fragmentedWireSize(len(payload), maxFrame, c.isClient())) {
		return ErrBackpressure
	}
	for offset := 0; offset < len(payload); {
		end := offset + int(min(uint64(len(payload)-offset), maxFrame))
		f := frame.Frame{Fin: end == len(payload), Opcode: frame.Continuation, Payload: payload[offset:end]}
		if offset == 0 {
			f.Opcode, f.RSV1 = opcode, compressed
		}
		if err := c.sendFrameLocked(f); err != nil {
			if offset > 0 && !errors.Is(err, ErrClosed) {
				c.failStream(err)
			}
			return err
		}
		offset = end
	}
	return nil
}

// sendFrame queues one frame and flushes it.
func (c *Conn) sendFrame(f frame.Frame) error {
	if !c.opened.Load() {
		return ErrNotReady
	}
	c.writes.mu.Lock()
	defer c.unlockWrite()
	if err := c.sendFrameLocked(f); err != nil {
		return err
	}
	return c.flushLocked()
}

// sendFrameLocked requires writes.mu. Server frames use header+borrowed
// payload writev above the coalescing threshold; client frames are materialized
// because RFC masking must not mutate caller-owned payload.
func (c *Conn) sendFrameLocked(f frame.Frame) error {
	if c.closed.Load() || (c.closing.Load() && f.Opcode != frame.Close) {
		return ErrClosed
	}
	if maxPayload := c.maxFramePayload(); maxPayload > 0 && uint64(len(f.Payload)) > maxPayload {
		return frame.ErrMessageTooBig
	}
	if f.Opcode == frame.Close && c.writes.close.closeFrameQueued() {
		return nil
	}
	var maskKey [4]byte
	if c.isClient() {
		if _, err := rand.Read(maskKey[:]); err != nil {
			return err
		}
		f.Masked = true
	}
	wireSize := frameWireSize(len(f.Payload), f.Masked)
	threshold := c.writeBufferedThreshold()
	small := threshold > 0 && wireSize < threshold
	if small && c.batching.Load() && c.outboundHasRoom(wireSize) {
		if err := c.batchFrame(f, maskKey, wireSize); err != nil {
			return err
		}
		c.trackOutbound(wireSize)
		c.markHeartbeatPingTarget(f)
		return c.finishFrameWrite(f.Opcode, wireSize, wireSize, nil)
	}
	// Frames batched earlier go first, so this one cannot overtake them.
	if err := c.flushBatchLocked(); err != nil {
		return err
	}
	c.trackOutbound(wireSize)
	c.markHeartbeatPingTarget(f)
	if f.Masked || small {
		if small {
			// Inside the connection's own task the frame is encoded straight
			// into the outbound queue: one copy of the payload, no buffer of
			// its own to acquire and release.
			dst, err := c.raw.ReserveOutbound(wireSize)
			if err == nil {
				frame.Append(dst[:0], f, maskKey)
				return c.finishFrameWrite(f.Opcode, wireSize, wireSize, nil)
			}
			if err != uio.ErrReserveUnsupported {
				return c.finishFrameWrite(f.Opcode, 0, wireSize, err)
			}
		}
		return c.writeFrameOwned(f, maskKey, wireSize)
	}
	scratch := acquireServerFrameScratch()
	header := frame.AppendHeader(scratch.header[:0], f, maskKey)
	scratch.vec[0] = header
	scratch.vec[1] = f.Payload
	n, err := c.raw.Writev(scratch.vec[:])
	releaseServerFrameScratch(scratch)
	return c.finishFrameWrite(f.Opcode, n, wireSize, err)
}

// writeFrameOwned encodes f into a buffer the transport takes over. The caller
// has already tracked wireSize.
func (c *Conn) writeFrameOwned(f frame.Frame, maskKey [4]byte, wireSize int) error {
	owned := uio.AcquireBuffer(wireSize)
	dst := owned.AvailableBuffer()[:wireSize]
	wire := frame.Append(dst[:0], f, maskKey)
	owned.CommitWrite(len(wire))
	n, err := c.raw.WriteOwned(owned)
	if errors.Is(err, uio.ErrOutboundOverflow) {
		// Nothing was accepted; the buffer stays ours.
		uio.ReleaseBuffer(owned)
	}
	return c.finishFrameWrite(f.Opcode, n, wireSize, err)
}

// batchMessageLocked is send for the common message of a batching read round:
// small, uncompressed, from a server with no heartbeat or close in progress.
// It makes send's checks and appends the frame to the batch in one step. It
// reports false, having done nothing, for any message it does not cover.
func (c *Conn) batchMessageLocked(opcode frame.OpCode, payload []byte) (bool, error) {
	config := c.config
	if config == nil || config.client || c.heartbeat != nil || c.compression != nil ||
		c.writes.close.flags.Load() != 0 || !c.opened.Load() || c.closed.Load() || c.closing.Load() {
		return false, nil
	}
	size := uint64(len(payload))
	if size > config.assembler.MaxMessage || size > config.parser.MaxFramePayload {
		return false, nil
	}
	wireSize := frameWireSize(len(payload), false)
	if config.writeBufferedThreshold <= 0 || wireSize >= config.writeBufferedThreshold || !c.outboundHasRoom(wireSize) {
		return false, nil
	}
	batch := c.batch
	if batch == nil || batch.Available() < wireSize {
		var err error
		if batch, err = c.startBatch(wireSize); err != nil {
			return true, err
		}
	}
	c.writes.close.pendingBytes.Add(int64(wireSize))
	dst := frame.AppendHeader(batch.AvailableBuffer()[:0], frame.Frame{Fin: true, Opcode: opcode, Payload: payload}, [4]byte{})
	copy(dst[len(dst):wireSize], payload)
	batch.CommitWrite(wireSize)
	return true, nil
}

func (c *Conn) batchBlockSize() int {
	if c.config == nil || c.config.batchBlockSize <= 0 {
		return writeBatchBlockSize(nil)
	}
	return c.config.batchBlockSize
}

// outboundHasRoom reports whether wireSize more bytes fit the transport's
// outbound limit behind the bytes already queued and batched. It requires
// writes.mu: the queue only shrinks while it is held, so bytes admitted here
// are still admitted when they reach the transport.
func (c *Conn) outboundHasRoom(wireSize int) bool {
	limit := c.maxOutbound()
	if limit <= 0 {
		return true
	}
	batched := 0
	if c.batch != nil {
		batched = c.batch.Len()
	}
	return c.raw.OutboundBuffered()+batched+wireSize <= limit
}

// batchFrame encodes a small frame into the write batch, which carries a read
// round's replies to the transport in one hand-off. It requires writes.mu.
func (c *Conn) batchFrame(f frame.Frame, maskKey [4]byte, wireSize int) error {
	batch := c.batch
	if batch == nil || batch.Available() < wireSize {
		var err error
		if batch, err = c.startBatch(wireSize); err != nil {
			return err
		}
	}
	frame.Append(batch.AvailableBuffer()[:0], f, maskKey)
	batch.CommitWrite(wireSize)
	return nil
}

// startBatch begins a write batch block with room for wireSize bytes, handing
// a full block over first. Only a round with more than one frame batches, so
// blocks are sized for many.
func (c *Conn) startBatch(wireSize int) (*uio.Buffer, error) {
	if err := c.flushBatchLocked(); err != nil {
		return nil, err
	}
	c.batch = uio.AcquireBuffer(max(wireSize, c.batchBlockSize()))
	return c.batch, nil
}

// flushBatchLocked hands the batched frames to the transport. It requires
// writes.mu. Their senders were already told they were accepted, so a
// transport that refuses them leaves a broken stream and is aborted.
func (c *Conn) flushBatchLocked() error {
	batch := c.batch
	if batch == nil {
		return nil
	}
	c.batch = nil
	want := batch.Len()
	n, err := c.raw.WriteOwned(batch)
	if errors.Is(err, uio.ErrOutboundOverflow) {
		uio.ReleaseBuffer(batch)
		err = errBatchRefused
	}
	if err = c.finishFrameWrite(frame.Continuation, n, want, err); err != nil && !errors.Is(err, ErrClosed) {
		c.failStream(err)
	}
	return err
}

// flushLocked hands any batched frames to the transport and then asks it to
// send what it holds. It requires writes.mu; callers without it use flush.
func (c *Conn) flushLocked() error {
	if err := c.flushBatchLocked(); err != nil {
		return err
	}
	return c.flush()
}

// beginWriteBatch makes the small frames sent from now on join the write
// batch until the read round ends. Only the connection's own read path calls
// it, so a plain load sees whether this round already did.
func (c *Conn) beginWriteBatch() {
	if !c.batching.Load() {
		c.batching.Store(true)
	}
}

// endWriteBatch closes a read round's write batch and hands it over. A lock
// holder that released during the round left the batch in place, so this
// waits for the lock, which is only ever held for one message or frame.
func (c *Conn) endWriteBatch() {
	c.batching.Store(false)
	c.writes.mu.Lock()
	c.unlockWrite()
}

// unlockWrite hands over a batch no read round will close any more, releases
// the lock, and claims a transport close that a rolled-back write completed.
func (c *Conn) unlockWrite() {
	if c.batch != nil && !c.batching.Load() {
		_ = c.flushBatchLocked()
	}
	c.writes.mu.Unlock()
	if c.writes.close.phase() == transportClosePending {
		_, _ = c.tryCloseTransport()
	}
}

// failStream aborts a connection whose byte stream can no longer be trusted,
// such as one holding part of a message or frames its senders were promised.
func (c *Conn) failStream(err error) {
	c.setCloseError(err)
	c.closing.Store(true)
	c.abortTransport(err)
}

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
	want := buffer.Len()
	// Handshake bytes participate in graceful-close ordering just like frames.
	c.writes.close.pendingBytes.Add(int64(want))
	if heartbeat := c.heartbeat; heartbeat != nil {
		heartbeat.outboundAccepted.Add(uint64(want))
	}
	n, err := c.raw.WriteOwned(buffer)
	if errors.Is(err, uio.ErrOutboundOverflow) {
		uio.ReleaseBuffer(buffer)
	}
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

// fragmentedWireSize is the wire size of payload split into frames of at most
// maxFrame bytes.
func fragmentedWireSize(payload int, maxFrame uint64, masked bool) int {
	if uint64(payload) <= maxFrame {
		return frameWireSize(payload, masked)
	}
	frameSize := int(maxFrame)
	size := payload / frameSize * frameWireSize(frameSize, masked)
	if rest := payload % frameSize; rest > 0 {
		size += frameWireSize(rest, masked)
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
// callback, which confirms bytes actually left its queue. Retiring bytes may
// also make room for a deferred Close frame.
func (c *Conn) releaseOutbound(n int) {
	if retiredBytes := c.reducePendingOutbound(n); retiredBytes != 0 && c.heartbeat != nil {
		retired := c.heartbeat.outboundRetired.Add(uint64(retiredBytes))
		c.heartbeat.markPingSent(retired)
	}
	if c.writes.close.closeIsDeferred() {
		c.sendDeferredClose()
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

// noteSendStall preserves the first send attempt the outbound limit refused
// across retries. A later accepted Ping inherits this queue deadline until
// OnOutbound proves that it was actually sent.
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
