//go:build linux || darwin || netbsd || freebsd || openbsd || dragonfly

package socket

import (
	"net"
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
	return int(n), nil
}

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
