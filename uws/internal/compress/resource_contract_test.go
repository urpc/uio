package compress

import (
	"bytes"
	"compress/flate"
	"errors"
	"io"
	"sync"
	"testing"
)

type failingCompressionWriter struct{ err error }

func (w failingCompressionWriter) Write([]byte) (int, error) { return 0, w.err }

func TestEncoderPropagatesWriterAndFinalizationErrors(t *testing.T) {
	payload := bytes.Repeat([]byte("must survive codec failures"), 80)
	closed, err := flate.NewWriter(io.Discard, flate.BestSpeed)
	if err != nil {
		t.Fatal(err)
	}
	if err := closed.Close(); err != nil {
		t.Fatal(err)
	}
	called := false
	if err := encodeWithFlateWriter(closed, new(bytes.Buffer), payload, func([]byte) error { called = true; return nil }); err == nil || called {
		t.Fatalf("closed writer error=%v callback=%v", err, called)
	}
	cause := errors.New("output storage unavailable")
	w, err := flate.NewWriter(failingCompressionWriter{cause}, flate.BestCompression)
	if err != nil {
		t.Fatal(err)
	}
	if err := encodeWithFlateWriter(w, new(bytes.Buffer), payload, func([]byte) error { called = true; return nil }); !errors.Is(err, cause) || called {
		t.Fatalf("final output error=%v callback=%v", err, called)
	}
}

func TestCodecPoolsDiscardOversizedOutputStorage(t *testing.T) {
	releaseFlateWriter(nil)
	releaseFlateReader(nil)
	writer, err := acquireFlateWriter(flate.BestSpeed)
	if err != nil {
		t.Fatal(err)
	}
	writer.output.Grow(maxPooledCompressionOutput + 1)
	releaseFlateWriter(writer)
	if writer.output.Cap() != 0 {
		t.Fatal("writer retained oversized storage")
	}
	dict, err := acquireDictWriter(flate.DefaultCompression, DefaultWindowBits)
	if err != nil {
		t.Fatal(err)
	}
	dict.output.Grow(maxPooledCompressionOutput + 1)
	releaseDictWriter(dict)
	if dict.output.Cap() != 0 {
		t.Fatal("dictionary writer retained oversized storage")
	}
	reader, err := acquireFlateReader(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	reader.output = make([]byte, 0, maxPooledCompressionOutput+1)
	releaseFlateReader(reader)
	if reader.output != nil {
		t.Fatal("reader retained oversized storage")
	}
}

func TestCompressionConfigurationAndCallbackFailureKeepContext(t *testing.T) {
	for _, noContext := range []bool{false, true} {
		if _, err := NewEncoder(100, noContext).Encode([]byte("payload")); err == nil {
			t.Fatalf("invalid level with noContext=%v accepted", noContext)
		}
	}
	encoder := NewEncoder(flate.DefaultCompression, false)
	decoder := NewDecoder(false)
	first := bytes.Repeat([]byte("shared-dictionary-"), 100)
	encoded, err := encoder.Encode(first)
	if err != nil {
		t.Fatal(err)
	}
	encoder.Commit(first)
	if got, err := decoder.Decode(encoded, len(first)); err != nil || !bytes.Equal(got, first) {
		t.Fatalf("first message: %v", err)
	}
	cause := errors.New("transport rejected encoded frame")
	if err := encoder.EncodeBorrowed([]byte("unsent message"), func([]byte) error { return cause }); !errors.Is(err, cause) {
		t.Fatal(err)
	}
	second := append([]byte("next:"), first...)
	encoded, err = encoder.Encode(second)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := decoder.Decode(encoded, len(second)); err != nil || !bytes.Equal(got, second) {
		t.Fatalf("context diverged after failed send: %v", err)
	}
}

func TestDecodeBoundsAndSourceExhaustion(t *testing.T) {
	payload := bytes.Repeat([]byte("bounded decoder"), 5000)
	wire, err := Compress(payload, flate.BestSpeed)
	if err != nil {
		t.Fatal(err)
	}
	for _, limit := range []int{0, -1, len(payload)} {
		got, err := Decompress(wire, limit)
		if err != nil || !bytes.Equal(got, payload) {
			t.Fatalf("limit=%d length=%d err=%v", limit, len(got), err)
		}
	}
	if _, err := Decompress(wire, len(payload)-1); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("limit error=%v", err)
	}
	source := new(decodeSource)
	source.Reset([]byte("wire"))
	got, err := io.ReadAll(source)
	if err != nil || string(got) != "wire"+decodeTail {
		t.Fatalf("source=%q err=%v", got, err)
	}
	if n, err := source.Read(make([]byte, 1)); n != 0 || err != io.EOF {
		t.Fatalf("exhausted source n=%d err=%v", n, err)
	}
	for _, limit := range []int{-1, maxDictionaryBytes + 1} {
		got := appendDictionary(nil, payload, limit)
		if !bytes.Equal(got, payload[len(payload)-maxDictionaryBytes:]) {
			t.Fatal("default dictionary window differs")
		}
	}
}

// Reader acquisition must discard an unusable resource before it can deliver
// bytes or advance the message dictionary. A fresh pool factory makes the
// interface failure deterministic without changing the codec's production API.
func TestDecodeDiscardsReaderThatCannotBeReused(t *testing.T) {
	cause := errors.New("reader reset failed")
	for _, resettable := range []bool{false, true} {
		reader := &contractReader{resetErr: cause}
		job := &flateReaderJob{reader: reader}
		if resettable {
			job.reader = &contractResetReader{reader}
		}
		flateReaderPool = sync.Pool{New: func() any { return job }}
		func() {
			defer func() { flateReaderPool = sync.Pool{} }()
			called := false
			decoder := NewDecoder(false)
			decoder.dictionary = []byte("committed context")
			err := decoder.DecodeBorrowed(nil, 100, func([]byte) error { called = true; return nil })
			if err == nil || called || !reader.closed || string(decoder.dictionary) != "committed context" {
				t.Fatalf("resettable=%v err=%v callback=%v closed=%v", resettable, err, called, reader.closed)
			}
			if resettable && !errors.Is(err, cause) {
				t.Fatal(err)
			}
		}()
	}
}

type contractReader struct {
	resetErr error
	closed   bool
}

func (r *contractReader) Read([]byte) (int, error) { return 0, io.EOF }
func (r *contractReader) Close() error             { r.closed = true; return nil }

type contractResetReader struct{ *contractReader }

func (r *contractResetReader) Reset(io.Reader, []byte) error { return r.resetErr }
