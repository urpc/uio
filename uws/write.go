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

package uws

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"time"
	"unicode/utf8"

	"unsafe"

	"github.com/urpc/uio"
	_ "github.com/urpc/uio/internal/bytebuf"
	"github.com/urpc/uio/internal/plocal"
	"github.com/urpc/uio/internal/writecontrol"
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

var serverFrameScratchPool plocal.Cache[*serverFrameScratch]

func init() {
	serverFrameScratchPool.Init(func() *serverFrameScratch { return new(serverFrameScratch) })
}

func acquireServerFrameScratch() *serverFrameScratch {
	return serverFrameScratchPool.Get()
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
	if state.aux != nil {
		state.aux.payload = nil
	}
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
	state.auxLocked().payload = append([]byte(nil), payload...)
	state.flags.Store(flags | closeFrameDeferred)
	return true
}

// auxLocked returns the close's rare state, allocating it; it requires
// transitionMu.
func (state *connCloseProgress) auxLocked() *closeAux {
	if state.aux == nil {
		state.aux = &closeAux{}
	}
	return state.aux
}

// takeDeferredClose claims the deferred payload for one submission attempt.
// closeFrameDeferred stays set meanwhile, so no other Close frame can pass it.
func (state *connCloseProgress) takeDeferredClose() []byte {
	state.transitionMu.Lock()
	var deferred []byte
	if state.aux != nil {
		deferred, state.aux.payload = state.aux.payload, nil
	}
	state.transitionMu.Unlock()
	return deferred
}

// restoreDeferredClose returns a payload the outbound limit refused again,
// unless an abort discarded the deferred frame meanwhile.
func (state *connCloseProgress) restoreDeferredClose(payload []byte) {
	state.transitionMu.Lock()
	if state.flags.Load()&closeFrameDeferred != 0 {
		state.auxLocked().payload = payload
	}
	state.transitionMu.Unlock()
}

func (state *connCloseProgress) claimAbort() bool {
	state.transitionMu.Lock()
	defer state.transitionMu.Unlock()
	if state.phase() == transportCloseClaimed {
		return false
	}
	if state.aux != nil {
		state.aux.payload = nil
	}
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

// closePending reports a transport close that waits for outbound to drain.
func (state *connCloseProgress) closePending() bool {
	return state.flags.Load()&transportPhaseMask == transportClosePending<<transportPhaseShift
}

// claimPendingTransportClose claims shutdown once any deferred Close frame went
// out and the outbound queue is empty.
func (state *connCloseProgress) claimPendingTransportClose(queued int) bool {
	if state.closeIsDeferred() || queued != 0 {
		return false
	}
	state.transitionMu.Lock()
	defer state.transitionMu.Unlock()
	if state.phase() != transportClosePending || state.closeIsDeferred() || queued != 0 {
		return false
	}
	state.setPhaseLocked(transportCloseClaimed)
	return true
}

// SendText queues payload as one text message. The transport flushes accepted
// data in the connection's current or next I/O task. payload must be valid
// UTF-8: it is sent as given, and only received text is validated.
//
// payload's bytes are framed without a copy: a string converts to the
// transaction's view of the wire for free, and nothing on the send path writes
// through it.
func (c *Conn) SendText(payload string) error {
	return c.send(TextMessage, stringBytes(payload))
}

// stringBytes views s as bytes without copying. The result aliases s, so it
// must not be written through; the send path only reads it and, where it
// retains bytes past the call, copies them.
func stringBytes(s string) []byte {
	if s == "" {
		return nil
	}
	return unsafe.Slice(unsafe.StringData(s), len(s))
}

// SendBinary queues payload as one binary message. The transport flushes
// accepted data in the connection's current or next I/O task.
func (c *Conn) SendBinary(payload []byte) error {
	return c.send(BinaryMessage, payload)
}

// Send queues payload as one message of typ — TextMessage or BinaryMessage —
// framing it with that opcode. It is SendText and SendBinary under one name
// for a caller that holds bytes and a type already: text goes out without
// converting the bytes to a string first. An unknown typ reports ErrProtocol.
//
// As with SendText and SendBinary, payload is framed without being retained:
// the bytes may be reused once Send returns. A text payload must be valid
// UTF-8; it is sent as given, and only received text is validated.
func (c *Conn) Send(typ MessageType, payload []byte) error {
	return c.send(typ, payload)
}

// Ping sends a ping control frame with up to 125 bytes of payload.
func (c *Conn) Ping(payload []byte) error {
	if len(payload) > 125 {
		return frame.ErrProtocol
	}
	return c.sendFrame(frame.Frame{Fin: true, Opcode: frame.Ping, Payload: payload})
}

// sendHeartbeatPing queues a heartbeat Ping unless one is outstanding. It
// records queue time now, while markPingSent / noteLayeredProgress starts the
// Pong timeout only after transport progress proves the Ping frame left UIO's queue.
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
	ping := frame.Frame{Fin: true, Opcode: frame.Ping, Payload: payload[:]}
	// The heartbeat speaks first: its Ping is no reply, even when a pull
	// Client runs it from inside a read round.
	_, _, err := c.queueFrameLocked(ping)
	if err == nil {
		err = c.flushLocked()
	}
	if err != nil {
		heartbeat.cancelPing(nonce)
		return err
	}
	heartbeat.armPing(now, c.raw.OutboundBuffered(), c.inexactWire())
	return nil
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
	_, _, err := c.queueFrameLocked(frame.Frame{Fin: true, Opcode: frame.Close, Payload: payload})
	if err == nil {
		err = c.flushLocked()
	}
	c.markClosing()
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
// way the connection stops accepting messages, and one that arrives already
// received but not yet reported to the Handler is dropped rather than
// delivered. CloseTimeout bounds the handshake, which the connection carries
// on after Close returns.
func (c *Conn) Close(code uint16, reason string) error {
	if !c.opened.Load() {
		return c.requestClose(io.EOF)
	}
	payload, err := c.closePayload(code, reason)
	if err != nil {
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

// closePayload builds and checks the Close frame payload for code and reason.
// A Close that fails it changes nothing on the connection, which is what lets
// Client.Close run it before committing to end reading.
func (c *Conn) closePayload(code uint16, reason string) ([]byte, error) {
	if len(reason) > 123 || !utf8.ValidString(reason) {
		return nil, frame.ErrInvalidUTF8
	}
	payload := make([]byte, 2+len(reason))
	payload[0] = byte(code >> 8)
	payload[1] = byte(code)
	copy(payload[2:], reason)
	if err := frame.ValidateClosePayload(payload); err != nil {
		return nil, err
	}
	return payload, nil
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
		if deferred {
			c.round.Or(roundDeferredClose)
			if c.config != nil && c.config.heartbeatInterval <= 0 && !c.config.observeTraffic {
				c.ensureCloseWatch()
			}
		}
	}
	c.markClosing()
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
		err := c.writeFrameOwned(f, maskKey, wireSize)
		if err == nil {
			c.round.And(^roundDeferredClose)
			_ = c.flush()
			// A writer may have retired these bytes while the deferred flag
			// was still set, leaving the transport close unclaimed; see
			// claimPendingTransportClose.
			if c.writes.close.closePending() {
				_, _ = c.tryCloseTransport()
			}
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
	if c.round.Load()&roundBatching != 0 {
		if batched, err := c.batchMessageLocked(opcode, payload); batched {
			c.writes.mu.Unlock()
			return err
		}
	} else if sent, err := c.sendShortLocked(opcode, payload); sent {
		c.unlockWrite()
		return err
	}
	return c.sendLocked(opcode, payload)
}

// sendLocked is send for a message neither fast path covers. It requires
// writes.mu and releases it.
func (c *Conn) sendLocked(opcode frame.OpCode, payload []byte) error {
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

func (c *Conn) errBackpressure() error {
	if c != nil && c.config != nil {
		c.config.stats.noteBackpressure(c)
	}
	return ErrBackpressure
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
		return c.errBackpressure()
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

// sendPong answers a Ping with a Pong carrying its payload.
func (c *Conn) sendPong(payload []byte) error {
	if !c.opened.Load() {
		return ErrNotReady
	}
	c.writes.mu.Lock()
	defer c.unlockWrite()
	if _, _, err := c.queueFrameLocked(frame.Frame{Fin: true, Opcode: frame.Pong, Payload: payload}); err != nil {
		return err
	}
	return c.flushLocked()
}

// sendFrameLocked queues one frame; see queueFrameLocked. It requires writes.mu.
func (c *Conn) sendFrameLocked(f frame.Frame) error {
	_, _, err := c.queueFrameLocked(f)
	return err
}

// queueFrameLocked hands one frame to the transport, or to the read round's
// write batch, and reports its wire size and whether it was batched; a Close
// frame already queued is not queued again and reports no size. It requires
// writes.mu. Server frames use header+borrowed payload writev above the
// coalescing threshold; client frames are materialized because RFC masking
// must not mutate caller-owned payload.
func (c *Conn) queueFrameLocked(f frame.Frame) (wireSize int, batched bool, err error) {
	if c.closed.Load() || (c.closing.Load() && f.Opcode != frame.Close) {
		return 0, false, ErrClosed
	}
	if maxPayload := c.maxFramePayload(); maxPayload > 0 && uint64(len(f.Payload)) > maxPayload {
		return 0, false, frame.ErrMessageTooBig
	}
	if f.Opcode == frame.Close && c.writes.close.closeFrameQueued() {
		return 0, false, nil
	}
	var maskKey [4]byte
	if c.isClient() {
		if _, err := rand.Read(maskKey[:]); err != nil {
			return 0, false, err
		}
		f.Masked = true
	}
	wireSize = frameWireSize(len(f.Payload), f.Masked)
	threshold := c.writeBufferedThreshold()
	small := threshold > 0 && wireSize < threshold
	if small && c.round.Load()&roundBatching != 0 && c.outboundHasRoom(wireSize) {
		if err := c.batchFrame(f, maskKey, wireSize); err != nil {
			return 0, false, err
		}
		return wireSize, true, c.finishFrameWrite(f.Opcode, wireSize, wireSize, nil)
	}
	// Frames batched earlier go first, so this one cannot overtake them.
	if err := c.flushBatchLocked(); err != nil {
		return 0, false, err
	}
	if f.Masked || small {
		return wireSize, false, c.writeFrameOwned(f, maskKey, wireSize)
	}
	scratch := acquireServerFrameScratch()
	header := frame.AppendHeader(scratch.header[:0], f, maskKey)
	scratch.vec[0] = header
	scratch.vec[1] = f.Payload
	n, err := c.raw.Writev(scratch.vec[:])
	releaseServerFrameScratch(scratch)
	return wireSize, false, c.finishFrameWrite(f.Opcode, n, wireSize, err)
}

// writeFrameOwned encodes f into a buffer the transport takes over. The caller
// has already tracked wireSize. A frame the outbound limit refuses is refused
// before it is built: building copies and masks the whole payload, for
// nothing when the transport turns it away.
func (c *Conn) writeFrameOwned(f frame.Frame, maskKey [4]byte, wireSize int) error {
	if c.outboundRefuses(wireSize) {
		return c.finishFrameWrite(f.Opcode, 0, wireSize, uio.ErrOutboundOverflow)
	}
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

// refuseBeforeBuild is the frame size from which outboundRefuses looks
// before the frame is built. A smaller frame costs little to build and is left
// to the transport to refuse: a Close frame's refusal there is what its
// deferral watches for room.
const refuseBeforeBuild = 4 << 10

// outboundRefuses reports a large frame MaxOutboundBuffered turns away: one
// larger than the limit, which no queue holds, or one the queue has no room
// for once a flush sent what it could — the transport flushes before it
// refuses too. It requires writes.mu.
func (c *Conn) outboundRefuses(wireSize int) bool {
	limit := c.maxOutbound()
	if limit <= 0 || wireSize < refuseBeforeBuild {
		return false
	}
	if wireSize > limit {
		return true
	}
	if c.raw.OutboundBuffered()+wireSize <= limit {
		return false
	}
	_ = c.raw.Flush()
	return c.raw.OutboundBuffered()+wireSize > limit
}

// batchMessageLocked is send for the common message of a batching read round:
// small, uncompressed, from a server with no heartbeat or close in progress.
// It makes send's checks and appends the frame to the batch in one step. It
// reports false, having done nothing, for any message it does not cover.
// round is the round word send loaded; see countReply.
//
// The frame's bytes join the pending count when the batch is handed over, in
// one addition for the whole batch. Until then they cannot retire, and no
// graceful close can claim the transport: its Close frame is counted, and it
// either follows them in the batch or hands the batch over first.
func (c *Conn) batchMessageLocked(opcode frame.OpCode, payload []byte) (bool, error) {
	config := c.config
	if config == nil || c.heartbeat != nil || c.compression != nil ||
		c.writes.close.flags.Load() != 0 || !c.opened.Load() || c.closed.Load() || c.closing.Load() {
		return false, nil
	}
	if len(payload) > config.shortSendLimit {
		// shortSendLimit settles the next checks for the common small reply;
		// -1 for a client keeps it off this path.
		size := uint64(len(payload))
		if config.client || size > config.assembler.MaxMessage || size > config.parser.MaxFramePayload {
			return false, nil
		}
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
	dst := batch.AvailableBuffer()[:wireSize]
	// The header is written in place for the lengths a batch carries: built
	// as a frame.Frame for AppendHeader, the struct's byte stores were read
	// back as one wide load, a stalled store forward on every reply.
	switch n := len(payload); {
	case n < 126:
		dst[0], dst[1] = 0x80|byte(opcode), byte(n)
		copy(dst[2:], payload)
	case n <= 0xFFFF:
		dst[0], dst[1] = 0x80|byte(opcode), 126
		binary.BigEndian.PutUint16(dst[2:4], uint16(n))
		copy(dst[4:], payload)
	default:
		header := frame.AppendHeader(dst[:0], frame.Frame{Fin: true, Opcode: opcode, Payload: payload}, [4]byte{})
		copy(dst[len(header):], payload)
	}
	batch.CommitWrite(wireSize)
	return true, nil
}

// sendShortLocked is send for a server's common single reply: an
// uncompressed message of up to shortSendDirect bytes, a 16-bit length at
// most, with no batch to hand over first, no heartbeat and no close in
// progress. It encodes the frame header directly into a pooled buffer handed
// to WriteOwned. It reports false, having done nothing, for any message it
// does not cover, including every message of a client, whose limit is -1.
func (c *Conn) sendShortLocked(opcode frame.OpCode, payload []byte) (bool, error) {
	size := len(payload)
	if c.config == nil || size > c.config.shortSendDirect || c.heartbeat != nil || c.compression != nil || c.batch != nil ||
		c.writes.close.flags.Load() != 0 || !c.opened.Load() || c.closed.Load() || c.closing.Load() {
		return false, nil
	}
	wireSize, head := size+2, 2
	if size > 125 {
		wireSize, head = size+4, 4
	}
	if c.outboundRefuses(wireSize) {
		return true, c.errBackpressure()
	}
	if reserver, ok := c.raw.(writecontrol.TurnReserver); ok {
		if dst, reserved, err := reserver.ReserveTurnWrite(wireSize); reserved {
			if err != nil {
				return true, c.finishFrameWrite(opcode, 0, wireSize, err)
			}
			encodeServerShortHeader(dst, opcode, size, head)
			copy(dst[head:], payload)
			return true, nil
		}
	}
	owned := uio.AcquireBuffer(wireSize)
	dst := owned.AvailableBuffer()[:wireSize]
	encodeServerShortHeader(dst, opcode, size, head)
	copy(dst[head:], payload)
	owned.CommitWrite(wireSize)
	n, err := c.raw.WriteOwned(owned)
	if errors.Is(err, uio.ErrOutboundOverflow) {
		uio.ReleaseBuffer(owned)
	}
	return true, c.finishFrameWrite(opcode, n, wireSize, err)
}

func encodeServerShortHeader(dst []byte, opcode frame.OpCode, size, head int) {
	if head == 4 {
		dst[0], dst[1] = 0x80|byte(opcode), 126
		binary.BigEndian.PutUint16(dst[2:4], uint16(size))
	} else {
		dst[0], dst[1] = 0x80|byte(opcode), byte(size)
	}
}

func (c *Conn) batchBlockSize() int {
	if c.config == nil || c.config.batchBlockSize <= 0 {
		return Options{}.batchBlockSize()
	}
	return c.config.batchBlockSize
}

// outboundHasRoom reports whether wireSize more bytes fit the transport's
// outbound limit behind the bytes already queued and batched. It requires
// writes.mu: the queue only shrinks while it is held, so bytes admitted here
// are still admitted when they reach the transport.
func (c *Conn) outboundHasRoom(wireSize int) bool {
	return c.config == nil || c.config.maxOutbound <= 0 || c.outboundHasRoomWithin(c.config.maxOutbound, wireSize)
}

func (c *Conn) outboundHasRoomWithin(limit, wireSize int) bool {
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
	observe := c.config != nil && c.config.observeTraffic
	if observe {
		// Completion may run inside WriteOwned. Defer user observer delivery
		// until the handoff settles so OutboundBuffered reflects the post-handoff queue.
		c.batchHandoff.Store(-1)
	}
	n, err := c.raw.WriteOwned(batch)
	if observe {
		c.batchHandoff.Store(0)
		c.flushOutboundObservation()
	}
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

// flushAll hands any batched frames to the transport and flushes the transport.
func (c *Conn) flushAll() error {
	c.writes.mu.Lock()
	err := c.flushLocked()
	c.unlockWrite()
	return err
}

// beginWriteBatch makes the small frames sent from now on join the write
// batch until the read round ends. Atomic Or preserves concurrent flags such as
// roundDeferredClose set by Conn.Close.
func (c *Conn) beginWriteBatch() {
	if c.round.Load()&roundBatching == 0 {
		c.round.Or(roundBatching)
	}
}

// endRound clears roundBatching as the read round ends — later sends are no
// longer its replies — and, when the round batched, hands its batch over.
func (c *Conn) endRound(round uint32) {
	c.round.And(^roundBatching)
	if round&roundBatching != 0 {
		c.writes.mu.Lock()
		c.unlockWrite()
	}
}

// unlockWrite hands over a batch no read round will close any more, releases
// the lock, and claims a transport close that a rolled-back write completed.
func (c *Conn) unlockWrite() {
	if c.batch != nil && c.round.Load()&roundBatching == 0 {
		_ = c.flushBatchLocked()
	}
	c.writes.mu.Unlock()
	if c.writes.close.closePending() {
		_, _ = c.tryCloseTransport()
	}
}

// failStream aborts a connection whose byte stream can no longer be trusted,
// such as one holding part of a message or frames its senders were promised.
func (c *Conn) failStream(err error) {
	c.setCloseError(err)
	c.markClosing()
	c.abortTransport(err)
}

// finishFrameWrite reconciles progress accounting with a rejected or partial
// transport write and translates UIO's connection-level limit to the UWS API.
func (c *Conn) finishFrameWrite(opcode frame.OpCode, n, want int, err error) error {
	if err != nil {
		if errors.Is(err, uio.ErrOutboundOverflow) {
			return c.errBackpressure()
		}
		// Any other failure leaves the transport closing.
		if errors.Is(err, net.ErrClosed) {
			return ErrClosed
		}
		return err
	}
	if n != want {
		return io.ErrShortWrite
	}
	if opcode == frame.Close {
		c.writes.close.markCloseFrameSent()
	}
	return nil
}

func (c *Conn) writeTransportOwned(buffer *uio.Buffer) error {
	want := buffer.Len()
	n, err := c.raw.WriteOwned(buffer)
	if errors.Is(err, uio.ErrOutboundOverflow) {
		uio.ReleaseBuffer(buffer)
	}
	if err != nil {
		return err
	}
	if n != want {
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

// transportSent is the transport's report that n bytes left its queue: it
// advances Ping send progress and retries any Close frame waiting for room.
func (c *Conn) transportSent(n int) {
	if c.heartbeat != nil {
		queued := 0
		if c.raw != nil {
			queued = c.raw.OutboundBuffered()
		}
		c.heartbeat.noteSendProgress(n, queued)
	}
	if c.writes.close.closeIsDeferred() {
		if _, asLayer := c.handler.(layerSink); asLayer {
			_ = c.raw.Wake()
		} else {
			c.sendDeferredClose()
		}
	}
}

func (heartbeat *heartbeatState) armPing(now time.Time, queued int, waitEmpty bool) {
	heartbeat.mu.Lock()
	if heartbeat.pingOutstanding.Load() && heartbeat.pingSentAt == 0 {
		if queued <= 0 {
			heartbeat.pingSentAt = now.UnixNano()
			heartbeat.sendStalledAt = 0
		} else {
			heartbeat.pingWireWait = int64(queued)
			heartbeat.pingWaitEmpty = waitEmpty
		}
	}
	heartbeat.mu.Unlock()
}

func (heartbeat *heartbeatState) noteSendProgress(sent, queued int) {
	if !heartbeat.pingOutstanding.Load() {
		return
	}
	heartbeat.mu.Lock()
	if heartbeat.pingOutstanding.Load() && heartbeat.pingSentAt == 0 {
		if sent > 0 && !heartbeat.pingWaitEmpty && heartbeat.pingWireWait > 0 {
			heartbeat.pingWireWait -= int64(sent)
		}
		if queued == 0 || (!heartbeat.pingWaitEmpty && heartbeat.pingWireWait <= 0) {
			heartbeat.pingSentAt = time.Now().UnixNano()
			heartbeat.sendStalledAt = 0
			heartbeat.pingWireWait = 0
		}
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
	heartbeat.pingWireWait = 0
	heartbeat.pingWaitEmpty = false
	heartbeat.pingOutstanding.Store(true)
	heartbeat.deferredAt = heartbeat.parsed.Load() // progress is measured from the Ping
	return true
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
// transport progress proves that it was actually sent.
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

// deferExpiry reports that a sent Ping's run-out deadline should wait: a
// Pong may be in input not yet parsed — waiting now, or behind a frame
// budget whose rounds yield to it and deliver frames as they come. The wait
// is bounded by one further timeout past the deadline, and a wait without
// waiting input needs parse progress since the last one, so a peer that
// streams on without answering, and input nobody parses (a paused
// connection's, a stopped peer's), still end the connection. A refused
// send's stall has no Pong to wait for and no deferral.
func (heartbeat *heartbeatState) deferExpiry(now time.Time, timeout time.Duration, inputWaiting bool) bool {
	heartbeat.mu.Lock()
	defer heartbeat.mu.Unlock()
	if !heartbeat.pingOutstanding.Load() || heartbeat.pingSentAt == 0 {
		return false
	}
	deadline := heartbeat.expiryLocked(timeout)
	if deadline.IsZero() || now.Before(deadline) || !now.Before(deadline.Add(timeout)) {
		return false
	}
	if inputWaiting {
		heartbeat.deferredAt = heartbeat.parsed.Load()
		return true
	}
	if parsed := heartbeat.parsed.Load(); parsed != heartbeat.deferredAt {
		heartbeat.deferredAt = parsed
		return true
	}
	return false
}

// expiry is the moment the current ping state ends at: the timeout past a
// queued or sent Ping, or past the first send the outbound limit refused.
func (heartbeat *heartbeatState) expiry(now time.Time, timeout time.Duration) time.Time {
	heartbeat.mu.Lock()
	defer heartbeat.mu.Unlock()
	return heartbeat.expiryLocked(timeout)
}

// expiryLocked is expiry with the mutex held.
func (heartbeat *heartbeatState) expiryLocked(timeout time.Duration) time.Time {
	started := int64(0)
	if heartbeat.pingOutstanding.Load() {
		started = heartbeat.pingQueuedAt
		if heartbeat.pingSentAt != 0 {
			started = heartbeat.pingSentAt
		}
	} else if heartbeat.sendStalledAt != 0 {
		started = heartbeat.sendStalledAt
	}
	if started == 0 {
		return time.Time{}
	}
	return time.Unix(0, started).Add(timeout)
}

// clearPingLocked only cancels the current Ping attempt. A failed enqueue must
// retain sendStalledAt so the next attempt cannot restart the send deadline.
func (heartbeat *heartbeatState) clearPingLocked() {
	heartbeat.pingOutstanding.Store(false)
	heartbeat.pingQueuedAt = 0
	heartbeat.pingSentAt = 0
	heartbeat.pingNonce = 0
	heartbeat.pingWireWait = 0
	heartbeat.pingWaitEmpty = false
}

func (c *Conn) flush() error {
	return c.raw.Flush()
}
