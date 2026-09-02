package socket

import (
	"net"
	"syscall"
	"testing"
)

func TestSockaddrToAddr(t *testing.T) {
	ip4 := syscall.SockaddrInet4{Port: 1234, Addr: [4]byte{127, 0, 0, 1}}
	if got, ok := SockaddrToAddr(&ip4, false).(*net.TCPAddr); !ok || got.Port != 1234 || !got.IP.Equal(net.IPv4(127, 0, 0, 1)) {
		t.Fatalf("IPv4 TCP address = %#v", got)
	}
	if got, ok := SockaddrToAddr(&ip4, true).(*net.UDPAddr); !ok || got.Port != 1234 || !got.IP.Equal(net.IPv4(127, 0, 0, 1)) {
		t.Fatalf("IPv4 UDP address = %#v", got)
	}

	ip6 := syscall.SockaddrInet6{Port: 4321}
	ip6.Addr[15] = 1
	ip6.ZoneId = 65535
	if got, ok := SockaddrToAddr(&ip6, false).(*net.TCPAddr); !ok || got.Port != 4321 || !got.IP.Equal(net.ParseIP("::1")) {
		t.Fatalf("IPv6 TCP address = %#v", got)
	}

	unixAddr := syscall.SockaddrUnix{Name: "/tmp/uio-test.sock"}
	if got, ok := SockaddrToAddr(&unixAddr, false).(*net.UnixAddr); !ok || got.Name != unixAddr.Name || got.Net != "unix" {
		t.Fatalf("Unix address = %#v", got)
	}
	if got := SockaddrToAddr(nil, false); got != nil {
		t.Fatalf("nil address = %#v, want nil", got)
	}
}
