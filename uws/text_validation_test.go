package uws

import (
	"bytes"
	"fmt"
	"io"
	"testing"
	"unicode/utf8"

	"github.com/urpc/uio/uws/internal/compress"
	"github.com/urpc/uio/uws/internal/frame"
)

// textPaths are the ways a text message reaches validation: one frame in one
// read (the short path up to 16-bit lengths, the parser past them), one frame
// over reads of seven bytes (the incremental parser), three fragments (the
// assembler), and permessage-deflate (validated after inflating).
var textPaths = []string{"frame", "split", "fragments", "compressed"}

// deliverText sends payload as one text message along path and reports what
// the handler got and whether the connection refused the message.
func deliverText(t testing.TB, payload []byte, path string) ([]Message, bool) {
	t.Helper()
	var got []Message
	server := NewServer(messageLog{&got})
	server.config = newServerConnConfig(server)
	raw := &echoPathConn{}
	conn := server.newConnection(raw)
	conn.opened.Store(true)
	raw.userdata = conn
	defer server.onClose(raw, io.EOF)
	raw.out = make([]byte, 0, 64) // room for the Close frame a refusal sends

	key := [4]byte{0x9e, 0x01, 0xc3, 0x7f}
	text := func(f frame.Frame) []byte {
		f.Masked = true
		return frame.Append(nil, f, key)
	}
	read := func(wire []byte, size int) bool {
		for len(wire) > 0 && !conn.closing.Load() {
			n := min(size, len(wire))
			raw.inbound = wire[:n]
			if err := server.onData(raw); err != nil {
				return true
			}
			wire = wire[n:]
		}
		return conn.closing.Load()
	}
	switch path {
	case "frame":
		return got, read(text(frame.Frame{Fin: true, Opcode: frame.Text, Payload: payload}), len(payload)+14)
	case "split":
		return got, read(text(frame.Frame{Fin: true, Opcode: frame.Text, Payload: payload}), 7)
	case "fragments":
		a, b := len(payload)/3, 2*len(payload)/3
		wire := text(frame.Frame{Opcode: frame.Text, Payload: payload[:a]})
		wire = append(wire, text(frame.Frame{Opcode: frame.Continuation, Payload: payload[a:b]})...)
		wire = append(wire, text(frame.Frame{Fin: true, Opcode: frame.Continuation, Payload: payload[b:]})...)
		return got, read(wire, len(wire))
	case "compressed":
		encoded, err := compress.Compress(payload, -1)
		if err != nil {
			t.Fatal(err)
		}
		conn.compression = &compressionState{decoder: compress.NewDecoder(true)}
		err = conn.acceptFrame(frame.Frame{Fin: true, RSV1: true, Opcode: frame.Text, Payload: encoded})
		return got, err != nil
	}
	t.Fatalf("unknown path %q", path)
	return nil, false
}

// checkTextDelivery holds every path to utf8.Valid: valid text is delivered
// once, byte for byte, as text; anything else is refused before the handler
// sees it.
func checkTextDelivery(t testing.TB, payload []byte, what string) {
	t.Helper()
	valid := utf8.Valid(payload)
	for _, path := range textPaths {
		got, refused := deliverText(t, append([]byte(nil), payload...), path)
		if valid && (refused || len(got) != 1 || got[0].Type != TextMessage || !bytes.Equal(got[0].Payload, payload)) {
			t.Fatalf("%s, %s path: valid text of %d bytes refused %v, %d messages", what, path, len(payload), refused, len(got))
		}
		if !valid && (!refused || len(got) != 0) {
			t.Fatalf("%s, %s path: invalid text of %d bytes refused %v, %d messages\n% x", what, path, len(payload), refused, len(got), payload)
		}
	}
}

// Multibyte text, valid and broken, is judged the same on every path and at
// every length around the validator's 32-byte blocks and the unmask's vector
// threshold: cut short, with a byte replaced or a bad sequence set at the
// block edges, the start and the end.
func TestTextValidationAgreesOnEveryPath(t *testing.T) {
	sizes := []int{1, 2, 3, 4, 31, 32, 33, 34, 63, 64, 65, 95, 96, 97, 125, 126, 127, 128, 129, 1000, 1024, 4097, 65535, 70000}
	bad := [][]byte{{0xff}, {0x80}, {0xc0, 0xaf}, {0xe0, 0x80, 0xaf}, {0xed, 0xa0, 0x80}, {0xf4, 0x90, 0x80, 0x80}, {0xe4, 0xb8}, {0xf0, 0x9f, 0x98}}
	for _, size := range sizes {
		for _, multibyte := range []bool{true, false} {
			text := textOfLength(size, multibyte)
			checkTextDelivery(t, text, fmt.Sprintf("text of %d bytes, multibyte %v", size, multibyte))
			if multibyte {
				// Cut short, possibly mid-sequence.
				cut := bytes.Repeat([]byte("中文"), size/6+1)[:size]
				checkTextDelivery(t, cut, fmt.Sprintf("Chinese cut at %d bytes", size))
			}
			for _, seq := range bad {
				for _, at := range []int{0, 29, 30, 31, 32, 33, 63, 64, size / 2, size - len(seq)} {
					if at < 0 || at+len(seq) > size {
						continue
					}
					p := append([]byte(nil), text...)
					copy(p[at:], seq)
					checkTextDelivery(t, p, fmt.Sprintf("%d bytes, multibyte %v, % x at %d", size, multibyte, seq, at))
				}
			}
		}
	}
}

func FuzzTextValidation(f *testing.F) {
	f.Add([]byte("中文文本"), uint8(0))
	f.Add([]byte("😀é€\xff"), uint8(40))
	f.Add([]byte("\xed\xa0\x80"), uint8(200))
	f.Add(bytes.Repeat([]byte("文本帧"), 50), uint8(130))
	f.Fuzz(func(t *testing.T, p []byte, lead uint8) {
		// A multibyte lead takes short input past the validator's block
		// size and the unmask's vector threshold.
		prefix := bytes.Repeat([]byte("中"), int(lead%64))
		if lead >= 128 {
			prefix = bytes.Repeat([]byte("a"), int(lead%128))
		}
		checkTextDelivery(t, append(prefix, p...), "fuzzed")
	})
}
