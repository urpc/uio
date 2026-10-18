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
	server := NewServer(Options{OutboundHighWatermark: -1}, observer)
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
