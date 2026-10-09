//go:build race

package uio

import (
	"bytes"
	"testing"
)

// The inbound-scope diagnostics run only in race-enabled builds; these pin
// their behavior there. See conn_access_release_test.go for the build without
// them.

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

func TestInboundAccessAllowsCurrentConnectionCallback(t *testing.T) {
	events := &Events{}
	loop := &eventLoop{}
	conn := &commonConn{events: events, loop: loop, inboundTail: []byte("x")}

	started := conn.beginInboundCallback()
	if conn.InboundBuffered() != 1 {
		t.Fatal("current connection callback could not access inbound data")
	}
	conn.endInboundCallback(started)
	defer func() {
		if recover() == nil {
			t.Fatal("inbound access after callback did not panic")
		}
	}()
	conn.InboundBuffered()
}

func TestInboundAccessIsConnectionScoped(t *testing.T) {
	events := &Events{}
	loop := &eventLoop{}
	first := &commonConn{events: events, loop: loop, inboundTail: []byte("a")}
	second := &commonConn{events: events, loop: loop, inboundTail: []byte("b")}
	started := first.beginInboundCallback()
	defer first.endInboundCallback(started)
	if first.InboundBuffered() != 1 {
		t.Fatal("own inbound access was rejected")
	}
	defer func() {
		if recover() == nil {
			t.Fatal("cross-connection inbound access did not panic")
		}
	}()
	second.InboundBuffered()
}
