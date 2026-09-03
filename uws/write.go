package uws

import (
	"crypto/rand"
	"errors"
	"io"
	"net"
	"sync"
	"unicode/utf8"

	"github.com/urpc/uio"
	"github.com/urpc/uio/uws/internal/frame"
)

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
	c.writeMu.Lock()
	if c.closed.Load() {
		c.writeMu.Unlock()
		return ErrClosed
	}
	if c.closing.Load() {
		c.writeMu.Unlock()
		return nil
	}
	c.setCloseReason(code, reason)
	if err := c.sendFrameLocked(frame.Frame{Fin: true, Opcode: frame.Close, Payload: payload}); err != nil {
		c.writeMu.Unlock()
		return err
	}
	if err := c.flush(); err != nil {
		c.writeMu.Unlock()
		_ = c.raw.CloseWith(err)
		return err
	}
	c.closing.Store(true)
	c.writeMu.Unlock()
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
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
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
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
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
		if c.closeSent {
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
	if f.Masked {
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
		c.closeSent = true
	}
	return nil
}

func (c *Conn) writeTransportOwned(buffer *uio.Buffer) error {
	want := buffer.Len()
	// Handshake bytes are not subject to message backpressure, but they still
	// participate in graceful-close ordering on asynchronous transports.
	c.pendingBytes.Add(int64(want))
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
			return
		}
	}
}

func (c *Conn) flush() error {
	return c.raw.Flush()
}
