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

package blockio

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urpc/uio/internal/bytebuf"
	"github.com/urpc/uio/internal/readcontrol"
	"github.com/urpc/uio/internal/writecontrol"
)

type fakeSourceAccess struct{}

func (fakeSourceAccess) GetReadControl(src any) *readcontrol.Control {
	return src.(*fakeSource).readCtrl
}

func (fakeSourceAccess) GetWriteControl(src any) *writecontrol.Control {
	return src.(*fakeSource).writeCtrl
}

func (fakeSourceAccess) GetBlockState(src any) (*State, bool) {
	f := src.(*fakeSource)
	if !f.supported {
		return nil, false
	}
	return f.state, true
}

type (
	fakeReadAccess  = readcontrol.Access[fakeSourceAccess]
	fakeWriteAccess = writecontrol.Access[fakeSourceAccess]
	fakeBlockAccess = Access[fakeSourceAccess]
)

type fakeSource struct {
	fakeReadAccess
	fakeWriteAccess
	fakeBlockAccess

	supported bool
	state     *State
	readCtrl  *readcontrol.Control
	writeCtrl *writecontrol.Control

	mu          sync.Mutex
	in          []byte
	out         bytes.Buffer
	outBuffered int
	maxIn       int
	maxOut      int
	hardCap     int
	readEnded   bool
	writeEnded  bool
	closing     bool
	writeErr    error
	flushErr    error
	enableErr   error
	onConnTurn  bool
	wakes       atomic.Int32
}

func newFakeSource(maxIn, maxOut int) *fakeSource {
	f := &fakeSource{
		supported: true,
		maxIn:     maxIn,
		maxOut:    maxOut,
		writeCtrl: &writecontrol.Control{},
	}
	f.readCtrl = readcontrol.New(
		func() error {
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.closing {
				return net.ErrClosed
			}
			return nil
		},
		func() error {
			f.wakes.Add(1)
			if r := f.state.ActiveReader(); r != nil {
				go func() {
					f.readCtrl.ConsumerRuns()
					r.Pump()
				}()
			}
			return nil
		},
	)
	f.state = NewState(f)
	return f
}

func (f *fakeSource) PeekChunk() []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]byte(nil), f.in...)
}

func (f *fakeSource) Discard(n int) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if n < 0 || n > len(f.in) {
		n = len(f.in)
	}
	f.in = f.in[n:]
	return n, nil
}

func (f *fakeSource) InboundBuffered() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.in)
}

func (f *fakeSource) ReadEnded() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.readEnded && len(f.in) == 0
}

func (f *fakeSource) WriteEnded() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.writeEnded
}

func (f *fakeSource) ClosingOrClosed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closing
}

func (f *fakeSource) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.writeErr != nil {
		return 0, f.writeErr
	}
	if f.closing || f.writeEnded {
		return 0, net.ErrClosed
	}
	if f.hardCap > 0 && f.outBuffered+len(p) > f.hardCap {
		return 0, ErrOverflow
	}
	f.out.Write(p)
	f.outBuffered += len(p)
	return len(p), nil
}

func (f *fakeSource) Writev(vec [][]byte) (int, error) {
	total := 0
	for _, s := range vec {
		total += len(s)
	}
	joined := make([]byte, 0, total)
	for _, s := range vec {
		joined = append(joined, s...)
	}
	return f.Write(joined)
}

func (f *fakeSource) WriteOwned(buf *bytebuf.Buffer) (int, error) {
	f.mu.Lock()
	if f.writeErr != nil {
		err := f.writeErr
		f.mu.Unlock()
		bytebuf.ReleaseBuffer(buf)
		return 0, err
	}
	if f.closing || f.writeEnded {
		f.mu.Unlock()
		bytebuf.ReleaseBuffer(buf)
		return 0, net.ErrClosed
	}
	n := buf.Len()
	if f.hardCap > 0 && f.outBuffered+n > f.hardCap {
		f.mu.Unlock()
		return 0, ErrOverflow
	}
	f.out.Write(buf.Bytes())
	f.outBuffered += n
	f.mu.Unlock()
	bytebuf.ReleaseBuffer(buf)
	return n, nil
}

func (f *fakeSource) Flush() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.flushErr
}

func (f *fakeSource) OutboundBuffered() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.outBuffered
}

func (f *fakeSource) drainOut(n int) {
	f.mu.Lock()
	if n < 0 || n > f.outBuffered {
		n = f.outBuffered
	}
	f.outBuffered -= n
	f.mu.Unlock()
	f.writeCtrl.Notify(n)
}

func (f *fakeSource) pushIn(data []byte, eof bool) {
	f.mu.Lock()
	f.in = append(f.in, data...)
	if eof {
		f.readEnded = true
	}
	paused := f.readCtrl.Held()
	f.mu.Unlock()
	if !paused {
		f.readCtrl.ConsumerRuns()
		if r := f.state.ActiveReader(); r != nil {
			r.Pump()
		}
	}
}

func (f *fakeSource) MaxInbound() int  { return f.maxIn }
func (f *fakeSource) MaxOutbound() int { return f.maxOut }
func (f *fakeSource) EnablePullRead() (bool, error) {
	if f.enableErr != nil {
		return false, f.enableErr
	}
	return f.onConnTurn, nil
}
func (f *fakeSource) Wake() error                      { f.wakes.Add(1); return nil }
func (f *fakeSource) NewReadPause() *readcontrol.Pause { return readcontrol.NewPause(f) }
func (f *fakeSource) WatchWrite(fn func(int)) bool     { return writecontrol.Watch(f, fn) }
func (f *fakeSource) IsOverflow(err error) bool        { return errors.Is(err, ErrOverflow) }

func TestTurnDetectionAndNestedRefCount(t *testing.T) {
	if OnTurn() {
		t.Fatal("fresh test goroutine should not be on turn")
	}
	EnterTurn()
	EnterTurn()
	if !OnTurn() {
		t.Fatal("nested EnterTurn did not register goroutine")
	}
	src := newFakeSource(64, 64)
	src.onConnTurn = true
	r, _ := src.state.Reader(0)
	w := src.state.Writer(0)
	if _, err := r.Read(make([]byte, 4)); !errors.Is(err, ErrBlockingOnTurn) {
		t.Fatalf("Read on turn = %v", err)
	}
	if _, err := r.WriteTo(io.Discard); !errors.Is(err, ErrBlockingOnTurn) {
		t.Fatalf("WriteTo on turn = %v", err)
	}
	if _, err := w.Write([]byte("x")); !errors.Is(err, ErrBlockingOnTurn) {
		t.Fatalf("Write on turn = %v", err)
	}
	if _, err := w.WriteOwned(bytebuf.AcquireBuffer(4)); !errors.Is(err, ErrBlockingOnTurn) {
		t.Fatalf("WriteOwned on turn = %v", err)
	}
	if _, err := w.ReadFrom(bytes.NewReader([]byte("x"))); !errors.Is(err, ErrBlockingOnTurn) {
		t.Fatalf("ReadFrom on turn = %v", err)
	}
	if err := w.Flush(); !errors.Is(err, ErrBlockingOnTurn) {
		t.Fatalf("Flush on turn = %v", err)
	}
	LeaveTurn()
	if !OnTurn() {
		t.Fatal("first LeaveTurn cleared nested registration early")
	}
	LeaveTurn()
	if OnTurn() {
		t.Fatal("final LeaveTurn did not clear registration")
	}
}

func TestAccessResolutionAndStateIdempotence(t *testing.T) {
	if st, ok := StateOf("unsupported"); ok || st != nil {
		t.Fatal("unsupported type returned state")
	}
	var nilState *State
	if nilState.ActiveReader() != nil {
		t.Fatal("nil State returned ActiveReader")
	}
	nilState.OnClose(true, nil)

	src := newFakeSource(0, 0)
	src.supported = false
	if _, ok := StateOf(src); ok {
		t.Fatal("disabled source returned ok")
	}
	src.supported = true
	st, ok := StateOf(src)
	if !ok || st != src.state {
		t.Fatal("StateOf failed")
	}
	r1, err := st.Reader(128)
	if err != nil {
		t.Fatal(err)
	}
	r2, err := st.Reader(256)
	if err != nil || r1 != r2 {
		t.Fatal("Reader was not singleton")
	}
	w1 := st.Writer(128)
	w2 := st.Writer(256)
	if w1 != w2 {
		t.Fatal("Writer was not singleton")
	}

	failSrc := newFakeSource(0, 0)
	failSrc.enableErr = errors.New("enable failed")
	if _, err := failSrc.state.Reader(0); err == nil {
		t.Fatal("expected EnablePullRead error")
	}
}

func TestWriterBackpressureDeadlineFlushAndClose(t *testing.T) {
	src := newFakeSource(0, 64)
	src.hardCap = 64
	w := src.state.Writer(0)

	if n, err := w.Write(nil); n != 0 || err != nil {
		t.Fatalf("empty write = %d, %v", n, err)
	}
	if n, err := w.WriteOwned(nil); n != 0 || err != nil {
		t.Fatalf("nil WriteOwned = %d, %v", n, err)
	}

	// Write 200 bytes across a 64-byte window: suspends until drained.
	payload := bytes.Repeat([]byte("abcd"), 50) // 200 bytes
	done := make(chan error, 1)
	go func() {
		n, err := w.Write(payload)
		if err == nil && n != len(payload) {
			err = io.ErrShortWrite
		}
		done <- err
	}()

	time.Sleep(15 * time.Millisecond)
	if w.Buffered() > 64 {
		t.Fatalf("Buffered = %d, exceeded 64-byte window", w.Buffered())
	}
	for range 10 {
		src.drainOut(64)
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
			goto writeDone
		case <-time.After(5 * time.Millisecond):
		}
	}
	t.Fatal("Write did not finish after draining")
writeDone:
	if !bytes.Equal(src.out.Bytes(), payload) {
		t.Fatal("written payload mismatch")
	}

	// Flush waits until OutboundBuffered == 0.
	_, _ = w.Write([]byte("flush-me"))
	flushDone := make(chan error, 1)
	go func() { flushDone <- w.Flush() }()
	time.Sleep(10 * time.Millisecond)
	src.drainOut(64)
	if err := <-flushDone; err != nil {
		t.Fatalf("Flush = %v", err)
	}

	// Deadline interrupts blocked write without closing connection, then clearing recovers!
	_, _ = w.Write(make([]byte, 64)) // fill window
	_ = w.SetWriteDeadline(time.Now().Add(20 * time.Millisecond))
	if n, err := w.Write([]byte("blocked")); !errors.Is(err, os.ErrDeadlineExceeded) || n != 0 {
		t.Fatalf("deadline Write = %d, %v", n, err)
	}
	// Past deadline fails immediately.
	_ = w.SetWriteDeadline(time.Now().Add(-time.Second))
	if _, err := w.Write([]byte("x")); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("past deadline = %v", err)
	}
	// Clear deadline and drain -> connection still works!
	_ = w.SetWriteDeadline(time.Time{})
	src.drainOut(64)
	if n, err := w.Write([]byte("alive")); err != nil || n != 5 {
		t.Fatalf("post-deadline Write = %d, %v", n, err)
	}
	src.drainOut(64)

	// ReadFrom copies using pooled WriteOwned buffers.
	fromData := bytes.Repeat([]byte("R"), 150)
	stopDrain1 := make(chan struct{})
	drainDone1 := make(chan struct{})
	go func() {
		defer close(drainDone1)
		for {
			select {
			case <-stopDrain1:
				return
			case <-time.After(time.Millisecond):
				src.drainOut(64)
			}
		}
	}()
	if copied, err := w.ReadFrom(bytes.NewReader(fromData)); err != nil || copied != int64(len(fromData)) {
		t.Fatalf("ReadFrom = %d, %v", copied, err)
	}
	close(stopDrain1)
	<-drainDone1

	// Oversized WriteOwned (> window) falls back to chunked write cleanly.
	bigOwned := bytebuf.AcquireBuffer(128)
	_, _ = bigOwned.Write(bytes.Repeat([]byte("B"), 100))
	stopDrain2 := make(chan struct{})
	drainDone2 := make(chan struct{})
	go func() {
		defer close(drainDone2)
		for {
			select {
			case <-stopDrain2:
				return
			case <-time.After(time.Millisecond):
				src.drainOut(64)
			}
		}
	}()
	if n, err := w.WriteOwned(bigOwned); err != nil || n != 100 {
		t.Fatalf("oversized WriteOwned = %d, %v", n, err)
	}
	close(stopDrain2)
	<-drainDone2

	// Close wakes parked writer with net.ErrClosed.
	src.drainOut(64)
	_, _ = w.Write(make([]byte, 64))
	closeWait := make(chan error, 1)
	go func() {
		_, err := w.Write([]byte("after"))
		closeWait <- err
	}()
	time.Sleep(10 * time.Millisecond)
	src.state.OnClose(true, nil)
	if err := <-closeWait; !errors.Is(err, net.ErrClosed) {
		t.Fatalf("closed Write = %v, want net.ErrClosed", err)
	}
}

func TestReaderDirectDeliveryBackpressureDeadlineAndCloseModes(t *testing.T) {
	src := newFakeSource(64, 64)
	r, err := src.state.Reader(0)
	if err != nil {
		t.Fatal(err)
	}

	// 1. Direct handoff when reader parks first!
	readRes := make(chan string, 1)
	go func() {
		buf := make([]byte, 16)
		n, err := r.Read(buf)
		if err != nil {
			t.Errorf("direct Read err = %v", err)
		}
		readRes <- string(buf[:n])
	}()
	time.Sleep(10 * time.Millisecond)
	src.pushIn([]byte("direct-hit"), false)
	if got := <-readRes; got != "direct-hit" {
		t.Fatalf("direct Read = %q, want direct-hit", got)
	}

	// 2. Pipe backpressure pauses source at >= 64B and resumes at <= 32B!
	src.pushIn(bytes.Repeat([]byte("p"), 100), false)
	if r.Buffered() != 64 || !src.readCtrl.Held() {
		t.Fatalf("Buffered = %d, Held = %v, want 64 and true", r.Buffered(), src.readCtrl.Held())
	}
	// Reading 20 bytes leaves 44 (> 32) -> still paused.
	tmp := make([]byte, 20)
	if n, err := r.Read(tmp); err != nil || n != 20 || !src.readCtrl.Held() {
		t.Fatalf("partial drain Read = %d, %v, Held = %v", n, err, src.readCtrl.Held())
	}
	// Reading another 20 bytes leaves 24 (<= 32) -> resumes pump and drains remaining 36 bytes!
	if n, err := r.Read(tmp); err != nil || n != 20 {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(time.Second); src.InboundBuffered() > 0 && time.Now().Before(deadline); {
		time.Sleep(time.Millisecond)
	}
	rest := make([]byte, 60)
	n1, _ := r.Read(rest)
	if n1 != 60 {
		n2, _ := r.Read(rest[n1:])
		n1 += n2
	}
	if n1 != 60 {
		t.Fatalf("remaining bytes = %d, want 60", n1)
	}

	// 3. Deadline interrupts Read without closing or losing buffered data!
	_ = r.SetReadDeadline(time.Now().Add(15 * time.Millisecond))
	if _, err := r.Read(make([]byte, 8)); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("deadline Read = %v", err)
	}
	_ = r.SetReadDeadline(time.Time{})
	if n, err := r.Read(nil); n != 0 || err != nil {
		t.Fatalf("zero-len Read = %d, %v", n, err)
	}

	// 4. Peer EOF drains pipe first, then returns io.EOF!
	src.pushIn([]byte("tail"), true)
	gotTail, err := io.ReadAll(r)
	if err != nil || string(gotTail) != "tail" {
		t.Fatalf("ReadAll at EOF = %q, %v", gotTail, err)
	}

	// 5. Local Close() drops unread pipe and returns net.ErrClosed immediately!
	src2 := newFakeSource(64, 64)
	r2, _ := src2.state.Reader(0)
	src2.pushIn([]byte("unread"), false)
	src2.state.OnClose(true, nil)
	if _, err := r2.Read(make([]byte, 8)); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("local Close Read = %v, want net.ErrClosed", err)
	}

	// 6. Remote reset error drains buffered bytes first, then returns reset error!
	resetErr := errors.New("connection reset by peer")
	src3 := newFakeSource(64, 64)
	r3, _ := src3.state.Reader(0)
	src3.pushIn([]byte("before-rst"), false)
	src3.state.OnClose(false, resetErr)
	buf3 := make([]byte, 16)
	n3, err3 := r3.Read(buf3)
	if err3 != nil || string(buf3[:n3]) != "before-rst" {
		t.Fatalf("pre-rst Read = %q, %v", buf3[:n3], err3)
	}
	if _, err3 = r3.Read(buf3); !errors.Is(err3, resetErr) {
		t.Fatalf("post-rst Read = %v, want %v", err3, resetErr)
	}
}

func TestZeroCopyWriteToBetweenReaderAndWriter(t *testing.T) {
	srcIn := newFakeSource(128, 128)
	srcOut := newFakeSource(128, 64)
	srcOut.hardCap = 64

	r, _ := srcIn.state.Reader(0)
	w := srcOut.state.Writer(0)

	payload := bytes.Repeat([]byte("zero-copy-relay-"), 32) // 512 bytes
	go func() {
		for off := 0; off < len(payload); off += 64 {
			srcIn.pushIn(payload[off:off+64], off+64 == len(payload))
			time.Sleep(2 * time.Millisecond)
		}
	}()
	go func() {
		for range 30 {
			time.Sleep(3 * time.Millisecond)
			srcOut.drainOut(64)
		}
	}()

	copied, err := io.Copy(w, r)
	if err != nil || copied != int64(len(payload)) {
		t.Fatalf("io.Copy(w, r) = %d, %v", copied, err)
	}
	if !bytes.Equal(srcOut.out.Bytes(), payload) {
		t.Fatal("zero-copy relayed payload mismatch")
	}

	// Also test WriteTo plain io.Writer and WriteTo preserving block on dst deadline!
	srcIn2 := newFakeSource(128, 128)
	rPlain, _ := srcIn2.state.Reader(0)
	srcIn2.pushIn([]byte("plain-writer"), true)
	var plainOut bytes.Buffer
	if n, err := rPlain.WriteTo(&plainOut); err != nil || n != 12 || plainOut.String() != "plain-writer" {
		t.Fatalf("plain WriteTo = %d, %v, %q", n, err, plainOut.String())
	}

	// WriteTo onto a deadline-expired *Writer prepends unconsumed block back into Reader!
	srcIn3 := newFakeSource(128, 128)
	srcOut3 := newFakeSource(128, 16)
	srcOut3.hardCap = 16
	r3, _ := srcIn3.state.Reader(0)
	w3 := srcOut3.state.Writer(0)
	_, _ = w3.Write(make([]byte, 16)) // fill w3
	_ = w3.SetWriteDeadline(time.Now().Add(-time.Millisecond))
	srcIn3.pushIn([]byte("keep-me"), true)
	if _, err := r3.WriteTo(w3); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("expired dst WriteTo = %v", err)
	}
	// Clear deadline & drain w3 -> r3 still has "keep-me"!
	_ = w3.SetWriteDeadline(time.Time{})
	srcOut3.drainOut(16)
	if n, err := r3.WriteTo(w3); err != nil || n != 7 || srcOut3.out.String()[16:] != "keep-me" {
		t.Fatalf("recovered WriteTo = %d, %v, %q", n, err, srcOut3.out.String()[16:])
	}
}

type writerWrapper struct{ w *Writer }

func (ww writerWrapper) Write(p []byte) (int, error) { return ww.w.Write(p) }
func (ww writerWrapper) InternalWriter() *Writer     { return ww.w }

type shortWriter struct{}

func (shortWriter) Write(p []byte) (int, error) { return len(p) / 2, nil }

type errorReader struct{ err error }

func (e errorReader) Read([]byte) (int, error) { return 0, e.err }

type errorWriter struct{ err error }

func (e errorWriter) Write([]byte) (int, error) { return 0, e.err }

func TestEdgeBranchesPreClosedReadFromChunkLoopAndShortWrite(t *testing.T) {
	// 1. Pre-closed State creating Reader and Writer + duplicate OnClose.
	preSrc := newFakeSource(0, 0)
	boom := errors.New("boom")
	preSrc.state.OnClose(false, boom)
	preSrc.state.OnClose(true, nil) // idempotent
	preSrc.onConnTurn = true
	pr, _ := preSrc.state.Reader(512)
	pw := preSrc.state.Writer(512)
	if _, err := pr.Read(make([]byte, 4)); !errors.Is(err, boom) {
		t.Fatalf("pre-closed Read = %v", err)
	}
	if _, err := pw.Write([]byte("x")); !errors.Is(err, boom) {
		t.Fatalf("pre-closed Write = %v", err)
	}

	// 2. ReadFrom non-WriterTo io.Reader + ReadFrom error & Flush error/deadline/close.
	src := newFakeSource(0, 0) // DefaultWindow path
	wDefault := src.state.Writer(0)
	if wDefault.window() != DefaultWindow {
		t.Fatalf("default window = %d", wDefault.window())
	}
	w := src.state.Writer(32)
	w.setWindow(128) // exceeds MaxOutbound(0) -> uses 128
	data := bytes.Repeat([]byte("chunk-"), 20)
	onlyReader := struct{ io.Reader }{bytes.NewReader(data)}
	if n, err := w.ReadFrom(onlyReader); err != nil || n != int64(len(data)) {
		t.Fatalf("onlyReader ReadFrom = %d, %v", n, err)
	}
	errReader := io.MultiReader(bytes.NewReader([]byte("part")), errorReader{err: boom})
	if n, err := w.ReadFrom(struct{ io.Reader }{errReader}); !errors.Is(err, boom) || n != 4 {
		t.Fatalf("error ReadFrom = %d, %v", n, err)
	}
	_ = w.SetWriteDeadline(time.Now().Add(-time.Millisecond))
	if _, err := w.ReadFrom(struct{ io.Reader }{bytes.NewReader([]byte("x"))}); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("deadline ReadFrom = %v", err)
	}
	if err := w.Flush(); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("deadline Flush = %v", err)
	}
	_ = w.SetWriteDeadline(time.Time{})

	src.flushErr = boom
	if err := w.Flush(); !errors.Is(err, boom) {
		t.Fatalf("Flush error = %v", err)
	}
	src.flushErr = nil
	src.writeErr = boom
	if _, err := w.Write([]byte("err")); !errors.Is(err, boom) {
		t.Fatalf("Write error = %v", err)
	}
	owned := bytebuf.AcquireBuffer(8)
	_, _ = owned.WriteString("err")
	if _, err := w.WriteOwned(owned); !errors.Is(err, boom) {
		t.Fatalf("WriteOwned error = %v", err)
	}
	src.writeErr = nil
	src.writeEnded = true
	if _, err := w.Write([]byte("shut")); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("WriteEnded Write = %v", err)
	}
	if _, err := w.ReadFrom(struct{ io.Reader }{bytes.NewReader([]byte("x"))}); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("closed ReadFrom = %v", err)
	}
	if err := w.Flush(); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("closed Flush = %v", err)
	}

	// 3. Reader setWindow resuming paused pipe, zero-len Read after local close,
	// Pump after local close, and WriteTo wrapper / short writer / wait loop.
	rSrc := newFakeSource(0, 0) // DefaultWindow path
	r, _ := rSrc.state.Reader(16)
	rSrc.pushIn(bytes.Repeat([]byte("z"), 20), false)
	r.setWindow(64) // pipe len 16 <= 32 -> resumes transport!
	for deadline := time.Now().Add(time.Second); rSrc.InboundBuffered() > 0 && time.Now().Before(deadline); {
		time.Sleep(time.Millisecond)
	}
	if _, err := r.WriteTo(shortWriter{}); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("short WriteTo = %v", err)
	}

	// WriteTo waiting on empty pipe, then receiving data + EOF into InternalWriter wrapper!
	rSrc2 := newFakeSource(64, 64)
	dSrc2 := newFakeSource(64, 64)
	rWait, _ := rSrc2.state.Reader(0)
	wWrap := dSrc2.state.Writer(128) // cw (128) > maxOut (64) -> clamped to 64
	go func() {
		time.Sleep(10 * time.Millisecond)
		rSrc2.pushIn([]byte("wrapped"), true)
	}()
	if n, err := rWait.WriteTo(writerWrapper{w: wWrap}); err != nil || n != 7 || dSrc2.out.String() != "wrapped" {
		t.Fatalf("wrapped WriteTo = %d, %v, %q", n, err, dSrc2.out.String())
	}

	// Local close while parked in Read + Pump discarding after local close.
	rSrc3 := newFakeSource(64, 64)
	rClose, _ := rSrc3.state.Reader(0)
	parkedErr := make(chan error, 1)
	go func() {
		_, err := rClose.Read(make([]byte, 8))
		parkedErr <- err
	}()
	time.Sleep(10 * time.Millisecond)
	rSrc3.state.OnClose(true, nil)
	if err := <-parkedErr; !errors.Is(err, net.ErrClosed) {
		t.Fatalf("parked local Close Read = %v", err)
	}
	if _, err := rClose.Read(nil); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("zero-len closed Read = %v", err)
	}
	rSrc3.mu.Lock()
	rSrc3.in = []byte("discarded")
	rSrc3.mu.Unlock()
	rClose.Pump()
	if rSrc3.InboundBuffered() != 0 {
		t.Fatal("Pump after local close did not discard")
	}

	// WriteTo deadline, local close, and remote error paths.
	if _, err := rClose.WriteTo(io.Discard); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("local-closed WriteTo = %v", err)
	}
	rSrc4 := newFakeSource(0, 0)
	r4, _ := rSrc4.state.Reader(0) // DefaultWindow
	if r4.window() != DefaultWindow {
		t.Fatalf("default reader window = %d", r4.window())
	}
	_ = r4.SetReadDeadline(time.Now().Add(15 * time.Millisecond))
	if _, err := r4.WriteTo(io.Discard); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("deadline WriteTo = %v", err)
	}
	_ = r4.SetReadDeadline(time.Now().Add(-time.Millisecond))
	if _, err := r4.WriteTo(io.Discard); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("past-deadline WriteTo = %v", err)
	}
	_ = r4.SetReadDeadline(time.Time{})
	rSrc4.state.OnClose(false, boom)
	if _, err := r4.WriteTo(io.Discard); !errors.Is(err, boom) {
		t.Fatalf("remote-error WriteTo = %v", err)
	}

	// Pump when already full + onClose(false, io.EOF) while paused + WriteTo errorWriter.
	rSrc5 := newFakeSource(16, 16)
	r5, _ := rSrc5.state.Reader(16)
	rSrc5.pushIn(bytes.Repeat([]byte("a"), 24), false)
	r5.Pump() // already at limit -> cur >= limit branch
	if _, err := r5.WriteTo(errorWriter{err: boom}); !errors.Is(err, boom) {
		t.Fatalf("errorWriter WriteTo = %v", err)
	}
	for deadline := time.Now().Add(time.Second); rSrc5.InboundBuffered() > 0 && time.Now().Before(deadline); {
		time.Sleep(time.Millisecond)
	}
	rSrc5.pushIn(bytes.Repeat([]byte("b"), 24), false)
	rSrc5.state.OnClose(false, io.EOF) // resumes paused pause & sets peerEOF!
	all5, err5 := io.ReadAll(r5)
	if err5 != nil || len(all5) == 0 {
		t.Fatalf("EOF after pause ReadAll = %d, %v", len(all5), err5)
	}
}

type partialVecSource struct {
	*fakeSource
	partial int
}

func (p *partialVecSource) Writev(vec [][]byte) (int, error) {
	if p.partial > 0 {
		n := p.partial
		p.partial = 0
		total := 0
		for _, s := range vec {
			total += len(s)
		}
		joined := make([]byte, 0, total)
		for _, s := range vec {
			joined = append(joined, s...)
		}
		_, _ = p.fakeSource.Write(joined[:n])
		return n, ErrOverflow
	}
	return p.fakeSource.Writev(vec)
}

func TestWriterWritevAndFairness(t *testing.T) {
	base := newFakeSource(64, 32)
	base.hardCap = 32
	ps := &partialVecSource{fakeSource: base}
	st := NewState(ps)
	w := st.Writer(32)

	// On-turn Writev returns ErrBlockingOnTurn.
	EnterTurn()
	if _, err := w.Writev([][]byte{[]byte("x")}); !errors.Is(err, ErrBlockingOnTurn) {
		t.Fatalf("on-turn Writev = %v", err)
	}
	LeaveTurn()

	// Empty Writev
	if n, err := w.Writev(nil); n != 0 || err != nil {
		t.Fatalf("empty Writev = %d, %v", n, err)
	}

	// Fast-path Writev within window
	if n, err := w.Writev([][]byte{[]byte("fast-"), []byte("vec")}); err != nil || n != 8 {
		t.Fatalf("fast Writev = %d, %v", n, err)
	}
	base.drainOut(32)

	// Partial Writev continuation (like umux partial window accept, mid-segment split)!
	ps.partial = 3
	done := make(chan error, 1)
	go func() {
		n, err := w.Writev([][]byte{[]byte("ab"), []byte("cde"), []byte("fghij")})
		if err == nil && n != 10 {
			err = io.ErrShortWrite
		}
		done <- err
	}()
	base.drainOut(32)
	if err := <-done; err != nil {
		t.Fatalf("partial Writev = %v", err)
	}

	// Oversized multi-chunk Writev (> window)!
	base.drainOut(32)
	_, _ = w.Write(make([]byte, 32)) // fill window
	bigDone := make(chan error, 1)
	go func() {
		n, err := w.Writev([][]byte{nil, bytes.Repeat([]byte("V"), 40), bytes.Repeat([]byte("W"), 30)})
		if err == nil && n != 70 {
			err = io.ErrShortWrite
		}
		bigDone <- err
	}()
	time.Sleep(5 * time.Millisecond)
	for range 10 {
		base.drainOut(32)
		select {
		case err := <-bigDone:
			if err != nil {
				t.Fatalf("chunked Writev = %v", err)
			}
			goto vecDone
		case <-time.After(3 * time.Millisecond):
		}
	}
	t.Fatal("chunked Writev did not finish")
vecDone:

	// Deadline & error & closed Writev guards
	_ = w.SetWriteDeadline(time.Now().Add(-time.Millisecond))
	if _, err := w.Writev([][]byte{[]byte("x")}); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("deadline Writev = %v", err)
	}
	_ = w.SetWriteDeadline(time.Time{})
	boom := errors.New("vec-boom")
	base.writeErr = boom
	if _, err := w.Writev([][]byte{[]byte("x")}); !errors.Is(err, boom) {
		t.Fatalf("error Writev = %v", err)
	}
	base.writeErr = nil
	st.OnClose(true, nil)
	if _, err := w.Writev([][]byte{[]byte("x")}); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("closed Writev = %v", err)
	}

	// Chunk halving when lower hardCap < w.window() on empty queue (both Write & WriteOwned)!
	capSrc := newFakeSource(64, 128)
	capSrc.hardCap = 16 // smaller than window 128!
	capWriter := capSrc.state.Writer(128)
	go func() {
		for range 20 {
			time.Sleep(time.Millisecond)
			capSrc.drainOut(16)
		}
	}()
	if n, err := capWriter.Write(bytes.Repeat([]byte("h"), 40)); err != nil || n != 40 {
		t.Fatalf("halved Write = %d, %v", n, err)
	}
	ownedBig := bytebuf.AcquireBuffer(40)
	_, _ = ownedBig.Write(bytes.Repeat([]byte("o"), 40))
	if n, err := capWriter.WriteOwned(ownedBig); err != nil || n != 40 {
		t.Fatalf("halved WriteOwned = %d, %v", n, err)
	}

	// Reader attached to an already-closing source immediately observes close without hanging!
	deadSrc := newFakeSource(64, 64)
	deadSrc.closing = true
	deadR, err := deadSrc.state.Reader(64)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := deadR.Read(make([]byte, 8)); !errors.Is(err, io.EOF) {
		t.Fatalf("already-closing source Read = %v, want io.EOF", err)
	}

	// Verify WriteContext, WritevContext, WriteOwnedContext, and FlushContext per-call cancellation!
	ctxSrc := newFakeSource(16, 16)
	ctxW := ctxSrc.state.Writer(16)
	if n, err := ctxW.WriteContext(context.Background(), make([]byte, 16)); err != nil || n != 16 {
		t.Fatalf("fill WriteContext = %d, %v", n, err)
	}
	// Park G1 in WriteContext while G2 cancels via its own ctx while waiting on wsem!
	g1Ctx, g1Cancel := context.WithCancel(context.Background())
	g1Err := make(chan error, 1)
	go func() {
		_, err := ctxW.WriteContext(g1Ctx, make([]byte, 8))
		g1Err <- err
	}()
	time.Sleep(5 * time.Millisecond)
	g2Ctx, g2Cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer g2Cancel()
	if _, err := ctxW.WriteOwnedContext(g2Ctx, func() *bytebuf.Buffer { b := bytebuf.AcquireBuffer(4); _, _ = b.WriteString("ab"); return b }()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("contended WriteOwnedContext = %v, want DeadlineExceeded", err)
	}
	g1Cancel()
	if err := <-g1Err; !errors.Is(err, context.Canceled) {
		t.Fatalf("parked WriteContext cancel = %v, want Canceled", err)
	}
	fCtx, fCancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer fCancel()
	if err := ctxW.FlushContext(fCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("FlushContext timeout = %v, want DeadlineExceeded", err)
	}
	// Parked WriteOwnedContext canceled via ctx while waiting on full queue, plus background wsem wait!
	bgDone := make(chan error, 1)
	pOwnedCtx, pOwnedCancel := context.WithCancel(context.Background())
	go func() {
		b := bytebuf.AcquireBuffer(4)
		_, _ = b.WriteString("abcd")
		_, err := ctxW.WriteOwnedContext(pOwnedCtx, b)
		bgDone <- err
	}()
	time.Sleep(5 * time.Millisecond)
	bgWait := make(chan error, 1)
	go func() {
		_, err := ctxW.Write([]byte("xy")) // waits on wsem with background ctx (ctx.Done() == nil)!
		bgWait <- err
	}()
	time.Sleep(5 * time.Millisecond)
	pOwnedCancel()
	if err := <-bgDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("parked WriteOwnedContext = %v", err)
	}
	ctxSrc.drainOut(16)
	if err := <-bgWait; err != nil {
		t.Fatalf("background Write after drain = %v", err)
	}
	vCtx, vCancel := context.WithCancel(context.Background())
	vCancel()
	if _, err := ctxW.WritevContext(vCtx, [][]byte{[]byte("a")}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled WritevContext = %v", err)
	}
	if _, err := ctxW.WriteContext(vCtx, []byte("a")); !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-canceled WriteContext = %v", err)
	}
	if err := ctxW.FlushContext(vCtx); !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-canceled FlushContext = %v", err)
	}
	// Oversized WriteOwnedContext (> window 16) that transfers first 16B chunk and cancels on 2nd chunk!
	ctxSrc.drainOut(16)
	overCtx, overCancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer overCancel()
	overBuf := bytebuf.AcquireBuffer(32)
	_, _ = overBuf.Write(bytes.Repeat([]byte("z"), 32))
	if nw, err := ctxW.WriteOwnedContext(overCtx, overBuf); nw != 16 || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("oversized WriteOwnedContext = %d, %v", nw, err)
	}
}
