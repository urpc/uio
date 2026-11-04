package bytebuf

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

func TestReservationsReuseTailAndBoundBorrowedCapacity(t *testing.T) {
	var b CompositeBuffer
	defer b.Reset()
	if b.Reserve(0, 16) != nil || b.Reserve(-1, 16) != nil {
		t.Fatal("nonpositive reservations changed the queue")
	}
	b.AppendOwned(NewBuffer(append(make([]byte, 0, 4), 'a')))
	room := b.Reserve(2, 16)
	if len(room) != 2 || cap(room) != 2 {
		t.Fatalf("borrowed room = len %d cap %d", len(room), cap(room))
	}
	copy(room, "bc")
	copy(b.Reserve(3, 16), "def")
	if b.Blocks() != 2 || string(compositeBytes(&b)) != "abcdef" || b.bufList[1].Cap() < 16 {
		t.Fatalf("reservation contents=%q blocks=%d", compositeBytes(&b), b.Blocks())
	}
	vec, n := b.PeekVecN(nil, 1)
	if n != 3 || len(vec) != 1 || string(vec[0]) != "abc" {
		t.Fatalf("bounded vector = %q, %d", vec, n)
	}
	vec, n = b.PeekVecN(make([][]byte, 0, 4), 4)
	if n != 6 || len(vec) != 2 {
		t.Fatalf("reused vector = %q, %d", vec, n)
	}
	clone := CloneBuffers(vec, n)
	defer ReleaseBuffer(clone)
	vec[0][0] = 'X'
	if string(clone.Bytes()) != "abcdef" {
		t.Fatal("vector clone retained caller-owned bytes")
	}
	if vec, n := b.PeekVecN(nil, 0); len(vec) != 0 || n != 0 {
		t.Fatal("zero vector limit returned payload")
	}
}

func TestVectorCoalescingPreservesSegmentsAndOwnership(t *testing.T) {
	var b CompositeBuffer
	defer b.Reset()
	if n := b.WritevCoalesced([][]byte{nil, {}}, 8); n != 0 {
		t.Fatal("empty vectors changed the buffer")
	}
	if n := b.WritevCoalesced([][]byte{[]byte("ab"), nil, []byte("cd")}, 8); n != 4 {
		t.Fatal(n)
	}
	if n := b.WritevCoalesced([][]byte{[]byte("ef"), []byte("ghij")}, 8); n != 6 {
		t.Fatal(n)
	}
	if got := string(compositeBytes(&b)); got != "abcdefghij" {
		t.Fatalf("coalesced payload = %q", got)
	}
	b.AppendOwned(nil)
	b.AppendOwned(AcquireBuffer(1))
	b.PrependOwned(nil)
	b.PrependOwned(AcquireBuffer(1))
	b.AppendOwnedCoalesced(nil, 8)
	b.AppendOwnedCoalesced(AcquireBuffer(1), 8)
	b.AppendOwnedCoalesced(CloneBuffer([]byte("kl")), 1)
	if got := string(compositeBytes(&b)); got != "abcdefghijkl" {
		t.Fatalf("ownership operations changed payload = %q", got)
	}
	if n, err := b.WriteString(""); n != 0 || err != nil {
		t.Fatalf("empty string = %d, %v", n, err)
	}
	var empty CompositeBuffer
	defer empty.Reset()
	copy(empty.Reserve(3, 128), "one")
	if empty.Blocks() != 1 || string(compositeBytes(&empty)) != "one" {
		t.Fatal("first reservation did not use an independent block")
	}
}

type contractWriter struct {
	n   int
	err error
}

func (w contractWriter) Write([]byte) (int, error) { return w.n, w.err }

type contractReader struct {
	n   int
	err error
}

func (r contractReader) Read(p []byte) (int, error) {
	if r.n > 0 {
		copy(p, bytes.Repeat([]byte{'x'}, r.n))
	}
	return r.n, r.err
}

func TestBufferRejectsInvalidCountsAndPreservesUnwrittenSuffix(t *testing.T) {
	for name, operation := range map[string]func(){
		"discard beyond input":   func() { NewBufferString("x").Discard(2) },
		"negative truncate":      func() { NewBufferString("x").Truncate(-1) },
		"long truncate":          func() { NewBufferString("x").Truncate(2) },
		"negative grow":          func() { NewBuffer(nil).Grow(-1) },
		"negative reader":        func() { _, _ = NewBuffer(nil).ReadFrom(contractReader{n: -1}) },
		"oversized writer count": func() { _, _ = NewBufferString("x").WriteTo(contractWriter{n: 2}) },
	} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("invalid contract did not panic")
				}
			}()
			operation()
		})
	}
	b := NewBufferString("abcd")
	failure := errors.New("writer failed")
	if n, err := b.WriteTo(contractWriter{n: 2, err: failure}); n != 2 || !errors.Is(err, failure) || b.String() != "cd" {
		t.Fatalf("partial write = %d, %v, suffix=%q", n, err, b.String())
	}
	if n, err := b.WriteTo(contractWriter{}); n != 0 || !errors.Is(err, io.ErrShortWrite) || b.String() != "cd" {
		t.Fatalf("short write = %d, %v, suffix=%q", n, err, b.String())
	}
	b.Discard(2)
	if b.Len() != 0 || b.off != 0 {
		t.Fatal("drained buffer did not recover head space")
	}
}

func TestCompositeReaderErrorRetainsAlreadyReadBytes(t *testing.T) {
	failure := errors.New("reader failed")
	var b CompositeBuffer
	defer b.Reset()
	b.AppendOwned(NewBuffer(append(make([]byte, 0, 8), 'a')))
	if n, err := b.ReadFrom(contractReader{n: 2, err: failure}); n != 2 || !errors.Is(err, failure) || string(compositeBytes(&b)) != "axx" {
		t.Fatalf("read failure = %d, %v, contents=%q", n, err, compositeBytes(&b))
	}
	var empty CompositeBuffer
	defer empty.Reset()
	if n, err := empty.ReadFrom(contractReader{err: failure}); n != 0 || !errors.Is(err, failure) || !empty.Empty() {
		t.Fatalf("empty read failure = %d, %v", n, err)
	}
}
