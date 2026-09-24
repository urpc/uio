//go:build linux && race

package socket

import (
	"syscall"

	"golang.org/x/sys/unix"
)

// Writev retains x/sys's race synchronization annotations in race builds.
func Writev(fd int, buffers [][]byte) (int, error) {
	switch len(buffers) {
	case 0:
		return 0, nil
	case 1:
		return syscall.Write(fd, buffers[0])
	default:
		return unix.Writev(fd, buffers)
	}
}
