//go:build !race

package uws

import (
	"testing"

	"github.com/urpc/uio/uws/internal/frame"
)

func TestBackpressureRejectsBeforeFrameAllocation(t *testing.T) {
	conn, _ := newBackpressuredConn()
	payload := make([]byte, DefaultMaxFramePayload)
	var sendErr error
	allocations := testing.AllocsPerRun(1, func() {
		sendErr = conn.SendBinary(payload)
	})
	if sendErr != ErrBackpressure {
		t.Fatalf("SendBinary error = %v, want %v", sendErr, ErrBackpressure)
	}
	if allocations != 0 {
		t.Fatalf("backpressured send allocations = %v, want 0", allocations)
	}
}

func TestReadAvailableCompleteFrameDoesNotAllocate(t *testing.T) {
	wire := frame.Append(nil, frame.Frame{
		Fin: true, Opcode: frame.Binary, Masked: true, Payload: make([]byte, 1024),
	}, [4]byte{1, 2, 3, 4})
	raw := &bufferedProbeConn{}
	conn := &Conn{raw: raw, config: testServerConfig(NewServer(nil))}
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
		raw: &writeProbeConn{},
		config: testServerConfig(&Server{
			MaxFramePayload:  1024,
			MaxOutboundBytes: -1,
		}),
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
		raw: &writeProbeConn{},
		config: testDialerConfig(&Dialer{
			MaxFramePayload:  1024,
			MaxOutboundBytes: -1,
		}),
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
		raw: &writeProbeConn{},
		config: testDialerConfig(&Dialer{
			MaxFramePayload:  payloadSize,
			MaxOutboundBytes: -1,
		}),
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

func TestTextWriterValidationDoesNotAllocatePerPayload(t *testing.T) {
	payload := make([]byte, 64<<10)
	conn := &Conn{
		raw: &writeProbeConn{},
		config: testServerConfig(&Server{
			MaxFramePayload:  uint64(len(payload)),
			MaxMessageSize:   1 << 30,
			MaxOutboundBytes: -1,
		}),
	}
	conn.opened.Store(true)
	writer, err := conn.BeginMessage(TextMessage)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = writer.Write(payload); err != nil {
		t.Fatal(err)
	}
	allocations := testing.AllocsPerRun(100, func() {
		if _, writeErr := writer.Write(payload); writeErr != nil {
			panic(writeErr)
		}
	})
	if allocations != 0 {
		t.Fatalf("text Writer.Write allocations = %v, want 0", allocations)
	}
	if err = writer.Close(); err != nil {
		t.Fatal(err)
	}
}
