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
	"syscall"
)

// SockaddrToAddr returns a go/net friendly address
func SockaddrToAddr(sa syscall.Sockaddr, udpAddr bool) net.Addr {
	var addr net.Addr
	switch sa := sa.(type) {
	case *syscall.SockaddrInet4:
		if udpAddr {
			addr = &net.UDPAddr{
				IP:   append([]byte{}, sa.Addr[:]...), // copy
				Port: sa.Port,
			}
		} else {
			addr = &net.TCPAddr{
				IP:   append([]byte{}, sa.Addr[:]...), // copy
				Port: sa.Port,
			}
		}
	case *syscall.SockaddrInet6:
		var zone string
		if sa.ZoneId != 0 {
			if ifi, err := net.InterfaceByIndex(int(sa.ZoneId)); err == nil {
				zone = ifi.Name
			}
		}

		if udpAddr {
			addr = &net.UDPAddr{
				IP:   append([]byte{}, sa.Addr[:]...), // copy
				Port: sa.Port,
				Zone: zone,
			}
		} else {
			addr = &net.TCPAddr{
				IP:   append([]byte{}, sa.Addr[:]...), // copy
				Port: sa.Port,
				Zone: zone,
			}
		}
	case *syscall.SockaddrUnix:
		addr = &net.UnixAddr{Net: "unix", Name: sa.Name}
	}
	return addr
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
