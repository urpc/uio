//go:build !race

package uws

import (
	"testing"

	"github.com/urpc/uio/uws/internal/frame"
)

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
			MaxFramePayload: 1024,
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
			MaxFramePayload: 1024,
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
			MaxFramePayload: payloadSize,
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
