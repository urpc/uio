package umux

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urpc/uio"
)

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

func (c *productionSessionWire) CloseWith(error) error {
	c.closed.Store(true)
	return nil
}

func TestTimerShortcutAndRescanPublishInOrder(t *testing.T) {
	for _, first := range []string{"shortcut", "rescan"} {
		t.Run(first, func(t *testing.T) {
			wire := &productionSessionWire{}
			s := newSession(wire, Config{KeepAlive: -1}.resolve(), true)
			st := newStream(s, 1, false, int64(s.cfg.WindowSize))
			s.streams[1] = st
			later := time.Now().Add(time.Hour)
			earlier := later.Add(-time.Minute)
			entered := make(chan struct{})
			release := make(chan struct{})
			var once sync.Once
			wire.timer = func(when time.Time) {
				if when.Equal(later) {
					once.Do(func() { close(entered); <-release })
				}
			}
			firstDone := make(chan struct{})
			if first == "shortcut" {
				go func() { s.armNotLaterThan(later); close(firstDone) }()
			} else {
				st.wakeAt = later
				go func() { s.rearmTimer(); close(firstDone) }()
			}
			<-entered
			secondDone := make(chan struct{})
			secondStarted := make(chan struct{})
			go func() {
				if first == "shortcut" {
					st.mu.Lock()
					st.wakeAt = earlier
					st.mu.Unlock()
					close(secondStarted)
					s.rearmTimer()
				} else {
					close(secondStarted)
					s.armNotLaterThan(earlier)
				}
				close(secondDone)
			}()
			<-secondStarted
			// The first submission must keep the shared lock until its setter
			// returns, even when it came through the O(1) Open shortcut.
			select {
			case <-secondDone:
				close(release)
				<-firstDone
				t.Fatal("a later submission passed an in-flight timer setter")
			case <-time.After(20 * time.Millisecond):
			}
			close(release)
			<-firstDone
			<-secondDone
			if got := wire.lastDeadline(); !got.Equal(earlier) {
				t.Fatalf("deadline = %s, want %s", got, earlier)
			}
			// An already earlier arm remains a constant-time no-op.
			s.armNotLaterThan(later)
			if got := wire.lastDeadline(); !got.Equal(earlier) {
				t.Fatalf("redundant Open postponed deadline to %s", got)
			}
		})
	}
}

func TestSessionRendezvousSettlesCloseDuringWake(t *testing.T) {
	wire := &productionSessionWire{}
	r := &router{cfg: Config{KeepAlive: -1}.resolve(), client: true}
	boom := errors.New("closed after waiter registration")
	wire.wake = func() error {
		r.OnClose(wire, boom)
		return nil // the liveness check raced the close callback
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
	wire.wake = func() error { cancel(); return nil }
	if _, err := r.waitSession(ctx, wire); !errors.Is(err, context.Canceled) {
		t.Fatalf("waitSession = %v, want context cancellation", err)
	}
	if !wire.closed.Load() || len(r.waiting) != 0 {
		t.Fatal("cancellation failed to close the transport and remove its waiter")
	}
	// Events preserves OnOpen -> OnClose ordering even when cancellation
	// requests a close before the first callback starts.
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
	cli.router.announce = func(sess *Session) { opened <- sess }
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
	// Reproduce a caller preempted between the two halves of Client.Dial.
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

type productionBlockedHandshake struct{ uio.PassThrough }

func (productionBlockedHandshake) Attach(uio.LayerContext) uio.LayerHandler {
	return productionBlockedHandshake{}
}

func (productionBlockedHandshake) OnOpen(uio.LayerContext) {}

func TestSessionDialContextCancelsUnopenedLayer(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := ln.Accept()
		if err == nil {
			accepted <- conn
		}
	}()
	cli := NewClient(Config{KeepAlive: -1})
	defer cli.Close(nil)
	if err := cli.start(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	_, err = cli.Dial(ctx, "tcp://"+ln.Addr().String(), uio.WithLayers(productionBlockedHandshake{}))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Dial = %v, want context deadline", err)
	}
	var peer net.Conn
	select {
	case peer = <-accepted:
	case <-time.After(time.Second):
		t.Fatal("TCP peer never accepted")
	}
	defer peer.Close()
	_ = peer.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := peer.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Fatalf("cancelled handshake transport remained open: %v", err)
	}
	cli.router.mu.Lock()
	defer cli.router.mu.Unlock()
	if len(cli.router.waiting) != 0 || len(cli.router.conns) != 0 {
		t.Fatal("cancelled handshake retained routing state")
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
	expectFrame(t, conn, func(h frameHeader) bool { return h.flags&flagACK != 0 })
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
	for _, cfg := range []Config{
		{},
		{ReadBufferSize: 4 << 10, SessionWindow: 64 << 10},
		{ReadBufferSize: maxInt, SessionWindow: maxInt, WindowSize: maxInt},
	} {
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

func TestKeepaliveRequiresMatchingPongSequence(t *testing.T) {
	wire := &productionSessionWire{}
	s := newSession(wire, Config{KeepAlive: time.Second}.resolve(), true)
	s.lastPing = time.Now().Add(-2 * time.Second)
	if err := s.onTimer(wire); err != nil {
		t.Fatal(err)
	}
	if len(wire.written) != 1 {
		t.Fatalf("keepalive wrote %d frames, want one ping", len(wire.written))
	}
	ping := wire.written[0]
	if ping.typ != typePing || ping.flags != flagSYN || ping.streamID != 0 || ping.length != s.pingSeq {
		t.Fatalf("keepalive header = %+v, want session ping with sequence %d", ping, s.pingSeq)
	}
	if err := s.handlePing(frameHeader{typ: typePing, flags: flagACK, length: ping.length + 1}); err != nil {
		t.Fatal(err)
	}
	if s.pingSent.IsZero() {
		t.Fatal("an unrelated pong cancelled the keepalive timeout")
	}
	if err := s.handlePing(frameHeader{typ: typePing, length: ping.length}); err != nil {
		t.Fatal(err)
	}
	if s.pingSent.IsZero() {
		t.Fatal("a ping without ACK cancelled the keepalive timeout")
	}
	if err := s.handlePing(frameHeader{typ: typePing, flags: flagACK, length: ping.length}); err != nil {
		t.Fatal(err)
	}
	if !s.pingSent.IsZero() {
		t.Fatal("the matching pong did not acknowledge the keepalive")
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
	s.onTransportClose(io.EOF) // repeated close neither panics nor changes Err
	if !errors.Is(s.Err(), boom) {
		t.Fatal("a repeated close changed the terminal cause")
	}
	// White-box sessions may omit the lifecycle channel; teardown remains safe.
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

func (*productionAcceptCloseObserver) OnBytesRead(uio.Conn, int)    {}
func (*productionAcceptCloseObserver) OnBytesWritten(uio.Conn, int) {}
func (o *productionAcceptCloseObserver) OnQueueDrained(uio.Conn) {
	close(o.entered)
	<-o.release
}

func TestSessionAcceptSendCannotRaceChannelClose(t *testing.T) {
	obs := &productionAcceptCloseObserver{entered: make(chan struct{}), release: make(chan struct{})}
	s := newSession(&productionSessionWire{}, Config{KeepAlive: -1, Observer: obs}.resolve(), false)
	// Pause the SYN path after its ACK flushed but before its Accept send.
	s.drained = []*Stream{newStream(s, 3, false, int64(s.cfg.WindowSize))}
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

type productionFINThenErrorWire struct {
	productionSessionWire
	writes int
	err    error
}

func (w *productionFINThenErrorWire) WriteOwned(b *uio.Buffer) (int, error) {
	w.writes++
	n := b.Len()
	uio.ReleaseBuffer(b)
	if w.writes > 1 {
		return 0, w.err
	}
	return n, nil
}

func TestTransportCloseDropsTerminalSchedulingQueues(t *testing.T) {
	wire := &productionFINThenErrorWire{err: errors.New("write failed after FIN")}
	closed := 0
	h := uio.FuncHandler{Close: func(uio.Conn, error) { closed++ }}
	s := newSession(wire, Config{KeepAlive: -1, Handler: h}.resolve(), true)
	fin := newStream(s, 1, false, int64(s.cfg.WindowSize))
	s.streams[1] = fin
	if err := fin.pushData(nil, true, false); err != nil {
		t.Fatal(err)
	}
	if err := fin.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	other := newStream(s, 3, false, int64(s.cfg.WindowSize))
	s.streams[3] = other
	if _, err := other.Write([]byte("later data")); err != nil {
		t.Fatal(err)
	}
	if err := s.onTimer(wire); !errors.Is(err, wire.err) {
		t.Fatalf("timer flush = %v, want the second write to fail", err)
	}
	if len(s.finFlushed) != 1 {
		t.Fatal("test did not leave the successful FIN awaiting settlement")
	}
	// Queue kinds a terminal pass may leave behind must all drop references.
	s.drained = []*Stream{fin}
	s.wakeQ = []*Stream{other}
	s.control = [][]byte{make([]byte, headerSize)}
	s.onTransportClose(wire.err)
	if len(s.finFlushed)+len(s.drained)+len(s.wakeQ)+len(s.control)+len(s.pending) != 0 {
		t.Fatal("terminal session retained invalid scheduling queues")
	}
	if closed != 2 || len(s.closeQ) != 0 {
		t.Fatalf("terminal closes = %d, queued = %d", closed, len(s.closeQ))
	}
	// Repeated application closes must not append unsendable RST controls.
	_ = other.CloseWith(errors.New("late application close"))
	if len(s.control) != 0 {
		t.Fatal("a post-terminal close retained an unsendable control frame")
	}
}
