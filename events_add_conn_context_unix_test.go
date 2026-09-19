//go:build (linux || darwin || netbsd || freebsd || openbsd || dragonfly) && !stdio

package uio

import (
	"context"
	"errors"
	"testing"

	"golang.org/x/sys/unix"
)

func TestAddConnContextRejectsAlreadyCanceledContext(t *testing.T) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = unix.Close(fds[1]) })

	cause := errors.New("registration canceled")
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(cause)
	conn := &fdConn{fd: fds[0], commonConn: commonConn{loop: &eventLoop{}}}
	if err = (&Events{}).addConnContext(ctx, conn); !errors.Is(err, cause) {
		t.Fatalf("addConnContext() error = %v, want %v", err, cause)
	}
	if !conn.closing.Load() || !conn.closed {
		t.Fatalf("canceled connection state = closing:%v closed:%v", conn.closing.Load(), conn.closed)
	}
}
