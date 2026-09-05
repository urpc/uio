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
	// Events configures the underlying UIO transport. Nil uses a default Events.
	Events *uio.Events
	// Subprotocols lists protocols offered in preference order.
	Subprotocols []string
	// MaxHeaderBytes bounds the HTTP upgrade response header. Zero uses the default.
	MaxHeaderBytes int
	// MaxFramePayload bounds each wire frame payload. On reads, it also bounds
	// aggregate compressed payload across a fragmented message.
	MaxFramePayload uint64
	// MaxMessageSize bounds a complete message after decompression.
	MaxMessageSize uint64
	// MaxOutboundBytes bounds accepted but unsent wire bytes per connection.
	// Zero uses DefaultMaxOutboundBytes; a negative value disables this limit.
	MaxOutboundBytes int
	// EnableCompression offers RFC 7692 permessage-deflate.
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

	startOnce       sync.Once
	closeMu         sync.Mutex
	closed          bool
	closeErr        error
	lifecycleCtx    context.Context
	lifecycleCancel context.CancelCauseFunc
	started         chan struct{}
	startResult     error
	startResultOnce sync.Once
	dispatchBudget  pendingBudget
	config          *connConfig
}

// NewDialer returns a Dialer configured with default transport settings.
func NewDialer() *Dialer {
	return &Dialer{Events: &uio.Events{}}
}

// Dial starts a WebSocket connection. The supplied context bounds the TCP
// connection and WebSocket handshake; it no longer affects the connection
// after OnOpen. The returned connection remains pending until Handler.OnOpen
// reports handshake success or Handler.OnClose reports a handshake failure.
// The returned error only covers failures that prevent the connection attempt
// from starting.
func (d *Dialer) Dial(ctx context.Context, addr string, handler Handler) (*Conn, error) {
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
		Context: attemptCtx,
		cleanup: cleanupAttempt,
		handler: handler,
		key:     key,
	}
	setup.conn = d.newClientConn(nil, setup)
	raw, err := d.Events.DialContext(attemptCtx, "tcp://"+target.Host, setup)
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
// return the first close cause. OnClose reports connection completion unless
// the configured Executor rejects dispatch.
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
	events := d.Events
	if cancel != nil {
		cancel(cause)
	}
	d.closeMu.Unlock()

	if events == nil {
		return nil
	}
	return events.Close(err)
}

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
		if d.Events == nil {
			d.Events = &uio.Events{}
		}
		events := d.Events
		configureWriteBuffer(events)
		d.dispatchBudget.configure(defaultMaxPendingTotalMessages, defaultMaxPendingTotalBytes)
		d.config = newDialerConnConfig(d)
		oldOnStart := events.OnStart
		events.OnStart = func(events *uio.Events) {
			if err := d.closedCause(); err != nil {
				d.publishStartResult(err)
				_ = events.Close(err)
				return
			}
			if oldOnStart != nil {
				oldOnStart(events)
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

func (d *Dialer) clientExtensions() string {
	if !d.EnableCompression {
		return ""
	}
	// Offer both window parameters so either endpoint may negotiate a smaller
	// RFC 7692 window when it needs to bound compression state.
	return "permessage-deflate; client_max_window_bits; server_max_window_bits=15"
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
		config = newDialerConnConfig(d)
	}
	conn := &Conn{
		raw:     raw,
		config:  config,
		handler: setup.handler,
	}
	if setup.handler != nil {
		conn.dispatch = newDispatchState(config.executor, defaultMaxPendingMessages, defaultMaxPendingBytes, config.dispatchBudget)
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
	if err != nil && conn.closing.Load() {
		return nil
	}
	return err
}

func (d *Dialer) onOutbound(raw uio.Conn, n int) {
	if conn, ok := raw.Userdata().(*Conn); ok && conn != nil {
		conn.releaseOutbound(n)
		conn.tryCloseTransport()
	}
}

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
		conn.dispatchClose(info)
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
	conn.dispatchClose(info)
}

func (d *Dialer) maxFramePayload() uint64 {
	if d.MaxFramePayload == 0 {
		return DefaultMaxFramePayload
	}
	return d.MaxFramePayload
}

func (d *Dialer) maxMessageSize() uint64 {
	if d.MaxMessageSize == 0 {
		return DefaultMaxMessageSize
	}
	return d.MaxMessageSize
}

func (d *Dialer) handshakeTimeout() time.Duration {
	if d.HandshakeTimeout <= 0 {
		return DefaultHandshakeTimeout
	}
	return d.HandshakeTimeout
}

func (d *Dialer) compressionLevel() int {
	if d.CompressionLevel == 0 {
		return -1
	}
	return d.CompressionLevel
}

type dialSetup struct {
	context.Context
	cleanup    func()
	handler    Handler
	key        string
	conn       *Conn
	attachOnce sync.Once
}

func (setup *dialSetup) attach(raw uio.Conn) {
	setup.attachOnce.Do(func() {
		setup.conn.raw = raw
	})
}
