package uws

import (
	"errors"
	"testing"
	"time"
)

func TestConnForwardsDeadlines(t *testing.T) {
	raw := &writeProbeConn{}
	conn := &Conn{raw: raw}
	deadline := time.Unix(1, 2)
	readDeadline := time.Unix(3, 4)
	writeDeadline := time.Unix(5, 6)
	if err := conn.SetDeadline(deadline); err != nil {
		t.Fatal(err)
	}
	if err := conn.SetReadDeadline(readDeadline); err != nil {
		t.Fatal(err)
	}
	if err := conn.SetWriteDeadline(writeDeadline); err != nil {
		t.Fatal(err)
	}
	if err := conn.SetNoDelay(true); err != nil {
		t.Fatal(err)
	}
	if !raw.deadline.Equal(deadline) || !raw.readDeadline.Equal(readDeadline) || !raw.writeDeadline.Equal(writeDeadline) {
		t.Fatalf("forwarded deadlines = %v, %v, %v", raw.deadline, raw.readDeadline, raw.writeDeadline)
	}
	if !raw.noDelay {
		t.Fatal("SetNoDelay was not forwarded")
	}

	sentinel := errors.New("deadline failed")
	raw.deadlineErr = sentinel
	if err := conn.SetWriteDeadline(time.Time{}); !errors.Is(err, sentinel) {
		t.Fatalf("SetWriteDeadline() error = %v, want %v", err, sentinel)
	}
	raw.noDelayErr = sentinel
	if err := conn.SetNoDelay(false); !errors.Is(err, sentinel) {
		t.Fatalf("SetNoDelay() error = %v, want %v", err, sentinel)
	}
	var nilConn *Conn
	if err := nilConn.SetDeadline(time.Time{}); !errors.Is(err, ErrNotReady) {
		t.Fatalf("nil SetDeadline() error = %v, want %v", err, ErrNotReady)
	}
	if err := (&Conn{}).SetReadDeadline(time.Time{}); !errors.Is(err, ErrNotReady) {
		t.Fatalf("unattached SetReadDeadline() error = %v, want %v", err, ErrNotReady)
	}
	if err := (&Conn{}).SetWriteDeadline(time.Time{}); !errors.Is(err, ErrNotReady) {
		t.Fatalf("unattached SetWriteDeadline() error = %v, want %v", err, ErrNotReady)
	}
	if err := nilConn.SetNoDelay(true); !errors.Is(err, ErrNotReady) {
		t.Fatalf("nil SetNoDelay() error = %v, want %v", err, ErrNotReady)
	}
}
