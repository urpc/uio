package uws

import (
	"bytes"
	"testing"

	"github.com/urpc/uio/uws/internal/frame"
)

// The fused short-frame paths cover 16-bit lengths as well as seven-bit ones:
// a masked frame whose payload is 126 bytes or more is consumed and echoed
// byte-for-byte as the generic parser would, and a frame past the frame or
// message limits falls back to it and is refused there.
func TestShortPathCoversSixteenBitLengths(t *testing.T) {
	for _, size := range []int{0, 1, 125, 126, 127, 1024, 4096, 65535} {
		server := NewServer(echoPathHandler{})
		server.config = newServerConnConfig(server)
		raw := &echoPathConn{}
		conn := server.newConnection(raw)
		conn.opened.Store(true)
		raw.userdata = conn

		payload := bytes.Repeat([]byte{'q'}, size)
		mask := [4]byte{1, 2, 3, 4}
		raw.inbound = frame.Append(nil, frame.Frame{Fin: true, Opcode: frame.Binary, Masked: true, Payload: payload}, mask)
		raw.out = make([]byte, 0, 2*size+64)
		if err := server.onData(raw); err != nil {
			t.Fatalf("size %d: read round failed: %v", size, err)
		}
		want := frame.Append(nil, frame.Frame{Fin: true, Opcode: frame.Binary, Payload: payload}, [4]byte{})
		if !bytes.Equal(raw.out, want) {
			t.Fatalf("size %d: reply %d bytes, want %d, equal=%v", size, len(raw.out), len(want), bytes.Equal(raw.out, want))
		}
		// One frame for one frame: the round consumed the whole chunk.
		if len(raw.inbound) != 0 {
			t.Fatalf("size %d: %d inbound bytes left", size, len(raw.inbound))
		}
	}
}

// A 16-bit-length frame over the frame limit must not slip through the fused
// path: it falls to the generic parser, which refuses it, and the round fails
// exactly as it did before the fused path learned the width.
func TestShortPathSixteenBitLengthHonorsFrameLimit(t *testing.T) {
	server := NewServer(echoPathHandler{})
	server.config = newServerConnConfig(server)
	server.config.parser.MaxFramePayload = 2048
	raw := &echoPathConn{}
	conn := server.newConnection(raw)
	conn.opened.Store(true)
	raw.userdata = conn

	payload := bytes.Repeat([]byte{'q'}, 4096)
	raw.inbound = frame.Append(nil, frame.Frame{Fin: true, Opcode: frame.Binary, Masked: true, Payload: payload}, [4]byte{1, 2, 3, 4})
	raw.out = make([]byte, 0, 4096)
	if err := server.onData(raw); err != nil {
		return // the error propagated, which is a refusal too
	}
	if !conn.closing.Load() {
		t.Fatal("a frame over MaxFramePayload was accepted")
	}
	// The refusal is one 4-byte close frame, not an echo of the frame.
	if len(raw.out) != 4 {
		t.Fatalf("a refused frame produced %d reply bytes, want one close frame", len(raw.out))
	}
	if len(raw.inbound) == 0 {
		t.Fatal("the refused frame was consumed")
	}
}

// A single-frame round of a 1 KiB message takes the send short path: the reply
// is one unmasked frame, and it stays byte-identical to the generic encoding
// across the seven-bit/sixteen-bit boundary.
func TestSingleFrameRoundRepliesThroughTheShortPath(t *testing.T) {
	for _, size := range []int{125, 126, 1024} {
		server := NewServer(echoPathHandler{})
		server.config = newServerConnConfig(server)
		raw := &echoPathConn{}
		conn := server.newConnection(raw)
		conn.opened.Store(true)
		raw.userdata = conn

		payload := bytes.Repeat([]byte{'m'}, size)
		raw.inbound = frame.Append(nil, frame.Frame{Fin: true, Opcode: frame.Binary, Masked: true, Payload: payload}, [4]byte{9, 8, 7, 6})
		raw.out = make([]byte, 0, 2*size+64)
		if err := server.onData(raw); err != nil {
			t.Fatalf("size %d: %v", size, err)
		}
		want := frame.Append(nil, frame.Frame{Fin: true, Opcode: frame.Binary, Payload: payload}, [4]byte{})
		if !bytes.Equal(raw.out, want) {
			t.Fatalf("size %d: reply differs from the generic encoding", size)
		}
	}
}

// Writev captures the header-and-payload vector the generic frame path hands
// to the transport, which echoPathConn (see echo_path_benchmark_test.go)
// otherwise leaves to the embedded nil Conn.
func (c *echoPathConn) Writev(buffers [][]byte) (int, error) {
	n := 0
	for _, b := range buffers {
		c.out = append(c.out, b...)
		n += len(b)
	}
	return n, nil
}
