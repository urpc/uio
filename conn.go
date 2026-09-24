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
	"errors"
	"fmt"
	"io"
	"net"
	"sync/atomic"
	"time"

	"github.com/urpc/uio/internal/bytebuf"
)

// Conn is a connection managed by Events. On native Unix, socket
// I/O and callbacks run in one serialized connection task outside the event
// loop. The stdio/Windows backend uses dedicated blocking I/O goroutines and
// serializes lifecycle callbacks per connection. Writes from other goroutines
// are safe and retain no caller-owned data after returning. Close and
// CloseWith return after the close request is accepted; OnClose is final.
// Native UDP writes from outside the owning event loop wait for the datagram's
// send result. Any other event-loop callback gets ErrUDPWriteOnEventLoop
// instead of risking a cross-loop wait cycle.
type Conn interface {
	// LocalAddr is the connection's local socket address.
	LocalAddr() net.Addr

	// RemoteAddr is the connection's remote address.
	RemoteAddr() net.Addr

	// Userdata returns user-defined connection data. It may be called outside a
	// callback, but callers must serialize it with SetUserdata.
	Userdata() any

	// SetUserdata replaces user-defined connection data. It may be called outside
	// a callback, but callers must serialize it with Userdata.
	SetUserdata(value any)

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
	// If it is time.Zero, SetWriteDeadline will clear the deadline.
	SetWriteDeadline(t time.Time) error

	// Peek returns the next len(b) bytes without advancing the inbound buffer.
	// It may only be called from a connection callback.
	Peek(b []byte) []byte

	// PeekChunk returns the first contiguous inbound chunk without advancing it.
	// The returned slice is valid only until Discard or the callback returns.
	// It may only be called from a connection callback.
	PeekChunk() []byte

	// Discard advances the inbound buffer with next n bytes, returning the number of bytes discarded.
	// It may only be called from a connection callback.
	Discard(n int) (int, error)

	// InboundBuffered returns a inbound buffer data length.
	// It may only be called from a connection callback.
	InboundBuffered() int

	// OutboundBuffered returns payload bytes accepted by this connection but not
	// yet written to the socket.
	OutboundBuffered() int

	// WriterTo
	// It may only be called from a connection callback.
	// Notice: non-blocking interface, should not be used as you use std.
	io.WriterTo

	// ReadWriteCloser
	// Read may only be called from a connection callback. Write and Close are
	// safe from other goroutines.
	// Stream writes are non-blocking; native UDP writes from outside the owning
	// loop wait for that loop's non-blocking datagram send result.
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

	// WriteOwned submits buffer without copying and always consumes its
	// ownership, including when it returns an error. For UDP, one buffer is sent
	// as one datagram. Native UDP writes from another event loop return
	// ErrUDPWriteOnEventLoop.
	WriteOwned(buffer *Buffer) (int, error)

	// Flush schedules buffered data for writing without waiting for socket I/O.
	// Writes accepted before Flush remain ordered before later writes.
	Flush() error

	// Wake schedules one OnData callback after previously submitted tasks.
	Wake() error

	// YieldRead ends the current read round and schedules remaining buffered or
	// socket data for a later OnData callback. It may only be called from a
	// connection callback.
	YieldRead() error

	// CloseWith asynchronously releases the connection on its owning event loop.
	// On Unix, unsent accepted payload is reported as UnflushedError.
	CloseWith(err error) error
}

var errUnsupported = fmt.Errorf("unsupported method")

var (
	ErrOutboundOverflow    = errors.New("uio: outbound buffer limit exceeded")
	ErrInboundOverflow     = errors.New("uio: inbound buffer limit exceeded")
	ErrUnflushedData       = errors.New("uio: connection closed with unflushed data")
	ErrDialOnEventLoop     = errors.New("uio: Dial cannot run on an event loop")
	ErrUDPWriteOnEventLoop = errors.New("uio: UDP write cannot wait on another event loop")
)

// UnflushedError reports payload accepted by the framework but not sent before
// the connection was closed.
type UnflushedError struct {
	Remaining int64
}

func (err UnflushedError) Error() string {
	return fmt.Sprintf("%v: %d bytes", ErrUnflushedData, err.Remaining)
}

func (err UnflushedError) Unwrap() error { return ErrUnflushedData }

// commonConn contains transport-independent identity and inbound storage.
// inboundTail is a borrowed current-read slice; inbound owns only bytes that a
// callback left unread before that slice had to be reused.
type commonConn struct {
	events      *Events                     // events
	loop        *eventLoop                  // event loop
	localAddr   net.Addr                    // local address
	remoteAddr  net.Addr                    // remote address
	userdata    atomic.Pointer[userdataBox] // user-defined data
	inboundGoid atomic.Int64                // current inbound callback owner
	inbound     bytebuf.CompositeBuffer     // inbound buffer
	inboundTail []byte                      // inbound tail buffer
	internal    bool                        // framework-owned endpoint, not a user connection
}

// userdataBox lets atomic.Pointer publish an arbitrary interface value.
type userdataBox struct{ value any }

func (fc *commonConn) LocalAddr() net.Addr  { return fc.localAddr }
func (fc *commonConn) RemoteAddr() net.Addr { return fc.remoteAddr }
func (fc *commonConn) Userdata() any {
	value := fc.userdata.Load()
	if value == nil {
		return nil
	}
	return value.value
}
func (fc *commonConn) SetUserdata(value any) { fc.userdata.Store(&userdataBox{value: value}) }

// Callback ownership is a contract check for borrowed inbound slices, not a
// lock. Native tasks and std callbackMu serialize access before entering here.
// Nested internal callback helpers leave the outer owner's scope intact.
func (fc *commonConn) beginInboundCallback() bool {
	id := currentGoroutineID()
	if fc.inboundGoid.Load() == id {
		return false
	}
	fc.inboundGoid.Store(id)
	return true
}

func (fc *commonConn) endInboundCallback(started bool) {
	if started && fc.inboundGoid.Load() == currentGoroutineID() {
		fc.inboundGoid.Store(0)
	}
}
func (fc *commonConn) SetDeadline(t time.Time) error      { return errUnsupported }
func (fc *commonConn) SetReadDeadline(t time.Time) error  { return errUnsupported }
func (fc *commonConn) SetWriteDeadline(t time.Time) error { return errUnsupported }

func (fc *commonConn) WriteTo(w io.Writer) (n int64, err error) {
	fc.assertInboundAccess()
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
	fc.assertInboundAccess()
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

// Peek reads across persistent inbound blocks and the borrowed current-read
// tail without advancing either. It returns a direct slice when the first
// source is already contiguous, otherwise it fills b.
func (fc *commonConn) Peek(b []byte) []byte {
	fc.assertInboundAccess()
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

func (fc *commonConn) PeekChunk() []byte {
	fc.assertInboundAccess()
	if !fc.inbound.Empty() {
		return fc.inbound.PeekChunk()
	}
	return fc.inboundTail
}

// Discard advances persistent blocks before the borrowed tail so stream order
// remains intact. A negative n consumes both sources completely.
func (fc *commonConn) Discard(n int) (int, error) {
	fc.assertInboundAccess()

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
	fc.assertInboundAccess()
	return fc.inbound.Len() + len(fc.inboundTail)
}

func (fc *commonConn) assertInboundAccess() {
	// Unregistered connections are used by low-level helpers and tests before an
	// owner exists. Registered connections always have both fields.
	if fc.loop == nil || fc.events == nil {
		return
	}
	if fc.inboundGoid.Load() == currentGoroutineID() {
		return
	}
	panic("uio: inbound access outside a connection callback")
}
