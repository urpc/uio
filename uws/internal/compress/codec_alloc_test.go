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
