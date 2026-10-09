package uio

import "testing"

func TestUserdataMayBeSerializedOutsideCallback(t *testing.T) {
	conn := &commonConn{events: &Events{}, loop: &eventLoop{}}
	conn.SetUserdata("value")
	if conn.Userdata() != "value" {
		t.Fatal("userdata was not available outside callback")
	}
}

// BenchmarkInboundAccessAssertion measures one inbound access on the
// connection's own callback path. In race-enabled builds it includes the
// scope diagnostic; without the race detector the diagnostic is compiled out
// and this is the access's own cost.
func BenchmarkInboundAccessAssertion(b *testing.B) {
	events := &Events{}
	loop := &eventLoop{}
	conn := &commonConn{events: events, loop: loop}
	b.Run("current-callback", func(b *testing.B) {
		started := conn.beginInboundCallback()
		for b.Loop() {
			conn.InboundBuffered()
		}
		conn.endInboundCallback(started)
	})
}
