//go:build netbsd || dragonfly

package socket

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// SetKeepAlivePeriod enables TCP keepalive and configures an idle period,
// probe interval, and bounded probe count.
func SetKeepAlivePeriod(fd, seconds int) error {
	if seconds <= 0 {
		return errors.New("invalid time duration")
	}
	if err := unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_KEEPALIVE, 1); err != nil {
		return os.NewSyscallError("setsockopt", err)
	}
	if err := unix.SetsockoptInt(fd, unix.IPPROTO_TCP, unix.TCP_KEEPIDLE, seconds); err != nil {
		return os.NewSyscallError("setsockopt", err)
	}
	if err := unix.SetsockoptInt(fd, unix.IPPROTO_TCP, unix.TCP_KEEPINTVL, max(seconds/5, 1)); err != nil {
		return os.NewSyscallError("setsockopt", err)
	}
	return os.NewSyscallError("setsockopt", unix.SetsockoptInt(fd, unix.IPPROTO_TCP, unix.TCP_KEEPCNT, 5))
}
