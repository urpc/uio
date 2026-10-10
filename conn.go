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
	"net/netip"
	"sync/atomic"
	"time"

	"github.com/urpc/uio/internal/bytebuf"
)

// Conn is a connection managed by Events. On native Unix, socket I/O and
// callbacks run in the connection's turn, one serialized task outside the
// event loops, which only collect readiness. The stdio/Windows backend uses
// dedicated blocking I/O goroutines and serializes lifecycle callbacks per
// connection. Writes from other goroutines are safe and retain no
// caller-owned data after returning. Close and CloseWith return after the
// close request is accepted; OnClose is final. Native UDP writes send the
// datagram before returning, from any goroutine.
type Conn interface {
	// LocalAddr is the connection's local socket address.
	LocalAddr() net.Addr

	// RemoteAddr is the connection's remote address.
	RemoteAddr() net.Addr

	// Userdata returns user-defined connection data. It may be called outside a
	// callback, but callers must serialize it with SetUserdata. Native
	// OnOutbound may run beside the connection's other callbacks.
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
	//
	// Peek, PeekChunk, Discard, InboundBuffered, Read, WriteTo and the slices
	// they return read the connection's inbound buffer: call them only from
	// inside the current invocation of this connection's OnOpen, OnInbound,
	// OnData or OnClose callback — never from OnOutbound, never from another
	// goroutine, and never after the callback returned. The slices are
	// borrowed from the buffer, and any call that consumes inbound data —
	// Read, WriteTo, Discard — or the callback's return may invalidate them
	// as the consumed blocks go back to the pool: copy anything that must
	// outlive that.
	Peek(b []byte) []byte

	// PeekChunk returns the first contiguous inbound chunk without advancing it.
	// The returned slice is borrowed: Read, WriteTo, Discard or the callback's
	// return may invalidate it. See Peek for the calling scope.
	PeekChunk() []byte

	// Discard advances the inbound buffer with next n bytes, returning the number of bytes discarded.
	// See Peek for the calling scope.
	Discard(n int) (int, error)

	// InboundBuffered returns a inbound buffer data length.
	// See Peek for the calling scope.
	InboundBuffered() int

	// OutboundBuffered returns payload bytes accepted by this connection but not
	// yet written to the socket.
	OutboundBuffered() int

	// WriterTo
	// WriteTo drains inbound data; see Peek for the calling scope.
	// Notice: non-blocking interface, should not be used as you use std.
	io.WriterTo

	// ReadWriteCloser
	// Read is inbound access; see Peek for the calling scope. Write and Close
	// are safe from other goroutines.
	// Stream writes are non-blocking; a native UDP write makes one
	// non-blocking send and returns its result.
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

	// WriteOwned submits buffer without copying and consumes its ownership on
	// every return path except ErrOutboundOverflow, which accepts nothing and
	// returns the buffer to the caller to resubmit or release. A direct write
	// that partially reached the socket and then could not queue its suffix
	// reports io.ErrShortWrite instead: that buffer is consumed and the stream
	// is being closed, so the partial frame is never resubmitted. For UDP, one
	// buffer is sent as one datagram.
	WriteOwned(buffer *Buffer) (int, error)

	// ReserveOutbound appends n bytes to the connection's outbound queue and
	// returns them for the caller to fill in place, so an encoder writes
	// straight into the queue instead of into a buffer that is copied again.
	// It works only in a native stream connection's own callback, where the
	// queue is sent after the callback returns; elsewhere, or while output
	// written from another goroutine is queued for or being sent by a write
	// turn, it reserves nothing and returns ErrReserveUnsupported, and the
	// caller writes another way. Once a reservation succeeds, output from other
	// goroutines waits for the end of the connection's turn.
	// Every reserved byte must be written before the callback returns and
	// before the next Flush.
	ReserveOutbound(n int) ([]byte, error)

	// Flush schedules buffered data for writing without waiting for socket I/O.
	// Writes accepted before Flush remain ordered before later writes. When
	// another sender is sending the connection's output, Flush leaves the
	// bytes to it and returns.
	Flush() error

	// Wake schedules one OnData callback after previously submitted tasks.
	Wake() error

	// YieldRead ends the current read round and schedules remaining buffered or
	// socket data for a later OnData callback. It may only be called from a
	// connection callback.
	YieldRead() error

	// CloseWith requests the close and returns. On native Unix the
	// connection's turn releases it once the callback that closed it has
	// returned, and reports unsent accepted payload as UnflushedError.
	CloseWith(err error) error
}

var errUnsupported = fmt.Errorf("unsupported method")

var (
	ErrOutboundOverflow   = errors.New("uio: outbound buffer limit exceeded")
	ErrInboundOverflow    = errors.New("uio: inbound buffer limit exceeded")
	ErrUnflushedData      = errors.New("uio: connection closed with unflushed data")
	ErrDialOnEventLoop    = errors.New("uio: Dial cannot run on an event loop")
	ErrReserveUnsupported = errors.New("uio: outbound reservation needs the connection's own callback")
)

// Deprecated: nothing returns ErrUDPWriteOnEventLoop any more; a UDP write
// sends its datagram from any goroutine.
var ErrUDPWriteOnEventLoop = errors.New("uio: UDP write cannot wait on another event loop")

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
// callback left unread before that slice had to be reused. Fields every data
// callback touches come first so they share a cache line; the addresses are
// read only on request.
type commonConn struct {
	events      *Events                 // events
	loop        *eventLoop              // event loop
	inboundLive atomic.Bool             // race-build only: an inbound callback is in progress; see conn_access_debug.go
	turn        uint8                   // turn-owned bits: turnCorked, turnFlushing, sticky turnHangup
	internal    bool                    // framework-owned endpoint, not a user connection
	userdata    any                     // user-defined data; see Conn.SetUserdata
	inboundTail []byte                  // inbound tail buffer
	inbound     bytebuf.CompositeBuffer // inbound buffer
	// addr carries the address objects LocalAddr and RemoteAddr hand back.
	// Accepted connections share the listener's single pair, and a connection
	// whose peer is not an IP address (Unix sockets) gets its own pair through
	// copy-on-write, so a shared pair is never mutated in place.
	addr       *addrPair
	remoteAddr netip.AddrPort // IP peer address, kept by value
}

// addrPair holds the address objects a connection returns for LocalAddr and
// RemoteAddr. A listener's pair is shared by every connection it accepts; a
// non-IP peer address forces a per-connection copy.
type addrPair struct {
	local  net.Addr
	remote net.Addr
}

func (fc *commonConn) LocalAddr() net.Addr {
	if fc.addr == nil {
		return nil
	}
	return fc.addr.local
}

// setLocalAddr stores a connection's own local address; it is only for the
// cold paths that have no shared object to point at.
func (fc *commonConn) setLocalAddr(addr net.Addr) { fc.addr = &addrPair{local: addr} }

// setRemoteAddr stores the peer address. IP peers keep the compact value form;
// any other address (Unix sockets) is kept as the object itself, copied out of
// a possibly shared pair first.
func (fc *commonConn) setRemoteAddr(addr net.Addr) {
	if addr == nil {
		return
	}
	if addrPort := remoteAddrFrom(addr); addrPort.IsValid() {
		fc.remoteAddr = addrPort
		return
	}
	if fc.addr == nil {
		fc.addr = &addrPair{remote: addr}
		return
	}
	pair := *fc.addr // the pair may be shared with a listener or a parent connection
	pair.remote = addr
	fc.addr = &pair
}

// remoteAddrFrom converts an IP net.Addr to the value form the connection
// stores; non-IP addresses report the zero value.
func remoteAddrFrom(addr net.Addr) netip.AddrPort {
	switch a := addr.(type) {
	case *net.TCPAddr:
		if a != nil {
			return a.AddrPort()
		}
	case *net.UDPAddr:
		if a != nil {
			return a.AddrPort()
		}
	}
	return netip.AddrPort{}
}

// Userdata is read on every data callback. Callers already serialize it with
// SetUserdata, so it is a plain field: boxing it for atomic publication cost a
// pointer chase, and a cache miss, per callback. Lifecycle and data callbacks
// are ordered by the connection's turn, but native OnOutbound may run beside
// them, so a value read there must not be replaced in those callbacks.
func (fc *commonConn) Userdata() any         { return fc.userdata }
func (fc *commonConn) SetUserdata(value any) { fc.userdata = value }

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
