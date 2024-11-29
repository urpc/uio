//go:build (linux || darwin || netbsd || freebsd || openbsd || dragonfly) && !stdio

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
	"syscall"
	"unsafe"

	"github.com/urpc/uio/internal/socket"
	"golang.org/x/sys/unix"
)

type fdConn struct {
	commonConn                    // base connection
	fd         int                // connection fd
	isUdp      bool               // is udp server or udp client
	rUdpAddr   syscall.Sockaddr   // remote udp address
	udpSvr     *fdConn            // owner udp server
	udpConns   map[string]*fdConn // child udp connections
}

func (fc *fdConn) Fd() int {
	return fc.fd
}

func (fc *fdConn) SetLinger(secs int) error {
	if fc.IsClosed() {
		return fc.err
	}
	return socket.SetLinger(fc.fd, secs)
}

func (fc *fdConn) SetNoDelay(nodelay bool) error {
	if fc.IsClosed() {
		return fc.err
	}
	return socket.SetNoDelay(fc.fd, nodelay)
}

func (fc *fdConn) SetReadBuffer(size int) error {
	if fc.IsClosed() {
		return fc.err
	}
	return socket.SetRecvBuffer(fc.fd, size)
}

func (fc *fdConn) SetWriteBuffer(size int) error {
	if fc.IsClosed() {
		return fc.err
	}
	return socket.SetSendBuffer(fc.fd, size)
}

func (fc *fdConn) SetKeepAlive(keepalive bool) error {
	if fc.IsClosed() {
		return fc.err
	}
	return socket.SetKeepAlive(fc.fd, keepalive)
}

func (fc *fdConn) SetKeepAlivePeriod(secs int) error {
	if fc.IsClosed() {
		return fc.err
	}
	return socket.SetKeepAlivePeriod(fc.fd, secs)
}

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

func (fc *fdConn) Write(b []byte) (n int, err error) {
	if fc.IsClosed() {
		return 0, fc.err
	}

	if fc.isUdp {
		return fc.onSendUDP(b)
	}

	fc.mux.Lock()

	bufferedThreshold := fc.events.WriteBufferedThreshold
	writeBuffered := bufferedThreshold > 0 && len(b) < bufferedThreshold

	if !fc.outbound.Empty() || writeBuffered {
		// write to outbound buffer.
		if n, err = fc.outbound.Write(b); nil != err {
			fc.mux.Unlock()
			return n, err
		}

		// If the current outbound buffer exceeds the threshold, the actively flushes the data to be sent.
		var socketWriteBytes int
		if bufferedThreshold > 0 && fc.outbound.Len() >= bufferedThreshold {
			// If there is an error in flush, the connection will be closed.
			if socketWriteBytes, err = fc.flush(true); nil != err {
				fc.mux.Unlock()
				fc.events.onSocketBytesWrite(fc, socketWriteBytes)
				fc.events.closeConn(fc, err)
				return 0, err
			}
		}

		fc.mux.Unlock()
		fc.events.onSocketBytesWrite(fc, socketWriteBytes)
		return
	}

	writeSize, err := unix.Write(fc.fd, b)
	if err != nil {
		if !errors.Is(err, unix.EAGAIN) {
			fc.mux.Unlock()
			fc.events.closeConn(fc, err)
			return 0, err
		}
		// ignore: EAGAIN
		writeSize, err = 0, nil
	}

	socketWriteBytes := writeSize

	if writeSize != len(b) {
		n, _ := fc.outbound.Write(b[writeSize:])
		writeSize += n

		// Read events are always registered, which will improve program performance in most cases,
		// unfortunately if there are malicious clients may deliberately slow to receive data will lead to server outbound buffer accumulation,
		// in the absence of intervention may lead to service memory depletion.
		// Turning this option off will change the event registration policy, readable events are unregistered if there is currently data waiting to be sent in the outbound buffer.
		// Readable events are re-registered after the send buffer has been sent. In this case, network reads and writes will degenerate into half-duplex mode, ensuring that server memory is not exhausted.
		if fc.events.FullDuplex {
			err = fc.loop.modReadWrite(fc)
		} else {
			err = fc.loop.modWrite(fc)
		}
	}

	fc.mux.Unlock()
	fc.events.onSocketBytesWrite(fc, socketWriteBytes)
	return writeSize, err
}

func (fc *fdConn) Writev(vec [][]byte) (n int, err error) {
	if fc.IsClosed() {
		return 0, fc.err
	}

	if fc.isUdp {
		return 0, errUnsupported
	}

	fc.mux.Lock()

	var totalBytes int
	for _, b := range vec {
		totalBytes += len(b)
	}

	bufferedThreshold := fc.events.WriteBufferedThreshold
	writeBuffered := bufferedThreshold > 0 && totalBytes < bufferedThreshold

	if !fc.outbound.Empty() || writeBuffered {
		// write to outbound buffer.
		if n, err = fc.outbound.Writev(vec); nil != err {
			fc.mux.Unlock()
			return n, err
		}

		// If the current outbound buffer exceeds the threshold, the actively flushes the data to be sent.
		var socketWriteBytes int
		if bufferedThreshold > 0 && fc.outbound.Len() >= bufferedThreshold {
			// If there is an error in flush, the connection will be closed.
			if socketWriteBytes, err = fc.flush(true); nil != err {
				fc.mux.Unlock()
				fc.events.onSocketBytesWrite(fc, socketWriteBytes)
				fc.events.closeConn(fc, err)
				return 0, err
			}
		}

		fc.mux.Unlock()
		fc.events.onSocketBytesWrite(fc, socketWriteBytes)
		return
	}

	// invoke writev() syscall.
	writeSize, err := socket.Writev(fc.fd, vec)
	if err != nil {
		if !errors.Is(err, unix.EAGAIN) {
			fc.mux.Unlock()
			fc.events.closeConn(fc, err)
			return 0, err
		}
		// ignore: EAGAIN
		writeSize, err = 0, nil
	}

	socketWriteBytes := writeSize

	// trim vec buffer.
	if writeSize < totalBytes {
		var skipBytes int
		var skipIdx = -1
		var skipOffset = -1
		for i, b := range vec {
			sz := min(len(b), writeSize-skipBytes)
			if skipBytes += sz; skipBytes >= writeSize {
				if sz == len(b) {
					skipIdx = i + 1
				} else {
					skipIdx = i
					skipOffset = sz
				}
				break
			}
		}

		if -1 != skipIdx {
			if vec = vec[skipIdx:]; len(vec) > 0 {
				vec[0] = vec[0][skipOffset:]
			}
		}
	} else {
		// all vec write success.
		vec = nil
	}

	// remain bytes write to outbound buffer.
	if len(vec) > 0 {
		n, _ := fc.outbound.Writev(vec)
		writeSize += n

		// Read events are always registered, which will improve program performance in most cases,
		// unfortunately if there are malicious clients may deliberately slow to receive data will lead to server outbound buffer accumulation,
		// in the absence of intervention may lead to service memory depletion.
		// Turning this option off will change the event registration policy, readable events are unregistered if there is currently data waiting to be sent in the outbound buffer.
		// Readable events are re-registered after the send buffer has been sent. In this case, network reads and writes will degenerate into half-duplex mode, ensuring that server memory is not exhausted.
		if fc.events.FullDuplex {
			err = fc.loop.modReadWrite(fc)
		} else {
			err = fc.loop.modWrite(fc)
		}
	}

	fc.mux.Unlock()
	fc.events.onSocketBytesWrite(fc, socketWriteBytes)
	return writeSize, err
}

func (fc *fdConn) Flush() (err error) {
	if fc.IsClosed() {
		return fc.err
	}

	fc.mux.Lock()

	// write outbound buffer.
	var socketWriteBytes int
	if !fc.outbound.Empty() {
		socketWriteBytes, err = fc.flush(true)
	}

	fc.mux.Unlock()
	fc.events.onSocketBytesWrite(fc, socketWriteBytes)

	if nil != err {
		fc.events.closeConn(fc, err)
	}
	return
}

func (fc *fdConn) flush(opEvent bool) (socketWriteBytes int, err error) {

	// udp unsupported.
	if fc.isUdp {
		return 0, nil // nothing to do.
	}

	var vecBuf [16][]byte
	for !fc.outbound.Empty() {
		// peek writeable vec bytes.
		vec, _ := fc.outbound.PeekVec(vecBuf[:0])
		// invoke writev() syscall.
		var writeSize int
		writeSize, err = socket.Writev(fc.fd, vec)
		if nil != err {
			if !errors.Is(err, unix.EAGAIN) {
				return socketWriteBytes, err
			}
			// ignore: EAGAIN
			writeSize, err = 0, nil
			break
		}

		// commit write offset.
		fc.outbound.Discard(writeSize)
		socketWriteBytes += writeSize
	}

	if opEvent && !fc.outbound.Empty() {
		// Read events are always registered, which will improve program performance in most cases,
		// unfortunately if there are malicious clients may deliberately slow to receive data will lead to server outbound buffer accumulation,
		// in the absence of intervention may lead to service memory depletion.
		// Turning this option off will change the event registration policy, readable events are unregistered if there is currently data waiting to be sent in the outbound buffer.
		// Readable events are re-registered after the send buffer has been sent. In this case, network reads and writes will degenerate into half-duplex mode, ensuring that server memory is not exhausted.
		if fc.events.FullDuplex {
			err = fc.loop.modReadWrite(fc)
		} else {
			err = fc.loop.modWrite(fc)
		}
	}

	return socketWriteBytes, err
}

func (fc *fdConn) fdClose(err error) bool {
	fc.mux.Lock()
	defer fc.mux.Unlock()

	if fc.IsClosed() {
		return false
	}

	// try flush outbound buffer.
	_, _ = fc.flush(false)

	// delete connection fd-mapping.
	fc.loop.delConn(fc)

	// close socket and release resource.
	_ = syscall.Close(fc.fd)

	fc.err = err
	fc.closed = 1

	fc.outbound.Reset()
	// warning: data race
	//fc.inbound.Reset()
	//fc.inboundTail = nil

	return true
}

func (fc *fdConn) Close() error {
	return fc.closeWithError(io.ErrUnexpectedEOF)
}

func (fc *fdConn) closeWithError(err error) error {
	if fc.IsClosed() {
		return fc.err
	}

	fc.mux.Lock()
	if 0 != fc.closed {
		fc.mux.Unlock()
		return fc.err
	}

	if fc.isUdp {
		// udp child connection
		if nil != fc.udpSvr {
			rAddr := fc.remoteAddr.String()

			fc.udpSvr.mux.Lock()
			defer fc.udpSvr.mux.Unlock()

			fc.closed = 1
			fc.err = err
			delete(fc.udpSvr.udpConns, rAddr)

			if onClose := fc.events.OnClose; nil != onClose {
				onClose(fc, err)
			}

			fc.mux.Unlock()
			return nil
		}

		// udp server
		if nil != fc.udpConns {
			for addr, fdc := range fc.udpConns {
				delete(fc.udpConns, addr)
				if onClose := fc.events.OnClose; nil != onClose {
					onClose(fdc, err)
				}
			}

			fc.closed = 1
			fc.err = err
			fc.mux.Unlock()

			fc.fdClose(err)
			return nil
		}

		// udp client connection.
		//
	}

	fc.mux.Unlock()
	fc.events.closeConn(fc, err)
	return nil
}

func (fc *fdConn) fireReadEvent() error {
	if fc.isUdp {
		return fc.onRecvUDP()
	}
	return fc.onRead()
}

func (fc *fdConn) fireWriteEvent() error {
	if fc.isUdp {
		panic("udp connection dont need fireWriteEvent")
	}
	return fc.onWrite()
}

func (fc *fdConn) onSendUDP(b []byte) (n int, err error) {

	// dialed udp client
	if nil == fc.rUdpAddr {
		n, err = syscall.Write(fc.fd, b)
		if n > 0 {
			fc.events.onSocketBytesWrite(fc, n)
		}
		return
	}

	// child udp connection
	if err = syscall.Sendto(fc.fd, b, 0, fc.rUdpAddr); nil != err {
		return 0, err
	}
	fc.events.onSocketBytesWrite(fc, len(b))
	return len(b), nil
}

func (fc *fdConn) onRecvUDP() error {
	buffer := fc.loop.getBuffer()

	n, sa, err := syscall.Recvfrom(fc.fd, buffer, 0)
	if nil != err {
		_ = fc.closeWithError(err)
		return err
	}

	// udp connection
	var udpConn = fc

	// udp server
	if nil != fc.udpConns {
		remoteAddr := socket.SockaddrToAddr(sa, true)
		rAddr := remoteAddr.String()
		conn, ok := fc.udpConns[rAddr]
		if !ok {
			conn = &fdConn{}
			conn.fd = fc.fd
			conn.localAddr = fc.localAddr
			conn.remoteAddr = remoteAddr
			conn.loop = fc.loop
			conn.events = fc.events
			conn.isUdp = true
			conn.rUdpAddr = sa
			conn.udpSvr = fc
			conn.udpConns = nil // udp connection always nil

			// save child connection.
			fc.mux.Lock()
			fc.udpConns[rAddr] = conn
			fc.mux.Unlock()

			// fire udp on-open event.
			if onOpen := fc.events.OnOpen; nil != onOpen {
				onOpen(conn)
			}
		}

		udpConn = conn
	}

	// fire udp on-data event.
	udpConn.inboundTail = buffer[:n]

	// trigger inbound event.
	fc.events.onSocketBytesRead(udpConn, n)

	err = fc.events.onData(udpConn)

	// drop unread udp packet.
	_, _ = udpConn.Discard(-1)

	if nil != err {
		// close udp connection
		_ = udpConn.closeWithError(err)
	}

	return nil
}

func (fc *fdConn) onRead() error {

	buffer := fc.loop.getBuffer()

	// read data from fd
	n, err := unix.Read(fc.fd, buffer)
	if 0 == n || err != nil {
		if nil != err && errors.Is(err, syscall.EAGAIN) {
			return nil
		}
		// remote closed
		if nil == err {
			err = io.EOF
		}
		fc.events.closeConn(fc, err)
		return nil
	}

	fc.inboundTail = buffer[:n]

	// trigger on any bytes received.
	fc.events.onSocketBytesRead(fc, n)

	// fire data callback.
	if err = fc.events.onData(fc); nil != err {
		fc.events.closeConn(fc, err)
		return nil
	}

	// append tail bytes to inbound buffer
	if len(fc.inboundTail) > 0 {
		_, _ = fc.inbound.Write(fc.inboundTail)
		fc.inboundTail = fc.inboundTail[:0]
	}

	// try flush outbound buffer.
	if fc.events.WriteBufferedThreshold > 0 {
		_ = fc.Flush()
	}

	return nil
}

func (fc *fdConn) onWrite() error {
	fc.mux.Lock()

	var totalWriteBytes int
	for !fc.outbound.Empty() {
		// writeable buffer
		data := fc.outbound.Peek(fc.loop.getBuffer())

		if 0 == len(data) {
			panic("uio: buffer is too small")
		}

		// write buffer to fd.
		sent, err := unix.Write(fc.fd, data)
		if nil != err {
			fc.mux.Unlock()
			// trigger on any bytes write.
			fc.events.onSocketBytesWrite(fc, totalWriteBytes)

			if errors.Is(err, syscall.EAGAIN) {
				return nil
			}
			fc.events.closeConn(fc, err)
			return nil
		}

		// count of write bytes.
		totalWriteBytes += sent

		// commit read offset.
		fc.outbound.Discard(sent)
	}

	fc.mux.Unlock()
	// trigger on any bytes write.
	fc.events.onSocketBytesWrite(fc, totalWriteBytes)

	// outbound buffer is empty.
	return fc.loop.modRead(fc)
}
