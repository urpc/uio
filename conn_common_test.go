package uio

import (
	"bytes"
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestUnflushedErrorDescriptionAndCause(t *testing.T) {
	err := UnflushedError{Remaining: 7}
	if got, want := err.Error(), "uio: connection closed with unflushed data: 7 bytes"; got != want {
		t.Fatalf("Error() = %q, want %q", got, want)
	}
	if !errors.Is(err, ErrUnflushedData) {
		t.Fatal("UnflushedError does not unwrap to ErrUnflushedData")
	}
}

func TestCommonConnMetadataAndUnsupportedDeadlines(t *testing.T) {
	local := &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1}
	remote := &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 2}
	conn := &commonConn{localAddr: local, remoteAddr: remote}
	conn.SetUserdata("userdata")
	if conn.LocalAddr() != local || conn.RemoteAddr() != remote || conn.Userdata() != "userdata" {
		t.Fatal("connection metadata was not preserved")
	}
	for name, set := range map[string]func() error{
		"deadline":       func() error { return conn.SetDeadline(time.Now()) },
		"read deadline":  func() error { return conn.SetReadDeadline(time.Now()) },
		"write deadline": func() error { return conn.SetWriteDeadline(time.Now()) },
	} {
		t.Run(name, func(t *testing.T) {
			if err := set(); !errors.Is(err, errUnsupported) {
				t.Fatalf("error = %v, want %v", err, errUnsupported)
			}
		})
	}
}

func TestCommonConnReadAndWriteToAcrossBuffers(t *testing.T) {
	conn := &commonConn{inboundTail: []byte("cd")}
	_, _ = conn.inbound.WriteString("ab")
	var dst bytes.Buffer
	n, err := conn.WriteTo(&dst)
	if err != nil || n != 4 || dst.String() != "abcd" {
		t.Fatalf("WriteTo = %d, %v, %q", n, err, dst.String())
	}
	if conn.InboundBuffered() != 0 {
		t.Fatalf("buffered after WriteTo = %d", conn.InboundBuffered())
	}

	conn.inboundTail = []byte("cd")
	_, _ = conn.inbound.WriteString("ab")
	if n, err = conn.WriteTo(failingWriter{}); n != 0 || !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("failing WriteTo = %d, %v", n, err)
	}
	if got := string(conn.inboundTail); got != "cd" {
		t.Fatalf("tail after failed WriteTo = %q", got)
	}

	conn = &commonConn{inboundTail: []byte("cd")}
	_, _ = conn.inbound.WriteString("ab")
	buffer := make([]byte, 3)
	if n, err := conn.Read(buffer); err != nil || n != 3 || string(buffer) != "abc" {
		t.Fatalf("Read = %d, %v, %q", n, err, buffer)
	}
	if n, err := conn.Read(buffer[:1]); err != nil || n != 1 || buffer[0] != 'd' {
		t.Fatalf("tail Read = %d, %v, %q", n, err, buffer[:1])
	}

	conn = &commonConn{}
	_, _ = conn.inbound.WriteString("ab")
	if n, err := conn.Read(buffer[:1]); err != nil || n != 1 || buffer[0] != 'a' {
		t.Fatalf("buffer-only Read = %d, %v, %q", n, err, buffer[:1])
	}
}

func TestCommonConnPeekVariants(t *testing.T) {
	conn := &commonConn{inboundTail: []byte("tail")}
	if got := conn.Peek(nil); got != nil {
		t.Fatalf("Peek(nil) = %q", got)
	}
	if got := (&commonConn{}).Peek(make([]byte, 1)); got != nil {
		t.Fatalf("empty Peek = %q", got)
	}
	if got := string(conn.Peek(make([]byte, 2))); got != "ta" {
		t.Fatalf("tail Peek = %q", got)
	}
	if got := string(conn.PeekChunk()); got != "tail" {
		t.Fatalf("tail PeekChunk = %q", got)
	}

	conn = &commonConn{inboundTail: []byte("cd")}
	_, _ = conn.inbound.WriteString("ab")
	buffer := make([]byte, 4)
	if got := string(conn.Peek(buffer)); got != "abcd" {
		t.Fatalf("combined Peek = %q", got)
	}
	if got := string(conn.Peek(buffer[:1])); got != "a" {
		t.Fatalf("buffer-only Peek = %q", got)
	}
	if got := string(conn.PeekChunk()); got != "ab" {
		t.Fatalf("buffer-first PeekChunk = %q", got)
	}
	if got := (&commonConn{}).PeekChunk(); got != nil {
		t.Fatalf("empty PeekChunk = %q", got)
	}
}

func TestCommonConnDiscardVariants(t *testing.T) {
	tests := []struct {
		name      string
		buffered  string
		tail      string
		discard   int
		wantN     int
		wantBytes string
	}{
		{name: "zero", tail: "ab", discard: 0, wantBytes: "ab"},
		{name: "tail only", tail: "abc", discard: 1, wantN: 1, wantBytes: "bc"},
		{name: "within buffer", buffered: "abc", tail: "de", discard: 2, wantN: 2, wantBytes: "cde"},
		{name: "across buffers", buffered: "abc", tail: "de", discard: 4, wantN: 4, wantBytes: "e"},
		{name: "all", buffered: "abc", tail: "de", discard: -1, wantN: 5},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			conn := &commonConn{inboundTail: []byte(test.tail)}
			_, _ = conn.inbound.WriteString(test.buffered)
			n, err := conn.Discard(test.discard)
			if err != nil || n != test.wantN {
				t.Fatalf("Discard = %d, %v, want %d, nil", n, err, test.wantN)
			}
			var remaining bytes.Buffer
			_, _ = conn.WriteTo(&remaining)
			if remaining.String() != test.wantBytes {
				t.Fatalf("remaining = %q, want %q", remaining.String(), test.wantBytes)
			}
		})
	}
}
