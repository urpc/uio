package bytebuf

import (
	"fmt"
	"testing"
)

func TestBufferPoolSizeClasses(t *testing.T) {
	for _, test := range []struct {
		requested int
		capacity  int
	}{
		{1, 512},
		{16, 512},
		{17, 512},
		{64, 512},
		{65, 512},
		{1024, 1024},
		{65536, 65536},
		{65537, 65537},
	} {
		buffer := getBuffer(test.requested)
		if got := buffer.Cap(); got != test.capacity {
			t.Errorf("getBuffer(%d) capacity = %d, want %d", test.requested, got, test.capacity)
		}
		putBuffer(buffer)
	}
}

var poolSink int

// BenchmarkAcquireRelease borrows a block the way a write batch does: acquire,
// fill a little, release.
func BenchmarkAcquireRelease(b *testing.B) {
	for _, size := range []int{1024, 8 << 10, 64 << 10} {
		b.Run(fmt.Sprintf("%d", size), func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				buffer := AcquireBuffer(size)
				buffer.AvailableBuffer()[:64][0] = 1
				buffer.CommitWrite(64)
				poolSink += buffer.Len()
				ReleaseBuffer(buffer)
			}
		})
	}
}

// BenchmarkCloneRelease copies a write into a pooled block and releases it
// after the send, as a Write from outside the connection's turn does.
func BenchmarkCloneRelease(b *testing.B) {
	for _, size := range []int{64, 1024} {
		payload := make([]byte, size)
		b.Run(fmt.Sprintf("%d", size), func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				buffer := CloneBuffer(payload)
				poolSink += buffer.Len()
				ReleaseBuffer(buffer)
			}
		})
	}
}
