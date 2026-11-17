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
	"context"
	"errors"
	"io"
	"os"
	"testing"
	"time"

	"github.com/urpc/uio/internal/bytebuf"
)

// A write blocked on a full window returns promptly when its context is
// cancelled, rather than waiting for space that may never come.
func TestWriteBlockedReturnsOnContextCancel(t *testing.T) {
	fs := newFakeSource(64<<10, 64<<10)
	fs.state = NewState(fs)
	w := fs.state.Writer(0)
	fs.mu.Lock()
	fs.outBuffered = fs.maxOut // window full: the next write must wait
	fs.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := w.WriteContext(ctx, []byte("blocked"))
		done <- err
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("blocked Write = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a cancelled blocked Write did not return")
	}
}

// A write blocked on a full window returns when its write deadline expires.
func TestWriteBlockedReturnsOnDeadline(t *testing.T) {
	fs := newFakeSource(64<<10, 64<<10)
	fs.state = NewState(fs)
	w := fs.state.Writer(0)
	fs.mu.Lock()
	fs.outBuffered = fs.maxOut
	fs.mu.Unlock()
	_ = w.SetWriteDeadline(time.Now().Add(20 * time.Millisecond))
	start := time.Now()
	_, err := w.Write([]byte("blocked"))
	if !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("blocked Write = %v, want a deadline error", err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("the deadline did not bound the blocked Write")
	}
}

// TryWriteOwned on a full window reports overflow and leaves the buffer with
// the caller, so it can fall back to a blocking write.
func TestTryWriteOwnedOverflowKeepsBuffer(t *testing.T) {
	fs := newFakeSource(64<<10, 64<<10)
	fs.state = NewState(fs)
	w := fs.state.Writer(0)
	fs.mu.Lock()
	fs.outBuffered = fs.maxOut
	fs.hardCap = fs.maxOut // the lower layer refuses anything past the window
	fs.mu.Unlock()
	buf := bytebuf.AcquireBuffer(4)
	_, _ = buf.WriteString("x")
	n, err := w.TryWriteOwned(buf)
	if !errors.Is(err, ErrOverflow) || n != 0 {
		t.Fatalf("TryWriteOwned = (%d, %v), want (0, ErrOverflow)", n, err)
	}
	if buf.Len() != 1 {
		t.Fatalf("overflow consumed the buffer: %d bytes left", buf.Len())
	}
	bytebuf.ReleaseBuffer(buf)
}

// errReader fails after delivering a prefix, as a source that errors mid-stream.
type errReader struct {
	data []byte
	err  error
}

func (r *errReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, r.err
	}
	n := copy(p, r.data)
	r.data = r.data[n:]
	return n, nil
}

// ReadFrom delivers what a reader gives before it fails, then reports the
// reader's error.
func TestReadFromReportsReaderError(t *testing.T) {
	fs := newFakeSource(64<<10, 64<<10)
	fs.state = NewState(fs)
	w := fs.state.Writer(0)
	sentinel := errors.New("reader failed")
	n, err := w.ReadFrom(&errReader{data: []byte("prefix"), err: sentinel})
	if !errors.Is(err, sentinel) {
		t.Fatalf("ReadFrom = %v, want the reader's error", err)
	}
	if n != 6 || fs.out.String() != "prefix" {
		t.Fatalf("ReadFrom delivered (%d, %q), want the prefix before the error", n, fs.out.String())
	}
}

// A reader that ends cleanly is read to EOF with no error.
func TestReadFromReadsToEOF(t *testing.T) {
	fs := newFakeSource(64<<10, 64<<10)
	fs.state = NewState(fs)
	w := fs.state.Writer(0)
	n, err := w.ReadFrom(&errReader{data: []byte("all"), err: io.EOF})
	if err != nil || n != 3 || fs.out.String() != "all" {
		t.Fatalf("ReadFrom = (%d, %v, %q), want (3, nil, all)", n, err, fs.out.String())
	}
}

// SendFile on a source without sendfile(2) reads a pipe (whose length fstat
// cannot tell) to its end.
func TestSendFileNonNativeReadsPipeToEnd(t *testing.T) {
	fs := newFakeSource(64<<10, 64<<10)
	fs.state = NewState(fs)
	w := fs.state.Writer(0)
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer pr.Close()
	go func() { _, _ = pw.Write([]byte("pipe-data")); _ = pw.Close() }()
	n, err := w.SendFile(pr, 0, -1)
	if err != nil || n != 9 || fs.out.String() != "pipe-data" {
		t.Fatalf("SendFile(pipe, -1) = (%d, %v, %q), want the pipe's contents", n, err, fs.out.String())
	}
}
