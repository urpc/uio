package uws

import (
	"errors"
	"testing"
	"time"
	"unsafe"
)

func TestConnFitsCompact64BitSizeClass(t *testing.T) {
	if unsafe.Sizeof(uintptr(0)) != 8 {
		return
	}
	// 192 is a size class whose objects start on a cache line, and the fields
	// every message touches fit in the first two lines. Rarely used state,
	// such as a deferred Close frame, lives behind pointers.
	const maximum = uintptr(192)
	if size := unsafe.Sizeof(Conn{}); size > maximum {
		t.Fatalf("Conn size = %d bytes, want at most %d", size, maximum)
	}
	const handshakeMaximum = uintptr(96)
	if size := unsafe.Sizeof(handshakeState{}); size > handshakeMaximum {
		t.Fatalf("handshakeState size = %d bytes, want at most %d", size, handshakeMaximum)
	}
}

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
