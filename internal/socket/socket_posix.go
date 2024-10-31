//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

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

package socket

import (
	"errors"
	"net"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// SetNoDelay controls whether the operating system should delay
// packet transmission in hopes of sending fewer packets (Nagle's algorithm).
//
// The default is true (no delay), meaning that data is
// sent as soon as possible after a Write.
func SetNoDelay(fd int, nodelay bool) error {
	var op = 0
	if nodelay {
		op = 1
	}
	return os.NewSyscallError("setsockopt", unix.SetsockoptInt(fd, unix.IPPROTO_TCP, unix.TCP_NODELAY, op))
}

// SetNonblock set this socket to use non-blocking mode or block mode.
func SetNonblock(fd int, nonblocking bool) error {
	return os.NewSyscallError("setnonblock", unix.SetNonblock(fd, nonblocking))
}

// SetRecvBuffer sets the size of the operating system's
// receive buffer associated with the connection.
func SetRecvBuffer(fd, size int) error {
	return os.NewSyscallError("setsockopt", unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_RCVBUF, size))
}

// SetSendBuffer sets the size of the operating system's
// transmit buffer associated with the connection.
func SetSendBuffer(fd, size int) error {
	return os.NewSyscallError("setsockopt", unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_SNDBUF, size))
}

// SetReuseAddr enables SO_REUSEADDR option on socket.
func SetReuseAddr(fd, reuseAddr int) error {
	return os.NewSyscallError("setsockopt", unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_REUSEADDR, reuseAddr))
}

// SetIPv6Only restricts a IPv6 socket to only process IPv6 requests or both IPv4 and IPv6 requests.
func SetIPv6Only(fd, ipv6only int) error {
	return os.NewSyscallError("setsockopt", unix.SetsockoptInt(fd, unix.IPPROTO_IPV6, unix.IPV6_V6ONLY, ipv6only))
}

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
func SetLinger(fd, secs int) error {
	var l unix.Linger
	if secs >= 0 {
		l.Onoff = 1
		l.Linger = int32(secs)
	} else {
		l.Onoff = 0
		l.Linger = 0
	}
	return unix.SetsockoptLinger(fd, syscall.SOL_SOCKET, syscall.SO_LINGER, &l)
}

func DupNetConn(conn net.Conn) (int, error) {
	sc, ok := conn.(interface {
		SyscallConn() (syscall.RawConn, error)
	})
	if !ok {
		return 0, errors.New("RawConn Unsupported")
	}
	rc, err := sc.SyscallConn()
	if err != nil {
		return 0, errors.New("RawConn Unsupported")
	}

	var newFd int
	errCtrl := rc.Control(func(fd uintptr) {
		newFd, err = syscall.Dup(int(fd))
	})

	if errCtrl != nil {
		return 0, errCtrl
	}

	if err != nil {
		return 0, err
	}

	return newFd, nil
}
