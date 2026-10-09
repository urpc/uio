//go:build !race

package uio

import "testing"

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
