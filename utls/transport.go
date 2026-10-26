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
	"net"
	"time"
)

// transport is what crypto/tls reads and writes the connection through. To
// crypto/tls it is a net.Conn: Read serves the ciphertext the connection
// below delivered, at most the rest of the record under way so crypto/tls's
// own buffer stays record-sized. During the handshake a Read that runs out
// suspends the handshake's coroutine and returns when the next round
// resumes it with more; afterwards it never waits, and reports errWouldBlock
// instead, which is how the record path runs without a goroutine. Write
// sends a record on the connection below.
type transport struct {
	c *conn
}

// Read is crypto/tls reading the transport; see the type's comment.
func (t *transport) Read(p []byte) (int, error) {
	c := t.c
	c.mu.Lock()
	for {
		if c.readAt < len(c.pending) {
			n := t.c.rec.take(p, c.pending[c.readAt:])
			c.readAt += n
			if c.readAt == len(c.pending) {
				c.releasePendingLocked()
			}
			c.mu.Unlock()
			return n, nil
		}
		if c.transportClosed || c.ended {
			c.mu.Unlock()
			return 0, errClosed
		}
		if c.yield == nil {
			c.mu.Unlock()
			return 0, errWouldBlock
		}
		// Hand control back to the round that runs us; the next round goes
		// on from here with what it carries, or the stop ends us.
		suspend := c.yield
		c.mu.Unlock()
		waiting := suspend(struct{}{})
		c.mu.Lock()
		if !waiting {
			c.mu.Unlock()
			return 0, net.ErrClosed
		}
	}
}

// Write is crypto/tls writing a record; the connection below copies it, as
// crypto/tls reuses its buffer for the next record. A close this end asked
// for still sends its goodbye out; only a connection that is gone refuses.
func (t *transport) Write(p []byte) (int, error) {
	if t.c.isGone() {
		return 0, errClosed
	}
	if _, err := t.c.ctx.Write(p); err != nil {
		return 0, err
	}
	return len(p), nil
}

// Close is crypto/tls abandoning the transport.
func (t *transport) Close() error {
	t.c.transportShutdown()
	return nil
}

func (t *transport) LocalAddr() net.Addr              { return t.c.ctx.Lower().LocalAddr() }
func (t *transport) RemoteAddr() net.Addr             { return t.c.ctx.Lower().RemoteAddr() }
func (t *transport) SetDeadline(time.Time) error      { return nil }
func (t *transport) SetReadDeadline(time.Time) error  { return nil }
func (t *transport) SetWriteDeadline(time.Time) error { return nil }

// recordCursor follows the TLS record framing of the ciphertext stream, so
// that Read can stop at the end of each record: a five-byte header whose
// last two bytes are the length of the body that follows.
type recordCursor struct {
	header [5]byte
	// headerLen is how much of the current record's header has been handed
	// over, and bodyLeft how much of its body has not.
	headerLen int
	bodyLeft  int
}

// take copies into p what src holds of the current record, as much as p has
// room for, and reports how many bytes it copied.
func (r *recordCursor) take(p, src []byte) int {
	if len(src) > len(p) {
		src = src[:len(p)]
	}
	n := 0
	if r.headerLen < len(r.header) {
		k := copy(r.header[r.headerLen:], src)
		r.headerLen += k
		n = k
		if r.headerLen < len(r.header) {
			return copy(p, src[:n])
		}
		r.bodyLeft = int(r.header[3])<<8 | int(r.header[4])
	}
	k := min(r.bodyLeft, len(src)-n)
	r.bodyLeft -= k
	n += k
	if r.bodyLeft == 0 {
		r.headerLen = 0
	}
	return copy(p, src[:n])
}
