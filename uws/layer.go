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
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"github.com/urpc/uio"
	"github.com/urpc/uio/uws/internal/handshake"
)

// LayerOptions configures ServerLayer and ClientLayer.
type LayerOptions struct {
	// Options holds the protocol's settings: subprotocols, limits,
	// compression, timeouts, heartbeats and the outbound watermarks;
	// ReadBufferSize and WriteCoalesce size only the layer's write batches
	// and read rounds, as the socket's reads are its Events'. The transport's
	// own — Handler, Pollers, ReusePort, Executor, TLSConfig and
	// MaxOutboundBuffered — belong to the Events the layer is installed on
	// and must be left zero: a stack's TLS is a layer of its own, and its
	// outbound limit is the Events' MaxOutboundBuffered.
	Options
	// CheckOrigin and Authorize decide a ServerLayer's upgrade requests as
	// they do a Server's; the userdata Authorize returns, when not nil,
	// becomes the connection's Userdata.
	CheckOrigin func(*http.Request) bool
	Authorize   func(r *http.Request) (userdata any, err error)
	// Header is added to a ClientLayer's upgrade request, as a Dialer's is.
	Header http.Header
}

// ServerLayer runs the server side of the WebSocket protocol as a uio.Layer,
// for a stack of your own:
//
//	events.Use(tls, uws.ServerLayer(opts), tunnel)
//
// It answers the upgrade request, and opens the layers above once the
// handshake succeeded; a request it refuses gets its answer and closes the
// connection, which the layers above never hear of. From then on it carries
// a byte stream: what the layer above writes leaves as Binary messages —
// split at MaxMessageSize — and the payload of every message that arrives,
// Text or Binary, is handed up once the message is whole, message boundaries
// dropped. Pings are answered, heartbeats run, and a Close from the peer is
// answered and ends the connection. Closing the connection from above sends
// a Close frame first: 1000 for a graceful close, 1011 for an abort. The
// stream has no half-close: CloseWrite is not supported.
//
// Each write from above leaves as its own frame, through the Events the layer
// runs on: set its WriteBufferedThreshold for the writes a Handler makes in
// one callback to leave in one send, as a Server's do.
//
// Options that are not valid for a layer fail every connection through it
// with ErrInvalidOptions — a dial reports it — as LayerOptions.Validate does.
//
// The layer above can read the negotiated subprotocol through uio.FindLayer:
//
//	if ws, ok := uio.FindLayer[interface{ Subprotocol() string }](conn); ok { … }
func ServerLayer(opts LayerOptions) uio.Layer {
	resolved, err := opts.resolve()
	if err != nil {
		return &wsLayer{err: err}
	}
	return &wsLayer{config: newServerConnConfig(resolved, opts.CheckOrigin, opts.Authorize, nil)}
}

// ClientLayer runs the client side of the WebSocket protocol as a uio.Layer:
// it sends the upgrade request for target, a ws:// or wss:// URL whose host
// and path the request names, and opens the layers above once the server
// accepted it. A dial through it returns once the handshake succeeded, and
// reports a refusal or a failure as its error — a *HandshakeError when the
// server answered. The scheme names no transport: TLS under the layer is
// another layer's, installed below it. One layer dials one target. See
// ServerLayer for the byte stream the layer carries and for invalid options.
func ClientLayer(target string, opts LayerOptions) uio.Layer {
	parsed, header, resolved, err := opts.resolveClient(target)
	if err != nil {
		return &wsLayer{err: err}
	}
	return &wsLayer{
		config: newDialerConnConfig(resolved, nil),
		target: parsed,
		header: header,
	}
}

// Validate reports what ServerLayer, and ClientLayer for target when it is
// not empty, would refuse: the error every connection through the layer
// would fail with.
func (o LayerOptions) Validate(target string) error {
	if target == "" {
		_, err := o.resolve()
		return err
	}
	_, _, _, err := o.resolveClient(target)
	return err
}

func (o LayerOptions) resolveClient(target string) (*url.URL, http.Header, Options, error) {
	resolved, err := o.resolve()
	if err != nil {
		return nil, nil, Options{}, err
	}
	parsed, err := url.Parse(target)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "ws" && parsed.Scheme != "wss") {
		return nil, nil, Options{}, fmt.Errorf("%w: ClientLayer needs a ws:// or wss:// URL, got %q", ErrInvalidOptions, target)
	}
	if err := handshake.CheckRequestHeader(o.Header); err != nil {
		return nil, nil, Options{}, fmt.Errorf("%w: Header: %v", ErrInvalidOptions, err)
	}
	return parsed, canonicalHeader(o.Header), resolved, nil
}

// resolve applies the protocol defaults and refuses the transport's settings.
func (o LayerOptions) resolve() (Options, error) {
	switch {
	case o.Handler != nil:
		return Options{}, fmt.Errorf("%w: a layer hands its stream to the layer above, not to a Handler", ErrInvalidOptions)
	case o.Pollers != 0, o.ReusePort, o.Executor != nil:
		return Options{}, fmt.Errorf("%w: Pollers, ReusePort and Executor belong to the layer's Events", ErrInvalidOptions)
	case o.TLSConfig != nil:
		return Options{}, fmt.Errorf("%w: TLS under a layer is a layer of its own", ErrInvalidOptions)
	case o.MaxOutboundBuffered != 0:
		return Options{}, fmt.Errorf("%w: a stack's outbound limit is its Events' MaxOutboundBuffered", ErrInvalidOptions)
	}
	return o.Options.resolve()
}

// wsLayer is what ServerLayer and ClientLayer build: the policy every
// connection through it shares.
type wsLayer struct {
	config *connConfig
	target *url.URL    // a client's
	header http.Header // a client's, canonical
	err    error       // the options' fault, which fails every connection
}

func (l *wsLayer) Attach(ctx uio.LayerContext) uio.LayerHandler {
	return &wsLayerHandler{layer: l, ctx: ctx}
}

// wsLayerHandler is the protocol engine on one connection of a stack: a Conn
// whose transport is the connection below, and whose Handler hands up.
type wsLayerHandler struct {
	layer *wsLayer
	ctx   uio.LayerContext
	// conn is set as the layer opens; OnWireSent, which runs wherever the socket
	// sends, may come first — for what the layers below sent to open.
	conn atomic.Pointer[Conn]
	// staged holds the frames that arrived behind the handshake: the engine
	// hands them over rather than reading them before the layers above
	// opened. Only the connection's turns touch it.
	staged []byte
	// writeMu keeps one write from above whole: the messages a large write
	// is split into go out together, never between another writer's.
	writeMu sync.Mutex
}

var (
	_ uio.LayerHandler        = (*wsLayerHandler)(nil)
	_ uio.WireCounterObserver = (*wsLayerHandler)(nil)
	_ uio.GracefulCloser      = (*wsLayerHandler)(nil)
)

func (w *wsLayerHandler) OnOpen(ctx uio.LayerContext) {
	if err := w.layer.err; err != nil {
		_ = ctx.Abort(err)
		return
	}
	config := w.layer.config
	conn := &Conn{raw: ctx.Down(), config: config, handler: layerSink{w}}
	if config.heartbeatInterval > 0 {
		conn.heartbeat = &heartbeatState{}
	}
	w.conn.Store(conn)
	if !config.client {
		conn.handshake.Store(&handshakeState{})
		conn.startHandshakeTimer(config.handshakeTimeout)
		return
	}
	keyBytes := make([]byte, 16)
	if _, err := rand.Read(keyBytes); err != nil {
		_ = ctx.Abort(err)
		return
	}
	key := base64.StdEncoding.EncodeToString(keyBytes)
	request, err := handshake.BuildClientRequest(w.layer.target, key, config.subprotocols, config.clientExtensions(), w.layer.header)
	if err != nil {
		_ = ctx.Abort(err)
		return
	}
	conn.handshake.Store(&handshakeState{clientKey: key})
	conn.startHandshakeTimer(config.handshakeTimeout)
	// The request is counted like any frame, so a close waits for it.
	conn.trackOutbound(len(request))
	n, err := ctx.Down().Write(request)
	if err == nil && n != len(request) {
		err = io.ErrShortWrite
	}
	if err != nil {
		conn.rollbackOutbound(len(request) - n)
		if !errors.Is(err, net.ErrClosed) {
			_ = ctx.Abort(err)
		}
	}
}

func (w *wsLayerHandler) OnRead(ctx uio.LayerContext) error {
	conn := w.conn.Load()
	if conn == nil {
		return nil
	}
	if len(w.staged) > 0 {
		// Frames that arrived behind a handshake which settled on an earlier
		// round: the layers above open after that callback returned, so they
		// go up now, in order.
		extra := w.staged
		w.staged = nil
		if _, err := conn.feedFrames(extra, conn.acceptFrame); err != nil {
			if errors.Is(err, ErrClosed) {
				return nil
			}
			return conn.protocolClose(err)
		}
	}
	err := conn.readAvailable()
	if protocolCloseOwnsTransport(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if ctx.Down().State()&uio.StatePeerEnded != 0 && !conn.closed.Load() {
		// The stream below ended without the peer's Close: an abnormal end,
		// unless this end's Close went out and waits for the transport.
		cause := io.ErrUnexpectedEOF
		if conn.closing.Load() {
			cause = io.EOF
		}
		return cause
	}
	if len(w.staged) > 0 {
		// Frames that arrived behind a handshake that settled this round: the
		// layers above open after this callback returns, and the round asked
		// for here feeds them up then.
		_ = ctx.Wake()
	}
	return nil
}

// OnWrite sends what the layer above writes as Binary messages, one per
// MaxMessageSize.
func (w *wsLayerHandler) OnWrite(_ uio.LayerContext, p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	conn := w.conn.Load()
	most := conn.maxMessageSizeInt()
	w.writeMu.Lock()
	defer w.writeMu.Unlock()
	sent := 0
	for {
		part := p[sent:]
		if len(part) > most {
			part = part[:most]
		}
		if err := conn.SendBinary(part); err != nil {
			return sent, err
		}
		sent += len(part)
		if sent == len(p) {
			return sent, nil
		}
	}
}

// OnTimer runs the heartbeat, which OnRead runs too: a turn skips
// OnRead while the layer above has not read what it holds, and a peer
// gone meanwhile must still be found out.
func (w *wsLayerHandler) OnTimer(uio.LayerContext) error {
	if conn := w.conn.Load(); conn != nil {
		conn.heartbeatTick(time.Now())
	}
	return nil
}

func (w *wsLayerHandler) OnWireSent(_ uio.LayerContext, n int) {
	conn := w.conn.Load()
	if conn == nil {
		return
	}
	conn.transportSent(n)
	if conn.writes.close.closePending() {
		conn.tryCloseTransport()
	}
	conn.resumeReadsAfterDrain()
}

// CloseGracefully sends the peer a Close as the layer above closes: 1000 for
// a graceful close, 1011 for an abort.
func (w *wsLayerHandler) CloseGracefully(_ uio.LayerContext, cause error) {
	conn := w.conn.Load()
	if conn == nil || !conn.opened.Load() {
		return
	}
	code := uint16(1000)
	if cause != nil && !errors.Is(cause, io.EOF) && !errors.Is(cause, io.ErrUnexpectedEOF) {
		code = 1011
	}
	_ = conn.Close(code, "")
}

func (w *wsLayerHandler) OnClose(uio.LayerContext, error) {
	conn := w.conn.Load()
	if conn == nil {
		return
	}
	conn.releaseParser()
	conn.releaseAssembler()
	if !conn.opened.Load() {
		conn.stopHandshakeTimer()
	}
	if conn.closed.CompareAndSwap(false, true) {
		conn.stopCloseTimer()
	}
}

// Subprotocol reports the subprotocol the handshake chose, if any.
func (w *wsLayerHandler) Subprotocol() string {
	if conn := w.conn.Load(); conn != nil {
		return conn.Subprotocol()
	}
	return ""
}

// layerSink is the Handler of a layer's engine: it opens the layers above
// and hands them what arrives.
type layerSink struct{ w *wsLayerHandler }

func (s layerSink) OnOpen(c *Conn) {
	if c.userData != nil {
		s.w.ctx.Down().SetUserdata(c.userData) // what Authorize returned
	}
	s.w.ctx.Up().Open()
}

func (s layerSink) OnMessage(_ *Conn, m Message) { s.w.ctx.Up().Write(m.Payload) }

func (s layerSink) OnClose(*Conn, CloseEvent) {}
