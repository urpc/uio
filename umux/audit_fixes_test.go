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

package umux

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urpc/uio"
)

// recWire is a transport stand-in that records the deadlines it is armed with
// and can be told to fail writes.
type recWire struct {
	uio.Conn
	mu        sync.Mutex
	deadlines []time.Time
	failWrite error
}

func (*recWire) Wake() error           { return nil }
func (*recWire) CloseWith(error) error { return nil }
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

// recHandler records the handler callbacks it receives.
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

func (h *recHandler) OnOpen(uio.Conn) {}

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

// recPair builds a white-box session over a recording wire.
func recPair(cfg Config) (*Session, *recWire) {
	wire := &recWire{}
	cfg.KeepAlive = -1
	s := &Session{
		cfg:      cfg.resolve(),
		conn:     wire,
		streams:  make(map[uint32]*Stream),
		openWait: make(map[uint32]chan error),
	}
	return s, wire
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// #1: WriteTo hands its queue to a writer; bytes arriving meanwhile must not
// compact the queue in place and rewrite what the writer still reads.
func TestWriteToBorrowStaysValid(t *testing.T) {
	s, _ := recPair(Config{})
	st := newStream(s, 1, false, int64(s.cfg.WindowSize))
	s.streams[1] = st
	if err := st.pushData([]byte("abcd"), false, false); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Discard(2); err != nil { // leave the queue compactable
		t.Fatal(err)
	}
	entered := make(chan []byte, 1)
	release := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = st.WriteTo(writerFunc(func(p []byte) (int, error) {
			entered <- p
			<-release
			return len(p), nil
		}))
	}()
	borrowed := <-entered
	if err := st.pushData([]byte("ef"), false, false); err != nil {
		t.Fatal(err)
	}
	if got := string(borrowed); got != "cd" {
		close(release)
		<-done
		t.Fatalf("borrowed bytes became %q, want %q", got, "cd")
	}
	close(release)
	<-done
}

// #2: a CloseWith from another goroutine must not run OnClose while the
// stream's OnRead callback is still executing.
func TestCloseQueuedBehindRead(t *testing.T) {
	h := &recHandler{readIn: make(chan struct{}, 1), readOut: make(chan struct{})}
	s, _ := recPair(Config{Handler: h})
	st := newStream(s, 1, false, int64(s.cfg.WindowSize))
	s.streams[1] = st
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.dispatchRead(st)
	}()
	<-h.readIn // OnRead is inside its callback now
	if err := st.CloseWith(errors.New("gone")); err != nil {
		t.Fatal(err)
	}
	if n := h.closeCount(); n != 0 {
		close(h.readOut)
		<-done
		t.Fatalf("OnClose ran %d times while OnRead was in flight", n)
	}
	close(h.readOut)
	<-done
	s.drainCloses() // the turn's end delivers what queueClose queued
	if h.closeCount() != 1 {
		t.Fatalf("closes = %d, want 1", h.closeCount())
	}
	if h.overlap.Load() {
		t.Fatal("OnClose overlapped OnRead")
	}
}

// #3: a transport write error must leave the teardown to the transport's
// close path, which releases the streams and wakes the Open waiters.
func TestTransportWriteErrorKeepsTeardownPath(t *testing.T) {
	h := &recHandler{}
	s, wire := recPair(Config{Handler: h, StreamOpenTimeout: time.Minute})
	wire.failWrite = errors.New("write failed")

	st := newStream(s, 2, false, int64(s.cfg.WindowSize))
	s.streams[2] = st
	st.out = append(st.out, frameBuffer(16))
	st.queued = true
	s.pending = append(s.pending, 2)

	opening := newStream(s, 4, true, int64(s.cfg.WindowSize))
	s.streams[4] = opening
	wait := make(chan error, 1)
	s.openWait[4] = wait

	s.mu.Lock()
	ferr := s.flushLocked()
	closed := s.closed
	s.mu.Unlock()
	if ferr == nil || closed {
		t.Fatalf("flush = %v, closed = %v; want an error and no self-close", ferr, closed)
	}

	s.onTransportClose(errors.New("transport gone"))
	select {
	case err := <-wait:
		if err == nil {
			t.Fatal("open waiter woke without a cause")
		}
	case <-time.After(time.Second):
		t.Fatal("open waiter never woke after the transport closed")
	}
	if h.closeCount() != 2 {
		t.Fatalf("closes = %d, want 2", h.closeCount())
	}
	if s.Closed() != true {
		t.Fatal("session did not close with its transport")
	}
}

// #4: an asymmetric window must not let the sender overrun the receiver: the
// ACK announces the peer's window and the write stops there.
func TestAsymmetricWindowIsRespected(t *testing.T) {
	slow := &slowServer{opened: make(chan *Stream, 1)}
	sess, _ := startPair(t,
		Config{Handler: slow, WindowSize: 256 << 10},
		Config{WindowSize: 1 << 20})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	st, err := sess.Open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer st.CloseWith(nil)
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

// #7: Open arms the open-stream timeout even when keepalive is off.
func TestOpenArmsTimeoutWithoutKeepalive(t *testing.T) {
	s, wire := recPair(Config{StreamOpenTimeout: 40 * time.Millisecond})
	before := wire.deadlineCount()
	done := make(chan error, 1)
	go func() {
		_, err := s.Open(context.Background())
		done <- err
	}()
	deadline := time.Now().Add(time.Second)
	for wire.deadlineCount() == before && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if wire.deadlineCount() == before {
		t.Fatal("Open never armed the transport timer")
	}
	if d := wire.lastDeadline(); d.IsZero() || d.After(time.Now().Add(time.Second)) {
		t.Fatalf("armed deadline = %v, want one bounded by the open timeout", d)
	}
	time.Sleep(50 * time.Millisecond)
	_ = s.onTimer(wire)
	select {
	case err := <-done:
		if !errors.Is(err, ErrStreamOpenTimeout) {
			t.Fatalf("Open = %v, want ErrStreamOpenTimeout", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Open never timed out")
	}
}

// #8: the timer setters replace each other, and an interval arms its first
// callback.
func TestTimerSettersReplaceEachOther(t *testing.T) {
	s, _ := recPair(Config{})
	st := newStream(s, 1, false, int64(s.cfg.WindowSize))
	s.streams[1] = st

	if err := st.SetWakeInterval(30 * time.Millisecond); err != nil {
		t.Fatal(err)
	}
	st.mu.Lock()
	armed, du := st.wakeAt, st.wakeDu
	st.mu.Unlock()
	if du != 30*time.Millisecond || armed.IsZero() || time.Until(armed) > 30*time.Millisecond {
		t.Fatalf("interval: wakeAt=%v wakeDu=%v, want an armed 30ms interval", armed, du)
	}

	when := time.Now().Add(time.Hour)
	if err := st.SetWakeDeadline(when); err != nil {
		t.Fatal(err)
	}
	st.mu.Lock()
	armed, du = st.wakeAt, st.wakeDu
	st.mu.Unlock()
	if du != 0 || !armed.Equal(when) {
		t.Fatalf("deadline replaced by interval: wakeAt=%v wakeDu=%v", armed, du)
	}

	if err := st.SetWakeInterval(0); err != nil {
		t.Fatal(err)
	}
	st.mu.Lock()
	armed, du = st.wakeAt, st.wakeDu
	st.mu.Unlock()
	if du != 0 || !armed.IsZero() {
		t.Fatalf("cancel left wakeAt=%v wakeDu=%v", armed, du)
	}
}

// #9: a deadline a timer callback sets must survive the arm at the turn's end.
func TestTimerRearmAfterCallback(t *testing.T) {
	var st *Stream
	h := &recHandler{onTimer: func(c uio.Conn) error {
		return c.SetWakeDeadline(time.Now().Add(time.Hour))
	}}
	s, wire := recPair(Config{Handler: h})
	st = newStream(s, 1, false, int64(s.cfg.WindowSize))
	s.streams[1] = st
	st.mu.Lock()
	st.wakeAt = time.Now().Add(-time.Millisecond)
	st.mu.Unlock()

	_ = s.onTimer(wire)
	if d := wire.lastDeadline(); time.Until(d) < 30*time.Minute {
		t.Fatalf("armed deadline = %v after the callback set one an hour out", d)
	}
}

// #10: an OnTimer error closes the stream, as every other callback's does.
func TestOnTimerErrorClosesStream(t *testing.T) {
	boom := errors.New("timer boom")
	h := &recHandler{onTimer: func(uio.Conn) error { return boom }}
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

// #11: a stream dropped by the half-close timeout or by the transport's close
// releases its queued frames rather than holding them while the application
// still holds the Stream.
func TestTimeoutsDropQueuedFrames(t *testing.T) {
	s, wire := recPair(Config{StreamCloseTimeout: 20 * time.Millisecond})
	st := newStream(s, 1, false, int64(s.cfg.WindowSize))
	s.streams[1] = st
	st.out = append(st.out, frameBuffer(8))
	st.queued = true
	s.pending = append(s.pending, 1)
	st.mu.Lock()
	st.localFIN = true
	st.halfSince = time.Now().Add(-time.Second)
	st.mu.Unlock()

	_ = s.onTimer(wire)
	if s.streams[st.id] != nil {
		t.Fatal("half-closed stream survived its close timeout")
	}
	if len(st.out) != 0 {
		t.Fatalf("%d queued frames retained after the timeout reset", len(st.out))
	}

	// The transport's close must drop queued frames too.
	s2, _ := recPair(Config{})
	live := newStream(s2, 2, false, int64(s2.cfg.WindowSize))
	s2.streams[2] = live
	live.out = append(live.out, frameBuffer(8))
	live.queued = true
	s2.onTransportClose(errors.New("gone"))
	if len(live.out) != 0 {
		t.Fatalf("%d queued frames retained after the transport closed", len(live.out))
	}
}

// #5: pairing a stream that already reset must reset the user leg at once
// instead of leaving it paired with a corpse.
func TestBridgePairRejectsClosedStream(t *testing.T) {
	echo := newEchoServer()
	echoAddr := freeAddr(t)
	srv := NewServer(Config{Handler: echo})
	go func() { _ = srv.Serve(echoAddr) }()
	t.Cleanup(func() { _ = srv.Close(nil) })
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
	t.Cleanup(func() { _ = cli.Close(nil) })

	br := &uio.Bridge{Dial: func(ctx context.Context, _ uio.Conn, _ uio.DialFunc) (uio.Conn, error) {
		st, err := sess.Open(ctx)
		if err != nil {
			return nil, err
		}
		_ = st.CloseWith(errors.New("gone before pairing"))
		return st, nil
	}}
	ev := br.Events()
	userAddr := freeAddr(t)
	go func() { _ = ev.Serve("tcp://" + userAddr) }()
	t.Cleanup(func() { _ = ev.Close(nil) })

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

// #6: the dial context covers the transport dial itself.
func TestDialContextCoversDial(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cli := NewClient(Config{})
	defer cli.Close(nil)
	if _, err := cli.Dial(ctx, "tcp://"+ln.Addr().String()); err == nil {
		t.Fatal("dial with a canceled context succeeded")
	}
	_ = ln.(*net.TCPListener).SetDeadline(time.Now().Add(300 * time.Millisecond))
	if c, aerr := ln.Accept(); aerr == nil {
		_ = c.Close()
		t.Fatal("the transport dial ran despite a canceled context")
	}
}
