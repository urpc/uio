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
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urpc/uio"
	"github.com/urpc/uio/uws/internal/compress"
	"github.com/urpc/uio/uws/internal/frame"
)

// newBackpressureConn pairs a probe transport with a server connection whose
// watermarks are small enough to cross by hand.
func newBackpressureConn(high, low int) (*limitedWire, *Conn, *Server) {
	server := NewServer(Options{Handler: &batchEchoHandler{}, OutboundHighWatermark: high, OutboundLowWatermark: low})
	raw := &limitedWire{}
	conn := &Conn{raw: raw, handler: &batchEchoHandler{}, config: testServerConfig(server)}
	conn.opened.Store(true)
	return raw, conn, server
}

// queueReplies adds n bytes of the connection's own replies to the probe's
// queue, as a read round's Handler sending them would: they count toward the
// pause until the transport reports them sent.
func queueReplies(raw *limitedWire, conn *Conn, n int) {
	raw.queued += n
	conn.writes.mu.Lock()
	defer conn.writes.mu.Unlock()
	replies := conn.replies.Load()
	if replies == nil {
		replies = newReplyLedger()
		conn.replies.Store(replies)
	}
	replies.add(int64(n), conn.queueEnd(replies))
}

// drainTo empties the probe's queue down to n bytes and reports what left,
// as the transport's outbound callback does.
func drainTo(raw *limitedWire, conn *Conn, n int) {
	sent := raw.queued - n
	raw.queued = n
	conn.transportSent(sent)
}

// ownQueued is how many of the connection's reply bytes it counts as queued.
func ownQueued(conn *Conn) int64 {
	conn.writes.mu.Lock()
	defer conn.writes.mu.Unlock()
	if replies := conn.replies.Load(); replies != nil {
		return replies.queued()
	}
	return 0
}

// pendingBatchReplies observes reply accounting only from the serialized test
// read path, just as the protocol's batch handoff does under writes.mu.
func pendingBatchReplies(conn *Conn) int64 {
	if replies := conn.replies.Load(); replies != nil {
		return replies.batched
	}
	return 0
}

// Replies queued to the high watermark stop the read round without consuming;
// dropping below the low watermark resumes it and wakes the connection.
func TestOutboundHighWatermarkPausesReads(t *testing.T) {
	raw, conn, server := newBackpressureConn(64, 16)
	raw.inbound = maskedFrames("one", "two", "three")
	queueReplies(raw, conn, 64)

	if err := conn.readAvailable(); err != nil {
		t.Fatal(err)
	}
	if !conn.internalReadsPaused() {
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
	if !conn.internalReadsPaused() || raw.wakes != 0 {
		t.Fatalf("resumed above the low watermark: paused=%v wakes=%d", conn.internalReadsPaused(), raw.wakes)
	}

	// At the low watermark reads resume.
	raw.queued = 16
	conn.resumeReadsAfterDrain()
	if conn.internalReadsPaused() {
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
	if got := opts.transportEvents().MaxInboundBuffered; got != DefaultOutboundHighWatermark {
		t.Fatalf("MaxInboundBuffered = %d, want the manual pause's receive bound", got)
	}
}

// A paused connection still delivers the frames it already read once the
// queue drains; nothing is dropped.
func TestBackpressureResumesWithFramesIntact(t *testing.T) {
	raw, conn, _ := newBackpressureConn(32, 8)
	raw.inbound = maskedFrames("alpha", "beta")
	queueReplies(raw, conn, 32)
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
	queueReplies(raw, conn, 3)
	if err := conn.readAvailable(); err != nil {
		t.Fatal(err)
	}
	if !conn.internalReadsPaused() {
		t.Fatal("reads were not paused at the high watermark")
	}
	raw.queued = 0
	conn.resumeReadsAfterDrain()
	if conn.internalReadsPaused() || raw.wakes != 1 {
		t.Fatalf("a drained queue left reads paused: paused=%v wakes=%d", conn.internalReadsPaused(), raw.wakes)
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
	server := NewServer(Options{Handler: &batchEchoHandler{}, OutboundHighWatermark: 64, OutboundLowWatermark: 16})
	inner := &limitedWire{}
	raw := &drainingWire{limitedWire: inner, high: 64}
	conn := &Conn{raw: raw, handler: &batchEchoHandler{}, config: testServerConfig(server)}
	raw.conn = conn
	conn.opened.Store(true)
	// The round's own replies reached the watermark.
	replies := newReplyLedger()
	replies.add(64, 64)
	conn.replies.Store(replies)
	inner.inbound = maskedFrames("one", "two")

	if err := conn.readAvailable(); err != nil {
		t.Fatal(err)
	}
	if conn.internalReadsPaused() {
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
	server := NewServer(Options{Handler: &batchEchoHandler{}, MaxOutboundBuffered: limit})
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
	queueReplies(raw, conn, 64)
	if err := conn.readAvailable(); err != nil {
		t.Fatal(err)
	}
	if !conn.internalReadsPaused() {
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
	if !conn.internalReadsPaused() {
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
	if conn.internalReadsPaused() {
		t.Fatal("reads stayed paused at the low watermark")
	}
	if got := len(raw.inbound); got != 0 {
		t.Fatalf("inbound left = %d bytes, want 0", got)
	}
	if stats := server.Stats(); stats.ReadsPaused != 1 || stats.ReadsResumed != 1 {
		t.Fatalf("stats = %+v, want one pause and one resume", stats)
	}
}

type echoAllHandler struct{}

func (echoAllHandler) OnOpen(*Conn)                 {}
func (echoAllHandler) OnMessage(c *Conn, m Message) { _ = c.SendBinary(m.Bytes()) }
func (echoAllHandler) OnClose(*Conn, CloseEvent)    {}

// readServerFrame reads one unmasked frame from the server.
func readEchoFrame(reader *bufio.Reader) ([]byte, error) {
	var header [2]byte
	if _, err := io.ReadFull(reader, header[:]); err != nil {
		return nil, err
	}
	length := int(header[1] & 0x7f)
	switch length {
	case 126:
		var ext [2]byte
		if _, err := io.ReadFull(reader, ext[:]); err != nil {
			return nil, err
		}
		length = int(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err := io.ReadFull(reader, ext[:]); err != nil {
			return nil, err
		}
		length = int(binary.BigEndian.Uint64(ext[:]))
	}
	payload := make([]byte, length)
	_, err := io.ReadFull(reader, payload)
	return payload, err
}

// A peer that stops reading its replies makes the server pause that
// connection's reads once the outbound watermark is crossed. Reading again
// drains the queue, the pause lifts, and every message is echoed exactly once.
func TestSlowPeerBackpressureKeepsEveryMessage(t *testing.T) {
	const (
		// Enough traffic that kernel socket buffers cannot absorb it: the
		// server must reach its watermark and stop reading.
		messages    = 8192
		payloadSize = 8 << 10
		high        = 32 << 10
	)
	server, addr, serveDone := startTestServer(t, func() *Server {
		return NewServer(Options{Handler: echoAllHandler{},
			Pollers:               1,
			ReadBufferSize:        4 << 10,
			OutboundHighWatermark: high,
			OutboundLowWatermark:  high / 4,
		})
	})
	t.Cleanup(func() { stopTestServer(t, server, serveDone) })

	var conn net.Conn
	var err error
	for deadline := time.Now().Add(testIOTimeout()); conn == nil && time.Now().Before(deadline); {
		conn, err = net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err != nil {
			time.Sleep(time.Millisecond)
		}
	}
	if conn == nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	if tcp, ok := conn.(*net.TCPConn); ok {
		// Keep the client's receive buffer small so the server's replies back
		// up quickly instead of disappearing into kernel buffers.
		_ = tcp.SetReadBuffer(64 << 10)
	}

	request := "GET / HTTP/1.1\r\nHost: " + addr + "\r\n" +
		"Connection: Upgrade\r\nUpgrade: websocket\r\n" +
		"Sec-WebSocket-Version: 13\r\nSec-WebSocket-Key: " + testKey + "\r\n\r\n"
	if _, err := conn.Write([]byte(request)); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(conn)
	if line, err := reader.ReadString('\n'); err != nil || len(line) < 12 || line[:12] != "HTTP/1.1 101" {
		t.Fatalf("handshake response = %q, %v", line, err)
	}
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if line == "\r\n" {
			break
		}
	}

	// Write requests without reading a single reply.
	writeDone := make(chan error, 1)
	var sent atomic.Int64
	go func() {
		maskKey := [4]byte{9, 8, 7, 6}
		var wire []byte
		for i := 0; i < messages; i++ {
			payload := make([]byte, payloadSize)
			for j := range payload {
				payload[j] = byte(i)
			}
			wire = frame.Append(wire[:0], frame.Frame{Fin: true, Opcode: frame.Binary, Masked: true, Payload: payload}, maskKey)
			if _, err := conn.Write(wire); err != nil {
				writeDone <- err
				return
			}
			sent.Add(1)
		}
		writeDone <- nil
	}()

	// The server must stop reading once its queued output crosses the
	// watermark, long before all requests are in.
	pausedBy := time.Now().Add(testIOTimeout())
	for {
		if server.Stats().ReadsPaused > 0 {
			break
		}
		if time.Now().After(pausedBy) {
			stats := server.Stats()
			t.Fatalf("reads were never paused: %+v", stats)
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Drain the replies: the pause lifts and every request is answered once.
	// The deadline bounds each frame, not the whole transfer: a loaded runner
	// can take longer than one timeout to move 64 MiB while making progress,
	// and a stalled stream still fails within one timeout. The overall cap
	// keeps a trickle from holding the test forever.
	start := time.Now()
	overall := start.Add(10 * testIOTimeout())
	for i := 0; i < messages; i++ {
		if err := conn.SetReadDeadline(time.Now().Add(testIOTimeout())); err != nil {
			t.Fatal(err)
		}
		payload, err := readEchoFrame(reader)
		if err != nil {
			stats := server.Stats()
			t.Fatalf("reply %d after %v (sent %d/%d, reads paused %d resumed %d): %v",
				i, time.Since(start), sent.Load(), messages, stats.ReadsPaused, stats.ReadsResumed, err)
		}
		if time.Now().After(overall) {
			stats := server.Stats()
			t.Fatalf("transfer exceeded %v at reply %d (sent %d/%d, reads paused %d resumed %d)",
				10*testIOTimeout(), i, sent.Load(), messages, stats.ReadsPaused, stats.ReadsResumed)
		}
		if len(payload) != payloadSize {
			t.Fatalf("reply %d length = %d, want %d", i, len(payload), payloadSize)
		}
		if payload[0] != byte(i) || payload[len(payload)-1] != byte(i) {
			t.Fatalf("reply %d payload = %x..%x, want %x", i, payload[0], payload[len(payload)-1], byte(i))
		}
	}
	select {
	case err := <-writeDone:
		if err != nil {
			t.Fatalf("client write: %v", err)
		}
	case <-time.After(testIOTimeout()):
		t.Fatal("client did not finish writing")
	}
	if stats := server.Stats(); stats.ReadsResumed == 0 {
		t.Fatalf("reads never resumed: %+v", stats)
	}
}

// batchProbeConn records every byte handed to the transport, in order, and
// how many hand-offs carried them.
type batchProbeConn struct {
	bufferedProbeConn
	wire     bytes.Buffer
	handoffs int
	outbound int
}

func (c *batchProbeConn) WriteOwned(buffer *uio.Buffer) (int, error) {
	c.handoffs++
	n, _ := c.wire.Write(buffer.Bytes())
	uio.ReleaseBuffer(buffer)
	return n, nil
}

func (c *batchProbeConn) Writev(vec [][]byte) (int, error) {
	c.handoffs++
	total := 0
	for _, segment := range vec {
		n, _ := c.wire.Write(segment)
		total += n
	}
	return total, nil
}

func (c *batchProbeConn) OutboundBuffered() int { return c.outbound }

func (c *batchProbeConn) payloads(t *testing.T) []string {
	t.Helper()
	reader := bufio.NewReader(bytes.NewReader(c.wire.Bytes()))
	var payloads []string
	for reader.Buffered() > 0 || c.wire.Len() > 0 {
		_, payload, _, err := readServerFrame(reader)
		if err != nil {
			break
		}
		payloads = append(payloads, string(payload))
	}
	return payloads
}

type batchEchoHandler struct {
	onMessage func(*Conn, Message)
}

func (*batchEchoHandler) OnOpen(*Conn) {}

func (h *batchEchoHandler) OnMessage(conn *Conn, message Message) {
	if h.onMessage != nil {
		h.onMessage(conn, message)
		return
	}
	_ = conn.SendBinary(message.Bytes())
}

func (*batchEchoHandler) OnClose(*Conn, CloseEvent) {}

func maskedFrames(payloads ...string) []byte {
	var wire []byte
	for _, payload := range payloads {
		wire = frame.Append(wire, frame.Frame{
			Fin: true, Opcode: frame.Binary, Masked: true, Payload: []byte(payload),
		}, [4]byte{9, 8, 7, 6})
	}
	return wire
}

func newBatchConn(raw uio.Conn, handler Handler) *Conn {
	conn := &Conn{raw: raw, handler: handler, config: testServerConfig(NewServer(Options{OutboundHighWatermark: -1}))}
	conn.opened.Store(true)
	return conn
}

func TestReadRoundBatchesRepliesIntoOneHandoff(t *testing.T) {
	raw := &batchProbeConn{}
	conn := newBatchConn(raw, &batchEchoHandler{})
	raw.inbound = maskedFrames("a", "bb", "ccc", "dddd")
	if err := conn.readAvailable(); err != nil {
		t.Fatal(err)
	}
	if raw.handoffs != 1 {
		t.Fatalf("hand-offs = %d, want 1", raw.handoffs)
	}
	if got := raw.payloads(t); len(got) != 4 || got[0] != "a" || got[3] != "dddd" {
		t.Fatalf("replies = %q", got)
	}
	if conn.batch != nil || conn.round.Load()&roundBatching != 0 {
		t.Fatal("read round left its write batch open")
	}
}

func TestSingleFrameRoundDoesNotBatch(t *testing.T) {
	raw := &batchProbeConn{}
	conn := newBatchConn(raw, &batchEchoHandler{})
	raw.inbound = maskedFrames("only")
	if err := conn.readAvailable(); err != nil {
		t.Fatal(err)
	}
	if conn.round.Load()&roundBatching != 0 || conn.batch != nil {
		t.Fatal("a single frame opened a write batch")
	}
	if got := raw.payloads(t); len(got) != 1 || got[0] != "only" {
		t.Fatalf("replies = %q", got)
	}
}

func TestBatchKeepsOrderWithOtherSenders(t *testing.T) {
	raw := &batchProbeConn{}
	handler := &batchEchoHandler{}
	conn := newBatchConn(raw, handler)
	handler.onMessage = func(conn *Conn, message Message) {
		_ = conn.SendBinary(message.Bytes())
		if string(message.Payload) == "first" {
			// A sender on another goroutine joins the batch in order.
			done := make(chan error, 1)
			go func() { done <- conn.SendBinary([]byte("external")) }()
			if err := <-done; err != nil {
				t.Errorf("external send = %v", err)
			}
		}
		if string(message.Payload) == "second" {
			// A flush hands everything accepted so far over at once.
			if err := conn.Ping([]byte("p")); err != nil {
				t.Errorf("ping = %v", err)
			}
		}
	}
	raw.inbound = maskedFrames("first", "second", "third")
	if err := conn.readAvailable(); err != nil {
		t.Fatal(err)
	}
	got := raw.payloads(t)
	want := []string{"first", "external", "second", "p", "third"}
	if len(got) != len(want) {
		t.Fatalf("replies = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("replies = %q, want %q", got, want)
		}
	}
}

func TestBatchRespectsOutboundLimit(t *testing.T) {
	raw := &batchProbeConn{outbound: 40}
	server := NewServer(Options{})
	server.Options.MaxOutboundBuffered = 64
	server.Options.OutboundHighWatermark = -1 // refusal paths, not the read pause
	conn := &Conn{raw: raw, handler: &batchEchoHandler{}, config: testServerConfig(server)}
	conn.opened.Store(true)
	raw.inbound = maskedFrames("0123456789", "0123456789", "0123456789")
	if err := conn.readAvailable(); err != nil {
		t.Fatal(err)
	}
	// Each reply is 12 bytes on the wire. With 40 already queued, two fit the
	// 64-byte limit; the third would not, so it hands the batch over and goes
	// the direct way, where the transport enforces the limit for it alone.
	if raw.handoffs != 2 {
		t.Fatalf("hand-offs = %d, want 2", raw.handoffs)
	}
	if got := raw.payloads(t); len(got) != 3 {
		t.Fatalf("replies = %q", got)
	}
	if conn.batch != nil {
		t.Fatal("batch left behind")
	}
}

// failingBatchConn refuses every hand-off, as a transport that broke would.
type failingBatchConn struct {
	batchProbeConn
	err error
}

func (c *failingBatchConn) WriteOwned(buffer *uio.Buffer) (int, error) {
	if errors.Is(c.err, uio.ErrOutboundOverflow) {
		// An overflowed buffer stays the caller's, like the real transport.
		return 0, c.err
	}
	uio.ReleaseBuffer(buffer)
	return 0, c.err
}

func TestFailedBatchHandoffAbortsTransport(t *testing.T) {
	raw := &failingBatchConn{err: errors.New("transport broke")}
	conn := &Conn{raw: raw, handler: &batchEchoHandler{}, config: testServerConfig(NewServer(Options{}))}
	conn.opened.Store(true)
	raw.inbound = maskedFrames("a", "b")
	if err := conn.readAvailable(); err != nil {
		t.Fatal(err)
	}
	if raw.closes != 1 || !conn.closing.Load() {
		t.Fatalf("failed hand-off = closes:%d closing:%v, want 1/true", raw.closes, conn.closing.Load())
	}
	if info := conn.closeInfo(); !errors.Is(info.Err, raw.err) {
		t.Fatalf("close error = %v, want %v", info.Err, raw.err)
	}
	if pending := conn.writes.close.pendingBytes.Load(); pending != 0 {
		t.Fatalf("refused batch left %d pending bytes", pending)
	}
}

func TestClosedTransportBatchHandoffDoesNotAbortAgain(t *testing.T) {
	raw := &failingBatchConn{err: net.ErrClosed}
	conn := &Conn{raw: raw, handler: &batchEchoHandler{}, config: testServerConfig(NewServer(Options{}))}
	conn.opened.Store(true)
	raw.inbound = maskedFrames("a", "b")
	if err := conn.readAvailable(); err != nil {
		t.Fatal(err)
	}
	if raw.closes != 0 {
		t.Fatalf("closed transport was closed again %d times", raw.closes)
	}
}

func pingWire(payload string) []byte {
	return frame.Append(nil, frame.Frame{
		Fin: true, Opcode: frame.Ping, Masked: true, Payload: []byte(payload),
	}, [4]byte{9, 8, 7, 6})
}

// wireFrames decodes the probe's stream into (opcode, payload) pairs.
func wireFrames(t *testing.T, raw *batchProbeConn) []frame.Frame {
	t.Helper()
	reader := bufio.NewReader(bytes.NewReader(raw.wire.Bytes()))
	var frames []frame.Frame
	for reader.Buffered() > 0 || raw.wire.Len() > 0 {
		first, payload, fin, err := readServerFrame(reader)
		if err != nil {
			if len(frames) == 0 {
				t.Fatalf("decode wire: %v", err)
			}
			break
		}
		frames = append(frames, frame.Frame{Opcode: frame.OpCode(first & 0x0f), Payload: payload, Fin: fin})
	}
	return frames
}

// limitProbeConn refuses hand-offs while at its outbound limit, keeping the
// buffer like the real transport does on ErrOutboundOverflow.
type limitProbeConn struct {
	batchProbeConn
	atLimit bool
}

func (c *limitProbeConn) WriteOwned(buffer *uio.Buffer) (int, error) {
	if c.atLimit {
		return 0, uio.ErrOutboundOverflow
	}
	return c.batchProbeConn.WriteOwned(buffer)
}

// A Pong a round sends joins its batch in order: it can neither overtake the
// replies accepted before it nor fall behind later ones.
func TestPongKeepsItsPlaceInTheBatch(t *testing.T) {
	raw := &batchProbeConn{}
	conn := newBatchConn(raw, &batchEchoHandler{})
	raw.inbound = append(append(maskedFrames("data"), pingWire("k")...), maskedFrames("tail")...)
	if err := conn.readAvailable(); err != nil {
		t.Fatal(err)
	}
	frames := wireFrames(t, raw)
	want := []frame.OpCode{frame.Binary, frame.Pong, frame.Binary}
	if len(frames) != len(want) {
		t.Fatalf("wire = %v", frames)
	}
	for i, opcode := range want {
		if frames[i].Opcode != opcode {
			t.Fatalf("wire frame %d opcode = %v, want %v", i, frames[i].Opcode, opcode)
		}
	}
	if string(frames[0].Payload) != "data" || string(frames[1].Payload) != "k" || string(frames[2].Payload) != "tail" {
		t.Fatalf("wire payloads = %q %q %q", frames[0].Payload, frames[1].Payload, frames[2].Payload)
	}
	if raw.closes != 0 || conn.closing.Load() || conn.batch != nil {
		t.Fatalf("closes:%d closing:%v batch:%v, want a clean ordered round", raw.closes, conn.closing.Load(), conn.batch != nil)
	}
}

// A round whose reply finds another goroutine mid-submit waits for it instead
// of failing, and still hands its replies over in order.
func TestRoundRepliesWaitForMomentaryLockHolder(t *testing.T) {
	raw := &batchProbeConn{}
	conn := newBatchConn(raw, &batchEchoHandler{})
	raw.inbound = append(maskedFrames("echo", "tail"), pingWire("k")...)
	conn.writes.mu.Lock()
	done := make(chan error, 1)
	go func() { done <- conn.readAvailable() }()
	select {
	case err := <-done:
		t.Fatalf("round finished while another goroutine held the write lock: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	conn.unlockWrite()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(testIOTimeout()):
		t.Fatal("round never resumed after the lock was released")
	}
	frames := wireFrames(t, raw)
	if len(frames) != 3 || string(frames[0].Payload) != "echo" || string(frames[1].Payload) != "tail" || frames[2].Opcode != frame.Pong {
		t.Fatalf("wire = %v, want echo, tail, then the Pong", frames)
	}
	if raw.handoffs != 1 {
		t.Fatalf("hand-offs = %d, want the round's replies in one", raw.handoffs)
	}
}

// Every batched frame was admitted against the outbound limit, so a transport
// that refuses the hand-off anyway broke its accounting: the connection is
// aborted with that cause rather than dropping frames its senders were told
// were accepted.
func TestRefusedBatchHandoffAbortsInsteadOfDroppingFrames(t *testing.T) {
	raw := &limitProbeConn{atLimit: true}
	conn := newBatchConn(raw, &batchEchoHandler{})
	raw.inbound = maskedFrames("a", "b")
	if err := conn.readAvailable(); err != nil {
		t.Fatal(err)
	}
	if raw.closes != 1 || !conn.closing.Load() {
		t.Fatalf("refused hand-off = closes:%d closing:%v, want 1/true", raw.closes, conn.closing.Load())
	}
	if info := conn.closeInfo(); !errors.Is(info.Err, errBatchRefused) {
		t.Fatalf("close error = %v, want %v", info.Err, errBatchRefused)
	}
	if conn.batch != nil {
		t.Fatal("refused batch left behind")
	}
	if pending := conn.writes.close.pendingBytes.Load(); pending != 0 {
		t.Fatalf("refused batch left %d pending bytes", pending)
	}
}

// Against a transport that enforces the limit the way UIO does, a batch is
// never refused: frames that do not fit are refused one by one when sent, and
// every frame that was accepted is delivered in order.
func TestBatchUnderOutboundLimitIsNeverRefused(t *testing.T) {
	raw := &limitedWire{limit: 500}
	handler := &batchEchoHandler{}
	server := NewServer(Options{})
	server.Options.MaxOutboundBuffered = raw.limit
	server.Options.OutboundHighWatermark = -1 // refusal paths, not the read pause
	conn := &Conn{raw: raw, handler: handler, config: testServerConfig(server)}
	conn.opened.Store(true)
	// One reply per block, so the batch fills and is handed over mid-round.
	conn.config.batchBlockSize = 64
	refused := 0
	handler.onMessage = func(conn *Conn, message Message) {
		switch err := conn.SendBinary(message.Bytes()); {
		case err == nil:
		case errors.Is(err, ErrBackpressure):
			refused++
		default:
			t.Errorf("reply = %v", err)
		}
	}
	payloads := make([]string, 5)
	for i := range payloads {
		payloads[i] = strings.Repeat(string(rune('a'+i)), 200)
	}
	raw.inbound = maskedFrames(payloads...)
	if err := conn.readAvailable(); err != nil {
		t.Fatal(err)
	}
	// Each reply is 204 bytes on the wire: two fit the 500-byte limit.
	if raw.closeCount() != 0 || conn.closing.Load() {
		t.Fatalf("closes:%d closing:%v, want an open connection", raw.closeCount(), conn.closing.Load())
	}
	if refused != 3 {
		t.Fatalf("refused replies = %d, want 3", refused)
	}
	frames := raw.frames(t)
	if len(frames) != 2 || string(frames[0].Payload) != payloads[0] || string(frames[1].Payload) != payloads[1] {
		t.Fatalf("delivered = %d frames, want the two accepted replies in order", len(frames))
	}
	if pending := conn.writes.close.pendingBytes.Load(); pending != int64(raw.queuedBytes()) {
		t.Fatalf("pending = %d, transport queued = %d", pending, raw.queuedBytes())
	}
}

// A Pong that finds the outbound limit full is dropped, not fatal.
func TestPongAtLimitIsDroppedNotClosed(t *testing.T) {
	raw := &limitProbeConn{atLimit: true}
	conn := newBatchConn(raw, &batchEchoHandler{})
	raw.inbound = pingWire("k")
	if err := conn.readAvailable(); err != nil {
		t.Fatalf("ping at limit closed the read: %v", err)
	}
	if raw.closes != 0 || conn.closing.Load() {
		t.Fatalf("dropped Pong closed the connection: closes:%d closing:%v", raw.closes, conn.closing.Load())
	}
	if pending := conn.writes.close.pendingBytes.Load(); pending != 0 {
		t.Fatalf("dropped Pong left %d pending bytes", pending)
	}
}

// gatedWriteConn stalls the first hand-off inside WriteOwned, so a test can
// observe what a concurrent control frame does while the batch is in flight.
type gatedWriteConn struct {
	batchProbeConn
	entered chan struct{}
	gate    chan struct{}
	mu      sync.Mutex
	blocked bool
}

func (c *gatedWriteConn) WriteOwned(buffer *uio.Buffer) (int, error) {
	c.mu.Lock()
	first := !c.blocked
	c.blocked = true
	c.mu.Unlock()
	if first {
		close(c.entered)
		<-c.gate
	}
	return c.batchProbeConn.WriteOwned(buffer)
}

// While another goroutine hands the batch over, a Pong waits for the write
// lock and follows the batch.
func TestPongWaitsForInFlightBatchHandoff(t *testing.T) {
	raw := &gatedWriteConn{entered: make(chan struct{}), gate: make(chan struct{})}
	conn := newBatchConn(raw, &batchEchoHandler{})
	conn.beginWriteBatch()
	if err := conn.send(BinaryMessage, []byte("batched")); err != nil {
		t.Fatal(err)
	}
	if conn.batch == nil {
		t.Fatal("message did not join the write batch")
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn.writes.mu.Lock()
		defer conn.writes.mu.Unlock()
		_ = conn.flushBatchLocked()
	}()
	select {
	case <-raw.entered:
	case <-time.After(testIOTimeout()):
		t.Fatal("external hand-off never reached the transport")
	}
	pongDone := make(chan error, 1)
	go func() {
		pongDone <- conn.sendFrame(frame.Frame{Fin: true, Opcode: frame.Pong, Payload: []byte("k")})
	}()
	select {
	case err := <-pongDone:
		t.Fatalf("Pong was submitted during the batch hand-off: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(raw.gate)
	select {
	case <-done:
	case <-time.After(testIOTimeout()):
		t.Fatal("hand-off did not complete")
	}
	select {
	case err := <-pongDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(testIOTimeout()):
		t.Fatal("Pong never followed the hand-off")
	}
	frames := wireFrames(t, &raw.batchProbeConn)
	if len(frames) != 2 || frames[0].Opcode != frame.Binary || frames[1].Opcode != frame.Pong {
		t.Fatalf("wire = %v, want the batched frame before the Pong", frames)
	}
	if string(frames[0].Payload) != "batched" {
		t.Fatalf("first payload = %q", frames[0].Payload)
	}
}

// maskedPings is count masked client Pings, each with an 8-byte payload.
func maskedPings(count int) []byte {
	var wire []byte
	for i := range count {
		payload := binary.BigEndian.AppendUint64(nil, uint64(i))
		wire = frame.Append(wire, frame.Frame{Fin: true, Opcode: frame.Ping, Masked: true, Payload: payload}, [4]byte{1, 2, 3, 4})
	}
	return wire
}

// A connection's own replies are what pause it: once the queue holds more
// than the low watermark, a round whose Handler answers into it up to the high
// watermark stops reading, and input that arrives later waits.
func TestOwnRepliesPauseReads(t *testing.T) {
	raw, conn, server := newBackpressureConn(64, 16)
	queueReplies(raw, conn, 20)
	raw.inbound = maskedFrames(strings.Repeat("r", 70))
	if err := conn.readAvailable(); err != nil {
		t.Fatal(err)
	}
	if !conn.internalReadsPaused() {
		t.Fatalf("replies queued to %d bytes did not pause reads", raw.queuedBytes())
	}
	raw.inbound = maskedFrames("next")
	if err := conn.readAvailable(); err != nil {
		t.Fatal(err)
	}
	if len(raw.inbound) == 0 {
		t.Fatal("a paused connection consumed input")
	}
	if stats := server.Stats(); stats.ReadsPaused != 1 {
		t.Fatalf("stats = %+v, want one pause", stats)
	}
}

// A round tells its replies apart only while the queue holds more than the
// low watermark: what a chunk read with the queue drained answers is not
// counted, and the next chunk's is.
func TestRepliesCountOnceTheQueueIsAboveLow(t *testing.T) {
	raw, conn, server := newBackpressureConn(64, 16)
	raw.inbound = maskedFrames(strings.Repeat("r", 70))
	if err := conn.readAvailable(); err != nil {
		t.Fatal(err)
	}
	if conn.internalReadsPaused() || conn.replies.Load() != nil || conn.round.Load() != 0 {
		t.Fatalf("a chunk read with the queue drained: paused %v, replies %d, round %#x",
			conn.internalReadsPaused(), ownQueued(conn), conn.round.Load())
	}
	raw.inbound = maskedFrames(strings.Repeat("s", 70))
	if err := conn.readAvailable(); err != nil {
		t.Fatal(err)
	}
	if !conn.internalReadsPaused() {
		t.Fatalf("replies into a queue of %d bytes did not pause reads: counted %d", raw.queuedBytes(), ownQueued(conn))
	}
	if stats := server.Stats(); stats.ReadsPaused != 1 {
		t.Fatalf("stats = %+v, want one pause", stats)
	}
}

// Output that is not the round's own reply never pauses reads, however full
// the queue: a backlog queued from elsewhere, a send another goroutine makes
// while the round runs, and a send the round's goroutine makes once the round
// has ended.
func TestOutputFromElsewhereDoesNotPauseReads(t *testing.T) {
	raw, conn, server := newBackpressureConn(64, 16)
	conn.handler.(*batchEchoHandler).onMessage = func(c *Conn, _ Message) {
		done := make(chan error, 1)
		go func() { done <- c.SendBinary(make([]byte, 200)) }()
		if err := <-done; err != nil {
			t.Errorf("external send = %v", err)
		}
	}
	raw.queued = 1 << 20 // a broadcast backlog
	raw.inbound = maskedFrames("one", "two")
	if err := conn.readAvailable(); err != nil {
		t.Fatal(err)
	}
	if err := conn.SendBinary(make([]byte, 200)); err != nil {
		t.Fatal(err)
	}
	raw.inbound = maskedFrames("three")
	if err := conn.readAvailable(); err != nil {
		t.Fatal(err)
	}
	if len(raw.inbound) != 0 || conn.internalReadsPaused() {
		t.Fatalf("output from elsewhere paused reads: inbound left %d, paused %v", len(raw.inbound), conn.internalReadsPaused())
	}
	if conn.replies.Load() != nil {
		t.Fatalf("replies counted = %d, want none", ownQueued(conn))
	}
	if stats := server.Stats(); stats.ReadsPaused != 0 {
		t.Fatalf("stats = %+v, want no pause", stats)
	}
}

// Pongs answer the peer, so a peer that floods Pings and never reads the
// answers is paused like one that never reads replies.
func TestPingFloodPausesReads(t *testing.T) {
	raw, conn, server := newBackpressureConn(64, 16)
	raw.inbound = maskedPings(10)
	if err := conn.readAvailable(); err != nil {
		t.Fatal(err)
	}
	if !conn.internalReadsPaused() {
		t.Fatalf("Pongs queued to %d bytes did not pause reads", raw.queuedBytes())
	}
	if stats := server.Stats(); stats.ReadsPaused != 1 {
		t.Fatalf("stats = %+v, want one pause", stats)
	}
}

// A Ping flood is paused however the queue got full: Pongs answered into a
// queue that output from elsewhere already holds past the high watermark
// count as they do anywhere else, so a peer that floods Pings behind a
// backlog cannot keep reads going.
func TestPingFloodBehindABacklogPausesReads(t *testing.T) {
	for _, backlog := range []int{60, 1 << 20} { // between the watermarks, and far past them
		t.Run(fmt.Sprint(backlog), func(t *testing.T) {
			raw, conn, server := newBackpressureConn(64, 16)
			raw.queued = backlog
			raw.inbound = maskedPings(200)
			for range 5 {
				if err := conn.readAvailable(); err != nil {
					t.Fatal(err)
				}
			}
			if !conn.internalReadsPaused() {
				t.Fatalf("a Ping flood behind a %d-byte backlog never paused reads: replies %d", backlog, ownQueued(conn))
			}
			if stats := server.Stats(); stats.ReadsPaused != 1 {
				t.Fatalf("stats = %+v, want one pause", stats)
			}
		})
	}
}

// A few Pongs behind a backlog from elsewhere do not pause: an uploading
// client its server pings now and then keeps reading.
func TestKeepalivePongsBehindABacklogDoNotPauseReads(t *testing.T) {
	raw, conn, server := newBackpressureConn(64, 16)
	raw.queued = 1 << 20
	raw.inbound = maskedPings(3)
	if err := conn.readAvailable(); err != nil {
		t.Fatal(err)
	}
	raw.inbound = maskedPings(1)
	if err := conn.readAvailable(); err != nil {
		t.Fatal(err)
	}
	if len(raw.inbound) != 0 || conn.internalReadsPaused() {
		t.Fatalf("keepalive Pongs paused reads: inbound left %d, replies %d", len(raw.inbound), ownQueued(conn))
	}
	if got := ownQueued(conn); got != 40 {
		t.Fatalf("replies counted = %d, want the four Pongs' 40 bytes", got)
	}
	if stats := server.Stats(); stats.ReadsPaused != 0 {
		t.Fatalf("stats = %+v, want no pause", stats)
	}
}

// bulkMessages and bulkPayload size the end-to-end transfers below: enough
// that the kernel's socket buffers cannot absorb them, so the queues reach
// their watermarks.
const (
	bulkMessages = 8192
	bulkPayload  = 4 << 10
)

// pumpAndDrain sends bulkMessages from one goroutine while reading the echoes
// on another, as an uploading client does, and fails once the echoes stop
// arriving.
func pumpAndDrain(t *testing.T, client *Client, stats func() string) {
	t.Helper()
	go func() {
		payload := make([]byte, bulkPayload)
		for i := range bulkMessages {
			payload[0] = byte(i)
			if client.SendBinary(payload) != nil {
				return
			}
		}
	}()
	for i := range bulkMessages {
		ctx, cancel := context.WithTimeout(context.Background(), testIOTimeout())
		message, err := client.ReadMessage(ctx)
		cancel()
		if err != nil {
			t.Fatalf("echo %d of %d: %v (%s)", i, bulkMessages, err, stats())
		}
		if len(message.Payload) != bulkPayload || message.Payload[0] != byte(i) {
			t.Fatalf("echo %d: %d bytes starting %d", i, len(message.Payload), message.Payload[0])
		}
	}
}

// A client sending from its own goroutine while it reads must not stop
// reading because of what it sends: with an echo server that pauses on its
// replies, the two used to wait on each other for good. The server's Pings
// answered meanwhile must not pause it either.
func TestUploadingClientKeepsReadingAnEchoServer(t *testing.T) {
	server, addr, serveDone := startTestServer(t, func() *Server {
		return NewServer(Options{Handler: echoAllHandler{},
			HeartbeatInterval: 5 * time.Millisecond, HeartbeatTimeout: time.Minute})
	})
	t.Cleanup(func() { stopTestServer(t, server, serveDone) })
	dialer := NewDialer(Options{})
	t.Cleanup(func() { _ = dialer.Close(nil) })
	ctx, cancel := context.WithTimeout(context.Background(), testIOTimeout())
	defer cancel()
	client, err := dialer.DialClient(ctx, "ws://"+addr+"/")
	if err != nil {
		t.Fatal(err)
	}
	pumpAndDrain(t, client, func() string {
		return fmt.Sprintf("client %+v, server %+v", dialer.Stats(), server.Stats())
	})
}

// relayLeg pairs one connection of a relay with its partner, which the dial
// fills in once the upstream leg opened.
type relayLeg struct {
	client   *Conn
	upstream atomic.Pointer[Conn]
	linked   chan struct{}
}

// relayHandler forwards each client message to the upstream leg it dials.
type relayHandler struct {
	dialer *Dialer
	target string
	legs   chan *relayLeg
}

func (h *relayHandler) OnOpen(c *Conn) {
	leg := &relayLeg{client: c, linked: make(chan struct{})}
	c.SetUserdata(leg)
	h.legs <- leg
	go func() { _ = h.dialer.Dial(context.Background(), h.target, leg) }()
}

func (*relayHandler) OnMessage(c *Conn, m Message) {
	if up := c.Userdata().(*relayLeg).upstream.Load(); up != nil {
		_ = up.Send(m.Type, m.Payload)
	}
}

func (*relayHandler) OnClose(*Conn, CloseEvent) {}

// relayUpstream forwards what the upstream answers back to the client leg.
type relayUpstream struct{}

func (relayUpstream) OnOpen(c *Conn) {
	leg := c.Userdata().(*relayLeg)
	leg.upstream.Store(c)
	close(leg.linked)
}

func (relayUpstream) OnMessage(c *Conn, m Message) {
	_ = c.Userdata().(*relayLeg).client.Send(m.Type, m.Payload)
}

func (relayUpstream) OnClose(*Conn, CloseEvent) {}

// slowStartEcho echoes, but holds its first message long enough for the relay's
// upstream queue to back up behind it.
type slowStartEcho struct{ slept atomic.Bool }

func (*slowStartEcho) OnOpen(*Conn) {}

func (h *slowStartEcho) OnMessage(c *Conn, m Message) {
	if h.slept.CompareAndSwap(false, true) {
		time.Sleep(300 * time.Millisecond)
	}
	_ = c.Send(m.Type, m.Payload)
}

func (*slowStartEcho) OnClose(*Conn, CloseEvent) {}

// A relay leg whose queue fills with what the other leg forwards must not
// stop reading because of it: its upstream, an echo server that pauses on its
// replies, would wait on it for good, as both relay legs would wait on the
// client.
func TestRelayLegsKeepReadingAgainstAnEchoUpstream(t *testing.T) {
	upstream, upstreamAddr, upstreamDone := startTestServer(t, func() *Server {
		return NewServer(Options{Handler: &slowStartEcho{}})
	})
	t.Cleanup(func() { stopTestServer(t, upstream, upstreamDone) })
	upstreamDialer := NewDialer(Options{Handler: relayUpstream{}})
	t.Cleanup(func() { _ = upstreamDialer.Close(nil) })
	relay := &relayHandler{dialer: upstreamDialer, target: "ws://" + upstreamAddr + "/", legs: make(chan *relayLeg, 1)}
	relayServer, relayAddr, relayDone := startTestServer(t, func() *Server {
		return NewServer(Options{Handler: relay})
	})
	t.Cleanup(func() { stopTestServer(t, relayServer, relayDone) })

	dialer := NewDialer(Options{})
	t.Cleanup(func() { _ = dialer.Close(nil) })
	ctx, cancel := context.WithTimeout(context.Background(), testIOTimeout())
	defer cancel()
	client, err := dialer.DialClient(ctx, "ws://"+relayAddr+"/")
	if err != nil {
		t.Fatal(err)
	}
	select {
	case leg := <-relay.legs:
		select {
		case <-leg.linked:
		case <-ctx.Done():
			t.Fatal("the relay never opened its upstream leg")
		}
	case <-ctx.Done():
		t.Fatal("the relay never accepted the client")
	}
	pumpAndDrain(t, client, func() string {
		return fmt.Sprintf("client %+v, relay %+v, relay upstream %+v, upstream %+v",
			dialer.Stats(), relayServer.Stats(), upstreamDialer.Stats(), upstream.Stats())
	})
}

// A connection that answers a little into a backlog queued from elsewhere
// does not pause: its replies have to fill the band between the watermarks
// themselves.
func TestSmallRepliesIntoABacklogDoNotPauseReads(t *testing.T) {
	raw, conn, server := newBackpressureConn(64, 16)
	raw.queued = 1 << 20
	raw.inbound = maskedFrames("hi")
	if err := conn.readAvailable(); err != nil {
		t.Fatal(err)
	}
	raw.inbound = maskedFrames("again")
	if err := conn.readAvailable(); err != nil {
		t.Fatal(err)
	}
	if len(raw.inbound) != 0 || conn.internalReadsPaused() {
		t.Fatalf("small replies into a backlog paused reads: inbound left %d, paused %v", len(raw.inbound), conn.internalReadsPaused())
	}
	if got := ownQueued(conn); got != 11 {
		t.Fatalf("replies counted = %d, want the two replies' 11 bytes", got)
	}
	if stats := server.Stats(); stats.ReadsPaused != 0 {
		t.Fatalf("stats = %+v, want no pause", stats)
	}
}

// Replies that have left the queue stop counting, as the transport reports
// them sent, whether or not a read round runs in between: a backlog from
// elsewhere that arrives later finds nothing of the connection's own to pause
// for.
func TestRepliesThatLeftDoNotCount(t *testing.T) {
	for _, tc := range []struct {
		name    string
		drainTo int
		own     int64
	}{
		{"partly drained", 100, 100}, // what is left of the replies
		{"drained to low", 64, 64},
		{"drained", 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, conn, server := newBackpressureConn(256, 64)
			conn.handler.(*batchEchoHandler).onMessage = func(*Conn, Message) {}
			queueReplies(raw, conn, 202) // past the band, under the high watermark
			drainTo(raw, conn, tc.drainTo)
			if got := ownQueued(conn); got != tc.own {
				t.Fatalf("replies counted = %d after the drain, want %d", got, tc.own)
			}
			raw.queued += 1 << 20 // a backlog from elsewhere
			raw.inbound = maskedFrames("y")
			if err := conn.readAvailable(); err != nil {
				t.Fatal(err)
			}
			if len(raw.inbound) != 0 || conn.internalReadsPaused() {
				t.Fatalf("replies that left paused reads: inbound left %d, replies %d", len(raw.inbound), ownQueued(conn))
			}
			if stats := server.Stats(); stats.ReadsPaused != 0 {
				t.Fatalf("stats = %+v, want no pause", stats)
			}
		})
	}
}

// Replies queued behind a backlog stay counted until the backlog ahead of
// them and they themselves have left: a drain that sends only the backlog
// leaves them in full.
func TestRepliesBehindABacklogCountUntilTheyLeave(t *testing.T) {
	raw, conn, _ := newBackpressureConn(64, 16)
	raw.queued = 1000 // a backlog from elsewhere
	queueReplies(raw, conn, 50)
	drainTo(raw, conn, 60) // 990 of the backlog left: all 50 replies are queued
	if got := ownQueued(conn); got != 50 {
		t.Fatalf("replies counted = %d, want all 50 still queued", got)
	}
	drainTo(raw, conn, 10) // the backlog and 40 bytes of the replies left
	if got := ownQueued(conn); got != 10 {
		t.Fatalf("replies counted = %d, want the 10 bytes still queued", got)
	}
	raw.queued += 1000          // more output from elsewhere
	queueReplies(raw, conn, 50) // and more replies behind it
	drainTo(raw, conn, 30)      // the first replies, the backlog and 20 bytes of the new ones left
	if got := ownQueued(conn); got != 30 {
		t.Fatalf("replies counted = %d, want 30 of the new 50 still queued", got)
	}
}

// A pause starts the count over: once the queue drained and reads resumed, a
// backlog from elsewhere that arrives before the next round does not pause
// again on replies that already left.
func TestResumedReadsDoNotPauseOnRepliesThatLeft(t *testing.T) {
	raw, conn, server := newBackpressureConn(64, 16)
	conn.handler.(*batchEchoHandler).onMessage = func(*Conn, Message) {}
	queueReplies(raw, conn, 64)
	raw.inbound = maskedFrames("one")
	if err := conn.readAvailable(); err != nil {
		t.Fatal(err)
	}
	if !conn.internalReadsPaused() {
		t.Fatal("replies at the high watermark did not pause reads")
	}
	drainTo(raw, conn, 16)
	conn.resumeReadsAfterDrain()
	raw.queued += 1 << 20
	if err := conn.readAvailable(); err != nil {
		t.Fatal(err)
	}
	if len(raw.inbound) != 0 || conn.internalReadsPaused() {
		t.Fatalf("a backlog from elsewhere paused resumed reads: inbound left %d", len(raw.inbound))
	}
	if stats := server.Stats(); stats.ReadsPaused != 1 || stats.ReadsResumed != 1 {
		t.Fatalf("stats = %+v, want one pause and one resume", stats)
	}
}

// A round whose queue drains to the low watermark withdraws its mark: the
// rounds after it run without telling their sends apart, which is what keeps
// the echo path free of the goroutine-id lookup.
func TestDrainedRoundsStopMarkingReplies(t *testing.T) {
	raw, conn, _ := newBackpressureConn(64, 16)
	var seen []uint32
	conn.handler.(*batchEchoHandler).onMessage = func(c *Conn, m Message) {
		seen = append(seen, c.round.Load())
		_ = c.SendBinary(m.Bytes()) // counted in the marked round, so the next one decides
	}
	raw.queued = 20
	raw.inbound = maskedFrames("a")
	if err := conn.readAvailable(); err != nil {
		t.Fatal(err)
	}
	raw.queued = 16
	raw.inbound = maskedFrames("b")
	if err := conn.readAvailable(); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 2 || seen[0] <= roundBatching || seen[1] != 0 {
		t.Fatalf("round words seen = %#x, want the first round marked and the drained one not", seen)
	}
}

// A compressed reply counts what it queued, not what the Handler handed to
// the send: a long repetitive message that compresses to a few bytes does not
// pause reads.
func TestCompressedRepliesCountTheirWireSize(t *testing.T) {
	server := NewServer(Options{Handler: &batchEchoHandler{}, OutboundHighWatermark: 64, OutboundLowWatermark: 16,
		EnableCompression: true})
	raw := &limitedWire{}
	conn := &Conn{raw: raw, handler: &batchEchoHandler{}, config: testServerConfig(server)}
	conn.opened.Store(true)
	conn.compression = &compressionState{encoder: compress.NewEncoder(-1, true)}
	raw.queued = 60 // above the low watermark: the round's replies are counted
	raw.inbound = maskedFrames(strings.Repeat("a", 1000))
	if err := conn.readAvailable(); err != nil {
		t.Fatal(err)
	}
	reply := raw.queuedBytes() - 60
	if reply <= 0 || reply >= 100 {
		t.Fatalf("the reply queued %d bytes, want it compressed", reply)
	}
	if got := ownQueued(conn); got != int64(reply) {
		t.Fatalf("replies counted = %d, want the %d bytes queued", got, reply)
	}
	raw.inbound = maskedFrames("x")
	conn.handler.(*batchEchoHandler).onMessage = func(*Conn, Message) {}
	if err := conn.readAvailable(); err != nil {
		t.Fatal(err)
	}
	if conn.internalReadsPaused() {
		t.Fatal("a compressed reply paused reads for its uncompressed size")
	}
}

// Every frame of a fragmented reply counts, at its own wire size.
func TestFragmentedRepliesCountEveryFrame(t *testing.T) {
	server := NewServer(Options{Handler: &batchEchoHandler{}, OutboundHighWatermark: 1 << 10, OutboundLowWatermark: 16,
		MaxFramePayload: 100})
	raw := &limitedWire{}
	conn := &Conn{raw: raw, handler: &batchEchoHandler{}, config: testServerConfig(server)}
	conn.opened.Store(true)
	raw.queued = 20
	raw.inbound = maskedFrames(strings.Repeat("f", 90))
	conn.handler.(*batchEchoHandler).onMessage = func(c *Conn, _ Message) {
		if err := c.SendBinary(make([]byte, 250)); err != nil {
			t.Errorf("send = %v", err)
		}
	}
	if err := conn.readAvailable(); err != nil {
		t.Fatal(err)
	}
	if want := int64(raw.queuedBytes() - 20); ownQueued(conn) != want || want != 3*2+250 {
		t.Fatalf("replies counted = %d, want the three frames' %d bytes", ownQueued(conn), want)
	}
}

// A watermark band past what an int32 holds still pauses: the replies are
// counted in full.
func TestWideWatermarkBandStillPauses(t *testing.T) {
	const high, low = 3 << 30, 1 << 30
	raw, conn, server := newBackpressureConn(high, low)
	queueReplies(raw, conn, high)
	raw.inbound = maskedFrames("one")
	if err := conn.readAvailable(); err != nil {
		t.Fatal(err)
	}
	if !conn.internalReadsPaused() {
		t.Fatalf("replies of %d bytes did not pause reads with a %d-byte band", ownQueued(conn), high-low)
	}
	if stats := server.Stats(); stats.ReadsPaused != 1 {
		t.Fatalf("stats = %+v, want one pause", stats)
	}
}

// A heartbeat Ping is this end speaking first, not a reply, even when a pull
// Client's full queue runs the heartbeat inside a read round.
func TestHeartbeatPingIsNoReply(t *testing.T) {
	raw, conn, _ := newBackpressureConn(64, 16)
	conn.heartbeat = &heartbeatState{}
	conn.round.Store(roundToken()) // a marked round's goroutine
	if err := conn.sendHeartbeatPing(time.Now()); err != nil {
		t.Fatal(err)
	}
	if raw.queuedBytes() == 0 || conn.replies.Load() != nil {
		t.Fatalf("queued %d, replies %d: want the Ping queued and not counted", raw.queuedBytes(), ownQueued(conn))
	}
}

// reservingWire is limitedWire with the reservation a connection's own turn
// gets from the native transport, so replies take the short send path.
type reservingWire struct {
	*limitedWire
	reserved int
}

func (w *reservingWire) ReserveOutbound(n int) ([]byte, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.queued += n
	w.reserved += n
	return make([]byte, n), nil
}

// The short send path — a reply encoded straight into the transport's queue —
// counts the reply too.
func TestShortPathRepliesCount(t *testing.T) {
	server := NewServer(Options{Handler: &batchEchoHandler{}, OutboundHighWatermark: 64, OutboundLowWatermark: 16})
	raw := &reservingWire{limitedWire: &limitedWire{}}
	conn := &Conn{raw: raw, handler: &batchEchoHandler{}, config: testServerConfig(server)}
	conn.opened.Store(true)
	raw.queued = 20
	raw.inbound = maskedFrames(strings.Repeat("s", 50))
	if err := conn.readAvailable(); err != nil {
		t.Fatal(err)
	}
	if raw.reserved != 52 {
		t.Fatalf("reserved %d bytes, want the reply's 52 on the short path", raw.reserved)
	}
	if got := ownQueued(conn); got != 52 {
		t.Fatalf("replies counted = %d, want 52", got)
	}
}

// Replies the round batches count once the batch is handed over, where they
// sit in the queue then.
func TestBatchedRepliesCount(t *testing.T) {
	raw, conn, server := newBackpressureConn(64, 16)
	raw.queued = 20
	raw.inbound = maskedFrames("aaaaaaaaaa", "bbbbbbbbbb", "cccccccccc", "dddddddddd", "eeeeeeeeee")
	if err := conn.readAvailable(); err != nil {
		t.Fatal(err)
	}
	if got := ownQueued(conn); got != 60 || raw.queuedBytes() != 80 {
		t.Fatalf("replies counted = %d with %d queued, want the batch's 60 of 80", got, raw.queuedBytes())
	}
	raw.inbound = maskedFrames("next")
	if err := conn.readAvailable(); err != nil {
		t.Fatal(err)
	}
	if !conn.internalReadsPaused() {
		t.Fatal("batched replies past the band did not pause reads")
	}
	if stats := server.Stats(); stats.ReadsPaused != 1 {
		t.Fatalf("stats = %+v, want one pause", stats)
	}
}

// A connection whose replies have all left drops its ledger once a round
// finds the queue drained, so an idle connection keeps none.
func TestDrainedLedgerIsReleased(t *testing.T) {
	raw, conn, _ := newBackpressureConn(64, 16)
	conn.handler.(*batchEchoHandler).onMessage = func(*Conn, Message) {}
	queueReplies(raw, conn, 30)
	drainTo(raw, conn, 0)
	raw.inbound = maskedFrames("x")
	if err := conn.readAvailable(); err != nil {
		t.Fatal(err)
	}
	if conn.replies.Load() != nil {
		t.Fatal("a drained connection kept its reply ledger")
	}

	// A ledger is not dropped while replies are still queued.
	conn.handler.(*batchEchoHandler).onMessage = func(*Conn, Message) {}
	queueReplies(raw, conn, 30)
	raw.queued += 100 // output from elsewhere behind the replies
	raw.inbound = maskedFrames("y")
	if err := conn.readAvailable(); err != nil {
		t.Fatal(err)
	}
	if got := ownQueued(conn); got != 30 {
		t.Fatalf("replies counted = %d after the round, want the 30 still queued", got)
	}
	if conn.replies.Load() == nil {
		t.Fatal("the round dropped a ledger whose replies were still queued")
	}
}

// trackedLimitedWire is limitedWire over a layer stack: it takes tracked
// writes and reports wire bytes, which are not the frame bytes queued.
type trackedLimitedWire struct {
	limitedWire
}

func (w *trackedLimitedWire) WriteTracked(p []byte) (int, uio.SendReceipt, error) {
	n, err := w.Write(p)
	return n, uio.SendReceipt{}, err
}

// A tracked transport — a layer stack, TLS under the protocol — reports wire
// bytes, not the frame bytes replies are counted in, so it tracks no replies
// and never pauses on them: the watermark is a plain transport's.
func TestTrackedTransportTracksNoReplies(t *testing.T) {
	server := NewServer(Options{Handler: &batchEchoHandler{}, OutboundHighWatermark: 64, OutboundLowWatermark: 16})
	raw := &trackedLimitedWire{}
	conn := &Conn{raw: raw, handler: &batchEchoHandler{}, config: testServerConfig(server)}
	conn.setRaw(raw)
	conn.opened.Store(true)
	if !conn.wireTracked() {
		t.Fatal("the probe does not read as a tracked transport")
	}
	raw.queued = 60
	raw.inbound = maskedPings(200)
	for range 5 {
		if err := conn.readAvailable(); err != nil {
			t.Fatal(err)
		}
	}
	raw.inbound = maskedFrames(strings.Repeat("r", 70))
	for range 5 {
		if err := conn.readAvailable(); err != nil {
			t.Fatal(err)
		}
	}
	if conn.internalReadsPaused() || conn.replies.Load() != nil || pendingBatchReplies(conn) != 0 {
		t.Fatalf("a tracked transport tracked replies: paused %v, ledger %v, batched %d",
			conn.internalReadsPaused(), conn.replies.Load() != nil, pendingBatchReplies(conn))
	}
	if stats := server.Stats(); stats.ReadsPaused != 0 {
		t.Fatalf("stats = %+v, want no pause over a tracked transport", stats)
	}
}

// Replies queued among output from elsewhere count exactly: each group the
// external bytes separate is a run of its own, and a drain retires no more
// than what left.
func TestInterleavedRepliesCountExactly(t *testing.T) {
	raw, conn, _ := newBackpressureConn(4096, 64)
	conn.handler.(*batchEchoHandler).onMessage = func(*Conn, Message) {}
	// Three groups of 30 reply bytes, 100 external bytes between them.
	for range 3 {
		queueReplies(raw, conn, 30)
		raw.queued += 100
	}
	if got := ownQueued(conn); got != 90 {
		t.Fatalf("replies counted = %d, want the three groups' 90", got)
	}
	// The first external block and the first group leave: 60 remain.
	drainTo(raw, conn, 300)
	if got := ownQueued(conn); got != 60 {
		t.Fatalf("replies counted = %d after 130 bytes left, want 60", got)
	}
	// Everything but the last group leaves: 30 remain.
	drainTo(raw, conn, 130)
	if got := ownQueued(conn); got != 30 {
		t.Fatalf("replies counted = %d, want the last group's 30", got)
	}
	drainTo(raw, conn, 0)
	if got := ownQueued(conn); got != 0 {
		t.Fatalf("replies counted = %d after a full drain, want 0", got)
	}
}

// A drained connection drops its ledger without waiting for input: the
// drain's report asks for the round that drops it.
func TestDrainedLedgerReleasesWithoutInput(t *testing.T) {
	raw, conn, _ := newBackpressureConn(64, 16)
	conn.handler.(*batchEchoHandler).onMessage = func(*Conn, Message) {}
	queueReplies(raw, conn, 30)
	drainTo(raw, conn, 0)
	raw.wakes = 0
	conn.resumeReadsAfterDrain() // the drain's report, as the callback makes it
	if raw.wakes != 1 {
		t.Fatalf("wakes = %d, want the round that drops the ledger", raw.wakes)
	}
	raw.inbound = maskedFrames("x")
	if err := conn.readAvailable(); err != nil {
		t.Fatal(err)
	}
	if conn.replies.Load() != nil {
		t.Fatal("the woken round left the ledger in place")
	}
}

// A Pong into a drained queue allocates nothing: no ledger is kept for it.
func TestPongIntoADrainedQueueKeepsNoLedger(t *testing.T) {
	raw, conn, _ := newBackpressureConn(64, 16)
	raw.inbound = maskedPings(1)
	if err := conn.readAvailable(); err != nil {
		t.Fatal(err)
	}
	if conn.replies.Load() != nil || pendingBatchReplies(conn) != 0 {
		t.Fatalf("a Pong into a drained queue kept a ledger: %v, batched %d",
			conn.replies.Load() != nil, pendingBatchReplies(conn))
	}
}

// BenchmarkPongAnswer measures the control-frame reply path: one masked Ping
// answered while the queue is drained, the shape a heartbeat exchange has.
//
//	go test -run '^$' -bench '^BenchmarkPongAnswer$' -benchmem ./uws
func BenchmarkPongAnswer(b *testing.B) {
	server := NewServer(Options{Handler: &batchEchoHandler{}, OutboundHighWatermark: 64 << 10})
	raw := &limitedWire{}
	conn := &Conn{raw: raw, handler: &batchEchoHandler{}, config: testServerConfig(server)}
	conn.opened.Store(true)
	wire := maskedPings(1)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		raw.inbound = wire
		if err := conn.readAvailable(); err != nil {
			b.Fatal(err)
		}
		// The peer reads the answer, as the drain reports it: without this
		// the queue fills and the round pauses, which is the flood the
		// watermark answers, not the pong.
		drainTo(raw, conn, 0)
	}
}

// A write batch the outbound limit refuses queued nothing, so its replies
// count nowhere: a refused hand-over must not place them.
func TestRefusedBatchCountsNoReplies(t *testing.T) {
	server := NewServer(Options{Handler: &batchEchoHandler{}, OutboundHighWatermark: 24, OutboundLowWatermark: 8,
		MaxOutboundBuffered: 30})
	raw := &limitedWire{limit: 30}
	conn := &Conn{raw: raw, handler: &batchEchoHandler{}, config: testServerConfig(server)}
	conn.opened.Store(true)
	raw.queued = 25 // near the outbound limit
	raw.inbound = maskedFrames("aa", "bb")
	if err := conn.readAvailable(); err != nil {
		t.Fatal(err)
	}
	if raw.queuedBytes() > 30 {
		t.Fatalf("queued = %d past the limit", raw.queuedBytes())
	}
	// The second reply fit the limit and went straight in, so it counts; the
	// batch the limit refused queued nothing and must not.
	if got := ownQueued(conn); got != 4 {
		t.Fatalf("replies counted = %d, want only the accepted frame's 4 bytes", got)
	}
}

// racingDrainWire reports the queue as it was, then drains part of it with
// the report a send makes — the completion lands between the position's two
// readings, as a real report can between queueEnd's.
type racingDrainWire struct {
	*limitedWire
	conn     *Conn
	drains   int // bytes each armed report retires
	armed    bool
	reported atomic.Int32
}

func (w *racingDrainWire) OutboundBuffered() int {
	v := w.queued
	if w.armed {
		w.armed = false
		w.limitedWire.queued = v - w.drains
		w.reported.Add(1)
		w.conn.transportSent(w.drains)
	}
	return v
}

// A send report landing between the position's two readings must not place a
// reply past its bytes: the ledger would keep counting it after it left, and
// output from elsewhere alone would pause reads.
func TestReportBetweenPositionReadsInflatesNoReply(t *testing.T) {
	server := NewServer(Options{Handler: &batchEchoHandler{}, OutboundHighWatermark: 64, OutboundLowWatermark: 16})
	inner := &limitedWire{}
	raw := &racingDrainWire{limitedWire: inner, drains: 60}
	conn := &Conn{raw: raw, handler: &batchEchoHandler{}, config: testServerConfig(server)}
	raw.conn = conn
	conn.opened.Store(true)
	conn.handler.(*batchEchoHandler).onMessage = func(*Conn, Message) {}

	// 60 bytes of replies queued and counted, then 55 more whose position is
	// read across a report retiring the first 60.
	raw.queued = 60
	replies := newReplyLedger()
	replies.add(60, 60)
	conn.replies.Store(replies)
	raw.queued = 115
	raw.armed = true
	conn.writes.mu.Lock()
	replies.add(55, conn.queueEnd(replies))
	conn.writes.mu.Unlock()
	if raw.reported.Load() != 1 {
		t.Fatal("the probe never made its report")
	}
	// Everything the transport still holds leaves: whatever the ledger counts
	// now must be gone with it.
	drainTo(raw.limitedWire, conn, 0)
	if got := ownQueued(conn); got != 0 {
		t.Fatalf("replies counted = %d after everything left, want 0", got)
	}
	raw.queued = 1 << 20 // output from elsewhere alone
	raw.inbound = maskedFrames("x")
	if err := conn.readAvailable(); err != nil {
		t.Fatal(err)
	}
	if conn.internalReadsPaused() {
		t.Fatal("output from elsewhere alone paused reads")
	}
}

// More separated groups than the ledger's initial storage still count
// exactly: growing the queue neither forgets replies nor merges them across
// output it does not hold.
func TestMoreGroupsThanRunsDoNotInflate(t *testing.T) {
	raw, conn, server := newBackpressureConn(64, 16)
	conn.handler.(*batchEchoHandler).onMessage = func(*Conn, Message) {}
	for range 15 {
		queueReplies(raw, conn, 6)
		raw.queued += 20 // output from elsewhere between the groups
	}
	// All but the last 30 bytes leave: the last group's 6 reply bytes and 24
	// of the output around them.
	drainTo(raw, conn, 30)
	if got := ownQueued(conn); got != 6 {
		t.Fatalf("replies counted = %d with 6 reply bytes truly queued, want 6", got)
	}
	raw.queued = 1 << 20
	raw.inbound = maskedFrames("x")
	if err := conn.readAvailable(); err != nil {
		t.Fatal(err)
	}
	if conn.internalReadsPaused() {
		t.Fatalf("reads paused with %d of the connection's own bytes queued", ownQueued(conn))
	}
	if stats := server.Stats(); stats.ReadsPaused != 0 {
		t.Fatalf("stats = %+v, want no pause", stats)
	}
}
