package uws

import (
	"bytes"
	"io"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/urpc/uio"
	"github.com/urpc/uio/uws/internal/frame"
)

// The fused short-frame paths cover 16-bit lengths as well as seven-bit ones:
// a masked frame whose payload is 126 bytes or more is consumed and echoed
// byte-for-byte as the generic parser would, and a frame past the frame or
// message limits falls back to it and is refused there.
func TestShortPathCoversSixteenBitLengths(t *testing.T) {
	for _, size := range []int{0, 1, 125, 126, 127, 1024, 4096, 65535} {
		server := NewServer(Options{}, echoPathHandler{})
		server.config = testServerConfig(server)
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
	server := NewServer(Options{}, echoPathHandler{})
	server.config = testServerConfig(server)
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
		server := NewServer(Options{}, echoPathHandler{})
		server.config = testServerConfig(server)
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
	server := NewServer(Options{}, echoPathHandler{})
	server.config = testServerConfig(server)
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
	dialer := NewDialer(Options{})
	dialer.Options.WriteCoalesce = 4096
	dialer.config = testDialerConfig(dialer)
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
		server := NewServer(Options{}, echoPathHandler{})
		server.Options.WriteCoalesce = tc.threshold
		config := testServerConfig(server)
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
	server := NewServer(Options{}, echoPathHandler{})
	server.Options.WriteCoalesce = 1 << 20
	server.MaxFramePayload, server.MaxMessageSize = 2048, 1000
	if config := testServerConfig(server); config.shortSendDirect != 1000 || config.shortReadLimit != 125 {
		t.Fatalf("direct=%d read=%d under a 1000-byte message limit, want 1000 and 125", config.shortSendDirect, config.shortReadLimit)
	}

	// A large message under a tiny threshold leaves one intact frame.
	for _, threshold := range []int{1, 2} {
		server := NewServer(Options{}, echoPathHandler{})
		server.Options.WriteCoalesce = threshold
		server.config = testServerConfig(server)
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

// messageLog keeps a copy of every message with its type.
type messageLog struct{ messages *[]Message }

func (messageLog) OnOpen(*Conn) {}

func (h messageLog) OnMessage(_ *Conn, message Message) {
	*h.messages = append(*h.messages, Message{Type: message.Type, Payload: append([]byte(nil), message.Payload...)})
}

func (messageLog) OnClose(*Conn, CloseEvent) {}

// Reads that end inside a frame, as a pipelining peer's full reads do, still
// deliver every message in order and byte for byte: the frame carried over a
// read is finished by the incremental parser, and the frames after it in the
// same read take the short path again, binary and text, seven- and 16-bit,
// around the vector unmask threshold.
func TestReadsSplitInsideFramesDeliverEveryMessage(t *testing.T) {
	want := []Message{
		{Type: BinaryMessage, Payload: bytes.Repeat([]byte{0xA5}, 256)},
		{Type: TextMessage, Payload: bytes.Repeat([]byte("ascii "), 50)},
		{Type: BinaryMessage, Payload: []byte{1, 2, 3}},
		{Type: TextMessage, Payload: []byte(strings.Repeat("ü€😀", 40))},
		{Type: BinaryMessage, Payload: bytes.Repeat([]byte{0x7f}, frame.VectorUnmaskMin)},
		{Type: BinaryMessage, Payload: bytes.Repeat([]byte{0x80}, frame.VectorUnmaskMin-1)},
		{Type: TextMessage, Payload: []byte("short")},
		{Type: BinaryMessage, Payload: bytes.Repeat([]byte{0x00, 0xff}, 600)},
	}
	var wire []byte
	for round := 0; round < 3; round++ {
		for i, message := range want {
			opcode := frame.Binary
			if message.Type == TextMessage {
				opcode = frame.Text
			}
			key := [4]byte{byte(i + 1), byte(round + 7), 0x5a, byte(31 * i)}
			wire = frame.Append(wire, frame.Frame{Fin: true, Opcode: opcode, Masked: true, Payload: message.Payload}, key)
		}
	}
	all := append(append(append([]Message(nil), want...), want...), want...)

	for _, readSize := range []int{1, 2, 3, 5, 8, 13, 64, 127, 128, 262, 263, 264, 265, 700, 1024, 4096, len(wire)} {
		var got []Message
		server := NewServer(Options{}, messageLog{&got})
		server.config = testServerConfig(server)
		raw := &echoPathConn{}
		conn := server.newConnection(raw)
		conn.opened.Store(true)
		raw.userdata = conn

		src := append([]byte(nil), wire...)
		for len(src) > 0 {
			n := min(readSize, len(src))
			raw.inbound = src[:n]
			if err := server.onData(raw); err != nil {
				t.Fatalf("reads of %d: round failed: %v", readSize, err)
			}
			if len(raw.inbound) != 0 {
				t.Fatalf("reads of %d: %d bytes left unconsumed", readSize, len(raw.inbound))
			}
			src = src[n:]
		}
		if len(got) != len(all) {
			t.Fatalf("reads of %d: %d messages, want %d", readSize, len(got), len(all))
		}
		for i := range all {
			if got[i].Type != all[i].Type || !bytes.Equal(got[i].Payload, all[i].Payload) {
				t.Fatalf("reads of %d: message %d differs: type %v len %d, want type %v len %d",
					readSize, i, got[i].Type, len(got[i].Payload), all[i].Type, len(all[i].Payload))
			}
		}
	}
}

// A text frame that follows a frame finished across reads is still validated:
// invalid UTF-8 there fails the round as it does anywhere else.
func TestShortPathAfterASplitFrameValidatesText(t *testing.T) {
	var got []Message
	server := NewServer(Options{}, messageLog{&got})
	server.config = testServerConfig(server)
	raw := &echoPathConn{}
	conn := server.newConnection(raw)
	conn.opened.Store(true)
	raw.userdata = conn
	t.Cleanup(func() { server.onClose(raw, io.EOF) })

	wire := frame.Append(nil, frame.Frame{Fin: true, Opcode: frame.Binary, Masked: true, Payload: bytes.Repeat([]byte{1}, 300)}, [4]byte{1, 2, 3, 4})
	split := len(wire) - 10
	wire = frame.Append(wire, frame.Frame{Fin: true, Opcode: frame.Text, Masked: true, Payload: []byte{0xff, 0xfe, 'x'}}, [4]byte{5, 6, 7, 8})

	raw.inbound = append([]byte(nil), wire[:split]...)
	if err := server.onData(raw); err != nil {
		t.Fatalf("first read failed: %v", err)
	}
	raw.inbound = append([]byte(nil), wire[split:]...)
	err := server.onData(raw)
	if err == nil && !conn.closing.Load() {
		t.Fatal("invalid UTF-8 after a frame finished across reads was accepted")
	}
	if len(got) != 1 {
		t.Fatalf("%d messages delivered, want only the binary one", len(got))
	}
}

// A batching round writes each reply's header in place: every length
// encoding, seven-bit, 16-bit and, past a threshold raised for it, 64-bit,
// comes out byte for byte as the generic encoder writes it.
func TestBatchedRepliesEncodeEveryLengthAsTheGenericEncoder(t *testing.T) {
	sizes := []int{0, 1, 125, 126, 127, 4096, 65535, 65536, 70000}
	server := NewServer(Options{}, echoPathHandler{})
	server.config = testServerConfig(server)
	server.config.writeBufferedThreshold = 1 << 20
	server.config.maxOutbound = 0
	raw := &echoPathConn{}
	conn := server.newConnection(raw)
	conn.opened.Store(true)
	raw.userdata = conn

	var wire, want []byte
	for i, size := range sizes {
		payload := bytes.Repeat([]byte{byte(i + 1)}, size)
		wire = frame.Append(wire, frame.Frame{Fin: true, Opcode: frame.Binary, Masked: true, Payload: payload}, [4]byte{3, 1, 4, 1})
		want = frame.Append(want, frame.Frame{Fin: true, Opcode: frame.Binary, Payload: payload}, [4]byte{})
	}
	raw.inbound = wire
	raw.out = make([]byte, 0, 2*len(want))
	captured := &batchCaptureConn{echoPathConn: raw}
	conn.raw = captured
	if err := server.onData(captured); err != nil {
		t.Fatalf("read round failed: %v", err)
	}
	if !bytes.Equal(captured.written, want) {
		t.Fatalf("batched replies: %d bytes, want %d", len(captured.written), len(want))
	}
}

// batchCaptureConn keeps what a write batch hands over.
type batchCaptureConn struct {
	*echoPathConn
	written []byte
}

func (c *batchCaptureConn) WriteOwned(buffer *uio.Buffer) (int, error) {
	n := buffer.Len()
	c.written = append(c.written, buffer.Bytes()...)
	uio.ReleaseBuffer(buffer)
	return n, nil
}

// textOfLength builds valid UTF-8 of exactly size bytes. With multibyte it
// starts off a 32-byte boundary and repeats two-, three- and four-byte
// sequences, so they straddle every vector and word boundary.
func textOfLength(size int, multibyte bool) []byte {
	if !multibyte {
		return bytes.Repeat([]byte("{\"k\":1}"), size/7+1)[:size]
	}
	out := make([]byte, 0, size)
	out = append(out, "abc"[:min(3, size)]...)
	runes := []string{"é", "€", "😀"}
	for i := 0; len(out) < size; i++ {
		r := runes[i%len(runes)]
		if len(out)+len(r) > size {
			out = append(out, bytes.Repeat([]byte{'x'}, size-len(out))...)
			break
		}
		out = append(out, r...)
	}
	return out
}

// Text frames take the vector unmask from frame.VectorUnmaskMin bytes with
// the ASCII verdict folded in: every length around the word and vector steps
// delivers ASCII and multibyte UTF-8 byte for byte as text, and refuses
// invalid UTF-8 wherever it sits, before the handler sees it.
func TestShortPathTextFramesValidateAcrossTheVectorThreshold(t *testing.T) {
	sizes := []int{0, 1, 7, 15, 16, 17, 31, 32, 33, 63, 64, 125, 126, 127, 128, 129, 130, 159, 160, 161,
		255, 256, 257, 511, 1023, 1024, 1025, 4095, 4096, 4097, 65535, 70000}
	bad := [][]byte{{0xff}, {0xc0, 0xaf}, {0xed, 0xa0, 0x80}, {0xe2, 0x82}, {0x80}}
	run := func(payload []byte) ([]Message, error, bool) {
		var got []Message
		server := NewServer(Options{}, messageLog{&got})
		server.config = testServerConfig(server)
		raw := &echoPathConn{}
		conn := server.newConnection(raw)
		conn.opened.Store(true)
		raw.userdata = conn
		defer server.onClose(raw, io.EOF)
		raw.inbound = frame.Append(nil, frame.Frame{Fin: true, Opcode: frame.Text, Masked: true, Payload: payload}, [4]byte{0xf1, 0x80, 0x3c, 0x9a})
		raw.out = make([]byte, 0, 64) // room for the Close frame a refusal sends
		err := server.onData(raw)
		return got, err, conn.closing.Load()
	}
	for _, size := range sizes {
		for _, multibyte := range []bool{false, true} {
			payload := textOfLength(size, multibyte)
			if !utf8.Valid(payload) || len(payload) != size {
				t.Fatalf("size %d multibyte %v: the generator broke", size, multibyte)
			}
			got, err, closing := run(append([]byte(nil), payload...))
			if err != nil || closing || len(got) != 1 || got[0].Type != TextMessage || !bytes.Equal(got[0].Payload, payload) {
				t.Fatalf("size %d multibyte %v: err %v closing %v, %d messages", size, multibyte, err, closing, len(got))
			}
		}
		for _, seq := range bad {
			if size < len(seq) {
				continue
			}
			for _, at := range []int{0, size / 2, size - len(seq), 31, 32, 127, 128} {
				if at < 0 || at+len(seq) > size {
					continue
				}
				payload := textOfLength(size, false)
				copy(payload[at:], seq)
				if utf8.Valid(payload) {
					continue
				}
				got, err, closing := run(payload)
				if (err == nil && !closing) || len(got) != 0 {
					t.Fatalf("size %d: invalid UTF-8 %x at %d accepted (err %v closing %v, %d messages)", size, seq, at, err, closing, len(got))
				}
			}
		}
	}
}
