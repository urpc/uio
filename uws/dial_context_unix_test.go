//go:build linux || darwin || netbsd || freebsd || openbsd || dragonfly

package uws

import (
	"context"
	"errors"
	"fmt"
	"golang.org/x/sys/unix"
	"net"
	"testing"
	"time"
)

func TestDialContextBoundsTCPConnect(t *testing.T) {
	addr := saturatedTCPListener(t)
	dialer := NewDialer(Options{})
	t.Cleanup(func() {
		_ = dialer.Close(nil)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	started := time.Now()
	err := dialer.Dial(ctx, "ws://"+addr+"/", nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Dial() error = %v, want %v", err, context.DeadlineExceeded)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("Dial() returned after %v, want under 1s", elapsed)
	}
}

func saturatedTCPListener(t *testing.T) string {
	t.Helper()
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	unix.CloseOnExec(fd)
	if err = unix.Bind(fd, &unix.SockaddrInet4{Addr: [4]byte{127, 0, 0, 1}}); err != nil {
		_ = unix.Close(fd)
		t.Fatal(err)
	}
	if err = unix.Listen(fd, 1); err != nil {
		_ = unix.Close(fd)
		t.Fatal(err)
	}
	sockaddr, err := unix.Getsockname(fd)
	if err != nil {
		_ = unix.Close(fd)
		t.Fatal(err)
	}
	port := sockaddr.(*unix.SockaddrInet4).Port
	addr := net.JoinHostPort("127.0.0.1", fmt.Sprint(port))
	held := make([]net.Conn, 0, 4)
	t.Cleanup(func() {
		for _, conn := range held {
			_ = conn.Close()
		}
		_ = unix.Close(fd)
	})
	for attempts := 0; attempts < 16; attempts++ {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		conn, dialErr := (&net.Dialer{}).DialContext(ctx, "tcp", addr)
		ctxErr := ctx.Err()
		cancel()
		if errors.Is(ctxErr, context.DeadlineExceeded) || isTimeout(dialErr) {
			return addr
		}
		if dialErr != nil {
			t.Fatalf("saturate listener: %v", dialErr)
		}
		held = append(held, conn)
	}
	t.Fatal("could not saturate listener backlog")
	return ""
}

func isTimeout(err error) bool {
	timeout, ok := err.(net.Error)
	return ok && timeout.Timeout()
}
