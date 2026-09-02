//go:build linux

package socket

import "syscall"

// Accept creates a non-blocking descriptor that cannot leak across exec.
func Accept(fd int) (int, syscall.Sockaddr, error) {
	return syscall.Accept4(fd, syscall.SOCK_NONBLOCK|syscall.SOCK_CLOEXEC)
}
