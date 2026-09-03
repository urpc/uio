package uws

import (
	"net/http"
	"testing"
	"time"
)

func TestServerConnectionsShareFrozenConfig(t *testing.T) {
	handler := &recordingHandler{}
	protocols := []string{"v1"}
	server := &Server{
		Handler:                         handler,
		Subprotocols:                    protocols,
		MaxHeaderBytes:                  101,
		MaxFramePayload:                 102,
		MaxMessageSize:                  103,
		MaxOutboundBytes:                104,
		CloseTimeout:                    105 * time.Millisecond,
		HandshakeTimeout:                106 * time.Millisecond,
		EnableCompression:               true,
		CompressionLevel:                1,
		AllowCompressionContextTakeover: true,
		CheckOrigin:                     func(*http.Request) bool { return true },
	}
	server.config = newServerConnConfig(server)

	protocols[0] = "mutated"
	server.MaxFramePayload = 1
	server.MaxMessageSize = 1
	server.Handler = nil

	firstRaw := newScriptedConn()
	secondRaw := newScriptedConn()
	server.onOpen(firstRaw)
	server.onOpen(secondRaw)
	first := firstRaw.userdata.(*Conn)
	second := secondRaw.userdata.(*Conn)
	t.Cleanup(func() {
		first.stopHandshakeTimer()
		second.stopHandshakeTimer()
	})

	if first.config != server.config || second.config != server.config || first.config != second.config {
		t.Fatal("server connections did not share the startup config")
	}
	if first.handler != handler || first.config.subprotocols[0] != "v1" {
		t.Fatal("server connection observed configuration mutated after startup")
	}
	if first.maxFramePayload() != 102 || first.maxMessageSize() != 103 || first.maxOutboundBytes() != 104 {
		t.Fatal("server connection limits changed after startup")
	}
}

func TestDialerConnectionsShareFrozenConfig(t *testing.T) {
	dialer := &Dialer{
		Subprotocols:      []string{"v1"},
		MaxHeaderBytes:    101,
		MaxFramePayload:   102,
		MaxMessageSize:    103,
		MaxOutboundBytes:  104,
		CloseTimeout:      105 * time.Millisecond,
		HandshakeTimeout:  106 * time.Millisecond,
		EnableCompression: true,
		CompressionLevel:  1,
	}
	dialer.config = newDialerConnConfig(dialer)

	dialer.Subprotocols[0] = "mutated"
	dialer.MaxFramePayload = 1
	dialer.MaxMessageSize = 1
	first := dialer.newClientConn(nil, &dialSetup{})
	second := dialer.newClientConn(nil, &dialSetup{})

	if first.config != dialer.config || second.config != dialer.config || first.config != second.config {
		t.Fatal("dialer connections did not share the startup config")
	}
	if first.config.subprotocols[0] != "v1" || !first.isClient() {
		t.Fatal("dialer connection observed configuration mutated after startup")
	}
	if first.maxFramePayload() != 102 || first.maxMessageSize() != 103 || first.maxOutboundBytes() != 104 {
		t.Fatal("dialer connection limits changed after startup")
	}
}
