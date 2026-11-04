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
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urpc/uio"
)

// waitFor polls cond until it holds or the deadline passes.
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

// waitInbound polls until the stream holds at least n unread bytes.
func waitInbound(t *testing.T, st *Stream, n int) {
	t.Helper()
	waitFor(t, "inbound bytes", func() bool { return st.InboundBuffered() >= n })
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

	// Socket-only setters report ErrUnsupported.
	unsupported := map[string]error{
		"SetLinger":          st.SetLinger(0),
		"SetNoDelay":         st.SetNoDelay(true),
		"SetKeepAlive":       st.SetKeepAlive(true),
		"SetKeepAlivePeriod": st.SetKeepAlivePeriod(1),
		"Control":            st.Control(func(uintptr) {}),
		"SetReadBuffer":      st.SetReadBuffer(1),
		"SetWriteBuffer":     st.SetWriteBuffer(1),
	}
	for name, err := range unsupported {
		if !errors.Is(err, uio.ErrUnsupported) {
			t.Fatalf("%s: err = %v, want ErrUnsupported", name, err)
		}
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

	// Echo through the byte and string writers.
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

	// WriteOwned frames an owned buffer.
	owned := uio.AcquireBuffer(6)
	_, _ = owned.Write([]byte("owned!"))
	if n, err := st.WriteOwned(owned); err != nil || n != 6 {
		t.Fatalf("WriteOwned = %d, %v", n, err)
	}
	if got := readFull(t, st, 6); string(got) != "owned!" {
		t.Fatalf("WriteOwned echo = %q", got)
	}
	if _, err := st.ReserveOutbound(8); !errors.Is(err, uio.ErrUnsupported) {
		t.Fatalf("ReserveOutbound: %v", err)
	}
	if st.OutboundBuffered() < 0 {
		t.Fatal("OutboundBuffered is negative")
	}
	if err := st.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	// Peek/PeekChunk/Discard/WriteTo over buffered inbound.
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

	// Timers and wakes are accepted and rearm the session.
	if err := st.SetWakeDeadline(time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := st.SetWakeInterval(time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := st.Wake(); err != nil {
		t.Fatal(err)
	}
	if err := st.YieldRead(); err != nil {
		t.Fatal(err)
	}
	_ = st.SetWakeDeadline(time.Time{})
	_ = st.SetWakeInterval(0)

	// Half-close state, then a graceful full close.
	if err := st.CloseWrite(); err != nil {
		t.Fatalf("CloseWrite: %v", err)
	}
	if st.State()&uio.StateWriteShut == 0 {
		t.Fatal("State lacks StateWriteShut after CloseWrite")
	}
	// A close that completed before this write reports its cause instead
	// of ErrClosed; both sides' FINs can finish that fast.
	if _, err := st.Write([]byte("late")); !errors.Is(err, ErrClosed) && !errors.Is(err, io.EOF) {
		t.Fatalf("write after CloseWrite: %v", err)
	}
	waitEOF(t, st)
	if state := st.State(); state&uio.StateClosed == 0 || state&uio.StatePeerEnded == 0 {
		t.Fatalf("State after both FINs = %b", state)
	}

	other, err := sess.Open(nil) // a nil context means Background
	if err != nil {
		t.Fatal(err)
	}
	if err := other.CloseGracefully(nil); err != nil {
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
	if err := st.CloseWith(boom); err != nil {
		t.Fatalf("CloseWith: %v", err)
	}
	if _, err := st.Write([]byte("x")); !errors.Is(err, boom) {
		t.Fatalf("write after reset: %v", err)
	}
	if _, err := st.Read(make([]byte, 1)); !errors.Is(err, boom) {
		t.Fatalf("read after reset: %v", err)
	}
	if err := st.CloseWith(nil); err != nil {
		t.Fatalf("CloseWith(nil) after close: %v", err)
	}

	// A locally opening stream refuses writes.
	sess.mu.Lock()
	opening := newStream(sess, 97, true, int64(sess.cfg.WindowSize))
	opening.opening = true
	sess.mu.Unlock()
	if _, err := opening.Write([]byte("x")); !errors.Is(err, ErrClosed) {
		t.Fatalf("write on opening stream: %v", err)
	}

	// Session close is idempotent and Open fails afterwards.
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
	defer st.CloseWith(errors.New("done"))
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
	sess, _ := startPair(t, Config{Accept: func() error { return errors.New("denied") }}, Config{})
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
	defer first.CloseWith(errors.New("done"))
	if _, err := sess.Open(ctx); !errors.Is(err, ErrStreamReset) {
		t.Fatalf("Open past the peer's MaxStreams: %v", err)
	}
}

func TestBacklogFullRejects(t *testing.T) {
	addr := freeAddr(t)
	srv := NewServer(Config{AcceptBacklog: 1})
	go func() { _ = srv.Serve(addr) }()
	t.Cleanup(func() { _ = srv.Close(nil) })
	cli := NewClient(Config{})
	t.Cleanup(func() { _ = cli.Close(nil) })

	var sess *Session
	waitFor(t, "dial", func() bool {
		s, err := cli.Dial(context.Background(), "tcp://"+addr)
		sess = s
		return err == nil
	})
	t.Cleanup(func() { _ = sess.Close(nil) })
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
	// The accept queue holds one stream and is not drained: the next SYN is
	// refused.
	if _, err := sess.Open(ctx); !errors.Is(err, ErrStreamReset) {
		t.Fatalf("Open with a full backlog: %v", err)
	}
	if srvSess.Accept() == nil {
		t.Fatal("server session has no accept queue")
	}
}

func TestPullModeEcho(t *testing.T) {
	addr := freeAddr(t)
	srv := NewServer(Config{})
	go func() { _ = srv.Serve(addr) }()
	t.Cleanup(func() { _ = srv.Close(nil) })
	cli := NewClient(Config{})
	t.Cleanup(func() { _ = cli.Close(nil) })

	var sess *Session
	waitFor(t, "dial", func() bool {
		s, err := cli.Dial(context.Background(), "tcp://"+addr)
		sess = s
		return err == nil
	})
	t.Cleanup(func() { _ = sess.Close(nil) })
	srvSess := <-srv.Sessions()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	st, err := sess.Open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var serverStream *Stream
	select {
	case in := <-srvSess.Accept():
		serverStream = in.Stream
	case <-time.After(5 * time.Second):
		t.Fatal("server did not accept the stream")
	}
	go func() {
		buf := make([]byte, 4<<10)
		for {
			n, err := serverStream.Read(buf)
			if n > 0 {
				if _, werr := serverStream.Write(buf[:n]); werr != nil {
					return
				}
			}
			if err != nil {
				_ = serverStream.CloseWrite()
				return
			}
			if n == 0 {
				time.Sleep(100 * time.Microsecond)
			}
		}
	}()
	payload := bytes.Repeat([]byte("p"), 8<<10)
	writeAll(t, st, payload)
	if got := readFull(t, st, len(payload)); !bytes.Equal(got, payload) {
		t.Fatalf("echo mismatched: %d bytes", len(got))
	}
	if err := st.CloseWrite(); err != nil {
		t.Fatalf("close write: %v", err)
	}
	waitEOF(t, st)
}

func TestSessionWindowOverrunHoldsNotFails(t *testing.T) {
	sess, _ := startPair(t, Config{SessionWindow: 1024}, Config{})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	st, err := sess.Open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer st.CloseWith(errors.New("done"))
	payload := bytes.Repeat([]byte("w"), 4096)
	writeAll(t, st, payload)
	if got := readFull(t, st, len(payload)); !bytes.Equal(got, payload) {
		t.Fatalf("echo mismatched: %d bytes", len(got))
	}
	if sess.Closed() {
		t.Fatal("session closed by its receive budget")
	}
}

// slowServer keeps each accepted stream without reading it.
type slowServer struct{ opened chan *Stream }

func (h *slowServer) OnOpen(c uio.Conn) {
	if st, ok := c.(*Stream); ok {
		select {
		case h.opened <- st:
		default:
		}
	}
}

func (h *slowServer) OnRead(uio.Conn) error   { return nil }
func (h *slowServer) OnClose(uio.Conn, error) {}
func (h *slowServer) OnTimer(uio.Conn) error  { return nil }

func TestSessionBudgetHoldsReads(t *testing.T) {
	const budget = 64 << 10
	const total = 200 << 10
	slow := &slowServer{opened: make(chan *Stream, 1)}
	sess, _ := startPair(t, Config{SessionWindow: budget, Handler: slow}, Config{})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cl, err := sess.Open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer cl.CloseWith(errors.New("done"))
	payload := make([]byte, total)
	for i := range payload {
		payload[i] = byte(i * 7)
	}
	writeAll(t, cl, payload)

	var srv *Stream
	select {
	case srv = <-slow.opened:
	case <-time.After(5 * time.Second):
		t.Fatal("server never saw the stream")
	}
	waitFor(t, "budget reached", func() bool { return srv.InboundBuffered() >= budget })
	time.Sleep(200 * time.Millisecond)
	if n := srv.InboundBuffered(); n > budget+(64<<10) {
		t.Fatalf("inbound %d exceeds budget %d: transport reads were not held", n, budget)
	}
	if sess.Closed() {
		t.Fatal("session closed by its receive budget")
	}

	// Draining the stream resumes parsing; the whole write arrives.
	got := make([]byte, 0, total)
	buf := make([]byte, 33<<10)
	deadline := time.Now().Add(5 * time.Second)
	for len(got) < total {
		n, rerr := srv.Read(buf)
		if rerr != nil {
			t.Fatalf("read: %v", rerr)
		}
		got = append(got, buf[:n]...)
		if n == 0 {
			if time.Now().After(deadline) {
				t.Fatalf("read stalled at %d/%d bytes", len(got), total)
			}
			time.Sleep(time.Millisecond)
		}
	}
	if !bytes.Equal(got, payload) {
		t.Fatal("payload mismatched after the hold released")
	}
}

// silentServer drains each stream but never closes its direction, so the
// half-close timeout has to reset it.
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

func (h *silentServer) OnTimer(uio.Conn) error  { return nil }
func (h *silentServer) OnClose(uio.Conn, error) {}

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

func TestStreamOpenTimeoutTimer(t *testing.T) {
	sess, _ := startPair(t, Config{}, Config{StreamOpenTimeout: 50 * time.Millisecond})
	sess.mu.Lock()
	st := newStream(sess, 99, true, int64(sess.cfg.WindowSize))
	st.opening = true
	st.openedAt = time.Now().Add(-time.Second)
	sess.streams[99] = st
	wait := make(chan error, 1)
	sess.openWait[99] = wait
	sess.mu.Unlock()
	if err := sess.onTimer(sess.conn); err != nil {
		t.Fatalf("onTimer: %v", err)
	}
	select {
	case err := <-wait:
		if !errors.Is(err, ErrStreamOpenTimeout) {
			t.Fatalf("open wait got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("open timeout never fired")
	}
	if st.State()&uio.StateClosed == 0 {
		t.Fatal("timed-out stream is still open")
	}
	sess.mu.Lock()
	_, still := sess.streams[99]
	sess.mu.Unlock()
	if still {
		t.Fatal("timed-out stream stayed in the table")
	}
}

func TestKeepaliveAnswered(t *testing.T) {
	sess, _ := startPair(t,
		Config{KeepAlive: 20 * time.Millisecond, KeepAliveMax: 200 * time.Millisecond},
		Config{KeepAlive: 20 * time.Millisecond, KeepAliveMax: 200 * time.Millisecond})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	st, err := sess.Open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(120 * time.Millisecond) // several ping/pong rounds
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
		// Hold the transport open without answering anything.
		time.Sleep(2 * time.Second)
		_ = conn.Close()
	}()
	cli := NewClient(Config{KeepAlive: 30 * time.Millisecond, KeepAliveMax: 30 * time.Millisecond})
	t.Cleanup(func() { _ = cli.Close(nil) })
	sess, err := cli.Dial(context.Background(), "tcp://"+ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	waitFor(t, "keepalive timeout", sess.Closed)
}

// ------------------------------------------------------------- raw frames

func rawFramed(typ uint8, flags uint16, sid, length uint32, payload []byte) []byte {
	b := make([]byte, headerSize+len(payload))
	encodeHeader(b, frameHeader{typ: typ, flags: flags, streamID: sid, length: length})
	copy(b[headerSize:], payload)
	return b
}

// readRawFrame reads one frame from a raw transport, skipping its payload.
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

// expectFrame reads frames until one matches.
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
	go func() { _ = srv.Serve(addr) }()
	t.Cleanup(func() { _ = srv.Close(nil) })
	return addr
}

func TestRawUnknownStreamAndPing(t *testing.T) {
	conn := dialTCP(t, rawServer(t))
	defer conn.Close()
	// Data for a stream the server never saw earns a RST.
	if _, err := conn.Write(rawFramed(typeData, 0, 99, 1, []byte("x"))); err != nil {
		t.Fatal(err)
	}
	rst := expectFrame(t, conn, func(h frameHeader) bool { return h.flags&flagRST != 0 })
	if rst.streamID != 99 {
		t.Fatalf("RST for stream %d, want 99", rst.streamID)
	}
	// The session survives: a ping is answered.
	if _, err := conn.Write(rawFramed(typePing, flagSYN, 7, 9, nil)); err != nil {
		t.Fatal(err)
	}
	pong := expectFrame(t, conn, func(h frameHeader) bool { return h.typ == typePing })
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
	t.Run("data with SYN", func(t *testing.T) {
		conn := dialTCP(t, addr)
		defer conn.Close()
		if _, err := conn.Write(rawFramed(typeData, flagSYN, 3, 1, []byte("x"))); err != nil {
			t.Fatal(err)
		}
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		if _, err := io.ReadFull(conn, make([]byte, 1)); err == nil {
			t.Fatal("connection stayed open after a malformed data frame")
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
		ack := expectFrame(t, conn, func(h frameHeader) bool { return h.flags&flagACK != 0 })
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
		// A SYN against a going-away session is refused with RST, so the
		// opener fails now instead of waiting for its own open timeout.
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
		expectFrame(t, conn, func(h frameHeader) bool { return h.flags&flagACK != 0 })
		// A grant past the sender's maximum is refused without closing.
		if _, err := conn.Write(rawFramed(typeWindowUpdate, 0, 9, 1<<30, nil)); err != nil {
			t.Fatal(err)
		}
		if _, err := conn.Write(rawFramed(typePing, flagSYN, 3, 3, nil)); err != nil {
			t.Fatal(err)
		}
		expectFrame(t, conn, func(h frameHeader) bool { return h.typ == typePing && h.flags&flagACK != 0 })
	})
}

func TestQueuedFramesDroppedOnAbort(t *testing.T) {
	sess, _ := startPair(t, Config{}, Config{})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	st, err := sess.Open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Park a frame in the stream's queue as a slow transport would, then
	// abort: the buffer must be released, not leaked.
	sess.mu.Lock()
	st.out = append(st.out, uio.AcquireBuffer(16))
	sess.mu.Unlock()
	_ = st.CloseWith(errors.New("abort"))
	if st.OutboundBuffered() != 0 {
		t.Fatal("queued frames survived an abort")
	}
}

func TestObserverSeesStreamTraffic(t *testing.T) {
	var count atomic.Int64
	obs := observerFunc(func(_ uio.Conn, n int) {
		count.Add(int64(n))
	})
	sess, _ := startPair(t, Config{}, Config{Observer: obs})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	st, err := sess.Open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Write([]byte("observe-me")); err != nil {
		t.Fatal(err)
	}
	if got := readFull(t, st, len("observe-me")); string(got) != "observe-me" {
		t.Fatalf("echo = %q", got)
	}
	if count.Load() == 0 {
		t.Fatal("observer saw no bytes")
	}
}

type observerFunc func(c uio.Conn, n int)

func (f observerFunc) OnBytesRead(c uio.Conn, n int)    { f(c, n) }
func (f observerFunc) OnBytesWritten(c uio.Conn, n int) { f(c, n) }
