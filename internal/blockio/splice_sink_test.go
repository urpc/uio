//go:build linux || darwin || netbsd || freebsd || openbsd || dragonfly

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
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// pipeTarget is a SpliceTarget that drains a real pipe into its fake source.
type pipeTarget struct{ *fakeSource }

func (p *pipeTarget) SpliceFromPipe(pipeR, n int) (int, error) {
	buf := make([]byte, n)
	nr, err := syscall.Read(pipeR, buf)
	if nr > 0 {
		return p.Write(buf[:nr])
	}
	return 0, err
}

// errTarget is a SpliceTarget that always fails.
type errTarget struct{ *fakeSource }

func (e *errTarget) SpliceFromPipe(int, int) (int, error) { return 0, errors.New("target failed") }

// driveTransfer is a SpliceTransfer whose engine body the test supplies, to
// drive the Reader's hooks and sink along chosen paths.
type driveTransfer struct {
	*fakeSource
	drive func(req SpliceRequest) (int64, bool, error)
}

func (d *driveTransfer) TransferSplice(req SpliceRequest) (int64, bool, error) {
	return d.drive(req)
}

func newPipe(t *testing.T) (pr, pw *os.File) {
	t.Helper()
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pr.Close(); pw.Close() })
	return pr, pw
}

// The sink reports the writer's window and hands pipe bytes to the target once
// the writer's queue is empty.
func TestWriterSinkSpliceFromPipe(t *testing.T) {
	dst := newFakeSource(64<<10, 64<<10)
	dst.state = NewState(dst)
	w := dst.state.Writer(0)
	sink := &writerSink{w: w, target: &pipeTarget{dst}, ctx: context.Background()}
	if sink.Window() != w.window() {
		t.Fatalf("Window = %d, want %d", sink.Window(), w.window())
	}
	pr, pw := newPipe(t)
	_, _ = pw.Write([]byte("hello"))
	n, taken, err := sink.SpliceFromPipe(int(pr.Fd()), 5)
	if err != nil || !taken || n != 5 || dst.out.String() != "hello" {
		t.Fatalf("SpliceFromPipe = (%d, %v, %v), out %q", n, taken, err, dst.out.String())
	}
}

// A failing target is reported as taken (the bytes left the pipe) with the
// error resolved against the writer's state.
func TestWriterSinkTargetError(t *testing.T) {
	dst := newFakeSource(64<<10, 64<<10)
	dst.state = NewState(dst)
	w := dst.state.Writer(0)
	sink := &writerSink{w: w, target: &errTarget{dst}, ctx: context.Background()}
	pr, pw := newPipe(t)
	_, _ = pw.Write([]byte("x"))
	_, taken, err := sink.SpliceFromPipe(int(pr.Fd()), 1)
	if !taken || err == nil {
		t.Fatalf("SpliceFromPipe = (taken=%v, %v), want taken with an error", taken, err)
	}
}

// A cancelled context stops the sink before it takes the write semaphore.
func TestWriterSinkCancelledContext(t *testing.T) {
	dst := newFakeSource(64<<10, 64<<10)
	dst.state = NewState(dst)
	w := dst.state.Writer(0)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	sink := &writerSink{w: w, target: &pipeTarget{dst}, ctx: ctx}
	pr, pw := newPipe(t)
	_, _ = pw.Write([]byte("x"))
	_, taken, err := sink.SpliceFromPipe(int(pr.Fd()), 1)
	if taken || !errors.Is(err, context.Canceled) {
		t.Fatalf("SpliceFromPipe = (taken=%v, %v), want not-taken context.Canceled", taken, err)
	}
}

// With bytes already queued the sink flushes and waits for the queue to empty
// before handing the pipe over.
func TestWriterSinkWaitsForQueueToEmpty(t *testing.T) {
	dst := newFakeSource(64<<10, 64<<10)
	dst.state = NewState(dst)
	w := dst.state.Writer(0)
	_, _ = w.Write([]byte("queued"))
	sink := &writerSink{w: w, target: &pipeTarget{dst}, ctx: context.Background()}
	pr, pw := newPipe(t)
	_, _ = pw.Write([]byte("after"))
	done := make(chan struct{})
	go func() {
		n, taken, err := sink.SpliceFromPipe(int(pr.Fd()), 5)
		if err != nil || !taken || n != 5 {
			t.Errorf("SpliceFromPipe = (%d, %v, %v)", n, taken, err)
		}
		close(done)
	}()
	// Let the sink reach the wait on the full queue, then drain so it proceeds.
	time.Sleep(20 * time.Millisecond)
	for dst.OutboundBuffered() > 0 {
		dst.drainOut(-1)
	}
	<-done
	if dst.out.String() != "queuedafter" {
		t.Fatalf("out = %q, want queuedafter", dst.out.String())
	}
}

// trySpliceNative falls back when the source cannot transfer, the bounded count
// is too small, the destination writer is no splice target, or the destination
// is neither a writer nor a file.
func TestTrySpliceNativeFallbacks(t *testing.T) {
	newReader := func(src Source) *Reader {
		st := NewState(src)
		if fs, ok := src.(*fakeSource); ok {
			fs.state = st
		}
		r, err := st.Reader(64 << 10)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	var scratch []byte
	// No SpliceTransfer capability.
	plain := newFakeSource(64<<10, 64<<10)
	r := newReader(plain)
	if _, fb, _ := r.trySpliceNative(context.Background(), &bytes.Buffer{}, nil, &scratch, -1, 0); !fb {
		t.Fatal("a source without SpliceTransfer did not fall back")
	}
	// Bounded count below the splice minimum.
	drive := func(req SpliceRequest) (int64, bool, error) { return req.Total, false, nil }
	small := &driveTransfer{fakeSource: newFakeSource(64<<10, 64<<10), drive: drive}
	r = newReader(small)
	if _, fb, _ := r.trySpliceNative(context.Background(), &bytes.Buffer{}, nil, &scratch, 100, 0); !fb {
		t.Fatal("a small bounded count did not fall back")
	}
	// Destination writer is not a SpliceTarget.
	dstPlain := newFakeSource(64<<10, 64<<10)
	dstPlain.state = NewState(dstPlain)
	r = newReader(small)
	if _, fb, _ := r.trySpliceNative(context.Background(), dstPlain.state.Writer(0), dstPlain.state.Writer(0), &scratch, -1, 0); !fb {
		t.Fatal("a non-target destination writer did not fall back")
	}
	// Destination is neither a writer nor a file.
	r = newReader(small)
	if _, fb, _ := r.trySpliceNative(context.Background(), &bytes.Buffer{}, nil, &scratch, -1, 0); !fb {
		t.Fatal("a generic writer destination did not fall back")
	}
}

// trySpliceNative builds a file request for an *os.File destination and a sink
// request for a splice-target writer, and runs the engine with the Reader's
// hooks wired in.
func TestTrySpliceNativeBuildsRequest(t *testing.T) {
	var scratch []byte
	// File destination.
	f, err := os.Create(filepath.Join(t.TempDir(), "out"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	src := &driveTransfer{fakeSource: newFakeSource(64<<10, 64<<10), drive: func(req SpliceRequest) (int64, bool, error) {
		if req.File != f || req.Sink != nil {
			t.Errorf("file request: File=%v Sink=%v", req.File, req.Sink)
		}
		if req.Hooks.Prepare == nil || req.Hooks.Wait == nil || req.Hooks.Drain == nil || req.Hooks.Rescue == nil {
			t.Error("hooks not wired")
		}
		return req.Total, false, nil
	}}
	src.state = NewState(src)
	r, _ := src.state.Reader(64 << 10)
	if _, fb, err := r.trySpliceNative(context.Background(), f, nil, &scratch, -1, 0); fb || err != nil {
		t.Fatalf("file transfer fell back: %v %v", fb, err)
	}
	// Sink destination.
	dst := newFakeSource(64<<10, 64<<10)
	dst.state = NewState(dst)
	tgt := &pipeTarget{dst}
	dst.state.src = tgt
	src2 := &driveTransfer{fakeSource: newFakeSource(64<<10, 64<<10), drive: func(req SpliceRequest) (int64, bool, error) {
		if req.Sink == nil || req.File != nil {
			t.Errorf("sink request: Sink=%v File=%v", req.Sink, req.File)
		}
		return req.Total, false, nil
	}}
	src2.state = NewState(src2)
	r2, _ := src2.state.Reader(64 << 10)
	if _, fb, err := r2.trySpliceNative(context.Background(), tgt.state.Writer(0), tgt.state.Writer(0), &scratch, -1, 0); fb || err != nil {
		t.Fatalf("sink transfer fell back: %v %v", fb, err)
	}
}
