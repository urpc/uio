package bytebuf

import (
	"bytes"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"reflect"
	"testing"
)

func newTestCompositeBuffer(buffers []*Buffer) *CompositeBuffer {
	buffer := &CompositeBuffer{bufList: buffers}
	for _, item := range buffers {
		buffer.length += item.Len()
	}
	return buffer
}

func assertCompositeBufferLength(t *testing.T, buffer *CompositeBuffer) {
	t.Helper()
	actual := 0
	for _, item := range buffer.bufList {
		actual += item.Len()
	}
	if buffer.Len() != actual {
		t.Fatalf("cached length = %d, actual = %d", buffer.Len(), actual)
	}
	if buffer.Empty() != (actual == 0) {
		t.Fatalf("Empty = %t with actual length %d", buffer.Empty(), actual)
	}
}

type partialCompositeWriter struct{ limit int }

func (writer partialCompositeWriter) Write(data []byte) (int, error) {
	return min(writer.limit, len(data)), io.ErrClosedPipe
}

func TestCompositeBufferLengthInvariant(t *testing.T) {
	var buffer CompositeBuffer
	assertCompositeBufferLength(t, &buffer)
	_, _ = buffer.Write([]byte("abc"))
	assertCompositeBufferLength(t, &buffer)
	_, _ = buffer.WriteString("def")
	assertCompositeBufferLength(t, &buffer)
	_ = buffer.WriteByte('g')
	assertCompositeBufferLength(t, &buffer)
	_, _ = buffer.Writev([][]byte{[]byte("hi"), []byte("jk")})
	assertCompositeBufferLength(t, &buffer)
	buffer.AppendOwned(CloneBuffer([]byte("owned")))
	assertCompositeBufferLength(t, &buffer)
	_, _ = buffer.Read(make([]byte, 4))
	assertCompositeBufferLength(t, &buffer)
	buffer.Discard(3)
	assertCompositeBufferLength(t, &buffer)
	_, _ = buffer.ReadFrom(bytes.NewBufferString("read-from"))
	assertCompositeBufferLength(t, &buffer)
	_, _ = buffer.WriteTo(io.Discard)
	assertCompositeBufferLength(t, &buffer)

	_, _ = buffer.WriteString("partial")
	written, err := buffer.WriteTo(partialCompositeWriter{limit: 3})
	if written != 3 || !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("partial WriteTo = %d, %v", written, err)
	}
	assertCompositeBufferLength(t, &buffer)
	buffer.Reset()
	assertCompositeBufferLength(t, &buffer)
	_, _ = buffer.WriteString("close")
	if err = buffer.Close(); err != nil {
		t.Fatal(err)
	}
	assertCompositeBufferLength(t, &buffer)
}

func BenchmarkCompositeBufferDrainSegments(b *testing.B) {
	for _, segments := range []int{1024, 2048, 4096, 8192} {
		b.Run(fmt.Sprintf("segments_%d", segments), func(b *testing.B) {
			b.ReportAllocs()
			b.ReportMetric(float64(segments), "segments/op")
			for range b.N {
				b.StopTimer()
				var buffer CompositeBuffer
				for range segments {
					buffer.AppendOwned(NewBuffer([]byte{'x'}))
				}
				b.StartTimer()
				for !buffer.Empty() {
					buffer.Discard(8)
				}
				b.StopTimer()
			}
		})
	}
}

func TestCompositeBuffer_Available(t *testing.T) {
	tests := []struct {
		name      string
		bufList   []*Buffer
		wantBytes int
	}{
		{
			name: "test1",
			bufList: []*Buffer{
				NewBuffer([]byte{}),
			},
			wantBytes: 0,
		},
		{
			name: "test2",
			bufList: []*Buffer{
				NewBuffer(make([]byte, 0, 5)),
			},
			wantBytes: 5,
		},
		{
			name: "test3",
			bufList: []*Buffer{
				NewBuffer(make([]byte, 0, 5)),
				NewBuffer(make([]byte, 0, 4)),
			},
			wantBytes: 9,
		},
		{
			name: "test4",
			bufList: []*Buffer{
				NewBuffer(make([]byte, 0, 5)),
				NewBuffer(make([]byte, 0, 5)),
				NewBuffer(make([]byte, 0, 5)),
			},
			wantBytes: 15,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := newTestCompositeBuffer(tt.bufList)
			if gotBytes := b.Available(); gotBytes != tt.wantBytes {
				t.Errorf("Available() = %v, want %v", gotBytes, tt.wantBytes)
			}
		})
	}
}

func TestCompositeBuffer_Cap(t *testing.T) {
	tests := []struct {
		name         string
		bufList      []*Buffer
		wantCapacity int
	}{
		{
			name: "test1",
			bufList: []*Buffer{
				NewBuffer([]byte{}),
			},
			wantCapacity: 0,
		},
		{
			name: "test2",
			bufList: []*Buffer{
				NewBuffer(make([]byte, 0, 5)),
			},
			wantCapacity: 5,
		},
		{
			name: "test3",
			bufList: []*Buffer{
				NewBuffer(make([]byte, 0, 5)),
				NewBuffer(make([]byte, 0, 4)),
			},
			wantCapacity: 9,
		},
		{
			name: "test4",
			bufList: []*Buffer{
				NewBuffer(make([]byte, 0, 5)),
				NewBuffer(make([]byte, 0, 5)),
				NewBuffer(make([]byte, 0, 5)),
			},
			wantCapacity: 15,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := newTestCompositeBuffer(tt.bufList)
			if gotCapacity := b.Cap(); gotCapacity != tt.wantCapacity {
				t.Errorf("Cap() = %v, want %v", gotCapacity, tt.wantCapacity)
			}
		})
	}
}

func TestCompositeBuffer_Len(t *testing.T) {
	tests := []struct {
		name       string
		bufList    []*Buffer
		wantLength int
	}{
		{
			name: "test1",
			bufList: []*Buffer{
				NewBuffer([]byte{}),
			},
			wantLength: 0,
		},
		{
			name: "test2",
			bufList: []*Buffer{
				NewBuffer(make([]byte, 2, 5)),
			},
			wantLength: 2,
		},
		{
			name: "test3",
			bufList: []*Buffer{
				NewBuffer(make([]byte, 0, 5)),
				NewBuffer(make([]byte, 3, 4)),
			},
			wantLength: 3,
		},
		{
			name: "test4",
			bufList: []*Buffer{
				NewBuffer(make([]byte, 1, 5)),
				NewBuffer(make([]byte, 2, 5)),
				NewBuffer(make([]byte, 3, 5)),
			},
			wantLength: 6,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := newTestCompositeBuffer(tt.bufList)
			if gotLength := b.Len(); gotLength != tt.wantLength {
				t.Errorf("Len() = %v, want %v", gotLength, tt.wantLength)
			}
		})
	}
}

func TestCompositeBuffer_Read(t *testing.T) {
	type args struct {
		p []byte
	}
	tests := []struct {
		name    string
		bufList []*Buffer
		args    args
		wantN   int
		wantErr bool
	}{
		{
			name:    "test0",
			bufList: []*Buffer{},
			args:    args{p: make([]byte, 1)},
			wantN:   0,
			wantErr: true,
		},
		{
			name:    "test1",
			bufList: []*Buffer{NewBuffer(make([]byte, 1))},
			args:    args{p: make([]byte, 1)},
			wantN:   1,
			wantErr: false,
		},
		{
			name:    "test2",
			bufList: []*Buffer{NewBuffer(make([]byte, 1)), NewBuffer(make([]byte, 10))},
			args:    args{p: make([]byte, 5)},
			wantN:   5,
			wantErr: false,
		},
		{
			name:    "test2",
			bufList: []*Buffer{NewBuffer(make([]byte, 1)), NewBuffer(make([]byte, 10))},
			args:    args{p: make([]byte, 25)},
			wantN:   11,
			wantErr: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := newTestCompositeBuffer(tt.bufList)
			gotN, err := b.Read(tt.args.p)
			if (err != nil) != tt.wantErr {
				t.Errorf("Read() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if gotN != tt.wantN {
				t.Errorf("Read() gotN = %v, want %v", gotN, tt.wantN)
			}
		})
	}
}

func TestCompositeBuffer_ReadFrom(t *testing.T) {
	type args struct {
		r io.Reader
	}
	tests := []struct {
		name    string
		bufList []*Buffer
		args    args
		wantN   int64
		wantErr bool
	}{
		{
			name:    "test0",
			bufList: []*Buffer{},
			args:    args{bytes.NewReader(make([]byte, 0))},
			wantN:   0,
			wantErr: false,
		},
		{
			name:    "test1",
			bufList: []*Buffer{},
			args:    args{bytes.NewReader(make([]byte, 1))},
			wantN:   1,
			wantErr: false,
		},
		{
			name:    "test2",
			bufList: []*Buffer{NewBuffer(make([]byte, 1, 5))},
			args:    args{bytes.NewReader(make([]byte, 10))},
			wantN:   10,
			wantErr: false,
		},
		{
			name:    "test3",
			bufList: []*Buffer{NewBuffer(make([]byte, 1, 5))},
			args:    args{bytes.NewReader(make([]byte, 3))},
			wantN:   3,
			wantErr: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := newTestCompositeBuffer(tt.bufList)
			gotN, err := b.ReadFrom(tt.args.r)
			if (err != nil) != tt.wantErr {
				t.Errorf("ReadFrom() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if gotN != tt.wantN {
				t.Errorf("ReadFrom() gotN = %v, want %v", gotN, tt.wantN)
			}
		})
	}
}

func TestCompositeBufferReadFromAfterPartialRead(t *testing.T) {
	var buffer CompositeBuffer
	_, _ = buffer.WriteString("abcdef")
	read := make([]byte, 2)
	if n, err := buffer.Read(read); err != nil || n != len(read) {
		t.Fatalf("Read = %d, %v", n, err)
	}
	if n, err := buffer.ReadFrom(bytes.NewBufferString("XYZ")); err != nil || n != 3 {
		t.Fatalf("ReadFrom = %d, %v", n, err)
	}
	if buffer.Len() != len("cdefXYZ") {
		t.Fatalf("Len = %d, want %d", buffer.Len(), len("cdefXYZ"))
	}
	result := make([]byte, buffer.Len())
	if n, err := buffer.Read(result); err != nil || n != len(result) {
		t.Fatalf("final Read = %d, %v", n, err)
	}
	if got := string(result); got != "cdefXYZ" {
		t.Fatalf("final content = %q, want cdefXYZ", got)
	}
}

func TestCompositeBuffer_Reset(t *testing.T) {
	tests := []struct {
		name    string
		bufList []*Buffer
	}{
		{
			name:    "test0",
			bufList: []*Buffer{},
		},
		{
			name:    "test1",
			bufList: []*Buffer{NewBuffer(make([]byte, 5))},
		},
		{
			name:    "test2",
			bufList: []*Buffer{NewBuffer(make([]byte, 5)), NewBuffer(make([]byte, 5))},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := newTestCompositeBuffer(tt.bufList)
			b.Reset()
			if n := b.Cap(); n != 0 {
				t.Errorf("Reset() gotN = %v, want %v", n, 0)
			}
		})
	}
}

func TestCompositeBuffer_Write(t *testing.T) {
	type args struct {
		p []byte
	}
	tests := []struct {
		name    string
		bufList []*Buffer
		args    args
		wantN   int
		wantErr bool
	}{
		{
			name:    "test0",
			bufList: []*Buffer{},
			args:    args{p: make([]byte, 0)},
			wantN:   0,
			wantErr: false,
		},
		{
			name:    "test1",
			bufList: []*Buffer{},
			args:    args{p: make([]byte, 1)},
			wantN:   1,
			wantErr: false,
		},
		{
			name:    "test2",
			bufList: []*Buffer{},
			args:    args{p: make([]byte, 10)},
			wantN:   10,
			wantErr: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := newTestCompositeBuffer(tt.bufList)
			gotN, err := b.Write(tt.args.p)
			if (err != nil) != tt.wantErr {
				t.Errorf("Write() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if gotN != tt.wantN {
				t.Errorf("Write() gotN = %v, want %v", gotN, tt.wantN)
			}
		})
	}
}

func TestCompositeBuffer_WriteTo(t *testing.T) {
	tests := []struct {
		name    string
		bufList []*Buffer
		wantW   string
		wantN   int64
		wantErr bool
	}{
		{
			name:    "test0",
			bufList: []*Buffer{},
			wantW:   "",
			wantN:   0,
			wantErr: false,
		},
		{
			name:    "test0",
			bufList: []*Buffer{NewBufferString("hello world")},
			wantW:   "hello world",
			wantN:   11,
			wantErr: false,
		},
		{
			name:    "test0",
			bufList: []*Buffer{NewBufferString("hello"), NewBufferString(" world")},
			wantW:   "hello world",
			wantN:   11,
			wantErr: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := newTestCompositeBuffer(tt.bufList)
			w := &Buffer{}
			gotN, err := b.WriteTo(w)
			if (err != nil) != tt.wantErr {
				t.Errorf("WriteTo() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if gotW := w.String(); gotW != tt.wantW {
				t.Errorf("WriteTo() gotW = %v, want %v", gotW, tt.wantW)
			}
			if gotN != tt.wantN {
				t.Errorf("WriteTo() gotN = %v, want %v", gotN, tt.wantN)
			}
		})
	}
}

func TestNewCompositeBuffer(t *testing.T) {
	tests := []struct {
		name string
		want *CompositeBuffer
	}{
		{
			name: "test0",
			want: NewCompositeBuffer(),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := NewCompositeBuffer(); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("NewCompositeBuffer() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCompositeBuffer_Discard(t *testing.T) {
	type args struct {
		n int
	}
	tests := []struct {
		name    string
		bufList []*Buffer
		args    args
		want    int
		wantErr bool
	}{
		{
			name:    "test0",
			bufList: []*Buffer{},
			args:    args{n: 5},
			want:    0,
			wantErr: false,
		},
		{
			name:    "test1",
			bufList: []*Buffer{NewBufferString("12345")},
			args:    args{n: 5},
			want:    5,
			wantErr: false,
		},
		{
			name:    "test2",
			bufList: []*Buffer{NewBufferString("12345")},
			args:    args{n: 0},
			want:    5,
			wantErr: false,
		},
		{
			name:    "test3",
			bufList: []*Buffer{NewBufferString("12345"), NewBufferString("12345")},
			args:    args{n: 8},
			want:    8,
			wantErr: false,
		},
		{
			name:    "test4",
			bufList: []*Buffer{NewBufferString("12345"), NewBufferString("12345")},
			args:    args{n: 45},
			want:    10,
		},
		{
			name:    "test5",
			bufList: []*Buffer{NewBufferString("12345"), NewBufferString("12345")},
			args:    args{n: -1},
			want:    10,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := newTestCompositeBuffer(tt.bufList)
			got := b.Discard(tt.args.n)

			if got != tt.want {
				t.Errorf("Discard() got = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCompositeBuffer_Peek(t *testing.T) {
	type args struct {
		p []byte
	}
	tests := []struct {
		name    string
		bufList []*Buffer
		args    args
		want    []byte
	}{
		{
			name:    "test0",
			bufList: []*Buffer{},
			args:    args{p: make([]byte, 0)},
			want:    nil,
		},
		{
			name:    "test1",
			bufList: []*Buffer{NewBufferString("12345")},
			args:    args{p: make([]byte, 0)},
			want:    nil,
		},
		{
			name:    "test2",
			bufList: []*Buffer{NewBufferString("12345")},
			args:    args{p: make([]byte, 3)},
			want:    []byte("123"),
		},
		{
			name:    "test3",
			bufList: []*Buffer{NewBufferString("12345"), NewBufferString("12345")},
			args:    args{p: make([]byte, 8)},
			want:    []byte("12345123"),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := newTestCompositeBuffer(tt.bufList)
			if got := b.Peek(tt.args.p); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Peek() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCompositeBufferPeekChunk(t *testing.T) {
	var buffer CompositeBuffer
	if got := buffer.PeekChunk(); got != nil {
		t.Fatalf("empty PeekChunk = %q", got)
	}
	buffer.AppendOwned(NewBufferString("first"))
	buffer.AppendOwned(NewBufferString("second"))
	if got := string(buffer.PeekChunk()); got != "first" {
		t.Fatalf("PeekChunk = %q, want first", got)
	}
	buffer.Discard(len("first"))
	if got := string(buffer.PeekChunk()); got != "second" {
		t.Fatalf("PeekChunk after Discard = %q, want second", got)
	}
}

func BenchmarkBuffer(b *testing.B) {

	var data [256]byte
	rand.Read(data[:])

	b.Run("Buffer.Write", func(b *testing.B) {
		buffer := NewBuffer(nil)

		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			buffer.Write(data[:])
		}
	})

	b.Run("CompositeBuffer.Write", func(b *testing.B) {
		buffer := NewCompositeBuffer()

		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			buffer.Write(data[:])
		}
	})

	b.Run("Buffer.ReadWrite", func(b *testing.B) {
		buffer := NewBuffer(nil)
		readBuffer := make([]byte, 150)

		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			buffer.Write(data[:])
			buffer.Read(readBuffer)
		}
	})

	b.Run("CompositeBuffer.ReadWrite", func(b *testing.B) {
		buffer := NewCompositeBuffer()
		readBuffer := make([]byte, 150)

		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			buffer.Write(data[:])
			buffer.Read(readBuffer)
		}
	})
}

func TestCompositeBuffer_PeekVec(t *testing.T) {

	tests := []struct {
		name       string
		bufList    []*Buffer
		wantVec    [][]byte
		wantLength int
	}{
		{
			name:       "test0",
			bufList:    []*Buffer{},
			wantVec:    nil,
			wantLength: 0,
		},
		{
			name:       "test1",
			bufList:    []*Buffer{NewBufferString("hello")},
			wantVec:    [][]byte{[]byte("hello")},
			wantLength: 5,
		},
		{
			name:       "test2",
			bufList:    []*Buffer{NewBufferString("hello"), NewBufferString(" "), NewBufferString("world!")},
			wantVec:    [][]byte{[]byte("hello"), []byte(" "), []byte("world!")},
			wantLength: 12,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := newTestCompositeBuffer(tt.bufList)
			gotVec, gotLength := b.PeekVec(nil)
			if !reflect.DeepEqual(gotVec, tt.wantVec) {
				t.Errorf("PeekVec() gotVec = %v, want %v", gotVec, tt.wantVec)
			}
			if gotLength != tt.wantLength {
				t.Errorf("PeekVec() gotLength = %v, want %v", gotLength, tt.wantLength)
			}
		})
	}
}

func TestCompositeBufferAppendOwnedAndPeekVecN(t *testing.T) {
	var buffer CompositeBuffer
	first := CloneBuffer([]byte("first"))
	second := CloneBuffer([]byte("second"))
	buffer.AppendOwned(first)
	buffer.AppendOwned(second)

	storage := make([][]byte, 0, 1)
	vec, length := buffer.PeekVecN(storage, 1)
	if len(vec) != 1 || string(vec[0]) != "first" || length != len("first") {
		t.Fatalf("PeekVecN = %q, %d", vec, length)
	}
	if got := buffer.Len(); got != len("firstsecond") {
		t.Fatalf("Len = %d", got)
	}
	var stackStorage [2][]byte
	allocations := testing.AllocsPerRun(1000, func() {
		got, gotLength := buffer.PeekVecN(stackStorage[:0], len(stackStorage))
		if len(got) != 2 || gotLength != len("firstsecond") {
			panic("unexpected PeekVecN result")
		}
	})
	if allocations != 0 {
		t.Fatalf("PeekVecN allocations = %v", allocations)
	}
	buffer.Reset()
}

func TestCloneBuffersFrom(t *testing.T) {
	buffer := CloneBuffersFrom([][]byte{[]byte("ab"), []byte("cde"), []byte("f")}, 3, 3)
	defer ReleaseBuffer(buffer)
	if got := string(buffer.Bytes()); got != "def" {
		t.Fatalf("CloneBuffersFrom = %q", got)
	}
}

func TestCompositeBuffer_WriteString(t *testing.T) {
	type args struct {
		s string
	}
	tests := []struct {
		name    string
		bufList []*Buffer
		args    args
		wantN   int
		wantErr bool
	}{
		{
			name:    "test0",
			bufList: []*Buffer{},
			args:    args{s: "hello"},
			wantN:   5,
			wantErr: false,
		},
		{
			name:    "test1",
			bufList: []*Buffer{NewBufferString("hello")},
			args:    args{s: " world!"},
			wantN:   7,
			wantErr: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := newTestCompositeBuffer(tt.bufList)
			gotN, err := b.WriteString(tt.args.s)
			if (err != nil) != tt.wantErr {
				t.Errorf("WriteString() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if gotN != tt.wantN {
				t.Errorf("WriteString() gotN = %v, want %v", gotN, tt.wantN)
			}
		})
	}
}

func TestCompositeBuffer_WriteByte(t *testing.T) {
	type fields struct {
		bufList []*Buffer
	}
	type args struct {
		c byte
	}
	tests := []struct {
		name    string
		bufList []*Buffer
		args    args
		wantErr bool
	}{
		{
			name:    "test0",
			bufList: []*Buffer{},
			args:    args{c: 'h'},
			wantErr: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := newTestCompositeBuffer(tt.bufList)
			if err := b.WriteByte(tt.args.c); (err != nil) != tt.wantErr {
				t.Errorf("WriteByte() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestCompositeBuffer_Writev(t *testing.T) {
	type args struct {
		vec [][]byte
	}
	tests := []struct {
		name    string
		bufList []*Buffer
		args    args
		wantN   int
		wantErr bool
	}{
		{
			name:    "test0",
			bufList: []*Buffer{},
			args:    args{vec: [][]byte{}},
			wantN:   0,
			wantErr: false,
		},
		{
			name:    "test1",
			bufList: []*Buffer{},
			args:    args{vec: [][]byte{[]byte("hello")}},
			wantN:   5,
			wantErr: false,
		},
		{
			name:    "test2",
			bufList: []*Buffer{},
			args:    args{vec: [][]byte{[]byte("hello"), []byte(" "), []byte("world!")}},
			wantN:   12,
			wantErr: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := newTestCompositeBuffer(tt.bufList)
			gotN, err := b.Writev(tt.args.vec)
			if (err != nil) != tt.wantErr {
				t.Errorf("Writev() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if gotN != tt.wantN {
				t.Errorf("Writev() gotN = %v, want %v", gotN, tt.wantN)
			}
		})
	}
}

func TestCompositeBuffer_Close(t *testing.T) {
	tests := []struct {
		name    string
		bufList []*Buffer
		wantErr bool
	}{
		{
			name:    "test0",
			bufList: []*Buffer{},
			wantErr: false,
		},
		{
			name:    "test1",
			bufList: []*Buffer{NewBufferString("1234")},
			wantErr: false,
		},
		{
			name:    "test2",
			bufList: []*Buffer{NewBufferString("1234"), NewBufferString("567"), NewBufferString("890")},
			wantErr: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := newTestCompositeBuffer(tt.bufList)
			if err := b.Close(); (err != nil) != tt.wantErr {
				t.Errorf("Close() error = %v, wantErr %v", err, tt.wantErr)
			}
			if !b.Empty() || b.Len() != 0 {
				t.Errorf("Close() got = %v, want %v", b.Len(), 0)
			}
		})
	}
}
