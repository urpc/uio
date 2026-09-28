//go:build linux && !race

package socket

import (
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Stream sockets are read and written with recvfrom, sendto and sendmsg.
// Those enter the socket layer directly, while read, write and writev go
// through the VFS first, which takes the file position lock and runs the file
// permission hooks on every call before reaching the same socket code.
// A descriptor that is not a socket falls back to the VFS calls.

var zeroByte byte

func bytesPointer(p []byte) unsafe.Pointer {
	if len(p) == 0 {
		return unsafe.Pointer(&zeroByte)
	}
	return unsafe.Pointer(&p[0])
}

// Recv reads from a connected stream socket.
func Recv(fd int, p []byte) (int, error) {
	n, _, errno := syscall.Syscall6(syscall.SYS_RECVFROM, uintptr(fd), uintptr(bytesPointer(p)), uintptr(len(p)), 0, 0, 0)
	if errno != 0 {
		if errno == syscall.ENOTSOCK {
			return syscall.Read(fd, p)
		}
		return int(n), errno
	}
	return int(n), nil
}

// Send writes to a connected stream socket.
func Send(fd int, p []byte) (int, error) {
	n, _, errno := syscall.Syscall6(syscall.SYS_SENDTO, uintptr(fd), uintptr(bytesPointer(p)), uintptr(len(p)), 0, 0, 0)
	if errno != 0 {
		if errno == syscall.ENOTSOCK {
			return syscall.Write(fd, p)
		}
		return int(n), errno
	}
	return int(n), nil
}

// sendmsgIovecs writes the iovecs of a connected stream socket with sendmsg.
func sendmsgIovecs(fd int, iovecs []unix.Iovec) (int, error) {
	var msg unix.Msghdr
	msg.Iov = &iovecs[0]
	msg.SetIovlen(len(iovecs))
	n, _, errno := unix.Syscall(unix.SYS_SENDMSG, uintptr(fd), uintptr(unsafe.Pointer(&msg)), 0)
	if errno != 0 {
		if errno == unix.ENOTSOCK {
			n, _, errno = unix.Syscall(unix.SYS_WRITEV, uintptr(fd), uintptr(unsafe.Pointer(&iovecs[0])), uintptr(len(iovecs)))
			if errno == 0 {
				return int(n), nil
			}
		}
		return int(n), errno
	}
	return int(n), nil
}
