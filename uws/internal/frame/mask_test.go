package frame

import (
	"bytes"
	"fmt"
	"testing"
)

func TestUnmaskMatchesBytewiseReference(t *testing.T) {
	key := [4]byte{0x12, 0x34, 0x56, 0x78}
	for offset := 0; offset < len(key); offset++ {
		for size := 0; size <= 137; size++ {
			payload := make([]byte, size)
			for i := range payload {
				payload[i] = byte(i*31 + size)
			}
			want := append([]byte(nil), payload...)
			unmaskBytewise(want, key, offset)

			unmask(payload, key, offset)
			if !bytes.Equal(payload, want) {
				t.Fatalf("offset %d size %d: unmask mismatch", offset, size)
			}
		}
	}
}

func BenchmarkUnmask(b *testing.B) {
	key := [4]byte{0x12, 0x34, 0x56, 0x78}
	for _, size := range []int{1, 4, 7, 8, 16, 64, 256, 1024, 4096} {
		b.Run(fmt.Sprintf("%d/optimized", size), func(b *testing.B) {
			payload := make([]byte, size)
			b.SetBytes(int64(size))
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				unmask(payload, key, i&3)
			}
		})
		b.Run(fmt.Sprintf("%d/bytewise", size), func(b *testing.B) {
			payload := make([]byte, size)
			b.SetBytes(int64(size))
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				unmaskBytewise(payload, key, i&3)
			}
		})
	}
}

func unmaskBytewise(payload []byte, key [4]byte, offset int) {
	for i := range payload {
		payload[i] ^= key[(offset+i)&3]
	}
}
