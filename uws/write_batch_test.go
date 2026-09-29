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
	server := NewServer(nil)
	server.Events.MaxOutboundBuffered = raw.limit
	conn := &Conn{raw: raw, handler: handler, config: testServerConfig(server)}
	conn.opened.Store(true)
	// One reply per block, so the batch fills and is handed over mid-round.
	conn.config.batchBlockSize = 64
	refused := 0
	handler.onMessage = func(conn *Conn, message Message) {
		switch err := conn.SendBinary(message.Payload); {
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
