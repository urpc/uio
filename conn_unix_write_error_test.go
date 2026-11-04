//go:build !windows

package uio

import (
	"errors"
	"syscall"
)

// isPeerGoneWrite reports a write that failed because the peer closed the
// connection, as a reset or a broken pipe.
func isPeerGoneWrite(err error) bool {
	return errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE)
}
