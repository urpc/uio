package uws

import (
	"testing"
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
