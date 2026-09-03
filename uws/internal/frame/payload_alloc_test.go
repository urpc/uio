//go:build !race

package frame

import (
	"bytes"
	"testing"
)

func TestIncrementalParserPayloadReusesAllocation(t *testing.T) {
	payload := bytes.Repeat([]byte{0xa5}, 1024)
	wire := Append(nil, Frame{Fin: true, Opcode: Binary, Payload: payload}, [4]byte{})
	parser := NewParser(ParserConfig{MaxFramePayload: uint64(len(payload))})
	emit := func(frame Frame) error {
		if !frame.Borrowed || !bytes.Equal(frame.Payload, payload) {
			panic("unexpected incremental frame")
		}
		return nil
	}
	feed := func() {
		parser.Reset()
		if _, err := parser.Feed(wire[:1], emit); err != nil {
			panic(err)
		}
		if _, err := parser.Feed(wire[1:], emit); err != nil {
			panic(err)
		}
	}
	feed()
	if allocations := testing.AllocsPerRun(1000, feed); allocations != 0 {
		t.Fatalf("incremental payload allocations = %v, want 0", allocations)
	}
}

func TestParserResetReleasesIncompletePayload(t *testing.T) {
	payload := bytes.Repeat([]byte{1}, 1024)
	wire := Append(nil, Frame{Fin: true, Opcode: Binary, Payload: payload}, [4]byte{})
	parser := NewParser(ParserConfig{MaxFramePayload: uint64(len(payload))})
	if _, err := parser.Feed(wire[:4], func(Frame) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if parser.payloadBuf == nil {
		t.Fatal("incremental payload buffer was not acquired")
	}
	parser.Reset()
	if parser.payload != nil || parser.payloadBuf != nil || parser.payloadSize != 0 {
		t.Fatal("Reset retained incremental payload state")
	}
}

func TestParserInitDoesNotAllocate(t *testing.T) {
	cfg := &ParserConfig{ExpectMask: true, MaxFramePayload: 1024}
	parser := &Parser{}
	if allocations := testing.AllocsPerRun(1000, func() { parser.Init(cfg) }); allocations != 0 {
		t.Fatalf("Parser.Init allocations = %v, want 0", allocations)
	}
}

func TestAssemblerRejectsOversizedFirstFragmentWithoutAllocation(t *testing.T) {
	const maxMessage = 1024
	payload := make([]byte, maxMessage+1)
	cfg := AssemblerConfig{MaxMessage: maxMessage, MaxCompressedPayload: maxMessage, ValidateUTF8: true}
	assembler := &Assembler{}
	accept := func() {
		assembler.Init(&cfg)
		err := assembler.Accept(Frame{Opcode: Binary, Payload: payload}, func(Frame) error { return nil }, func(Message) error { return nil })
		if err != ErrMessageTooBig {
			panic(err)
		}
		if assembler.payload != nil {
			panic("oversized fragment was copied")
		}
	}
	if allocations := testing.AllocsPerRun(1000, accept); allocations != 0 {
		t.Fatalf("oversized first fragment allocations = %v, want 0", allocations)
	}
}

func TestAssemblerRejectsOversizedCompressedFirstFragmentWithoutAllocation(t *testing.T) {
	const maxCompressed = 1024
	payload := make([]byte, maxCompressed+1)
	cfg := AssemblerConfig{MaxMessage: 64 << 20, MaxCompressedPayload: maxCompressed, ValidateUTF8: true}
	assembler := &Assembler{}
	accept := func() {
		assembler.Init(&cfg)
		err := assembler.Accept(Frame{RSV1: true, Opcode: Binary, Payload: payload}, func(Frame) error { return nil }, func(Message) error { return nil })
		if err != ErrMessageTooBig {
			panic(err)
		}
		if assembler.payload != nil {
			panic("oversized compressed fragment was copied")
		}
	}
	if allocations := testing.AllocsPerRun(1000, accept); allocations != 0 {
		t.Fatalf("oversized compressed first fragment allocations = %v, want 0", allocations)
	}
}
