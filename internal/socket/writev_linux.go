//go:build linux && !race

package socket

import (
	"runtime"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

const stackWritevLimit = 64

// Writev invokes SYS_WRITEV with stack-backed iovecs for UIO's common batches.
// x/sys reserves only eight iovecs and allocates when a corked read round
// produces a larger batch.
func Writev(fd int, buffers [][]byte) (int, error) {
	switch len(buffers) {
	case 0:
		return 0, nil
	case 1:
		return syscall.Write(fd, buffers[0])
	}
	if len(buffers) > stackWritevLimit {
		return unix.Writev(fd, buffers)
	}
	var storage [stackWritevLimit]unix.Iovec
	iovecs := storage[:len(buffers)]
	for index, buffer := range buffers {
		iovecs[index].SetLen(len(buffer))
		if len(buffer) > 0 {
			iovecs[index].Base = &buffer[0]
		}
	}
	// Keep the runtime's syscall enter/exit accounting. These sockets are
	// non-blocking, but writev can still contend in the kernel under load and
	// hiding that interval from the scheduler hurts CPU efficiency.
	written, _, errno := unix.Syscall(
		unix.SYS_WRITEV,
		uintptr(fd),
		uintptr(unsafe.Pointer(&iovecs[0])),
		uintptr(len(iovecs)),
	)
	runtime.KeepAlive(buffers)
	if errno != 0 {
		return int(written), errno
	}
	return int(written), nil
}
