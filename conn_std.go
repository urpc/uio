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
	"io"
	"net"
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

type fdConn struct {
	commonConn
	conn       net.Conn
	udp        *net.UDPConn
	udpSvr     *fdConn
	udpConns   map[string]*fdConn
	writeSig   chan struct{} // coalesced notification, not one ack per Write
	closeSig   chan struct{}
	closed     int32
	err        error
	mux        sync.Mutex
	outbound   bytebuf.CompositeBuffer
	writeMu    sync.Mutex // keeps close cleanup from racing the detached write batch
	writeBatch bytebuf.CompositeBuffer
	writeBytes int
	closing    atomic.Bool
	callbackMu sync.Mutex // serializes OnData, Wake, and OnClose
	touched    bool
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
func (fc *fdConn) isClosedOnLoop() bool             { return fc.IsClosed() }
func (fc *fdConn) afterRegister() {
	_ = fc.fireWriteEvent()
	_ = fc.fireReadEvent()
}
func (fc *fdConn) markTouched() bool {
	if fc.touched {
		return false
	}
	fc.touched = true
	return true
}
func (fc *fdConn) clearTouched() { fc.touched = false }

func (fc *fdConn) fireOnOpen() {
	if callback := fc.events.OnOpen; callback != nil {
		callback(fc)
	}
}

func (fc *fdConn) closeUnregistered() {
	fc.closing.Store(true)
	atomic.StoreInt32(&fc.closed, 1)
	if fc.conn != nil {
		_ = fc.conn.Close()
	}
	if fc.udp != nil {
		_ = fc.udp.Close()
	}
}

func (fc *fdConn) Fd() int {
	var rc syscall.Conn

	if fc.udp != nil {
		rc = net.PacketConn(fc.udp).(syscall.Conn)
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

func (fc *fdConn) applyDeadline(kind deadlineKind, deadline time.Time) error {
	if fc.isClosing() {
		return net.ErrClosed
	}
	var target net.Conn
	if fc.conn != nil {
		target = fc.conn
	} else if fc.udp != nil {
		target = fc.udp
	}
	if target == nil {
		return errUnsupported
	}
	switch kind {
	case deadlineBoth:
		return target.SetDeadline(deadline)
	case deadlineRead:
		return target.SetReadDeadline(deadline)
	default:
		return target.SetWriteDeadline(deadline)
	}
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

func (fc *fdConn) Write(p []byte) (n int, err error) {
	if fc.isClosing() {
		return 0, net.ErrClosed
	}

	if fc.udp != nil {

		if nil == fc.udpSvr && nil == fc.udpConns {
			// connected udp client.
			n, err = fc.udp.Write(p)
		} else {
			// udp child connection.
			n, err = fc.udp.WriteTo(p, fc.remoteAddr)
		}

		if n > 0 {
			fc.events.onSocketBytesWrite(fc, n)
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
		return fc.WriteOwned(bytebuf.CloneBuffer(p))
	}

	fc.mux.Lock()
	if fc.isClosing() {
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
	//case <-fc.closeSig:
	default:
	}

	return
}

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
		if fc.isClosing() {
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
	if fc.isClosing() {
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
	//case <-fc.closeSig:
	default:
	}

	return
}

func releaseStdBuffers(buffers []*bytebuf.Buffer) {
	for _, buffer := range buffers {
		bytebuf.ReleaseBuffer(buffer)
	}
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
	if fc.isClosing() {
		fc.mux.Unlock()
		bytebuf.ReleaseBuffer(owned)
		return 0, net.ErrClosed
	}
	if limit := fc.events.MaxOutboundBuffered; limit > 0 && fc.outbound.Len()+fc.writeBytes > limit-size {
		fc.mux.Unlock()
		bytebuf.ReleaseBuffer(owned)
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

func (fc *fdConn) drainOutbound(vec [][]byte) ([][]byte, int, error) {
	fc.writeMu.Lock()
	defer fc.writeMu.Unlock()
	vec = vec[:0]

	fc.mux.Lock()
	if fc.isClosing() {
		fc.mux.Unlock()
		return vec[:0], 0, net.ErrClosed
	}
	fc.writeBytes = fc.outbound.Len()
	if fc.writeBytes == 0 {
		fc.mux.Unlock()
		return vec[:0], 0, nil
	}
	// The detached batch is immutable while producers append to a fresh buffer.
	fc.outbound, fc.writeBatch = fc.writeBatch, fc.outbound
	fc.mux.Unlock()

	totalWritten := 0
	var writeErr error
	for !fc.writeBatch.Empty() {
		buffers, size := fc.writeBatch.PeekVecN(vec[:0], 8)
		netBuffers := net.Buffers(buffers)
		written, err := netBuffers.WriteTo(fc.conn)
		clear(buffers)
		vec = buffers[:0]
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
	return vec, totalWritten, writeErr
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

func (fc *fdConn) closeOnLoop(err error) {
	if fc.IsClosed() {
		return
	}
	fc.closing.Store(true)
	if fc.udpSvr != nil {
		fc.udpSvr.mux.Lock()
		delete(fc.udpSvr.udpConns, fc.remoteAddr.String())
		fc.udpSvr.mux.Unlock()
		if !atomic.CompareAndSwapInt32(&fc.closed, 0, 1) {
			return
		}
		fc.err = err
	} else {
		if fc.udpConns != nil {
			fc.mux.Lock()
			children := make([]*fdConn, 0, len(fc.udpConns))
			for _, child := range fc.udpConns {
				children = append(children, child)
			}
			fc.udpConns = nil
			fc.mux.Unlock()
			for _, child := range children {
				child.closeOnLoop(err)
			}
		}
		if !fc.fdClose(err) {
			return
		}
	}
	fc.callbackMu.Lock()
	defer fc.callbackMu.Unlock()
	fc.inbound.Reset()
	fc.inboundTail = nil
	if callback := fc.events.OnClose; callback != nil && !fc.internal {
		callback(fc, err)
	}
}

func (fc *fdConn) fdClose(err error) bool {
	if !atomic.CompareAndSwapInt32(&fc.closed, 0, 1) {
		return false
	}

	// save close reason
	fc.err = err

	// notify send/write loop connection will be closed.
	if fc.closeSig != nil {
		close(fc.closeSig)
	}

	// delete connection fd-mapping.
	fc.loop.delConn(fc)

	// Close the socket before waiting on mux so blocked writes are interrupted.
	switch {
	case nil != fc.conn:
		_ = fc.conn.Close()
	case nil != fc.udp:
		_ = fc.udp.Close()
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
	t := acquireTask(wakeTask, fc)
	if !fc.loop.submitTask(t) {
		releaseTask(t)
		return net.ErrClosed
	}
	return nil
}

func (fc *fdConn) runWakeTask() error {
	if fc.isClosing() {
		return net.ErrClosed
	}
	// A Wake callback must not race the blocking read goroutine's inbound view.
	fc.callbackMu.Lock()
	defer fc.callbackMu.Unlock()
	return fc.events.onData(fc)
}

func (fc *fdConn) runWriteTask(t *task) {
	if fc.isClosing() {
		return
	}
	fc.mux.Lock()
	_, _ = fc.outbound.Write(t.buf.Bytes())
	fc.mux.Unlock()
	select {
	case fc.writeSig <- struct{}{}:
	default:
	}
}

func (fc *fdConn) runFlushTask() error                { return fc.Flush() }
func (fc *fdConn) flushOnLoop() (int, error)          { return 0, nil }
func (fc *fdConn) updateInterest() error              { return nil }
func (fc *fdConn) handleTimeout(deadlineKind, uint64) {}

func (fc *fdConn) writeLoop() {
	callbackID := fc.events.enterExternalCallback()
	defer fc.events.finishExternalCallback(callbackID)
	var storage [8][]byte
	writeBuffers := storage[:0]
	for {
		select {
		case <-fc.closeSig:
			return
		case <-fc.writeSig:
			var written int
			var err error
			writeBuffers, written, err = fc.drainOutbound(writeBuffers)
			if written > 0 {
				fc.events.onSocketBytesWrite(fc, written)
			}
			if err != nil {
				fc.events.closeConn(fc, err)
				return
			}
		}
	}
}

func (fc *fdConn) readUDPLoop() {
	// This goroutine only enters user code through callbacks. Register it once
	// so callback-initiated Events.Close does not wait for its own return.
	callbackID := fc.events.enterExternalCallback()
	defer fc.events.finishExternalCallback(callbackID)

	var buffer = make([]byte, fc.events.MaxBufferSize)
	for {
		n, _, err := fc.udp.ReadFrom(buffer)
		if nil != err {
			// close on error.
			fc.events.closeConn(fc, err)
			return
		}

		fc.callbackMu.Lock()
		fc.inboundTail = buffer[:n]

		// trigger inbound event.
		fc.events.onSocketBytesRead(fc, n)

		// fire data callback.
		if err = fc.events.onData(fc); nil != err {
			fc.callbackMu.Unlock()
			// close on error.
			fc.events.closeConn(fc, err)
			break
		}

		// drop unread udp packet.
		_, _ = fc.Discard(-1)
		fc.callbackMu.Unlock()
	}
}

func (fc *fdConn) readLoop() {
	// Keep one callback marker for the lifetime of this dedicated read goroutine.
	callbackID := fc.events.enterExternalCallback()
	defer fc.events.finishExternalCallback(callbackID)

	var buffer = make([]byte, fc.events.MaxBufferSize)
	for {
		n, err := fc.conn.Read(buffer)
		if nil != err {
			// close on error.
			fc.events.closeConn(fc, err)
			return
		}

		// fire data callback.
		fc.callbackMu.Lock()
		fc.inboundTail = buffer[:n]

		// trigger inbound event.
		fc.events.onSocketBytesRead(fc, n)

		if err = fc.events.onData(fc); nil != err {
			fc.callbackMu.Unlock()
			// close on error.
			fc.events.closeConn(fc, err)
			break
		}

		if len(fc.inboundTail) > 0 {
			if limit := fc.events.MaxInboundBuffered; limit > 0 && fc.InboundBuffered() > limit {
				fc.inboundTail = nil
				fc.callbackMu.Unlock()
				fc.events.closeConn(fc, ErrInboundOverflow)
				return
			}
			_, _ = fc.inbound.Write(fc.inboundTail)
			fc.inboundTail = fc.inboundTail[:0]
		}

		// try flush outbound buffer.
		if fc.events.WriteBufferedThreshold > 0 {
			_ = fc.Flush()
		}
		fc.callbackMu.Unlock()
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

func (fc *fdConn) fireReadEvent() error {
	fc.events.callbackWG.Add(1)
	// udp client
	if nil != fc.udp {
		go fc.readUDPLoop()
	} else {
		go fc.readLoop()
	}
	return nil
}

func (fc *fdConn) listenUDP() error {
	// UDP peer callbacks all run on this listener goroutine.
	callbackID := fc.events.enterExternalCallback()
	defer fc.events.finishExternalCallback(callbackID)

	var buffer = make([]byte, fc.events.MaxBufferSize)

	for {
		n, addr, err := fc.udp.ReadFrom(buffer)
		if nil != err {
			_ = fc.CloseWith(err)
			return err
		}

		// remote address.
		var rAddr = addr.String()

		// udp server
		fc.mux.Lock()
		udpConn, ok := fc.udpConns[rAddr]
		if !ok {
			udpConn = &fdConn{}
			udpConn.udp = fc.udp
			udpConn.localAddr = fc.localAddr
			udpConn.remoteAddr = addr
			udpConn.loop = fc.loop
			udpConn.events = fc.events
			udpConn.udpSvr = fc
			udpConn.udpConns = nil // udp connection always nil

			fc.udpConns[rAddr] = udpConn
		}
		fc.mux.Unlock()
		if !ok {
			// fire udp on-open event.
			udpConn.callbackMu.Lock()
			if onOpen := fc.events.OnOpen; nil != onOpen {
				onOpen(udpConn)
			}
			udpConn.callbackMu.Unlock()
		}
		if udpConn.isClosing() {
			continue
		}

		udpConn.callbackMu.Lock()
		udpConn.inboundTail = buffer[:n]

		// trigger inbound event
		fc.events.onSocketBytesRead(udpConn, n)

		// fire udp on-data event.
		err = fc.events.onData(udpConn)

		// drop unread udp packet.
		_, _ = udpConn.Discard(-1)
		udpConn.callbackMu.Unlock()

		if nil != err {
			// close udp connection
			_ = udpConn.CloseWith(err)
		}
	}
}
