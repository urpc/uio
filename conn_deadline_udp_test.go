package uio

import (
	"errors"
	"net"
	"os"
	"testing"
	"time"
)

// dialConnectedUDP starts a dial-only Events and dials a datagram connection
// to a local receiver, returning the connection and the channel the Close
// cause arrives on.
func dialConnectedUDP(t *testing.T) (Conn, chan error) {
	t.Helper()
	receiver, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = receiver.Close() })
	closed := make(chan error, 1)
	started := make(chan struct{})
	events := withFuncs(&Events{Pollers: 1}, FuncHandler{
		Start: func(*Events) { close(started) },
		Read:  func(Conn) error { return nil },
		Close: func(_ Conn, err error) { signal(closed, err) },
	})
	done := make(chan error, 1)
	go func() { done <- events.Serve() }()
	t.Cleanup(func() {
		_ = events.Close(nil)
		<-done
	})
	<-started
	conn, err := events.Dial("udp://" + receiver.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn, closed
}

// A write deadline bounds a connected datagram connection too: one that
// passes closes it with os.ErrDeadlineExceeded even when nothing is written,
// on every backend.
func TestConnDeadlineUDPIdleWriteDeadlineCloses(t *testing.T) {
	conn, closed := dialConnectedUDP(t)
	if err := conn.SetWriteDeadline(time.Now().Add(50 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-closed:
		if !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("closed with %v, want the write deadline", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("an idle datagram connection outlived its write deadline")
	}
}

// A write deadline cleared before it passes never closes a datagram
// connection.
func TestConnDeadlineUDPClearedWriteDeadlineNeverCloses(t *testing.T) {
	conn, closed := dialConnectedUDP(t)
	if err := conn.SetWriteDeadline(time.Now().Add(100 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if err := conn.SetWriteDeadline(time.Time{}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-closed:
		t.Fatalf("a cleared write deadline closed the connection: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
}
