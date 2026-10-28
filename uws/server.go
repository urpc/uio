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
	"net/http"
	"sync"
	"sync/atomic"

	"github.com/urpc/uio"
)

// Server accepts WebSocket connections over a UIO transport. Configure its
// fields before calling Serve; Serve freezes the connection configuration.
type Server struct {
	// Options configures the protocol, the transport and backpressure, and
	// carries the Handler every connection reports to.
	Options
	// CheckOrigin accepts an upgrade request. Nil accepts every origin.
	CheckOrigin func(*http.Request) bool
	// Authorize decides an upgrade request that passed validation and
	// CheckOrigin, before the server answers it. A nil error accepts it, and
	// userdata becomes the connection's Userdata from OnOpen on (`uws.Conn`
	// in the sugar; a stack's `ServerLayer` reports it through
	// uio.FindLayer instead). On the ServeHTTP path Authorize runs on the
	// http server's goroutine, before the connection is adopted. An error
	// refuses it: a *RejectError with the status, header and reason it names,
	// any other error with 403. A refused connection never reaches the
	// Handler. Authorize runs on the connection's turn, so a slow check holds
	// up that connection only; it must not block for long all the same. Nil
	// accepts every request.
	Authorize func(r *http.Request) (userdata any, err error)

	config  *connConfig
	opts    Options     // resolved snapshot, fixed at Serve
	events  *uio.Events // private transport configuration
	stats   statsCounters
	started atomic.Bool
	ready   atomic.Bool
	closeMu sync.Mutex
	closed  bool

	// startHook runs inside the transport's OnStart, after ready is set; it
	// lets tests observe or block startup on the calling goroutine.
	startHook func()
}

// NewServer returns a Server using opts. http.Server-style: the exported
// fields are the configuration, this constructor only fills them in.
func NewServer(opts Options) *Server {
	return &Server{Options: opts}
}

// Stats reports the read pausing of this server's connections.
func (s *Server) Stats() Stats { return s.stats.snapshot() }

// Serve starts the event loops and listens on each supplied address. Calling
// Serve without an address prepares the Server for use as an http.Handler.
func (s *Server) Serve(addrs ...string) error {
	opts, err := s.Options.resolve()
	if err != nil {
		return err
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
	s.opts = opts
	s.config = newServerConnConfig(opts, s.CheckOrigin, s.Authorize, &s.stats)
	events := opts.transportEvents()
	s.events = events
	handler := uio.FuncHandler{
		Start: func(*uio.Events) {
			s.closeMu.Lock()
			if !s.closed {
				s.ready.Store(true)
			}
			hook := s.startHook
			s.closeMu.Unlock()
			if hook != nil {
				hook()
			}
		},
		Open:         s.onOpen,
		Read:         s.onData,
		Close:        s.onClose,
		BytesWritten: s.onOutbound,
	}
	if s.config.observeTraffic {
		handler.BytesRead = s.onInbound
	}
	events.Handler = handler
	if opts.TLSConfig != nil {
		events.Use(serverTLSLayer(opts.TLSConfig, opts))
	}
	events.Observer, events.Hooks = handler, handler
	s.closeMu.Unlock()

	err = events.Serve(addrs...)
	s.ready.Store(false)
	_ = s.Close(err)
	return err
}

// Close permanently stops the server and requests transport shutdown. It
// returns once the shutdown is requested; a Serve running on the server
// returns after all event loops and connection tasks have exited.
func (s *Server) Close(err error) error {
	s.closeMu.Lock()
	if s.closed {
		s.closeMu.Unlock()
		return nil
	}
	s.closed = true
	s.ready.Store(false)
	events := s.events
	s.closeMu.Unlock()

	if events == nil {
		return nil
	}
	return events.Close(err)
}

func (c *Conn) expireHeartbeat() {
	if c.closed.Load() || c.closing.Load() {
		return
	}
	c.sendHeartbeatClose(1001, "heartbeat timeout")
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
		opts, _ := s.Options.resolve()
		config = newServerConnConfig(opts, s.CheckOrigin, s.Authorize, &s.stats)
	}
	conn := &Conn{
		raw:     raw,
		config:  config,
		handler: config.handler,
	}
	if config.heartbeatInterval > 0 {
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
	if err != nil && protocolCloseOwnsTransport(err) {
		// protocolClose owns the graceful transport shutdown after its Close
		// frame drains; returning the protocol error would close std transports
		// before their asynchronous writer sends it.
		return nil
	}
	return err
}

// onInbound reports transport bytes to a Handler that observes traffic.
func (s *Server) onInbound(raw uio.Conn, n int) {
	if conn, ok := raw.Userdata().(*Conn); ok && conn != nil {
		if observer, ok := conn.handler.(TrafficObserver); ok {
			observer.OnInbound(conn, n)
		}
	}
}

func (s *Server) onOutbound(raw uio.Conn, n int) {
	if conn, ok := raw.Userdata().(*Conn); ok && conn != nil {
		conn.transportSent(n)
		if conn.writes.close.closePending() {
			conn.tryCloseTransport()
		}
		conn.resumeReadsAfterDrain()
		if conn.config != nil && conn.config.observeTraffic {
			if observer, ok := conn.handler.(TrafficObserver); ok {
				observer.OnOutbound(conn, n)
			}
		}
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
	info := conn.closeInfo()
	if info.Err == nil {
		info.Err = err
	}
	conn.notifyClose(info)
}
