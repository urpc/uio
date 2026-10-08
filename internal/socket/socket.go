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

// Package socket contains platform-specific descriptor and socket operations
// used by UIO's native transports.
package socket

import (
	"net"
	"net/netip"
	"syscall"
)

// SockaddrToAddr returns a go/net friendly address
// SockaddrToAddrPort converts a sockaddr to the value form, with no
// allocation; an unsupported or empty address returns the zero value.
func SockaddrToAddrPort(sa syscall.Sockaddr) netip.AddrPort {
	switch sa := sa.(type) {
	case *syscall.SockaddrInet4:
		return netip.AddrPortFrom(netip.AddrFrom4(sa.Addr), uint16(sa.Port))
	case *syscall.SockaddrInet6:
		addr := netip.AddrFrom16(sa.Addr)
		if sa.ZoneId != 0 {
			// Link-local peers carry their interface; keep it the way
			// SockaddrToAddr does, as a zone name.
			if ifi, err := net.InterfaceByIndex(int(sa.ZoneId)); err == nil {
				addr = addr.WithZone(ifi.Name)
			}
		}
		return netip.AddrPortFrom(addr, uint16(sa.Port))
	case *syscall.SockaddrUnix:
		_ = sa
	}
	return netip.AddrPort{}
}

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
