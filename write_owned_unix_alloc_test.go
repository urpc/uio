//go:build (linux || darwin || netbsd || freebsd || openbsd || dragonfly) && !stdio && !race

package uio

import "testing"

func TestUnixWriteOwnedReusesAllocation(t *testing.T) {
	loop := &eventLoop{}
	loop.loopGoid.Store(currentGoroutineID())
	conn := &fdConn{fd: -1}
	conn.events = &Events{WriteBufferedThreshold: 2048, MaxOutboundBuffered: -1}
	conn.loop = loop
	payload := make([]byte, 1024)

	write := func() {
		buffer := AcquireBuffer(len(payload))
		dst := buffer.AvailableBuffer()[:len(payload)]
		buffer.CommitWrite(copy(dst, payload))
		if _, err := conn.WriteOwned(buffer); err != nil {
			panic(err)
		}
		conn.outbound.Reset()
		conn.pending.Store(0)
	}
	write()
	if allocations := testing.AllocsPerRun(1000, write); allocations != 0 {
		t.Fatalf("WriteOwned allocations = %v, want 0", allocations)
	}
}

func TestUnixRejectedWriteOwnedReleasesBuffer(t *testing.T) {
	conn := &fdConn{}
	conn.closing.Store(true)
	payload := make([]byte, 1024)
	write := func() {
		buffer := AcquireBuffer(len(payload))
		_, _ = buffer.Write(payload)
		if _, err := conn.WriteOwned(buffer); err == nil {
			panic("closed connection accepted owned buffer")
		}
	}
	write()
	if allocations := testing.AllocsPerRun(1000, write); allocations != 0 {
		t.Fatalf("rejected WriteOwned allocations = %v, want 0", allocations)
	}
}
