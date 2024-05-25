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
	"io"
	"net"
	"sync"

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

	// Peek returns the next len(b) bytes without advancing the inbound buffer.
	// it's not concurrency-safe.
	Peek(b []byte) []byte

	// Discard advances the inbound buffer with next n bytes, returning the number of bytes discarded.
	// it's not concurrency-safe.
	Discard(n int) (int, error)

	// AvailableReadBytes returns a receive buffer data length.
	// it's not concurrency-safe.
	AvailableReadBytes() int

	// AvailableWriteBytes returns a send buffer data length.
	AvailableWriteBytes() int

	// ReadWriteCloser
	// it's not concurrency-safe.
	// Notice: non-blocking interface, should not be used as you use std.
	io.ReadWriteCloser

	// WriterTo
	// it's not concurrency-safe.
	// Notice: non-blocking interface, should not be used as you use std.
	io.WriterTo
}

type fdConn struct {
	fd          int                     // connection fd
	localAddr   net.Addr                // local address
	remoteAddr  net.Addr                // remote address
	loopIdx     int                     // event loop
	events      *Events                 // events
	opened      int32                   // opened event fired
	err         error                   // close error
	ctx         interface{}             // user-defined data
	mux         sync.Mutex              // outbound buffer mutex
	outbound    bytebuf.CompositeBuffer // outbound buffer
	inbound     bytebuf.CompositeBuffer // inbound buffer
	inboundTail []byte                  // inbound tail buffer
}

func (fc *fdConn) LocalAddr() net.Addr        { return fc.localAddr }
func (fc *fdConn) RemoteAddr() net.Addr       { return fc.remoteAddr }
func (fc *fdConn) Context() interface{}       { return fc.ctx }
func (fc *fdConn) SetContext(ctx interface{}) { fc.ctx = ctx }

func (fc *fdConn) WriteTo(w io.Writer) (n int64, err error) {
	if !fc.inbound.Empty() {
		if n, err = fc.inbound.WriteTo(w); nil != err {
			return
		}
	}

	if 0 != len(fc.inboundTail) {
		var sz int
		sz, err = w.Write(fc.inboundTail)
		n += int64(sz)
		fc.inboundTail = fc.inboundTail[sz:]
	}
	return
}

func (fc *fdConn) Read(b []byte) (n int, err error) {
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

func (fc *fdConn) Peek(b []byte) []byte {
	// inbound buffer size
	inboundLen := fc.inbound.Len()
	inboundTailLen := len(fc.inboundTail)

	if 0 == len(b) || 0 == (inboundLen+inboundTailLen) {
		return nil
	}

	if 0 == inboundTailLen {
		return fc.inbound.Peek(b)
	}

	data := fc.inbound.Peek(b)
	n := len(data)
	if n < len(b) {
		sz := copy(b[n:], fc.inboundTail)
		n += sz
	}

	return b[:n]
}

func (fc *fdConn) Discard(n int) (int, error) {

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

func (fc *fdConn) Close() error {
	fc.events.delConn(fc, io.ErrUnexpectedEOF)
	return nil
}

func (fc *fdConn) AvailableReadBytes() int {
	return fc.inbound.Len() + len(fc.inboundTail)
}

func (fc *fdConn) AvailableWriteBytes() int {
	fc.mux.Lock()
	defer fc.mux.Unlock()

	return fc.outbound.Len()
}
