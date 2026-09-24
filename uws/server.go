package uws

import (
	"errors"
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
	// AllowCompressionContextTakeover enables RFC 7692 context takeover for
	// peers that do not request a no-context-takeover parameter. It is disabled
	// by default to bound per-connection compression state.
	AllowCompressionContextTakeover bool
	// HeartbeatInterval enables server pings when positive.
	HeartbeatInterval time.Duration
	// HeartbeatTimeout bounds both the time a ping may remain queued and the
	// time to receive its matching pong after it is written. Zero uses twice
	// the interval.
	HeartbeatTimeout time.Duration

	connections   sync.Map
	heartbeatStop chan struct{}
	heartbeatDone chan struct{}
	heartbeatMu   sync.Mutex
	config        *connConfig
	started       atomic.Bool
	ready         atomic.Bool
	closeMu       sync.Mutex
	closed        bool
}

// NewServer returns a Server configured with the default limits.
func NewServer(handler Handler) *Server {
	return &Server{
		Events:          &uio.Events{},
		Handler:         handler,
		MaxHeaderBytes:  DefaultMaxHeaderBytes,
		MaxFramePayload: DefaultMaxFramePayload,
		MaxMessageSize:  DefaultMaxMessageSize,
	}
}

// Serve starts the event loops and listens on each supplied address. Calling
// Serve without an address prepares the Server for use as an http.Handler.
func (s *Server) Serve(addrs ...string) error {
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
	if s.EnableCompression && s.CompressionLevel == 0 {
		s.CompressionLevel = -1
	}
	configureWriteBuffer(events)
	s.config = newServerConnConfig(s)
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
// returns after all event loops and connection tasks have exited.
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

// startHeartbeat replaces any prior scanner and waits for it to exit before
// publishing the new lifecycle channels. A Server normally calls it once, but
// the replacement discipline keeps tests and partial startup deterministic.
func (s *Server) startHeartbeat(config *connConfig) {
	if config == nil || config.heartbeatConnections == nil {
		return
	}
	interval := config.heartbeatInterval
	timeout := config.heartbeatTimeout
	connections := config.heartbeatConnections
	stop := make(chan struct{})
	done := make(chan struct{})
	s.heartbeatMu.Lock()
	previousStop := s.heartbeatStop
	previousDone := s.heartbeatDone
	if previousStop != nil {
		close(previousStop)
	}
	if previousDone != nil {
		<-previousDone
	}
	s.heartbeatStop = stop
	s.heartbeatDone = done
	go s.runHeartbeat(connections, interval, timeout, stop, done)
	s.heartbeatMu.Unlock()
}

func (s *Server) stopHeartbeat() {
	s.heartbeatMu.Lock()
	stop := s.heartbeatStop
	done := s.heartbeatDone
	s.heartbeatStop = nil
	s.heartbeatDone = nil
	if stop != nil {
		close(stop)
	}
	if done != nil {
		<-done
	}
	s.heartbeatMu.Unlock()
}

func (s *Server) runHeartbeat(connections *sync.Map, interval, timeout time.Duration, stop <-chan struct{}, done chan<- struct{}) {
	defer close(done)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case now := <-ticker.C:
			if !scanHeartbeat(connections, now, timeout, stop) {
				return
			}
		case <-stop:
			return
		}
	}
}

// scanHeartbeat performs only non-blocking per-connection operations. A busy
// streaming Writer starts a send-stall deadline without delaying other peers.
func scanHeartbeat(connections *sync.Map, now time.Time, timeout time.Duration, stop <-chan struct{}) bool {
	completed := true
	connections.Range(func(_, value any) bool {
		select {
		case <-stop:
			completed = false
			return false
		default:
		}
		conn := value.(*Conn)
		if conn.closed.Load() || conn.closing.Load() || conn.heartbeat == nil {
			return true
		}
		heartbeat := conn.heartbeat
		if heartbeat.expirePing(now, timeout) {
			conn.expireHeartbeat()
			return true
		}
		if heartbeat.pingOutstanding.Load() {
			return true
		}
		attempted, err := conn.tryHeartbeatPing(now)
		switch {
		case err == nil && !attempted:
			heartbeat.noteSendStall(now)
		case errors.Is(err, ErrBackpressure):
			heartbeat.noteSendStall(now)
		case err != nil:
			if !conn.closed.Load() && !conn.closing.Load() {
				conn.setCloseError(err)
				conn.closing.Store(true)
				conn.abortTransport(err)
			}
		}
		return true
	})
	return completed
}

func (c *Conn) expireHeartbeat() {
	const (
		code   = uint16(1001)
		reason = "heartbeat timeout"
	)
	if c.closed.Load() || c.closing.Load() || c.tryHeartbeatClose(code, reason) {
		return
	}
	c.setCloseReason(code, reason)
	_ = c.closeTransport()
}

// onOpen distinguishes an adopted, pre-validated net/http upgrade from a native
// socket that still needs incremental HTTP parsing.
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
	if protocolCloseOwnsTransport(err) {
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

// onClose releases incremental protocol state before publishing the terminal
// callback. closed makes duplicate transport notifications harmless.
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
	conn.notifyClose(info)
}
