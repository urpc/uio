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

package umux

import (
	"context"
	"io"
	"sync"

	"github.com/urpc/uio"
)

// Server accepts transport connections, one Session each: every inbound
// connection becomes a multiplexed transport whose streams reach Config.Handler
// or the Session's Accept queue.
type Server struct {
	cfg            Config
	router         *router
	events         *uio.Events
	once           sync.Once
	serveErr       error
	serveDone      chan struct{}
	session        chan *Session
	announceMu     sync.Mutex
	sessionsClosed bool
}

// NewServer builds the transport handler. Config.Handler receives stream
// callbacks; without it, sessions are handed to Sessions and streams to
// Session.Accept.
func NewServer(cfg Config) *Server {
	cfg = cfg.resolve()
	s := &Server{cfg: cfg, session: make(chan *Session, 64), serveDone: make(chan struct{})}
	s.router = &router{
		cfg:      cfg,
		started:  make(chan struct{}),
		announce: s.publishSession,
	}
	s.events = &uio.Events{Handler: s.router, Observer: s.router, Hooks: s.router, MaxBufferSize: cfg.ReadBufferSize, MaxInboundBuffered: transportReadLimit(cfg)}
	return s
}

// Sessions reports accepted transports and closes when the server stops. The
// queue holds 64 sessions; with a Handler, an undelivered session continues
// through its stream callbacks. Without a Handler, a full queue refuses the
// transport so its peer does not wait on an unreachable accept queue.
func (s *Server) Sessions() <-chan *Session { return s.session }

func (s *Server) publishSession(sess *Session) {
	s.announceMu.Lock()
	if s.sessionsClosed {
		s.announceMu.Unlock()
		_ = sess.Close(ErrClosed)
		return
	}
	delivered := false
	select {
	case s.session <- sess:
		delivered = true
	default:
	}
	s.announceMu.Unlock()
	if !delivered && s.cfg.Handler == nil {
		_ = sess.Close(ErrMaxStreams)
	}
}

func (s *Server) closeSessions() {
	s.announceMu.Lock()
	if !s.sessionsClosed {
		s.sessionsClosed = true
		close(s.session)
	}
	s.announceMu.Unlock()
}

// Serve listens on addrs and blocks until the events close, like
// uws.Server.Serve.
func (s *Server) Serve(addrs ...string) error {
	owner := false
	s.once.Do(func() { owner = true })
	if !owner {
		return uio.ErrEventsStarted
	}
	return s.serve(addrs...)
}

func (s *Server) serve(addrs ...string) error {
	s.serveErr = s.events.Serve(addrs...)
	s.closeSessions()
	close(s.serveDone)
	return s.serveErr
}

// Listen adds one listener to the transport events, starting them first.
func (s *Server) Listen(addr string, opts ...uio.ListenOption) (uio.Listener, error) {
	if err := s.start(); err != nil {
		return nil, err
	}
	return s.events.Listen(addr, opts...)
}

// Close requests shutdown of the transport events and every session with cause.
// Use Wait, or the return of Serve, to wait for all callbacks to finish.
func (s *Server) Close(cause error) error {
	s.closeSessions()
	return s.events.Close(cause)
}

// Wait joins transport shutdown. Do not call it from a stream callback.
func (s *Server) Wait() error { return s.events.Wait() }

// start runs the transport events and waits for their start hook. A Serve
// that ends before the hook reports its error instead of waiting forever.
func (s *Server) start() error {
	s.once.Do(func() {
		go func() { _ = s.serve() }()
	})
	select {
	case <-s.router.started:
		return nil
	case <-s.serveDone:
		if s.serveErr != nil {
			return s.serveErr
		}
		return ErrClosed
	}
}

// Client dials transport connections, one Session each.
type Client struct {
	cfg       Config
	router    *router
	events    *uio.Events
	once      sync.Once
	serveErr  error
	serveDone chan struct{}
}

// NewClient builds the transport handler.
func NewClient(cfg Config) *Client {
	cfg = cfg.resolve()
	c := &Client{cfg: cfg, serveDone: make(chan struct{})}
	c.router = &router{cfg: cfg, client: true, started: make(chan struct{})}
	c.events = &uio.Events{Handler: c.router, Observer: c.router, Hooks: c.router, MaxBufferSize: cfg.ReadBufferSize, MaxInboundBuffered: transportReadLimit(cfg)}
	return c
}

// transportReadLimit leaves room to assemble one complete legal data frame.
// SessionWindow still bounds decoded payload held by streams; bytes awaiting
// a complete frame are a separate transport budget. A small session budget
// must not stop the transport halfway through a frame the parser cannot yet
// consume. Events may exceed this limit by one socket read.
func transportReadLimit(cfg Config) int {
	limit := bufferLimitSum(cfg.SessionWindow, cfg.ReadBufferSize)
	if frame := bufferLimitSum(cfg.WindowSize, headerSize); limit < frame {
		limit = frame
	}
	// A metadata SYN is a single frame larger than any data frame, and
	// readFrame parses nothing until the frame lies whole in the buffer: the
	// transport must be able to hold it, or a small session budget would
	// stall the read forever.
	if meta := bufferLimitSum(cfg.MaxMetadata, headerSize+4); limit < meta {
		limit = meta
	}
	return limit
}

// Config.resolve makes both terms positive. Saturate rather than wrapping a
// large configured budget into zero or a negative, which disables the bound.
func bufferLimitSum(a, b int) int {
	maxInt := int(^uint(0) >> 1)
	if a > maxInt-b {
		return maxInt
	}
	return a + b
}

// Dial opens a transport to addr and returns its Session. Dial options travel
// to the transport dial, so layer stacks (utls, ukcp) apply here.
func (c *Client) Dial(ctx context.Context, addr string, opts ...uio.DialOption) (*Session, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := c.start(); err != nil {
		return nil, err
	}
	conn, err := c.events.DialContext(ctx, addr, opts...)
	if err != nil {
		return nil, err
	}
	return c.router.waitSession(ctx, conn)
}

// Close requests shutdown of the transport events and every session with cause.
// Use Wait to wait for all callbacks to finish.
func (c *Client) Close(cause error) error { return c.events.Close(cause) }

// Wait joins transport shutdown. Do not call it from a stream callback.
func (c *Client) Wait() error { return c.events.Wait() }

// start runs the transport events and waits for their start hook, reporting a
// Serve that ended first.
func (c *Client) start() error {
	c.once.Do(func() {
		go func() {
			c.serveErr = c.events.Serve()
			close(c.serveDone)
		}()
	})
	select {
	case <-c.router.started:
		return nil
	case <-c.serveDone:
		if c.serveErr != nil {
			return c.serveErr
		}
		return ErrClosed
	}
}

// router routes one Events' transport connections to their sessions. It is
// the Handler, Observer and ServeHooks of the transport Events.
type router struct {
	cfg       Config
	client    bool
	announce  func(*Session)
	started   chan struct{}
	startOnce sync.Once

	mu      sync.Mutex
	conns   map[uio.Conn]*Session
	waiting map[uio.Conn]chan sessionResult
}

// sessionResult settles a dial's rendezvous with the transport callbacks.
// Only the callback that removes the waiter may deliver its result.
type sessionResult struct {
	session *Session
	err     error
}

// OnOpen builds the session for a transport connection.
func (r *router) OnOpen(c uio.Conn) {
	sess := newSession(c, r.cfg, r.client)
	r.mu.Lock()
	r.ensureMapsLocked()
	r.conns[c] = sess
	ch := r.waiting[c]
	delete(r.waiting, c)
	r.mu.Unlock()
	if ch != nil {
		ch <- sessionResult{session: sess}
	}
	if r.announce != nil {
		r.announce(sess)
	}
}

// OnRead dispatches the transport's inbound bytes to its session.
func (r *router) OnRead(c uio.Conn) error {
	if sess := r.sessionOf(c); sess != nil {
		return sess.onRead(c)
	}
	return nil
}

// OnTimer dispatches the transport's wake timer to its session.
func (r *router) OnTimer(c uio.Conn) error {
	if sess := r.sessionOf(c); sess != nil {
		return sess.onTimer(c)
	}
	return nil
}

// OnClose closes the transport's session.
func (r *router) OnClose(c uio.Conn, err error) {
	r.mu.Lock()
	sess := r.conns[c]
	delete(r.conns, c)
	ch := r.waiting[c]
	delete(r.waiting, c)
	r.mu.Unlock()
	if sess != nil {
		sess.onTransportClose(err)
	}
	if ch != nil {
		if err == nil {
			err = io.EOF
		}
		ch <- sessionResult{err: err}
	}
}

// ensureMapsLocked creates the routing maps on first use.
func (r *router) ensureMapsLocked() {
	if r.conns == nil {
		r.conns = make(map[uio.Conn]*Session)
		r.waiting = make(map[uio.Conn]chan sessionResult)
	}
}

func (r *router) sessionOf(c uio.Conn) *Session {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.conns[c]
}

// OnBytesRead carries the transport's read observation: streams report their
// own bytes, so only a configured Observer hears these.
func (r *router) OnBytesRead(c uio.Conn, n int) {
	if obs := r.cfg.Observer; obs != nil {
		obs.OnBytesRead(c, n)
	}
}

// OnBytesWritten retries frames a full transport refused, then reports.
func (r *router) OnBytesWritten(c uio.Conn, n int) {
	if sess := r.sessionOf(c); sess != nil {
		sess.onTransportWritable()
	}
	if obs := r.cfg.Observer; obs != nil {
		obs.OnBytesWritten(c, n)
	}
}

// OnStart marks the events serving; Dial and Listen wait for it.
func (r *router) OnStart(ev *uio.Events) {
	r.startOnce.Do(func() { close(r.started) })
}

// OnStop is a no-op for the transport.
func (r *router) OnStop(ev *uio.Events) {}

// waitSession pairs a freshly dialed transport with the session its OnOpen
// created.
func (r *router) waitSession(ctx context.Context, conn uio.Conn) (*Session, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	r.mu.Lock()
	r.ensureMapsLocked()
	if sess := r.conns[conn]; sess != nil {
		r.mu.Unlock()
		return sess, nil
	}
	ch := make(chan sessionResult, 1)
	r.waiting[conn] = ch
	r.mu.Unlock()

	// Register before probing the connection: a close that follows the probe
	// settles ch in OnClose, while a close that already finished makes Wake
	// return its terminal error. No closed-connection tombstones are needed.
	// Wake touches no inbound data and costs one empty turn only when OnOpen
	// has not yet reached this rendezvous.
	if err := conn.Wake(); err != nil {
		r.mu.Lock()
		if r.waiting[conn] == ch {
			delete(r.waiting, conn)
		}
		r.mu.Unlock()
		_ = conn.CloseWith(err)
		return nil, err
	}
	select {
	case result := <-ch:
		return result.session, result.err
	case <-ctx.Done():
		r.mu.Lock()
		if r.waiting[conn] == ch {
			delete(r.waiting, conn)
		}
		r.mu.Unlock()
		_ = conn.CloseWith(ctx.Err())
		return nil, ctx.Err()
	}
}

var (
	_ uio.Handler         = (*router)(nil)
	_ uio.TrafficObserver = (*router)(nil)
	_ uio.ServeHooks      = (*router)(nil)
)
