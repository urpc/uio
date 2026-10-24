//go:build linux || darwin || netbsd || freebsd || openbsd || dragonfly

package socket

import (
	"fmt"
	"syscall"
	"testing"
)

func TestClassifyAcceptError(t *testing.T) {
	cases := []struct {
		err  error
		want AcceptFailure
	}{
		{syscall.ECONNABORTED, AcceptRetry},
		{syscall.EINTR, AcceptRetry},
		{syscall.EPROTO, AcceptRetry},
		{syscall.EPERM, AcceptRetry},
		{syscall.EMFILE, AcceptBackoff},
		{syscall.ENFILE, AcceptBackoff},
		{syscall.ENOBUFS, AcceptBackoff},
		{syscall.ENOMEM, AcceptBackoff},
		{fmt.Errorf("accept: %w", syscall.EMFILE), AcceptBackoff},
		{syscall.EBADF, AcceptFatal},
		{syscall.EINVAL, AcceptFatal},
		{syscall.ENOTSOCK, AcceptFatal},
		{fmt.Errorf("not an errno"), AcceptBackoff},
	}
	for _, c := range cases {
		if got := ClassifyAcceptError(c.err); got != c.want {
			t.Errorf("ClassifyAcceptError(%v) = %d, want %d", c.err, got, c.want)
		}
	}
	if pendingNetworkError(syscall.ENETUNREACH) != (ClassifyAcceptError(syscall.ENETUNREACH) == AcceptRetry) {
		t.Error("a pending network error is not retried")
	}
}
