//go:build linux || darwin || netbsd || freebsd || openbsd || dragonfly

package socket

import (
	"net"
	"strconv"
	"testing"

	"golang.org/x/sys/unix"
)

func TestAcceptSetsNonblockAndCloseOnExec(t *testing.T) {
	listener, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	file, err := listener.File()
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	client, err := net.Dial("tcp4", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	fd, _, err := Accept(int(file.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	fdFlags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
	if err != nil {
		t.Fatal(err)
	}
	if fdFlags&unix.FD_CLOEXEC == 0 {
		t.Fatal("accepted descriptor is missing FD_CLOEXEC")
	}
	statusFlags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFL, 0)
	if err != nil {
		t.Fatal(err)
	}
	if statusFlags&unix.O_NONBLOCK == 0 {
		t.Fatal("accepted descriptor is blocking")
	}
}

// AcceptPeer reports an IP peer as the address the peer dialed from, for IPv4,
// IPv6 and IPv4-mapped peers alike, and hands back a descriptor set up as
// Accept's is.
func TestAcceptPeerReportsTheDialingAddress(t *testing.T) {
	for _, c := range []struct{ listen, dial, addr string }{
		{"tcp4", "tcp4", "127.0.0.1:0"},
		{"tcp6", "tcp6", "[::1]:0"},
		{"tcp", "tcp4", "127.0.0.1:0"}, // dual stack: the peer arrives IPv4-mapped
	} {
		t.Run(c.listen+"/"+c.dial, func(t *testing.T) {
			addr := c.addr
			if c.listen == "tcp" {
				addr = ":0"
			}
			ln, err := net.Listen(c.listen, addr)
			if err != nil {
				t.Skipf("%s: %v", c.listen, err)
			}
			defer ln.Close()
			file, err := ln.(*net.TCPListener).File()
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			port := ln.Addr().(*net.TCPAddr).Port
			target := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
			if c.dial == "tcp6" {
				target = net.JoinHostPort("::1", strconv.Itoa(port))
			}
			client, err := net.Dial(c.dial, target)
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			fd, peer, _, err := AcceptPeer(int(file.Fd()), true)
			if err != nil {
				t.Fatal(err)
			}
			defer unix.Close(fd)
			want := client.LocalAddr().(*net.TCPAddr).AddrPort()
			if peer.Addr().Unmap() != want.Addr().Unmap() || peer.Port() != want.Port() {
				t.Fatalf("peer %v, want %v", peer, want)
			}
			flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFL, 0)
			if err != nil {
				t.Fatal(err)
			}
			if flags&unix.O_NONBLOCK == 0 {
				t.Fatal("accepted descriptor is blocking")
			}
		})
	}
}
