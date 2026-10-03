//go:build linux

package socket

import (
	"testing"

	"golang.org/x/sys/unix"
)

// A batch after the first that finds the socket full ends the call with the
// count already sent and no error: callers queue the rest, and a would-block
// that hid the count would make them send those bytes again.
func TestWritevReportsProgressWhenALaterBatchWouldBlock(t *testing.T) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_NONBLOCK, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fds[0])
	defer unix.Close(fds[1])
	// Every one-byte send queues a buffer of the same size; the first send
	// refused finds the socket's allowance used up.
	for {
		if _, err = unix.Write(fds[0], []byte{0}); err == unix.EAGAIN {
			break
		} else if err != nil {
			t.Fatal(err)
		}
	}
	// Taking one byte frees one of those buffers: room for the next batch,
	// which uses it up again, and none for the batch after it.
	if _, err = unix.Read(fds[1], make([]byte, 1)); err != nil {
		t.Fatal(err)
	}
	buffers := make([][]byte, 2*writevBatchLimit)
	for index := range buffers {
		buffers[index] = []byte{1}
	}
	written, err := Writev(fds[0], buffers)
	if err != nil || written != writevBatchLimit {
		t.Fatalf("Writev = %d, %v; want the first batch's %d bytes and no error", written, err, writevBatchLimit)
	}
	if _, err = Writev(fds[0], buffers[written:]); err != unix.EAGAIN {
		t.Fatalf("Writev on a full socket = %v, want EAGAIN", err)
	}
}
