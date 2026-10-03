//go:build linux && race

package socket

import (
	"syscall"

	"golang.org/x/sys/unix"
)

const writevBatchLimit = maxWritevBuffers

// Writev retains x/sys's race synchronization annotations in race builds.
// Longer vectors than one writev takes go out in batches.
func Writev(fd int, buffers [][]byte) (int, error) {
	switch len(buffers) {
	case 0:
		return 0, nil
	case 1:
		return syscall.Write(fd, buffers[0])
	}
	if len(buffers) > writevBatchLimit {
		return writevBatches(fd, buffers)
	}
	return unix.Writev(fd, buffers)
}
