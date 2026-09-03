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

func BenchmarkStreamEncoderNoContext64KiB(b *testing.B) {
	payload := bytes.Repeat([]byte("compressible-"), (64<<10)/len("compressible-")+1)[:64<<10]
	encoder := NewEncoder(-1, true)
	emit := func([]byte) error { return nil }
	b.ReportAllocs()
	b.SetBytes(int64(len(payload)))
	b.ResetTimer()
	for b.Loop() {
		stream, err := encoder.NewStream(emit)
		if err != nil {
			b.Fatal(err)
		}
		if _, err = stream.Write(payload); err != nil {
			b.Fatal(err)
		}
		if err = stream.Close(); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkStreamEncoderDictionary64MiB(b *testing.B) {
	const messageSize = 64 << 20
	for _, chunkSize := range []int{1 << 10, 32 << 10, 1 << 20} {
		b.Run(fmt.Sprintf("chunk_%d", chunkSize), func(b *testing.B) {
			chunk := make([]byte, chunkSize)
			b.ReportAllocs()
			b.SetBytes(messageSize)
			b.ResetTimer()
			for range b.N {
				encoder := NewEncoder(-1, false)
				stream, err := encoder.NewStream(func([]byte) error { return nil })
				if err != nil {
					b.Fatal(err)
				}
				for written := 0; written < messageSize; written += chunkSize {
					if _, err = stream.Write(chunk); err != nil {
						b.Fatal(err)
					}
				}
				if err = stream.Close(); err != nil {
					b.Fatal(err)
				}
				encoder.CommitStream(stream)
			}
		})
	}
}
