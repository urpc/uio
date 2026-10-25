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

// Package uws provides an asynchronous WebSocket server and client built on
// UIO.
package uws

import (
	"errors"
	"time"
	"unsafe"

	"github.com/urpc/uio/uws/internal/frame"
	"github.com/urpc/uio/uws/internal/handshake"
)

// MessageType identifies the payload format of a WebSocket message.
type MessageType uint8

const (
	// TextMessage identifies a UTF-8 text message.
	TextMessage MessageType = 1
	// BinaryMessage identifies a binary message.
	BinaryMessage MessageType = 2
)

const (
	// DefaultMaxHeaderBytes is the default HTTP upgrade header limit.
	DefaultMaxHeaderBytes = handshake.DefaultMaxBytes
	// DefaultMaxFramePayload is the default wire payload limit for one frame.
	DefaultMaxFramePayload = 16 << 20
	// DefaultMaxMessageSize is the default decompressed message size limit.
	DefaultMaxMessageSize = 64 << 20
	// DefaultCloseTimeout is the default graceful close handshake timeout.
	DefaultCloseTimeout = 5 * time.Second
	// DefaultHandshakeTimeout is the default HTTP upgrade timeout.
	DefaultHandshakeTimeout = 10 * time.Second

	// defaultWriteBufferedThreshold lets UIO coalesce small WebSocket frames in
	// the connection task's outbound buffer.
	defaultWriteBufferedThreshold = 4 << 10
	// A block of a read round's write batch holds one read's replies, which the
	// write batch hands over once that read is done: an echo's replies are no
	// longer than the frames they answer, so they fill it, and the transport
	// queues a full block as its own segment where it copies a half-empty one
	// (see uio's coalescing of owned writes).
	minWriteBatchBlock = 4 << 10
	maxWriteBatchBlock = 64 << 10
	// minFramesPerDataEvent is the floor of the per-chunk frame budget: a busy
	// connection cannot starve others, but the budget always covers every
	// frame the smallest read can carry, since stopping inside a chunk costs a
	// whole extra connection turn. Options raises it with ReadBufferSize; see
	// connConfig.maxFramesPerDataEvent.
	minFramesPerDataEvent = 1024
)

var (
	// ErrClosed reports an operation on a closed connection.
	ErrClosed = errors.New("uws: connection closed")
	// ErrNotReady reports an operation attempted before the handshake completes.
	ErrNotReady = errors.New("uws: handshake not complete")
	// ErrBackpressure reports that the UIO connection outbound queue is full.
	ErrBackpressure = errors.New("uws: outbound queue is full")
	// ErrServerStarted reports a second attempt to run a Server listener after
	// its transport has already started.
	ErrServerStarted = errors.New("uws: server already started")
	// ErrProtocol reports a WebSocket protocol violation.
	ErrProtocol   = frame.ErrProtocol
	errReadBudget = errors.New("uws: read budget exhausted")
)

// Message is one complete text or binary WebSocket message.
type Message struct {
	// Type identifies whether Payload contains text or binary data.
	Type MessageType
	// Payload is valid until OnMessage returns. Call Clone before retaining it
	// or handing it to another goroutine.
	Payload []byte
}

// Clone returns an independently owned copy of m.
func (m Message) Clone() Message {
	m.Payload = append([]byte(nil), m.Payload...)
	return m
}

// Text returns Payload as a string without copying, for the common echo —
// handing a text message straight back to SendText. Like Payload, it is valid
// only until OnMessage returns; call Clone to keep either past it.
func (m Message) Text() string {
	if len(m.Payload) == 0 {
		return ""
	}
	return unsafe.String(unsafe.SliceData(m.Payload), len(m.Payload))
}

// Bytes returns Payload, the counterpart of Text for a binary message handed
// straight back to SendBinary. It is Payload itself, valid on the same terms:
// only until OnMessage returns unless cloned.
func (m Message) Bytes() []byte {
	return m.Payload
}

// CloseEvent describes why a WebSocket connection closed.
type CloseEvent struct {
	// Code is the peer or locally generated WebSocket close status code.
	Code uint16
	// Reason is the optional close reason sent with Code.
	Reason string
	// Err is the transport, protocol, or application error that caused closure.
	Err error
}

// Handler receives ordered lifecycle and message callbacks for a connection.
type Handler interface {
	// OnOpen is called after a successful WebSocket handshake.
	OnOpen(*Conn)
	// OnMessage receives complete messages in wire order.
	OnMessage(*Conn, Message)
	// OnClose is called once after the transport closes. A client handshake
	// failure may call it without a preceding OnOpen.
	OnClose(*Conn, CloseEvent)
}
