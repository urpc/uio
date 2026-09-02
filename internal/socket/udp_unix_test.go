//go:build linux || darwin || netbsd || freebsd || openbsd || dragonfly

package socket

import (
	"errors"
	"net"
	"syscall"
	"testing"
	"unsafe"

	"golang.org/x/sys/unix"
)

func TestRecvUDPIPv4(t *testing.T) {
	receiver, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(receiver)
	if err = unix.Bind(receiver, &unix.SockaddrInet4{Addr: [4]byte{127, 0, 0, 1}}); err != nil {
		t.Fatal(err)
	}
	target, err := unix.Getsockname(receiver)
	if err != nil {
		t.Fatal(err)
	}
	sender, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(sender)
	if err = unix.Sendto(sender, []byte("ping"), 0, target); err != nil {
		t.Fatal(err)
	}

	var receive UDPReceive
	packet := make([]byte, 16)
	n, err := RecvUDP(receiver, packet, &receive)
	if err != nil || string(packet[:n]) != "ping" {
		t.Fatalf("RecvUDP = %q, %v", packet[:n], err)
	}
	if receive.Addr.Family != 4 || receive.Addr.Port == 0 || receive.Addr.Addr[0] != 127 {
		t.Fatalf("source address = %#v", receive.Addr)
	}
	if sockaddr, ok := receive.Addr.Sockaddr().(*syscall.SockaddrInet4); !ok || sockaddr.Port != int(receive.Addr.Port) {
		t.Fatalf("Sockaddr = %#v", sockaddr)
	}
	if addr := receive.Addr.NetAddr(); addr.Port != int(receive.Addr.Port) || !addr.IP.Equal(net.IPv4(127, 0, 0, 1)) {
		t.Fatalf("NetAddr = %#v", addr)
	}
}

func TestUDPReceiveParsesIPv6AndRejectsInvalidAddresses(t *testing.T) {
	var receive UDPReceive
	raw := (*unix.RawSockaddrInet6)(unsafe.Pointer(&receive.raw))
	raw.Family = unix.AF_INET6
	port := (*[2]byte)(unsafe.Pointer(&raw.Port))
	port[0], port[1] = 0x12, 0x34
	raw.Addr[15] = 1
	raw.Scope_id = 7
	if err := receive.parse(unix.SizeofSockaddrInet6); err != nil {
		t.Fatal(err)
	}
	if receive.Addr.Family != 6 || receive.Addr.Port != 0x1234 || receive.Addr.Zone != 7 || receive.Addr.Addr[15] != 1 {
		t.Fatalf("IPv6 address = %#v", receive.Addr)
	}
	if sockaddr, ok := receive.Addr.Sockaddr().(*syscall.SockaddrInet6); !ok || sockaddr.Port != 0x1234 || sockaddr.ZoneId != 7 {
		t.Fatalf("IPv6 Sockaddr = %#v", sockaddr)
	}

	receive.raw.Addr.Family = unix.AF_INET
	if err := receive.parse(unix.SizeofSockaddrInet4 - 1); !errors.Is(err, unix.EINVAL) {
		t.Fatalf("short IPv4 address error = %v", err)
	}
	receive.raw.Addr.Family = unix.AF_UNSPEC
	if err := receive.parse(unix.SizeofSockaddrAny); !errors.Is(err, unix.EAFNOSUPPORT) {
		t.Fatalf("unsupported address error = %v", err)
	}
}
