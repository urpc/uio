package frame

import (
	"bytes"
	"fmt"
	"testing"
)

func TestUnmaskMatchesBytewiseReference(t *testing.T) {
	key := [4]byte{0x12, 0x34, 0x56, 0x78}
	sizes := make([]int, 0, 150)
	for size := 0; size <= 137; size++ {
		sizes = append(sizes, size)
	}
	// Larger payloads reach the unrolled vector loop and its tails.
	sizes = append(sizes, 159, 160, 161, 255, 256, 257, 1023, 1024, 1025, 4099)
	for offset := 0; offset < len(key); offset++ {
		for _, size := range sizes {
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
	for _, size := range []int{1, 4, 7, 8, 16, 64, 96, 128, 192, 256, 1024, 4096} {
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
