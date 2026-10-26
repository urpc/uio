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

// conn is the connection TLS middleware hands upward: what it reads is what the
// engine decrypted, what is written to it leaves encrypted, and it opens
// only once the handshake settled. Everything runs on the connection below's
// rounds, which UIO serializes: the handshake as a coroutine those rounds
// resume, the records straight in the round.
type conn struct {
	uio.InnerConn
	middleware *middleware
	next       uio.Handler

	// mu carries the engine's own state: the ciphertext pending, the
	// staging, the handshake's coroutine handles and the flags. crypto/tls
	// is called without it.
	mu sync.Mutex

	tls *stdtls.Conn
	rec recordCursor
	// pending is ciphertext the connection below delivered that the engine
	// has not read, from readAt on.
	pending []byte
	readAt  int
	// staging is what the engine decrypted, delivered no further than the
	// handler above reads; stageAt is the delivered prefix of it. Keeping the
	// prefix instead of reslicing forward lets the capacity be reused once the
	// handler drains it, which is what a bulk transfer does every round. The
	// engine decrypts straight into its spare capacity, so a round costs no
	// buffer of its own and no copy after the decryption.
	staging []byte
	stageAt int

	// resume continues the handshake's coroutine, and stop ends it; both
	// nil once the handshake settled. yield is the suspension inside the
	// coroutine's reads.
	resume func() (struct{}, bool)
	stop   func()
	yield  func(struct{}) bool

	ready           bool // the handshake settled
	opened          bool // the handler above learned of the connection
	ended           bool // this end asked for the close
	closed          bool // the connection below's close arrived
	transportClosed bool // crypto/tls closed the transport
	readEnded       bool // the peer's close_notify or a read failure
}

var _ uio.Conn = (*conn)(nil)

// open starts the handshake: its coroutine, and the deadline that bounds it.
func (c *conn) open() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	engine := &transport{c: c}
	if c.middleware.client {
		c.tls = stdtls.Client(engine, c.middleware.config)
	} else {
		c.tls = stdtls.Server(engine, c.middleware.config)
	}
	c.resume, c.stop = iter.Pull(handshake(c))
	c.mu.Unlock()
	// The deadline closes the connection below when it passes, and the close
	// ends the handshake; cleared once it settles.
	if timeout := c.middleware.handshakeTimeout; timeout >= 0 {
		_ = c.Conn.SetReadDeadline(time.Now().Add(c.handshakeTimeout()))
	}
	// The first flight needs no data: a client writes its ClientHello here,
	// and whatever it wrote leaves before the round ends. A server's
	// handshake asks to read at once and suspends until a round arrives.
	c.mu.Lock()
	resume := c.resume
	c.mu.Unlock()
	if resume != nil {
		if _, waiting := resume(); !waiting {
			c.settle()
		}
	}
}

// settle takes a handshake coroutine that has finished — its result is read
// out, the deadline it ran under is cleared, and a failure ends the
// connection with its cause. It runs on the connection's round.
func (c *conn) settle() {
	c.mu.Lock()
	c.resume, c.stop, c.yield = nil, nil, nil
	if err := c.tls.Handshake(); err != nil { // the settled result
		c.mu.Unlock()
		_ = c.Conn.CloseWith(err)
		return
	}
	c.ready = true
	_ = c.Conn.SetReadDeadline(time.Time{}) // the handshake's bound
	c.mu.Unlock()
}

func (c *conn) handshakeTimeout() time.Duration {
	if timeout := c.middleware.handshakeTimeout; timeout != 0 {
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

// data is a round of the connection below: the handshake's coroutine runs
// on what arrived until it settles, and from then on the round's bytes are
// decrypted and passed up, as far as the staging bound leaves room.
func (c *conn) data() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	chunk := c.Conn.PeekChunk()
	if c.resume != nil {
		// The round carries the handshake: it takes the bytes and continues,
		// until it needs more than arrived or it settles.
		c.pending = append(c.pending, chunk...)
		_, _ = c.Conn.Discard(len(chunk))
		resume := c.resume
		c.mu.Unlock()
		if _, waiting := resume(); waiting {
			return nil
		}
		c.settle()
		c.mu.Lock()
		if !c.ready || c.closed {
			c.mu.Unlock()
			return nil
		}
		// What this round fed the handshake may hold records: decrypt them
		// before the tail delivers.
		c.mu.Unlock()
		if err := c.pump(c.stagingBound()); err != nil {
			_ = c.Conn.CloseWith(err)
			return nil
		}
		c.mu.Lock()
		// still locked: the tail below opens the handler above
	} else if c.ready {
		bound := c.stagingBound()
		// A full staging leaves what arrived below unread, so the pause
		// travels to the wire; what it already holds still reaches the handler
		// above, whose reads make room again.
		if len(c.staging) < bound {
			c.pending = append(c.pending, chunk...)
			_, _ = c.Conn.Discard(len(chunk))
			c.mu.Unlock()
			if err := c.pump(bound); err != nil {
				_ = c.Conn.CloseWith(err)
				return nil
			}
			c.mu.Lock()
		}
	} else {
		c.mu.Unlock()
		return nil
	}

	first := c.ready && !c.opened
	c.opened = c.ready
	staged := c.stagedLocked()
	opened := c.opened && !c.closed
	c.mu.Unlock()
	if first {
		c.next.OnOpen(c)
	}
	// Plaintext goes up, and so does a turn that brought no bytes at all: a
	// Wake, or a timer a layer above armed, whose turn the handler above is
	// owed though it carries no data. A turn whose bytes only part-filled a
	// record is this layer's own business — the read that completes the
	// record delivers it.
	if opened && (staged > 0 || len(chunk) == 0) {
		return c.next.OnData(c)
	}
	return nil
}

// pump decrypts into staging, one record at a time, until the staging bound
// or the end of what arrived. It runs on the connection's round.
func (c *conn) pump(bound int) error {
	for {
		c.mu.Lock()
		if c.stagedLocked() >= bound || c.readEnded || !c.ready {
			c.mu.Unlock()
			return nil
		}
		// Nothing left for the engine to read: a Read here could only report
		// errWouldBlock, and growing the staging for it would cost every
		// settled connection a buffer it never fills.
		if c.readAt >= len(c.pending) {
			c.mu.Unlock()
			return nil
		}
		// Decrypt into the staging's spare capacity: the engine's Read needs
		// bytes to write, and the delivered prefix keeps its length until the
		// round extends it, so nothing observes unwritten space. The capacity
		// is grown here, under the lock, and the slice handed out is the round's
		// own: pump runs on the connection's round, which serialize.
		if spare := cap(c.staging) - len(c.staging); spare < readBufferSize {
			grown := make([]byte, len(c.staging), len(c.staging)+readBufferSize)
			copy(grown, c.staging)
			c.staging = grown
		}
		target := c.staging[len(c.staging):cap(c.staging)][:readBufferSize]
		c.mu.Unlock()
		n, err := c.tls.Read(target)
		c.mu.Lock()
		if n > 0 {
			c.staging = c.staging[:len(c.staging)+n]
		}
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

// closedBelow is the connection below's end: a handshake still running is
// stopped with it, and what the engine holds whole is the handler above's to
// read in OnClose, which always reaches it, with the cause kept.
func (c *conn) closedBelow(err error) {
	c.mu.Lock()
	c.closed = true
	stop := c.stop
	c.resume, c.stop, c.yield = nil, nil, nil
	first := c.ready && !c.opened
	c.opened = c.ready
	c.mu.Unlock()
	if stop != nil {
		stop() // a handshake waiting for data ends with the connection
	}
	if first {
		// The handshake settled with this close in flight: the handler above
		// learns of the connection before it ends, and reads what arrived.
		c.next.OnOpen(c)
	}
	c.next.OnClose(c, err)
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
	return c.closed || c.transportClosed
}

func (c *conn) stagingBound() int {
	if bound := c.middleware.stagingBound; bound > 0 {
		return bound
	}
	return DefaultStagingBound
}

// compactLocked drops the ciphertext the engine read and reclaims a fully
// delivered staging; it requires mu.
func (c *conn) compactLocked() {
	if c.readAt > 0 {
		c.pending = c.pending[:copy(c.pending, c.pending[c.readAt:])]
		c.readAt = 0
	}
	if c.stageAt >= len(c.staging) {
		c.staging, c.stageAt = c.staging[:0], 0
	}
}

// stagedLocked reports how many decrypted bytes the handler above has not
// taken yet; it requires mu.
func (c *conn) stagedLocked() int {
	return len(c.staging) - c.stageAt
}

// releasePendingLocked drops the ciphertext buffer once the engine drained
// it; it requires mu. The capacity goes with it: a round's ciphertext is at
// most one socket read, keeping it would cost every settled connection a
// buffer it never fills, and measured no throughput.
func (c *conn) releasePendingLocked() {
	c.pending, c.readAt = nil, 0
}

// State reports what the handshake settled on; before it settles, the zero
// ConnectionState.
func (c *conn) State() ConnectionState {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.tls == nil {
		return ConnectionState{}
	}
	return c.tls.ConnectionState()
}

// The reads are the handler above's, in its callbacks.

func (c *conn) Peek(b []byte) []byte {
	n := copy(b, c.staging[c.stageAt:])
	return b[:n]
}

func (c *conn) PeekChunk() []byte { return c.staging[c.stageAt:] }

func (c *conn) Discard(n int) (int, error) {
	if staged := c.staged(); n > staged {
		n = staged
	}
	c.stageAt += n
	if c.stageAt == len(c.staging) {
		// Fully delivered: the round that pumps next reuses the capacity.
		c.staging, c.stageAt = c.staging[:0], 0
	}
	if staged := c.staged(); staged < c.stagingBound() {
		// Room again: a wake runs the round that takes what is left unread
		// below, and the socket reads with it.
		_ = c.Conn.Wake()
	}
	return n, nil
}

func (c *conn) Read(b []byte) (int, error) {
	n := copy(b, c.staging[c.stageAt:])
	c.stageAt += n
	return n, nil // non-blocking, as a connection below reports nothing
}

func (c *conn) WriteTo(w io.Writer) (int64, error) {
	n, err := w.Write(c.staging[c.stageAt:])
	c.stageAt += n
	return int64(n), err
}

// staged reports how many decrypted bytes the handler above has not taken.
func (c *conn) staged() int { return len(c.staging) - c.stageAt }

func (c *conn) InboundBuffered() int {
	c.mu.Lock()
	held := c.stagedLocked() + len(c.pending) - c.readAt
	c.mu.Unlock()
	return held + c.Conn.InboundBuffered()
}

func (c *conn) ReserveInbound(int) error { return nil }

func (c *conn) OutboundBuffered() int { return c.Conn.OutboundBuffered() }

// The writes are the handler above's, from any goroutine; the handler above
// holds the connection only once the handshake settled.

func (c *conn) engine() (*stdtls.Conn, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.tls == nil || !c.ready {
		return nil, net.ErrClosed
	}
	return c.tls, nil
}

func (c *conn) Write(p []byte) (int, error) {
	engine, err := c.engine()
	if err != nil {
		return 0, err
	}
	return engine.Write(p)
}

func (c *conn) WriteByte(b byte) error {
	_, err := c.Write([]byte{b})
	return err
}

func (c *conn) WriteString(s string) (int, error) { return c.Write([]byte(s)) }

func (c *conn) Writev(vec [][]byte) (int, error) {
	total := 0
	for _, segment := range vec {
		n, err := c.Write(segment)
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

func (c *conn) WriteOwned(buffer *uio.Buffer) (int, error) {
	size := buffer.Len()
	_, err := c.Write(buffer.Bytes())
	uio.ReleaseBuffer(buffer)
	if err != nil {
		return 0, err
	}
	return size, nil
}

func (c *conn) ReserveOutbound(int) ([]byte, error) { return nil, uio.ErrReserveUnsupported }

func (c *conn) Flush() error { return c.Conn.Flush() }

func (c *conn) YieldRead() error {
	c.mu.Lock()
	idle := c.stagedLocked() == 0 && c.readAt == len(c.pending)
	c.mu.Unlock()
	if idle {
		return c.Conn.YieldRead()
	}
	return nil
}

// The closes are the handler above's; CloseWith sends the peer close_notify
// once what was accepted has left, then ends the connection below.

func (c *conn) CloseWith(err error) error {
	c.mu.Lock()
	if c.ended {
		c.mu.Unlock()
		return net.ErrClosed
	}
	c.ended = true
	ready := c.ready
	stop := c.stop
	c.resume, c.stop, c.yield = nil, nil, nil
	c.mu.Unlock()
	if stop != nil {
		stop() // a handshake in flight ends with the close's cause
	}
	if ready {
		_ = c.tls.Close() // close_notify, out through the connection below
	}
	return c.Conn.CloseWith(err)
}

func (c *conn) Close() error { return c.CloseWith(io.ErrUnexpectedEOF) }

func (c *conn) CloseWrite() error {
	engine, err := c.engine()
	if err != nil {
		return err
	}
	return engine.CloseWrite()
}
