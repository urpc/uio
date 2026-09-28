//go:build linux && !race

package socket

import (
	"bytes"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

func TestStreamSendRecvUseSocketCalls(t *testing.T) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fds[0])
	defer unix.Close(fds[1])
	if n, err := Send(fds[0], []byte("hello")); err != nil || n != 5 {
		t.Fatalf("Send = %d, %v", n, err)
	}
	if n, err := Writev(fds[0], [][]byte{[]byte(" "), []byte("world")}); err != nil || n != 6 {
		t.Fatalf("Writev = %d, %v", n, err)
	}
	buffer := make([]byte, 32)
	n, err := Recv(fds[1], buffer)
	if err != nil || !bytes.Equal(buffer[:n], []byte("hello world")) {
		t.Fatalf("Recv = %q, %v", buffer[:n], err)
	}
	if err := unix.SetNonblock(fds[1], true); err != nil {
		t.Fatal(err)
	}
	if _, err := Recv(fds[1], buffer); err != syscall.EAGAIN {
		t.Fatalf("Recv on empty socket = %v, want EAGAIN", err)
	}
	if n, err := Send(fds[0], nil); err != nil || n != 0 {
		t.Fatalf("empty Send = %d, %v", n, err)
	}
}

func TestStreamCallsFallBackForNonSockets(t *testing.T) {
	var pipe [2]int
	if err := unix.Pipe(pipe[:]); err != nil {
		t.Fatal(err)
	}
	defer unix.Close(pipe[0])
	defer unix.Close(pipe[1])
	if n, err := Send(pipe[1], []byte("ab")); err != nil || n != 2 {
		t.Fatalf("Send to pipe = %d, %v", n, err)
	}
	if n, err := Writev(pipe[1], [][]byte{[]byte("c"), []byte("d")}); err != nil || n != 2 {
		t.Fatalf("Writev to pipe = %d, %v", n, err)
	}
	buffer := make([]byte, 8)
	n, err := Recv(pipe[0], buffer)
	if err != nil || string(buffer[:n]) != "abcd" {
		t.Fatalf("Recv from pipe = %q, %v", buffer[:n], err)
	}
}
