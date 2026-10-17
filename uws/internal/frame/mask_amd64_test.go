//go:build amd64 && !purego

package frame

import (
	"bytes"
	"testing"
)

// The word loops alone, as on a CPU without AVX2, give the same answers.
func TestUnmaskASCIIWithoutAVX2(t *testing.T) {
	saved := useAVX2
	useAVX2 = false
	t.Cleanup(func() { useAVX2 = saved })
	checkUnmaskASCII(t)
}

// maskASCIIAVX2 itself: the bytes it leaves and its high-bit verdict match a
// bytewise reference for every multiple of 32 it is given, with one high byte
// planted at every position.
func TestMaskASCIIAVX2MatchesReference(t *testing.T) {
	if !useAVX2 {
		t.Skip("no AVX2")
	}
	key := [4]byte{0xa1, 0x02, 0x7e, 0xf3}
	key64 := uint64(key[0]) | uint64(key[1])<<8 | uint64(key[2])<<16 | uint64(key[3])<<24
	key64 |= key64 << 32
	for n := 32; n <= 640; n += 32 {
		plain := make([]byte, n)
		for i := range plain {
			plain[i] = byte(i*13) & 0x7f
		}
		for p := -1; p < n; p++ {
			want := append([]byte(nil), plain...)
			if p >= 0 {
				want[p] |= 0x80
			}
			payload := append([]byte(nil), want...)
			unmaskBytewise(payload, key, 0)
			high := maskASCIIAVX2(&payload[0], n, key64)
			if !bytes.Equal(payload, want) {
				t.Fatalf("n %d high byte %d: bytes differ", n, p)
			}
			if (high != 0) != (p >= 0) {
				t.Fatalf("n %d high byte %d: verdict %#x", n, p, high)
			}
		}
	}
}
