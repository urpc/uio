package compress

import (
	"bytes"
	"fmt"
	"testing"
)

func BenchmarkBorrowedCodecNoContext(b *testing.B) {
	for _, size := range []int{1024, 64 << 10} {
		b.Run(fmt.Sprintf("encode_%d", size), func(b *testing.B) {
			payload := bytes.Repeat([]byte("compressible-"), size/len("compressible-")+1)[:size]
			encoder := NewEncoder(-1, true)
			consume := func([]byte) error { return nil }
			if err := encoder.EncodeBorrowed(payload, consume); err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.SetBytes(int64(size))
			b.ResetTimer()
			for b.Loop() {
				if err := encoder.EncodeBorrowed(payload, consume); err != nil {
					b.Fatal(err)
				}
			}
		})
		b.Run(fmt.Sprintf("decode_%d", size), func(b *testing.B) {
			payload := bytes.Repeat([]byte("compressible-"), size/len("compressible-")+1)[:size]
			encoded, err := Compress(payload, -1)
			if err != nil {
				b.Fatal(err)
			}
			decoder := NewDecoder(true)
			consume := func([]byte) error { return nil }
			if err = decoder.DecodeBorrowed(encoded, size, consume); err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.SetBytes(int64(size))
			b.ResetTimer()
			for b.Loop() {
				if err = decoder.DecodeBorrowed(encoded, size, consume); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
