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

// unmaskASCIIKeys include a zero key and one whose every byte has the high bit
// set, which makes an all-ASCII payload look non-ASCII while masked: the
// verdict must come from the unmasked bytes.
var unmaskASCIIKeys = [][4]byte{{0x12, 0x34, 0x56, 0x78}, {}, {0xff, 0xff, 0xff, 0xff}, {0x80, 0x01, 0xfe, 0x7f}}

func unmaskASCIISizes() []int {
	sizes := make([]int, 0, 320)
	for size := 0; size <= 300; size++ {
		sizes = append(sizes, size)
	}
	return append(sizes, 511, 512, 513, 1023, 1024, 1025, 4095, 4096, 4097, 65539)
}

// unmaskASCIIPositions are where a non-ASCII byte is planted: everywhere in a
// short payload, and at each edge of the vector body, the 32- and 8-byte word
// loops and the byte tail in a long one.
func unmaskASCIIPositions(size int) []int {
	if size <= 300 {
		positions := make([]int, size)
		for i := range positions {
			positions[i] = i
		}
		return positions
	}
	var positions []int
	seen := map[int]bool{}
	body := size &^ 31
	for _, p := range []int{0, 1, 31, 32, 127, 128, 129, size / 2, body - 1, body, body + 1, size - 9, size - 8, size - 1} {
		if p >= 0 && p < size && !seen[p] {
			seen[p] = true
			positions = append(positions, p)
		}
	}
	return positions
}

// checkUnmaskASCII compares UnmaskASCII with a bytewise unmask and scan, for
// all-ASCII payloads and for payloads with one non-ASCII byte planted at each
// position unmaskASCIIPositions gives.
func checkUnmaskASCII(t *testing.T) {
	t.Helper()
	for _, key := range unmaskASCIIKeys {
		for _, size := range unmaskASCIISizes() {
			plain := make([]byte, size)
			for i := range plain {
				plain[i] = byte(i*7+size) & 0x7f
			}
			try := func(plain []byte, wantASCII bool, where string) {
				t.Helper()
				payload := append([]byte(nil), plain...)
				unmaskBytewise(payload, key, 0) // masking is the same XOR
				gotASCII := UnmaskASCII(payload, key)
				if !bytes.Equal(payload, plain) {
					t.Fatalf("key %x size %d %s: unmasked bytes differ", key, size, where)
				}
				if gotASCII != wantASCII {
					t.Fatalf("key %x size %d %s: ASCII = %v, want %v", key, size, where, gotASCII, wantASCII)
				}
			}
			try(plain, true, "all ASCII")
			for _, p := range unmaskASCIIPositions(size) {
				poisoned := append([]byte(nil), plain...)
				poisoned[p] |= 0x80
				try(poisoned, false, fmt.Sprintf("non-ASCII at %d", p))
			}
		}
	}
}

func TestUnmaskASCIIMatchesBytewiseReference(t *testing.T) {
	checkUnmaskASCII(t)
}

// Unmask and UnmaskASCII leave identical bytes behind.
func TestUnmaskASCIIUnmasksLikeUnmask(t *testing.T) {
	key := [4]byte{9, 0x81, 0x33, 0xc4}
	for _, size := range unmaskASCIISizes() {
		a := make([]byte, size)
		for i := range a {
			a[i] = byte(i * 151)
		}
		b := append([]byte(nil), a...)
		Unmask(a, key)
		UnmaskASCII(b, key)
		if !bytes.Equal(a, b) {
			t.Fatalf("size %d: UnmaskASCII left different bytes than Unmask", size)
		}
	}
}

func BenchmarkUnmaskASCII(b *testing.B) {
	key := [4]byte{0x12, 0x34, 0x56, 0x78}
	for _, size := range []int{16, 64, 128, 256, 1024, 4096} {
		b.Run(fmt.Sprintf("%d/ascii", size), func(b *testing.B) {
			payload := make([]byte, size)
			b.SetBytes(int64(size))
			for i := 0; i < b.N; i++ {
				UnmaskASCII(payload, key)
			}
		})
		b.Run(fmt.Sprintf("%d/unmask", size), func(b *testing.B) {
			payload := make([]byte, size)
			b.SetBytes(int64(size))
			for i := 0; i < b.N; i++ {
				Unmask(payload, key)
			}
		})
	}
}
