//go:build !race

package compress

import (
	"bytes"
	"testing"
)

func TestBorrowedNoContextCodecReusesAllocations(t *testing.T) {
	payload := bytes.Repeat([]byte("compressible-"), 128)
	encoded, err := Compress(payload, -1)
	if err != nil {
		t.Fatal(err)
	}
	encoder := NewEncoder(-1, true)
	decoder := NewDecoder(true)
	consume := func([]byte) error { return nil }
	if err = encoder.EncodeBorrowed(payload, consume); err != nil {
		t.Fatal(err)
	}
	if err = decoder.DecodeBorrowed(encoded, len(payload), consume); err != nil {
		t.Fatal(err)
	}
	encodeAllocs := testing.AllocsPerRun(1000, func() {
		if encodeErr := encoder.EncodeBorrowed(payload, consume); encodeErr != nil {
			panic(encodeErr)
		}
	})
	if encodeAllocs != 0 {
		t.Fatalf("borrowed encode allocations = %v, want 0", encodeAllocs)
	}
	decodeAllocs := testing.AllocsPerRun(1000, func() {
		if decodeErr := decoder.DecodeBorrowed(encoded, len(payload), consume); decodeErr != nil {
			panic(decodeErr)
		}
	})
	if decodeAllocs != 0 {
		t.Fatalf("borrowed decode allocations = %v, want 0", decodeAllocs)
	}
}

func TestRollingDictionaryAllocationsDoNotScaleWithChunks(t *testing.T) {
	const (
		window      = 32 << 10
		chunkSize   = 1 << 10
		messageSize = 8 << 20
	)
	chunk := make([]byte, chunkSize)
	allocations := testing.AllocsPerRun(10, func() {
		dictionary := newRollingDictionary(window, nil)
		for written := 0; written < messageSize; written += chunkSize {
			dictionary.Append(chunk)
		}
		if got := len(dictionary.Take()); got != window {
			panic(got)
		}
	})
	if allocations != 1 {
		t.Fatalf("rolling dictionary allocations = %v, want 1", allocations)
	}
}

func TestNoContextDictionaryOperationsDoNotAllocate(t *testing.T) {
	encoder := NewEncoder(-1, true)
	stream, err := encoder.NewStream(func([]byte) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if _, err = stream.Write(make([]byte, 1024)); err != nil {
		t.Fatal(err)
	}
	if err = stream.Close(); err != nil {
		t.Fatal(err)
	}
	allocations := testing.AllocsPerRun(1000, func() {
		if stream.Dictionary() != nil {
			panic("unexpected dictionary")
		}
		encoder.CommitStream(stream)
	})
	if allocations != 0 {
		t.Fatalf("no-context dictionary allocations = %v, want 0", allocations)
	}
}
