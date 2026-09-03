package uws

import (
	"testing"
	"unsafe"
)

func TestConnFitsCompact64BitSizeClass(t *testing.T) {
	if unsafe.Sizeof(uintptr(0)) != 8 {
		return
	}
	const maximum = uintptr(168)
	if size := unsafe.Sizeof(Conn{}); size > maximum {
		t.Fatalf("Conn size = %d bytes, want at most %d", size, maximum)
	}
	const handshakeMaximum = uintptr(96)
	if size := unsafe.Sizeof(handshakeState{}); size > handshakeMaximum {
		t.Fatalf("handshakeState size = %d bytes, want at most %d", size, handshakeMaximum)
	}
}
