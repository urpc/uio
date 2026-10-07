package uws

import (
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/urpc/uio"
	"github.com/urpc/uio/uws/internal/compress"
	"github.com/urpc/uio/uws/internal/frame"
	"github.com/urpc/uio/uws/internal/handshake"
)

// Conn is an established or handshaking WebSocket connection. UIO serializes
// protocol and handler callbacks per connection; atomics here coordinate
// external writers, timers, heartbeat scanning, and transport callbacks. The
// fields every message touches come first, so they share as few cache lines
// as the struct's size class allows.
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

	readFrames int32       // frames delivered in the current read round
	batching   atomic.Bool // a read round is batching the small frames it sends

	batch     *uio.Buffer // small frames accepted but not yet handed over; guarded by writes.mu
	heartbeat *heartbeatState
	writes    connWriteState

	userData   any
	metadata   atomic.Pointer[connMetadata]
	handshake  atomic.Pointer[handshakeState]
	closeTimer atomic.Pointer[closeTimerState]
}

// connWriteState serializes frame construction and submission. mu is held only
// while one message or control frame is encoded and handed to UIO, never
// across application code, so every sender may wait for it. close tracks
// progress that UIO's outbound callback updates without taking mu.
type connWriteState struct {
	mu    sync.Mutex
	close connCloseProgress
}

// These fields represent independent obligations rather than one phase: a
// Close frame can wait for outbound room while earlier bytes are still
// retiring. transitionMu only protects short state transitions; it never
// covers socket I/O or application work.
type connCloseProgress struct {
	pendingBytes  atomic.Int64
	flags         atomic.Uint32
	transitionMu  sync.Mutex
	deferredClose *deferredCloseFrame // guarded by transitionMu
}

// deferredCloseFrame owns the payload of a Close frame the outbound limit had
// no room for, until the transport drains enough to take it.
type deferredCloseFrame struct{ payload []byte }

const (
	transportCloseIdle uint32 = iota
	transportClosePending
	transportCloseClaimed
)

const (
	closeFrameSent uint32 = 1 << iota
	// closeFrameDeferred marks a Close frame that waits for outbound room,
	// from the refusal until the frame is accepted or the transport aborts.
	closeFrameDeferred
)

const (
	transportPhaseShift = 2
	transportPhaseMask  = uint32(3 << transportPhaseShift)
)

// compressionState holds negotiated direction-specific RFC 7692 contexts.
type compressionState struct {
	encoder *compress.Encoder
	decoder *compress.Decoder
}

// heartbeatState distinguishes a Ping waiting to be accepted, queued for
// transport, and actually sent. The first failed enqueue keeps its deadline
// across retries; byte positions mark when the Ping leaves the transport queue.
// Every submission holds the connection's write lock, so accepted positions
// follow the transport's order.
type heartbeatState struct {
	mu sync.Mutex
	// sendStalledAt remains set across backpressure retries.
	sendStalledAt int64
	pingQueuedAt  int64
	pingSentAt    int64
	pingNonce     uint64
	pingTarget    uint64
	// Accepted and retired are monotonic FIFO byte positions. The ping target
	// marks when its complete frame has left the transport queue without
	// requiring the connection's later writes to drain first.
	outboundAccepted atomic.Uint64
	outboundRetired  atomic.Uint64
	pingOutstanding  atomic.Bool
}

// closeTimerState protects lazy creation, replacement, and cancellation of the
// graceful-close deadline from callback and timer goroutines.
type closeTimerState struct {
	mu    sync.Mutex
	shard uint8 // deadline shard the close timeout lives in
}

// connMetadata groups infrequently used negotiated and close information so a
// normal connection does not pay for separate synchronization fields.
type connMetadata struct {
	mu          sync.Mutex
	protocol    string
	closeErr    error
	closeCode   uint16
	closeReason string
}

// handshakeState contains every resource whose lifetime ends at handshake
// completion. notifyOpen releases the entire object after OnOpen returns, which
// also bounds the lifetime of an adopted net/http request and of a native
// upgrade request a handler may ask for during OnOpen.
type handshakeState struct {
	mu          sync.Mutex
	data        []byte
	upgrade     *httpUpgrade
	request     *handshake.Request // native upgrade request, exposed through Request()
	clientKey   string
	contextStop func() bool
	cleanup     func()
	epoch       uint64
	shard       uint8 // deadline shard the handshake timeout lives in
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

// SetNoDelay controls whether the underlying TCP connection uses Nagle's
// algorithm.
func (c *Conn) SetNoDelay(noDelay bool) error {
	if c == nil || c.raw == nil {
		return ErrNotReady
	}
	return c.raw.SetNoDelay(noDelay)
}

// Request returns the HTTP upgrade request during OnOpen for server
// connections, whether accepted by Server.Serve or adopted through
// Server.ServeHTTP. It returns nil afterward — handlers that need it beyond
// OnOpen must keep it — and for client connections. The request is read-only.
func (c *Conn) Request() *http.Request {
	if c == nil {
		return nil
	}
	return handshakeHTTPRequest(c.handshake.Load())
}

func handshakeHTTPRequest(state *handshakeState) *http.Request {
	if state == nil {
		return nil
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.upgrade != nil {
		return state.upgrade.request.HTTP
	}
	if state.request == nil {
		return nil
	}
	if state.request.HTTP == nil {
		// The native path parsed the request without net/http; build it from
		// the retained header block only when a handler asks for it.
		if request, err := state.request.BuildHTTP(); err == nil {
			state.request.HTTP = request
		}
	}
	return state.request.HTTP
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

func (c *Conn) maxOutbound() int {
	if c.config == nil {
		return 0
	}
	return c.config.maxOutbound
}

func (c *Conn) writeBufferedThreshold() int {
	if c.config == nil || c.config.writeBufferedThreshold == 0 {
		return defaultWriteBufferedThreshold
	}
	return c.config.writeBufferedThreshold
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
