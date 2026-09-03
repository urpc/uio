package uws

import (
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/urpc/uio"
	"github.com/urpc/uio/uws/internal/compress"
	"github.com/urpc/uio/uws/internal/frame"
)

// Conn is an established or handshaking WebSocket connection.
type Conn struct {
	raw         uio.Conn
	config      *connConfig
	handler     Handler
	parser      *frame.Parser    // retained only while a frame spans input reads
	assembler   *frame.Assembler // retained only while a fragmented message is open
	compression *compressionState

	opened  atomic.Bool
	closed  atomic.Bool
	closing atomic.Bool

	userData any
	metadata atomic.Pointer[connMetadata]

	writeMu               sync.Mutex
	closeSent             bool
	handshake             atomic.Pointer[handshakeState]
	heartbeat             *heartbeatState
	transportClosePending atomic.Bool
	pendingBytes          atomic.Int64
	dispatch              *dispatchState
	closeTimer            atomic.Pointer[closeTimerState]
}

type compressionState struct {
	encoder *compress.Encoder
	decoder *compress.Decoder
}

type heartbeatState struct {
	lastPong        atomic.Int64
	pingOutstanding atomic.Bool
}

type closeTimerState struct {
	mu    sync.Mutex
	timer *time.Timer
}

type connMetadata struct {
	mu          sync.Mutex
	protocol    string
	closeErr    error
	closeCode   uint16
	closeReason string
}

type handshakeState struct {
	mu          sync.Mutex
	data        []byte
	upgrade     *httpUpgrade
	clientKey   string
	timer       *time.Timer
	contextStop func() bool
	cleanup     func()
	epoch       uint64
	expired     bool
}

// LocalAddr returns the local transport address.
func (c *Conn) LocalAddr() net.Addr { return c.raw.LocalAddr() }

// RemoteAddr returns the peer transport address.
func (c *Conn) RemoteAddr() net.Addr { return c.raw.RemoteAddr() }

// IsClosed reports whether the connection has closed.
func (c *Conn) IsClosed() bool { return c.closed.Load() }

// SetDeadline forwards the transport read and write deadline. A zero value
// clears it.
func (c *Conn) SetDeadline(deadline time.Time) error {
	if c == nil || c.raw == nil {
		return ErrNotReady
	}
	return c.raw.SetDeadline(deadline)
}

// SetReadDeadline forwards the transport read deadline. A zero value clears it.
func (c *Conn) SetReadDeadline(deadline time.Time) error {
	if c == nil || c.raw == nil {
		return ErrNotReady
	}
	return c.raw.SetReadDeadline(deadline)
}

// SetWriteDeadline forwards the transport write deadline. A zero value clears it.
func (c *Conn) SetWriteDeadline(deadline time.Time) error {
	if c == nil || c.raw == nil {
		return ErrNotReady
	}
	return c.raw.SetWriteDeadline(deadline)
}

func (c *Conn) isClient() bool { return c.config != nil && c.config.client }

func (c *Conn) frameParserConfig() *frame.ParserConfig {
	if c.config == nil {
		return nil
	}
	return &c.config.parser
}

func (c *Conn) utf8ValidationEnabled() bool {
	if c.config == nil {
		return true
	}
	return c.config.assembler.ValidateUTF8
}

// Userdata returns the user data associated with the connection. It is not
// safe for concurrent use from multiple goroutines.
func (c *Conn) Userdata() any { return c.userData }

// SetUserdata replaces the user data associated with the connection. It is not
// safe for concurrent use from multiple goroutines.
func (c *Conn) SetUserdata(value any) {
	c.userData = value
}

// Subprotocol returns the negotiated WebSocket subprotocol, or an empty string.
func (c *Conn) Subprotocol() string {
	metadata := c.metadata.Load()
	if metadata == nil {
		return ""
	}
	metadata.mu.Lock()
	defer metadata.mu.Unlock()
	return metadata.protocol
}

func (c *Conn) maxFramePayload() uint64 {
	if c.config == nil || c.config.parser.MaxFramePayload == 0 {
		return DefaultMaxFramePayload
	}
	return c.config.parser.MaxFramePayload
}

func (c *Conn) maxMessageSizeInt() int {
	maxMessage := c.maxMessageSize()
	if maxMessage > uint64(^uint(0)>>1) {
		return int(^uint(0) >> 1)
	}
	return int(maxMessage)
}

func (c *Conn) maxMessageSize() uint64 {
	if c.config == nil || c.config.assembler.MaxMessage == 0 {
		return DefaultMaxMessageSize
	}
	return c.config.assembler.MaxMessage
}

func (c *Conn) maxOutboundBytes() int {
	if c.config == nil || c.config.maxOutboundBytes == 0 {
		return DefaultMaxOutboundBytes
	}
	return c.config.maxOutboundBytes
}

func (c *Conn) closeInfo() CloseEvent {
	metadata := c.metadata.Load()
	if metadata == nil {
		return CloseEvent{}
	}
	metadata.mu.Lock()
	defer metadata.mu.Unlock()
	return CloseEvent{Code: metadata.closeCode, Reason: metadata.closeReason, Err: metadata.closeErr}
}

func (c *Conn) ensureMetadata() *connMetadata {
	for {
		if metadata := c.metadata.Load(); metadata != nil {
			return metadata
		}
		metadata := &connMetadata{}
		if c.metadata.CompareAndSwap(nil, metadata) {
			return metadata
		}
	}
}

func (c *Conn) setSubprotocol(protocol string) {
	if protocol == "" {
		return
	}
	metadata := c.ensureMetadata()
	metadata.mu.Lock()
	metadata.protocol = protocol
	metadata.mu.Unlock()
}

func (c *Conn) setCloseReason(code uint16, reason string) {
	metadata := c.ensureMetadata()
	metadata.mu.Lock()
	metadata.closeCode = code
	metadata.closeReason = reason
	metadata.mu.Unlock()
}

func (c *Conn) setCloseError(err error) {
	metadata := c.ensureMetadata()
	metadata.mu.Lock()
	if metadata.closeErr == nil {
		metadata.closeErr = err
	}
	metadata.mu.Unlock()
}
