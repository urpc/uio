//go:build !race

package uio

import "testing"

func TestPeekChunkDoesNotAllocate(t *testing.T) {
	for _, conn := range []*commonConn{
		{inboundTail: []byte("tail")},
		{},
	} {
		if len(conn.inboundTail) == 0 {
			_, _ = conn.inbound.WriteString("buffered")
		}
		var peeker interface{ PeekChunk() []byte } = conn
		var chunk []byte
		allocations := testing.AllocsPerRun(1000, func() {
			chunk = peeker.PeekChunk()
		})
		if allocations != 0 || len(chunk) == 0 {
			t.Fatalf("PeekChunk allocations/length = %v/%d", allocations, len(chunk))
		}
	}
}

func TestOwnedBufferPoolReusesLargeBuffer(t *testing.T) {
	payload := make([]byte, 1<<20)
	use := func() {
		buffer := AcquireBuffer(len(payload))
		_, _ = buffer.Write(payload)
		ReleaseBuffer(buffer)
	}
	use()
	if allocations := testing.AllocsPerRun(100, use); allocations != 0 {
		t.Fatalf("large owned buffer allocations = %v, want 0", allocations)
	}
}
