//go:build linux && !race

package socket

import (
	"runtime"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Stream sockets are read and written with recvfrom, sendto and sendmsg.
// Those enter the socket layer directly, while read, write and writev go
// through the VFS first, which takes the file position lock and runs the file
// permission hooks on every call before reaching the same socket code.
// A descriptor that is not a socket falls back to the VFS calls.
//
// Sends pass MSG_NOSIGNAL. A peer that has gone makes them fail with EPIPE
// either way; without the flag the kernel also raises SIGPIPE, which a
// program that subscribes to the signal receives for every such send.
//
// The calls are raw on hosts with Ps to spare: they do not enter and exit
// through the runtime's syscall accounting, so the calling thread keeps its P
// for the whole call. Over loopback a send runs the receiving TCP path too
// and can hold a core for tens of microseconds; an accounted call hands the P
// off for that span and the worker then queues behind other goroutines to get
// one back — measured at ~300µs mean per send, 2.5k times a second per pool
// under echo load. Raw calls removed that wait and gained 2–7% throughput in
// go-websocket-benchmark and HttpArena with equal or better CPU efficiency.
//
// The trade inverts on a small host. Kernel time the runtime cannot see is
// invisible to GOMAXPROCS sizing and to the taskgo monitor, and with few Ps a
// thread held in a syscall is a large share of the machine: echo measured −18%
// at four Ps, +13% at eight, +2–4% from twelve up. So the calls stay raw only
// from eight Ps on, and small hosts keep the accounted path they had.
const rawMinProcs = 8

// rawCallsEnabled is the gate's answer, read once. runtime.GOMAXPROCS takes
// the runtime's global scheduler lock, and a check inside Recv, Send and
// sendmsgIovecs puts that lock on the hot path: under echo the lock's own
// ceiling, three calls per message, became the server's — 1.5M requests a
// second on 64 cores instead of 3.7M. The answer only says whether this host
// has Ps to spare, and GOMAXPROCS does not move under a running server, so it
// is settled before any goroutine reads it.
var rawCallsEnabled = runtime.GOMAXPROCS(0) >= rawMinProcs

var zeroByte byte

func bytesPointer(p []byte) unsafe.Pointer {
	if len(p) == 0 {
		return unsafe.Pointer(&zeroByte)
	}
	return unsafe.Pointer(&p[0])
}

// Recv reads from a connected stream socket.
func Recv(fd int, p []byte) (int, error) {
	var n uintptr
	var errno syscall.Errno
	if rawCallsEnabled {
		n, _, errno = syscall.RawSyscall6(syscall.SYS_RECVFROM, uintptr(fd), uintptr(bytesPointer(p)), uintptr(len(p)), 0, 0, 0)
	} else {
		n, _, errno = syscall.Syscall6(syscall.SYS_RECVFROM, uintptr(fd), uintptr(bytesPointer(p)), uintptr(len(p)), 0, 0, 0)
	}
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
	var n uintptr
	var errno syscall.Errno
	if rawCallsEnabled {
		n, _, errno = syscall.RawSyscall6(syscall.SYS_SENDTO, uintptr(fd), uintptr(bytesPointer(p)), uintptr(len(p)), unix.MSG_NOSIGNAL, 0, 0)
	} else {
		n, _, errno = syscall.Syscall6(syscall.SYS_SENDTO, uintptr(fd), uintptr(bytesPointer(p)), uintptr(len(p)), unix.MSG_NOSIGNAL, 0, 0)
	}
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
	var n uintptr
	var errno syscall.Errno
	if rawCallsEnabled {
		n, _, errno = unix.RawSyscall(unix.SYS_SENDMSG, uintptr(fd), uintptr(unsafe.Pointer(&msg)), unix.MSG_NOSIGNAL)
	} else {
		n, _, errno = unix.Syscall(unix.SYS_SENDMSG, uintptr(fd), uintptr(unsafe.Pointer(&msg)), unix.MSG_NOSIGNAL)
	}
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
