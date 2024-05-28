//go:build linux || darwin || netbsd || freebsd || openbsd || dragonfly

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
	"sync/atomic"
	"syscall"

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
	if 0 != atomic.LoadInt32(&fc.closed) {
		return fc.err
	}
	return socket.SetLinger(fc.fd, secs)
}

func (fc *fdConn) SetNoDelay(nodelay bool) error {
	if 0 != atomic.LoadInt32(&fc.closed) {
		return fc.err
	}

	var op = 0
	if nodelay {
		op = 1
	}
	return socket.SetNoDelay(fc.fd, op)
}

func (fc *fdConn) SetReadBuffer(size int) error {
	if 0 != atomic.LoadInt32(&fc.closed) {
		return fc.err
	}
	return socket.SetRecvBuffer(fc.fd, size)
}

func (fc *fdConn) SetWriteBuffer(size int) error {
	if 0 != atomic.LoadInt32(&fc.closed) {
		return fc.err
	}
	return socket.SetSendBuffer(fc.fd, size)
}

func (fc *fdConn) SetKeepAlive(keepalive bool) error {
	if 0 != atomic.LoadInt32(&fc.closed) {
		return fc.err
	}
	return socket.SetKeepAlive(fc.fd, keepalive)
}

func (fc *fdConn) SetKeepAlivePeriod(secs int) error {
	if 0 != atomic.LoadInt32(&fc.closed) {
		return fc.err
	}
	return socket.SetKeepAlivePeriod(fc.fd, secs)
}

func (fc *fdConn) Write(b []byte) (int, error) {
	fc.mux.Lock()

	if 0 != atomic.LoadInt32(&fc.closed) {
		fc.mux.Unlock()
		return 0, fc.err
	}

	if fc.isUdp {
		fc.mux.Unlock()
		return fc.onSendUDP(b)
	}

	if !fc.outbound.Empty() {
		defer fc.mux.Unlock()
		return fc.outbound.Write(b)
	}

	writeSize, err := unix.Write(fc.fd, b)
	if err != nil {
		if !errors.Is(err, unix.EAGAIN) {
			fc.mux.Unlock()
			fc.events.delConn(fc, err)
			return 0, err
		}
		// ignore: EAGAIN
		writeSize, err = 0, nil
	}

	if writeSize != len(b) {
		n, _ := fc.outbound.Write(b[writeSize:])

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

		writeSize += n
	}

	fc.mux.Unlock()
	return writeSize, err
}

func (fc *fdConn) fdClose(err error) {
	fc.mux.Lock()
	defer fc.mux.Unlock()

	// close socket and release resource.
	_ = syscall.Close(fc.fd)
	fc.outbound.Reset()
	fc.inbound.Reset()
	fc.inboundTail = nil
	fc.err = err
}

func (fc *fdConn) Close() error {
	if 0 != atomic.LoadInt32(&fc.closed) {
		return nil
	}

	var closeReason = io.ErrUnexpectedEOF

	if fc.isUdp {
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

	fc.events.delConn(fc, closeReason)
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

func (fc *fdConn) onSendUDP(b []byte) (int, error) {

	// dialed udp client
	if nil == fc.rUdpAddr {
		return syscall.Write(fc.fd, b)
	}

	// child udp connection
	if err := syscall.Sendto(fc.fd, b, 0, fc.rUdpAddr); nil != err {
		return 0, err
	}
	return len(b), nil
}

func (fc *fdConn) onRecvUDP() error {
	buffer := fc.loop.getBuffer()

	n, sa, err := syscall.Recvfrom(fc.fd, buffer, 0)
	if nil != err {
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
	err = fc.events.onData(udpConn)

	// drop unread udp packet.
	_, _ = udpConn.Discard(-1)
	return err
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
		fc.events.delConn(fc, err)
		return nil
	}

	fc.inboundTail = buffer[:n]

	// fire data callback.
	if err = fc.events.onData(fc); nil != err {
		fc.events.delConn(fc, err)
		return nil
	}

	// append tail bytes to inbound buffer
	if len(fc.inboundTail) > 0 {
		_, _ = fc.inbound.Write(fc.inboundTail)
		fc.inboundTail = fc.inboundTail[:0]
	}

	return nil
}

func (fc *fdConn) onWrite() error {
	fc.mux.Lock()

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

			if errors.Is(err, syscall.EAGAIN) {
				return nil
			}

			fc.events.delConn(fc, err)
			return nil
		}

		// commit read offset.
		fc.outbound.Discard(sent)
	}

	defer fc.mux.Unlock()

	// outbound buffer is empty.
	return fc.loop.modRead(fc)
}
