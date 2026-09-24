// Package uws provides an asynchronous WebSocket server built on UIO.
package uws

import (
	"errors"
	"time"

	"github.com/urpc/uio"
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
	// maxFramesPerDataEvent bounds frame callbacks handled in one connection
	// task turn so a busy connection cannot starve other runnable connections.
	maxFramesPerDataEvent = 64
)

var (
	// ErrClosed reports an operation on a closed connection.
	ErrClosed = errors.New("uws: connection closed")
	// ErrNotReady reports an operation attempted before the handshake completes.
	ErrNotReady = errors.New("uws: handshake not complete")
	// ErrBackpressure reports that the UIO connection outbound queue is full.
	ErrBackpressure = errors.New("uws: outbound queue is full")
	// ErrWriteBusy reports that a streaming Writer currently owns the
	// connection's write path.
	ErrWriteBusy = errors.New("uws: connection writer is busy")
	// ErrWriterClosed reports an operation on a closed message writer.
	ErrWriterClosed = errors.New("uws: message writer is closed")
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

func configureWriteBuffer(events *uio.Events) {
	if events.WriteBufferedThreshold == 0 {
		events.WriteBufferedThreshold = defaultWriteBufferedThreshold
	}
}

func effectiveWriteBufferedThreshold(events *uio.Events) int {
	if events == nil || events.WriteBufferedThreshold == 0 {
		return defaultWriteBufferedThreshold
	}
	return events.WriteBufferedThreshold
}
