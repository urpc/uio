package umux

import (
	"bytes"
	"context"
	"errors"
	"github.com/urpc/uio"
	"github.com/urpc/uio/ubridge"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestTimeoutTeardownDespiteTransportError(t *testing.T) {
	h := &recHandler{}
	s, wire := recPair(Config{Handler: h, StreamOpenTimeout: 10 * time.Millisecond})
	wire.failWrite = errors.New("write failed")
	opening := newStream(s, 2, true, int64(s.cfg.WindowSize))
	carrier := newStream(s, 4, false, int64(s.cfg.WindowSize))
	carrier.out = append(carrier.out, frameBuffer(8))
	carrier.queued = true
	s.mu.Lock()
	s.streams[2] = opening
	s.streams[4] = carrier
	s.pending = append(s.pending, 4)
	wait := make(chan error, 1)
	s.openWait[2] = wait
	s.mu.Unlock()
	time.Sleep(20 * time.Millisecond)
	_ = s.onTimer(wire)
	select {
	case err := <-wait:
		if !errors.Is(err, ErrStreamOpenTimeout) {
			t.Fatalf("waiter = %v, want ErrStreamOpenTimeout", err)
		}
	default:
		t.Fatal("open waiter lost when the turn's flush failed")
	}
	if h.closeCount() != 1 {
		t.Fatalf("closes = %d, want 1", h.closeCount())
	}
}

func TestHeldSessionDeliversCloses(t *testing.T) {
	h := &recHandler{}
	s, wire := recPair(Config{Handler: h})
	st := newStream(s, 1, false, int64(s.cfg.WindowSize))
	s.mu.Lock()
	s.streams[1] = st
	s.mu.Unlock()
	if err := st.Abort(errors.New("gone")); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.readHeld = true
	s.pendingBytes = int64(s.cfg.SessionWindow) + 1
	s.mu.Unlock()
	_ = s.onRead(wire)
	if h.closeCount() != 1 {
		t.Fatalf("closes = %d after a held turn, want 1", h.closeCount())
	}
}

func TestConcurrentArmKeepsNewerDeadline(t *testing.T) {
	s, wire := recPair(Config{})
	for round := 0; round < 300; round++ {
		st := newStream(s, 1, false, int64(s.cfg.WindowSize))
		s.mu.Lock()
		s.streams[1] = st
		s.mu.Unlock()
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			_ = st.SetWakeDeadline(time.Now().Add(time.Hour))
		}()
		go func() {
			defer wg.Done()
			_ = st.SetWakeInterval(0)
		}()
		wg.Wait()
		s.mu.Lock()
		want := s.nextTimerLocked(time.Now())
		s.mu.Unlock()
		got := wire.lastDeadline()
		if did, wantd := got.IsZero(), want.IsZero(); did != wantd {
			t.Fatalf("round %d: armed %v while the state wants %v", round, got, want)
		}
	}
}

func TestListenDialOnClosedEvents(t *testing.T) {
	srv := NewServer(Config{})
	_ = srv.Close(nil)
	done := make(chan error, 1)
	go func() {
		_, err := srv.Listen("tcp://127.0.0.1:0")
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Listen on a closed transport succeeded")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Listen on a closed transport blocked")
	}
	cli := NewClient(Config{})
	_ = cli.Close(nil)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := cli.Dial(ctx, "tcp://127.0.0.1:1"); err == nil {
		t.Fatal("Dial on a closed transport succeeded")
	}
}

func TestNoReadAfterClose(t *testing.T) {
	h := &recHandler{}
	s, _ := recPair(Config{Handler: h})
	st := newStream(s, 1, false, int64(s.cfg.WindowSize))
	s.mu.Lock()
	s.streams[1] = st
	s.mu.Unlock()
	_ = st.Wake()
	s.abortStream(st, errors.New("gone"))
	s.drainCloses()
	if h.closeCount() != 1 {
		t.Fatalf("closes = %d, want 1", h.closeCount())
	}
	s.runWakes()
	if err := st.Wake(); err != nil {
		t.Fatal(err)
	}
	s.runWakes()
	h.mu.Lock()
	reads := h.reads
	h.mu.Unlock()
	if reads != 0 {
		t.Fatalf("%d read turns ran after the close callback", reads)
	}
}

type recWire struct {
	uio.Conn
	mu        sync.Mutex
	deadlines []time.Time
	failWrite error
}

func (*recWire) Wake() error {
	return nil
}

func (*recWire) Close() error {
	return nil
}

func (*recWire) Abort(error) error {
	return nil
}

func (*recWire) Write(p []byte) (int, error) {
	return len(p), nil
}

func (c *recWire) SetWakeDeadline(when time.Time) error {
	c.mu.Lock()
	c.deadlines = append(c.deadlines, when)
	c.mu.Unlock()
	return nil
}

func (c *recWire) WriteOwned(b *uio.Buffer) (int, error) {
	c.mu.Lock()
	fail := c.failWrite
	c.mu.Unlock()
	if fail != nil {
		uio.ReleaseBuffer(b)
		return 0, fail
	}
	n := b.Len()
	uio.ReleaseBuffer(b)
	return n, nil
}

func (c *recWire) lastDeadline() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.deadlines) == 0 {
		return time.Time{}
	}
	return c.deadlines[len(c.deadlines)-1]
}

func (c *recWire) deadlineCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.deadlines)
}

type recHandler struct {
	mu       sync.Mutex
	reads    int
	closes   []error
	inflight atomic.Bool
	overlap  atomic.Bool
	readIn   chan struct{}
	readOut  chan struct{}
	onTimer  func(c uio.Conn) error
}

func (h *recHandler) OnOpen(uio.Conn) {
}

func (h *recHandler) OnRead(c uio.Conn) error {
	h.mu.Lock()
	h.reads++
	h.mu.Unlock()
	if h.readIn != nil {
		h.inflight.Store(true)
		h.readIn <- struct{}{}
		<-h.readOut
		h.inflight.Store(false)
	}
	return nil
}

func (h *recHandler) OnTimer(c uio.Conn) error {
	if h.onTimer != nil {
		return h.onTimer(c)
	}
	return nil
}

func (h *recHandler) OnClose(_ uio.Conn, err error) {
	if h.inflight.Load() {
		h.overlap.Store(true)
	}
	h.mu.Lock()
	h.closes = append(h.closes, err)
	h.mu.Unlock()
}

func (h *recHandler) closeCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.closes)
}

func (h *recHandler) lastClose() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.closes) == 0 {
		return nil
	}
	return h.closes[len(h.closes)-1]
}

func recPair(cfg Config) (*Session, *recWire) {
	wire := &recWire{}
	cfg.KeepAlive = -1
	s := &Session{cfg: cfg.resolve(), conn: wire, streams: make(map[uint32]*Stream), openWait: make(map[uint32]chan error)}
	return s, wire
}

func TestAsymmetricWindowIsRespected(t *testing.T) {
	slow := &slowServer{opened: make(chan *Stream, 1)}
	sess, _ := startPair(t, Config{Handler: slow, WindowSize: 256 << 10}, Config{WindowSize: 1 << 20})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	st, err := sess.Open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	payload := make([]byte, 300<<10)
	n, werr := st.Write(payload)
	if n != 256<<10 || !errors.Is(werr, uio.ErrOutboundOverflow) {
		t.Fatalf("write = %d, %v; want %d and overflow", n, werr, 256<<10)
	}
	time.Sleep(100 * time.Millisecond)
	if sess.Closed() {
		t.Fatal("session closed after an oversized write")
	}
}

func TestOnTimerErrorClosesStream(t *testing.T) {
	boom := errors.New("timer boom")
	h := &recHandler{onTimer: func(uio.Conn) error {
		return boom
	}}
	s, wire := recPair(Config{Handler: h})
	st := newStream(s, 1, false, int64(s.cfg.WindowSize))
	s.streams[1] = st
	st.mu.Lock()
	st.wakeAt = time.Now().Add(-time.Millisecond)
	st.mu.Unlock()
	_ = s.onTimer(wire)
	if s.streams[st.id] != nil {
		t.Fatal("stream stayed open after its OnTimer returned an error")
	}
	if err := h.lastClose(); !errors.Is(err, boom) {
		t.Fatalf("close cause = %v, want %v", err, boom)
	}
}

func TestBridgePairRejectsClosedStream(t *testing.T) {
	echo := newEchoServer()
	echoAddr := freeAddr(t)
	srv := NewServer(Config{Handler: echo})
	go func() {
		_ = srv.Serve(echoAddr)
	}()
	t.Cleanup(func() {
		_ = srv.Close(nil)
	})
	cli := NewClient(Config{})
	var sess *Session
	deadline := time.Now().Add(5 * time.Second)
	for {
		var err error
		sess, err = cli.Dial(context.Background(), echoAddr)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("dial: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Cleanup(func() {
		_ = cli.Close(nil)
	})
	br := &ubridge.Bridge{Dial: func(ctx context.Context, _ uio.Conn, _ ubridge.DialFunc) (uio.Conn, error) {
		st, err := sess.Open(ctx)
		if err != nil {
			return nil, err
		}
		_ = st.Abort(errors.New("gone before pairing"))
		return st, nil
	}}
	ev := br.Events()
	userAddr := freeAddr(t)
	go func() {
		_ = ev.Serve("tcp://" + userAddr)
	}()
	t.Cleanup(func() {
		_ = ev.Close(nil)
	})
	var conn net.Conn
	deadline = time.Now().Add(5 * time.Second)
	for {
		var err error
		conn, err = net.Dial("tcp", userAddr)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("dial user: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	start := time.Now()
	_, err := conn.Read(make([]byte, 1))
	if err == nil {
		t.Fatal("user read succeeded on a dead pair")
	}
	var nerr net.Error
	if errors.As(err, &nerr) && nerr.Timeout() {
		t.Fatal("user leg hung on a dead pair until its deadline")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("user leg was not reset promptly (%v)", elapsed)
	}
}

func frameBuffer(n int) *uio.Buffer {
	b := uio.AcquireBuffer(headerSize + n)
	var hb [headerSize]byte
	encodeHeader(hb[:], frameHeader{typ: typeData, streamID: 1, length: uint32(n)})
	_, _ = b.Write(hb[:])
	_, _ = b.Write(make([]byte, n))
	return b
}

func TestFlushOverflowAndErrorPaths(t *testing.T) {
	sess, _ := startPair(t, Config{}, Config{})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stX, err := sess.Open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	fs := newSession(stX, Config{}.resolve(), true)
	fs.mu.Lock()
	st := newStream(fs, 1, false, 0)
	st.opening = false
	fs.streams[1] = st
	st.out = append(st.out, frameBuffer(150<<10), frameBuffer(150<<10))
	st.queued = true
	fs.pending = append(fs.pending, 1)
	flushErr := fs.flushLocked()
	queued := len(st.out)
	fs.mu.Unlock()
	if flushErr != nil || queued != 1 {
		t.Fatalf("flushLocked = %v with %d frames queued, want nil and 1", flushErr, queued)
	}
	_ = readFull(t, stX, headerSize+(150<<10))
	waitFor(t, "requeued frame flushed", func() bool {
		fs.flush()
		fs.mu.Lock()
		left := len(st.out)
		fs.mu.Unlock()
		return left == 0
	})
	_ = readFull(t, stX, headerSize+(150<<10))
	stX.Abort(errors.New("gone"))
	fs.mu.Lock()
	fs.sendControlLocked(typePing, flagSYN, 1, 0)
	ferr := fs.flushLocked()
	closed := fs.closed
	fs.mu.Unlock()
	if ferr == nil || closed {
		t.Fatalf("flush after transport close = %v, closed=%v, want an error and no self-close", ferr, closed)
	}
	fs.flush()
	fs2 := newSession(stX, Config{}.resolve(), true)
	if _, err := fs2.Open(ctx); err == nil {
		t.Fatal("Open on a closed transport succeeded")
	}
}

func TestOpenContextCanceled(t *testing.T) {
	silent := &silentServer{opened: make(chan *Stream, 4)}
	sess, _ := startPair(t, Config{Handler: silent}, Config{})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stY, err := sess.Open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	fs := newSession(stY, Config{}.resolve(), true)
	dead, cancel2 := context.WithCancel(context.Background())
	cancel2()
	if _, err := fs.Open(dead); !errors.Is(err, context.Canceled) {
		t.Fatalf("Open with a canceled context: %v", err)
	}
}

func TestStreamCloseIDempotence(t *testing.T) {
	sess, _ := startPair(t, Config{}, Config{})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	st, err := sess.Open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	st.deliverClose(nil)
	st.deliverClose(nil)
	st.resetRemote(errors.New("late"))
	st.resetLocal(errors.New("later"))
	closed := &Stream{sess: sess}
	closed.mu.Lock()
	closed.closedLocal = true
	closed.mu.Unlock()
	if _, err := closed.Read(make([]byte, 1)); err != io.EOF {
		t.Fatalf("Read on a nil-cause closed stream: %v", err)
	}
	if n, err := closed.Read(nil); n != 0 || err != nil {
		t.Fatalf("Read(nil) = %d, %v, want 0, nil", n, err)
	}
	if err := closed.pushData([]byte("x"), false, false); err != nil {
		t.Fatalf("pushData after close: %v", err)
	}
	if closed.grantWindow(5) {
		t.Fatal("grant on a closed stream reported credit")
	}
	other, _ := sess.Open(ctx)
	if other.grantWindow(0) {
		t.Fatal("a zero grant reported credit")
	}
	sess.mu.Lock()
	other.out = append(other.out, frameBuffer(8))
	sess.mu.Unlock()
	sess.abortStream(other, errors.New("drop"))
	if other.OutboundBuffered() != 0 {
		t.Fatal("abortStream kept queued frames")
	}
}

func TestRouterWithoutSessions(t *testing.T) {
	sess, _ := startPair(t, Config{}, Config{})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	st, err := sess.Open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	r := &router{cfg: Config{}.resolve(), client: true, started: make(chan struct{})}
	if err := r.OnRead(st); err != nil {
		t.Fatalf("OnRead without a session: %v", err)
	}
	if err := r.OnTimer(st); err != nil {
		t.Fatalf("OnTimer without a session: %v", err)
	}
	r.OnClose(st, nil)
	r.OnBytesRead(st, 1)
	r.OnBytesWritten(st, 1)
	r.OnStart(nil)
	r.OnStop(nil)
	dead, cancel2 := context.WithCancel(context.Background())
	cancel2()
	if _, err := r.waitSession(dead, st); !errors.Is(err, context.Canceled) {
		t.Fatalf("waitSession with a canceled context: %v", err)
	}
}

func TestRawExtra(t *testing.T) {
	addr := rawServer(t)
	t.Run("duplicate SYN is a protocol error", func(t *testing.T) {
		conn := dialTCP(t, addr)
		defer conn.Close()
		syn := rawFramed(typeWindowUpdate, flagSYN, 3, 0, nil)
		if _, err := conn.Write(syn); err != nil {
			t.Fatal(err)
		}
		expectFrame(t, conn, func(h frameHeader) bool {
			return h.flags&flagACK != 0
		})
		if _, err := conn.Write(syn); err != nil {
			t.Fatal(err)
		}
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		if _, err := io.ReadFull(conn, make([]byte, 1)); err == nil {
			t.Fatal("duplicate SYN left the connection open")
		}
	})
	t.Run("ACK for an unknown stream is ignored", func(t *testing.T) {
		conn := dialTCP(t, addr)
		defer conn.Close()
		if _, err := conn.Write(rawFramed(typeWindowUpdate, flagACK, 77, 0, nil)); err != nil {
			t.Fatal(err)
		}
		if _, err := conn.Write(rawFramed(typePing, flagSYN, 5, 5, nil)); err != nil {
			t.Fatal(err)
		}
		expectFrame(t, conn, func(h frameHeader) bool {
			return h.typ == typePing && h.flags&flagACK != 0
		})
	})
	t.Run("wrong stream parity is a protocol error", func(t *testing.T) {
		conn := dialTCP(t, addr)
		defer conn.Close()
		if _, err := conn.Write(rawFramed(typeWindowUpdate, flagSYN, 4, 0, nil)); err != nil {
			t.Fatal(err)
		}
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		if _, err := io.ReadFull(conn, make([]byte, 1)); err == nil {
			t.Fatal("an even stream id on the server side was accepted")
		}
	})
	t.Run("split data payload echoes", func(t *testing.T) {
		conn := dialTCP(t, addr)
		defer conn.Close()
		if _, err := conn.Write(rawFramed(typeWindowUpdate, flagSYN, 11, 0, nil)); err != nil {
			t.Fatal(err)
		}
		expectFrame(t, conn, func(h frameHeader) bool {
			return h.flags&flagACK != 0
		})
		frame := rawFramed(typeData, 0, 11, 10, []byte("0123456789"))
		if _, err := conn.Write(frame[:headerSize+4]); err != nil {
			t.Fatal(err)
		}
		time.Sleep(30 * time.Millisecond)
		if _, err := conn.Write(frame[headerSize+4:]); err != nil {
			t.Fatal(err)
		}
		echo := expectFrame(t, conn, func(h frameHeader) bool {
			return h.typ == typeData && h.streamID == 11
		})
		if echo.length != 10 {
			t.Fatalf("echo length = %d", echo.length)
		}
	})
	t.Run("buffered inbound beyond the window is a protocol error", func(t *testing.T) {
		conn := dialTCP(t, addr)
		defer conn.Close()
		if _, err := conn.Write(rawFramed(typeWindowUpdate, flagSYN, 13, 0, nil)); err != nil {
			t.Fatal(err)
		}
		expectFrame(t, conn, func(h frameHeader) bool {
			return h.flags&flagACK != 0
		})
		first := bytes.Repeat([]byte("a"), 200<<10)
		if _, err := conn.Write(rawFramed(typeData, 0, 13, uint32(len(first)), first)); err != nil {
			t.Fatal(err)
		}
		second := bytes.Repeat([]byte("b"), 100<<10)
		if _, err := conn.Write(rawFramed(typeData, 0, 13, uint32(len(second)), second)); err != nil {
			t.Fatal(err)
		}
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		for {
			if _, err := io.ReadFull(conn, make([]byte, 1)); err != nil {
				break
			}
		}
	})
	t.Run("FIN with a window delta", func(t *testing.T) {
		conn := dialTCP(t, addr)
		defer conn.Close()
		if _, err := conn.Write(rawFramed(typeWindowUpdate, flagSYN, 15, 0, nil)); err != nil {
			t.Fatal(err)
		}
		expectFrame(t, conn, func(h frameHeader) bool {
			return h.flags&flagACK != 0
		})
		if _, err := conn.Write(rawFramed(typeWindowUpdate, flagFIN, 15, 16, nil)); err != nil {
			t.Fatal(err)
		}
		if _, err := conn.Write(rawFramed(typePing, flagSYN, 5, 5, nil)); err != nil {
			t.Fatal(err)
		}
		expectFrame(t, conn, func(h frameHeader) bool {
			return h.typ == typePing && h.flags&flagACK != 0
		})
	})
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(200 * time.Microsecond)
	}
}

func waitInbound(t *testing.T, st *Stream, n int) {
	t.Helper()
	waitFor(t, "inbound bytes", func() bool {
		return st.InboundBuffered() >= n
	})
}

func TestStreamSurface(t *testing.T) {
	sess, _ := startPair(t, Config{}, Config{})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	st, err := sess.Open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.ID() != 1 {
		t.Fatalf("first stream id = %d, want 1", st.ID())
	}
	if st.LocalAddr() == nil || st.RemoteAddr() == nil {
		t.Fatal("stream addresses are nil")
	}
	st.SetUserdata("marker")
	if st.Userdata() != "marker" {
		t.Fatalf("Userdata = %v", st.Userdata())
	}
	if err := st.ReserveInbound(64); err != nil {
		t.Fatalf("ReserveInbound: %v", err)
	}
	if _, ok := uio.SocketOf(st); ok {
		t.Fatal("SocketOf succeeded on a multiplexed stream")
	}
	if err := st.SetDeadline(time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("SetDeadline: %v", err)
	}
	if err := st.SetReadDeadline(time.Time{}); err != nil {
		t.Fatalf("SetReadDeadline(clear): %v", err)
	}
	if err := st.SetWriteDeadline(time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("SetWriteDeadline: %v", err)
	}
	if err := st.SetWriteDeadline(time.Time{}); err != nil {
		t.Fatalf("SetWriteDeadline(clear): %v", err)
	}
	part1 := []byte("part-one ")
	if n, err := st.WriteString(string(part1)); err != nil || n != len(part1) {
		t.Fatalf("WriteString = %d, %v", n, err)
	}
	if err := st.WriteByte('-'); err != nil {
		t.Fatalf("WriteByte: %v", err)
	}
	vec := [][]byte{[]byte("vec-a "), []byte("vec-b")}
	if n, err := st.Writev(vec); err != nil || n != len("vec-a vec-b") {
		t.Fatalf("Writev = %d, %v", n, err)
	}
	want := append(append(append([]byte{}, part1...), '-'), []byte("vec-a vec-b")...)
	got := readFull(t, st, len(want))
	if !bytes.Equal(got, want) {
		t.Fatalf("echo mismatch: %q", got)
	}
	owned := uio.AcquireBuffer(6)
	_, _ = owned.Write([]byte("owned!"))
	if n, err := st.WriteOwned(owned); err != nil || n != 6 {
		t.Fatalf("WriteOwned = %d, %v", n, err)
	}
	if got := readFull(t, st, 6); string(got) != "owned!" {
		t.Fatalf("WriteOwned echo = %q", got)
	}
	if st.OutboundBuffered() < 0 {
		t.Fatal("OutboundBuffered is negative")
	}
	if err := st.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if _, err := st.Write([]byte("buffered-data")); err != nil {
		t.Fatal(err)
	}
	waitInbound(t, st, len("buffered-data"))
	if p := st.Peek(make([]byte, 4)); string(p) != "buff" {
		t.Fatalf("Peek = %q", p)
	}
	if chunk := st.PeekChunk(); len(chunk) == 0 || chunk[0] != 'b' {
		t.Fatalf("PeekChunk = %q", chunk)
	}
	if _, err := st.Discard(4); err != nil {
		t.Fatalf("Discard: %v", err)
	}
	var sink bytes.Buffer
	if n, err := st.WriteTo(&sink); err != nil || n != int64(len("ered-data")) || sink.String() != "ered-data" {
		t.Fatalf("WriteTo = %d, %v, %q", n, err, sink.String())
	}
	if err := st.SetWakeDeadline(time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := st.SetWakeInterval(time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := st.Wake(); err != nil {
		t.Fatal(err)
	}
	_ = st.SetWakeDeadline(time.Time{})
	_ = st.SetWakeInterval(0)
	if err := st.CloseWrite(); err != nil {
		t.Fatalf("CloseWrite: %v", err)
	}
	if st.State()&uio.StateWriteEnded == 0 {
		t.Fatal("State lacks StateWriteEnded after CloseWrite")
	}
	if _, err := st.Write([]byte("late")); !errors.Is(err, ErrClosed) && !errors.Is(err, io.EOF) {
		t.Fatalf("write after CloseWrite: %v", err)
	}
	waitEOF(t, st)
	for deadline := time.Now().Add(time.Second); st.State()&uio.StateClosed == 0 && time.Now().Before(deadline); {
		time.Sleep(time.Millisecond)
	}
	if state := st.State(); state&uio.StateClosed == 0 || state&uio.StateReadEnded == 0 {
		t.Fatalf("State after both FINs = %b", state)
	}
	other, err := sess.Open(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := other.Close(); err != nil {
		t.Fatalf("CloseGracefully: %v", err)
	}
	waitEOF(t, other)
	third, err := sess.Open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := third.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	waitEOF(t, third)
}

func TestStreamAfterClose(t *testing.T) {
	sess, _ := startPair(t, Config{}, Config{})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	st, err := sess.Open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	boom := errors.New("boom")
	if err := st.Abort(boom); err != nil {
		t.Fatalf("CloseWith: %v", err)
	}
	if _, err := st.Write([]byte("x")); !errors.Is(err, boom) {
		t.Fatalf("write after reset: %v", err)
	}
	if _, err := st.Read(make([]byte, 1)); !errors.Is(err, boom) {
		t.Fatalf("read after reset: %v", err)
	}
	if err := st.Close(); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("Close() after close: %v", err)
	}
	sess.mu.Lock()
	opening := newStream(sess, 97, true, int64(sess.cfg.WindowSize))
	opening.opening = true
	sess.mu.Unlock()
	if _, err := opening.Write([]byte("x")); !errors.Is(err, ErrClosed) {
		t.Fatalf("write on opening stream: %v", err)
	}
	if err := sess.Close(nil); err != nil {
		t.Fatalf("session Close: %v", err)
	}
	waitFor(t, "session closed", sess.Closed)
	if err := sess.Close(nil); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("second session Close: %v", err)
	}
	if _, err := sess.Open(ctx); !errors.Is(err, ErrClosed) {
		t.Fatalf("Open after close: %v", err)
	}
}

func TestLocalLimitsAndGoAway(t *testing.T) {
	sess, _ := startPair(t, Config{}, Config{MaxStreams: 1})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	st, err := sess.Open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Abort(errors.New("done"))
	if _, err := sess.Open(ctx); !errors.Is(err, ErrMaxStreams) {
		t.Fatalf("Open past MaxStreams: %v", err)
	}
	sess.mu.Lock()
	sess.goAway = true
	sess.mu.Unlock()
	if _, err := sess.Open(ctx); !errors.Is(err, ErrGoAway) {
		t.Fatalf("Open after goaway: %v", err)
	}
}

func TestAcceptHookRejects(t *testing.T) {
	sess, _ := startPair(t, Config{Accept: func() error {
		return errors.New("denied")
	}}, Config{})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := sess.Open(ctx); !errors.Is(err, ErrStreamReset) {
		t.Fatalf("Open rejected by Accept: %v", err)
	}
}

func TestServerMaxStreamsRejects(t *testing.T) {
	sess, _ := startPair(t, Config{MaxStreams: 1}, Config{})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	first, err := sess.Open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Abort(errors.New("done"))
	if _, err := sess.Open(ctx); !errors.Is(err, ErrStreamReset) {
		t.Fatalf("Open past the peer's MaxStreams: %v", err)
	}
}

func TestBacklogFullRejects(t *testing.T) {
	addr := freeAddr(t)
	srv := NewServer(Config{AcceptBacklog: 1})
	go func() {
		_ = srv.Serve(addr)
	}()
	t.Cleanup(func() {
		_ = srv.Close(nil)
	})
	cli := NewClient(Config{})
	t.Cleanup(func() {
		_ = cli.Close(nil)
	})
	var sess *Session
	waitFor(t, "dial", func() bool {
		s, err := cli.Dial(context.Background(), "tcp://"+addr)
		sess = s
		return err == nil
	})
	t.Cleanup(func() {
		_ = sess.Close(nil)
	})
	var srvSess *Session
	select {
	case srvSess = <-srv.Sessions():
	case <-time.After(5 * time.Second):
		t.Fatal("server session missing")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := sess.Open(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := sess.Open(ctx); !errors.Is(err, ErrStreamReset) {
		t.Fatalf("Open with a full backlog: %v", err)
	}
	if srvSess.Accept() == nil {
		t.Fatal("server session has no accept queue")
	}
}

type slowServer struct{ opened chan *Stream }

func (h *slowServer) OnOpen(c uio.Conn) {
	if st, ok := c.(*Stream); ok {
		select {
		case h.opened <- st:
		default:
		}
	}
}

func (h *slowServer) OnRead(uio.Conn) error {
	return nil
}

func (h *slowServer) OnClose(uio.Conn, error) {
}

func (h *slowServer) OnTimer(uio.Conn) error {
	return nil
}

type silentServer struct{ opened chan *Stream }

func (h *silentServer) OnOpen(c uio.Conn) {
	select {
	case h.opened <- c.(*Stream):
	default:
	}
}

func (h *silentServer) OnRead(c uio.Conn) error {
	st := c.(*Stream)
	buf := make([]byte, 4<<10)
	for {
		n, err := st.Read(buf)
		if n == 0 && err == nil {
			return nil
		}
		if err != nil {
			return nil
		}
	}
}

func (h *silentServer) OnTimer(uio.Conn) error {
	return nil
}

func (h *silentServer) OnClose(uio.Conn, error) {
}

func TestStreamCloseTimeoutResets(t *testing.T) {
	silent := &silentServer{opened: make(chan *Stream, 1)}
	sess, _ := startPair(t, Config{Handler: silent, StreamCloseTimeout: 50 * time.Millisecond}, Config{})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	st, err := sess.Open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	if err := st.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "half-close timeout", func() bool {
		return st.State()&uio.StateClosed != 0
	})
}

func TestKeepaliveAnswered(t *testing.T) {
	sess, _ := startPair(t, Config{KeepAlive: 20 * time.Millisecond, KeepAliveMax: 200 * time.Millisecond}, Config{KeepAlive: 20 * time.Millisecond, KeepAliveMax: 200 * time.Millisecond})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	st, err := sess.Open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(120 * time.Millisecond)
	if sess.Closed() {
		t.Fatal("session closed despite answering pings")
	}
	if _, err := st.Write([]byte("still-alive")); err != nil {
		t.Fatalf("write after keepalives: %v", err)
	}
	if got := readFull(t, st, len("still-alive")); string(got) != "still-alive" {
		t.Fatalf("echo = %q", got)
	}
}

func TestKeepaliveTimeoutCloses(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		time.Sleep(2 * time.Second)
		_ = conn.Close()
	}()
	cli := NewClient(Config{KeepAlive: 30 * time.Millisecond, KeepAliveMax: 30 * time.Millisecond})
	t.Cleanup(func() {
		_ = cli.Close(nil)
	})
	sess, err := cli.Dial(context.Background(), "tcp://"+ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	waitFor(t, "keepalive timeout", sess.Closed)
}

func rawFramed(typ uint8, flags uint16, sid, length uint32, payload []byte) []byte {
	b := make([]byte, headerSize+len(payload))
	encodeHeader(b, frameHeader{typ: typ, flags: flags, streamID: sid, length: length})
	copy(b[headerSize:], payload)
	return b
}

func readRawFrame(t *testing.T, conn net.Conn, timeout time.Duration) (frameHeader, bool) {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(timeout))
	var hb [headerSize]byte
	if _, err := io.ReadFull(conn, hb[:]); err != nil {
		return frameHeader{}, false
	}
	h, err := decodeHeader(hb[:])
	if err != nil {
		t.Fatalf("server sent an undecodable frame: %v", err)
	}
	if h.typ == typeData && h.length > 0 {
		if _, err := io.CopyN(io.Discard, conn, int64(h.length)); err != nil {
			t.Fatalf("drain payload: %v", err)
		}
	}
	return h, true
}

func expectFrame(t *testing.T, conn net.Conn, match func(frameHeader) bool) frameHeader {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		h, ok := readRawFrame(t, conn, time.Second)
		if !ok {
			t.Fatal("transport closed before the expected frame")
		}
		if match(h) {
			return h
		}
	}
	t.Fatal("expected frame never arrived")
	return frameHeader{}
}

func rawServer(t *testing.T) string {
	t.Helper()
	addr := freeAddr(t)
	srv := NewServer(Config{Handler: newEchoServer()})
	go func() {
		_ = srv.Serve(addr)
	}()
	t.Cleanup(func() {
		_ = srv.Close(nil)
	})
	return addr
}

func TestRawUnknownStreamAndPing(t *testing.T) {
	conn := dialTCP(t, rawServer(t))
	defer conn.Close()
	if _, err := conn.Write(rawFramed(typeData, 0, 99, 1, []byte("x"))); err != nil {
		t.Fatal(err)
	}
	rst := expectFrame(t, conn, func(h frameHeader) bool {
		return h.flags&flagRST != 0
	})
	if rst.streamID != 99 {
		t.Fatalf("RST for stream %d, want 99", rst.streamID)
	}
	if _, err := conn.Write(rawFramed(typePing, flagSYN, 7, 9, nil)); err != nil {
		t.Fatal(err)
	}
	pong := expectFrame(t, conn, func(h frameHeader) bool {
		return h.typ == typePing
	})
	if pong.flags&flagACK == 0 || pong.streamID != 7 || pong.length != 9 {
		t.Fatalf("pong = %+v", pong)
	}
}

func TestRawProtocolErrorsClose(t *testing.T) {
	addr := rawServer(t)
	t.Run("bad version", func(t *testing.T) {
		conn := dialTCP(t, addr)
		defer conn.Close()
		frame := rawFramed(typePing, flagSYN, 1, 0, nil)
		frame[0] = 9
		if _, err := conn.Write(frame); err != nil {
			t.Fatal(err)
		}
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		if _, err := io.ReadFull(conn, make([]byte, 1)); err == nil {
			t.Fatal("connection stayed open after a bad version")
		}
	})
	t.Run("data with SYN and ACK", func(t *testing.T) {
		conn := dialTCP(t, addr)
		defer conn.Close()
		if _, err := conn.Write(rawFramed(typeData, flagSYN|flagACK, 3, 1, []byte("x"))); err != nil {
			t.Fatal(err)
		}
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		if _, err := io.ReadFull(conn, make([]byte, 1)); err == nil {
			t.Fatal("connection stayed open after a contradictory data frame")
		}
	})
	t.Run("split header", func(t *testing.T) {
		conn := dialTCP(t, addr)
		defer conn.Close()
		frame := rawFramed(typeWindowUpdate, flagSYN, 5, 0, nil)
		if _, err := conn.Write(frame[:5]); err != nil {
			t.Fatal(err)
		}
		time.Sleep(50 * time.Millisecond)
		if _, err := conn.Write(frame[5:]); err != nil {
			t.Fatal(err)
		}
		ack := expectFrame(t, conn, func(h frameHeader) bool {
			return h.flags&flagACK != 0
		})
		if ack.streamID != 5 {
			t.Fatalf("ACK for stream %d", ack.streamID)
		}
	})
	t.Run("goaway resets new streams", func(t *testing.T) {
		conn := dialTCP(t, addr)
		defer conn.Close()
		if _, err := conn.Write(rawFramed(typeGoAway, 0, 0, goAwayNormal, nil)); err != nil {
			t.Fatal(err)
		}
		if _, err := conn.Write(rawFramed(typeWindowUpdate, flagSYN, 7, 0, nil)); err != nil {
			t.Fatal(err)
		}
		expectFrame(t, conn, func(h frameHeader) bool {
			return h.flags&flagRST != 0 && h.streamID == 7
		})
	})
	t.Run("window overrun tolerated", func(t *testing.T) {
		conn := dialTCP(t, addr)
		defer conn.Close()
		if _, err := conn.Write(rawFramed(typeWindowUpdate, flagSYN, 9, 0, nil)); err != nil {
			t.Fatal(err)
		}
		expectFrame(t, conn, func(h frameHeader) bool {
			return h.flags&flagACK != 0
		})
		if _, err := conn.Write(rawFramed(typeWindowUpdate, 0, 9, 1<<30, nil)); err != nil {
			t.Fatal(err)
		}
		if _, err := conn.Write(rawFramed(typePing, flagSYN, 3, 3, nil)); err != nil {
			t.Fatal(err)
		}
		expectFrame(t, conn, func(h frameHeader) bool {
			return h.typ == typePing && h.flags&flagACK != 0
		})
	})
}

func encoded(h frameHeader) []byte {
	var b [headerSize]byte
	encodeHeader(b[:], h)
	return b[:]
}

func TestDecodeHeaderErrors(t *testing.T) {
	cases := []struct {
		name string
		b    []byte
		want error
	}{{"short header", make([]byte, headerSize-1), errFrameShort}, {"wrong version", func() []byte {
		b := encoded(frameHeader{typ: typeData, streamID: 1})
		b[0] = protoVersion + 1
		return b
	}(), errFrameVersion}, {"unknown type", func() []byte {
		b := encoded(frameHeader{typ: typeData, streamID: 1})
		b[1] = typeSynMetadata + 1
		return b
	}(), errFrameType}, {"undefined flag", func() []byte {
		b := encoded(frameHeader{typ: typeData, streamID: 1})
		b[3] = 0x80
		return b
	}(), errFrameFlags}}
	for _, tc := range cases {
		if _, err := decodeHeader(tc.b); !errors.Is(err, tc.want) {
			t.Fatalf("%s: err = %v, want %v", tc.name, err, tc.want)
		}
	}
}

func TestValidateFrame(t *testing.T) {
	const maxData = 64 << 10
	const maxMeta = 64
	cases := []struct {
		name string
		h    frameHeader
		want error
	}{{"data within the limit", frameHeader{typ: typeData, streamID: 1, length: maxData}, nil}, {"data beyond the limit", frameHeader{typ: typeData, streamID: 1, length: maxData + 1}, errFrameLength}, {"window update names a stream", frameHeader{typ: typeWindowUpdate, streamID: 2, length: 1}, nil}, {"window update without a stream", frameHeader{typ: typeWindowUpdate, streamID: 0, length: 1}, errFrameFlags}, {"goaway names the session", frameHeader{typ: typeGoAway, streamID: 0, length: goAwayNormal}, nil}, {"goaway names a stream", frameHeader{typ: typeGoAway, streamID: 3, length: goAwayNormal}, errFrameFlags}, {"metadata syn", frameHeader{typ: typeSynMetadata, flags: flagSYN, streamID: 1, length: 4 + maxMeta}, nil}, {"metadata syn without its delta", frameHeader{typ: typeSynMetadata, flags: flagSYN, streamID: 1, length: 3}, errFrameLength}, {"metadata syn beyond the limit", frameHeader{typ: typeSynMetadata, flags: flagSYN, streamID: 1, length: 4 + maxMeta + 1}, errFrameLength}, {"metadata syn without the open flag", frameHeader{typ: typeSynMetadata, flags: flagACK, streamID: 1, length: 4}, errFrameFlags}, {"metadata syn without a stream", frameHeader{typ: typeSynMetadata, flags: flagSYN, streamID: 0, length: 4}, errFrameFlags}}
	for _, tc := range cases {
		if err := validateFrame(tc.h, maxData, maxMeta); !errors.Is(err, tc.want) {
			t.Fatalf("%s: err = %v, want %v", tc.name, err, tc.want)
		}
	}
}

func FuzzDecodeHeader(f *testing.F) {
	f.Add(make([]byte, headerSize))
	f.Add(encoded(frameHeader{typ: typeData, flags: flagSYN, streamID: 1, length: 4}))
	f.Add(encoded(frameHeader{typ: typeWindowUpdate, flags: flagFIN, streamID: 9, length: 1024}))
	f.Fuzz(func(t *testing.T, b []byte) {
		h, err := decodeHeader(b)
		if err != nil {
			return
		}
		var out [headerSize]byte
		encodeHeader(out[:], h)
		if !bytes.Equal(out[:], b[:headerSize]) {
			t.Fatalf("re-encode %x != input %x", out[:], b[:headerSize])
		}
	})
}

func TestServerServeAndListenShareLifecycle(t *testing.T) {
	srv := NewServer(Config{KeepAlive: -1})
	served := make(chan error, 1)
	go func() {
		served <- srv.Serve()
	}()
	select {
	case <-srv.router.started:
	case <-time.After(5 * time.Second):
		t.Fatal("server did not start")
	}
	ln, err := srv.Listen("tcp://127.0.0.1:0")
	if err != nil {
		_ = srv.Close(err)
		_ = srv.Wait()
		t.Fatal(err)
	}
	cli := NewClient(Config{KeepAlive: -1})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sess, err := cli.Dial(ctx, "tcp://"+ln.Addr().String())
	if err != nil {
		_ = cli.Close(err)
		_ = cli.Wait()
		_ = srv.Close(err)
		_ = srv.Wait()
		t.Fatal(err)
	}
	_ = cli.Close(nil)
	_ = cli.Wait()
	select {
	case <-sess.Done():
	case <-ctx.Done():
		t.Fatal("client shutdown did not finish its session")
	}
	_ = srv.Close(nil)
	_ = srv.Wait()
	select {
	case <-served:
	case <-ctx.Done():
		t.Fatal("Serve did not return after shutdown")
	}
	for range srv.Sessions() {
	}
}

func TestIdleServerCloseEndsSessionConsumer(t *testing.T) {
	srv := NewServer(Config{})
	_ = srv.Close(nil)
	_ = srv.Wait()
	select {
	case _, ok := <-srv.Sessions():
		if ok {
			t.Fatal("idle server announced a session after Close")
		}
	case <-time.After(time.Second):
		t.Fatal("idle server left Sessions consumers blocked")
	}
	if _, err := srv.Listen("tcp://127.0.0.1:0"); err == nil {
		t.Fatal("closed server listened again")
	}
}

func TestSessionAnnouncementQueueRefusesUnreachableTransports(t *testing.T) {
	srv := NewServer(Config{KeepAlive: -1})
	t.Cleanup(func() {
		_ = srv.Close(nil)
	})
	ln, err := srv.Listen("tcp://127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cli := NewClient(Config{KeepAlive: -1})
	t.Cleanup(func() {
		_ = cli.Close(nil)
		_ = cli.Wait()
		_ = srv.Close(nil)
		_ = srv.Wait()
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for i := 0; i < cap(srv.session); i++ {
		if _, err := cli.Dial(ctx, "tcp://"+ln.Addr().String()); err != nil {
			t.Fatalf("dial %d: %v", i, err)
		}
		waitFor(t, "session announcement", func() bool {
			return len(srv.session) == i+1
		})
	}
	sess, err := cli.Dial(ctx, "tcp://"+ln.Addr().String())
	if err == nil {
		select {
		case <-sess.Done():
		case <-ctx.Done():
			t.Fatal("full announcement queue left an unreachable transport open")
		}
	}
}

type productionRejectWire struct {
	productionSessionWire
	writeErr error
}

func (w *productionRejectWire) Write(p []byte) (int, error) {
	if w.writeErr != nil {
		return 0, w.writeErr
	}
	return w.productionSessionWire.Write(p)
}

func TestUnacceptedInboundDoesNotDeliverUnmatchedClose(t *testing.T) {
	for _, mode := range []string{"shutdown-during-accept", "ack-write-failure"} {
		t.Run(mode, func(t *testing.T) {
			closed := 0
			boom := errors.New(mode)
			wire := &productionRejectWire{}
			cfg := Config{KeepAlive: -1, Handler: uio.FuncHandler{Close: func(uio.Conn, error) {
				closed++
			}}}
			var s *Session
			if mode == "shutdown-during-accept" {
				cfg.Accept = func() error {
					return s.Close(boom)
				}
			} else {
				wire.writeErr = boom
			}
			s = newSession(wire, cfg.resolve(), false)
			_ = s.handleSyn(frameHeader{typ: typeWindowUpdate, flags: flagSYN, streamID: 1})
			s.onTransportClose(boom)
			if closed != 0 {
				t.Fatalf("delivered %d closes without a preceding accepted OnOpen", closed)
			}
		})
	}
}

func TestDuplicateAckCannotRestoreSpentCredit(t *testing.T) {
	s, st, _ := quietPair()
	st.mu.Lock()
	st.sendWin.take(123)
	want := st.sendWin.available()
	st.mu.Unlock()
	if err := s.handleAck(frameHeader{typ: typeWindowUpdate, flags: flagACK, streamID: st.id}); err != nil {
		t.Fatal(err)
	}
	st.mu.Lock()
	got := st.sendWin.available()
	st.mu.Unlock()
	if got != want {
		t.Fatalf("duplicate ACK restored send credit: got %d, want %d", got, want)
	}
}

type productionSessionWire struct {
	uio.Conn
	mu       sync.Mutex
	deadline time.Time
	written  []frameHeader
	timer    func(time.Time)
	wake     func() error
	closed   atomic.Bool
}

func (c *productionSessionWire) SetWakeDeadline(when time.Time) error {
	if c.timer != nil {
		c.timer(when)
	}
	c.mu.Lock()
	c.deadline = when
	c.mu.Unlock()
	return nil
}

func (c *productionSessionWire) lastDeadline() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.deadline
}

func (c *productionSessionWire) Write(p []byte) (int, error) {
	h, err := decodeHeader(p)
	if err != nil {
		return 0, err
	}
	c.mu.Lock()
	c.written = append(c.written, h)
	c.mu.Unlock()
	return len(p), nil
}

func (c *productionSessionWire) Wake() error {
	if c.wake != nil {
		return c.wake()
	}
	if c.closed.Load() {
		return net.ErrClosed
	}
	return nil
}

func (c *productionSessionWire) Close() error {
	return c.Abort(nil)
}

func (c *productionSessionWire) Abort(error) error {
	c.closed.Store(true)
	return nil
}

func TestSessionRendezvousSettlesCloseDuringWake(t *testing.T) {
	wire := &productionSessionWire{}
	r := &router{cfg: Config{KeepAlive: -1}.resolve(), client: true}
	boom := errors.New("closed after waiter registration")
	wire.wake = func() error {
		r.OnClose(wire, boom)
		return nil
	}
	sess, err := r.waitSession(context.Background(), wire)
	if sess != nil || !errors.Is(err, boom) {
		t.Fatalf("waitSession = %v, %v", sess, err)
	}
	if len(r.waiting) != 0 || len(r.conns) != 0 {
		t.Fatal("closed rendezvous retained routing state")
	}
}

func TestSessionRendezvousCancellationAndLateOpen(t *testing.T) {
	wire := &productionSessionWire{}
	r := &router{cfg: Config{KeepAlive: -1}.resolve(), client: true}
	ctx, cancel := context.WithCancel(context.Background())
	wire.wake = func() error {
		cancel()
		return nil
	}
	if _, err := r.waitSession(ctx, wire); !errors.Is(err, context.Canceled) {
		t.Fatalf("waitSession = %v, want context cancellation", err)
	}
	if !wire.closed.Load() || len(r.waiting) != 0 {
		t.Fatal("cancellation failed to close the transport and remove its waiter")
	}
	r.OnOpen(wire)
	r.OnClose(wire, context.Canceled)
	if len(r.waiting) != 0 || len(r.conns) != 0 {
		t.Fatal("late callbacks retained a cancelled dial")
	}
}

func TestImmediatePeerCloseBeforeSessionRendezvous(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err == nil {
			_ = conn.Close()
		}
	}()
	cli := NewClient(Config{KeepAlive: -1})
	defer cli.Close(nil)
	opened := make(chan *Session, 1)
	cli.router.announce = func(sess *Session) {
		opened <- sess
	}
	if err := cli.start(); err != nil {
		t.Fatal(err)
	}
	conn, err := cli.events.DialContext(context.Background(), "tcp://"+ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	var sess *Session
	select {
	case sess = <-opened:
	case <-time.After(time.Second):
		t.Fatal("transport never opened")
	}
	select {
	case <-sess.Done():
	case <-time.After(time.Second):
		t.Fatal("immediate peer close did not end the session")
	}
	if sess.Err() == nil {
		t.Fatal("Done did not publish the transport cause")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := cli.router.waitSession(ctx, conn); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("closed transport rendezvous = %v, want net.ErrClosed", err)
	}
	cli.router.mu.Lock()
	defer cli.router.mu.Unlock()
	if len(cli.router.waiting) != 0 || len(cli.router.conns) != 0 {
		t.Fatal("finished TCP lifecycle retained routing state")
	}
}

func TestTransportAssemblesFrameBeyondSessionBudget(t *testing.T) {
	srv := NewServer(Config{KeepAlive: -1, ReadBufferSize: 4 << 10, SessionWindow: 64 << 10})
	defer srv.Close(nil)
	ln, err := srv.Listen("tcp://127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Write(rawFramed(typeWindowUpdate, flagSYN, 1, 0, nil)); err != nil {
		t.Fatal(err)
	}
	expectFrame(t, conn, func(h frameHeader) bool {
		return h.flags&flagACK != 0
	})
	sess := <-srv.Sessions()
	st := (<-sess.Accept()).Stream
	payload := bytes.Repeat([]byte("f"), 128<<10)
	if _, err := conn.Write(rawFramed(typeData, 0, 1, uint32(len(payload)), payload)); err != nil {
		t.Fatal(err)
	}
	waitInbound(t, st, len(payload))
	waitFor(t, "session read budget hold", func() bool {
		sess.mu.Lock()
		defer sess.mu.Unlock()
		return sess.readHeld && sess.pendingBytes == int64(len(payload))
	})
	if got := st.PeekChunk(); !bytes.Equal(got, payload) {
		t.Fatalf("assembled payload differs: got %d bytes", len(got))
	}
	if _, err := st.Discard(-1); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "session receive budget return", func() bool {
		sess.mu.Lock()
		defer sess.mu.Unlock()
		return sess.pendingBytes == 0 && !sess.readHeld
	})
}

func TestTransportReadLimitPreservesBounds(t *testing.T) {
	maxInt := int(^uint(0) >> 1)
	for _, cfg := range []Config{{}, {ReadBufferSize: 4 << 10, SessionWindow: 64 << 10}, {ReadBufferSize: maxInt, SessionWindow: maxInt, WindowSize: maxInt}} {
		cfg = cfg.resolve()
		limit := transportReadLimit(cfg)
		if limit <= 0 || limit < cfg.WindowSize {
			t.Fatalf("transport limit wrapped or cannot fit a legal frame: %d", limit)
		}
		if cfg.WindowSize <= maxInt-headerSize && limit < cfg.WindowSize+headerSize {
			t.Fatalf("transport limit %d excludes the frame header", limit)
		}
		if got := NewClient(cfg).events.MaxInboundBuffered; got != limit {
			t.Fatalf("client transport limit = %d, want %d", got, limit)
		}
		if got := NewServer(cfg).events.MaxInboundBuffered; got != limit {
			t.Fatalf("server transport limit = %d, want %d", got, limit)
		}
	}
}

func TestSessionDoneAndAcceptFollowTransportLifetime(t *testing.T) {
	s := newSession(&productionSessionWire{}, Config{KeepAlive: -1}.resolve(), true)
	st := newStream(s, 2, false, int64(s.cfg.WindowSize))
	s.streams[st.id] = st
	s.acceptCh <- Accepted{Stream: st}
	if err := s.Err(); err != nil {
		t.Fatalf("open session Err = %v", err)
	}
	select {
	case <-s.Done():
		t.Fatal("new session already done")
	default:
	}
	boom := errors.New("transport ended")
	s.onTransportClose(boom)
	select {
	case <-s.Done():
	default:
		t.Fatal("transport close did not signal Done")
	}
	if !errors.Is(s.Err(), boom) {
		t.Fatalf("terminal Err = %v, want %v", s.Err(), boom)
	}
	if got, ok := <-s.Accept(); !ok || got.Stream != st || got.Stream.State()&uio.StateClosed == 0 {
		t.Fatal("buffered accepted stream was lost or remained open")
	}
	if _, ok := <-s.Accept(); ok {
		t.Fatal("Accept remained open after draining buffered streams")
	}
	s.onTransportClose(io.EOF)
	if !errors.Is(s.Err(), boom) {
		t.Fatal("a repeated close changed the terminal cause")
	}
	literal := &Session{}
	literal.onTransportClose(nil)
	if !errors.Is(literal.Err(), io.EOF) {
		t.Fatalf("nil-cause transport close = %v, want EOF", literal.Err())
	}
}

type productionAcceptCloseObserver struct {
	entered chan struct{}
	release chan struct{}
}

func (*productionAcceptCloseObserver) OnBytesRead(uio.Conn, int) {
}

func (*productionAcceptCloseObserver) OnBytesWritten(uio.Conn, int) {
}

func TestSessionAcceptSendCannotRaceChannelClose(t *testing.T) {
	obs := &productionAcceptCloseObserver{entered: make(chan struct{}), release: make(chan struct{})}
	s := newSession(&productionSessionWire{}, Config{KeepAlive: -1, Observer: obs}.resolve(), false)
	drainedSt := newStream(s, 3, false, int64(s.cfg.WindowSize))
	drainedSt.streamWriteControl().Watch(func(int) {
		close(obs.entered)
		<-obs.release
	})
	s.drained = []*Stream{drainedSt}
	s.hasDrained.Store(true)
	done := make(chan error, 1)
	go func() {
		done <- s.handleSyn(frameHeader{typ: typeWindowUpdate, flags: flagSYN, streamID: 1})
	}()
	<-obs.entered
	s.onTransportClose(errors.New("close before accept delivery"))
	close(obs.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, ok := <-s.Accept(); ok {
		t.Fatal("a stream entered the accept queue after transport close")
	}
}

func TestSessionProtocolRejectionsDoNotPublishStreams(t *testing.T) {
	s, st, _, h := productionStreamPair(t)
	for _, frame := range []frameHeader{{typ: typeData, flags: flagSYN, streamID: st.id}, {typ: typeSynMetadata, streamID: 2}} {
		if err := s.handleFrame(frame, nil, false); !errors.Is(err, ErrProtocol) {
			t.Fatalf("invalid frame type=%d flags=%d: %v", frame.typ, frame.flags, err)
		}
	}
	for _, id := range []uint32{0, 3} {
		s.client = true
		if err := s.checkPeerStreamID(id); !errors.Is(err, ErrProtocol) {
			t.Fatalf("invalid server stream ID %d: %v", id, err)
		}
	}
	if err := s.handleFrame(frameHeader{typ: 255}, nil, false); err != nil || !s.goAway {
		t.Fatal("unknown extension did not stop new stream admission")
	}
	if err := s.handleWindowUpdate(frameHeader{streamID: st.id}); err != nil {
		t.Fatal("an empty credit update was not harmless")
	}
	if err := s.handleData(frameHeader{streamID: st.id, flags: flagRST}, nil, false); err != nil {
		t.Fatal(err)
	}
	s.drainCloses()
	if closed := h.closed(); len(closed) != 1 || !errors.Is(closed[0], ErrStreamReset) {
		t.Fatalf("data-frame reset closes = %v", closed)
	}
	if len(s.streams) != 0 {
		t.Fatal("reset stream remained published")
	}
	s.closed = true
	if err := s.acceptStream(2, 0, nil); err != nil || len(s.streams) != 0 {
		t.Fatal("late SYN published a stream after transport shutdown")
	}
}

func TestSessionShutdownCallsShareOneDrainAndPreserveItsResult(t *testing.T) {
	s, st, _, _ := productionStreamPair(t)
	results := make(chan error, 2)
	var started sync.WaitGroup
	for range 2 {
		started.Add(1)
		go func() {
			started.Done()
			results <- s.Shutdown(nil)
		}()
	}
	started.Wait()
	waitFor(t, "shutdown admission barrier", func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.draining
	})
	s.abortStream(st, ErrStreamReset)
	for range 2 {
		select {
		case err := <-results:
			if err != nil {
				t.Fatalf("shared graceful shutdown = %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("one of the shutdown callers missed the drain completion")
		}
	}
	s.forceDrain(context.Canceled)
	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatalf("completed drain lost its result: %v", err)
	}
}

func TestSessionShutdownReportsBrokenTransportAndAlreadyClosedCause(t *testing.T) {
	cause := errors.New("test: GoAway write failed")
	wire := &productionRejectWire{writeErr: cause}
	s := newSession(wire, Config{KeepAlive: -1}.resolve(), true)
	st := newStream(s, 1, false, int64(s.cfg.WindowSize))
	s.streams[st.id] = st
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result := make(chan error, 1)
	go func() {
		result <- s.Shutdown(ctx)
	}()
	waitFor(t, "broken transport close", wire.closed.Load)
	s.onTransportClose(cause)
	if err := <-result; !errors.Is(err, cause) {
		t.Fatalf("shutdown = %v, want broken transport cause", err)
	}
	closed := newSession(&productionSessionWire{}, Config{KeepAlive: -1}.resolve(), true)
	closed.onTransportClose(cause)
	if err := closed.Shutdown(nil); !errors.Is(err, cause) {
		t.Fatalf("shutdown after transport failure = %v", err)
	}
}

func TestSessionRendezvousAndAnnouncementAfterServerClose(t *testing.T) {
	wire := &productionSessionWire{}
	s := newSession(wire, Config{KeepAlive: -1}.resolve(), true)
	r := &router{conns: map[uio.Conn]*Session{wire: s}}
	if got, err := r.waitSession(nil, wire); err != nil || got != s {
		t.Fatal("completed session rendezvous did not return its existing session")
	}
	r.OnStop(nil)
	server := NewServer(Config{KeepAlive: -1})
	server.closeSessions()
	server.publishSession(s)
	if !wire.closed.Load() {
		t.Fatal("a session announced after shutdown was left live")
	}
	if _, ok := <-server.Sessions(); ok {
		t.Fatal("server shutdown reopened its announcement queue")
	}
}

func TestSessionClosesOnContradictoryFlags(t *testing.T) {
	for _, tc := range []struct {
		name string
		h    frameHeader
	}{{"data fin+rst", frameHeader{typ: typeData, flags: flagFIN | flagRST, streamID: 1}}, {"data syn+ack", frameHeader{typ: typeData, flags: flagSYN | flagACK, streamID: 1}}, {"data ack+rst", frameHeader{typ: typeData, flags: flagACK | flagRST, streamID: 1}}, {"update syn+ack", frameHeader{typ: typeWindowUpdate, flags: flagSYN | flagACK, streamID: 1}}, {"ping bare", frameHeader{typ: typePing}}, {"data on the session", frameHeader{typ: typeData, streamID: 0}}} {
		t.Run(tc.name, func(t *testing.T) {
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				_ = ln.Close()
			})
			accepted := make(chan net.Conn, 1)
			go func() {
				if c, err := ln.Accept(); err == nil {
					accepted <- c
				}
			}()
			cli := NewClient(Config{})
			t.Cleanup(func() {
				_ = cli.Close(nil)
			})
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			sess, err := cli.Dial(ctx, "tcp://"+ln.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			transport := <-accepted
			t.Cleanup(func() {
				_ = transport.Close()
			})
			hb := make([]byte, headerSize)
			encodeHeader(hb, tc.h)
			if _, err := transport.Write(hb); err != nil {
				t.Fatal(err)
			}
			waitFor(t, "session end on the invalid frame", sess.Closed)
			if cause := sess.Err(); !errors.Is(cause, errFrameFlags) {
				t.Fatalf("close cause = %v, want errFrameFlags", cause)
			}
		})
	}
}

func protocolRawPeer(t *testing.T) (*Session, net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = ln.Close()
	})
	type acceptResult struct {
		conn net.Conn
		err  error
	}
	accepted := make(chan acceptResult, 1)
	go func() {
		conn, err := ln.Accept()
		accepted <- acceptResult{conn, err}
	}()
	client := NewClient(Config{KeepAlive: -1})
	t.Cleanup(func() {
		_ = client.Close(nil)
		_ = client.Wait()
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sess, err := client.Dial(ctx, "tcp://"+ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-accepted:
		if result.err != nil {
			t.Fatal(result.err)
		}
		t.Cleanup(func() {
			_ = result.conn.Close()
		})
		return sess, result.conn
	case <-ctx.Done():
		t.Fatal("raw peer did not accept the transport")
		return nil, nil
	}
}

func protocolReadFrame(t *testing.T, peer net.Conn) (frameHeader, []byte) {
	t.Helper()
	if err := peer.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	var header [headerSize]byte
	if _, err := io.ReadFull(peer, header[:]); err != nil {
		t.Fatalf("read peer frame: %v", err)
	}
	h, err := decodeHeader(header[:])
	if err != nil {
		t.Fatal(err)
	}
	var payload []byte
	if h.typ == typeData || h.typ == typeSynMetadata {
		if h.length > 1<<20 {
			t.Fatalf("unexpected peer payload length %d", h.length)
		}
		payload = make([]byte, int(h.length))
		if _, err := io.ReadFull(peer, payload); err != nil {
			t.Fatalf("read peer payload: %v", err)
		}
	}
	return h, payload
}

func protocolWriteFrame(t *testing.T, peer net.Conn, h frameHeader, payload []byte) {
	t.Helper()
	if err := peer.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	frame := rawFramed(h.typ, h.flags, h.streamID, h.length, payload)
	if n, err := peer.Write(frame); err != nil || n != len(frame) {
		t.Fatalf("write peer frame = %d/%d, %v", n, len(frame), err)
	}
}

func protocolReplyToOpen(t *testing.T, sess *Session, peer net.Conn, typ uint8, flags uint16, payload []byte) *Stream {
	t.Helper()
	type openResult struct {
		stream *Stream
		err    error
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	opened := make(chan openResult, 1)
	go func() {
		stream, err := sess.Open(ctx)
		opened <- openResult{stream, err}
	}()
	syn, _ := protocolReadFrame(t, peer)
	if syn.typ != typeWindowUpdate || syn.flags != flagSYN {
		t.Fatalf("open sent %+v, want WindowUpdate|SYN", syn)
	}
	length := uint32(0)
	if typ == typeData {
		length = uint32(len(payload))
	}
	protocolWriteFrame(t, peer, frameHeader{typ: typ, flags: flags, streamID: syn.streamID, length: length}, payload)
	select {
	case result := <-opened:
		if result.err != nil {
			t.Fatalf("Open after peer reply: %v", result.err)
		}
		if result.stream.ID() != syn.streamID {
			t.Fatalf("opened stream ID = %d, want %d", result.stream.ID(), syn.streamID)
		}
		return result.stream
	case <-ctx.Done():
		t.Fatal("Open did not finish after the peer acknowledged it")
		return nil
	}
}

func protocolAssertSecondStream(t *testing.T, sess *Session, peer net.Conn) {
	t.Helper()
	if sess.Closed() {
		t.Fatalf("session closed after a legal combined frame: %v", sess.Err())
	}
	second := protocolReplyToOpen(t, sess, peer, typeData, flagACK|flagFIN, []byte("second"))
	if got := string(readFull(t, second, len("second"))); got != "second" {
		t.Fatalf("second stream payload = %q", got)
	}
	waitEOF(t, second)
	writeAll(t, second, []byte("reply"))
	h, payload := protocolReadFrame(t, peer)
	if h.typ != typeData || h.streamID != second.ID() || string(payload) != "reply" {
		t.Fatalf("second stream write = %+v %q", h, payload)
	}
	if sess.Closed() {
		t.Fatalf("second stream ended the session: %v", sess.Err())
	}
}

func TestSessionAcceptsAckAndFinRidingOnData(t *testing.T) {
	for _, tc := range []struct {
		name    string
		typ     uint8
		flags   uint16
		payload string
	}{{"data_ack_empty", typeData, flagACK, ""}, {"data_ack_payload", typeData, flagACK, "hello"}, {"data_ack_fin_empty", typeData, flagACK | flagFIN, ""}, {"data_ack_fin_payload", typeData, flagACK | flagFIN, "hello"}, {"window_ack_fin", typeWindowUpdate, flagACK | flagFIN, ""}} {
		t.Run(tc.name, func(t *testing.T) {
			sess, peer := protocolRawPeer(t)
			st := protocolReplyToOpen(t, sess, peer, tc.typ, tc.flags, []byte(tc.payload))
			if got := string(readFull(t, st, len(tc.payload))); got != tc.payload {
				t.Fatalf("payload = %q, want %q", got, tc.payload)
			}
			if tc.flags&flagFIN == 0 {
				if st.State()&uio.StateReadEnded != 0 {
					t.Fatal("an ACK without FIN ended the peer's half")
				}
				protocolWriteFrame(t, peer, frameHeader{typ: typeData, flags: flagFIN, streamID: st.ID()}, nil)
			}
			waitEOF(t, st)
			protocolAssertSecondStream(t, sess, peer)
		})
	}
}

func TestPeerGoAwayRefusesOpen(t *testing.T) {
	s, _ := recPair(Config{})
	if err := s.handleGoAway(frameHeader{typ: typeGoAway, length: goAwayNormal}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := s.Open(ctx); !errors.Is(err, ErrGoAway) {
		t.Fatalf("Open = %v, want ErrGoAway", err)
	}
}

type drainWire struct {
	uio.Conn
	mu       sync.Mutex
	overflow bool
	frames   []frameHeader
	closed   int
	closeErr error
}

func (w *drainWire) Wake() error {
	return nil
}

func (w *drainWire) SetWakeDeadline(time.Time) error {
	return nil
}

func (w *drainWire) Write(p []byte) (int, error) {
	w.mu.Lock()
	if w.overflow {
		w.overflow = false
		w.mu.Unlock()
		return 0, uio.ErrOutboundOverflow
	}
	if len(p) >= headerSize {
		if h, err := decodeHeader(p[:headerSize]); err == nil {
			w.frames = append(w.frames, h)
		}
	}
	w.mu.Unlock()
	return len(p), nil
}

func (w *drainWire) WriteOwned(b *uio.Buffer) (int, error) {
	n := b.Len()
	uio.ReleaseBuffer(b)
	return n, nil
}

func (w *drainWire) Close() error {
	return w.Abort(nil)
}

func (w *drainWire) Abort(err error) error {
	w.mu.Lock()
	w.closed++
	w.closeErr = err
	w.mu.Unlock()
	return nil
}

func TestShutdownWaitsForGoAwayFlush(t *testing.T) {
	wire := &drainWire{overflow: true}
	s := &Session{cfg: Config{KeepAlive: -1}.resolve(), conn: wire, nextID: 2, streams: make(map[uint32]*Stream), openWait: make(map[uint32]chan error)}
	done := make(chan error, 1)
	go func() {
		done <- s.Shutdown(context.Background())
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		wire.mu.Lock()
		stuck := wire.overflow
		wire.mu.Unlock()
		if !stuck {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Shutdown never tried to write the GoAway")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case err := <-done:
		t.Fatalf("Shutdown = %v before the GoAway reached the transport", err)
	case <-time.After(50 * time.Millisecond):
	}
	wire.mu.Lock()
	closed := wire.closed
	wire.mu.Unlock()
	if closed != 0 {
		t.Fatal("transport closed while the GoAway was still queued")
	}
	s.flush()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Shutdown = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Shutdown did not return after the GoAway was flushed")
	}
	wire.mu.Lock()
	defer wire.mu.Unlock()
	if wire.closed == 0 || wire.closeErr != nil {
		t.Fatalf("transport close = %d with %v, want one clean close", wire.closed, wire.closeErr)
	}
	sawGoAway := false
	for _, h := range wire.frames {
		if h.typ == typeGoAway {
			sawGoAway = true
		}
	}
	if !sawGoAway {
		t.Fatal("the transport never saw the GoAway frame")
	}
}

func TestShutdownForceUnderReadHold(t *testing.T) {
	h := &recHandler{}
	s, _ := recPair(Config{Handler: h})
	st := newStream(s, 1, false, int64(s.cfg.WindowSize))
	s.mu.Lock()
	s.streams[1] = st
	s.readHeld = true
	s.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := s.Shutdown(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown = %v, want the context's deadline error", err)
	}
	s.mu.Lock()
	left := len(s.streams)
	s.mu.Unlock()
	if left != 0 {
		t.Fatalf("%d streams survived the forced shutdown", left)
	}
	if st.State()&uio.StateClosed == 0 {
		t.Fatal("stream stayed open after the forced shutdown")
	}
}

type echoServer struct {
	opened chan *Stream
	closed chan error
}

func newEchoServer() *echoServer {
	return &echoServer{opened: make(chan *Stream, 16), closed: make(chan error, 16)}
}

func (h *echoServer) OnOpen(c uio.Conn) {
	select {
	case h.opened <- c.(*Stream):
	default:
	}
}

func (h *echoServer) OnRead(c uio.Conn) error {
	st := c.(*Stream)
	buf := make([]byte, 8<<10)
	for {
		n, err := st.Read(buf)
		if n > 0 {
			if _, werr := st.Write(buf[:n]); werr != nil {
				return werr
			}
			continue
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return err
		}
		break
	}
	if st.State()&uio.StateReadEnded != 0 {
		return st.CloseWrite()
	}
	return nil
}

func (h *echoServer) OnTimer(uio.Conn) error {
	return nil
}

func (h *echoServer) OnClose(c uio.Conn, err error) {
	select {
	case h.closed <- err:
	default:
	}
}

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

func writeAll(t *testing.T, st *Stream, p []byte) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for len(p) > 0 {
		n, err := st.Write(p)
		p = p[n:]
		if err == nil {
			continue
		}
		if !errors.Is(err, uio.ErrOutboundOverflow) {
			t.Fatalf("write: %v", err)
		}
		if len(p) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("window never reopened with %d bytes left", len(p))
		}
		time.Sleep(time.Millisecond)
	}
}

func dialTCP(t *testing.T, addr string) net.Conn {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		conn, err := net.DialTimeout("tcp", addr, time.Second)
		if err == nil {
			return conn
		}
		if time.Now().After(deadline) {
			t.Fatalf("dial %s: %v", addr, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func startPair(t *testing.T, serverCfg, clientCfg Config) (*Session, *echoServer) {
	t.Helper()
	addr := freeAddr(t)
	echo := newEchoServer()
	if serverCfg.Handler == nil {
		serverCfg.Handler = echo
	}
	srv := NewServer(serverCfg)
	go func() {
		_ = srv.Serve(addr)
	}()
	cli := NewClient(clientCfg)
	t.Cleanup(func() {
		_ = cli.Close(nil)
		_ = srv.Close(nil)
	})
	deadline := time.Now().Add(5 * time.Second)
	for {
		sess, err := cli.Dial(context.Background(), "tcp://"+addr)
		if err == nil {
			t.Cleanup(func() {
				_ = sess.Close(nil)
			})
			return sess, echo
		}
		if time.Now().After(deadline) {
			t.Fatalf("dial: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func readFull(t *testing.T, st *Stream, n int) []byte {
	t.Helper()
	buf := make([]byte, n)
	off := 0
	deadline := time.Now().Add(5 * time.Second)
	for off < n {
		m, err := st.Read(buf[off:])
		if err != nil {
			t.Fatalf("read %d/%d: %v", off, n, err)
		}
		if m > 0 {
			off += m
			continue
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out at %d/%d bytes", off, n)
		}
		time.Sleep(200 * time.Microsecond)
	}
	return buf
}

func waitEOF(t *testing.T, st *Stream) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	buf := make([]byte, 256)
	for {
		n, err := st.Read(buf)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return
			}
			t.Fatalf("read: %v", err)
		}
		if n > 0 {
			continue
		}
		if time.Now().After(deadline) {
			t.Fatal("no EOF received")
		}
		time.Sleep(200 * time.Microsecond)
	}
}

func TestSendWindowTakeAndGrant(t *testing.T) {
	w := newSendWindow(100)
	if !w.take(60) || w.available() != 40 {
		t.Fatalf("take(60): avail = %d, want 40", w.available())
	}
	if w.take(41) {
		t.Fatal("take beyond the window succeeded")
	}
	if err := w.grant(50); err != nil || w.available() != 90 {
		t.Fatalf("grant(50): avail = %d, err = %v", w.available(), err)
	}
	if err := w.grant(11); !errors.Is(err, errWindowExceeded) {
		t.Fatalf("grant past the maximum: err = %v, want errWindowExceeded", err)
	}
	if err := w.grant(0); !errors.Is(err, errWindowInvalid) {
		t.Fatalf("grant(0): err = %v, want errWindowInvalid", err)
	}
}

func TestRecvWindowGrantsAtQuarter(t *testing.T) {
	w := newRecvWindow(100)
	if err := w.onData(90); err != nil {
		t.Fatalf("onData(90): %v", err)
	}
	if grant := w.consume(20); grant != 0 {
		t.Fatalf("grant below a quarter window = %d, want 0", grant)
	}
	if grant := w.consume(5); grant != 25 {
		t.Fatalf("grant at a quarter window = %d, want 25", grant)
	}
	if w.bufferedBytes() != 65 {
		t.Fatalf("buffered = %d, want 65", w.bufferedBytes())
	}
}

func TestRecvWindowRejectsOverrun(t *testing.T) {
	w := newRecvWindow(100)
	if err := w.onData(101); !errors.Is(err, errWindowExceeded) {
		t.Fatalf("onData past the window: err = %v, want errWindowExceeded", err)
	}
}

func TestRecvWindowIgnoresBadConsume(t *testing.T) {
	w := newRecvWindow(100)
	if err := w.onData(10); err != nil {
		t.Fatal(err)
	}
	if grant := w.consume(11); grant != 0 {
		t.Fatalf("consume beyond buffered returned a grant %d", grant)
	}
	if grant := w.consume(-1); grant != 0 {
		t.Fatalf("negative consume returned a grant %d", grant)
	}
}
