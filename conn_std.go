//go:build windows || stdio

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
	"io"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"github.com/urpc/uio/internal/bytebuf"
	"github.com/urpc/uio/internal/poller"
)

// Keep tiny writes coalesced while bounding payload work done under mux.
const stdOwnedWriteThreshold = 4 << 10
const stdWriteVecLimit = 16

// fdConn uses one blocking reader and one blocking writer per stream. mux
// protects producer admission and counters; writeMu protects the detached
// batch while the writer performs blocking net.Conn writes; callbackMu keeps
// OnData, Wake, and OnClose serialized for the connection.
// stdUDPState carries the datagram fields off the stream connections, which
// are the vast majority: sock is the shared packet socket of a server's peers
// or a connected client's socket, svr links a logical peer back to its
// server, and peers is the server's logical-peer map.
type stdUDPState struct {
	sock  *net.UDPConn
	svr   *fdConn
	peers map[string]*fdConn

	// Datagrams are sent on whichever goroutine writes them, and the bytes
	// they report wait in outboundPending for whoever holds outboundClaim; see
	// reportOutbound.
	outboundPending atomic.Int64
	outboundClaim   atomic.Bool
}

type fdConn struct {
	commonConn
	conn           net.Conn
	udp            *stdUDPState
	writeSig       chan struct{} // coalesced "output exists" wake; see writeLoop
	closed         int32
	closeDelivered int32
	err            error
	mux            sync.Mutex
	outbound       bytebuf.CompositeBuffer
	writeMu        sync.Mutex // keeps close cleanup from racing the detached write batch
	writeBatch     bytebuf.CompositeBuffer
	writeBytes     int
	closing        atomic.Bool
	callbackMu     sync.Mutex // serializes OnOpen, OnData, Wake, and OnClose
	internal       bool       // framework-owned endpoint, not a user connection

	// opened records that OnOpen has run, which a connection's read goroutine
	// does first; only an opened connection gets OnClose. wakesAfterOpen
	// counts the Wakes that arrived before OnOpen, for the read goroutine to
	// deliver right after it, one OnData each. Both are guarded by callbackMu.
	opened         bool
	wakesAfterOpen int

	// readHeld stops the read goroutine once callbacks leave
	// MaxInboundBuffered bytes unconsumed; it waits on readResume until a Wake
	// consumes below the limit, the read deadline passes, or the connection
	// closes. readHeld is guarded by callbackMu; readResume, buffered by one so
	// no signal waits for the reader, is made with the stream connection. The
	// held wait reads no socket, so it keeps the read deadline the socket
	// enforces on a Read: deadlineMu orders each setter's two updates, so
	// readDeadline and the socket always hold the same value.
	readHeld     bool
	readResume   chan struct{}
	deadlineMu   sync.Mutex
	readDeadline time.Time

	// writeShut records a CloseWrite, guarded by mux: no more output is
	// accepted, and the writer shuts the socket's writing half once what was
	// accepted before it is sent, then sets writeShutDone. readEOF records,
	// on a connection that allows half-open, that the peer shut its own, and
	// readEnded, set under callbackMu, that callbacks consumed everything it
	// sent; the side that sets the second of writeShutDone and readEnded
	// closes the connection.
	writeShut     bool
	writeShutDone atomic.Bool
	readEOF       atomic.Bool
	readEnded     atomic.Bool
}

func (fc *fdConn) IsClosed() bool { return atomic.LoadInt32(&fc.closed) != 0 }

func (fc *fdConn) OutboundBuffered() int {
	fc.mux.Lock()
	defer fc.mux.Unlock()
	return fc.outbound.Len() + fc.writeBytes
}

func (fc *fdConn) initialInterest() poller.Interest { return poller.Readable }
func (fc *fdConn) setInterest(poller.Interest)      {}
func (fc *fdConn) currentInterest() poller.Interest { return poller.Readable }
func (fc *fdConn) isClosing() bool                  { return fc.closing.Load() || fc.IsClosed() }
func (fc *fdConn) beginShutdown()                   {}
func (fc *fdConn) isDatagram() bool                 { return fc.udp != nil }
func (fc *fdConn) isClosedOnLoop() bool             { return fc.IsClosed() }

// afterRegister starts the connection's read goroutine, which runs OnOpen and
// then starts the writer; see openOnReader. OnOpen stays off the event loop,
// so it may Dial and a slow one holds up no other connection.
func (fc *fdConn) afterRegister() {
	_ = fc.fireReadEvent()
}

// openOnReader runs OnOpen on the connection's read goroutine before its
// first read, then starts the writer, so output written in OnOpen leaves after
// it returns, as it did when OnOpen ran on the loop. Wakes that came first are
// delivered right after, one OnData each. It reports whether the connection is still open; a
// connection closed before its OnOpen gets neither OnOpen nor OnClose.
func (fc *fdConn) openOnReader() bool {
	fc.callbackMu.Lock()
	if fc.isClosing() {
		fc.callbackMu.Unlock()
		return false
	}
	fc.fireOnOpen()
	fc.opened = true
	for ; fc.wakesAfterOpen > 0 && !fc.isClosing(); fc.wakesAfterOpen-- {
		started := fc.beginInboundCallback()
		err := fc.events.onData(fc)
		fc.endInboundCallback(started)
		if err != nil {
			fc.requestClose(err)
		}
	}
	closing := fc.isClosing()
	fc.callbackMu.Unlock()
	if closing {
		return false
	}
	_ = fc.fireWriteEvent()
	return true
}

func (fc *fdConn) fireOnOpen() {
	if callback := fc.events.OnOpen; callback != nil {
		started := fc.beginInboundCallback()
		callback(fc)
		fc.endInboundCallback(started)
	}
}

func (fc *fdConn) closeUnregistered() {
	fc.closing.Store(true)
	atomic.StoreInt32(&fc.closed, 1)
	if fc.conn != nil {
		_ = fc.conn.Close()
	}
	if fc.udp != nil {
		_ = fc.udp.sock.Close()
	}
}

func (fc *fdConn) RemoteAddr() net.Addr {
	if fc.remoteAddr.IsValid() {
		if fc.isDatagram() {
			return net.UDPAddrFromAddrPort(fc.remoteAddr)
		}
		return net.TCPAddrFromAddrPort(fc.remoteAddr)
	}
	// Non-IP peers (Unix sockets) keep their address object in the pair.
	if fc.addr != nil {
		return fc.addr.remote
	}
	return nil
}

func (fc *fdConn) Fd() int {
	var rc syscall.Conn

	if fc.udp != nil {
		rc = net.PacketConn(fc.udp.sock).(syscall.Conn)
	} else {
		rc = fc.conn.(syscall.Conn)
	}

	sc, err := rc.SyscallConn()
	if err != nil {
		return -1
	}

	var fd int
	err = sc.Control(func(h uintptr) { fd = int(h) })
	if nil != err {
		return -1
	}
	return fd
}

func (fc *fdConn) SetLinger(secs int) error {
	if fc.isClosing() {
		return net.ErrClosed
	}
	if tcpConn, ok := fc.conn.(*net.TCPConn); ok {
		return tcpConn.SetLinger(secs)
	}
	return errUnsupported
}

func (fc *fdConn) SetNoDelay(nodelay bool) error {
	if fc.isClosing() {
		return net.ErrClosed
	}
	if tcpConn, ok := fc.conn.(*net.TCPConn); ok {
		return tcpConn.SetNoDelay(nodelay)
	}
	return errUnsupported
}

func (fc *fdConn) SetKeepAlive(keepalive bool) error {
	if fc.isClosing() {
		return net.ErrClosed
	}
	if tcpConn, ok := fc.conn.(*net.TCPConn); ok {
		return tcpConn.SetKeepAlive(keepalive)
	}
	return errUnsupported
}

func (fc *fdConn) SetKeepAlivePeriod(secs int) error {
	if fc.isClosing() {
		return net.ErrClosed
	}
	if tcpConn, ok := fc.conn.(*net.TCPConn); ok {
		if err := tcpConn.SetKeepAlive(true); nil != err {
			return err
		}

		if err := tcpConn.SetKeepAlivePeriod(time.Duration(secs) * time.Second); nil != err {
			_ = tcpConn.SetKeepAlive(false)
			return err
		}
		return nil
	}
	return errUnsupported
}

func (fc *fdConn) SetReadBuffer(size int) error {
	if fc.isClosing() {
		return net.ErrClosed
	}
	if tcpConn, ok := fc.conn.(*net.TCPConn); ok {
		return tcpConn.SetReadBuffer(size)
	}
	return errUnsupported
}

func (fc *fdConn) SetWriteBuffer(size int) error {
	if fc.isClosing() {
		return net.ErrClosed
	}
	if tcpConn, ok := fc.conn.(*net.TCPConn); ok {
		return tcpConn.SetWriteBuffer(size)
	}
	return errUnsupported
}

func (fc *fdConn) SetDeadline(deadline time.Time) error {
	return fc.applyDeadline(deadlineBoth, deadline)
}

func (fc *fdConn) SetReadDeadline(deadline time.Time) error {
	return fc.applyDeadline(deadlineRead, deadline)
}

func (fc *fdConn) SetWriteDeadline(deadline time.Time) error {
	return fc.applyDeadline(deadlineWrite, deadline)
}

// readDeadlineNotedForTest, when a test sets it, runs between setting a read
// deadline on the socket and recording it for the held wait.
var readDeadlineNotedForTest func()

// heldDeadlineExpiredForTest, when a test sets it, runs once a held wait has
// found its read deadline passed and before it acts on that.
var heldDeadlineExpiredForTest func()

// applyDeadline delegates directly to the blocking net.Conn; unlike native
// timers, the Go network poller owns interruption and reset semantics. A read
// deadline the socket accepted is also recorded for a held read, which reads
// no socket. deadlineMu orders the two updates against other setters and
// against the held wait's expiry, which closes the connection under it: a
// setter either finds the connection closing or replaces the deadline before
// the wait can act on the old one.
func (fc *fdConn) applyDeadline(kind deadlineKind, deadline time.Time) error {
	if fc.isClosing() {
		return net.ErrClosed
	}
	var target net.Conn
	if fc.conn != nil {
		target = fc.conn
	} else if fc.udp != nil {
		target = fc.udp.sock
	}
	if target == nil {
		return errUnsupported
	}
	fc.deadlineMu.Lock()
	defer fc.deadlineMu.Unlock()
	if fc.isClosing() {
		return net.ErrClosed
	}
	var err error
	switch kind {
	case deadlineBoth:
		err = target.SetDeadline(deadline)
	case deadlineRead:
		err = target.SetReadDeadline(deadline)
	default:
		err = target.SetWriteDeadline(deadline)
	}
	if err != nil || kind == deadlineWrite || fc.conn == nil {
		return err
	}
	if readDeadlineNotedForTest != nil {
		readDeadlineNotedForTest()
	}
	fc.readDeadline = deadline
	// Wake a held wait to re-time against the new deadline.
	select {
	case fc.readResume <- struct{}{}:
	default:
	}
	return nil
}

func (fc *fdConn) applySocketOption(socketOptionKind, int) error { return errUnsupported }

func (fc *fdConn) WriteByte(b byte) error {
	var bb [1]byte
	bb[0] = b
	_, err := fc.Write(bb[:])
	return err
}

func (fc *fdConn) WriteString(s string) (n int, err error) {
	var data = unsafe.Slice(unsafe.StringData(s), len(s))
	return fc.Write(data)
}

// Write copies tiny payloads into the shared outbound tail and transfers larger
// payloads as owned blocks, keeping allocation and copying outside mux.
func (fc *fdConn) Write(p []byte) (n int, err error) {
	if fc.isClosing() {
		return 0, net.ErrClosed
	}

	if fc.udp != nil {

		if nil == fc.udp.svr && nil == fc.udp.peers {
			// connected udp client.
			n, err = fc.udp.sock.Write(p)
		} else {
			// udp child connection.
			n, err = fc.udp.sock.WriteTo(p, net.UDPAddrFromAddrPort(fc.remoteAddr))
		}

		if n > 0 {
			fc.reportOutbound(n)
		}
		return
	}
	if len(p) == 0 {
		return 0, nil
	}
	if len(p) >= stdOwnedWriteThreshold {
		if limit := fc.events.MaxOutboundBuffered; limit > 0 && len(p) > limit {
			return 0, ErrOutboundOverflow
		}
		owned := bytebuf.CloneBuffer(p)
		n, err := fc.WriteOwned(owned)
		if errors.Is(err, ErrOutboundOverflow) {
			// The clone never entered the queue, so Write still owns it.
			bytebuf.ReleaseBuffer(owned)
		}
		return n, err
	}

	fc.mux.Lock()
	if fc.isClosing() || fc.writeShut {
		fc.mux.Unlock()
		return 0, net.ErrClosed
	}
	if limit := fc.events.MaxOutboundBuffered; limit > 0 && fc.outbound.Len()+fc.writeBytes > limit-len(p) {
		fc.mux.Unlock()
		return 0, ErrOutboundOverflow
	}
	n, err = fc.outbound.Write(p)
	fc.mux.Unlock()

	if nil != err {
		// unreachable here.
		return
	}

	select {
	case fc.writeSig <- struct{}{}:
	default:
	}

	return
}

// reportOutbound runs OnOutbound for a datagram send. Datagrams are sent on
// whichever goroutine writes them, so the calls for one connection go through
// a claim: a sender adds its bytes and reports them itself if it takes the
// claim, and otherwise leaves them to the holder, which reports until nothing
// is pending, so the calls never overlap and a send made inside OnOutbound is
// reported once that call returns. Nobody waits for the claim, so OnOutbound
// callbacks that write to each other cannot deadlock.
func (fc *fdConn) reportOutbound(n int) {
	if fc.events.OnOutbound == nil {
		return
	}
	udp := fc.udp
	udp.outboundPending.Add(int64(n))
	for udp.outboundClaim.CompareAndSwap(false, true) {
		fc.drainOutboundReports()
		// Bytes added between the last drain and the release found the
		// claim taken; look again.
		if udp.outboundPending.Load() == 0 {
			return
		}
	}
}

// drainOutboundReports reports pending bytes while holding the claim, and
// releases it even when OnOutbound panics.
func (fc *fdConn) drainOutboundReports() {
	udp := fc.udp
	defer udp.outboundClaim.Store(false)
	for {
		pending := udp.outboundPending.Swap(0)
		if pending == 0 {
			return
		}
		fc.events.OnOutbound(fc, int(pending))
	}
}

// Writev preserves large source segments as separate owned blocks when the
// fixed writer vector can carry them; larger segment counts collapse into one
// block to avoid retaining an unbounded pointer list.
func (fc *fdConn) Writev(vec [][]byte) (n int, err error) {

	if fc.isClosing() {
		return 0, net.ErrClosed
	}

	if fc.udp != nil {
		return 0, errUnsupported
	}

	total := 0
	segments := 0
	for _, segment := range vec {
		if len(segment) > int(^uint(0)>>1)-total {
			return 0, ErrOutboundOverflow
		}
		total += len(segment)
		if len(segment) > 0 {
			segments++
		}
	}
	if total == 0 {
		return 0, nil
	}
	if total >= stdOwnedWriteThreshold {
		if limit := fc.events.MaxOutboundBuffered; limit > 0 && total > limit {
			return 0, ErrOutboundOverflow
		}
		var inline [8]*bytebuf.Buffer
		owned := inline[:0]
		if segments > len(inline) {
			owned = append(owned, bytebuf.CloneBuffers(vec, total))
		} else {
			for _, segment := range vec {
				if len(segment) > 0 {
					owned = append(owned, bytebuf.CloneBuffer(segment))
				}
			}
		}
		fc.mux.Lock()
		if fc.isClosing() || fc.writeShut {
			fc.mux.Unlock()
			releaseStdBuffers(owned)
			return 0, net.ErrClosed
		}
		if limit := fc.events.MaxOutboundBuffered; limit > 0 && fc.outbound.Len()+fc.writeBytes > limit-total {
			fc.mux.Unlock()
			releaseStdBuffers(owned)
			return 0, ErrOutboundOverflow
		}
		for _, buffer := range owned {
			fc.outbound.AppendOwned(buffer)
		}
		fc.mux.Unlock()
		select {
		case fc.writeSig <- struct{}{}:
		default:
		}
		return total, nil
	}
	fc.mux.Lock()
	if fc.isClosing() || fc.writeShut {
		fc.mux.Unlock()
		return 0, net.ErrClosed
	}
	if limit := fc.events.MaxOutboundBuffered; limit > 0 && fc.outbound.Len()+fc.writeBytes > limit-total {
		fc.mux.Unlock()
		return 0, ErrOutboundOverflow
	}
	n, err = fc.outbound.Writev(vec)
	fc.mux.Unlock()

	if nil != err {
		// unreachable here.
		return
	}

	select {
	case fc.writeSig <- struct{}{}:
	default:
	}

	return
}

func releaseStdBuffers(buffers []*bytebuf.Buffer) {
	for _, buffer := range buffers {
		bytebuf.ReleaseBuffer(buffer)
	}
}

// ReserveOutbound is unsupported: the dedicated writer goroutine may detach
// outbound at any moment, so bytes cannot be left in it to be filled later.
func (fc *fdConn) ReserveOutbound(int) ([]byte, error) {
	if fc.isClosing() {
		return nil, net.ErrClosed
	}
	return nil, ErrReserveUnsupported
}

func (fc *fdConn) WriteOwned(owned *Buffer) (n int, err error) {
	if owned == nil {
		return 0, nil
	}
	size := owned.Len()
	if size == 0 {
		bytebuf.ReleaseBuffer(owned)
		return 0, nil
	}
	if fc.isClosing() {
		bytebuf.ReleaseBuffer(owned)
		return 0, net.ErrClosed
	}
	if fc.udp != nil {
		defer bytebuf.ReleaseBuffer(owned)
		return fc.Write(owned.Bytes())
	}

	fc.mux.Lock()
	if fc.isClosing() || fc.writeShut {
		fc.mux.Unlock()
		bytebuf.ReleaseBuffer(owned)
		return 0, net.ErrClosed
	}
	if limit := fc.events.MaxOutboundBuffered; limit > 0 && fc.outbound.Len()+fc.writeBytes > limit-size {
		fc.mux.Unlock()
		// An overflowed buffer was never accepted, so it remains the caller's.
		return 0, ErrOutboundOverflow
	}
	fc.outbound.AppendOwned(owned)
	fc.mux.Unlock()

	select {
	case fc.writeSig <- struct{}{}:
	default:
	}
	return size, nil
}

func (fc *fdConn) Flush() error {
	if fc.isClosing() {
		return net.ErrClosed
	}
	// Writes are FIFO in outbound. A coalesced wake is enough to make the
	// dedicated writer drain everything accepted before this call.
	select {
	case fc.writeSig <- struct{}{}:
	default:
	}
	return nil
}

// stdWriteScratch is the fixed scratch one drain fills and hands over. It
// lives in a pool rather than on the writer goroutine's stack: both the
// vector and the net.Buffers wrapper reach net.Buffers.WriteTo as values
// whose address the compiler must assume escapes, which would move the vector
// to the heap once per connection, for the connection's whole life, and
// allocate the wrapper once per drain. Pooled, an idle connection holds
// neither.
type stdWriteScratch struct {
	vec  [stdWriteVecLimit][]byte
	bufs net.Buffers
}

var stdWriteScratchPool = sync.Pool{New: func() any { return new(stdWriteScratch) }}

// drainOutbound swaps the producer buffer into writeBatch before blocking on
// the network. Producers immediately continue on a fresh CompositeBuffer;
// writeMu prevents close cleanup from returning in-flight blocks to the pool.
func (fc *fdConn) drainOutbound() (int, error) {
	scratch := stdWriteScratchPool.Get().(*stdWriteScratch)
	defer func() {
		clear(scratch.vec[:])
		scratch.bufs = nil
		stdWriteScratchPool.Put(scratch)
	}()
	fc.writeMu.Lock()
	defer fc.writeMu.Unlock()

	fc.mux.Lock()
	if fc.isClosing() {
		fc.mux.Unlock()
		return 0, net.ErrClosed
	}
	fc.writeBytes = fc.outbound.Len()
	if fc.writeBytes == 0 {
		fc.mux.Unlock()
		return 0, nil
	}
	// The detached batch is immutable while producers append to a fresh buffer.
	fc.outbound, fc.writeBatch = fc.writeBatch, fc.outbound
	fc.mux.Unlock()

	totalWritten := 0
	var writeErr error
	for !fc.writeBatch.Empty() {
		buffers, size := fc.writeBatch.PeekVecN(scratch.vec[:], len(scratch.vec))
		scratch.bufs = buffers
		written, err := scratch.bufs.WriteTo(fc.conn)
		clear(buffers)
		if written > 0 {
			n := int(written)
			fc.writeBatch.Discard(n)
			totalWritten += n
			fc.mux.Lock()
			fc.writeBytes -= n
			fc.mux.Unlock()
		}
		if err != nil {
			writeErr = err
			break
		}
		if written < int64(size) {
			writeErr = io.ErrShortWrite
			break
		}
	}
	fc.writeBatch.Reset()
	fc.mux.Lock()
	fc.writeBytes = 0
	fc.mux.Unlock()
	return totalWritten, writeErr
}

func (fc *fdConn) Close() error {
	return fc.CloseWith(io.ErrUnexpectedEOF)
}

func (fc *fdConn) CloseWith(err error) error {
	return fc.enqueueClose(err)
}

func (fc *fdConn) enqueueClose(err error) error {
	if !fc.closing.CompareAndSwap(false, true) {
		return net.ErrClosed
	}
	// Resource release and OnClose still belong to the logical event loop.
	t := acquireTask(closeTask, fc)
	t.err = err
	if fc.loop == nil || !fc.loop.submitTask(t) {
		releaseTask(t)
		return net.ErrClosed
	}
	return nil
}

func (fc *fdConn) requestClose(err error) { _ = fc.enqueueClose(err) }

// closeOnLoop owns resource teardown and detaches UDP children. The user
// callback is scheduled separately so a slow OnClose cannot block the logical
// event loop or race an active OnData callback.
func (fc *fdConn) closeOnLoop(err error) {
	if fc.IsClosed() {
		return
	}
	fc.closing.Store(true)
	if fc.udp != nil && fc.udp.svr != nil {
		fc.udp.svr.mux.Lock()
		delete(fc.udp.svr.udp.peers, fc.remoteAddr.String())
		fc.udp.svr.mux.Unlock()
		// The reason publishes before the flag: a reader that sees closed
		// observes err without further synchronization.
		fc.err = err
		if !atomic.CompareAndSwapInt32(&fc.closed, 0, 1) {
			return
		}
	} else {
		if fc.udp != nil && fc.udp.peers != nil {
			fc.mux.Lock()
			children := make([]*fdConn, 0, len(fc.udp.peers))
			for _, child := range fc.udp.peers {
				children = append(children, child)
			}
			fc.udp.peers = nil
			fc.mux.Unlock()
			for _, child := range children {
				child.closeOnLoop(err)
			}
		}
		if !fc.fdClose(err) {
			return
		}
	}
	fc.scheduleCloseCallback(err)
}

// testHookUDPChildCreated runs on the listener goroutine after a UDP child
// was created and published, before its OnOpen. Tests use it to pin the
// close-while-opening race; it is nil outside tests.
var testHookUDPChildCreated func(*fdConn)

func (fc *fdConn) scheduleCloseCallback(err error) {
	fc.events.callbackWG.Add(1)
	go func() {
		defer fc.events.callbackWG.Done()
		fc.callbackMu.Lock()
		defer fc.callbackMu.Unlock()
		if callback := fc.events.OnClose; callback != nil && !fc.internal && fc.opened {
			// A child whose OnOpen had not run when its server closed is
			// opened afterwards by the listener; whichever of the two close
			// callbacks finds it opened delivers it, exactly once.
			if !atomic.CompareAndSwapInt32(&fc.closeDelivered, 0, 1) {
				return
			}
			fc.inbound.Reset()
			fc.inboundTail = nil
			started := fc.beginInboundCallback()
			callback(fc, err)
			fc.endInboundCallback(started)
		}
	}()
}

// fdClose wins physical teardown once, interrupts blocking I/O by closing the
// socket first, then waits for the detached writer batch before recycling any
// pooled blocks.
func (fc *fdConn) fdClose(err error) bool {
	// The reason publishes before the flag: a reader that sees closed
	// observes err without further synchronization.
	fc.err = err
	if !atomic.CompareAndSwapInt32(&fc.closed, 0, 1) {
		return false
	}

	// Wake the writer so it observes the close and stops. A notification that
	// is already pending is just as good a wake as this one, so a full channel
	// needs no second signal.
	if fc.writeSig != nil {
		select {
		case fc.writeSig <- struct{}{}:
		default:
		}
	}
	// A reader held by MaxInboundBuffered waits for a resume; the close is one.
	if fc.readResume != nil {
		select {
		case fc.readResume <- struct{}{}:
		default:
		}
	}

	// delete connection fd-mapping.
	fc.loop.delConn(fc)

	// Close the socket before waiting on mux so blocked writes are interrupted.
	switch {
	case nil != fc.conn:
		_ = fc.conn.Close()
	case nil != fc.udp:
		_ = fc.udp.sock.Close()
	}

	// Writers may still reference pooled outbound blocks until they return.
	fc.writeMu.Lock()
	fc.mux.Lock()
	fc.outbound.Reset()
	fc.writeBatch.Reset()
	fc.writeBytes = 0
	fc.mux.Unlock()
	fc.writeMu.Unlock()

	return true
}

func (fc *fdConn) Wake() error {
	if fc.isClosing() {
		return net.ErrClosed
	}
	// The wake still travels through the loop's queue, so it starts after
	// previously submitted tasks and one callback is scheduled per call. The
	// loop only dequeues it; see runWakeTask for why the callback itself runs
	// elsewhere.
	t := acquireTask(wakeTask, fc)
	if !fc.loop.submitTask(t) {
		releaseTask(t)
		return net.ErrClosed
	}
	return nil
}

func (fc *fdConn) YieldRead() error { return fc.Wake() }

// runWakeTask runs on the event loop, in submission order, and hands the
// callback to its own goroutine instead of waiting for the connection's
// callback mutex: a callback that dials synchronously waits for the loop to
// register the dialed connection, so the loop waiting here would close the
// cycle. The callback still cannot race the blocking read goroutine or
// OnClose — callbackMu serializes them — and it counts as a lifecycle callback
// so Wait joins it.
func (fc *fdConn) runWakeTask() error {
	fc.events.callbackWG.Add(1)
	go fc.deliverWake()
	return nil
}

func (fc *fdConn) deliverWake() {
	defer fc.events.callbackWG.Done()
	if fc.isClosing() {
		return
	}
	// A Wake callback must not race the blocking read goroutine's inbound view.
	fc.callbackMu.Lock()
	defer fc.callbackMu.Unlock()
	if fc.isClosing() {
		// The connection closed while this callback waited its turn.
		return
	}
	if !fc.opened {
		// OnOpen has not run yet; the read goroutine delivers this after it.
		fc.wakesAfterOpen++
		return
	}
	started := fc.beginInboundCallback()
	defer fc.endInboundCallback(started)
	if err := fc.events.onData(fc); err != nil {
		fc.requestClose(err)
		return
	}
	fc.noteReadEnded()
	if fc.readHeld && fc.inbound.Len() < fc.events.MaxInboundBuffered {
		fc.readHeld = false
		// A close may have filled the slot already; either signal resumes.
		select {
		case fc.readResume <- struct{}{}:
		default:
		}
	}
}

func (fc *fdConn) flushOnLoop() (int, error)          { return 0, nil }
func (fc *fdConn) handleTimeout(deadlineKind, uint64) {}

// writeLoop drains every coalesced notification to completion. writeSig is a
// level signal for "output exists", not one acknowledgement per Write.
// writeLoop drains every coalesced notification to completion until the
// connection closes: a close wakes it through the same notification channel,
// and the drain then reports the closed transport.
func (fc *fdConn) writeLoop() {
	defer fc.events.callbackWG.Done()
	for range fc.writeSig {
		written, err := fc.drainOutbound()
		if written > 0 {
			fc.events.onSocketBytesWrite(fc, written)
		}
		if err == nil {
			err = fc.shutWriteIfDrained()
		}
		if err != nil {
			fc.events.closeConn(fc, err)
			return
		}
	}
}

// halfCloser is a net.Conn that can shut its writing half, as *net.TCPConn
// and *net.UnixConn can.
type halfCloser interface{ CloseWrite() error }

// CloseWrite shuts the writing half once the writer has sent everything
// accepted before it; see Conn.CloseWrite.
func (fc *fdConn) CloseWrite() error {
	if fc.udp != nil {
		return errUnsupported
	}
	if _, ok := fc.conn.(halfCloser); !ok {
		return errUnsupported
	}
	fc.mux.Lock()
	if fc.isClosing() {
		fc.mux.Unlock()
		return net.ErrClosed
	}
	if fc.writeShut {
		fc.mux.Unlock()
		return nil
	}
	fc.writeShut = true
	fc.mux.Unlock()
	select {
	case fc.writeSig <- struct{}{}:
	default:
	}
	return nil
}

// shutWriteIfDrained runs on the writer after a drain and shuts the writing
// half a CloseWrite requested once nothing accepted is left. Output is no
// longer accepted then, so what is left only drains.
func (fc *fdConn) shutWriteIfDrained() error {
	if fc.writeShutDone.Load() {
		return nil
	}
	fc.mux.Lock()
	ready := fc.writeShut && fc.outbound.Len() == 0 && fc.writeBytes == 0
	fc.mux.Unlock()
	if !ready {
		return nil
	}
	if fc.readEnded.Load() {
		// Both halves end: closing sends the FIN behind what was sent.
		fc.writeShutDone.Store(true)
		fc.requestClose(io.EOF)
		return nil
	}
	err := fc.conn.(halfCloser).CloseWrite()
	fc.writeShutDone.Store(true)
	if err != nil {
		return err
	}
	if fc.readEnded.Load() {
		fc.requestClose(io.EOF)
	}
	return nil
}

// noteReadEnded runs under callbackMu after an OnData past the peer's end of
// stream. Once callbacks have consumed everything the peer sent, the reading
// half is over and the connection closes if its writing half is shut too.
func (fc *fdConn) noteReadEnded() {
	if !fc.readEOF.Load() || fc.readEnded.Load() || !fc.inbound.Empty() || len(fc.inboundTail) != 0 {
		return
	}
	fc.readEnded.Store(true)
	if fc.writeShutDone.Load() {
		fc.requestClose(io.EOF)
	}
}

// onReadEOF ends the read goroutine of a half-open connection whose peer shut
// its writing half: OnData runs once more, so the application sees Read
// report io.EOF, and the connection closes once its own writing half is shut
// as well and callbacks consumed what the peer sent.
func (fc *fdConn) onReadEOF() {
	fc.callbackMu.Lock()
	if fc.isClosing() {
		fc.callbackMu.Unlock()
		return
	}
	fc.readEOF.Store(true)
	started := fc.beginInboundCallback()
	err := fc.events.onData(fc)
	fc.endInboundCallback(started)
	if err == nil {
		fc.noteReadEnded()
	}
	fc.callbackMu.Unlock()
	if err != nil {
		fc.events.closeConn(fc, err)
	}
}

// Read is commonConn.Read that reports io.EOF once a half-open connection's
// peer has shut its writing half and the buffered bytes are consumed.
func (fc *fdConn) Read(b []byte) (int, error) {
	n, err := fc.commonConn.Read(b)
	if n == 0 && err == nil && fc.readEOF.Load() && fc.inbound.Empty() && len(fc.inboundTail) == 0 {
		return 0, io.EOF
	}
	return n, err
}

// ReserveInbound implements Conn.ReserveInbound.
func (fc *fdConn) ReserveInbound(n int) error {
	if fc.isDatagram() || n <= 0 {
		return nil
	}
	return fc.reserveInbound(n, fc.events.MaxInboundBuffered)
}

// readUDPLoop owns a connected UDP socket. A datagram is borrowed for one
// callback and always discarded before the next blocking read.
func (fc *fdConn) readUDPLoop() {
	defer fc.events.callbackWG.Done()

	holder := fc.events.readPool.Get()
	defer fc.events.readPool.Put(holder)
	buffer := holder.bytes
	for {
		n, _, err := fc.udp.sock.ReadFrom(buffer)
		if nil != err {
			// close on error.
			fc.events.closeConn(fc, err)
			return
		}

		fc.callbackMu.Lock()
		if fc.isClosing() {
			// OnClose may already have run; see readLoop.
			fc.callbackMu.Unlock()
			return
		}
		fc.inboundTail = buffer[:n]

		// trigger inbound event.
		fc.events.onSocketBytesRead(fc, n)

		// fire data callback.
		started := fc.beginInboundCallback()
		if err = fc.events.onData(fc); nil != err {
			fc.endInboundCallback(started)
			fc.callbackMu.Unlock()
			// close on error.
			fc.events.closeConn(fc, err)
			break
		}

		// drop unread udp packet.
		_, _ = fc.Discard(-1)
		fc.endInboundCallback(started)
		fc.callbackMu.Unlock()
	}
}

// readLoop delivers n > 0 before a simultaneous terminal error, as required by
// io.Reader. callbackMu protects both the borrowed slice and retained inbound
// storage for the full callback lifetime.
func (fc *fdConn) readLoop() {
	defer fc.events.callbackWG.Done()

	holder := fc.events.readPool.Get()
	defer fc.events.readPool.Put(holder)
	buffer := holder.bytes
	for {
		n, readErr := fc.conn.Read(buffer)
		if n > 0 {
			// A Reader may return payload and a terminal error together. Deliver the
			// payload before closing so the final stream segment is not lost.
			fc.callbackMu.Lock()
			if fc.isClosing() {
				// OnClose may already have run: a close requested while this
				// read was returning ends the stream here.
				fc.callbackMu.Unlock()
				return
			}
			fc.inboundTail = buffer[:n]
			fc.events.onSocketBytesRead(fc, n)
			started := fc.beginInboundCallback()
			if callbackErr := fc.events.onData(fc); callbackErr != nil {
				fc.endInboundCallback(started)
				fc.callbackMu.Unlock()
				fc.events.closeConn(fc, callbackErr)
				return
			}
			if len(fc.inboundTail) > 0 {
				_, _ = fc.inbound.Write(fc.inboundTail)
				fc.inboundTail = fc.inboundTail[:0]
				if limit := fc.events.MaxInboundBuffered; limit > 0 && fc.inbound.Len() >= limit {
					fc.readHeld = true
				}
			}
			held := fc.readHeld
			fc.endInboundCallback(started)
			fc.callbackMu.Unlock()
			if held && !fc.waitHeldRead() {
				return
			}
		}
		if readErr != nil {
			if readErr == io.EOF && fc.events.AllowHalfOpen {
				fc.onReadEOF()
				return
			}
			fc.events.closeConn(fc, readErr)
			return
		}
	}
}

// waitHeldRead keeps a held read in the socket until a Wake consumes below
// MaxInboundBuffered, and reports whether the read loop goes on. A close ends
// it, and so does the read deadline, which closes the connection as an
// expired Read would. A changed deadline also signals readResume, so the wait
// re-times against it.
func (fc *fdConn) waitHeldRead() bool {
	var timer *time.Timer
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	for {
		var expired <-chan time.Time
		fc.deadlineMu.Lock()
		deadline := fc.readDeadline
		if !deadline.IsZero() && !time.Now().Before(deadline) {
			// Closed under the lock, so a setter that replaces this deadline
			// either comes first or finds the connection closing. The close
			// request only queues a task, so holding the lock costs nothing.
			if heldDeadlineExpiredForTest != nil {
				heldDeadlineExpiredForTest()
			}
			fc.events.closeConn(fc, os.ErrDeadlineExceeded)
			fc.deadlineMu.Unlock()
			return false
		}
		fc.deadlineMu.Unlock()
		if !deadline.IsZero() {
			wait := time.Until(deadline)
			if timer == nil {
				timer = time.NewTimer(wait)
			} else {
				timer.Reset(wait)
			}
			expired = timer.C
		}
		select {
		case <-fc.readResume:
		case <-expired:
			continue // rechecked above, against a deadline moved meanwhile
		}
		if fc.isClosing() {
			return false
		}
		fc.callbackMu.Lock()
		held := fc.readHeld
		fc.callbackMu.Unlock()
		if !held {
			return true
		}
	}
}

func (fc *fdConn) fireWriteEvent() error {
	if nil == fc.conn {
		return nil // udp client nothing to do.
	}

	fc.events.callbackWG.Add(1)
	go fc.writeLoop()

	return nil
}

// fireReadEvent starts the connection's read goroutine, which runs OnOpen
// before its first read; see openOnReader.
func (fc *fdConn) fireReadEvent() error {
	fc.events.callbackWG.Add(1)
	go func() {
		if !fc.openOnReader() {
			fc.events.callbackWG.Done()
			return
		}
		if fc.udp != nil {
			fc.readUDPLoop() // connected UDP
		} else {
			fc.readLoop()
		}
	}()
	return nil
}

// listenUDP owns the shared packet socket and logical peer map. Each packet is
// borrowed for one callback and discarded before the receive buffer is reused.
func (fc *fdConn) listenUDP() error {
	// UDP peer callbacks all run on this listener goroutine.
	defer fc.events.callbackWG.Done()

	holder := fc.events.readPool.Get()
	defer fc.events.readPool.Put(holder)
	buffer := holder.bytes

	for {
		n, addr, err := fc.udp.sock.ReadFrom(buffer)
		if nil != err {
			_ = fc.CloseWith(err)
			return err
		}

		// remote address.
		var rAddr = addr.String()

		// udp server
		fc.mux.Lock()
		udpConn, ok := fc.udp.peers[rAddr]
		// A closed server leaves peers nil; a lookup on the nil map misses,
		// and inserting would panic. The datagram is dropped instead.
		if !ok && fc.udp.peers != nil {
			udpConn = &fdConn{}
			udpConn.udp = &stdUDPState{sock: fc.udp.sock, svr: fc}
			udpConn.addr = fc.addr
			udpConn.setRemoteAddr(addr)
			udpConn.loop = fc.loop
			udpConn.events = fc.events

			fc.udp.peers[rAddr] = udpConn
		}
		fc.mux.Unlock()
		if udpConn == nil {
			continue
		}
		if !ok {
			if testHookUDPChildCreated != nil {
				testHookUDPChildCreated(udpConn)
			}
			// fire udp on-open event.
			udpConn.callbackMu.Lock()
			if onOpen := fc.events.OnOpen; nil != onOpen {
				started := udpConn.beginInboundCallback()
				onOpen(udpConn)
				udpConn.endInboundCallback(started)
			}
			udpConn.opened = true
			if atomic.LoadInt32(&udpConn.closed) == 1 {
				// The server closed this child before its OnOpen ran, and the
				// close callback skipped it as unopened; deliver it now.
				udpConn.scheduleCloseCallback(udpConn.err)
			}
			udpConn.callbackMu.Unlock()
		}
		if udpConn.isClosing() {
			continue
		}

		udpConn.callbackMu.Lock()
		if udpConn.isClosing() {
			// The peer closed while this packet waited for its callback
			// mutex, and OnClose may already have run.
			udpConn.callbackMu.Unlock()
			continue
		}
		udpConn.inboundTail = buffer[:n]

		// trigger inbound event
		fc.events.onSocketBytesRead(udpConn, n)

		// fire udp on-data event.
		started := udpConn.beginInboundCallback()
		err = fc.events.onData(udpConn)

		// drop unread udp packet.
		_, _ = udpConn.Discard(-1)
		udpConn.endInboundCallback(started)
		udpConn.callbackMu.Unlock()

		if nil != err {
			// close udp connection
			_ = udpConn.CloseWith(err)
		}
	}
}
