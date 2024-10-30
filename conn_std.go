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
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"
)

type fdConn struct {
	commonConn
	conn     net.Conn
	udp      *net.UDPConn
	udpSvr   *fdConn
	udpConns map[string]*fdConn
	writeSig chan struct{}
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
	if fc.IsClosed() {
		return fc.err
	}
	if tcpConn, ok := fc.conn.(*net.TCPConn); ok {
		return tcpConn.SetLinger(secs)
	}
	return errUnsupported
}

func (fc *fdConn) SetNoDelay(nodelay bool) error {
	if fc.IsClosed() {
		return fc.err
	}
	if tcpConn, ok := fc.conn.(*net.TCPConn); ok {
		return tcpConn.SetNoDelay(nodelay)
	}
	return errUnsupported
}

func (fc *fdConn) SetKeepAlive(keepalive bool) error {
	if fc.IsClosed() {
		return fc.err
	}
	if tcpConn, ok := fc.conn.(*net.TCPConn); ok {
		return tcpConn.SetKeepAlive(keepalive)
	}
	return errUnsupported
}

func (fc *fdConn) SetKeepAlivePeriod(secs int) error {
	if fc.IsClosed() {
		return fc.err
	}
	if tcpConn, ok := fc.conn.(*net.TCPConn); ok {
		if err := tcpConn.SetKeepAlive(true); nil != err {
			return err
		}

		if err := tcpConn.SetKeepAlivePeriod(time.Duration(secs) * time.Second); nil != err {
			_ = tcpConn.SetKeepAlive(false)
			return err
		}
	}
	return errUnsupported
}

func (fc *fdConn) SetReadBuffer(size int) error {
	if fc.IsClosed() {
		return fc.err
	}
	if tcpConn, ok := fc.conn.(*net.TCPConn); ok {
		return tcpConn.SetReadBuffer(size)
	}
	return errUnsupported
}

func (fc *fdConn) SetWriteBuffer(size int) error {
	if fc.IsClosed() {
		return fc.err
	}
	if tcpConn, ok := fc.conn.(*net.TCPConn); ok {
		return tcpConn.SetWriteBuffer(size)
	}
	return errUnsupported
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

func (fc *fdConn) Write(p []byte) (n int, err error) {
	if fc.IsClosed() {
		return 0, fc.err
	}

	fc.mux.Lock()
	defer fc.mux.Unlock()

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

	n, err = fc.outbound.Write(p)

	select {
	case fc.writeSig <- struct{}{}:
	default:
	}

	return
}

func (fc *fdConn) Writev(vec [][]byte) (n int, err error) {
	fc.mux.Lock()
	defer fc.mux.Unlock()

	if fc.IsClosed() {
		return 0, fc.err
	}

	if fc.udp != nil {
		return 0, errUnsupported
	}

	n, err = fc.outbound.Writev(vec)
	select {
	case fc.writeSig <- struct{}{}:
	default:
	}

	return
}

func (fc *fdConn) Flush() error {
	if fc.IsClosed() {
		return fc.err
	}

	select {
	case fc.writeSig <- struct{}{}:
	default:
	}

	return nil
}

func (fc *fdConn) Close() error {
	return fc.closeWithError(io.ErrUnexpectedEOF)
}

func (fc *fdConn) closeWithError(err error) error {

	if fc.IsClosed() {
		return fc.err
	}

	if nil != fc.udp {
		// udp child connection
		if nil != fc.udpSvr {
			rAddr := fc.remoteAddr.String()

			fc.udpSvr.mux.Lock()
			defer fc.udpSvr.mux.Unlock()
			// 从udp映射表移除记录
			delete(fc.udpSvr.udpConns, rAddr)

			fc.mux.Lock()
			fc.closed = 1
			fc.err = err

			// 确保回调完成后释放锁
			defer fc.mux.Unlock()

			if onClose := fc.events.OnClose; nil != onClose {
				onClose(fc, err)
			}

			return nil
		}

		// udp server
		if nil != fc.udpConns {
			fc.mux.Lock()
			for addr, fdc := range fc.udpConns {
				delete(fc.udpConns, addr)

				if onClose := fc.events.OnClose; nil != onClose {
					onClose(fdc, err)
				}
			}

			fc.fdCloseNoLock(err)
			fc.mux.Unlock()
			return nil
		}

		// udp client connection.
		//
	}

	fc.events.closeConn(fc, err)
	return nil
}

func (fc *fdConn) fdClose(err error) bool {
	fc.mux.Lock()
	defer fc.mux.Unlock()
	return fc.fdCloseNoLock(err)
}

func (fc *fdConn) fdCloseNoLock(err error) bool {

	if !atomic.CompareAndSwapInt32(&fc.closed, 0, 1) {
		return false
	}

	// delete connection fd-mapping.
	fc.loop.delConn(fc)

	// close socket and release resource.
	switch {
	case nil != fc.conn:
		_ = fc.conn.Close()
	case nil != fc.udp:
		_ = fc.udp.Close()
	}

	fc.outbound.Reset()
	fc.inbound.Reset()
	fc.inboundTail = nil
	fc.err = err

	// close write signal.
	if nil != fc.writeSig {
		close(fc.writeSig)
	}

	return true
}

func (fc *fdConn) fireWriteEvent() error {
	if nil == fc.conn {
		return nil // udp client nothing to do.
	}

	go func() {
		var writeBuffer = make([]byte, 1024)
		for {
			select {
			case _, ok := <-fc.writeSig:
				if !ok {
					return
				}

				fc.mux.Lock()
				if fc.outbound.Empty() {
					fc.mux.Unlock()
					continue
				}
				fc.mux.Unlock()

				var totalWriteBytes int
				for {
					fc.mux.Lock()
					data := fc.outbound.Peek(writeBuffer)
					fc.mux.Unlock()

					// no more outbound bytes.
					if 0 == len(data) {
						break
					}

					n, err := fc.conn.Write(data)
					if nil != err {
						if totalWriteBytes > 0 {
							// trigger outbound event.
							fc.events.onSocketBytesWrite(fc, totalWriteBytes)
						}
						// close on error.
						fc.events.closeConn(fc, err)
						return
					}

					// mark outbound read offset.
					fc.mux.Lock()
					fc.outbound.Discard(n)
					fc.mux.Unlock()

					// write success.
					totalWriteBytes += n
				}

				// trigger outbound event.
				fc.events.onSocketBytesWrite(fc, totalWriteBytes)
			}
		}
	}()

	return nil
}

func (fc *fdConn) fireReadEvent() error {

	// udp client
	if nil != fc.udp {
		go func() {
			var buffer = make([]byte, fc.events.MaxBufferSize)
			for {
				n, _, err := fc.udp.ReadFrom(buffer)
				if nil != err {
					// close on error.
					fc.events.closeConn(fc, err)
					return
				}

				fc.inboundTail = buffer[:n]

				// trigger inbound event.
				fc.events.onSocketBytesRead(fc, n)

				// fire data callback.
				if err = fc.events.onData(fc); nil != err {
					// close on error.
					fc.events.closeConn(fc, err)
					break
				}

				// drop unread udp packet.
				_, _ = fc.Discard(-1)
			}
		}()

		return nil
	}

	// tcp connection.
	go func() {
		var buffer = make([]byte, 1024)
		for {
			n, err := fc.conn.Read(buffer)
			if nil != err {
				// close on error.
				fc.events.closeConn(fc, err)
				return
			}

			// fire data callback.
			fc.inboundTail = buffer[:n]

			// trigger inbound event.
			fc.events.onSocketBytesRead(fc, n)

			if err = fc.events.onData(fc); nil != err {
				// close on error.
				fc.events.closeConn(fc, err)
				break
			}

			if len(fc.inboundTail) > 0 {
				_, _ = fc.inbound.Write(fc.inboundTail)
				fc.inboundTail = fc.inboundTail[:0]
			}

			// try flush outbound buffer.
			if fc.events.WriteBufferedThreshold > 0 {
				_ = fc.Flush()
			}
		}
	}()

	return nil
}

func (fc *fdConn) listenUDP() error {

	var buffer = make([]byte, fc.events.MaxBufferSize)

	for {
		n, addr, err := fc.udp.ReadFrom(buffer)
		if nil != err {
			_ = fc.closeWithError(err)
			return err
		}

		// remote address.
		var rAddr = addr.String()

		// udp server
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

			// save child connection.
			fc.mux.Lock()
			fc.udpConns[rAddr] = udpConn
			fc.mux.Unlock()

			// fire udp on-open event.
			if onOpen := fc.events.OnOpen; nil != onOpen {
				onOpen(udpConn)
			}
		}

		udpConn.inboundTail = buffer[:n]

		// trigger inbound event
		fc.events.onSocketBytesRead(udpConn, n)

		// fire udp on-data event.
		err = fc.events.onData(udpConn)

		// drop unread udp packet.
		_, _ = udpConn.Discard(-1)

		if nil != err {
			// close udp connection
			_ = udpConn.closeWithError(err)
		}
	}
}
