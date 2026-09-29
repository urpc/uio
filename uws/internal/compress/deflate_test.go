package compress

import (
	"bytes"
	"errors"
	"fmt"
	"testing"
)

func TestCompressRoundTrip(t *testing.T) {
	payload := bytes.Repeat([]byte("websocket payload "), 100)
	compressed, err := Compress(payload, -1)
	if err != nil {
		t.Fatal(err)
	}
	if len(compressed) >= len(payload) {
		t.Fatalf("compressed size = %d, payload size = %d", len(compressed), len(payload))
	}
	decoded, err := Decompress(compressed, len(payload))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(decoded, payload) {
		t.Fatal("decompressed payload differs")
	}
}

func TestCompressRoundTripEmptyPayload(t *testing.T) {
	compressed, err := Compress(nil, -1)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := Decompress(compressed, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(decoded) != 0 {
		t.Fatalf("decoded length = %d, want 0", len(decoded))
	}
}

func TestBorrowedCodecCallbacks(t *testing.T) {
	payload := bytes.Repeat([]byte("borrowed-payload-"), 64)
	encoder := NewEncoder(-1, true)
	var encoded []byte
	if err := encoder.EncodeBorrowed(payload, func(value []byte) error {
		encoded = append(encoded, value...)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	decoder := NewDecoder(true)
	if err := decoder.DecodeBorrowed(encoded, len(payload), func(value []byte) error {
		if !bytes.Equal(value, payload) {
			t.Fatal("borrowed decode payload mismatch")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	wantErr := errors.New("callback failed")
	if err := encoder.EncodeBorrowed(payload, func([]byte) error { return wantErr }); !errors.Is(err, wantErr) {
		t.Fatalf("encode callback error = %v, want %v", err, wantErr)
	}
	if err := decoder.DecodeBorrowed(encoded, len(payload), func([]byte) error { return wantErr }); !errors.Is(err, wantErr) {
		t.Fatalf("decode callback error = %v, want %v", err, wantErr)
	}
	if err := encoder.EncodeBorrowed(payload, nil); err == nil {
		t.Fatal("nil encode callback succeeded")
	}
	if err := decoder.DecodeBorrowed(encoded, len(payload), nil); err == nil {
		t.Fatal("nil decode callback succeeded")
	}
}

func TestDecompressEnforcesLimit(t *testing.T) {
	compressed, err := Compress(bytes.Repeat([]byte{'x'}, 1024), -1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = Decompress(compressed, 100); err != ErrTooLarge {
		t.Fatalf("Decompress() error = %v, want %v", err, ErrTooLarge)
	}
}

func TestContextTakeoverCarriesDictionaryAcrossMessages(t *testing.T) {
	encoder := NewEncoder(-1, false)
	decoder := NewDecoder(false)
	first := bytes.Repeat([]byte("dictionary-value-"), 64)
	second := append([]byte("message:"), first...)
	encodedFirst, err := encoder.Encode(first)
	if err != nil {
		t.Fatal(err)
	}
	encoder.Commit(first)
	encodedSecond, err := encoder.Encode(second)
	if err != nil {
		t.Fatal(err)
	}
	decodedFirst, err := decoder.Decode(encodedFirst, len(first))
	if err != nil {
		t.Fatal(err)
	}
	decodedSecond, err := decoder.Decode(encodedSecond, len(second))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(decodedFirst, first) || !bytes.Equal(decodedSecond, second) {
		t.Fatal("context takeover round trip differs")
	}
}

func TestEncoderDoesNotAdvanceUntilCommit(t *testing.T) {
	first := bytes.Repeat([]byte("dictionary-value-"), 1024)
	second := append([]byte("message:"), first...)

	withoutCommit := NewEncoder(-1, false)
	if _, err := withoutCommit.Encode(first); err != nil {
		t.Fatal(err)
	}
	without := mustEncode(t, withoutCommit, second)

	withCommit := NewEncoder(-1, false)
	if _, err := withCommit.Encode(first); err != nil {
		t.Fatal(err)
	}
	withCommit.Commit(first)
	with := mustEncode(t, withCommit, second)
	if len(with) >= len(without) {
		t.Fatalf("committed dictionary did not improve encoding: committed=%d uncommitted=%d", len(with), len(without))
	}
}

func TestWindowedContextTakeoverRoundTrip(t *testing.T) {
	for _, bits := range []int{8, 12, 15} {
		t.Run(fmt.Sprintf("w%d", bits), func(t *testing.T) {
			encoder := NewEncoderWithWindow(-1, false, bits)
			decoder := NewDecoderWithWindow(false, bits)
			first := bytes.Repeat([]byte("windowed-dictionary-"), 128)
			second := append([]byte("next:"), first...)
			encodedFirst := mustEncode(t, encoder, first)
			encoder.Commit(first)
			encodedSecond := mustEncode(t, encoder, second)
			decodedFirst, err := decoder.Decode(encodedFirst, len(first))
			if err != nil {
				t.Fatal(err)
			}
			decodedSecond, err := decoder.Decode(encodedSecond, len(second))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(decodedFirst, first) || !bytes.Equal(decodedSecond, second) {
				t.Fatal("windowed context takeover round trip differs")
			}
		})
	}
}

func TestEncoderDecoderLifecycleAndInvalidInputs(t *testing.T) {
	if _, err := Compress([]byte("payload"), 100); err == nil {
		t.Fatal("Compress accepted invalid level")
	}
	if _, err := NewEncoder(100, true).Encode([]byte("payload")); err == nil {
		t.Fatal("invalid compression level was accepted")
	}
	encoder := NewEncoderWithWindow(-1, true, 7)
	if encoder.windowBits != DefaultWindowBits {
		t.Fatalf("normalized window bits = %d", encoder.windowBits)
	}
	encoder.Commit([]byte("ignored"))
	if err := encoder.Close(); err != nil {
		t.Fatal(err)
	}
	var nilEncoder *Encoder
	nilEncoder.Commit(nil)

	decoder := NewDecoderWithWindow(true, 20)
	if decoder.windowBytes != 1<<DefaultWindowBits {
		t.Fatalf("normalized decoder window = %d", decoder.windowBytes)
	}
	if _, err := decoder.Decode([]byte{0xff, 0xff}, 1024); err == nil {
		t.Fatal("invalid compressed data was accepted")
	}
	if err := decoder.Close(); err != nil {
		t.Fatal(err)
	}
	payload := []byte("unbounded decode")
	encoded := mustEncode(t, NewEncoder(-1, true), payload)
	decoded, err := NewDecoder(true).Decode(encoded, 0)
	if err != nil || !bytes.Equal(decoded, payload) {
		t.Fatalf("unbounded decode = %q, %v", decoded, err)
	}
}

func mustEncode(t *testing.T, encoder *Encoder, payload []byte) []byte {
	t.Helper()
	data, err := encoder.Encode(payload)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func FuzzDecompressNeverPanics(f *testing.F) {
	f.Add([]byte{3, 0})
	f.Add([]byte{0x4a, 0x4d, 0x2d, 0x2e, 0x01, 0x00})
	f.Fuzz(func(t *testing.T, data []byte) {
		defer func() {
			if recovered := recover(); recovered != nil {
				t.Fatalf("decompressor panicked: %v", recovered)
			}
		}()
		_, _ = Decompress(data, 1<<20)
	})
}
