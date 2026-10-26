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
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/urpc/uio"
	"github.com/urpc/uio/uws/internal/handshake"
)

// Dialer creates outbound WebSocket connections over a shared UIO transport.
// Configure its fields before the first Dial; the first Dial freezes the
// connection configuration.
type Dialer struct {
	// Options configures the protocol, the transport and backpressure.
	Options
	// Header is added to every upgrade request, for credentials such as an
	// Authorization token. The headers the handshake sets itself are refused
	// — Host, Upgrade, Connection, Content-Length, Transfer-Encoding, Expect,
	// and Sec-WebSocket-Key, -Version, -Protocol, -Extensions and -Accept —
	// the first Dial returns ErrInvalidOptions.
	Header http.Header
	// RequestHeader, when set, returns headers for one dial, given its
	// userdata — nil for a DialClient, which dials without one; they replace
	// Header's values of the same name. A header the handshake sets itself
	// fails that Dial with ErrInvalidOptions.
	RequestHeader func(userdata any) http.Header

	startOnce       sync.Once
	closeMu         sync.Mutex
	closed          bool
	closeErr        error
	lifecycleCtx    context.Context
	lifecycleCancel context.CancelCauseFunc
	started         chan struct{}
	startResult     error
	startResultOnce sync.Once
	config          *connConfig
	opts            Options     // resolved snapshot, fixed at first Dial
	header          http.Header // Header, canonical, fixed at first Dial
	events          *uio.Events // private transport configuration
	clients         sync.Map    // the open pull Clients, for Close to release
	stats           statsCounters

	// startHook runs inside the transport's OnStart before the start result
	// is published; it lets tests observe or block startup.
	startHook func()
}

// NewDialer returns a Dialer using opts.
func NewDialer(opts Options) *Dialer {
	return &Dialer{Options: opts}
}

// Stats reports the read pausing of this dialer's connections.
func (d *Dialer) Stats() Stats { return d.stats.snapshot() }

// Dial starts a WebSocket connection and returns once the upgrade request is
// on its way; the Handler reports the rest. An error means no connection was
// made and no callback follows — uio.ErrFirstTurnRefused when a custom
// Executor declined the connection's first turn. After a nil error, OnOpen
// reports a completed handshake, and OnClose always reports how the
// connection ended, a failed handshake or upgrade request included. userdata
// is the connection's Userdata in every callback. ctx bounds the TCP
// connection and the handshake, not the connection after OnOpen.
func (d *Dialer) Dial(ctx context.Context, addr string, userdata any) error {
	return d.dialWithHandler(ctx, addr, userdata, nil)
}

// dialWithHandler runs Dial's setup; a non-nil handler replaces the dialer's
// frozen one on this connection only, which is how DialClient installs its
// internal callbacks.
func (d *Dialer) dialWithHandler(ctx context.Context, addr string, userdata any, handler Handler) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := d.closedCause(); err != nil {
		return err
	}
	target, err := url.Parse(addr)
	if err != nil || target.Host == "" || (target.Scheme != "ws" && target.Scheme != "wss") {
		return fmt.Errorf("uws: only ws:// and wss:// targets are supported: %w", ErrProtocol)
	}
	if target.Scheme == "wss" && len(d.Options.Middleware) == 0 {
		return fmt.Errorf("%w: wss:// needs TLS middleware in Middleware", ErrInvalidOptions)
	}
	keyBytes := make([]byte, 16)
	if _, err = rand.Read(keyBytes); err != nil {
		return err
	}
	key := base64.StdEncoding.EncodeToString(keyBytes)
	attemptCtx, cleanupAttempt, err := d.newAttemptContext(ctx)
	if err != nil {
		return err
	}
	if err = d.start(attemptCtx); err != nil {
		cleanupAttempt()
		return err
	}
	config := d.config
	header, err := d.requestHeader(userdata)
	if err != nil {
		cleanupAttempt()
		return err
	}
	request, err := handshake.BuildClientRequest(target, key, config.subprotocols, config.clientExtensions(), header)
	if err != nil {
		cleanupAttempt()
		return err
	}
	setup := &dialSetup{
		Context:  attemptCtx,
		cleanup:  cleanupAttempt,
		userdata: userdata,
		handler:  handler,
		key:      key,
	}
	setup.conn = d.newClientConn(nil, setup)
	setup.request = request
	raw, err := d.events.DialContext(attemptCtx, "tcp://"+target.Host, setup)
	if err != nil {
		cause := context.Cause(attemptCtx)
		if deadline, ok := attemptCtx.Deadline(); cause == nil && ok && !time.Now().Before(deadline) {
			cause = context.DeadlineExceeded
		}
		cleanupAttempt()
		if cause != nil {
			return cause
		}
		return err
	}
	_ = raw
	return nil
}

// sendRequest writes the upgrade request once the stack under it has
// settled — with a TLS layer, its OnOpen runs after that layer's handshake,
// and the request must not go out in the clear before it. The request's
// OnOutbound retires pending bytes like any frame's, so it must be counted,
// or it would retire the bytes of a later Close frame and let the graceful
// close drop that frame.
func (d *Dialer) sendRequest(raw uio.Conn, setup *dialSetup) {
	conn := setup.conn
	if conn == nil {
		return
	}
	conn.trackOutbound(len(setup.request))
	n, err := raw.Write(setup.request)
	if err == nil && n != len(setup.request) {
		err = io.ErrShortWrite
	}
	if err != nil {
		conn.rollbackOutbound(len(setup.request) - n)
		if !errors.Is(err, net.ErrClosed) {
			// A connection already closed, possibly after the whole
			// handshake ran, reports its own cause.
			_ = raw.CloseWith(err)
		}
		return
	}
	if err := raw.Flush(); err != nil && !errors.Is(err, net.ErrClosed) {
		_ = raw.CloseWith(err)
	}
}

// Close permanently stops the dialer, cancels in-flight connection attempts,
// and requests closure of established connections. Subsequent Dial calls
// return the first close cause. OnClose reports connection completion.
func (d *Dialer) Close(err error) error {
	d.closeMu.Lock()
	if d.closed {
		d.closeMu.Unlock()
		return nil
	}
	d.closed = true
	d.closeErr = err
	if d.closeErr == nil {
		d.closeErr = ErrClosed
	}
	cause := d.closeErr
	cancel := d.lifecycleCancel
	d.lifecycleCancel = nil
	events := d.events
	if cancel != nil {
		cancel(cause)
	}
	d.closeMu.Unlock()

	d.releaseClients()
	if events == nil {
		return nil
	}
	return events.Close(err)
}

// start initializes the shared dial-only Events exactly once. The first caller
// waits until OnStart publishes success; later callers observe the same result.
func (d *Dialer) start(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := d.closedCause(); err != nil {
		return err
	}
	var setupErr error
	d.startOnce.Do(func() {
		d.closeMu.Lock()
		d.started = make(chan struct{})
		if d.closed {
			setupErr = d.closeErr
			d.closeMu.Unlock()
			d.publishStartResult(setupErr)
			return
		}
		opts, err := d.Options.resolve()
		if err == nil {
			if err = handshake.CheckRequestHeader(d.Header); err != nil {
				err = fmt.Errorf("%w: Header: %v", ErrInvalidOptions, err)
			}
		}
		if err != nil {
			d.closeMu.Unlock()
			d.publishStartResult(err)
			return
		}
		d.opts = opts
		d.header = canonicalHeader(d.Header)
		d.config = newDialerConnConfig(opts, &d.stats)
		events := opts.transportEvents()
		d.events = events
		handler := uio.FuncHandler{
			Start: func(events *uio.Events) {
				if err := d.closedCause(); err != nil {
					d.publishStartResult(err)
					_ = events.Close(err)
					return
				}
				if hook := d.startHook; hook != nil {
					hook()
				}
				if err := d.closedCause(); err != nil {
					d.publishStartResult(err)
					return
				}
				d.publishStartResult(nil)
			},
			Open:     d.onOpen,
			Data:     d.onData,
			Close:    d.onClose,
			Outbound: d.onOutbound,
			Inbound:  d.onInbound,
		}
		// The middleware wraps what the connections carry, not the traffic the
		// observer reports or the hooks around Serve.
		events.Handler = handler
		events.Use(opts.Middleware...)
		events.Observer, events.Hooks = handler, handler
		go func() {
			err := events.Serve()
			startErr := err
			if startErr == nil {
				startErr = ErrClosed
			}
			d.publishStartResult(startErr)
			_ = d.Close(err)
		}()
		d.closeMu.Unlock()
	})
	if setupErr != nil {
		return setupErr
	}
	select {
	case <-d.started:
		return d.startResult
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}

func (d *Dialer) publishStartResult(err error) {
	d.startResultOnce.Do(func() {
		d.startResult = err
		close(d.started)
	})
}

func (d *Dialer) closedCause() error {
	d.closeMu.Lock()
	defer d.closeMu.Unlock()
	if !d.closed {
		return nil
	}
	return d.closeErr
}

// newAttemptContext combines the caller's deadline with the Dialer's lifecycle
// cancellation. cleanup detaches both AfterFunc hooks exactly once.
func (d *Dialer) newAttemptContext(parent context.Context) (context.Context, func(), error) {
	if parent == nil {
		parent = context.Background()
	}
	d.closeMu.Lock()
	if d.closed {
		err := d.closeErr
		d.closeMu.Unlock()
		return nil, nil, err
	}
	if d.lifecycleCtx == nil {
		d.lifecycleCtx, d.lifecycleCancel = context.WithCancelCause(context.Background())
	}
	lifecycleCtx := d.lifecycleCtx
	d.closeMu.Unlock()

	attemptCtx, cancelAttempt := context.WithCancelCause(parent)
	stopDialer := context.AfterFunc(lifecycleCtx, func() {
		cancelAttempt(context.Cause(lifecycleCtx))
	})
	var cleanupOnce sync.Once
	cleanup := func() {
		cleanupOnce.Do(func() {
			stopDialer()
			cancelAttempt(ErrClosed)
		})
	}
	return attemptCtx, cleanup, nil
}

// requestHeader returns the headers one dial adds to its upgrade request:
// Header, frozen by the first Dial, with RequestHeader's on top.
func (d *Dialer) requestHeader(userdata any) (http.Header, error) {
	if d.RequestHeader == nil {
		return d.header, nil
	}
	extra := d.RequestHeader(userdata)
	if err := handshake.CheckRequestHeader(extra); err != nil {
		return nil, fmt.Errorf("%w: RequestHeader: %v", ErrInvalidOptions, err)
	}
	if len(extra) == 0 {
		return d.header, nil
	}
	header := d.header.Clone()
	if header == nil {
		header = make(http.Header, len(extra))
	}
	for name, values := range extra {
		header[http.CanonicalHeaderKey(name)] = values
	}
	return header, nil
}

// canonicalHeader copies h with its names in canonical form, so a dial's
// RequestHeader replaces a value whatever case either was written in.
func canonicalHeader(h http.Header) http.Header {
	if len(h) == 0 {
		return nil
	}
	out := make(http.Header, len(h))
	for name, values := range h {
		key := http.CanonicalHeaderKey(name)
		out[key] = append(out[key], values...)
	}
	return out
}

func (d *Dialer) onOpen(raw uio.Conn) {
	setup, _ := raw.Userdata().(*dialSetup)
	if setup == nil {
		_ = raw.Close()
		return
	}
	if setup.conn == nil {
		setup.conn = d.newClientConn(nil, setup)
	}
	setup.attach(raw)
	conn := setup.conn
	raw.SetUserdata(conn)
	conn.startHandshakeTimer(conn.config.handshakeTimeout)
	conn.watchHandshakeContext(setup.Context)
	d.sendRequest(raw, setup)
}

func (d *Dialer) newClientConn(raw uio.Conn, setup *dialSetup) *Conn {
	config := d.config
	if config == nil {
		opts, _ := d.Options.resolve()
		config = newDialerConnConfig(opts, &d.stats)
	}
	handler := config.handler
	if setup.handler != nil {
		handler = setup.handler
	}
	conn := &Conn{
		raw:      raw,
		config:   config,
		handler:  handler,
		userData: setup.userdata,
	}
	if config.heartbeatInterval > 0 {
		conn.heartbeat = &heartbeatState{}
	}
	conn.handshake.Store(&handshakeState{
		clientKey: setup.key,
		cleanup:   setup.cleanup,
	})
	return conn
}

func (d *Dialer) onData(raw uio.Conn) error {
	conn, ok := raw.Userdata().(*Conn)
	if !ok || conn == nil {
		return ErrClosed
	}
	err := conn.readAvailable()
	if protocolCloseOwnsTransport(err) {
		return nil
	}
	return err
}

// onInbound reports transport bytes to a Handler that observes traffic.
func (d *Dialer) onInbound(raw uio.Conn, n int) {
	if conn, ok := raw.Userdata().(*Conn); ok && conn != nil {
		if observer, ok := conn.handler.(TrafficObserver); ok {
			observer.OnInbound(conn, n)
		}
	}
}

func (d *Dialer) onOutbound(raw uio.Conn, n int) {
	if conn, ok := raw.Userdata().(*Conn); ok && conn != nil {
		conn.releaseOutbound(n)
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

// onClose also handles failure before OnOpen has replaced dialSetup userdata
// with *Conn. Both paths converge on exactly one handler notification.
func (d *Dialer) onClose(raw uio.Conn, err error) {
	conn, _ := raw.Userdata().(*Conn)
	if conn == nil {
		setup, _ := raw.Userdata().(*dialSetup)
		if setup == nil {
			return
		}
		if setup.conn == nil {
			setup.conn = d.newClientConn(nil, setup)
		}
		setup.attach(raw)
		conn = setup.conn
		if conn == nil {
			setup.cleanup()
			return
		}
		raw.SetUserdata(conn)
	}
	conn.releaseParser()
	conn.releaseAssembler()
	if !conn.opened.Load() {
		conn.stopHandshakeTimer()
		if !conn.closed.CompareAndSwap(false, true) {
			return
		}
		conn.stopCloseTimer()
		if err == nil {
			err = ErrClosed
		}
		info := conn.closeInfo()
		if info.Err == nil {
			info.Err = err
		}
		conn.notifyClose(info)
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

// dialSetup bridges synchronous TCP setup and asynchronous WebSocket handshake
// callbacks. attachOnce prevents Dial and OnOpen races from replacing raw.
type dialSetup struct {
	context.Context
	cleanup    func()
	userdata   any
	handler    Handler // overrides the dialer's Handler when non-nil
	key        string
	conn       *Conn
	attachOnce sync.Once
	request    []byte // the upgrade request, sent once the layers settle
}

func (setup *dialSetup) attach(raw uio.Conn) {
	setup.attachOnce.Do(func() {
		setup.conn.raw = raw
	})
}
