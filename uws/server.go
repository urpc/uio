package uws

import (
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/urpc/uio"
)

// Server accepts WebSocket connections over a UIO transport. Configure its
// fields before calling Serve; Serve freezes the connection configuration.
type Server struct {
	// Events configures the underlying UIO transport. Nil uses a default Events.
	Events *uio.Events
	// Handler receives connection lifecycle and message callbacks. It may be nil.
	Handler Handler
	// CheckOrigin accepts an upgrade request. Nil accepts every origin.
	CheckOrigin func(*http.Request) bool
	// Subprotocols lists supported protocols in server preference order.
	Subprotocols []string
	// MaxHeaderBytes bounds HTTP upgrade headers read by the native Serve path.
	// When using ServeHTTP, configure http.Server.MaxHeaderBytes instead.
	MaxHeaderBytes int
	// MaxFramePayload bounds each wire frame payload. On reads, it also bounds
	// aggregate compressed payload across a fragmented message.
	MaxFramePayload uint64
	// MaxMessageSize bounds a complete message after decompression.
	MaxMessageSize uint64
	// MaxOutboundBytes bounds accepted but unsent wire bytes per connection.
	// Zero uses DefaultMaxOutboundBytes; a negative value disables this limit.
	MaxOutboundBytes int
	// EnableCompression enables RFC 7692 permessage-deflate negotiation.
	EnableCompression bool
	// CompressionLevel selects the flate level. Zero uses default compression.
	CompressionLevel int
	// DisableUTF8Check skips Text Message UTF-8 validation. This violates RFC
	// 6455 and should only be used with trusted peers. Close reasons remain validated.
	DisableUTF8Check bool
	// CloseTimeout bounds the graceful WebSocket close handshake. A zero value
	// uses DefaultCloseTimeout.
	CloseTimeout time.Duration
	// HandshakeTimeout bounds the HTTP upgrade handshake. A zero value uses
	// DefaultHandshakeTimeout.
	HandshakeTimeout time.Duration
	// Executor dispatches OnOpen, OnMessage, and OnClose callbacks away from
	// the I/O event loop. A nil executor preserves synchronous callbacks. If
	// Submit rejects, remaining callbacks are dropped rather than run on the loop.
	Executor Executor
	// AllowCompressionContextTakeover enables RFC 7692 context takeover for
	// peers that do not request a no-context-takeover parameter. It is disabled
	// by default to bound per-connection compression state.
	AllowCompressionContextTakeover bool
	// HeartbeatInterval enables server pings when positive.
	HeartbeatInterval time.Duration
	// HeartbeatTimeout closes a connection without a pong. Zero uses twice the interval.
	HeartbeatTimeout time.Duration

	connections    sync.Map
	heartbeatStop  chan struct{}
	heartbeatMu    sync.Mutex
	dispatchBudget pendingBudget
	config         *connConfig
	started        atomic.Bool
	ready          atomic.Bool
	closeMu        sync.Mutex
	closed         bool
}

// NewServer returns a Server configured with the default limits.
func NewServer(handler Handler) *Server {
	return &Server{
		Events:           &uio.Events{},
		Handler:          handler,
		MaxHeaderBytes:   DefaultMaxHeaderBytes,
		MaxFramePayload:  DefaultMaxFramePayload,
		MaxMessageSize:   DefaultMaxMessageSize,
		MaxOutboundBytes: DefaultMaxOutboundBytes,
	}
}

// Serve starts the event loops and optionally listens on one address. Calling
// Serve without an address prepares the Server for use as an http.Handler.
func (s *Server) Serve(addrs ...string) error {
	if len(addrs) > 1 {
		return uio.ErrTooManyListenAddresses
	}
	s.closeMu.Lock()
	if s.closed {
		s.closeMu.Unlock()
		return ErrClosed
	}
	if !s.started.CompareAndSwap(false, true) {
		s.closeMu.Unlock()
		return ErrServerStarted
	}
	if s.Events == nil {
		s.Events = &uio.Events{}
	}
	events := s.Events
	if s.MaxHeaderBytes <= 0 {
		s.MaxHeaderBytes = DefaultMaxHeaderBytes
	}
	if s.MaxFramePayload == 0 {
		s.MaxFramePayload = DefaultMaxFramePayload
	}
	if s.MaxMessageSize == 0 {
		s.MaxMessageSize = DefaultMaxMessageSize
	}
	if s.MaxOutboundBytes == 0 {
		s.MaxOutboundBytes = DefaultMaxOutboundBytes
	}
	if s.EnableCompression && s.CompressionLevel == 0 {
		s.CompressionLevel = -1
	}
	s.dispatchBudget.configure(defaultMaxPendingTotalMessages, defaultMaxPendingTotalBytes)
	s.config = newServerConnConfig(s)
	configureWriteBuffer(events)
	oldOnStart := events.OnStart
	events.OnStart = func(events *uio.Events) {
		s.closeMu.Lock()
		if !s.closed {
			s.ready.Store(true)
		}
		s.closeMu.Unlock()
		if oldOnStart != nil {
			oldOnStart(events)
		}
	}
	events.OnOpen = s.onOpen
	events.OnData = s.onData
	events.OnClose = s.onClose
	events.OnOutbound = s.onOutbound
	s.startHeartbeat(s.config)
	s.closeMu.Unlock()

	err := events.Serve(addrs...)
	s.ready.Store(false)
	_ = s.Close(err)
	return err
}

// Close permanently stops the server and requests transport shutdown. Serve
// returns after all event loops and callbacks have exited.
func (s *Server) Close(err error) error {
	s.closeMu.Lock()
	if s.closed {
		s.closeMu.Unlock()
		return nil
	}
	s.closed = true
	s.ready.Store(false)
	events := s.Events
	s.closeMu.Unlock()

	s.stopHeartbeat()
	if events == nil {
		return nil
	}
	return events.Close(err)
}

func (s *Server) startHeartbeat(config *connConfig) {
	if config == nil || config.heartbeatConnections == nil {
		return
	}
	interval := config.heartbeatInterval
	timeout := config.heartbeatTimeout
	connections := config.heartbeatConnections
	stop := make(chan struct{})
	s.heartbeatMu.Lock()
	previous := s.heartbeatStop
	s.heartbeatStop = stop
	s.heartbeatMu.Unlock()
	if previous != nil {
		close(previous)
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case now := <-ticker.C:
				connections.Range(func(_, value any) bool {
					conn := value.(*Conn)
					if conn.closed.Load() {
						return true
					}
					heartbeat := conn.heartbeat
					if heartbeat == nil {
						return true
					}
					last := time.Unix(0, heartbeat.lastPong.Load())
					if heartbeat.pingOutstanding.Load() && now.Sub(last) >= timeout {
						_ = conn.Close(1001, "heartbeat timeout")
						_ = conn.closeTransport()
						return true
					}
					if heartbeat.pingOutstanding.CompareAndSwap(false, true) {
						if err := conn.Ping(nil); err != nil {
							heartbeat.pingOutstanding.Store(false)
						}
					}
					return true
				})
			case <-stop:
				return
			}
		}
	}()
}

func (s *Server) stopHeartbeat() {
	s.heartbeatMu.Lock()
	stop := s.heartbeatStop
	s.heartbeatStop = nil
	s.heartbeatMu.Unlock()
	if stop != nil {
		close(stop)
	}
}

func (s *Server) onOpen(raw uio.Conn) {
	if conn, ok := raw.Userdata().(*Conn); ok && conn != nil {
		if state := conn.handshake.Load(); state != nil && state.upgrade != nil {
			conn.raw = raw
			s.openHTTPConnection(conn, state)
			return
		}
	}
	conn := s.newConnection(raw)
	conn.handshake.Store(&handshakeState{})
	raw.SetUserdata(conn)
	conn.startHandshakeTimer(conn.config.handshakeTimeout)
}

func (s *Server) newConnection(raw uio.Conn) *Conn {
	config := s.config
	if config == nil {
		config = newServerConnConfig(s)
	}
	conn := &Conn{
		raw:     raw,
		config:  config,
		handler: config.handler,
	}
	if config.handler != nil {
		conn.dispatch = newDispatchState(config.executor, defaultMaxPendingMessages, defaultMaxPendingBytes, config.dispatchBudget)
	}
	if config.heartbeatConnections != nil {
		conn.heartbeat = &heartbeatState{}
	}
	return conn
}

func (s *Server) onData(raw uio.Conn) error {
	conn, ok := raw.Userdata().(*Conn)
	if !ok || conn == nil {
		return ErrClosed
	}
	err := conn.readAvailable()
	if err != nil && conn.closing.Load() {
		// protocolClose owns the graceful transport shutdown after its Close
		// frame drains; returning the protocol error would close std transports
		// before their asynchronous writer sends it.
		return nil
	}
	return err
}

func (s *Server) onOutbound(raw uio.Conn, n int) {
	if conn, ok := raw.Userdata().(*Conn); ok && conn != nil {
		conn.releaseOutbound(n)
		conn.tryCloseTransport()
	}
}

func (s *Server) onClose(raw uio.Conn, err error) {
	conn, ok := raw.Userdata().(*Conn)
	if !ok || conn == nil {
		return
	}
	conn.releaseParser()
	conn.releaseAssembler()
	if !conn.opened.Load() {
		conn.stopHandshakeTimer()
		if !conn.closed.CompareAndSwap(false, true) {
			return
		}
		conn.stopCloseTimer()
		return
	}
	if !conn.closed.CompareAndSwap(false, true) {
		return
	}
	conn.stopCloseTimer()
	s.connections.Delete(conn)
	info := conn.closeInfo()
	if info.Err == nil {
		info.Err = err
	}
	conn.dispatchClose(info)
}
