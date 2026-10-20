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
	"fmt"
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
	events          *uio.Events // private transport configuration
	connections     sync.Map
	heartbeat       heartbeatRunner
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

// Dial starts a WebSocket connection. The supplied context bounds the TCP
// connection and WebSocket handshake; it no longer affects the connection
// after OnOpen. The returned connection remains pending until Handler.OnOpen
// reports handshake success or Handler.OnClose reports a handshake failure.
// The returned error covers setup, network dial, and upgrade-request write or
// flush failures. A handshake response failure is reported through OnClose.
func (d *Dialer) Dial(ctx context.Context, addr string, userdata any) (*Conn, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := d.closedCause(); err != nil {
		return nil, err
	}
	target, err := url.Parse(addr)
	if err != nil || target.Scheme != "ws" || target.Host == "" {
		return nil, fmt.Errorf("uws: only ws:// targets are supported: %w", ErrProtocol)
	}
	keyBytes := make([]byte, 16)
	if _, err = rand.Read(keyBytes); err != nil {
		return nil, err
	}
	key := base64.StdEncoding.EncodeToString(keyBytes)
	attemptCtx, cleanupAttempt, err := d.newAttemptContext(ctx)
	if err != nil {
		return nil, err
	}
	if err = d.start(attemptCtx); err != nil {
		cleanupAttempt()
		return nil, err
	}
	config := d.config
	request, err := handshake.BuildClientRequest(target, key, config.subprotocols, config.clientExtensions())
	if err != nil {
		cleanupAttempt()
		return nil, err
	}
	setup := &dialSetup{
		Context:  attemptCtx,
		cleanup:  cleanupAttempt,
		userdata: userdata,
		key:      key,
	}
	setup.conn = d.newClientConn(nil, setup)
	raw, err := d.events.DialContext(attemptCtx, "tcp://"+target.Host, setup)
	if err != nil {
		cause := context.Cause(attemptCtx)
		if deadline, ok := attemptCtx.Deadline(); cause == nil && ok && !time.Now().Before(deadline) {
			cause = context.DeadlineExceeded
		}
		cleanupAttempt()
		if cause != nil {
			return nil, cause
		}
		return nil, err
	}
	setup.attach(raw)
	conn := setup.conn
	if _, err = raw.Write(request); err != nil {
		_ = raw.CloseWith(err)
		return nil, err
	}
	if err = raw.Flush(); err != nil {
		_ = raw.CloseWith(err)
		return nil, err
	}
	return conn, nil
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

	d.stopHeartbeat()
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
		if err != nil {
			d.closeMu.Unlock()
			d.publishStartResult(err)
			return
		}
		d.opts = opts
		d.config = newDialerConnConfig(opts, &d.connections, &d.stats)
		events := opts.transportEvents()
		d.events = events
		events.OnStart = func(events *uio.Events) {
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
		}
		events.OnOpen = d.onOpen
		events.OnData = d.onData
		events.OnClose = d.onClose
		events.OnOutbound = d.onOutbound
		events.OnInbound = d.onInbound
		d.startHeartbeat(d.config)
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

// startHeartbeat starts the scanner that pings this dialer's connections.
func (d *Dialer) startHeartbeat(config *connConfig) {
	if config == nil {
		return
	}
	d.heartbeat.start(config.heartbeatConnections, config.heartbeatInterval, config.heartbeatTimeout)
}

// stopHeartbeat ends the heartbeat scanner and waits for it to exit.
func (d *Dialer) stopHeartbeat() {
	d.heartbeat.stop()
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
}

func (d *Dialer) newClientConn(raw uio.Conn, setup *dialSetup) *Conn {
	config := d.config
	if config == nil {
		opts, _ := d.Options.resolve()
		config = newDialerConnConfig(opts, &d.connections, &d.stats)
	}
	conn := &Conn{
		raw:      raw,
		config:   config,
		handler:  config.handler,
		userData: setup.userdata,
	}
	if config.heartbeatConnections != nil {
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
	d.connections.Delete(conn)
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
	key        string
	conn       *Conn
	attachOnce sync.Once
}

func (setup *dialSetup) attach(raw uio.Conn) {
	setup.attachOnce.Do(func() {
		setup.conn.raw = raw
	})
}
