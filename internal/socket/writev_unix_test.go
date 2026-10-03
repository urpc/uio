//go:build linux || darwin || netbsd || freebsd || openbsd || dragonfly

package socket

import (
	"bytes"
	"testing"

	"golang.org/x/sys/unix"
)

// One writev or sendmsg takes at most maxWritevBuffers buffers and fails
// with EINVAL beyond that; Writev takes any number.
func TestWritevTakesMoreBuffersThanOneCallAllows(t *testing.T) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fds[0])
	defer unix.Close(fds[1])
	buffers := make([][]byte, 2*maxWritevBuffers+1)
	var want []byte
	for index := range buffers {
		if index%10 == 9 {
			continue // empty buffers count toward the limit too
		}
		buffers[index] = []byte{byte('a' + index%26)}
		want = append(want, buffers[index]...)
	}
	written, err := Writev(fds[0], buffers)
	if err != nil || written != len(want) {
		t.Fatalf("Writev of %d buffers = %d, %v; want %d", len(buffers), written, err, len(want))
	}
	got := make([]byte, len(want))
	for read := 0; read < len(got); {
		n, err := unix.Read(fds[1], got[read:])
		if err != nil || n == 0 {
			t.Fatalf("read after %d bytes: %d, %v", read, n, err)
		}
		read += n
	}
	if !bytes.Equal(got, want) {
		t.Fatal("bytes arrived out of order")
	}
}
