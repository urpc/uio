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
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/urpc/uio"
)

// readUntilEOF drains a stream until the peer's end surfaces.
func readUntilEOF(t *testing.T, st *Stream) {
	t.Helper()
	buf := make([]byte, 4<<10)
	deadline := time.Now().Add(5 * time.Second)
	for {
		n, err := st.Read(buf)
		if n > 0 {
			continue
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return
			}
			t.Fatalf("read: %v", err)
		}
		if time.Now().After(deadline) {
			t.Fatal("stream never ended")
		}
		time.Sleep(time.Millisecond)
	}
}

// waitOpenRefused waits until the session refuses new streams.
func waitOpenRefused(t *testing.T, sess *Session) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		st, err := sess.Open(ctx)
		cancel()
		if errors.Is(err, ErrGoAway) {
			return
		}
		if err == nil {
			_ = st.CloseWith(nil) // opened before the drain began
		}
		if time.Now().After(deadline) {
			t.Fatalf("Open was not refused with ErrGoAway: %v", err)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// TestShutdownDrainsExistingStreams: a graceful shutdown refuses new streams,
// keeps the open ones working, and returns once they end on their own.
func TestShutdownDrainsExistingStreams(t *testing.T) {
	sess, _ := startPair(t, Config{Handler: newEchoServer()}, Config{})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	st, err := sess.Open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n, werr := st.Write([]byte("ping")); n != 4 || werr != nil {
		t.Fatalf("write = %d, %v", n, werr)
	}
	if got := string(readFull(t, st, 4)); got != "ping" {
		t.Fatalf("echo = %q, want %q", got, "ping")
	}

	done := make(chan error, 1)
	go func() { done <- sess.Shutdown(ctx) }()
	waitOpenRefused(t, sess)

	// The open stream is untouched by the drain.
	if n, werr := st.Write([]byte("pong")); n != 4 || werr != nil {
		t.Fatalf("write during drain = %d, %v", n, werr)
	}
	if got := string(readFull(t, st, 4)); got != "pong" {
		t.Fatalf("echo during drain = %q, want %q", got, "pong")
	}

	// Ending the streams lets the drain finish.
	if err := st.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	readUntilEOF(t, st)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Shutdown = %v, want nil after every stream settled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Shutdown did not return after the last stream settled")
	}
	deadline := time.Now().Add(5 * time.Second)
	for !sess.Closed() {
		if time.Now().After(deadline) {
			t.Fatal("session stayed open after a clean drain")
		}
		time.Sleep(time.Millisecond)
	}
}

// TestShutdownTimeoutResetsStreams: a stream that never ends is reset with
// ErrShutdownTimeout when the shutdown context expires.
func TestShutdownTimeoutResetsStreams(t *testing.T) {
	h := &recHandler{}
	sess, _ := startPair(t, Config{Handler: newEchoServer()}, Config{Handler: h})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if _, err := sess.Open(ctx); err != nil {
		t.Fatal(err)
	}
	sctx, scancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer scancel()
	if err := sess.Shutdown(sctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown = %v, want the context's deadline error", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if cause := h.lastClose(); errors.Is(cause, ErrShutdownTimeout) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("stream close = %v, want ErrShutdownTimeout", h.lastClose())
		}
		time.Sleep(time.Millisecond)
	}
}

// TestPeerGoAwayRefusesOpen: the peer's GoAway ends the local Open path.
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

// drainWire records frames and can fail one write with overflow; it records a
// CloseWith instead of closing anything.
type drainWire struct {
	uio.Conn
	mu       sync.Mutex
	overflow bool
	frames   []frameHeader
	closed   int
	closeErr error
}

func (w *drainWire) Wake() error                     { return nil }
func (w *drainWire) SetWakeDeadline(time.Time) error { return nil }

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

func (w *drainWire) CloseWith(err error) error {
	w.mu.Lock()
	w.closed++
	w.closeErr = err
	w.mu.Unlock()
	return nil
}

// TestShutdownWaitsForGoAwayFlush: the transport is not closed while the
// GoAway frame is still stuck in the queue; the retry that gets it out ends
// the drain.
func TestShutdownWaitsForGoAwayFlush(t *testing.T) {
	wire := &drainWire{overflow: true}
	s := &Session{
		cfg:      Config{KeepAlive: -1}.resolve(),
		conn:     wire,
		nextID:   2,
		streams:  make(map[uint32]*Stream),
		openWait: make(map[uint32]chan error),
	}
	done := make(chan error, 1)
	go func() { done <- s.Shutdown(context.Background()) }()

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

	s.flush() // the transport accepted the retry
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

// TestShutdownForceUnderReadHold: a held session cannot parse frames, yet the
// shutdown context still ends the drain.
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

// TestShutdownRacingOpen: an Open racing a Shutdown either registers before
// the drain or fails with ErrGoAway; nothing hangs.
func TestShutdownRacingOpen(t *testing.T) {
	for round := 0; round < 5; round++ {
		sess, _ := startPair(t, Config{Handler: newEchoServer()}, Config{})
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		opened := make(chan error, 1)
		go func() {
			st, err := sess.Open(ctx)
			if err == nil {
				_ = st.CloseWith(nil)
			}
			opened <- err
		}()
		drained := make(chan error, 1)
		go func() { drained <- sess.Shutdown(ctx) }()
		select {
		case err := <-opened:
			if err != nil && !errors.Is(err, ErrGoAway) && !errors.Is(err, ErrClosed) && !errors.Is(err, net.ErrClosed) {
				t.Fatalf("round %d: Open = %v", round, err)
			}
		case <-time.After(6 * time.Second):
			t.Fatalf("round %d: Open hung against a shutdown", round)
		}
		select {
		case <-drained:
		case <-time.After(6 * time.Second):
			t.Fatalf("round %d: Shutdown hung", round)
		}
		cancel()
	}
}
