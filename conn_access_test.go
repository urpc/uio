package uio

import (
	"bytes"
	"testing"
)

func TestInboundAccessPanicsOutsideCallback(t *testing.T) {
	operations := map[string]func(*commonConn){
		"peek":      func(conn *commonConn) { conn.Peek(make([]byte, 1)) },
		"peekChunk": func(conn *commonConn) { conn.PeekChunk() },
		"discard":   func(conn *commonConn) { _, _ = conn.Discard(1) },
		"buffered":  func(conn *commonConn) { conn.InboundBuffered() },
		"read":      func(conn *commonConn) { _, _ = conn.Read(make([]byte, 1)) },
		"write to":  func(conn *commonConn) { _, _ = conn.WriteTo(&bytes.Buffer{}) },
	}
	for name, operation := range operations {
		t.Run(name, func(t *testing.T) {
			conn := &commonConn{
				events:      &Events{},
				loop:        &eventLoop{},
				inboundTail: []byte("x"),
			}
			defer func() {
				if recover() == nil {
					t.Fatal("off-callback inbound access did not panic")
				}
			}()
			operation(conn)
		})
	}
}

func TestInboundAccessAllowsOwnerAndExternalCallbacks(t *testing.T) {
	events := &Events{}
	loop := &eventLoop{}
	conn := &commonConn{events: events, loop: loop, inboundTail: []byte("x")}

	loop.loopGoid.Store(currentGoroutineID())
	if conn.InboundBuffered() != 1 {
		t.Fatal("owner loop could not access inbound data")
	}
	loop.loopGoid.Store(0)

	id := events.enterExternalCallback()
	if conn.InboundBuffered() != 1 {
		t.Fatal("registered external callback could not access inbound data")
	}
	events.leaveExternalCallback(id)
}

func TestUserdataMayBeSerializedOutsideCallback(t *testing.T) {
	conn := &commonConn{events: &Events{}, loop: &eventLoop{}}
	conn.SetUserdata("value")
	if conn.Userdata() != "value" {
		t.Fatal("userdata was not available outside callback")
	}
}

func BenchmarkInboundAccessAssertion(b *testing.B) {
	events := &Events{}
	loop := &eventLoop{}
	conn := &commonConn{events: events, loop: loop}
	b.Run("owner-loop", func(b *testing.B) {
		loop.loopGoid.Store(currentGoroutineID())
		for b.Loop() {
			conn.InboundBuffered()
		}
		loop.loopGoid.Store(0)
	})
	b.Run("external-callback", func(b *testing.B) {
		id := events.enterExternalCallback()
		for b.Loop() {
			conn.InboundBuffered()
		}
		events.leaveExternalCallback(id)
	})
}
