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
	raw.queued = 17
	conn.resumeReadsAfterDrain()
	if !conn.readPaused.Load() || raw.wakes != 0 {
		t.Fatalf("resumed above the low watermark: paused=%v wakes=%d", conn.readPaused.Load(), raw.wakes)
	}

	// At the low watermark reads resume.
	raw.queued = 16
	conn.resumeReadsAfterDrain()
	if conn.readPaused.Load() {
		t.Fatal("reads stayed paused at the low watermark")
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

// A high watermark under four leaves a low watermark of zero; a drained queue
// must still resume reads.
func TestZeroLowWatermarkResumes(t *testing.T) {
	raw, conn, _ := newBackpressureConn(3, 0)
	if conn.config.outboundLow != 0 {
		t.Fatalf("low watermark = %d, want 0", conn.config.outboundLow)
	}
	raw.inbound = maskedFrames("one")
	raw.queued = 3
	if err := conn.readAvailable(); err != nil {
		t.Fatal(err)
	}
	if !conn.readPaused.Load() {
		t.Fatal("reads were not paused at the high watermark")
	}
	raw.queued = 0
	conn.resumeReadsAfterDrain()
	if conn.readPaused.Load() || raw.wakes != 1 {
		t.Fatalf("a drained queue left reads paused: paused=%v wakes=%d", conn.readPaused.Load(), raw.wakes)
	}
}

// drainingWire reports the high watermark once, and an empty queue after,
// as if the writer drained everything between the read round's check and its
// pause; it runs the outbound callback's resume at that moment, which finds
// nothing paused yet.
type drainingWire struct {
	*limitedWire
	conn  *Conn
	calls int
	high  int
}

func (w *drainingWire) OutboundBuffered() int {
	w.calls++
	if w.calls == 1 {
		// The read round sees the high watermark; before it publishes the
		// pause the writer drains the queue and its callback finds nothing
		// paused yet.
		w.conn.resumeReadsAfterDrain()
		return w.high
	}
	return 0
}

// The writer draining the queue between the read round's watermark check and
// the pause must not leave the connection paused with nothing left to wake
// it: the round sees the drained queue and keeps reading.
func TestPauseRacingADrainDoesNotStrandReads(t *testing.T) {
	server := NewServer(Options{OutboundHighWatermark: 64, OutboundLowWatermark: 16}, &batchEchoHandler{})
	inner := &limitedWire{}
	raw := &drainingWire{limitedWire: inner, high: 64}
	conn := &Conn{raw: raw, handler: &batchEchoHandler{}, config: testServerConfig(server)}
	raw.conn = conn
	conn.opened.Store(true)
	inner.inbound = maskedFrames("one", "two")

	if err := conn.readAvailable(); err != nil {
		t.Fatal(err)
	}
	if conn.readPaused.Load() {
		t.Fatalf("reads stayed paused after the queue drained: wakes=%d", inner.wakes)
	}
	if got := len(inner.inbound); got != 0 {
		t.Fatalf("inbound left = %d bytes, want the round to keep reading", got)
	}
	if stats := server.Stats(); stats.ReadsPaused != 0 {
		t.Fatalf("stats = %+v, want no pause counted", stats)
	}
}

// Output sent from outside the connection's reads, such as a broadcast, is
// bounded by MaxOutboundBuffered: past it a send fails and queues nothing.
func TestMaxOutboundBufferedBoundsBackgroundSends(t *testing.T) {
	const limit = 64 << 10
	raw := &limitedWire{limit: limit}
	server := NewServer(Options{MaxOutboundBuffered: limit}, &batchEchoHandler{})
	conn := &Conn{raw: raw, handler: &batchEchoHandler{}, config: testServerConfig(server)}
	conn.opened.Store(true)
	payload := make([]byte, 8<<10)
	refused := 0
	for range 128 {
		if err := conn.SendBinary(payload); err != nil {
			if !errors.Is(err, ErrBackpressure) {
				t.Fatalf("send error = %v, want ErrBackpressure", err)
			}
			refused++
		}
	}
	if refused == 0 {
		t.Fatal("no send was refused past MaxOutboundBuffered")
	}
	if queued := raw.queuedBytes(); queued > limit {
		t.Fatalf("queued = %d bytes, want at most %d", queued, limit)
	}
	opts, err := Options{MaxOutboundBuffered: limit}.resolve()
	if err != nil {
		t.Fatal(err)
	}
	if got := opts.transportEvents().MaxOutboundBuffered; got != limit {
		t.Fatalf("transport MaxOutboundBuffered = %d, want %d", got, limit)
	}
}

// A paused connection stays paused until its queue drains to the low
// watermark, whatever runs its next read round: input that arrives while the
// queue sits between the watermarks is left unconsumed.
func TestPausedReadsWaitForTheLowWatermark(t *testing.T) {
	raw, conn, server := newBackpressureConn(64, 16)
	raw.inbound = maskedFrames("one")
	raw.queued = 64
	if err := conn.readAvailable(); err != nil {
		t.Fatal(err)
	}
	if !conn.readPaused.Load() {
		t.Fatal("reads were not paused at the high watermark")
	}

	// More input arrives and runs a round while the queue is between the
	// watermarks: still paused.
	raw.queued = 40
	raw.inbound = append(raw.inbound, maskedFrames("two")...)
	held := len(raw.inbound)
	if err := conn.readAvailable(); err != nil {
		t.Fatal(err)
	}
	if !conn.readPaused.Load() {
		t.Fatal("a round between the watermarks lifted the pause")
	}
	if got := len(raw.inbound); got != held {
		t.Fatalf("a round between the watermarks consumed %d bytes", held-got)
	}

	// At the low watermark a round resumes on its own and consumes.
	raw.queued = 16
	if err := conn.readAvailable(); err != nil {
		t.Fatal(err)
	}
	if conn.readPaused.Load() {
		t.Fatal("reads stayed paused at the low watermark")
	}
	if got := len(raw.inbound); got != 0 {
		t.Fatalf("inbound left = %d bytes, want 0", got)
	}
	if stats := server.Stats(); stats.ReadsPaused != 1 || stats.ReadsResumed != 1 {
		t.Fatalf("stats = %+v, want one pause and one resume", stats)
	}
}
