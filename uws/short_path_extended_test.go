package uws

import (
	"bytes"
	"io"
	"testing"

	"github.com/urpc/uio"
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
	// The refusal starts a graceful close, which arms the close timer: end
	// the connection as the transport would once it is released.
	t.Cleanup(func() { server.onClose(raw, io.EOF) })

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

// A 16-bit length under 126 is not canonical. The fused read path must leave
// such a frame to the generic parser, which closes the connection with a
// protocol error, rather than deliver it.
func TestShortPathRefusesNonCanonicalSixteenBitLength(t *testing.T) {
	server := NewServer(echoPathHandler{})
	server.config = newServerConnConfig(server)
	raw := &echoPathConn{}
	conn := server.newConnection(raw)
	conn.opened.Store(true)
	raw.userdata = conn
	t.Cleanup(func() { server.onClose(raw, io.EOF) })

	for _, n := range []int{0, 2, 125} {
		mask := [4]byte{1, 2, 3, 4}
		wire := []byte{0x80 | byte(frame.Binary), 0x80 | 126, 0, byte(n), mask[0], mask[1], mask[2], mask[3]}
		wire = append(wire, make([]byte, n)...)
		if size, ok, err := conn.consumeShortFrame(wire, conn.config.shortReadLimit); ok {
			t.Fatalf("length %d in 16 bits: the fused path took %d bytes (err %v)", n, size, err)
		}
	}

	raw.inbound = []byte{0x80 | byte(frame.Binary), 0x80 | 126, 0, 2, 1, 2, 3, 4, 'h' ^ 1, 'i' ^ 2}
	raw.out = make([]byte, 0, 64)
	_ = server.onData(raw)
	if !conn.closing.Load() {
		t.Fatal("a non-canonical 16-bit length was accepted")
	}
	// The reply is the protocol-error close frame, not an echo.
	if want := []byte{0x88, 2, 0x03, 0xea}; !bytes.Equal(raw.out, want) {
		t.Fatalf("reply % x, want the 1002 close frame % x", raw.out, want)
	}
}

// A client masks every frame it sends, so it takes none of the fused paths,
// an empty message included: a zero limit there would let that one through
// unmasked.
func TestClientTakesNoShortPath(t *testing.T) {
	dialer := NewDialer()
	dialer.Events = &uio.Events{WriteBufferedThreshold: 4096}
	dialer.config = newDialerConnConfig(dialer)
	if c := dialer.config; c.shortSendLimit != -1 || c.shortSendDirect != -1 || c.shortReadLimit != -1 {
		t.Fatalf("client short limits send=%d direct=%d read=%d, want -1 each",
			c.shortSendLimit, c.shortSendDirect, c.shortReadLimit)
	}
	for _, size := range []int{0, 1, 125, 126} {
		raw := &echoPathConn{}
		conn := dialer.newClientConn(raw, &dialSetup{})
		conn.opened.Store(true)
		raw.out = make([]byte, 0, size+64)
		if err := conn.SendBinary(make([]byte, size)); err != nil {
			t.Fatalf("size %d: %v", size, err)
		}
		f, n, complete, err := frame.ParseFrame(raw.out, &frame.ParserConfig{ExpectMask: true})
		if err != nil || !complete || n != len(raw.out) || !f.Masked || len(f.Payload) != size {
			t.Fatalf("size %d: wire % x is not one masked frame (err %v)", size, raw.out, err)
		}
	}
}

// The send limits keep a fused frame below the buffering threshold with its
// header counted at its real width, two bytes for a seven-bit length and four
// for a 16-bit one, and switch the paths off where no frame is that small. A
// threshold under 3 once wrapped around to the frame limit, and the 16-bit
// length then truncated.
func TestShortSendLimitsFollowTheThreshold(t *testing.T) {
	for _, tc := range []struct{ threshold, send, direct int }{
		{-1, -1, -1}, {1, -1, -1}, {2, -1, -1}, {3, 0, 0},
		{128, 125, 125}, {130, 125, 125}, {131, 125, 126},
		{4096, 125, 4091}, {1 << 20, 125, 0xFFFF},
	} {
		server := NewServer(echoPathHandler{})
		server.Events = &uio.Events{WriteBufferedThreshold: tc.threshold}
		config := newServerConnConfig(server)
		if config.shortSendLimit != tc.send || config.shortSendDirect != tc.direct {
			t.Errorf("threshold %d: send=%d direct=%d, want %d and %d",
				tc.threshold, config.shortSendLimit, config.shortSendDirect, tc.send, tc.direct)
		}
		for _, limit := range []int{config.shortSendLimit, config.shortSendDirect} {
			if limit >= 0 && frameWireSize(limit, false) >= tc.threshold {
				t.Errorf("threshold %d: a %d-byte payload makes a %d-byte frame", tc.threshold, limit, frameWireSize(limit, false))
			}
		}
	}

	// The frame and message limits cap them too.
	server := NewServer(echoPathHandler{})
	server.Events = &uio.Events{WriteBufferedThreshold: 1 << 20}
	server.MaxFramePayload, server.MaxMessageSize = 2048, 1000
	if config := newServerConnConfig(server); config.shortSendDirect != 1000 || config.shortReadLimit != 125 {
		t.Fatalf("direct=%d read=%d under a 1000-byte message limit, want 1000 and 125", config.shortSendDirect, config.shortReadLimit)
	}

	// A large message under a tiny threshold leaves one intact frame.
	for _, threshold := range []int{1, 2} {
		server := NewServer(echoPathHandler{})
		server.Events = &uio.Events{WriteBufferedThreshold: threshold}
		server.config = newServerConnConfig(server)
		raw := &echoPathConn{}
		conn := server.newConnection(raw)
		conn.opened.Store(true)
		raw.userdata = conn
		payload := bytes.Repeat([]byte{'z'}, 70000)
		raw.out = make([]byte, 0, len(payload)+64)
		if err := conn.SendBinary(payload); err != nil {
			t.Fatal(err)
		}
		f, n, complete, err := frame.ParseFrame(raw.out, &frame.ParserConfig{})
		if err != nil || !complete || n != len(raw.out) || len(f.Payload) != len(payload) {
			t.Fatalf("threshold %d: %d wire bytes parse to a %d-byte payload (err %v)", threshold, len(raw.out), len(f.Payload), err)
		}
	}
}
