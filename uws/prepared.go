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
	"context"
	"sync"
	"sync/atomic"

	"github.com/urpc/uio"
	"github.com/urpc/uio/uws/internal/compress"
	"github.com/urpc/uio/uws/internal/frame"
)

// PreparedMessage holds an immutable, pre-framed WebSocket message backed by
// [uio.SharedBuffer] for zero-copy fan-out across server connections.
//
// On standard server connections (uncompressed, or RFC 7692 permessage-deflate
// without context takeover), [Conn.SendPrepared] queues a read-only view of the
// pre-built wire frame directly to the transport without copying payload bytes
// or allocating per connection, lazily compressing at most once across all
// recipients. Connections that require per-connection framing — client masking,
// stateful sliding-window compression dictionaries, or fragmentation into
// smaller MaxFramePayload chunks — transparently fall back to regular sending.
type PreparedMessage struct {
	typ     MessageType
	opcode  frame.OpCode
	payload []byte           // immutable slice into rawWire after the WS header
	rawWire uio.SharedBuffer // unmasked server single frame: [header | payload]

	compDone       atomic.Bool
	compMu         sync.Mutex
	compWire       *uio.SharedBuffer
	compWireOwned  uio.SharedBuffer
	compPayloadLen int
	compErr        error

	released atomic.Bool
}

// NewPreparedMessage copies payload once into a pooled WebSocket server frame
// and freezes it into an immutable [PreparedMessage]. typ must be [TextMessage]
// or [BinaryMessage] (for text, payload must be valid UTF-8). Callers may
// immediately reuse or modify payload after NewPreparedMessage returns, and
// should call [PreparedMessage.Release] once the broadcast loop finishes.
func NewPreparedMessage(typ MessageType, payload []byte) (*PreparedMessage, error) {
	opcode := frame.Text
	if typ == BinaryMessage {
		opcode = frame.Binary
	} else if typ != TextMessage {
		return nil, frame.ErrProtocol
	}

	wireSize := frameWireSize(len(payload), false)
	buf := uio.AcquireBuffer(wireSize)
	wire := frame.Append(buf.AvailableBuffer()[:0], frame.Frame{
		Fin:     true,
		Opcode:  opcode,
		Payload: payload,
	}, [4]byte{})
	buf.CommitWrite(len(wire))

	pm := &PreparedMessage{
		typ:    typ,
		opcode: opcode,
	}
	pm.rawWire.InitOwned(buf)
	headLen := wireSize - len(payload)
	pm.payload = pm.rawWire.Bytes()[headLen:]
	return pm, nil
}

// Type returns the WebSocket message type ([TextMessage] or [BinaryMessage]).
func (m *PreparedMessage) Type() MessageType {
	if m == nil {
		return 0
	}
	return m.typ
}

// Len returns the uncompressed application payload length in bytes.
func (m *PreparedMessage) Len() int {
	if m == nil {
		return 0
	}
	return len(m.payload)
}

// Payload returns the immutable application payload slice.
func (m *PreparedMessage) Payload() []byte {
	if m == nil {
		return nil
	}
	return m.payload
}

// Release ends the broadcaster's hold on m. Any connections that already queued
// m retain their in-flight read-only views safely until their socket writes
// complete, at which point the underlying buffer returns to UIO's pool.
// Calling Release more than once panics.
func (m *PreparedMessage) Release() {
	if m == nil {
		return
	}
	if !m.released.CompareAndSwap(false, true) {
		panic("uws: PreparedMessage.Release called more than once")
	}
	m.compMu.Lock()
	comp := m.compWire
	m.compMu.Unlock()
	if comp != nil && comp != &m.rawWire {
		comp.Release()
	}
	m.rawWire.Release()
}

// wireFor selects the shared wire buffer for c, or reports ok == false when c
// requires per-connection framing (client masking, stateful compression
// context, custom <32KiB deflate window, or multi-frame fragmentation).
func (m *PreparedMessage) wireFor(c *Conn) (wire *uio.SharedBuffer, ok bool, err error) {
	if c.isClient() {
		return nil, false, nil
	}
	maxFrame := c.maxFramePayload()
	if c.compression == nil || len(m.payload) == 0 {
		if maxFrame > 0 && uint64(len(m.payload)) > maxFrame {
			return nil, false, nil
		}
		return &m.rawWire, true, nil
	}

	level, stateless := c.compression.encoder.StatelessDefaultWindow()
	if !stateless {
		return nil, false, nil
	}
	if !m.compDone.Load() {
		m.compMu.Lock()
		if m.released.Load() {
			m.compMu.Unlock()
			panic("uws: PreparedMessage used after Release")
		}
		if !m.compDone.Load() {
			enc := compress.NewEncoder(level, true)
			m.compErr = enc.EncodeBorrowed(m.payload, func(encoded []byte) error {
				if len(encoded) >= len(m.payload) {
					m.compWire = &m.rawWire
					m.compPayloadLen = len(m.payload)
					return nil
				}
				wSize := frameWireSize(len(encoded), false)
				cbuf := uio.AcquireBuffer(wSize)
				cwire := frame.Append(cbuf.AvailableBuffer()[:0], frame.Frame{
					Fin:     true,
					RSV1:    true,
					Opcode:  m.opcode,
					Payload: encoded,
				}, [4]byte{})
				cbuf.CommitWrite(len(cwire))
				m.compWireOwned.InitOwned(cbuf)
				m.compWire = &m.compWireOwned
				m.compPayloadLen = len(encoded)
				return nil
			})
			m.compDone.Store(true)
		}
		m.compMu.Unlock()
	}
	if m.compErr != nil {
		return nil, false, m.compErr
	}
	if maxFrame > 0 && uint64(m.compPayloadLen) > maxFrame {
		return nil, false, nil
	}
	return m.compWire, true, nil
}

// SendPrepared queues m to c without copying payload bytes on compatible server
// connections, and transparently falls back to per-connection framing when
// client masking, stateful compression dictionaries, or frame splitting apply.
func (c *Conn) SendPrepared(m *PreparedMessage) error {
	if m == nil {
		return frame.ErrProtocol
	}
	if m.released.Load() {
		panic("uws: PreparedMessage used after Release")
	}
	if !c.opened.Load() {
		return ErrNotReady
	}
	if c.closed.Load() || c.closing.Load() {
		return ErrClosed
	}
	if uint64(len(m.payload)) > c.maxMessageSize() {
		return frame.ErrMessageTooBig
	}

	wire, ok, err := m.wireFor(c)
	if err != nil {
		return err
	}

	c.writes.mu.Lock()
	if !ok {
		view := m.rawWire.AcquireView()
		defer uio.ReleaseBuffer(view)
		// sendLocked releases c.writes.mu via its own defer c.unlockWrite().
		return c.sendLocked(m.opcode, m.payload)
	}
	defer c.unlockWrite()
	if c.closed.Load() || c.closing.Load() {
		return ErrClosed
	}
	// Flush any earlier frames batched in this read round before checking
	// outboundRefuses or queueing wire so FIFO order and queue accounting hold.
	if err := c.flushBatchLocked(); err != nil {
		return err
	}
	if c.outboundRefuses(wire.Len()) {
		return c.errBackpressure()
	}
	n, werr := uio.WriteShared(c.raw, wire)
	return c.finishFrameWrite(m.opcode, n, wire.Len(), werr)
}

// WritePrepared sends m on a blocking connection, suspending while the outbound
// buffer is full until space drains, the connection closes, or ctx ends.
func (c *BlockingConn) WritePrepared(ctx context.Context, m *PreparedMessage) error {
	if m == nil {
		return frame.ErrProtocol
	}
	if m.released.Load() {
		panic("uws: PreparedMessage used after Release")
	}
	return c.writeBlocking(ctx, len(m.payload), func() error {
		return c.Conn.SendPrepared(m)
	})
}
