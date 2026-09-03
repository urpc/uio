package bytebuf

import (
	"testing"
	"unsafe"
)

func TestBufferPoolKindDoesNotGrow64BitBuffer(t *testing.T) {
	if unsafe.Sizeof(uintptr(0)) != 8 {
		return
	}
	const maximum = uintptr(40)
	if size := unsafe.Sizeof(Buffer{}); size > maximum {
		t.Fatalf("Buffer size = %d bytes, want at most %d", size, maximum)
	}
}
