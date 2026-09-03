//go:build (windows || stdio) && !race

package uws

import (
	"testing"

	"github.com/urpc/uio/uws/internal/frame"
)

func TestStdServerFrameWritevDoesNotAllocatePerPayload(t *testing.T) {
	payload := make([]byte, 1<<20)
	raw := &stdFrameSink{dst: make([]byte, len(payload)+14)}
	conn := &Conn{
		raw: raw,
		config: testServerConfig(&Server{
			MaxFramePayload:  uint64(len(payload)),
			MaxOutboundBytes: -1,
		}),
	}
	message := frame.Frame{Fin: true, Opcode: frame.Binary, Payload: payload}
	if err := conn.sendFrameLocked(message); err != nil {
		t.Fatal(err)
	}
	allocations := testing.AllocsPerRun(100, func() {
		if err := conn.sendFrameLocked(message); err != nil {
			panic(err)
		}
	})
	if allocations != 0 {
		t.Fatalf("server frame allocations = %v, want 0", allocations)
	}
}
