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

// An encoder that keeps its context, or runs a smaller window, reuses its
// writer: once warm, a message allocates nothing — it once built a writer
// of about a megabyte per message.
func TestEncoderReusesItsWriter(t *testing.T) {
	payload := bytes.Repeat([]byte("reused writer, "), 70)
	for _, mode := range []struct {
		name      string
		noContext bool
		bits      int
	}{{"context", false, 15}, {"small window", true, 12}, {"small window with context", false, 12}} {
		t.Run(mode.name, func(t *testing.T) {
			encoder := NewEncoderWithWindow(-1, mode.noContext, mode.bits)
			use := func([]byte) error { return nil }
			for range 4 {
				_ = encoder.EncodeBorrowed(payload, use)
				encoder.Commit(payload)
			}
			allocs := testing.AllocsPerRun(50, func() {
				_ = encoder.EncodeBorrowed(payload, use)
				encoder.Commit(payload)
			})
			if allocs > 1 {
				t.Fatalf("%v allocations a message", allocs)
			}
		})
	}
}
