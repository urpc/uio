//go:build !race

package uws

import (
	"testing"

	"github.com/urpc/uio/uws/internal/frame"
)

func TestCompleteFrameParsingDoesNotAllocateParser(t *testing.T) {
	conn := &Conn{config: testDialerConfig(NewDialer(Options{}))}
	wire := frame.Append(nil, frame.Frame{Fin: true, Opcode: frame.Binary, Payload: []byte("payload")}, [4]byte{})
	emit := func(frame.Frame) error { return nil }
	parse := func() {
		consumed, err := conn.feedFrames(wire, emit)
		if err != nil || consumed != len(wire) || conn.parser != nil {
			panic("complete frame retained parser state")
		}
	}
	parse()
	if allocations := testing.AllocsPerRun(1000, parse); allocations != 0 {
		t.Fatalf("complete frame parser allocations = %v, want 0", allocations)
	}
}

func TestIncrementalFrameParserPoolReusesState(t *testing.T) {
	conn := &Conn{config: testDialerConfig(NewDialer(Options{}))}
	wire := frame.Append(nil, frame.Frame{Fin: true, Opcode: frame.Binary, Payload: []byte("payload")}, [4]byte{})
	emit := func(frame.Frame) error { return nil }
	parse := func() {
		if _, err := conn.feedFrames(wire[:1], emit); err != nil || conn.parser == nil {
			panic("partial frame did not retain parser state")
		}
		if _, err := conn.feedFrames(wire[1:], emit); err != nil || conn.parser != nil {
			panic("completed frame retained parser state")
		}
	}
	parse()
	if allocations := testing.AllocsPerRun(1000, parse); allocations != 0 {
		t.Fatalf("reused incremental parser allocations = %v, want 0", allocations)
	}
}

func TestCompleteMessageDoesNotAllocateAssembler(t *testing.T) {
	conn := &Conn{config: testDialerConfig(NewDialer(Options{}))}
	message := frame.Frame{Fin: true, Opcode: frame.Binary}
	accept := func() {
		if err := conn.acceptFrame(message); err != nil || conn.assembler != nil {
			panic("complete message retained assembler state")
		}
	}
	accept()
	if allocations := testing.AllocsPerRun(1000, accept); allocations != 0 {
		t.Fatalf("complete message assembler allocations = %v, want 0", allocations)
	}
}

func TestFragmentedMessageAssemblerPoolReusesState(t *testing.T) {
	conn := &Conn{config: testDialerConfig(NewDialer(Options{}))}
	first := frame.Frame{Opcode: frame.Binary}
	last := frame.Frame{Fin: true, Opcode: frame.Continuation}
	accept := func() {
		if err := conn.acceptFrame(first); err != nil || conn.assembler == nil {
			panic("first fragment did not retain assembler state")
		}
		if err := conn.acceptFrame(last); err != nil || conn.assembler != nil {
			panic("last fragment retained assembler state")
		}
	}
	accept()
	if allocations := testing.AllocsPerRun(1000, accept); allocations != 0 {
		t.Fatalf("reused fragmented assembler allocations = %v, want 0", allocations)
	}
}

func TestReadAvailableCompleteFrameDoesNotAllocate(t *testing.T) {
	wire := frame.Append(nil, frame.Frame{
		Fin: true, Opcode: frame.Binary, Masked: true, Payload: make([]byte, 1024),
	}, [4]byte{1, 2, 3, 4})
	raw := &bufferedProbeConn{}
	conn := &Conn{raw: raw, config: testServerConfig(NewServer(Options{}, nil))}
	conn.opened.Store(true)
	read := func() {
		raw.inbound = wire
		if err := conn.readAvailable(); err != nil {
			panic(err)
		}
	}
	read()
	if allocations := testing.AllocsPerRun(1000, read); allocations != 0 {
		t.Fatalf("complete frame read allocations = %v, want 0", allocations)
	}
}

func TestServerFrameScratchReusesAllocation(t *testing.T) {
	conn := &Conn{
		raw:    &writeProbeConn{},
		config: testServerConfig(&Server{Options: Options{MaxFramePayload: 1024}}),
	}
	message := frame.Frame{Fin: true, Opcode: frame.Binary, Payload: make([]byte, 1024)}
	if err := conn.sendFrameLocked(message); err != nil {
		t.Fatal(err)
	}
	allocations := testing.AllocsPerRun(1000, func() {
		if err := conn.sendFrameLocked(message); err != nil {
			panic(err)
		}
	})
	if allocations != 0 {
		t.Fatalf("server frame allocations = %v, want 0", allocations)
	}
}

func TestClientMaskedFrameOwnedWriteDoesNotAllocate(t *testing.T) {
	conn := &Conn{
		raw:    &writeProbeConn{},
		config: testDialerConfig(&Dialer{Options: Options{MaxFramePayload: 1024}}),
	}
	message := frame.Frame{Fin: true, Opcode: frame.Binary, Payload: make([]byte, 1024)}
	if err := conn.sendFrameLocked(message); err != nil {
		t.Fatal(err)
	}
	allocations := testing.AllocsPerRun(1000, func() {
		if err := conn.sendFrameLocked(message); err != nil {
			panic(err)
		}
	})
	if allocations != 0 {
		t.Fatalf("client owned frame allocations = %v, want 0", allocations)
	}
}

func TestLargeClientMaskedFrameOwnedWriteDoesNotAllocate(t *testing.T) {
	const payloadSize = 1 << 20
	conn := &Conn{
		raw:    &writeProbeConn{},
		config: testDialerConfig(&Dialer{Options: Options{MaxFramePayload: payloadSize}}),
	}
	message := frame.Frame{Fin: true, Opcode: frame.Binary, Payload: make([]byte, payloadSize)}
	if err := conn.sendFrameLocked(message); err != nil {
		t.Fatal(err)
	}
	allocations := testing.AllocsPerRun(20, func() {
		if err := conn.sendFrameLocked(message); err != nil {
			panic(err)
		}
	})
	if allocations != 0 {
		t.Fatalf("large client owned frame allocations = %v, want 0", allocations)
	}
}
