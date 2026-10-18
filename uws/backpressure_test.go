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
	"testing"
)

// newBackpressureConn pairs a probe transport with a server connection whose
// watermarks are small enough to cross by hand.
func newBackpressureConn(high, low int) (*limitedWire, *Conn, *Server) {
	server := NewServer(Options{OutboundHighWatermark: high, OutboundLowWatermark: low}, &batchEchoHandler{})
	raw := &limitedWire{}
	conn := &Conn{raw: raw, handler: &batchEchoHandler{}, config: testServerConfig(server)}
	conn.opened.Store(true)
	return raw, conn, server
}

// Queued output at the high watermark stops the read round without consuming;
// dropping below the low watermark resumes it and wakes the connection.
func TestOutboundHighWatermarkPausesReads(t *testing.T) {
	raw, conn, server := newBackpressureConn(64, 16)
	raw.inbound = maskedFrames("one", "two", "three")
	raw.queued = 64

	if err := conn.readAvailable(); err != nil {
		t.Fatal(err)
	}
	if !conn.readPaused.Load() {
		t.Fatal("reads were not paused at the high watermark")
	}
	if got := len(conn.raw.PeekChunk()); got == 0 {
		t.Fatal("the read round consumed input while over the watermark")
	}
	if stats := server.Stats(); stats.ReadsPaused != 1 || stats.ReadsResumed != 0 {
		t.Fatalf("stats = %+v, want one pause", stats)
	}

	// Still above the low watermark: no wake, no resume.
	raw.queued = 16
	conn.resumeReadsAfterDrain()
	if !conn.readPaused.Load() || raw.wakes != 0 {
		t.Fatalf("resumed at the low watermark: paused=%v wakes=%d", conn.readPaused.Load(), raw.wakes)
	}

	raw.queued = 8
	conn.resumeReadsAfterDrain()
	if conn.readPaused.Load() {
		t.Fatal("reads stayed paused below the low watermark")
	}
	if raw.wakes != 1 {
		t.Fatalf("wakes = %d, want 1", raw.wakes)
	}
	if stats := server.Stats(); stats.ReadsResumed != 1 {
		t.Fatalf("stats = %+v, want one resume", stats)
	}

	// A resumed round consumes what the pause left behind.
	if err := conn.readAvailable(); err != nil {
		t.Fatal(err)
	}
	if got := len(conn.raw.PeekChunk()); got != 0 {
		t.Fatalf("inbound left = %d bytes, want 0", got)
	}
}

// The pause needs no application help: the transport's inbound hold gets the
// same bound, so a paused connection's socket stops being read.
func TestBackpressureBoundsInbound(t *testing.T) {
	opts, err := Options{OutboundHighWatermark: 32 << 10}.resolve()
	if err != nil {
		t.Fatal(err)
	}
	if got := opts.transportEvents().MaxInboundBuffered; got != 32<<10 {
		t.Fatalf("MaxInboundBuffered = %d, want the high watermark", got)
	}
	opts, err = Options{OutboundHighWatermark: -1}.resolve()
	if err != nil {
		t.Fatal(err)
	}
	if got := opts.transportEvents().MaxInboundBuffered; got != 0 {
		t.Fatalf("MaxInboundBuffered = %d, want disabled", got)
	}
}

// A paused connection still delivers the frames it already read once the
// queue drains; nothing is dropped.
func TestBackpressureResumesWithFramesIntact(t *testing.T) {
	raw, conn, _ := newBackpressureConn(32, 8)
	raw.inbound = maskedFrames("alpha", "beta")
	raw.queued = 32
	if err := conn.readAvailable(); err != nil {
		t.Fatal(err)
	}
	pending := len(conn.raw.PeekChunk())
	if pending == 0 {
		t.Fatal("the read round consumed input while over the watermark")
	}
	raw.queued = 0
	conn.resumeReadsAfterDrain()
	if err := conn.readAvailable(); err != nil {
		t.Fatal(err)
	}
	if got := len(conn.raw.PeekChunk()); got != 0 {
		t.Fatalf("inbound left = %d bytes, want 0", got)
	}
	frames := raw.frames(t)
	if len(frames) != 2 || string(frames[0].Payload) != "alpha" || string(frames[1].Payload) != "beta" {
		t.Fatalf("replies = %d, want the two echoed frames", len(frames))
	}
}

// WriteCoalesce disabled stays disabled through resolution; the transport
// keeps receiving the negative sentinel.
func TestWriteCoalesceSentinelPreserved(t *testing.T) {
	opts, err := Options{WriteCoalesce: -1}.resolve()
	if err != nil {
		t.Fatal(err)
	}
	if got := opts.transportEvents().WriteBufferedThreshold; got != -1 {
		t.Fatalf("WriteBufferedThreshold = %d, want -1", got)
	}
}
