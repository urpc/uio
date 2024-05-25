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

	"github.com/urpc/uio/internal/socket"
	"golang.org/x/sys/unix"
)

func (fc *fdConn) SetLinger(secs int) error {
	if 0 == fc.opened {
		return fc.err
	}
	return socket.SetLinger(fc.fd, secs)
}

func (fc *fdConn) SetNoDelay(nodelay bool) error {
	if 0 == fc.opened {
		return fc.err
	}

	var op = 0
	if nodelay {
		op = 1
	}
	return socket.SetNoDelay(fc.fd, op)
}

func (fc *fdConn) SetReadBuffer(size int) error {
	if 0 == fc.opened {
		return fc.err
	}
	return socket.SetRecvBuffer(fc.fd, size)
}

func (fc *fdConn) SetWriteBuffer(size int) error {
	if 0 == fc.opened {
		return fc.err
	}
	return socket.SetSendBuffer(fc.fd, size)
}

func (fc *fdConn) SetKeepAlive(keepalive bool) error {
	if 0 == fc.opened {
		return fc.err
	}
	return socket.SetKeepAlive(fc.fd, keepalive)
}

func (fc *fdConn) SetKeepAlivePeriod(secs int) error {
	if 0 == fc.opened {
		return fc.err
	}
	return socket.SetKeepAlivePeriod(fc.fd, secs)
}

func (fc *fdConn) Write(b []byte) (int, error) {
	fc.mux.Lock()
	defer fc.mux.Unlock()

	if 0 == fc.opened {
		return 0, fc.err
	}

	if !fc.outbound.Empty() {
		return fc.outbound.Write(b)
	}

	writeSize, err := unix.Write(fc.fd, b)
	if err != nil {
		if !errors.Is(err, unix.EAGAIN) {
			return 0, err
		}
		// ignore: EAGAIN
		writeSize, err = 0, nil
	}

	if writeSize != len(b) {
		n, _ := fc.outbound.Write(b[writeSize:])
		//fmt.Println("write to outbound:", writeSize, "+", n, "/", len(b), ",total:", fc.outbound.Len(), ", modWrite")
		err = fc.events.workers[fc.loopIdx].modWrite(fc.fd)

		writeSize += n
	}

	return writeSize, err
}
