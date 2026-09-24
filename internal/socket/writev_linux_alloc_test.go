//go:build linux && !race

package socket

import (
	"testing"

	"golang.org/x/sys/unix"
)

func TestWritevStackBatchDoesNotAllocate(t *testing.T) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fds[0])
	defer unix.Close(fds[1])
	storage := make([]byte, stackWritevLimit)
	buffers := make([][]byte, stackWritevLimit)
	for index := range buffers {
		buffers[index] = storage[index : index+1]
	}
	var received [stackWritevLimit]byte
	allocations := testing.AllocsPerRun(1000, func() {
		written, writeErr := Writev(fds[0], buffers)
		if writeErr != nil || written != len(buffers) {
			panic("writev failed")
		}
		read := 0
		for read < len(received) {
			n, readErr := unix.Read(fds[1], received[read:])
			if readErr != nil || n == 0 {
				panic("read failed")
			}
			read += n
		}
	})
	if allocations != 0 {
		t.Fatalf("Writev allocations = %.2f, want 0", allocations)
	}
}
