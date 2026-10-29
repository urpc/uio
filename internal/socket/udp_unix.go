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

package socket

import (
	"net"
	"net/netip"
	"strconv"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// UDPAddress is a comparable source address suitable for use as a map key.
type UDPAddress struct {
	Addr   [16]byte
	Port   uint16
	Zone   uint32
	Family uint8
}

// UDPReceive holds reusable source-address storage for RecvUDP.
type UDPReceive struct {
	raw  unix.RawSockaddrAny
	Addr UDPAddress
	// Len is the datagram's original length, which exceeds its buffer when
	// the datagram was truncated.
	Len int
}

// RecvUDP receives one datagram without allocating a Sockaddr interface.
func RecvUDP(fd int, packet []byte, receive *UDPReceive) (int, error) {
	addrLen := uint32(unix.SizeofSockaddrAny)
	var zero byte
	data := unsafe.Pointer(&zero)
	if len(packet) > 0 {
		data = unsafe.Pointer(&packet[0])
	}
	n, _, errno := unix.Syscall6(
		unix.SYS_RECVFROM,
		uintptr(fd),
		uintptr(data),
		uintptr(len(packet)),
		0,
		uintptr(unsafe.Pointer(&receive.raw)),
		uintptr(unsafe.Pointer(&addrLen)),
	)
	if errno != 0 {
		return int(n), errno
	}
	if err := receive.parse(addrLen); err != nil {
		return int(n), err
	}
	receive.Len = int(n)
	return int(n), nil
}

// parse converts kernel sockaddr storage into a comparable allocation-free key.
func (receive *UDPReceive) parse(addrLen uint32) error {
	receive.Addr = UDPAddress{}
	switch receive.raw.Addr.Family {
	case unix.AF_INET:
		if addrLen < unix.SizeofSockaddrInet4 {
			return unix.EINVAL
		}
		raw := (*unix.RawSockaddrInet4)(unsafe.Pointer(&receive.raw))
		receive.Addr.Family = 4
		receive.Addr.Port = networkPort(raw.Port)
		copy(receive.Addr.Addr[:4], raw.Addr[:])
	case unix.AF_INET6:
		if addrLen < unix.SizeofSockaddrInet6 {
			return unix.EINVAL
		}
		raw := (*unix.RawSockaddrInet6)(unsafe.Pointer(&receive.raw))
		receive.Addr.Family = 6
		receive.Addr.Port = networkPort(raw.Port)
		receive.Addr.Zone = raw.Scope_id
		receive.Addr.Addr = raw.Addr
	default:
		return unix.EAFNOSUPPORT
	}
	return nil
}

func networkPort(port uint16) uint16 {
	bytes := (*[2]byte)(unsafe.Pointer(&port))
	return uint16(bytes[0])<<8 | uint16(bytes[1])
}

// UDPAddressFor returns the key a datagram from addr arrives with on a
// socket of family (4 or 6): on an IPv6 socket an IPv4 peer is reported in
// its IPv4-mapped form, and on an IPv4 socket only IPv4 peers exist.
func UDPAddressFor(family uint8, addr netip.AddrPort) (UDPAddress, error) {
	ip := addr.Addr()
	if !ip.IsValid() {
		return UDPAddress{}, unix.EINVAL
	}
	var key UDPAddress
	key.Port = addr.Port()
	switch family {
	case 4:
		ip = ip.Unmap()
		if !ip.Is4() {
			return UDPAddress{}, unix.EAFNOSUPPORT
		}
		key.Family = 4
		v4 := ip.As4()
		copy(key.Addr[:4], v4[:])
	case 6:
		key.Family = 6
		key.Addr = ip.As16()
		if zone := ip.Zone(); zone != "" {
			// A numeric zone is an interface index, as in fe80::1%2; a name
			// is looked up, as in fe80::1%en0.
			if index, err := strconv.Atoi(zone); err == nil {
				key.Zone = uint32(index)
			} else if iface, err := net.InterfaceByName(zone); err == nil {
				key.Zone = uint32(iface.Index)
			} else {
				return UDPAddress{}, err
			}
		}
	default:
		return UDPAddress{}, unix.EAFNOSUPPORT
	}
	return key, nil
}

// SocketFamily reports whether fd is an IPv4 (4) or IPv6 (6) socket.
func SocketFamily(fd int) (uint8, error) {
	sa, err := unix.Getsockname(fd)
	if err != nil {
		return 0, err
	}
	switch sa.(type) {
	case *unix.SockaddrInet4:
		return 4, nil
	case *unix.SockaddrInet6:
		return 6, nil
	}
	return 0, unix.EAFNOSUPPORT
}

// Sockaddr allocates the persistent send address used only for a new peer.
func (addr UDPAddress) Sockaddr() syscall.Sockaddr {
	switch addr.Family {
	case 4:
		sockaddr := &syscall.SockaddrInet4{Port: int(addr.Port)}
		copy(sockaddr.Addr[:], addr.Addr[:4])
		return sockaddr
	case 6:
		return &syscall.SockaddrInet6{Port: int(addr.Port), ZoneId: addr.Zone, Addr: addr.Addr}
	default:
		return nil
	}
}

// NetAddr allocates the user-visible address used only for a new peer.
func (addr UDPAddress) NetAddr() *net.UDPAddr {
	ipLength := net.IPv6len
	if addr.Family == 4 {
		ipLength = net.IPv4len
	}
	ip := make(net.IP, ipLength)
	copy(ip, addr.Addr[:ipLength])
	var zone string
	if addr.Zone != 0 {
		if iface, err := net.InterfaceByIndex(int(addr.Zone)); err == nil {
			zone = iface.Name
		}
	}
	return &net.UDPAddr{IP: ip, Port: int(addr.Port), Zone: zone}
}
