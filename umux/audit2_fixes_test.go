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
	"sync"
	"testing"
	"time"

	"github.com/urpc/uio"
)

// drainHandler reads and discards every stream's inbound.
type drainHandler struct {
	opened chan *Stream
}

func (h *drainHandler) OnOpen(c uio.Conn) {
	select {
	case h.opened <- c.(*Stream):
	default:
	}
}

func (h *drainHandler) OnRead(c uio.Conn) error {
	st := c.(*Stream)
	buf := make([]byte, 8<<10)
	for {
		n, err := st.Read(buf)
		if n > 0 {
			continue
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		return nil
	}
}

func (h *drainHandler) OnTimer(uio.Conn) error  { return nil }
func (h *drainHandler) OnClose(uio.Conn, error) {}

// #1: a slice the transport lent must be copied before WriteTo hands it out:
// the transport reuses that memory as soon as the frame is consumed.
func TestWriteToOwnsTransportBorrow(t *testing.T) {
	s, _ := recPair(Config{})
	st := newStream(s, 1, false, int64(s.cfg.WindowSize))
	s.streams[1] = st
	transport := []byte("abcd")
	if err := st.pushData(transport, false, true); err != nil {
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
	copy(transport, "xxxx") // the transport reuses its buffer
	if got := string(borrowed); got != "abcd" {
		close(release)
		<-done
		t.Fatalf("writer saw %q after the transport reused its buffer, want %q", got, "abcd")
	}
	close(release)
	<-done
}

// #2: WriteOwned is whole or refused whole, even when writers contend for the
// last credit: a short acceptance reported as overflow would have the caller
// retry bytes the stream already consumed its buffer for.
func TestWriteOwnedWholeUnderContention(t *testing.T) {
	s, _ := recPair(Config{})
	for round := 0; round < 5000; round++ {
		start := make(chan struct{})
		st := newStream(s, 1, false, int64(s.cfg.WindowSize))
		s.mu.Lock()
		s.streams[1] = st
		s.mu.Unlock()
		st.mu.Lock()
		st.sendWin.reset(4)
		st.mu.Unlock()

		var wg sync.WaitGroup
		results := make([]struct {
			n   int
			err error
			buf *uio.Buffer
		}, 2)
		for i := range results {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				b := uio.AcquireBuffer(3)
				_, _ = b.Write([]byte("abc"))
				n, err := st.WriteOwned(b)
				results[i].n, results[i].err, results[i].buf = n, err, b
			}(i)
		}
		close(start)
		wg.Wait()
		accepted := 0
		for _, r := range results {
			if r.err == nil {
				accepted += r.n
				continue
			}
			if r.n != 0 {
				t.Fatalf("round %d: accepted %d bytes and returned %v; want the write refused whole", round, r.n, r.err)
			}
			uio.ReleaseBuffer(r.buf) // refused whole: ownership stayed here
		}
		if accepted != 3 {
			t.Fatalf("round %d: accepted %d bytes across writers, want 3", round, accepted)
		}
		s.mu.Lock()
		st.dropOutLocked()
		s.mu.Unlock()
	}
}

// #3: when the peer's send ceiling is below a quarter of this end's window,
// the grant threshold must follow the smaller window or the sender's credit
// never comes back.
func TestAsymmetricGrantStillFlows(t *testing.T) {
	drain := &drainHandler{opened: make(chan *Stream, 1)}
	sess, _ := startPair(t,
		Config{Handler: drain, WindowSize: 2 << 20},
		Config{WindowSize: 256 << 10})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	st, err := sess.Open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer st.CloseWith(nil)
	payload := make([]byte, 256<<10)
	if n, werr := st.Write(payload); n != len(payload) || werr != nil {
		t.Fatalf("first write = %d, %v; want the whole %d bytes", n, werr, len(payload))
	}
	// The receiver consumes everything; its grant must come back even though
	// the consumption is far below a quarter of its 2 MiB window.
	deadline := time.Now().Add(5 * time.Second)
	for {
		n, werr := st.Write([]byte("ping"))
		if n == 4 && werr == nil {
			return
		}
		if !errors.Is(werr, uio.ErrOutboundOverflow) {
			t.Fatalf("write = %d, %v", n, werr)
		}
		if time.Now().After(deadline) {
			t.Fatal("sender credit never returned: grant threshold follows the wrong window")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// #4: a stream removed by a timer must still be released when the same turn's
// flush fails: the transport close path cannot find it again.
func TestTimeoutTeardownDespiteTransportError(t *testing.T) {
	h := &recHandler{}
	s, wire := recPair(Config{Handler: h, StreamOpenTimeout: 10 * time.Millisecond})
	wire.failWrite = errors.New("write failed")
	opening := newStream(s, 2, true, int64(s.cfg.WindowSize))
	// A live stream carries a frame so the turn's flush really touches the
	// failing transport.
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

// #6: a held session's turn still delivers queued closes.
func TestHeldSessionDeliversCloses(t *testing.T) {
	h := &recHandler{}
	s, wire := recPair(Config{Handler: h})
	st := newStream(s, 1, false, int64(s.cfg.WindowSize))
	s.mu.Lock()
	s.streams[1] = st
	s.mu.Unlock()
	if err := st.CloseWith(errors.New("gone")); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.readHeld = true
	s.pendingBytes = int64(s.cfg.SessionWindow) + 1 // keeps the hold engaged
	s.mu.Unlock()
	_ = s.onRead(wire)
	if h.closeCount() != 1 {
		t.Fatalf("closes = %d after a held turn, want 1", h.closeCount())
	}
}

// #7: concurrent arms must not let a stale deadline land last.
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
			_ = st.SetWakeInterval(0) // cancels whatever was armed
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

// #8: a transport closed before it started reports an error instead of
// blocking Listen/Dial forever.
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

// #9: no read turn runs after a stream's close callback.
func TestNoReadAfterClose(t *testing.T) {
	h := &recHandler{}
	s, _ := recPair(Config{Handler: h})
	st := newStream(s, 1, false, int64(s.cfg.WindowSize))
	s.mu.Lock()
	s.streams[1] = st
	s.mu.Unlock()
	_ = st.Wake() // queued while open
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

// #10: a write racing an abort must not strand frames on a dropped stream.
func TestAbortRejectsLateEnqueue(t *testing.T) {
	s, _ := recPair(Config{})
	st := newStream(s, 1, false, int64(s.cfg.WindowSize))
	s.mu.Lock()
	s.streams[1] = st
	s.mu.Unlock()
	s.abortStream(st, errors.New("gone"))
	s.drainCloses()

	frame := frameBuffer(8)
	if ok := s.enqueue(st, frame); ok {
		t.Fatal("enqueue accepted a frame for a stream that left the session")
	}
	uio.ReleaseBuffer(frame)
	st.mu.Lock()
	left := len(st.out)
	st.mu.Unlock()
	if left != 0 {
		t.Fatalf("%d frames stranded on the dropped stream", left)
	}
}
