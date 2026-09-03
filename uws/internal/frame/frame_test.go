package frame

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"testing"
)

func TestAppendAndFeedRoundTripAcrossLengths(t *testing.T) {
	key := [4]byte{1, 2, 3, 4}
	for _, size := range []int{0, 1, 125, 126, 127, 65535, 65536} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			payload := bytes.Repeat([]byte{0xa5}, size)
			wire := Append(nil, Frame{Fin: true, Opcode: Binary, Masked: true, Payload: payload}, key)
			parser := NewParser(ParserConfig{ExpectMask: true, MaxFramePayload: uint64(size)})
			var got Frame
			frames := 0
			for i := range wire {
				consumed, err := parser.Feed(wire[i:i+1], func(frame Frame) error {
					got = frame
					frames++
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
				if consumed != 1 {
					t.Fatalf("Feed() consumed %d bytes, want 1", consumed)
				}
			}
			if frames != 1 {
				t.Fatalf("frames = %d, want 1", frames)
			}
			if !got.Fin || got.Opcode != Binary || !bytes.Equal(got.Payload, payload) {
				t.Fatalf("decoded frame = fin:%v opcode:%x payload:%d, want binary %d bytes", got.Fin, got.Opcode, len(got.Payload), size)
			}
		})
	}
}

func TestAppendUsesCanonicalLengths(t *testing.T) {
	for _, test := range []struct {
		size       int
		headerSize int
	}{
		{size: 125, headerSize: 2},
		{size: 126, headerSize: 4},
		{size: 65535, headerSize: 4},
		{size: 65536, headerSize: 10},
	} {
		wire := Append(nil, Frame{Fin: true, Opcode: Binary, Payload: make([]byte, test.size)}, [4]byte{})
		if got := len(wire) - test.size; got != test.headerSize {
			t.Fatalf("size %d header bytes = %d, want %d", test.size, got, test.headerSize)
		}
	}
}

func TestAppendHeaderMatchesFramePrefix(t *testing.T) {
	key := [4]byte{1, 2, 3, 4}
	for _, masked := range []bool{false, true} {
		for _, size := range []int{0, 1, 125, 126, 65535, 65536} {
			frame := Frame{Fin: true, Opcode: Binary, Masked: masked, Payload: make([]byte, size)}
			wire := Append(nil, frame, key)
			header := AppendHeader(nil, frame, key)
			if got := wire[:len(header)]; !bytes.Equal(got, header) {
				t.Fatalf("masked %v size %d: header %x, want %x", masked, size, header, got)
			}
		}
	}
}

func TestParserFeedsMultipleFramesFromOneBuffer(t *testing.T) {
	wire := Append(nil, Frame{Fin: true, Opcode: Text, Payload: []byte("one")}, [4]byte{})
	wire = Append(wire, Frame{Fin: true, Opcode: Binary, Payload: []byte("two")}, [4]byte{})
	parser := NewParser(ParserConfig{})
	var got []Frame
	if consumed, err := parser.Feed(wire, func(f Frame) error {
		got = append(got, f)
		return nil
	}); err != nil || consumed != len(wire) {
		t.Fatalf("Feed() = %d, %v; want %d, nil", consumed, err, len(wire))
	}
	if len(got) != 2 || string(got[0].Payload) != "one" || string(got[1].Payload) != "two" {
		t.Fatalf("frames = %+v, want one and two", got)
	}
}

func TestParseFrameCompleteAndIncomplete(t *testing.T) {
	cfg := &ParserConfig{ExpectMask: true, MaxFramePayload: 64}
	wire := Append(nil, Frame{Fin: true, Opcode: Binary, Masked: true, Payload: []byte("payload")}, [4]byte{1, 2, 3, 4})
	if _, size, complete, err := ParseFrame(wire[:1], cfg); err != nil || complete || size != 0 {
		t.Fatalf("incomplete ParseFrame = size %d, complete %v, error %v", size, complete, err)
	}
	parsed, size, complete, err := ParseFrame(wire, cfg)
	if err != nil || !complete || size != len(wire) {
		t.Fatalf("complete ParseFrame = size %d, complete %v, error %v", size, complete, err)
	}
	if parsed.Opcode != Binary || string(parsed.Payload) != "payload" || !parsed.Borrowed {
		t.Fatalf("parsed frame = %+v", parsed)
	}
}

func TestParserInitClearsIncrementalStateAndChangesConfig(t *testing.T) {
	masked := &ParserConfig{ExpectMask: true, MaxFramePayload: 64}
	unmasked := &ParserConfig{MaxFramePayload: 64}
	p := &Parser{}
	p.Init(masked)
	wire := Append(nil, Frame{Fin: true, Opcode: Binary, Masked: true, Payload: []byte("payload")}, [4]byte{1, 2, 3, 4})
	if _, err := p.Feed(wire[:7], func(Frame) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if p.AtFrameBoundary() || p.payloadBuf == nil {
		t.Fatal("partial frame did not retain incremental state")
	}

	p.Init(unmasked)
	if !p.AtFrameBoundary() || p.payloadBuf != nil || p.cfg != unmasked {
		t.Fatal("Init retained old parser state or configuration")
	}
	unmaskedWire := Append(nil, Frame{Fin: true, Opcode: Binary, Payload: []byte("next")}, [4]byte{})
	if _, err := p.Feed(unmaskedWire, func(Frame) error { return nil }); err != nil {
		t.Fatal(err)
	}
	p.Init(nil)
	if p.cfg != nil || !p.AtFrameBoundary() {
		t.Fatal("nil Init retained configuration or frame state")
	}
}

func TestParserRejectsInvalidFrames(t *testing.T) {
	tests := []struct {
		name string
		wire []byte
		cfg  ParserConfig
		want error
	}{
		{
			name: "unexpected unmasked",
			wire: Append(nil, Frame{Fin: true, Opcode: Text, Payload: []byte("x")}, [4]byte{}),
			cfg:  ParserConfig{ExpectMask: true},
			want: ErrProtocol,
		},
		{
			name: "unexpected masked",
			wire: Append(nil, Frame{Fin: true, Opcode: Text, Masked: true, Payload: []byte("x")}, [4]byte{}),
			cfg:  ParserConfig{ExpectMask: false},
			want: ErrProtocol,
		},
		{
			name: "reserved rsv1",
			wire: []byte{0xc1, 0},
			cfg:  ParserConfig{},
			want: ErrProtocol,
		},
		{
			name: "reserved rsv1 control",
			wire: []byte{0xc9, 0},
			cfg:  ParserConfig{AllowRSV1: true},
			want: ErrProtocol,
		},
		{
			name: "reserved rsv2",
			wire: []byte{0xa1, 0},
			cfg:  ParserConfig{AllowRSV1: true},
			want: ErrProtocol,
		},
		{
			name: "invalid opcode",
			wire: []byte{0x83, 0},
			cfg:  ParserConfig{},
			want: ErrProtocol,
		},
		{
			name: "fragmented ping",
			wire: []byte{0x09, 0},
			cfg:  ParserConfig{},
			want: ErrProtocol,
		},
		{
			name: "extended control payload",
			wire: []byte{0x89, 126, 0, 126},
			cfg:  ParserConfig{},
			want: ErrProtocol,
		},
		{
			name: "length reserved bit",
			wire: []byte{0x82, 127, 0x80, 0, 0, 0, 0, 0, 0, 0},
			cfg:  ParserConfig{},
			want: ErrProtocol,
		},
		{
			name: "frame limit",
			wire: []byte{0x82, 126, 0x04, 0x01},
			cfg:  ParserConfig{MaxFramePayload: 1024},
			want: ErrMessageTooBig,
		},
		{
			name: "noncanonical 126 length",
			wire: []byte{0x82, 126, 0, 125},
			cfg:  ParserConfig{},
			want: ErrProtocol,
		},
		{
			name: "noncanonical 127 length",
			wire: []byte{0x82, 127, 0, 0, 0, 0, 0, 0, 0, 1},
			cfg:  ParserConfig{},
			want: ErrProtocol,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			parser := NewParser(test.cfg)
			_, err := parser.Feed(test.wire, func(Frame) error { return nil })
			if !errors.Is(err, test.want) {
				t.Fatalf("Feed() error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestParserRejectsPayloadLengthThatOverflowsIntWithHeader(t *testing.T) {
	length := uint64(^uint(0) >> 1)
	wire := []byte{0x82, 127, byte(length >> 56), byte(length >> 48), byte(length >> 40), byte(length >> 32), byte(length >> 24), byte(length >> 16), byte(length >> 8), byte(length)}
	for _, test := range []struct {
		name string
		feed []byte
	}{
		{name: "fast", feed: wire},
		{name: "incremental", feed: append([]byte(nil), wire...)},
	} {
		t.Run(test.name, func(t *testing.T) {
			parser := NewParser(ParserConfig{MaxFramePayload: length})
			var err error
			if test.name == "incremental" {
				if _, err = parser.Feed(test.feed[:1], func(Frame) error { return nil }); err != nil {
					t.Fatal(err)
				}
				_, err = parser.Feed(test.feed[1:], func(Frame) error { return nil })
			} else {
				_, err = parser.Feed(test.feed, func(Frame) error { return nil })
			}
			if !errors.Is(err, ErrMessageTooBig) {
				t.Fatalf("Feed() error = %v, want %v", err, ErrMessageTooBig)
			}
		})
	}
}

func TestParserDoesNotAllocateDeclaredPayloadBeforeBytesArrive(t *testing.T) {
	const payloadLen = 16 << 20
	header := make([]byte, 10)
	header[0] = 0x82
	header[1] = 127
	binary.BigEndian.PutUint64(header[2:], payloadLen)

	parser := NewParser(ParserConfig{MaxFramePayload: payloadLen})
	consumed, err := parser.Feed(header, func(Frame) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if consumed != len(header) {
		t.Fatalf("header consumed = %d, want %d", consumed, len(header))
	}
	if got := cap(parser.payload); got > initialPayloadBuffer {
		t.Fatalf("payload capacity after header = %d, want at most %d", got, initialPayloadBuffer)
	}

	if _, err = parser.Feed([]byte("x"), func(Frame) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if got := len(parser.payload); got != 1 {
		t.Fatalf("payload length after one byte = %d, want 1", got)
	}
}

func TestAssemblerHandlesFragmentsAndControls(t *testing.T) {
	a := NewAssembler(64)
	var controls []OpCode
	var messages []Message
	accept := func(f Frame) error {
		return a.Accept(f, func(control Frame) error {
			controls = append(controls, control.Opcode)
			return nil
		}, func(message Message) error {
			messages = append(messages, message)
			return nil
		})
	}
	if err := accept(Frame{Opcode: Text, Payload: []byte("hel")}); err != nil {
		t.Fatal(err)
	}
	if err := accept(Frame{Fin: true, Opcode: Ping, Payload: []byte("?")}); err != nil {
		t.Fatal(err)
	}
	if err := accept(Frame{Fin: true, Opcode: Continuation, Payload: []byte("lo")}); err != nil {
		t.Fatal(err)
	}
	if len(controls) != 1 || controls[0] != Ping {
		t.Fatalf("controls = %v, want ping", controls)
	}
	if len(messages) != 1 || messages[0].Opcode != Text || string(messages[0].Payload) != "hello" {
		t.Fatalf("messages = %+v, want hello text", messages)
	}
}

func TestAssemblerInitClearsPooledState(t *testing.T) {
	cfg := AssemblerConfig{MaxMessage: 64, MaxCompressedPayload: 32, ValidateUTF8: true}
	a := &Assembler{}
	a.Init(&cfg)
	if err := a.Accept(Frame{Opcode: Binary, Payload: []byte("partial")}, func(Frame) error { return nil }, func(Message) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if a.cfg == nil || a.payload == nil || a.AtMessageBoundary() {
		t.Fatal("assembler did not retain fragmented state")
	}
	a.Init(nil)
	if a.cfg != nil || a.payload != nil || !a.AtMessageBoundary() {
		t.Fatal("pooled assembler retained configuration or payload state")
	}
}

func TestAssemblerRejectsInvalidMessageSequences(t *testing.T) {
	tests := []struct {
		name   string
		first  Frame
		second *Frame
	}{
		{
			name:  "continuation without start",
			first: Frame{Fin: true, Opcode: Continuation},
		},
		{
			name:   "new data while fragmented",
			first:  Frame{Opcode: Text, Payload: []byte("a")},
			second: &Frame{Fin: true, Opcode: Binary, Payload: []byte("b")},
		},
		{
			name:   "rsv1 continuation",
			first:  Frame{Opcode: Binary, Payload: []byte("a")},
			second: &Frame{Fin: true, RSV1: true, Opcode: Continuation, Payload: []byte("b")},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			a := NewAssembler(64)
			accept := func(f Frame) error {
				return a.Accept(f, func(Frame) error { return nil }, func(Message) error { return nil })
			}
			if err := accept(test.first); test.second == nil && !errors.Is(err, ErrProtocol) {
				t.Fatalf("first Accept() error = %v, want protocol error", err)
			}
			if test.second != nil {
				if err := accept(*test.second); !errors.Is(err, ErrProtocol) {
					t.Fatalf("second Accept() error = %v, want protocol error", err)
				}
			}
		})
	}
}

func TestAssemblerValidatesTextAndMessageSize(t *testing.T) {
	a := NewAssembler(2)
	err := a.Accept(Frame{Fin: true, Opcode: Text, Payload: []byte{0xff}}, func(Frame) error { return nil }, func(Message) error { return nil })
	if !errors.Is(err, ErrInvalidUTF8) {
		t.Fatalf("invalid text error = %v, want %v", err, ErrInvalidUTF8)
	}
	a.Reset()
	err = a.Accept(Frame{Opcode: Binary, Payload: []byte("ab")}, func(Frame) error { return nil }, func(Message) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	err = a.Accept(Frame{Fin: true, Opcode: Continuation, Payload: []byte("c")}, func(Frame) error { return nil }, func(Message) error { return nil })
	if !errors.Is(err, ErrMessageTooBig) {
		t.Fatalf("oversized message error = %v, want %v", err, ErrMessageTooBig)
	}
}

func TestAssemblerRejectsOversizedMessageBeforeEmission(t *testing.T) {
	const maxMessage = 1024
	payload := make([]byte, maxMessage+1)
	for _, test := range []struct {
		name   string
		frames []Frame
	}{
		{
			name:   "complete",
			frames: []Frame{{Fin: true, Opcode: Binary, Payload: payload}},
		},
		{
			name: "fragmented",
			frames: []Frame{
				{Opcode: Binary, Payload: payload[:maxMessage/2]},
				{Fin: true, Opcode: Continuation, Payload: payload[maxMessage/2:]},
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			a := NewAssembler(maxMessage)
			emitted := 0
			var err error
			for _, f := range test.frames {
				err = a.Accept(f, func(Frame) error { return nil }, func(Message) error {
					emitted++
					return nil
				})
				if err != nil {
					break
				}
			}
			if !errors.Is(err, ErrMessageTooBig) {
				t.Fatalf("Accept() error = %v, want %v", err, ErrMessageTooBig)
			}
			if emitted != 0 {
				t.Fatalf("emitted messages = %d, want 0", emitted)
			}
		})
	}
}

func TestAssemblerDefersCompressedSizeLimitToDecoder(t *testing.T) {
	a := NewAssemblerWithLimits(1, 64)
	var got Message
	err := a.Accept(Frame{
		Fin: true, RSV1: true, Opcode: Binary, Payload: []byte("compressed wire data"),
	}, func(Frame) error { return nil }, func(message Message) error {
		got = message
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !got.Compressed || string(got.Payload) != "compressed wire data" {
		t.Fatalf("message = %+v", got)
	}
}

func TestAssemblerRejectsOversizedCompressedFragmentsBeforeAppend(t *testing.T) {
	a := NewAssemblerWithLimits(64, 4)
	emitted := 0
	accept := func(f Frame) error {
		return a.Accept(f, func(Frame) error { return nil }, func(Message) error {
			emitted++
			return nil
		})
	}
	if err := accept(Frame{RSV1: true, Opcode: Binary, Payload: []byte("ab")}); err != nil {
		t.Fatal(err)
	}
	if err := accept(Frame{Opcode: Continuation, Payload: []byte("cd")}); err != nil {
		t.Fatal(err)
	}
	beforeLen, beforeCap := len(a.payload), cap(a.payload)
	if err := accept(Frame{Opcode: Continuation, Payload: []byte("e")}); !errors.Is(err, ErrMessageTooBig) {
		t.Fatalf("oversized continuation error = %v, want %v", err, ErrMessageTooBig)
	}
	if len(a.payload) != beforeLen || cap(a.payload) != beforeCap {
		t.Fatalf("payload grew from len/cap %d/%d to %d/%d", beforeLen, beforeCap, len(a.payload), cap(a.payload))
	}
	if emitted != 0 {
		t.Fatalf("emitted messages = %d, want 0", emitted)
	}
}

func TestAssemblerPreservesCompressedMessageMetadata(t *testing.T) {
	a := NewAssembler(64)
	var got Message
	if err := a.Accept(Frame{Fin: true, RSV1: true, Opcode: Binary, Payload: []byte{1, 2}}, func(Frame) error { return nil }, func(message Message) error {
		got = message
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !got.Compressed || got.Opcode != Binary || len(got.Payload) != 2 {
		t.Fatalf("message = %+v", got)
	}
	if err := a.Accept(Frame{Fin: true, RSV1: true, Opcode: Ping}, func(Frame) error { return nil }, func(Message) error { return nil }); !errors.Is(err, ErrProtocol) {
		t.Fatalf("compressed control error = %v, want %v", err, ErrProtocol)
	}
}

func TestFrameHelpers(t *testing.T) {
	if !IsControl(Ping) || IsControl(Binary) {
		t.Fatal("IsControl classified frame incorrectly")
	}
	if code := CloseCode(nil); code != 1005 {
		t.Fatalf("empty close code = %d", code)
	}
	if code := CloseCode([]byte{3, 232}); code != 1000 {
		t.Fatalf("close code = %d", code)
	}
	if assembler := NewAssembler(0); assemblerMaxMessage(assembler.cfg) != uint64(maxInt()) || assemblerMaxCompressedPayload(assembler.cfg) != uint64(maxInt()) {
		t.Fatalf("default assembler limits = %d/%d", assemblerMaxMessage(assembler.cfg), assemblerMaxCompressedPayload(assembler.cfg))
	}
}

func TestParserAndAssemblerRejectNilCallbacks(t *testing.T) {
	parser := NewParser(ParserConfig{})
	wire := Append(nil, Frame{Fin: true, Opcode: Binary}, [4]byte{})
	if _, err := parser.Feed(wire, nil); err == nil {
		t.Fatalf("nil frame callback error = %v", err)
	}

	assembler := NewAssembler(64)
	if err := assembler.Accept(Frame{Fin: true, Opcode: Ping}, nil, func(Message) error { return nil }); err == nil {
		t.Fatalf("nil control callback error = %v", err)
	}
	if err := assembler.Accept(Frame{Fin: true, Opcode: Binary}, func(Frame) error { return nil }, nil); err == nil {
		t.Fatalf("nil message callback error = %v", err)
	}
}

func TestIncrementalParserResetsBeforeCallbackError(t *testing.T) {
	first := Append(nil, Frame{Fin: true, Opcode: Binary, Payload: []byte("first")}, [4]byte{})
	second := Append(nil, Frame{Fin: true, Opcode: Binary, Payload: []byte("second")}, [4]byte{})
	parser := NewParser(ParserConfig{})
	if _, err := parser.Feed(first[:1], func(Frame) error { return nil }); err != nil {
		t.Fatal(err)
	}
	sentinel := errors.New("stop after frame")
	if _, err := parser.Feed(first[1:], func(Frame) error { return sentinel }); !errors.Is(err, sentinel) {
		t.Fatalf("callback error = %v", err)
	}
	var payload string
	if _, err := parser.Feed(second, func(frame Frame) error {
		payload = string(frame.Payload)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if payload != "second" {
		t.Fatalf("next payload = %q, want second", payload)
	}
}

func TestClosePayloadValidation(t *testing.T) {
	valid := make([]byte, 2)
	binary.BigEndian.PutUint16(valid, 1000)
	for _, payload := range [][]byte{nil, valid, append(valid, []byte("bye")...)} {
		if err := ValidateClosePayload(payload); err != nil {
			t.Fatalf("ValidateClosePayload(%x) = %v", payload, err)
		}
	}
	for _, payload := range [][]byte{{1}, {0, 1}, {3, 236}, {3, 232, 0xff}} {
		if err := ValidateClosePayload(payload); err == nil {
			t.Fatalf("ValidateClosePayload(%x) accepted invalid payload", payload)
		}
	}
}

func FuzzParserNeverPanics(f *testing.F) {
	f.Add([]byte{0x81, 0x00})
	f.Add([]byte{0x81, 0x82, 1, 2, 3, 4, 'o', 'k'})
	f.Fuzz(func(t *testing.T, data []byte) {
		parser := NewParser(ParserConfig{ExpectMask: true, AllowRSV1: true, MaxFramePayload: 1 << 20})
		defer func() {
			if recovered := recover(); recovered != nil {
				t.Fatalf("parser panicked: %v", recovered)
			}
		}()
		_, _ = parser.Feed(data, func(Frame) error { return nil })
	})
}
