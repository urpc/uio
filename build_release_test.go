//go:build !race

package uio

import (
	"testing"
)

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

// Without the race detector the inbound-scope diagnostics are compiled out;
// the contract is the API documentation's and the connection turn's
// serialization. This pins the documented release behavior: a call outside a
// callback is served normally, it simply loses the timely panic.
func TestInboundAccessOutsideCallbackIsUndiagnosed(t *testing.T) {
	conn := &commonConn{events: &Events{}, loop: &eventLoop{}, inboundTail: []byte("x")}
	if got := conn.InboundBuffered(); got != 1 {
		t.Fatalf("inbound data = %d, want 1", got)
	}
	if started := conn.beginInboundCallback(); started {
		// begin/end are no-ops without the diagnostics; the returned flag
		// only ever reports the nested-callback bookkeeping the race build
		// does, so nothing outside it may rely on a true.
		t.Fatal("diagnostics-free build reported an entered callback")
	}
}
