//go:build windows

package uio

import (
	"errors"
	"syscall"
)

// isPeerGoneWrite reports a write that failed because the peer closed the
// connection. Windows reports it as a WSA error; syscall.ECONNRESET there is
// an invented APPLICATION_ERROR value, so errors.Is never matches the socket's
// own errno.
func isPeerGoneWrite(err error) bool {
	return errors.Is(err, syscall.WSAECONNRESET) ||
		errors.Is(err, syscall.WSAECONNABORTED) ||
		errors.Is(err, syscall.ERROR_NETNAME_DELETED)
}
