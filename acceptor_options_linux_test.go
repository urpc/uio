//go:build linux && !stdio

package uio

import (
	"net"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// TestListenerOptionsInheritedByAcceptedConnections pins the platform behavior
// setListenerOptions relies on: Linux copies the listening socket's TCP
// options to every accepted socket, which is what lets an accepted connection
// skip its own setsockopt calls. Should a kernel ever stop doing that, this
// fails before the loss goes unnoticed.
func TestListenerOptionsInheritedByAcceptedConnections(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	setListenerOptions(ln)

	raw, err := ln.(*net.TCPListener).SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var dup int
	if err = raw.Control(func(fd uintptr) {
		dup, err = syscall.Dup(int(fd))
	}); err != nil {
		t.Fatal(err)
	}
	defer syscall.Close(dup)

	release := make(chan struct{})
	go func() {
		c, derr := net.Dial("tcp", ln.Addr().String())
		if derr == nil {
			defer c.Close()
			<-release // hold the connection until the checks are done
		}
	}()
	defer close(release)
	var nfd int
	for deadline := time.Now().Add(5 * time.Second); ; {
		nfd, _, err = syscall.Accept(dup)
		if err == nil {
			break
		}
		if err != syscall.EAGAIN && err != syscall.EWOULDBLOCK && err != syscall.EINTR {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("accept: %v", err)
		}
		time.Sleep(time.Millisecond)
	}
	defer syscall.Close(nfd)

	if got, gerr := unix.GetsockoptInt(nfd, unix.IPPROTO_TCP, unix.TCP_NODELAY); gerr != nil || got != 1 {
		t.Fatalf("accepted TCP_NODELAY = %d, %v; want 1", got, gerr)
	}
	if got, gerr := unix.GetsockoptInt(nfd, unix.SOL_SOCKET, unix.SO_KEEPALIVE); gerr != nil || got != 1 {
		t.Fatalf("accepted SO_KEEPALIVE = %d, %v; want 1", got, gerr)
	}
	if got, gerr := unix.GetsockoptInt(nfd, unix.IPPROTO_TCP, unix.TCP_KEEPIDLE); gerr != nil || got != defaultTCPKeepAliveSecs {
		t.Fatalf("accepted TCP_KEEPIDLE = %d, %v; want %d", got, gerr, defaultTCPKeepAliveSecs)
	}
}
