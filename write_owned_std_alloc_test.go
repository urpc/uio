//go:build (windows || stdio) && !race

package uio

import "testing"

func TestStdWriteOwnedReusesAllocation(t *testing.T) {
	conn := &fdConn{
		commonConn: commonConn{events: &Events{MaxOutboundBuffered: -1}},
		writeSig:   make(chan struct{}, 1),
	}
	payload := make([]byte, 1024)
	write := func() {
		buffer := AcquireBuffer(len(payload))
		dst := buffer.AvailableBuffer()[:len(payload)]
		buffer.CommitWrite(copy(dst, payload))
		if _, err := conn.WriteOwned(buffer); err != nil {
			panic(err)
		}
		conn.outbound.Reset()
		select {
		case <-conn.writeSig:
		default:
		}
	}
	write()
	if allocations := testing.AllocsPerRun(1000, write); allocations != 0 {
		t.Fatalf("WriteOwned allocations = %v, want 0", allocations)
	}
}
