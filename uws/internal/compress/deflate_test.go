package compress

import (
	"bytes"
	"errors"
	"fmt"
	"io"
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

func TestStreamEncoderAbortDoesNotEmitMoreData(t *testing.T) {
	emissions := 0
	stream, err := NewEncoder(-1, true).NewStream(func([]byte) error {
		emissions++
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = stream.Write([]byte("payload")); err != nil {
		t.Fatal(err)
	}
	beforeAbort := emissions
	stream.Abort()
	stream.Abort()
	if emissions != beforeAbort {
		t.Fatalf("Abort emitted %d additional chunks", emissions-beforeAbort)
	}
	if err = stream.Close(); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("Close after Abort error = %v, want io.ErrClosedPipe", err)
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

func TestStreamEncoderEmitsDecodableMessage(t *testing.T) {
	for _, bits := range []int{8, 15} {
		t.Run(fmt.Sprintf("w%d", bits), func(t *testing.T) {
			var encoded []byte
			stream, err := NewEncoderWithWindow(-1, false, bits).NewStream(func(chunk []byte) error {
				encoded = append(encoded, chunk...)
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			payload := bytes.Repeat([]byte("streaming-payload-"), 128)
			if n, err := stream.Write(payload[:len(payload)/2]); err != nil || n != len(payload)/2 {
				t.Fatalf("first stream write = %d, %v", n, err)
			}
			if n, err := stream.Write(payload[len(payload)/2:]); err != nil || n != len(payload)-len(payload)/2 {
				t.Fatalf("second stream write = %d, %v", n, err)
			}
			if err := stream.Close(); err != nil {
				t.Fatal(err)
			}
			decoded, err := NewDecoderWithWindow(true, bits).Decode(encoded, len(payload))
			if err != nil || !bytes.Equal(decoded, payload) {
				t.Fatalf("stream decode = %d bytes, %v", len(decoded), err)
			}
		})
	}
}

func TestStreamEncoderPublishesContextAfterClose(t *testing.T) {
	encoder := NewEncoderWithWindow(-1, false, 8)
	var firstWire []byte
	stream, err := encoder.NewStream(func(chunk []byte) error {
		firstWire = append(firstWire, chunk...)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	first := bytes.Repeat([]byte("stream-dictionary-"), 64)
	second := append([]byte("next:"), first...)
	if _, err = stream.Write(first); err != nil {
		t.Fatal(err)
	}
	if err = stream.Close(); err != nil {
		t.Fatal(err)
	}
	encoder.CommitStream(stream)
	secondWire := mustEncode(t, encoder, second)
	decoder := NewDecoderWithWindow(false, 8)
	if decoded, err := decoder.Decode(firstWire, len(first)); err != nil || !bytes.Equal(decoded, first) {
		t.Fatalf("first stream decode = %d bytes, %v", len(decoded), err)
	}
	if decoded, err := decoder.Decode(secondWire, len(second)); err != nil || !bytes.Equal(decoded, second) {
		t.Fatalf("second stream decode = %d bytes, %v", len(decoded), err)
	}
}

func TestNoContextStreamDoesNotTrackDictionary(t *testing.T) {
	encoder := NewEncoder(-1, true)
	stream, err := encoder.NewStream(func([]byte) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if _, err = stream.Write(bytes.Repeat([]byte("payload"), 1024)); err != nil {
		t.Fatal(err)
	}
	if stream.dictionary.data != nil || stream.Dictionary() != nil {
		t.Fatal("no-context stream retained a dictionary")
	}
	if err = stream.Close(); err != nil {
		t.Fatal(err)
	}
	encoder.CommitStream(stream)
	if encoder.dictionary != nil {
		t.Fatal("no-context encoder adopted a stream dictionary")
	}
}

func TestStreamEncoderTransfersRollingDictionary(t *testing.T) {
	const window = 1 << 8
	encoder := NewEncoderWithWindow(-1, false, 8)
	initial := bytes.Repeat([]byte("initial-"), 20)
	encoder.SetDictionary(initial)
	stream, err := encoder.NewStream(func([]byte) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	chunks := [][]byte{
		bytes.Repeat([]byte{'a'}, 100),
		bytes.Repeat([]byte{'b'}, 200),
		bytes.Repeat([]byte{'c'}, 300),
	}
	want := append([]byte(nil), initial...)
	for _, chunk := range chunks {
		if _, err = stream.Write(chunk); err != nil {
			t.Fatal(err)
		}
		want = append(want, chunk...)
	}
	want = want[len(want)-window:]
	if err = stream.Close(); err != nil {
		t.Fatal(err)
	}
	if got := stream.Dictionary(); !bytes.Equal(got, want) {
		t.Fatalf("rolling dictionary = %q, want %q", got, want)
	}
	backing := &stream.dictionary.data[0]
	encoder.CommitStream(stream)
	if !bytes.Equal(encoder.dictionary, want) {
		t.Fatalf("committed dictionary = %q, want %q", encoder.dictionary, want)
	}
	if &encoder.dictionary[0] != backing {
		t.Fatal("stream dictionary was copied instead of transferred")
	}
	if stream.Dictionary() != nil {
		t.Fatal("committed stream retained dictionary ownership")
	}
}

func TestRollingDictionaryMatchesLinearReference(t *testing.T) {
	for _, limit := range []int{8, 32, 256} {
		initial := bytes.Repeat([]byte("initial"), 11)
		dictionary := newRollingDictionary(limit, initial)
		want := appendDictionary(nil, initial, limit)
		chunks := [][]byte{
			{},
			[]byte("a"),
			bytes.Repeat([]byte("bc"), limit/2),
			bytes.Repeat([]byte("def"), limit+1),
			[]byte("tail"),
		}
		for _, chunk := range chunks {
			dictionary.Append(chunk)
			want = appendDictionary(want, chunk, limit)
			if got := dictionary.Clone(); !bytes.Equal(got, want) {
				t.Fatalf("limit %d after %d bytes: dictionary = %x, want %x", limit, len(chunk), got, want)
			}
		}
		if got := dictionary.Take(); !bytes.Equal(got, want) {
			t.Fatalf("limit %d transferred dictionary = %x, want %x", limit, got, want)
		}
	}
}

func TestStreamEncoderClosePreservesLatestFlushError(t *testing.T) {
	sentinel := errors.New("emit failed")
	fail := false
	stream, err := NewEncoder(-1, true).NewStream(func([]byte) error {
		if fail {
			return sentinel
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = stream.Write(bytes.Repeat([]byte("first"), 128)); err != nil {
		t.Fatal(err)
	}
	fail = true
	if _, err = stream.Write(bytes.Repeat([]byte("second"), 128)); !errors.Is(err, sentinel) {
		t.Fatalf("second Write() error = %v, want %v", err, sentinel)
	}
	if err = stream.Close(); !errors.Is(err, sentinel) {
		t.Fatalf("Close() error = %v, want %v", err, sentinel)
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
	encoder.SetDictionary([]byte("ignored"))
	encoder.Commit([]byte("ignored"))
	if err := encoder.Close(); err != nil {
		t.Fatal(err)
	}
	var nilEncoder *Encoder
	nilEncoder.SetDictionary(nil)
	nilEncoder.Commit(nil)
	if _, err := NewEncoder(-1, true).NewStream(nil); err == nil {
		t.Fatal("nil stream callback was accepted")
	}

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

func TestStreamLifecycleAndTruncationErrors(t *testing.T) {
	var nilStream *StreamEncoder
	if _, err := nilStream.Write(nil); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("nil stream Write() = %v", err)
	}
	if err := nilStream.Close(); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("nil stream Close() = %v", err)
	}
	if nilStream.Dictionary() != nil {
		t.Fatal("nil stream returned a dictionary")
	}

	stream, err := NewEncoder(-1, true).NewStream(func([]byte) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if err = stream.Close(); err != nil {
		t.Fatal(err)
	}
	if err = stream.Close(); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("second stream Close() = %v", err)
	}
	if _, err = stream.Write(nil); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("closed stream Write() = %v", err)
	}

	badTail := &streamTruncWriter{pending: []byte("bad")}
	if err = badTail.finish(); err == nil {
		t.Fatal("invalid sync-flush tail was accepted")
	}
	finished := &streamTruncWriter{finished: true}
	if n, err := finished.Write([]byte("discarded")); err != nil || n != len("discarded") {
		t.Fatalf("finished trunc writer = %d, %v", n, err)
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

func FuzzStreamEncoderNeverPanics(f *testing.F) {
	f.Add([]byte("stream payload"))
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, payload []byte) {
		var encoded []byte
		stream, err := NewEncoderWithWindow(-1, false, 8).NewStream(func(chunk []byte) error {
			encoded = append(encoded, chunk...)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if recovered := recover(); recovered != nil {
				t.Fatalf("stream encoder panicked: %v", recovered)
			}
		}()
		_, _ = stream.Write(payload)
		_ = stream.Close()
		if len(encoded) > 0 {
			_, _ = NewDecoderWithWindow(true, 8).Decode(encoded, 1<<20)
		}
	})
}
