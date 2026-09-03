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
	// DefaultMaxOutboundBytes is the default accepted but unsent wire byte
	// limit. It includes room for the largest legal frame header so a maximum
	// sized default frame is not rejected before reaching the transport.
	DefaultMaxOutboundBytes = DefaultMaxFramePayload + 14
	// DefaultCloseTimeout is the default graceful close handshake timeout.
	DefaultCloseTimeout = 5 * time.Second
	// DefaultHandshakeTimeout is the default HTTP upgrade timeout.
	DefaultHandshakeTimeout = 10 * time.Second

	// defaultMaxPendingMessages limits queued OnMessage callbacks for one
	// connection when an Executor is configured.
	defaultMaxPendingMessages = 16 << 10
	// defaultMaxPendingBytes limits copied message payload queued for one
	// connection when an Executor is configured.
	defaultMaxPendingBytes = 64 << 20
	// defaultMaxPendingTotalMessages limits queued OnMessage callbacks across
	// one Server or Dialer when an Executor is configured.
	defaultMaxPendingTotalMessages int64 = 1 << 20
	// defaultMaxPendingTotalBytes limits copied message payload queued across
	// one Server or Dialer when an Executor is configured.
	defaultMaxPendingTotalBytes int64 = 4 << 30
	// defaultWriteBufferedThreshold enables direct callback writes up to this
	// size before UIO falls back to its asynchronous write path.
	defaultWriteBufferedThreshold = 4 << 10
	// maxFramesPerDataEvent bounds frame callbacks handled in one I/O turn so
	// a busy connection cannot starve other connections on the same loop.
	maxFramesPerDataEvent = 64
)

var (
	// ErrClosed reports an operation on a closed connection.
	ErrClosed = errors.New("uws: connection closed")
	// ErrNotReady reports an operation attempted before the handshake completes.
	ErrNotReady = errors.New("uws: handshake not complete")
	// ErrBackpressure reports that the transport outbound queue is full.
	ErrBackpressure = errors.New("uws: outbound queue is full")
	// ErrApplicationBackpressure reports that the Executor mailbox is full.
	ErrApplicationBackpressure = errors.New("uws: application queue is full")
	// ErrExecutorRejected reports that Executor.Submit rejected a callback.
	ErrExecutorRejected = errors.New("uws: executor rejected callback")
	// ErrWriterClosed reports an operation on a closed message writer.
	ErrWriterClosed = errors.New("uws: message writer is closed")
	// ErrServerStarted reports a second attempt to run a Server listener after
	// its transport has already started.
	ErrServerStarted = errors.New("uws: server already started")
	// ErrProtocol reports a WebSocket protocol violation.
	ErrProtocol   = frame.ErrProtocol
	errReadBudget = errors.New("uws: read budget exhausted")
	errReadPaused = errors.New("uws: reads paused")
)

// Executor runs application callbacks outside the I/O event loop. Submit must
// return promptly and reports false when a bounded worker queue is full.
type Executor interface {
	// Submit schedules callback and reports whether it was accepted.
	Submit(func()) bool
}

type readPauseProbe interface {
	IsReadPaused() bool
}

func isReadPaused(raw uio.Conn) bool {
	probe, ok := raw.(readPauseProbe)
	return ok && probe.IsReadPaused()
}

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
	// failure may call it without a preceding OnOpen. If Executor rejects
	// dispatch, pending callbacks, including OnClose, may be dropped.
	OnClose(*Conn, CloseEvent)
}

func configureWriteBuffer(events *uio.Events) {
	if events.WriteBufferedThreshold == 0 {
		events.WriteBufferedThreshold = defaultWriteBufferedThreshold
	}
}
