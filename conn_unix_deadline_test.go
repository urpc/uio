//go:build (linux || darwin || netbsd || freebsd || openbsd || dragonfly) && !stdio

package uio

import (
	"errors"
	"os"
	"testing"
	"time"
)

// A write deadline bounds the connection itself: one that passes closes it
// with os.ErrDeadlineExceeded even when nothing is written, on every backend.
func TestConnDeadlineUnixIdleWriteDeadlineCloses(t *testing.T) {
	closed := make(chan error, 1)
	events := withFuncs(&Events{Pollers: 1}, FuncHandler{
		Read:  func(Conn) error { return nil },
		Close: func(_ Conn, err error) { signal(closed, err) },
	})
	client, server := serveInboundTest(t, events)
	defer client.Close()
	if err := server.SetWriteDeadline(time.Now().Add(50 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-closed:
		if !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("closed with %v, want the write deadline", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("an idle connection outlived its write deadline")
	}
}

// A write deadline cleared before it passes never closes the connection.
func TestConnDeadlineUnixClearedWriteDeadlineNeverCloses(t *testing.T) {
	closed := make(chan error, 1)
	events := withFuncs(&Events{Pollers: 1}, FuncHandler{
		Read:  func(Conn) error { return nil },
		Close: func(_ Conn, err error) { signal(closed, err) },
	})
	client, server := serveInboundTest(t, events)
	defer client.Close()
	if err := server.SetWriteDeadline(time.Now().Add(100 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if err := server.SetWriteDeadline(time.Time{}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-closed:
		t.Fatalf("a cleared write deadline closed the connection: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
}
