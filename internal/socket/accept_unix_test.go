//go:build linux || darwin || netbsd || freebsd || openbsd || dragonfly

package socket

import (
	"net"
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
