package bytebuf

import (
	"bytes"
	"crypto/rand"
	"io"
	"reflect"
	"testing"
)

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
			b := &CompositeBuffer{
				bufList: tt.bufList,
			}
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
			b := &CompositeBuffer{
				bufList: tt.bufList,
			}
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
			b := &CompositeBuffer{
				bufList: tt.bufList,
			}
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
			b := &CompositeBuffer{
				bufList: tt.bufList,
			}
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
			b := &CompositeBuffer{
				bufList: tt.bufList,
			}
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
			b := &CompositeBuffer{
				bufList: tt.bufList,
			}
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
			b := &CompositeBuffer{
				bufList: tt.bufList,
			}
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
			b := &CompositeBuffer{
				bufList: tt.bufList,
			}
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
			b := &CompositeBuffer{
				bufList: tt.bufList,
			}
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
			b := &CompositeBuffer{
				bufList: tt.bufList,
			}
			if got := b.Peek(tt.args.p); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Peek() = %v, want %v", got, tt.want)
			}
		})
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
