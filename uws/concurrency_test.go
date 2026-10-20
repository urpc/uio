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
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urpc/uio"
	"github.com/urpc/uio/uws/internal/frame"
)

func waitSignal(t *testing.T, ch <-chan struct{}, label string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for %s", label)
	}
}

func TestExternalCloseInSingleFrameRound(t *testing.T) {
	raw := &limitedWire{limit: 1 << 20}
	c := newLimitedConn(raw, time.Hour)
	t.Cleanup(c.stopCloseTimer)
	entered, resume := make(chan struct{}), make(chan struct{})
	c.handler = &batchEchoHandler{onMessage: func(*Conn, Message) {
		close(entered)
		<-resume
	}}
	raw.inbound = maskedFrames("one")
	done := make(chan error, 1)
	go func() { done <- c.readAvailable() }()
	waitSignal(t, entered, "handler")
	closeErr := c.Close(1000, "bye")
	close(resume)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if closeErr != nil {
		t.Fatal(closeErr)
	}
	if got := raw.frames(t); len(got) != 1 || got[0].Opcode != frame.Close {
		t.Fatalf("round returned without sending Close: frames=%v deferred=%v", got, c.writes.close.closeIsDeferred())
	}
}

func TestExternalSendCannotStealBatchRoom(t *testing.T) {
	raw := &limitedWire{limit: 64}
	c := newLimitedConn(raw, time.Hour)
	entered, resume := make(chan struct{}), make(chan struct{})
	accepted := make(chan error, 1)
	first := true
	c.handler = &batchEchoHandler{onMessage: func(c *Conn, _ Message) {
		if !first {
			return
		}
		first = false
		accepted <- c.SendBinary(bytes.Repeat([]byte("a"), 40))
		close(entered)
		<-resume
	}}
	raw.inbound = maskedFrames("one", "two")
	done := make(chan error, 1)
	go func() { done <- c.readAvailable() }()
	waitSignal(t, entered, "accepted batch")
	batchErr := <-accepted
	externalErr := c.SendBinary(bytes.Repeat([]byte("b"), 40))
	close(resume)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if batchErr != nil {
		t.Fatal(batchErr)
	}
	if raw.closeCount() != 0 {
		t.Fatalf("accepted batch lost: external send=%v closes=%d close error=%v delivered=%d", externalErr, raw.closeCount(), c.closeInfo().Err, len(raw.frames(t)))
	}
}

func TestHeartbeatCloseFollowsAcceptedBatch(t *testing.T) {
	raw := &limitedWire{limit: 1 << 20}
	c := newLimitedConn(raw, time.Hour)
	c.heartbeat = &heartbeatState{}
	t.Cleanup(c.stopCloseTimer)
	entered, resume := make(chan struct{}), make(chan struct{})
	accepted := make(chan error, 1)
	first := true
	c.handler = &batchEchoHandler{onMessage: func(c *Conn, _ Message) {
		if !first {
			return
		}
		first = false
		accepted <- c.SendBinary([]byte("accepted"))
		close(entered)
		<-resume
	}}
	raw.inbound = maskedFrames("one", "two")
	done := make(chan error, 1)
	go func() { done <- c.readAvailable() }()
	waitSignal(t, entered, "accepted batch")
	if err := <-accepted; err != nil {
		t.Fatal(err)
	}
	c.sendHeartbeatClose(1001, "heartbeat timeout")
	close(resume)
	if err := <-done; err != nil && !errors.Is(err, ErrClosed) {
		t.Fatal(err)
	}
	got := raw.frames(t)
	if len(got) != 2 || got[0].Opcode != frame.Binary || got[1].Opcode != frame.Close {
		t.Fatalf("heartbeat Close overtook accepted batch: %v", got)
	}
}

type fragmentGateWire struct {
	limitedWire
	firstFragment   chan struct{}
	releaseFragment chan struct{}
	batchFlushed    chan struct{}
	firstOnce       sync.Once
	batchOnce       sync.Once
}

func (w *fragmentGateWire) WriteOwned(b *uio.Buffer) (int, error) {
	header, size := b.Bytes()[0], b.Len()
	n, err := w.limitedWire.WriteOwned(b)
	if err != nil {
		return n, err
	}
	if header == byte(frame.Binary) {
		w.firstOnce.Do(func() { close(w.firstFragment); <-w.releaseFragment })
	}
	if header == 0x80|byte(frame.Binary) && size > 3 {
		w.batchOnce.Do(func() { close(w.batchFlushed) })
	}
	return n, err
}

func (w *fragmentGateWire) Writev(vec [][]byte) (int, error) {
	header := vec[0][0]
	n, err := w.limitedWire.Writev(vec)
	if err == nil && header == byte(frame.Binary) {
		w.firstOnce.Do(func() { close(w.firstFragment); <-w.releaseFragment })
	}
	return n, err
}

func TestBatchCannotInterruptFragmentedMessage(t *testing.T) {
	raw := &fragmentGateWire{
		limitedWire:   limitedWire{limit: 1 << 20},
		firstFragment: make(chan struct{}), releaseFragment: make(chan struct{}), batchFlushed: make(chan struct{}),
	}
	c := newLimitedConn(&raw.limitedWire, time.Hour)
	c.raw = raw
	c.config = testServerConfig(NewServer(Options{MaxOutboundBuffered: 1 << 20, OutboundHighWatermark: -1, MaxFramePayload: 8, WriteCoalesce: 10}, nil))
	entered, resume := make(chan struct{}), make(chan struct{})
	first := true
	handlerErr := make(chan error, 1)
	c.handler = &batchEchoHandler{onMessage: func(c *Conn, _ Message) {
		if !first {
			return
		}
		first = false
		if err := c.SendBinary([]byte("x")); err != nil {
			handlerErr <- err
			return
		}
		close(entered)
		<-resume
		for i := 0; i < 2000; i++ {
			if err := c.SendBinary([]byte("x")); err != nil {
				handlerErr <- err
				return
			}
		}
		handlerErr <- nil
	}}
	raw.inbound = maskedFrames("one", "two")
	roundDone := make(chan error, 1)
	go func() { roundDone <- c.readAvailable() }()
	waitSignal(t, entered, "accepted round reply")
	externalDone := make(chan error, 1)
	go func() { externalDone <- c.SendBinary([]byte("abcdefghijklmnop")) }()
	waitSignal(t, raw.firstFragment, "external first fragment")
	close(resume)
	select {
	case <-raw.batchFlushed:
	case <-time.After(100 * time.Millisecond):
	}
	close(raw.releaseFragment)
	if err := <-externalDone; err != nil {
		t.Fatal(err)
	}
	if err := <-roundDone; err != nil {
		t.Fatal(err)
	}
	if err := <-handlerErr; err != nil {
		t.Fatal(err)
	}
	inside := false
	for i, f := range raw.frames(t) {
		if f.Opcode == frame.Binary {
			if inside {
				t.Fatalf("new Binary frame interrupted unfinished external message at frame %d: fin=%v payload=%q", i, f.Fin, f.Payload)
			}
			inside = !f.Fin
		} else if f.Opcode == frame.Continuation {
			inside = !f.Fin
		}
	}
}

func TestFragmentAdmissionRacesBatchMutation(t *testing.T) {
	raw := &limitedWire{limit: 100 << 20}
	c := newLimitedConn(raw, time.Hour)
	c.config = testServerConfig(NewServer(Options{MaxOutboundBuffered: 100 << 20, OutboundHighWatermark: -1, MaxFramePayload: 8}, nil))
	start := make(chan struct{})
	c.handler = &batchEchoHandler{onMessage: func(c *Conn, _ Message) {
		<-start
		for i := 0; i < 5000; i++ {
			if err := c.SendBinary([]byte("12345678")); err != nil {
				return
			}
		}
	}}
	raw.inbound = maskedFrames("one", "two")
	done := make(chan error, 1)
	go func() { done <- c.readAvailable() }()
	close(start)
	for i := 0; i < 5000; i++ {
		if err := c.SendBinary([]byte("abcdefghijklmnop")); err != nil {
			t.Fatal(fmt.Errorf("external send: %w", err))
		}
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestHeartbeatBatchRespectsQuota(t *testing.T) {
	raw := &limitedWire{limit: 64}
	c := newLimitedConn(raw, time.Hour)
	c.heartbeat = &heartbeatState{}
	var sends []error
	c.handler = &batchEchoHandler{onMessage: func(c *Conn, m Message) {
		sends = append(sends, c.SendBinary(m.Payload))
	}}
	raw.inbound = maskedFrames(string(bytes.Repeat([]byte("a"), 40)), string(bytes.Repeat([]byte("b"), 40)))
	if err := c.readAvailable(); err != nil {
		t.Fatal(err)
	}
	held := 0
	if c.batch != nil {
		held = c.batch.Len()
	}
	if len(sends) != 2 || sends[0] != nil || !errors.Is(sends[1], ErrBackpressure) || held != 0 || len(raw.frames(t)) != 1 {
		t.Fatalf("quota failed: sends=%v delivered=%d retained=%d transport=%d pending=%d", sends, len(raw.frames(t)), held, raw.queuedBytes(), c.writes.close.pendingBytes.Load())
	}
}

type snapshotGateWire struct {
	limitedWire
	gate    atomic.Bool
	entered chan struct{}
	resume  chan struct{}
}

func (w *snapshotGateWire) OutboundBuffered() int {
	queued := w.limitedWire.OutboundBuffered()
	if w.gate.CompareAndSwap(false, true) {
		close(w.entered)
		<-w.resume
	}
	return queued
}

func TestQuotaReservationIsAtomic(t *testing.T) {
	raw := &snapshotGateWire{limitedWire: limitedWire{limit: 64}, entered: make(chan struct{}), resume: make(chan struct{})}
	c := newLimitedConn(&raw.limitedWire, time.Hour)
	c.raw = raw
	first := true
	accepted := make(chan error, 1)
	c.handler = &batchEchoHandler{onMessage: func(c *Conn, _ Message) {
		if first {
			first = false
			accepted <- c.SendBinary(bytes.Repeat([]byte("a"), 40))
		}
	}}
	raw.inbound = maskedFrames("one", "two")
	done := make(chan error, 1)
	go func() { done <- c.readAvailable() }()
	waitSignal(t, raw.entered, "round's quota snapshot")
	externalDone := make(chan error, 1)
	go func() { externalDone <- c.SendBinary(bytes.Repeat([]byte("b"), 40)) }()
	var externalErr error
	select {
	case externalErr = <-externalDone:
		close(raw.resume)
	case <-time.After(100 * time.Millisecond):
		// The locked baseline cannot reserve until the round releases its lock.
		close(raw.resume)
		externalErr = <-externalDone
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	batchErr := <-accepted
	held := 0
	if c.batch != nil {
		held = c.batch.Len()
	}
	if raw.queuedBytes()+held > raw.limit {
		t.Fatalf("non-atomic quota: round send=%v external send=%v transport=%d retained=%d limit=%d", batchErr, externalErr, raw.queuedBytes(), held, raw.limit)
	}
}

// captureEchoHandler echoes every message and publishes its connection, so a
// test can send from a goroutine of its own while a read round is batching.
type captureEchoHandler struct {
	conn chan *Conn
	seen chan int
	once sync.Once
}

func newCaptureEchoHandler() *captureEchoHandler {
	return &captureEchoHandler{conn: make(chan *Conn, 1), seen: make(chan int, 1024)}
}

func (h *captureEchoHandler) OnOpen(c *Conn) {
	h.once.Do(func() { h.conn <- c })
}

func (h *captureEchoHandler) OnMessage(c *Conn, m Message) {
	select {
	case h.seen <- len(m.Payload):
	default:
	}
	_ = c.SendBinary(m.Payload)
}

func (h *captureEchoHandler) OnClose(*Conn, CloseEvent) {}

// startEchoServer runs a server on an ephemeral port and returns its address.
func startEchoServer(t *testing.T, handler Handler) string {
	t.Helper()
	return startRobustServer(t, Options{Pollers: 1}, handler)
}

// dialTestWS opens a raw WebSocket client to addr and completes the upgrade.
func dialTestWS(t *testing.T, addr string) (net.Conn, *bufio.Reader) {
	t.Helper()
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
	t.Cleanup(func() { _ = conn.Close() })
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
	return conn, reader
}

func echoPayload(i int) []byte {
	payload := make([]byte, 256)
	binary.BigEndian.PutUint32(payload, uint32(i))
	for j := 4; j < len(payload); j++ {
		payload[j] = byte(i)
	}
	return payload
}

func externalPayload(j int) []byte {
	payload := make([]byte, 64)
	copy(payload, fmt.Sprintf("ext-%04d", j))
	for i := len(fmt.Sprintf("ext-%04d", j)); i < len(payload); i++ {
		payload[i] = byte(j)
	}
	return payload
}

// A handler that echoes while another goroutine sends to the same connection
// keeps every frame whole: the round's replies and the outside sends never
// share a buffer, so no message is truncated, merged or byte-interleaved.
// Each sender's order holds; the order between them does not.
func TestConcurrentSendsKeepFramesWhole(t *testing.T) {
	const (
		echoes    = 4096
		externals = 512
	)
	handler := newCaptureEchoHandler()
	addr := startEchoServer(t, handler)
	conn, reader := dialTestWS(t, addr)
	var serverConn *Conn
	select {
	case serverConn = <-handler.conn:
	case <-time.After(testIOTimeout()):
		t.Fatal("OnOpen never ran")
	}

	externalDone := make(chan error, 1)
	go func() {
		for j := 0; j < externals; j++ {
			if err := serverConn.SendBinary(externalPayload(j)); err != nil {
				externalDone <- fmt.Errorf("external %d: %w", j, err)
				return
			}
			if j%16 == 0 {
				time.Sleep(100 * time.Microsecond)
			}
		}
		externalDone <- nil
	}()

	// Read replies while writing, so the server never pauses its reads.
	if err := conn.SetReadDeadline(time.Now().Add(testIOTimeout())); err != nil {
		t.Fatal(err)
	}
	frames := make(chan []byte, echoes+externals)
	readErr := make(chan error, 1)
	go func() {
		defer close(frames)
		for i := 0; i < echoes+externals; i++ {
			payload, err := readEchoFrame(reader)
			if err != nil {
				readErr <- err
				return
			}
			frames <- payload
		}
		readErr <- nil
	}()

	maskKey := [4]byte{1, 2, 3, 4}
	var wire []byte
	for i := 0; i < echoes; i++ {
		wire = frame.Append(wire[:0], frame.Frame{Fin: true, Opcode: frame.Binary, Masked: true, Payload: echoPayload(i)}, maskKey)
		if _, err := conn.Write(wire); err != nil {
			t.Fatalf("write echo %d: %v", i, err)
		}
	}

	nextEcho, nextExternal := 0, 0
	for payload := range frames {
		switch len(payload) {
		case 256:
			if nextEcho >= echoes {
				t.Fatalf("more echoes than sent")
			}
			if want := echoPayload(nextEcho); string(payload) != string(want) {
				t.Fatalf("echo %d corrupted", nextEcho)
			}
			nextEcho++
		case 64:
			if nextExternal >= externals {
				t.Fatalf("more external sends than made")
			}
			if want := externalPayload(nextExternal); string(payload) != string(want) {
				t.Fatalf("external %d corrupted or out of order", nextExternal)
			}
			nextExternal++
		default:
			t.Fatalf("unexpected frame of %d bytes", len(payload))
		}
	}
	if err := <-readErr; err != nil {
		t.Fatalf("after echo %d, external %d: %v", nextEcho, nextExternal, err)
	}
	if nextEcho != echoes || nextExternal != externals {
		t.Fatalf("received %d echoes and %d outside sends, want %d and %d", nextEcho, nextExternal, echoes, externals)
	}
	select {
	case err := <-externalDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(testIOTimeout()):
		t.Fatal("external sends did not finish")
	}
}

// A Close from another goroutine while a read round is batching waits for the
// round's replies: the peer sees every accepted echo before the Close frame
// and nothing after it.
func TestExternalCloseFollowsTheRoundsReplies(t *testing.T) {
	const total = 4096
	handler := newCaptureEchoHandler()
	addr := startEchoServer(t, handler)
	conn, reader := dialTestWS(t, addr)
	var serverConn *Conn
	select {
	case serverConn = <-handler.conn:
	case <-time.After(testIOTimeout()):
		t.Fatal("OnOpen never ran")
	}

	// Stream requests; the server is batching its replies while it reads.
	var wire []byte
	maskKey := [4]byte{5, 6, 7, 8}
	go func() {
		for i := 0; i < total; i++ {
			wire = frame.Append(wire[:0], frame.Frame{Fin: true, Opcode: frame.Binary, Masked: true, Payload: echoPayload(i)}, maskKey)
			if _, err := conn.Write(wire); err != nil {
				return
			}
		}
	}()
	// Close from a goroutine of its own once replies are certainly flowing.
	time.Sleep(20 * time.Millisecond)
	if err := serverConn.Close(1000, "bye"); err != nil && err != ErrClosed {
		t.Fatalf("close: %v", err)
	}

	// Read until the Close frame: the echoes before it must be whole and in
	// order, and nothing may follow it.
	if err := conn.SetReadDeadline(time.Now().Add(testIOTimeout())); err != nil {
		t.Fatal(err)
	}
	for next := 0; ; {
		opcode, payload, err := readFrameWithOpcode(reader)
		if err != nil {
			t.Fatalf("after %d echoes: %v", next, err)
		}
		switch opcode {
		case frame.Binary:
			if want := echoPayload(next); string(payload) != string(want) {
				t.Fatalf("echo %d corrupted before the Close frame", next)
			}
			next++
		case frame.Close:
			// Answer the close handshake, then make sure no data follows.
			reply := frame.Append(nil, frame.Frame{Fin: true, Opcode: frame.Close, Masked: true, Payload: payload}, maskKey)
			_, _ = conn.Write(reply)
			if err := conn.SetReadDeadline(time.Now().Add(250 * time.Millisecond)); err != nil {
				t.Fatal(err)
			}
			if extra, extraPayload, err := readFrameWithOpcode(reader); err == nil {
				t.Fatalf("frame after the Close frame: opcode %d, %d bytes", extra, len(extraPayload))
			}
			return
		default:
			t.Fatalf("unexpected opcode %d before the Close frame", opcode)
		}
	}
}

// readFrameWithOpcode reads one unmasked server frame and reports its opcode.
func readFrameWithOpcode(reader *bufio.Reader) (frame.OpCode, []byte, error) {
	var header [2]byte
	if _, err := io.ReadFull(reader, header[:]); err != nil {
		return 0, nil, err
	}
	length := int(header[1] & 0x7f)
	switch length {
	case 126:
		var ext [2]byte
		if _, err := io.ReadFull(reader, ext[:]); err != nil {
			return 0, nil, err
		}
		length = int(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err := io.ReadFull(reader, ext[:]); err != nil {
			return 0, nil, err
		}
		length = int(binary.BigEndian.Uint64(ext[:]))
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(reader, payload); err != nil {
		return 0, nil, err
	}
	return frame.OpCode(header[0] & 0x0f), payload, nil
}

// limitedWire models UIO's outbound limit: it accepts a write only when the
// write fits beside the bytes still queued, keeps the accepted stream in
// order, and retires queued bytes on demand. Writes are safe from any
// goroutine; the inbound side belongs to the goroutine driving readAvailable.
type limitedWire struct {
	writeProbeConn
	mu         sync.Mutex
	limit      int
	queued     int
	wire       bytes.Buffer
	closeCalls int
	closedCh   chan struct{}
	inbound    []byte
}

func (c *limitedWire) admitLocked(n int) bool { return c.limit <= 0 || c.queued+n <= c.limit }

func (c *limitedWire) WriteOwned(buffer *uio.Buffer) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := buffer.Len()
	if !c.admitLocked(n) {
		// An overflowed buffer stays the caller's, like the real transport.
		return 0, uio.ErrOutboundOverflow
	}
	c.queued += n
	c.wire.Write(buffer.Bytes())
	uio.ReleaseBuffer(buffer)
	return n, nil
}

func (c *limitedWire) Writev(vec [][]byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	total := 0
	for _, segment := range vec {
		total += len(segment)
	}
	if !c.admitLocked(total) {
		return 0, uio.ErrOutboundOverflow
	}
	c.queued += total
	for _, segment := range vec {
		c.wire.Write(segment)
	}
	return total, nil
}

func (c *limitedWire) OutboundBuffered() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.queued
}

func (c *limitedWire) Flush() error { return nil }

func (c *limitedWire) CloseWith(error) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closeCalls++
	if c.closeCalls == 1 && c.closedCh != nil {
		close(c.closedCh)
	}
	return nil
}

func (c *limitedWire) closeCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closeCalls
}

func (c *limitedWire) queuedBytes() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.queued
}

// retire drains up to n queued bytes and reports them the way UIO's
// OnOutbound callback does.
func (c *limitedWire) retire(conn *Conn, n int) {
	c.mu.Lock()
	n = min(n, c.queued)
	c.queued -= n
	c.mu.Unlock()
	conn.releaseOutbound(n)
	_, _ = conn.tryCloseTransport()
}

func (c *limitedWire) frames(t *testing.T) []frame.Frame {
	t.Helper()
	c.mu.Lock()
	wire := append([]byte(nil), c.wire.Bytes()...)
	c.mu.Unlock()
	reader := bufio.NewReader(bytes.NewReader(wire))
	var frames []frame.Frame
	for reader.Buffered() > 0 || len(wire) > 0 {
		first, payload, fin, err := readServerFrame(reader)
		if err != nil {
			break
		}
		frames = append(frames, frame.Frame{Opcode: frame.OpCode(first & 0x0f), Payload: payload, Fin: fin})
	}
	return frames
}

func (c *limitedWire) PeekChunk() []byte { return c.inbound }

func (c *limitedWire) InboundBuffered() int { return len(c.inbound) }

func (c *limitedWire) Discard(n int) (int, error) {
	n = min(n, len(c.inbound))
	c.inbound = c.inbound[n:]
	return n, nil
}

func newLimitedConn(raw *limitedWire, closeTimeout time.Duration) *Conn {
	server := NewServer(Options{}, nil)
	server.Options.MaxOutboundBuffered = raw.limit
	server.Options.OutboundHighWatermark = -1 // refusal paths, not the read pause
	server.CloseTimeout = closeTimeout
	conn := &Conn{raw: raw, config: testServerConfig(server)}
	conn.opened.Store(true)
	return conn
}

func TestRepeatedTransportCloseHasOneOwner(t *testing.T) {
	raw := newScriptedConn()
	conn := testServerConn(raw)
	if err := conn.closeTransport(); err != nil {
		t.Fatal(err)
	}
	if err := conn.closeTransport(); err != nil {
		t.Fatal(err)
	}
	if raw.closes != 1 || conn.writes.close.phase() != transportCloseClaimed {
		t.Fatalf("transport closes = %d, phase = %d", raw.closes, conn.writes.close.phase())
	}
}

// A Close frame the outbound limit has no room for waits behind the bytes
// already accepted instead of failing and leaving the connection open.
func TestCloseWaitsForOutboundRoom(t *testing.T) {
	raw := &limitedWire{limit: 64}
	conn := newLimitedConn(raw, time.Hour)
	if err := conn.SendBinary(bytes.Repeat([]byte("x"), 60)); err != nil {
		t.Fatal(err)
	}
	if err := conn.Close(1000, "bye"); err != nil {
		t.Fatalf("Close with a full outbound queue = %v, want nil", err)
	}
	if !conn.closing.Load() || !conn.writes.close.closeIsDeferred() {
		t.Fatalf("closing:%v deferred:%v, want both", conn.closing.Load(), conn.writes.close.closeIsDeferred())
	}
	if err := conn.SendBinary([]byte("late")); !errors.Is(err, ErrClosed) {
		t.Fatalf("send after Close = %v, want %v", err, ErrClosed)
	}
	if frames := raw.frames(t); len(frames) != 1 {
		t.Fatalf("wire = %d frames before the queue drained, want only the message", len(frames))
	}

	raw.retire(conn, raw.queuedBytes())
	frames := raw.frames(t)
	if len(frames) != 2 || frames[0].Opcode != frame.Binary || frames[1].Opcode != frame.Close {
		t.Fatalf("wire = %v, want the message then the Close frame", frames)
	}
	if code := frame.CloseCode(frames[1].Payload); code != 1000 || string(frames[1].Payload[2:]) != "bye" {
		t.Fatalf("Close frame = %d %q", code, frames[1].Payload[2:])
	}
	if conn.writes.close.closeIsDeferred() || !conn.writes.close.closeFrameWasSent() {
		t.Fatal("sent Close frame is still marked deferred")
	}

	// The peer answers; the transport closes once our frame has retired.
	if err := conn.acceptControl(frame.Frame{Fin: true, Opcode: frame.Close, Payload: []byte{3, 232}}); err != nil {
		t.Fatal(err)
	}
	if frames := raw.frames(t); len(frames) != 2 {
		t.Fatalf("wire = %d frames, want no second Close frame", len(frames))
	}
	if raw.closeCount() != 0 {
		t.Fatal("transport closed before the Close frame retired")
	}
	raw.retire(conn, raw.queuedBytes())
	if raw.closeCount() != 1 || conn.writes.close.phase() != transportCloseClaimed {
		t.Fatalf("closes = %d, phase = %d", raw.closeCount(), conn.writes.close.phase())
	}
}

// The reply to a peer's Close also waits for room, and the transport stays
// open until the reply went out.
func TestPeerCloseReplyWaitsForOutboundRoom(t *testing.T) {
	raw := &limitedWire{limit: 64}
	conn := newLimitedConn(raw, time.Hour)
	if err := conn.SendBinary(bytes.Repeat([]byte("x"), 60)); err != nil {
		t.Fatal(err)
	}
	if err := conn.acceptControl(frame.Frame{Fin: true, Opcode: frame.Close, Payload: []byte{3, 232}}); err != nil {
		t.Fatal(err)
	}
	if !conn.writes.close.closeIsDeferred() || raw.closeCount() != 0 {
		t.Fatalf("deferred:%v closes:%d, want a deferred reply and an open transport", conn.writes.close.closeIsDeferred(), raw.closeCount())
	}
	raw.retire(conn, 62)
	frames := raw.frames(t)
	if len(frames) != 2 || frames[1].Opcode != frame.Close || frame.CloseCode(frames[1].Payload) != 1000 {
		t.Fatalf("wire = %v, want the message then the Close reply", frames)
	}
	if raw.closeCount() != 0 {
		t.Fatal("transport closed before the Close reply retired")
	}
	raw.retire(conn, raw.queuedBytes())
	if raw.closeCount() != 1 {
		t.Fatalf("closes = %d, want 1", raw.closeCount())
	}
}

// A peer that never drains cannot hold a deferred Close forever.
func TestCloseTimeoutAbortsDeferredClose(t *testing.T) {
	raw := &limitedWire{limit: 64, closedCh: make(chan struct{})}
	conn := newLimitedConn(raw, 5*time.Millisecond)
	if err := conn.SendBinary(bytes.Repeat([]byte("x"), 60)); err != nil {
		t.Fatal(err)
	}
	if err := conn.Close(1000, ""); err != nil {
		t.Fatal(err)
	}
	select {
	case <-raw.closedCh:
	case <-time.After(testIOTimeout()):
		t.Fatal("close timeout did not abort a deferred Close")
	}
	if raw.closeCount() != 1 || conn.writes.close.phase() != transportCloseClaimed || conn.writes.close.closeIsDeferred() {
		t.Fatalf("closes:%d phase:%d deferred:%v", raw.closeCount(), conn.writes.close.phase(), conn.writes.close.closeIsDeferred())
	}
	// Room freed after the abort must not resurrect the Close frame.
	raw.retire(conn, raw.queuedBytes())
	if frames := raw.frames(t); len(frames) != 1 {
		t.Fatalf("wire = %d frames after the abort, want only the message", len(frames))
	}
}

// drainOnRefusalWire drains the transport while it refuses its second write,
// so the retirement lands after the refusal and before the frame is deferred
// again, when it finds nothing to send.
type drainOnRefusalWire struct {
	*limitedWire
	conn     *Conn
	refusals int
}

func (c *drainOnRefusalWire) WriteOwned(buffer *uio.Buffer) (int, error) {
	n, err := c.limitedWire.WriteOwned(buffer)
	if errors.Is(err, uio.ErrOutboundOverflow) {
		c.refusals++
		if c.refusals == 2 {
			c.limitedWire.retire(c.conn, c.limitedWire.queuedBytes())
		}
	}
	return n, err
}

func TestDeferredCloseNoticesRoomFreedDuringRefusal(t *testing.T) {
	limited := &limitedWire{limit: 64}
	raw := &drainOnRefusalWire{limitedWire: limited}
	server := NewServer(Options{}, nil)
	server.Options.MaxOutboundBuffered = limited.limit
	server.Options.OutboundHighWatermark = -1 // refusal paths, not the read pause
	server.CloseTimeout = time.Hour
	conn := &Conn{raw: raw, config: testServerConfig(server)}
	conn.opened.Store(true)
	raw.conn = conn
	if err := conn.SendBinary(bytes.Repeat([]byte("x"), 60)); err != nil {
		t.Fatal(err)
	}
	if err := conn.Close(1000, ""); err != nil {
		t.Fatal(err)
	}
	if raw.refusals != 2 {
		t.Fatalf("refusals = %d, want the Close attempt and the deferred attempt", raw.refusals)
	}
	frames := limited.frames(t)
	if len(frames) != 2 || frames[1].Opcode != frame.Close {
		t.Fatalf("wire = %v, want the Close frame sent once room freed", frames)
	}
	if conn.writes.close.closeIsDeferred() {
		t.Fatal("Close frame stayed deferred with room available")
	}
}

// Senders on any goroutine never fail because another sender is mid-submit:
// every message is accepted and reaches the transport once.
func TestConcurrentSendersNeverSeeContention(t *testing.T) {
	raw := &limitedWire{}
	conn := newLimitedConn(raw, time.Hour)
	const senders, perSender = 8, 500
	start := make(chan struct{})
	errs := make(chan error, senders)
	var wg sync.WaitGroup
	for i := range senders {
		wg.Add(1)
		go func(id byte) {
			defer wg.Done()
			<-start
			for range perSender {
				var err error
				if id%2 == 0 {
					err = conn.SendBinary([]byte{id})
				} else {
					err = conn.Ping([]byte{id})
				}
				if err != nil {
					errs <- err
					return
				}
			}
		}(byte(i))
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent send = %v", err)
	}
	if got := len(raw.frames(t)); got != senders*perSender {
		t.Fatalf("frames = %d, want %d", got, senders*perSender)
	}
}

// A Close racing senders splits them cleanly: every send lands before the
// Close frame or reports ErrClosed, and nothing follows the Close frame.
func TestCloseRacingSendersKeepsStreamOrder(t *testing.T) {
	raw := &limitedWire{}
	conn := newLimitedConn(raw, time.Hour)
	const senders = 4
	var accepted atomic.Int64
	errs := make(chan error, senders)
	var wg sync.WaitGroup
	for range senders {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				err := conn.SendBinary([]byte("data"))
				if errors.Is(err, ErrClosed) {
					return
				}
				if err != nil {
					errs <- err
					return
				}
				accepted.Add(1)
			}
		}()
	}
	for accepted.Load() < 100 {
		time.Sleep(time.Millisecond)
	}
	if err := conn.Close(1000, ""); err != nil {
		t.Fatalf("Close racing senders = %v", err)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("send racing Close = %v", err)
	}
	frames := raw.frames(t)
	if int64(len(frames)) != accepted.Load()+1 {
		t.Fatalf("frames = %d, want %d accepted messages and one Close frame", len(frames), accepted.Load())
	}
	for i, f := range frames[:len(frames)-1] {
		if f.Opcode != frame.Binary {
			t.Fatalf("frame %d opcode = %v before the Close frame", i, f.Opcode)
		}
	}
	if last := frames[len(frames)-1]; last.Opcode != frame.Close {
		t.Fatalf("last frame opcode = %v, want Close", last.Opcode)
	}
}

// A read round's batch is handed over when the round ends even while other
// goroutines keep taking the write lock, so no accepted reply is stranded.
func TestReadRoundsAndExternalSendersNeverStrandBatch(t *testing.T) {
	raw := &limitedWire{}
	handler := &batchEchoHandler{}
	conn := newBatchConn(raw, handler)
	handler.onMessage = func(conn *Conn, message Message) {
		if err := conn.SendBinary(message.Payload); err != nil {
			t.Errorf("echo = %v", err)
		}
	}
	stop := make(chan struct{})
	var external atomic.Int64
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if err := conn.SendBinary([]byte("ext")); err != nil {
					t.Errorf("external send = %v", err)
					return
				}
				external.Add(1)
			}
		}()
	}
	const rounds = 2000
	for range rounds {
		raw.inbound = maskedFrames("a", "b", "c")
		if err := conn.readAvailable(); err != nil {
			t.Fatal(err)
		}
	}
	close(stop)
	wg.Wait()
	conn.writes.mu.Lock()
	stranded := conn.batch != nil
	conn.writes.mu.Unlock()
	if stranded {
		t.Fatal("a write batch was left behind")
	}
	want := rounds*3 + int(external.Load())
	if got := len(raw.frames(t)); got != want {
		t.Fatalf("frames = %d, want %d", got, want)
	}
	if pending := conn.writes.close.pendingBytes.Load(); pending != int64(raw.queuedBytes()) {
		t.Fatalf("pending = %d, transport queued = %d", pending, raw.queuedBytes())
	}
}

// Replies batched in a read round are counted as pending only when the batch
// is handed over, so a peer Close in the same round must still not let the
// transport close before they and the Close reply have retired.
func TestPeerCloseInARoundWaitsForTheBatchedReplies(t *testing.T) {
	raw := &limitedWire{}
	conn := newBatchConn(raw, &batchEchoHandler{})
	raw.inbound = append(maskedFrames("a", "bb"), frame.Append(nil, frame.Frame{
		Fin: true, Opcode: frame.Close, Masked: true, Payload: []byte{3, 232},
	}, [4]byte{1, 2, 3, 4})...)
	raw.retire(conn, 0)
	if err := conn.readAvailable(); err != nil && !errors.Is(err, ErrClosed) {
		t.Fatal(err)
	}
	if raw.closeCount() != 0 {
		t.Fatal("transport closed before the batched replies went out")
	}
	frames := raw.frames(t)
	if len(frames) != 3 || string(frames[0].Payload) != "a" || string(frames[1].Payload) != "bb" || frames[2].Opcode != frame.Close {
		t.Fatalf("wire = %v, want both replies then the Close reply", frames)
	}
	if pending := conn.writes.close.pendingBytes.Load(); pending != int64(raw.queuedBytes()) {
		t.Fatalf("pending = %d, transport queued = %d", pending, raw.queuedBytes())
	}
	raw.retire(conn, 3+4)
	if raw.closeCount() != 0 {
		t.Fatal("transport closed before the Close reply retired")
	}
	raw.retire(conn, raw.queuedBytes())
	if raw.closeCount() != 1 {
		t.Fatalf("closes = %d, want 1", raw.closeCount())
	}
}
