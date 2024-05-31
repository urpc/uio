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
	udp      net.PacketConn
	udpSvr   *fdConn
	udpConns map[string]*fdConn
	writeSig chan struct{}
}

func (fc *fdConn) Fd() int {
	if 0 != atomic.LoadInt32(&fc.closed) {
		return -1
	}

	sc, err := fc.conn.(syscall.Conn).SyscallConn()
	if nil != err {
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
	if 0 != atomic.LoadInt32(&fc.closed) {
		return fc.err
	}
	if tcpConn, ok := fc.conn.(*net.TCPConn); ok {
		return tcpConn.SetLinger(secs)
	}
	return errUnsupported
}

func (fc *fdConn) SetNoDelay(nodelay bool) error {
	if 0 != atomic.LoadInt32(&fc.closed) {
		return fc.err
	}
	if tcpConn, ok := fc.conn.(*net.TCPConn); ok {
		return tcpConn.SetNoDelay(nodelay)
	}
	return errUnsupported
}

func (fc *fdConn) SetKeepAlive(keepalive bool) error {
	if 0 != atomic.LoadInt32(&fc.closed) {
		return fc.err
	}
	if tcpConn, ok := fc.conn.(*net.TCPConn); ok {
		return tcpConn.SetKeepAlive(keepalive)
	}
	return errUnsupported
}

func (fc *fdConn) SetKeepAlivePeriod(secs int) error {
	if 0 != atomic.LoadInt32(&fc.closed) {
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
	if 0 != atomic.LoadInt32(&fc.closed) {
		return fc.err
	}
	if tcpConn, ok := fc.conn.(*net.TCPConn); ok {
		return tcpConn.SetReadBuffer(size)
	}
	return errUnsupported
}

func (fc *fdConn) SetWriteBuffer(size int) error {
	if 0 != atomic.LoadInt32(&fc.closed) {
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
	if 0 != atomic.LoadInt32(&fc.closed) {
		return 0, fc.err
	}

	if fc.udp != nil {
		return fc.udp.WriteTo(p, fc.remoteAddr)
	}

	fc.mux.Lock()
	n, err = fc.outbound.Write(p)
	fc.mux.Unlock()

	select {
	case fc.writeSig <- struct{}{}:
	default:
	}

	return
}

func (fc *fdConn) Writev(vec [][]byte) (n int, err error) {
	if 0 != atomic.LoadInt32(&fc.closed) {
		return 0, fc.err
	}

	if fc.udp != nil {
		return 0, errUnsupported
	}

	fc.mux.Lock()
	n, err = fc.outbound.Writev(vec)
	fc.mux.Unlock()

	select {
	case fc.writeSig <- struct{}{}:
	default:
	}

	return
}

func (fc *fdConn) Flush() error {
	if 0 != atomic.LoadInt32(&fc.closed) {
		return fc.err
	}

	select {
	case fc.writeSig <- struct{}{}:
	default:
	}
	return nil
}

func (fc *fdConn) Close() error {
	if 0 != atomic.LoadInt32(&fc.closed) {
		return nil
	}

	var closeReason = io.ErrUnexpectedEOF

	if nil != fc.udp {
		// udp child connection
		if nil != fc.udpSvr {
			rAddr := fc.remoteAddr.String()

			fc.udpSvr.mux.Lock()
			defer fc.udpSvr.mux.Unlock()

			delete(fc.udpSvr.udpConns, rAddr)

			if onClose := fc.events.OnClose; nil != onClose {
				onClose(fc, closeReason)
			}

			return nil
		}

		// udp server
		if nil != fc.udpConns {
			fc.mux.Lock()
			defer fc.mux.Unlock()

			for addr, fdc := range fc.udpConns {
				delete(fc.udpConns, addr)

				if onClose := fc.events.OnClose; nil != onClose {
					onClose(fdc, closeReason)
				}
			}

			fc.loop.delConn(fc)
			return nil
		}

		// udp client connection.
		//
	}

	fc.events.closeConn(fc, io.ErrUnexpectedEOF)
	return nil
}

func (fc *fdConn) fdClose(err error) {
	fc.mux.Lock()
	defer fc.mux.Unlock()

	// close socket and release resource.
	_ = fc.conn.Close()
	fc.outbound.Reset()
	fc.inbound.Reset()
	fc.inboundTail = nil
	fc.err = err

	// close write signal.
	close(fc.writeSig)
}

func (fc *fdConn) fireWriteEvent() error {
	if nil == fc.conn {
		return errUnsupported
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
						fc.events.delConn(fc, err)
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
	if nil == fc.conn {
		return errUnsupported
	}

	go func() {
		var buffer = make([]byte, 1024)
		for {
			n, err := fc.conn.Read(buffer)
			if nil != err {
				// close on error.
				fc.events.delConn(fc, err)
				return
			}

			fc.inboundTail = buffer[:n]

			// trigger inbound event.
			fc.events.onSocketBytesRead(fc, n)

			// fire data callback.
			if err = fc.events.onData(fc); nil != err {
				// close on error.
				fc.events.delConn(fc, err)
				break
			}

			// append tail bytes to inbound buffer
			if len(fc.inboundTail) > 0 {
				_, _ = fc.inbound.Write(fc.inboundTail)
				fc.inboundTail = fc.inboundTail[:0]
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
			return err
		}

		// udp connection
		var udpConn = fc

		// remote address.
		var rAddr = addr.String()

		// udp server
		if nil != fc.udpConns {
			conn, ok := fc.udpConns[rAddr]
			if !ok {
				conn = &fdConn{}
				conn.udp = fc.udp
				conn.localAddr = fc.localAddr
				conn.remoteAddr = addr
				conn.loop = fc.loop
				conn.events = fc.events
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

		udpConn.inboundTail = buffer[:n]

		// fire udp on-data event.
		err = fc.events.onData(udpConn)

		// drop unread udp packet.
		_, _ = udpConn.Discard(-1)

		if nil != err {
			// delete child connection.
			fc.mux.Lock()
			delete(fc.udpConns, rAddr)
			fc.mux.Unlock()

			// fire udp on-close event.
			if onClose := fc.events.OnClose; nil != onClose {
				onClose(udpConn, err)
			}
		}
	}
}
