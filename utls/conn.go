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

package utls

import (
	stdtls "crypto/tls"
	"io"
	"iter"
	"net"
	"sync"
	"time"

	"github.com/urpc/uio"
)

// conn is TLS on one connection. Everything but the writes runs on the
// connection's turns, which UIO serializes: the handshake as a coroutine
// those turns resume, the records straight in the turn. Decrypted bytes go
// straight into the layer's delivery space, so the layer above reads them
// where crypto/tls wrote them.
type conn struct {
	tlsLayer *tlsLayer
	ctx      uio.LayerContext

	// mu carries the engine's own state: the ciphertext pending, the
	// handshake's coroutine handles and the flags. crypto/tls is called
	// without it.
	mu sync.Mutex

	tls *stdtls.Conn
	rec recordCursor
	// pending is ciphertext the connection below delivered that the engine
	// has not read, from readAt on.
	pending []byte
	readAt  int

	// resume continues the handshake's coroutine, and stop ends it; both
	// nil once the handshake settled. yield is the suspension inside the
	// coroutine's reads.
	resume func() (struct{}, bool)
	stop   func()
	yield  func(struct{}) bool

	ready           bool // the handshake settled
	ended           bool // the connection's OnClose ran
	transportClosed bool // crypto/tls closed the transport
	readEnded       bool // the peer's close_notify or a read failure
}

var (
	_ uio.LayerConn                        = (*conn)(nil)
	_ uio.CloseWriter                      = (*conn)(nil)
	_ uio.GracefulCloser                   = (*conn)(nil)
	_ interface{ State() ConnectionState } = (*conn)(nil)
)

// OnOpen starts the handshake: its coroutine, and the deadline that bounds
// it.
func (c *conn) OnOpen(ctx uio.LayerContext) {
	c.mu.Lock()
	engine := &transport{c: c}
	if c.tlsLayer.client {
		c.tls = stdtls.Client(engine, c.tlsLayer.config)
	} else {
		c.tls = stdtls.Server(engine, c.tlsLayer.config)
	}
	c.resume, c.stop = iter.Pull(handshake(c))
	resume := c.resume
	c.mu.Unlock()
	// The deadline closes the connection below when it passes, and the close
	// ends the handshake; cleared once it settles.
	if timeout := c.tlsLayer.handshakeTimeout; timeout >= 0 {
		_ = ctx.Lower().SetReadDeadline(time.Now().Add(c.handshakeTimeout()))
	}
	// The first flight needs no data: a client writes its ClientHello here.
	// A server's handshake asks to read at once and suspends until a turn
	// brings bytes.
	if _, waiting := resume(); !waiting {
		c.settle(ctx)
	}
}

// settle takes a handshake coroutine that has finished: its result is read
// out, the deadline it ran under is cleared, and the layers above open — or
// a failure ends the connection with its cause. It runs on a turn.
func (c *conn) settle(ctx uio.LayerContext) {
	c.mu.Lock()
	c.resume, c.stop, c.yield = nil, nil, nil
	if err := c.tls.Handshake(); err != nil { // the settled result
		c.mu.Unlock()
		_ = ctx.CloseWith(err)
		return
	}
	c.ready = true
	c.mu.Unlock()
	_ = ctx.Lower().SetReadDeadline(time.Time{}) // the handshake's bound
	// What arrived with the last flight may hold records already.
	if err := c.pump(ctx); err != nil {
		_ = ctx.CloseWith(err)
		return
	}
	ctx.Open()
}

func (c *conn) handshakeTimeout() time.Duration {
	if timeout := c.tlsLayer.handshakeTimeout; timeout != 0 {
		return timeout
	}
	return DefaultHandshakeTimeout
}

// handshake is what the coroutine runs; yield is its suspension, stored
// where the transport's reads find it.
func handshake(c *conn) func(func(struct{}) bool) {
	return func(yield func(struct{}) bool) {
		c.mu.Lock()
		c.yield = yield
		c.mu.Unlock()
		_ = c.tls.Handshake() // the settled result is read where it ends
	}
}

// OnInbound is a turn: the handshake's coroutine runs on what arrived until
// it settles, and from then on the turn's bytes are decrypted and delivered,
// as far as the staging bound leaves room.
func (c *conn) OnInbound(ctx uio.LayerContext) error {
	lower := ctx.Lower()
	c.mu.Lock()
	if c.resume != nil {
		chunk := lower.PeekChunk()
		c.pending = append(c.pending, chunk...)
		_, _ = lower.Discard(len(chunk))
		resume := c.resume
		c.mu.Unlock()
		if _, waiting := resume(); !waiting {
			c.settle(ctx)
		}
		return nil
	}
	ready := c.ready
	c.mu.Unlock()
	if !ready {
		return nil
	}
	if ctx.HeldAbove() >= c.stagingBound() {
		// A full staging leaves what arrived below unread, so the pause
		// travels to the wire; the read above that makes room runs the turn
		// that takes it.
		ctx.Stall()
		return nil
	}
	chunk := lower.PeekChunk()
	c.mu.Lock()
	c.pending = append(c.pending, chunk...)
	c.mu.Unlock()
	_, _ = lower.Discard(len(chunk))
	if err := c.pump(ctx); err != nil {
		_ = ctx.CloseWith(err)
	}
	return nil
}

// pump decrypts into the layer's delivery space, one record at a time,
// until the staging bound or the end of what arrived. It runs on a turn.
func (c *conn) pump(ctx uio.LayerContext) error {
	bound := c.stagingBound()
	for {
		if ctx.HeldAbove() >= bound {
			c.mu.Lock()
			waiting := c.readAt < len(c.pending)
			c.mu.Unlock()
			if waiting {
				ctx.Stall()
			}
			return nil
		}
		c.mu.Lock()
		if c.readEnded || !c.ready || c.readAt >= len(c.pending) {
			// Nothing left for the engine to read: a Read here could only
			// report errWouldBlock.
			c.mu.Unlock()
			return nil
		}
		c.mu.Unlock()
		n, err := c.tls.Read(ctx.Space(readBufferSize)[:readBufferSize])
		if n > 0 {
			ctx.Commit(n)
		}
		c.mu.Lock()
		c.compactLocked()
		c.mu.Unlock()
		if err != nil {
			if err == errWouldBlock {
				return nil
			}
			if err == io.EOF { // the peer's close_notify
				c.mu.Lock()
				c.readEnded = true
				c.mu.Unlock()
				return nil
			}
			return err
		}
	}
}

// OnTimer: TLS arms no timer of its own.
func (c *conn) OnTimer(uio.LayerContext) error { return nil }

// OnClose is the connection's end: a handshake still running is stopped
// with it.
func (c *conn) OnClose(uio.LayerContext, error) {
	c.mu.Lock()
	c.ended = true
	stop := c.stop
	c.resume, c.stop, c.yield = nil, nil, nil
	c.mu.Unlock()
	if stop != nil {
		stop() // a handshake waiting for data ends with the connection
	}
}

// OnOutbound encrypts what the layer above writes, from any goroutine.
func (c *conn) OnOutbound(_ uio.LayerContext, p []byte) (int, error) {
	engine, err := c.engine()
	if err != nil {
		return 0, err
	}
	return engine.Write(p)
}

// CloseGracefully sends the peer close_notify, once what was accepted has
// left, as the layer above closes the connection.
func (c *conn) CloseGracefully(uio.LayerContext) {
	if engine, err := c.engine(); err == nil {
		_ = engine.Close()
	}
}

// CloseWrite sends close_notify one way: the peer's reads end, this end's
// go on.
func (c *conn) CloseWrite(uio.LayerContext) error {
	engine, err := c.engine()
	if err != nil {
		return err
	}
	return engine.CloseWrite()
}

// State reports what the handshake settled on; before it settles, the zero
// ConnectionState. The connection the layer above holds reports it through
// uio.LayerOf.
func (c *conn) State() ConnectionState {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.tls == nil {
		return ConnectionState{}
	}
	return c.tls.ConnectionState()
}

func (c *conn) engine() (*stdtls.Conn, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.tls == nil || !c.ready || c.ended {
		return nil, net.ErrClosed
	}
	return c.tls, nil
}

// transportShutdown is crypto/tls abandoning the transport.
func (c *conn) transportShutdown() {
	c.mu.Lock()
	c.transportClosed = true
	c.mu.Unlock()
}

func (c *conn) isGone() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ended || c.transportClosed
}

func (c *conn) stagingBound() int {
	if bound := c.tlsLayer.stagingBound; bound > 0 {
		return bound
	}
	return DefaultStagingBound
}

// compactLocked drops the ciphertext the engine read; it requires mu.
func (c *conn) compactLocked() {
	if c.readAt > 0 {
		c.pending = c.pending[:copy(c.pending, c.pending[c.readAt:])]
		c.readAt = 0
	}
}

// releasePendingLocked drops the ciphertext buffer once the engine drained
// it; it requires mu. The capacity goes with it: a turn's ciphertext is at
// most one socket read, and keeping it would cost every settled connection a
// buffer it never fills.
func (c *conn) releasePendingLocked() {
	c.pending, c.readAt = nil, 0
}
