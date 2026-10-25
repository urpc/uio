//go:build darwin || netbsd || freebsd || openbsd || dragonfly

package socket

import (
	"testing"

	"golang.org/x/sys/unix"
)

// Batches of every length, longer vectors included, keep their iovecs on the
// stack: a batch UIO hands over never allocates.
func TestWritevStackBatchDoesNotAllocate(t *testing.T) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fds[0])
	defer unix.Close(fds[1])
	for _, count := range []int{2, stackWritevLimit, 3*stackWritevLimit + 1} {
		storage := make([]byte, count)
		buffers := make([][]byte, count)
		for index := range buffers {
			buffers[index] = storage[index : index+1]
		}
		received := make([]byte, count)
		allocations := testing.AllocsPerRun(200, func() {
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
			t.Fatalf("Writev of %d buffers allocations = %.2f, want 0", count, allocations)
		}
	}
}

// An empty segment carries no bytes but keeps its place in the vector, which
// is what a coalescing layer hands over.
func TestWritevEmptySegments(t *testing.T) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fds[0])
	defer unix.Close(fds[1])

	buffers := [][]byte{{'a'}, nil, {'b', 'c'}, {}, {'d'}}
	written, err := Writev(fds[0], buffers)
	if err != nil {
		t.Fatal(err)
	}
	if written != 4 {
		t.Fatalf("wrote %d bytes, want 4", written)
	}
	received := make([]byte, 4)
	read := 0
	for read < len(received) {
		n, readErr := unix.Read(fds[1], received[read:])
		if readErr != nil {
			t.Fatal(readErr)
		}
		read += n
	}
	if string(received) != "abcd" {
		t.Fatalf("read %q, want abcd", received)
	}
}
