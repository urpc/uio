package uws

import (
	"bytes"
	"encoding/binary"
	"math/rand/v2"
	"testing"
)

func TestUnmaskShortMatchesBytewiseUnmask(t *testing.T) {
	random := rand.New(rand.NewPCG(1, 2))
	for size := 0; size < 126; size++ {
		for _, ascii := range []bool{true, false} {
			key := [4]byte{byte(random.Uint32()), byte(random.Uint32()), byte(random.Uint32()), byte(random.Uint32())}
			plain := make([]byte, size)
			for i := range plain {
				plain[i] = byte(random.Uint32()) & 0x7f
			}
			if !ascii {
				if size == 0 {
					continue
				}
				plain[random.IntN(size)] |= 0x80
			}
			masked := make([]byte, size)
			for i := range plain {
				masked[i] = plain[i] ^ key[i&3]
			}
			if got := unmaskShort(masked, binary.LittleEndian.Uint32(key[:])); got != ascii {
				t.Fatalf("size %d: ascii = %v, want %v", size, got, ascii)
			}
			if !bytes.Equal(masked, plain) {
				t.Fatalf("size %d: unmasked %x, want %x", size, masked, plain)
			}
		}
	}
}
