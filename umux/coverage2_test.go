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
	"testing"
	"time"

	"github.com/urpc/uio"
)

// frameBuffer builds an owned data frame for white-box queue tests.
func frameBuffer(n int) *uio.Buffer {
	b := uio.AcquireBuffer(headerSize + n)
	var hb [headerSize]byte
	encodeHeader(hb[:], frameHeader{typ: typeData, streamID: 1, length: uint32(n)})
	_, _ = b.Write(hb[:])
	_, _ = b.Write(make([]byte, n))
	return b
}

// TestFlushOverflowAndErrorPaths drives flushLocked's overflow requeue and its
// transport-error teardown through a session whose transport is a stream.
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
	// Drain the echoed first frame whole: the consumption grants enough
	// credit for the second one.
	_ = readFull(t, stX, headerSize+(150<<10))
	waitFor(t, "requeued frame flushed", func() bool {
		fs.flush()
		fs.mu.Lock()
		left := len(st.out)
		fs.mu.Unlock()
		return left == 0
	})
	_ = readFull(t, stX, headerSize+(150<<10))

	// A closed transport fails the flush; tearing the session down is the
	// transport close path's job, not the write error's: marking the session
	// closed here would make that path skip the streams and the Open waiters.
	stX.CloseWith(errors.New("gone"))
	fs.mu.Lock()
	fs.sendControlLocked(typePing, flagSYN, 1, 0)
	ferr := fs.flushLocked()
	closed := fs.closed
	fs.mu.Unlock()
	if ferr == nil || closed {
		t.Fatalf("flush after transport close = %v, closed=%v, want an error and no self-close", ferr, closed)
	}
	fs.flush() // the wrapper's CloseWith branch

	// Open on a closed transport fails through its first flush.
	fs2 := newSession(stX, Config{}.resolve(), true)
	if _, err := fs2.Open(ctx); err == nil {
		t.Fatal("Open on a closed transport succeeded")
	}
}

// TestOpenContextCanceled cancels an Open whose ACK never arrives.
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

type errHandler struct{}

func (errHandler) OnOpen(uio.Conn)         {}
func (errHandler) OnRead(uio.Conn) error   { return errors.New("read failed") }
func (errHandler) OnTimer(uio.Conn) error  { return nil }
func (errHandler) OnClose(uio.Conn, error) {}

func TestDispatchErrorPaths(t *testing.T) {
	silent := &silentServer{opened: make(chan *Stream, 4)}
	sess, _ := startPair(t, Config{Handler: silent}, Config{Handler: errHandler{}})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	st, err := sess.Open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	sess.dispatchRead(st) // OnRead error aborts the stream
	if st.State()&uio.StateClosed == 0 {
		t.Fatal("dispatchRead did not abort on a handler error")
	}

	st2, err := sess.Open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	sess.mu.Lock()
	st2.mu.Lock()
	st2.wakePending = true
	sess.wakeQ = append(sess.wakeQ, st2)
	st2.mu.Unlock()
	sess.mu.Unlock()
	sess.runWakes() // the wake callback errors the same way
	if st2.State()&uio.StateClosed == 0 {
		t.Fatal("runWakes did not abort on a handler error")
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
	st.deliverClose(nil) // the second call is a no-op

	st.resetRemote(errors.New("late"))
	st.resetLocal(errors.New("later"))

	// A closed stream reports io.EOF when its cause is nil.
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

	// Late data and grants after a close are dropped.
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

func TestSessionCloseSkipsSecondGoAway(t *testing.T) {
	sess, _ := startPair(t, Config{}, Config{})
	st, err := sess.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	fs := newSession(st, Config{}.resolve(), true)
	fs.goAwaySent = true
	if err := fs.Close(nil); err != nil {
		t.Fatalf("Close: %v", err)
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

	// waitSession honors a canceled context and closes its transport.
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
		expectFrame(t, conn, func(h frameHeader) bool { return h.flags&flagACK != 0 })
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
		expectFrame(t, conn, func(h frameHeader) bool { return h.typ == typePing && h.flags&flagACK != 0 })
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
		expectFrame(t, conn, func(h frameHeader) bool { return h.flags&flagACK != 0 })
		frame := rawFramed(typeData, 0, 11, 10, []byte("0123456789"))
		if _, err := conn.Write(frame[:headerSize+4]); err != nil {
			t.Fatal(err)
		}
		time.Sleep(30 * time.Millisecond)
		if _, err := conn.Write(frame[headerSize+4:]); err != nil {
			t.Fatal(err)
		}
		echo := expectFrame(t, conn, func(h frameHeader) bool { return h.typ == typeData && h.streamID == 11 })
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
		expectFrame(t, conn, func(h frameHeader) bool { return h.flags&flagACK != 0 })
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
			// Drain any frames until the transport closes.
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
		expectFrame(t, conn, func(h frameHeader) bool { return h.flags&flagACK != 0 })
		if _, err := conn.Write(rawFramed(typeWindowUpdate, flagFIN, 15, 16, nil)); err != nil {
			t.Fatal(err)
		}
		if _, err := conn.Write(rawFramed(typePing, flagSYN, 5, 5, nil)); err != nil {
			t.Fatal(err)
		}
		expectFrame(t, conn, func(h frameHeader) bool { return h.typ == typePing && h.flags&flagACK != 0 })
	})
}
