/*
 * Copyright 2024 the urpc project
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *      https://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package uws

import (
	"errors"
	"net/http"
	"testing"
	"time"
)

func TestOptionsResolveDefaults(t *testing.T) {
	opts, err := (Options{}).resolve()
	if err != nil {
		t.Fatal(err)
	}
	if opts.MaxHeaderBytes != DefaultMaxHeaderBytes || opts.MaxFramePayload != DefaultMaxFramePayload ||
		opts.MaxMessageSize != DefaultMaxMessageSize ||
		opts.HandshakeTimeout != DefaultHandshakeTimeout || opts.CloseTimeout != DefaultCloseTimeout {
		t.Fatalf("defaults = %+v", opts)
	}
	if opts.WriteCoalesce != defaultWriteBufferedThreshold {
		t.Fatalf("WriteCoalesce = %d, want %d", opts.WriteCoalesce, defaultWriteBufferedThreshold)
	}
	if opts.OutboundHighWatermark != DefaultOutboundHighWatermark ||
		opts.OutboundLowWatermark != DefaultOutboundHighWatermark/4 {
		t.Fatalf("watermarks = %d/%d", opts.OutboundHighWatermark, opts.OutboundLowWatermark)
	}
	events := opts.transportEvents()
	if events.MaxInboundBuffered != DefaultOutboundHighWatermark {
		t.Fatalf("MaxInboundBuffered = %d", events.MaxInboundBuffered)
	}
	if events.WriteBufferedThreshold != defaultWriteBufferedThreshold {
		t.Fatalf("transport WriteBufferedThreshold = %d", events.WriteBufferedThreshold)
	}
}

func TestOptionsAreNotMutated(t *testing.T) {
	original := Options{HeartbeatInterval: time.Second}
	if _, err := original.resolve(); err != nil {
		t.Fatal(err)
	}
	if original.HeartbeatTimeout != 0 || original.WriteCoalesce != 0 || original.OutboundHighWatermark != 0 {
		t.Fatalf("resolve mutated its receiver: %+v", original)
	}
}

func TestOptionsValidation(t *testing.T) {
	for _, test := range []struct {
		name string
		opts Options
	}{
		{"negative header limit", Options{MaxHeaderBytes: -1}},
		{"negative timeout", Options{HandshakeTimeout: -time.Second}},
		{"negative close timeout", Options{CloseTimeout: -time.Second}},
		{"negative heartbeat", Options{HeartbeatInterval: -time.Second}},
		{"negative pollers", Options{Pollers: -1}},
		{"negative read buffer", Options{ReadBufferSize: -1}},
		{"negative low watermark", Options{OutboundHighWatermark: 64, OutboundLowWatermark: -1}},
		{"low at high", Options{OutboundHighWatermark: 64, OutboundLowWatermark: 64}},
		{"compression level range", Options{EnableCompression: true, CompressionLevel: 10}},
		{"level without compression", Options{CompressionLevel: 1}},
	} {
		if _, err := test.opts.resolve(); !errors.Is(err, ErrInvalidOptions) {
			t.Fatalf("%s: error = %v, want %v", test.name, err, ErrInvalidOptions)
		}
	}
}

type trafficObserver struct {
	inbound  int
	outbound int
}

func (o *trafficObserver) OnOpen(*Conn)              {}
func (o *trafficObserver) OnMessage(*Conn, Message)  {}
func (o *trafficObserver) OnClose(*Conn, CloseEvent) {}
func (o *trafficObserver) OnInbound(_ *Conn, n int)  { o.inbound += n }
func (o *trafficObserver) OnOutbound(_ *Conn, n int) { o.outbound += n }

// A Handler that implements TrafficObserver sees both directions of transport
// traffic; the internal accounting still runs first.
func TestTrafficObserverReceivesBothDirections(t *testing.T) {
	observer := &trafficObserver{}
	server := NewServer(Options{Handler: observer, OutboundHighWatermark: -1})
	raw := newScriptedConn()
	conn := &Conn{raw: raw, handler: observer, config: testServerConfig(server)}
	raw.userdata = conn

	server.onInbound(raw, 123)
	server.onOutbound(raw, 45)
	if observer.inbound != 123 || observer.outbound != 45 {
		t.Fatalf("observer saw %d/%d bytes", observer.inbound, observer.outbound)
	}
	if pending := conn.writes.close.pendingBytes.Load(); pending != 0 {
		t.Fatalf("pending = %d", pending)
	}
}

// The watermarks keep their documented meaning through resolution: a negative
// high watermark disables the read pause, and only zero selects the 64 KiB
// default. The resolved values reach the transport and every connection
// configuration unchanged.
func TestOutboundWatermarkResolution(t *testing.T) {
	disabled, err := Options{OutboundHighWatermark: -1}.resolve()
	if err != nil {
		t.Fatal(err)
	}
	if disabled.OutboundHighWatermark != 0 || disabled.OutboundLowWatermark != 0 {
		t.Fatalf("negative high watermark resolved to %d/%d, want 0/0 (pause disabled)",
			disabled.OutboundHighWatermark, disabled.OutboundLowWatermark)
	}
	if got := disabled.transportEvents().MaxInboundBuffered; got != 0 {
		t.Fatalf("transport inbound bound = %d, want 0 (pause disabled)", got)
	}
	if cfg := newServerConnConfig(disabled, nil, nil, &statsCounters{}); cfg.outboundHigh != 0 {
		t.Fatalf("server connection high watermark = %d, want 0", cfg.outboundHigh)
	}
	if cfg := newDialerConnConfig(disabled, &statsCounters{}); cfg.outboundHigh != 0 {
		t.Fatalf("dialer connection high watermark = %d, want 0", cfg.outboundHigh)
	}

	fallback, err := Options{}.resolve()
	if err != nil {
		t.Fatal(err)
	}
	if fallback.OutboundHighWatermark != DefaultOutboundHighWatermark ||
		fallback.OutboundLowWatermark != DefaultOutboundHighWatermark/4 {
		t.Fatalf("zero high watermark resolved to %d/%d, want the %d/%d default",
			fallback.OutboundHighWatermark, fallback.OutboundLowWatermark,
			DefaultOutboundHighWatermark, DefaultOutboundHighWatermark/4)
	}
	if got := fallback.transportEvents().MaxInboundBuffered; got != DefaultOutboundHighWatermark {
		t.Fatalf("transport inbound bound = %d, want the %d default", got, DefaultOutboundHighWatermark)
	}
}

func TestServerConnectionsShareFrozenConfig(t *testing.T) {
	handler := &recordingHandler{}
	protocols := []string{"v1"}
	server := &Server{Options: Options{Handler: handler, Subprotocols: protocols, MaxHeaderBytes: 101, MaxFramePayload: 102, MaxMessageSize: 103, CloseTimeout: 105 * time.Millisecond, HandshakeTimeout: 106 * time.Millisecond, EnableCompression: true, CompressionLevel: 1, KeepCompressionContext: true}, CheckOrigin: func(*http.Request) bool { return true }}
	server.config = testServerConfig(server)

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
	if first.maxFramePayload() != 102 || first.maxMessageSize() != 103 {
		t.Fatal("server connection limits changed after startup")
	}
}

func TestDialerConnectionsShareFrozenConfig(t *testing.T) {
	dialer := &Dialer{Options: Options{Subprotocols: []string{"v1"}, MaxHeaderBytes: 101, MaxFramePayload: 102, MaxMessageSize: 103, CloseTimeout: 105 * time.Millisecond, HandshakeTimeout: 106 * time.Millisecond, EnableCompression: true, CompressionLevel: 1}}
	dialer.config = testDialerConfig(dialer)

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
	if first.maxFramePayload() != 102 || first.maxMessageSize() != 103 {
		t.Fatal("dialer connection limits changed after startup")
	}
}
