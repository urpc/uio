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
	"encoding/binary"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urpc/uio"
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
	server := NewServer(Options{Handler: &batchEchoHandler{}, OutboundHighWatermark: 64, OutboundLowWatermark: 16})
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
	if conn.batch != nil || conn.batching.Load() {
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
	if conn.batching.Load() || conn.batch != nil {
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
