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

package uio

import (
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/urpc/uio/internal/bytebuf"
)

// Conn is an interface of underlying connection.
type Conn interface {
	// LocalAddr is the connection's local socket address.
	LocalAddr() net.Addr

	// RemoteAddr is the connection's remote address.
	RemoteAddr() net.Addr

	// Context returns a user-defined context, it's not concurrency-safe.
	Context() interface{}

	// SetContext sets a user-defined context, it's not concurrency-safe.
	SetContext(ctx interface{})

	// SetLinger sets the behavior of Close on a connection which still
	// has data waiting to be sent or to be acknowledged.
	//
	// If sec < 0 (the default), the operating system finishes sending the
	// data in the background.
	//
	// If sec == 0, the operating system discards any unsent or
	// unacknowledged data.
	//
	// If sec > 0, the data is sent in the background as with sec < 0. On
	// some operating systems after sec seconds have elapsed any remaining
	// unsent data may be discarded.
	SetLinger(secs int) error

	// SetNoDelay controls whether the operating system should delay
	// packet transmission in hopes of sending fewer packets (Nagle's
	// algorithm).
	// The default is true (no delay), meaning that data is sent as soon as possible after a Write.
	SetNoDelay(nodelay bool) error

	// SetKeepAlive sets whether the operating system should send
	// keep-alive messages on the connection.
	SetKeepAlive(keepalive bool) error

	// SetKeepAlivePeriod tells operating system to send keep-alive messages on the connection
	// and sets period between TCP keep-alive probes.
	SetKeepAlivePeriod(secs int) error

	// SetReadBuffer sets the size of the operating system's
	// receive buffer associated with the connection.
	SetReadBuffer(size int) error

	// SetWriteBuffer sets the size of the operating system's
	// transmit buffer associated with the connection.
	SetWriteBuffer(size int) error

	// SetDeadline sets deadline for both read and write.
	// If it is time.Zero, SetDeadline will clear the deadlines.
	SetDeadline(t time.Time) error

	// SetReadDeadline sets the deadline for future Read calls.
	// When the user doesn't update the deadline and the deadline exceeds,
	// the connection will be closed.
	// If it is time.Zero, SetReadDeadline will clear the deadline.
	SetReadDeadline(t time.Time) error

	// SetWriteDeadline sets the deadline for future data writing.
	// If it is time.Zero, SetReadDeadline will clear the deadline.
	SetWriteDeadline(t time.Time) error

	// Peek returns the next len(b) bytes without advancing the inbound buffer.
	// it's not concurrency-safe.
	Peek(b []byte) []byte

	// Discard advances the inbound buffer with next n bytes, returning the number of bytes discarded.
	// it's not concurrency-safe.
	Discard(n int) (int, error)

	// InboundBuffered returns a inbound buffer data length.
	// it's not concurrency-safe.
	InboundBuffered() int

	// OutboundBuffered returns a outbound buffer data length.
	OutboundBuffered() int

	// WriterTo
	// it's not concurrency-safe.
	// Notice: non-blocking interface, should not be used as you use std.
	io.WriterTo

	// ReadWriteCloser
	// Read() it's not concurrency-safe.
	// Notice: non-blocking interface, should not be used as you use std.
	io.ReadWriteCloser

	// ByteWriter
	// Notice: non-blocking interface, should not be used as you use std.
	io.ByteWriter

	// StringWriter
	// Notice: non-blocking interface, should not be used as you use std.
	io.StringWriter

	// Writev "writev"-like batch write optimization.
	// Notice: non-blocking interface, should not be used as you use std.
	Writev(vec [][]byte) (int, error)

	// Flush writes any buffered data to the underlying connection.
	// Notice: non-blocking interface, should not be used as you use std.
	Flush() error
}

var errUnsupported = fmt.Errorf("unsupported method")

type commonConn struct {
	events      *Events                 // events
	loop        *eventLoop              // event loop
	localAddr   net.Addr                // local address
	remoteAddr  net.Addr                // remote address
	closed      int32                   // closed flag
	err         error                   // close error
	ctx         interface{}             // user-defined data
	mux         sync.Mutex              // outbound buffer & udpConns
	outbound    bytebuf.CompositeBuffer // outbound buffer
	inbound     bytebuf.CompositeBuffer // inbound buffer
	inboundTail []byte                  // inbound tail buffer
}

func (fc *commonConn) LocalAddr() net.Addr                { return fc.localAddr }
func (fc *commonConn) RemoteAddr() net.Addr               { return fc.remoteAddr }
func (fc *commonConn) Context() interface{}               { return fc.ctx }
func (fc *commonConn) SetContext(ctx interface{})         { fc.ctx = ctx }
func (fc *commonConn) SetDeadline(t time.Time) error      { return errUnsupported }
func (fc *commonConn) SetReadDeadline(t time.Time) error  { return errUnsupported }
func (fc *commonConn) SetWriteDeadline(t time.Time) error { return errUnsupported }

func (fc *commonConn) IsClosed() bool {
	return 0 != atomic.LoadInt32(&fc.closed)
}

func (fc *commonConn) WriteTo(w io.Writer) (n int64, err error) {
	if !fc.inbound.Empty() {
		n, err = fc.inbound.WriteTo(w)
	}

	if nil == err && 0 != len(fc.inboundTail) {
		var sz int
		sz, err = w.Write(fc.inboundTail)
		n += int64(sz)
		fc.inboundTail = fc.inboundTail[sz:]
	}

	return
}

func (fc *commonConn) Read(b []byte) (n int, err error) {
	if !fc.inbound.Empty() {
		if n, _ = fc.inbound.Read(b); n == len(b) {
			return
		}
	}

	if 0 != len(fc.inboundTail) {
		sz := copy(b[n:], fc.inboundTail)
		n += sz
		fc.inboundTail = fc.inboundTail[sz:]
	}
	return
}

func (fc *commonConn) Peek(b []byte) []byte {
	// inbound buffer size
	inboundLen := fc.inbound.Len()
	inboundTailLen := len(fc.inboundTail)

	if 0 == len(b) || 0 == (inboundLen+inboundTailLen) {
		return nil
	}

	if 0 == inboundLen {
		n := min(len(b), len(fc.inboundTail))
		return fc.inboundTail[:n]
	}

	data := fc.inbound.Peek(b)
	if n := len(data); n < len(b) {
		n += copy(b[n:], fc.inboundTail)
		return b[:n]
	}

	return data
}

func (fc *commonConn) Discard(n int) (int, error) {

	// inbound buffer size
	inboundLen := fc.inbound.Len()
	inboundTailLen := len(fc.inboundTail)

	// discard all inbound buffer
	if n < 0 || n > (inboundLen+inboundTailLen) {
		n = inboundLen + inboundTailLen
	}

	if 0 == n {
		return 0, nil
	}

	if 0 == inboundLen {
		fc.inboundTail = fc.inboundTail[n:]
		return n, nil
	}

	if n <= inboundLen {
		fc.inbound.Discard(n)
		return n, nil
	}

	fc.inbound.Discard(inboundLen)
	fc.inboundTail = fc.inboundTail[n-inboundLen:]
	return n, nil
}

func (fc *commonConn) InboundBuffered() int {
	return fc.inbound.Len() + len(fc.inboundTail)
}

func (fc *commonConn) OutboundBuffered() int {
	fc.mux.Lock()
	defer fc.mux.Unlock()
	return fc.outbound.Len()
}
