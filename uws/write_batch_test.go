package uws

import (
	"bufio"
	"bytes"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urpc/uio"
	"github.com/urpc/uio/uws/internal/frame"
)

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
	_ = conn.SendBinary(message.Payload)
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
	conn := &Conn{raw: raw, handler: handler, config: testServerConfig(NewServer(nil))}
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
		_ = conn.SendBinary(message.Payload)
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
	server := NewServer(nil)
	server.Events.MaxOutboundBuffered = 64
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

func TestBatchHandedOffByWriterHoldingLock(t *testing.T) {
	raw := &batchProbeConn{}
	var writer *Writer
	handler := &batchEchoHandler{}
	handler.onMessage = func(conn *Conn, message Message) {
		if string(message.Payload) == "echo" {
			_ = conn.SendBinary(message.Payload)
			return
		}
		var err error
		if writer, err = conn.BeginMessage(BinaryMessage); err != nil {
			t.Errorf("BeginMessage = %v", err)
		}
	}
	conn := newBatchConn(raw, handler)
	raw.inbound = maskedFrames("echo", "open-writer")
	if err := conn.readAvailable(); err != nil {
		t.Fatal(err)
	}
	if writer == nil {
		t.Fatal("writer was not opened")
	}
	// The round ended while the writer held the lock: its reply waits for the
	// writer, which hands it over before its own frames.
	if _, err := writer.Write([]byte("streamed")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	got := raw.payloads(t)
	if len(got) < 2 || got[0] != "echo" || got[1] != "streamed" {
		t.Fatalf("replies = %q, want echo before the streamed message", got)
	}
	if conn.batch != nil {
		t.Fatal("batch left behind after writer closed")
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
	conn := &Conn{raw: raw, handler: &batchEchoHandler{}, config: testServerConfig(NewServer(nil))}
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
	conn := &Conn{raw: raw, handler: &batchEchoHandler{}, config: testServerConfig(NewServer(nil))}
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

// A Ping that arrives while a streaming Writer holds the write lock must not
// overtake the messages the round batched before the Writer began.
func TestPingUnderWriterDoesNotOvertakeBatch(t *testing.T) {
	raw := &batchProbeConn{}
	var writer *Writer
	handler := &batchEchoHandler{}
	handler.onMessage = func(conn *Conn, message Message) {
		switch string(message.Payload) {
		case "data":
			_ = conn.SendBinary(message.Payload)
		case "open-writer":
			var err error
			if writer, err = conn.BeginMessage(BinaryMessage); err != nil {
				t.Errorf("BeginMessage = %v", err)
			}
		}
	}
	conn := newBatchConn(raw, handler)
	raw.inbound = append(maskedFrames("data", "open-writer"), pingWire("k")...)
	if err := conn.readAvailable(); err != nil {
		t.Fatal(err)
	}
	if writer == nil {
		t.Fatal("writer was not opened")
	}
	// The Pong bypassed the held lock as designed, but only after the batch
	// the round had accepted went out ahead of it.
	if _, err := writer.Write([]byte("streamed")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	frames := wireFrames(t, raw)
	// The batched echo, then the bypassing Pong, then the Writer's two
	// fragments of "streamed" (payload piece and the final empty continuation).
	want := []frame.OpCode{frame.Binary, frame.Pong, frame.Binary, frame.Continuation}
	if len(frames) != len(want) {
		t.Fatalf("wire = %v", frames)
	}
	for i, opcode := range want {
		if frames[i].Opcode != opcode {
			t.Fatalf("wire frame %d opcode = %v, want %v", i, frames[i].Opcode, opcode)
		}
	}
	if frames[2].Fin || !frames[3].Fin {
		t.Fatalf("streamed fragments finalized out of order: %+v", frames[2:])
	}
	if string(frames[0].Payload) != "data" || string(frames[2].Payload) != "streamed" {
		t.Fatalf("wire payloads = %q, %q", frames[0].Payload, frames[2].Payload)
	}
	if raw.closes != 0 || conn.closing.Load() || conn.batch != nil {
		t.Fatalf("closes:%d closing:%v batch:%v, want a clean ordered round", raw.closes, conn.closing.Load(), conn.batch != nil)
	}
}

// A Ping arriving while a momentary lock holder keeps the batch from being
// handed over parks behind the batch instead of overtaking it.
func TestPongParksBehindHeldBatch(t *testing.T) {
	raw := &batchProbeConn{}
	handler := &batchEchoHandler{}
	release := make(chan struct{})
	held := make(chan struct{})
	released := make(chan struct{})
	handler.onMessage = func(conn *Conn, message Message) {
		if string(message.Payload) == "echo" {
			_ = conn.SendBinary(message.Payload)
			// Stand in for an external sender mid-submit: hold the write lock
			// while the round keeps reading.
			go func() {
				defer close(released)
				if !conn.writes.mu.TryLock() {
					t.Errorf("external sender could not take the write lock")
					close(held)
					return
				}
				close(held)
				<-release
				conn.unlockWrite()
			}()
			<-held
			return
		}
		_ = conn.SendBinary(message.Payload)
	}
	conn := newBatchConn(raw, handler)
	raw.inbound = append(maskedFrames("echo", "tail"), pingWire("k")...)
	if err := conn.readAvailable(); err != nil {
		t.Fatal(err)
	}
	if raw.handoffs != 0 {
		t.Fatalf("hand-offs before release = %d, want 0", raw.handoffs)
	}
	if order := conn.batchOrder.Load(); order == nil || len(order.parked) != 1 {
		t.Fatalf("parked controls = %v, want 1", order)
	}
	close(release)
	select {
	case <-released:
	case <-time.After(testIOTimeout()):
		t.Fatal("held lock never handed the batch over")
	}
	if raw.handoffs != 2 {
		t.Fatalf("hand-offs = %d, want 2 (batch, then parked Pong)", raw.handoffs)
	}
	frames := wireFrames(t, raw)
	if len(frames) != 2 || frames[0].Opcode != frame.Binary || frames[1].Opcode != frame.Pong {
		t.Fatalf("wire = %v, want the batched echo before the parked Pong", frames)
	}
	if string(frames[0].Payload) != "echo" {
		t.Fatalf("first payload = %q, want the batched echo", frames[0].Payload)
	}
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

// A full transport refuses the batch hand-off: the accepted frames stay
// queued, and the retirement callback retries them once room frees.
func TestFullTransportDefersBatchHandoff(t *testing.T) {
	raw := &limitProbeConn{atLimit: true}
	conn := newBatchConn(raw, &batchEchoHandler{})
	raw.inbound = maskedFrames("a", "b")
	if err := conn.readAvailable(); err != nil {
		t.Fatal(err)
	}
	if raw.closes != 0 || conn.closing.Load() {
		t.Fatalf("deferred hand-off closed the connection: closes:%d closing:%v", raw.closes, conn.closing.Load())
	}
	if order := conn.batchOrder.Load(); conn.batch == nil || order == nil || !order.held.Load() || !order.retry.Load() {
		t.Fatal("refused hand-off did not keep the batch queued")
	}
	if raw.handoffs != 0 || raw.wire.Len() != 0 {
		t.Fatalf("hand-offs = %d wire = %d bytes, want nothing submitted", raw.handoffs, raw.wire.Len())
	}
	// The transport drains; OnOutbound retries the hand-off.
	raw.atLimit = false
	conn.releaseOutbound(16)
	if conn.batch != nil || (conn.batchOrder.Load() != nil && conn.batchOrder.Load().retry.Load()) {
		t.Fatal("retry did not hand the batch over")
	}
	if got := raw.payloads(t); len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("retried hand-off = %q", got)
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

// A batch that fills mid-round and meets a full transport must keep the frames
// it already accepted; the next message must not overwrite them.
func TestBatchFullFlushKeepsOldBatchOnBackpressure(t *testing.T) {
	// Pooled blocks start at 512 bytes, so replies of 204 wire bytes fill one
	// after two frames and the third finds it full mid-round.
	raw := &limitProbeConn{atLimit: true}
	conn := newBatchConn(raw, &batchEchoHandler{})
	conn.config.batchBlockSize = 64
	first, second := strings.Repeat("a", 200), strings.Repeat("b", 200)
	raw.inbound = maskedFrames(first, second, strings.Repeat("c", 200))
	if err := conn.readAvailable(); err != nil {
		t.Fatal(err)
	}
	if raw.closes != 0 || conn.closing.Load() {
		t.Fatalf("deferred mid-round hand-off closed the connection: closes:%d closing:%v", raw.closes, conn.closing.Load())
	}
	if conn.batch == nil {
		t.Fatal("the accepted batch was overwritten instead of kept")
	}
	order := conn.batchOrder.Load()
	if order == nil || !order.held.Load() || !order.retry.Load() {
		t.Fatal("kept batch lost its ordering state")
	}
	wantPending := int64(2 * frameWireSize(len(first), false)) // two accepted replies
	if pending := conn.writes.close.pendingBytes.Load(); pending != wantPending {
		t.Fatalf("pending bytes = %d, want %d (only the accepted replies)", pending, wantPending)
	}
	// The transport drains; OnOutbound retries the kept batch.
	raw.atLimit = false
	conn.releaseOutbound(int(conn.writes.close.pendingBytes.Load()))
	if conn.batch != nil || (conn.batchOrder.Load() != nil && conn.batchOrder.Load().retry.Load()) {
		t.Fatal("retry did not hand the kept batch over")
	}
	got := raw.payloads(t)
	if len(got) != 2 || got[0] != first || got[1] != second {
		t.Fatalf("delivered = %d frames, want the two accepted replies in order", len(got))
	}
	if pending := conn.writes.close.pendingBytes.Load(); pending != 0 {
		t.Fatalf("pending bytes = %d after delivery", pending)
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

// While the batch is inside WriteOwned, a Pong arriving through the held lock
// must park behind it — the gate may not open before the batch is queued.
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
	if err := conn.sendProtocolControlFrame(frame.Frame{Fin: true, Opcode: frame.Pong, Payload: []byte("k")}); err != nil {
		t.Fatal(err)
	}
	if order := conn.batchOrder.Load(); order == nil || len(order.parked) != 1 {
		t.Fatalf("Pong did not park behind the in-flight batch: %+v", order)
	}
	close(raw.gate)
	select {
	case <-done:
	case <-time.After(testIOTimeout()):
		t.Fatal("hand-off did not complete")
	}
	frames := wireFrames(t, &raw.batchProbeConn)
	if len(frames) != 2 || frames[0].Opcode != frame.Binary || frames[1].Opcode != frame.Pong {
		t.Fatalf("wire = %v, want the batched frame before the parked Pong", frames)
	}
	if string(frames[0].Payload) != "batched" {
		t.Fatalf("first payload = %q", frames[0].Payload)
	}
}

// The read task can pass the batch-gate check and only then reach the park;
// if the hand-off completed in between, the Pong must not strand itself in a
// queue nothing will drain — least of all while a Writer holds the lock.
func TestPongDoesNotWaitForHandoffRace(t *testing.T) {
	raw := &batchProbeConn{}
	conn := newBatchConn(raw, &batchEchoHandler{})
	conn.beginWriteBatch()
	if err := conn.send(BinaryMessage, []byte("batched")); err != nil {
		t.Fatal(err)
	}
	order := conn.batchOrder.Load()
	if order == nil || !order.held.Load() {
		t.Fatal("batch gate not set")
	}
	pong := frame.Frame{Fin: true, Opcode: frame.Pong, Payload: []byte("k")}
	// The frame's gate check passed while the batch was queued; the hand-off
	// then completed and drained an empty parked queue.
	conn.writes.mu.Lock()
	if err := conn.flushBatchLocked(); err != nil {
		conn.writes.mu.Unlock()
		t.Fatal(err)
	}
	conn.writes.mu.Unlock()
	if order.held.Load() {
		t.Fatal("hand-off did not clear the gate")
	}
	parked, err := conn.parkControlFrame(order, pong)
	if err != nil || parked {
		t.Fatalf("parkControlFrame = parked:%v err:%v, want the frame declined", parked, err)
	}
	if len(order.parked) != 0 {
		t.Fatal("stale Pong parked behind a batch that already left")
	}
	// A Writer taking the lock now must not delay the Pong: it goes directly
	// while that lock stays held, instead of waiting for Writer.Close.
	held := make(chan struct{})
	release := make(chan struct{})
	go func() {
		conn.writes.mu.Lock()
		close(held)
		<-release
		conn.writes.mu.Unlock()
	}()
	<-held
	if err := conn.sendProtocolControlFrame(pong); err != nil {
		close(release)
		t.Fatal(err)
	}
	frames := wireFrames(t, raw)
	if len(frames) != 2 || frames[0].Opcode != frame.Binary || frames[1].Opcode != frame.Pong {
		close(release)
		t.Fatalf("wire = %v, want the batched frame and the directly submitted Pong", frames)
	}
	close(release)
}

// rejectPongConn accepts ordinary frames while refusing Pongs, like a
// transport whose outbound limit is full when the parked Pong arrives.
type rejectPongConn struct {
	batchProbeConn
	rejectPongs bool
}

func (c *rejectPongConn) WriteOwned(buffer *uio.Buffer) (int, error) {
	if wire := buffer.Bytes(); c.rejectPongs && len(wire) > 0 && wire[0]&0x0f == byte(frame.Pong) {
		return 0, uio.ErrOutboundOverflow // retained, like the real transport
	}
	return c.batchProbeConn.WriteOwned(buffer)
}

// The parked Pong's bytes must be counted before the queue publishes it: a
// drain that pulls and rejects it rolls back exactly those bytes, never the
// old batch's, so no phantom pending can outlive the batch.
func TestParkedPongCountedBeforeDrain(t *testing.T) {
	raw := &rejectPongConn{}
	conn := newBatchConn(raw, &batchEchoHandler{})
	conn.beginWriteBatch()
	if err := conn.send(BinaryMessage, []byte("batched")); err != nil {
		t.Fatal(err)
	}
	batched := conn.writes.close.pendingBytes.Load()
	if batched == 0 {
		t.Fatal("batch was not accepted")
	}
	order := conn.batchOrder.Load()
	pong := frame.Frame{Fin: true, Opcode: frame.Pong, Payload: []byte("k")}
	pongWire := int64(frameWireSize(len(pong.Payload), false))
	if parked, err := conn.parkControlFrame(order, pong); err != nil || !parked {
		t.Fatalf("parkControlFrame = parked:%v err:%v", parked, err)
	}
	if pending := conn.writes.close.pendingBytes.Load(); pending != batched+pongWire {
		t.Fatalf("pending = %d, want the batch plus the parked Pong (%d)", pending, batched+pongWire)
	}
	// The batch leaves while the Pong is refused: the rollback may only
	// remove the Pong's own bytes.
	raw.rejectPongs = true
	conn.writes.mu.Lock()
	err := conn.flushBatchLocked()
	conn.writes.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if len(order.parked) != 0 {
		t.Fatal("rejected Pong stayed parked")
	}
	if pending := conn.writes.close.pendingBytes.Load(); pending != batched {
		t.Fatalf("pending = %d, want the batch's own bytes (%d)", pending, batched)
	}
	conn.releaseOutbound(int(batched))
	if pending := conn.writes.close.pendingBytes.Load(); pending != 0 {
		t.Fatalf("pending = %d after retirement, want 0 without a phantom", pending)
	}
}

// Concurrently racing the park against the hand-off must never leave bytes
// that no submission will ever retire.
type retireOnWriteConn struct {
	batchProbeConn
	conn *Conn
}

func (c *retireOnWriteConn) WriteOwned(buffer *uio.Buffer) (int, error) {
	// The transport retires everything it has submitted so far, as UIO's
	// outbound callback would while a drain runs.
	if c.conn != nil {
		c.conn.releaseOutbound(int(c.conn.writes.close.pendingBytes.Load()))
	}
	if wire := buffer.Bytes(); len(wire) > 0 && wire[0]&0x0f == byte(frame.Pong) {
		return 0, uio.ErrOutboundOverflow
	}
	return c.batchProbeConn.WriteOwned(buffer)
}

func TestParkDrainRaceKeepsAccounting(t *testing.T) {
	pong := frame.Frame{Fin: true, Opcode: frame.Pong, Payload: []byte("k")}
	for i := 0; i < 200; i++ {
		raw := &retireOnWriteConn{}
		conn := newBatchConn(raw, &batchEchoHandler{})
		raw.conn = conn
		conn.beginWriteBatch()
		if err := conn.send(BinaryMessage, []byte("batched")); err != nil {
			t.Fatal(err)
		}
		order := conn.batchOrder.Load()
		start := make(chan struct{})
		done := make(chan struct{}, 2)
		go func() {
			<-start
			conn.writes.mu.Lock()
			_ = conn.flushBatchLocked()
			conn.writes.mu.Unlock()
			done <- struct{}{}
		}()
		go func() {
			<-start
			_, _ = conn.parkControlFrame(order, pong)
			done <- struct{}{}
		}()
		close(start)
		<-done
		<-done
		if pending := conn.writes.close.pendingBytes.Load(); pending != 0 {
			t.Fatalf("iteration %d left %d phantom pending bytes", i, pending)
		}
	}
}

// BeginMessage must report the backpressure of a batch the limit kept queued
// instead of handing out a Writer whose first write would abort the connection.
func TestBeginMessageReportsBackpressureInsteadOfAborting(t *testing.T) {
	raw := &limitProbeConn{}
	conn := newBatchConn(raw, &batchEchoHandler{})
	conn.beginWriteBatch()
	if err := conn.send(BinaryMessage, []byte("batched")); err != nil {
		t.Fatal(err)
	}
	batched := conn.writes.close.pendingBytes.Load()
	raw.atLimit = true
	writer, err := conn.BeginMessage(BinaryMessage)
	if !errors.Is(err, ErrBackpressure) || writer != nil {
		t.Fatalf("BeginMessage = %v, %v; want ErrBackpressure and no Writer", writer, err)
	}
	if raw.closes != 0 || conn.closing.Load() {
		t.Fatalf("backpressured BeginMessage aborted the connection: closes:%d closing:%v", raw.closes, conn.closing.Load())
	}
	if conn.batch == nil {
		t.Fatal("accepted batch lost its place")
	}
	// The transport drains: the kept batch is delivered, then a Writer fits.
	raw.atLimit = false
	conn.releaseOutbound(int(batched))
	if conn.batch != nil {
		t.Fatal("retry did not hand the kept batch over")
	}
	if got := raw.payloads(t); len(got) != 1 || got[0] != "batched" {
		t.Fatalf("delivered = %q", got)
	}
	writer, err = conn.BeginMessage(BinaryMessage)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = writer.Write([]byte("later")); err != nil {
		t.Fatal(err)
	}
	if err = writer.Close(); err != nil {
		t.Fatal(err)
	}
}
