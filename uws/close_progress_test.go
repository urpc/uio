package uws

import (
	"bufio"
	"bytes"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urpc/uio"
	"github.com/urpc/uio/uws/internal/frame"
)

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
	server := NewServer(nil)
	server.Events.MaxOutboundBuffered = raw.limit
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
	server := NewServer(nil)
	server.Events.MaxOutboundBuffered = limited.limit
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
