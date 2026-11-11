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
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urpc/uio/internal/filerange"
)

// fakeFileSender is a native source as FileSender describes one: SendFile
// sends up to direct bytes of a range at once and queues the rest, which
// counts toward OutboundBuffered until sendQueued sends it, and
// CancelQueuedFiles withdraws what an owner still has queued. Its out holds
// what was sent, in order.
type fakeFileSender struct {
	*fakeSource
	direct  int64
	native  bool
	sendErr error
	calls   atomic.Int32
	queue   []fakeRange // guarded by fakeSource.mu
	owners  []uint64    // the owners withdrawn, in order
}

type fakeRange struct {
	owner uint64
	data  []byte
}

func newFakeFileSender(window int, direct int64) *fakeFileSender {
	f := &fakeFileSender{fakeSource: newFakeSource(window, window), direct: direct, native: true}
	f.state = NewState(f)
	return f
}

func (f *fakeFileSender) HasNativeSendFile() bool { return f.native }

func (f *fakeFileSender) SendFile(file *os.File, r filerange.Range, owner uint64) (int64, error) {
	f.calls.Add(1)
	if f.sendErr != nil {
		return 0, f.sendErr
	}
	data := make([]byte, r.Len)
	if _, err := file.ReadAt(data, r.Start); err != nil {
		return 0, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closing || f.writeEnded {
		return 0, net.ErrClosed
	}
	if f.maxOut > 0 && f.outBuffered >= f.maxOut {
		return 0, ErrOverflow
	}
	sent := min(f.direct, r.Len)
	f.out.Write(data[:sent])
	if rest := data[sent:]; len(rest) > 0 {
		f.queue = append(f.queue, fakeRange{owner, rest})
		f.outBuffered += len(rest)
	}
	return r.Len, nil
}

func (f *fakeFileSender) CancelQueuedFiles(owner uint64) int64 {
	f.mu.Lock()
	var n int64
	kept := f.queue[:0]
	for _, q := range f.queue {
		if q.owner == owner {
			n += int64(len(q.data))
			continue
		}
		kept = append(kept, q)
	}
	f.queue = kept
	f.outBuffered -= int(n)
	f.owners = append(f.owners, owner)
	f.mu.Unlock()
	return n
}

// sendQueued sends up to n queued bytes, or all of them when n < 0, and
// reports the progress as a native connection does.
func (f *fakeFileSender) sendQueued(n int) {
	f.mu.Lock()
	sent := 0
	for len(f.queue) > 0 && (n < 0 || sent < n) {
		q := &f.queue[0]
		k := len(q.data)
		if n >= 0 {
			k = min(k, n-sent)
		}
		f.out.Write(q.data[:k])
		q.data = q.data[k:]
		sent += k
		if len(q.data) == 0 {
			f.queue = f.queue[1:]
		}
	}
	f.outBuffered -= sent
	f.mu.Unlock()
	f.writeCtrl.Notify(sent)
}

func (f *fakeFileSender) sent() []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]byte(nil), f.out.Bytes()...)
}

func testFile(t *testing.T, size int) (*os.File, []byte) {
	t.Helper()
	data := make([]byte, size)
	for i := range data {
		data[i] = byte(i*7 + i>>8)
	}
	path := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f, data
}

type sendResult struct {
	n   int64
	err error
}

func sendFileAsync(w *Writer, ctx context.Context, f *os.File, offset, count int64) <-chan sendResult {
	done := make(chan sendResult, 1)
	go func() {
		n, err := w.SendFileContext(ctx, f, offset, count)
		done <- sendResult{n, err}
	}()
	return done
}

// waitQueued waits until the source holds at least n queued bytes.
func waitQueued(t *testing.T, f *fakeFileSender, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for f.OutboundBuffered() < n {
		if time.Now().After(deadline) {
			t.Fatalf("the source holds %d queued bytes, want %d", f.OutboundBuffered(), n)
		}
		time.Sleep(time.Millisecond)
	}
}

// A native range is admitted whole, in one call, however much longer than the
// window it is; the call returns once what is left of it fits in the window.
func TestSendFileContextAdmitsANativeRangeWhole(t *testing.T) {
	f, data := testFile(t, 256<<10)
	src := newFakeFileSender(4096, 1000)
	w := src.state.Writer(0)
	done := sendFileAsync(w, context.Background(), f, 0, -1)
	waitQueued(t, src, len(data)-1000)
	select {
	case r := <-done:
		t.Fatalf("SendFileContext returned (%d, %v) with the range still queued", r.n, r.err)
	case <-time.After(20 * time.Millisecond):
	}
	src.sendQueued(len(data) - 1000 - 4096)
	r := <-done
	if r.n != int64(len(data)) || r.err != nil {
		t.Fatalf("SendFileContext = (%d, %v), want (%d, nil)", r.n, r.err, len(data))
	}
	if calls := src.calls.Load(); calls != 1 {
		t.Fatalf("the range went to the source in %d calls, want 1", calls)
	}
	src.sendQueued(-1)
	if !bytes.Equal(src.sent(), data) {
		t.Fatal("the source sent something other than the file")
	}
}

// Cut short — by ctx or by the write deadline — the call withdraws what of the
// range is still queued and reports what was sent: a prefix of the range, so
// a caller that resumes at the count sends nothing twice.
func TestSendFileContextWithdrawsWhatIsQueuedWhenCutShort(t *testing.T) {
	cuts := map[string]func(w *Writer) context.Context{
		"ctx": func(*Writer) context.Context {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
			_ = cancel
			return ctx
		},
		"deadline": func(w *Writer) context.Context {
			_ = w.SetWriteDeadline(time.Now().Add(30 * time.Millisecond))
			return context.Background()
		},
	}
	for name, cut := range cuts {
		t.Run(name, func(t *testing.T) {
			f, data := testFile(t, 128<<10)
			src := newFakeFileSender(4096, 1000)
			w := src.state.Writer(0)
			done := sendFileAsync(w, cut(w), f, 0, -1)
			waitQueued(t, src, len(data)-1000)
			src.sendQueued(5000)
			r := <-done
			if r.err == nil || r.n != 6000 {
				t.Fatalf("SendFileContext = (%d, %v), want 6000 bytes and an error", r.n, r.err)
			}
			if src.OutboundBuffered() != 0 || len(src.owners) != 1 {
				t.Fatalf("after the cut: %d bytes queued, %d withdrawals", src.OutboundBuffered(), len(src.owners))
			}
			if got := src.sent(); !bytes.Equal(got, data[:r.n]) {
				t.Fatalf("the source sent %d bytes, want the %d-byte prefix the count reports", len(got), r.n)
			}
		})
	}
}

// A call whose range has all left the queue completes even when ctx ends
// while it waits for other bytes to drain.
func TestSendFileContextWithNothingLeftToWithdrawSucceeds(t *testing.T) {
	f, data := testFile(t, 64<<10)
	src := newFakeFileSender(4096, 1000)
	w := src.state.Writer(0)
	ctx, cancel := context.WithCancel(context.Background())
	done := sendFileAsync(w, ctx, f, 0, -1)
	waitQueued(t, src, len(data)-1000)
	src.sendQueued(-1)
	src.mu.Lock()
	src.outBuffered = 8192 // another writer's bytes keep the queue above the window
	src.mu.Unlock()
	src.writeCtrl.Notify(0)
	time.Sleep(10 * time.Millisecond)
	cancel()
	if r := <-done; r.n != int64(len(data)) || r.err != nil {
		t.Fatalf("SendFileContext = (%d, %v), want (%d, nil)", r.n, r.err, len(data))
	}
}

// From the position, the call moves the position past what it sent.
func TestSendFileContextMovesThePositionPastWhatItSent(t *testing.T) {
	f, data := testFile(t, 64<<10)
	src := newFakeFileSender(4096, 1000)
	w := src.state.Writer(0)
	if _, err := f.Seek(100, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	r := <-sendFileAsync(w, ctx, f, -1, -1)
	if r.n != 1000 {
		t.Fatalf("SendFileContext = (%d, %v), want the 1000 sent bytes", r.n, r.err)
	}
	if pos, _ := f.Seek(0, io.SeekCurrent); pos != 1100 {
		t.Fatalf("position %d after sending 1000 bytes from 100", pos)
	}
	if got := src.sent(); !bytes.Equal(got, data[100:1100]) {
		t.Fatal("the source sent other bytes than those from the position")
	}
}

// Without sendfile(2) the range is read through the window.
func TestSendFileContextCopiesWithoutANativeSender(t *testing.T) {
	f, data := testFile(t, 40000)
	src := newFakeFileSender(4096, 0)
	src.native = false
	w := src.state.Writer(0)
	done := sendFileAsync(w, context.Background(), f, 0, -1)
	for {
		select {
		case r := <-done:
			if r.n != int64(len(data)) || r.err != nil || src.calls.Load() != 0 {
				t.Fatalf("SendFileContext = (%d, %v) with %d native calls", r.n, r.err, src.calls.Load())
			}
			if !bytes.Equal(src.out.Bytes(), data) {
				t.Fatal("the copy differs from the file")
			}
			return
		case <-time.After(time.Millisecond):
			src.drainOut(-1)
		}
	}
}

// The errors a SendFileContext reports before it sends anything.
func TestSendFileContextRefusals(t *testing.T) {
	f, data := testFile(t, 1000)
	src := newFakeFileSender(4096, 0)
	w := src.state.Writer(0)
	if _, err := w.SendFile(nil, 0, 1); !errors.Is(err, os.ErrInvalid) {
		t.Fatalf("SendFile(nil) = %v, want os.ErrInvalid", err)
	}
	if _, err := w.SendFile(f, 0, int64(len(data))+1); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("SendFile past the end = %v, want io.ErrUnexpectedEOF", err)
	}
	if n, err := w.SendFile(f, int64(len(data)), -1); n != 0 || err != nil {
		t.Fatalf("SendFile at the end = (%d, %v), want nothing to send", n, err)
	}
	EnterTurn()
	_, err := w.SendFile(f, 0, 1)
	LeaveTurn()
	if !errors.Is(err, ErrBlockingOnTurn) {
		t.Fatalf("SendFile on a turn = %v, want ErrBlockingOnTurn", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := w.SendFileContext(ctx, f, 0, 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("SendFileContext with a done ctx = %v", err)
	}
	src.sendErr = errors.New("boom")
	if _, err := w.SendFile(f, 0, 10); err == nil || err.Error() != "boom" {
		t.Fatalf("SendFile on a failing source = %v", err)
	}
	src.state.OnClose(true, nil)
	if _, err := w.SendFile(f, 0, 10); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("SendFile on a closed writer = %v, want net.ErrClosed", err)
	}
}

// io.Copy hands ReadFrom the wrapper os.File.WriteTo makes, and ReadFrom
// still sends the file with SendFile; so do a LimitedReader of a file and the
// file itself. A pipe is read as any reader.
func TestReadFromSendsFilesWithSendFile(t *testing.T) {
	f, data := testFile(t, 32<<10)
	readers := map[string]func() io.Reader{
		"file":    func() io.Reader { return f },
		"limited": func() io.Reader { return io.LimitReader(f, 1000) },
	}
	for name, reader := range readers {
		t.Run(name, func(t *testing.T) {
			src := newFakeFileSender(1<<20, 1<<20)
			w := src.state.Writer(0)
			if _, err := f.Seek(0, io.SeekStart); err != nil {
				t.Fatal(err)
			}
			n, err := w.ReadFrom(reader())
			want := len(data)
			if name == "limited" {
				want = 1000
			}
			if n != int64(want) || err != nil || src.calls.Load() != 1 || !bytes.Equal(src.sent(), data[:want]) {
				t.Fatalf("ReadFrom = (%d, %v) with %d SendFile calls", n, err, src.calls.Load())
			}
		})
	}
	t.Run("io.Copy", func(t *testing.T) {
		src := newFakeFileSender(1<<20, 1<<20)
		w := src.state.Writer(0)
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			t.Fatal(err)
		}
		n, err := io.Copy(w, f)
		if n != int64(len(data)) || err != nil || src.calls.Load() != 1 {
			t.Fatalf("io.Copy = (%d, %v) with %d SendFile calls, want one", n, err, src.calls.Load())
		}
		if pos, _ := f.Seek(0, io.SeekCurrent); pos != int64(len(data)) {
			t.Fatalf("io.Copy left the position at %d", pos)
		}
	})
	t.Run("pipe", func(t *testing.T) {
		pr, pw, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		defer pr.Close()
		go func() {
			_, _ = pw.Write([]byte("through a pipe"))
			_ = pw.Close()
		}()
		src := newFakeFileSender(1<<20, 1<<20)
		w := src.state.Writer(0)
		n, err := io.Copy(w, pr)
		if n != 14 || err != nil || src.calls.Load() != 0 || src.out.String() != "through a pipe" {
			t.Fatalf("io.Copy of a pipe = (%d, %v) with %d SendFile calls", n, err, src.calls.Load())
		}
	})
}

// osFile unwraps only os's own wrapper: a type of the caller's that embeds a
// file may read it differently.
func TestOsFileUnwrapsOnlyTheWriteToWrapper(t *testing.T) {
	f, _ := testFile(t, 10)
	var captured io.Reader
	_, _ = f.WriteTo(readerCapture{&captured})
	if got, ok := osFile(captured); !ok || got != f {
		t.Fatalf("osFile(%T) = %v, %v", captured, got, ok)
	}
	type myFile struct{ *os.File }
	if _, ok := osFile(myFile{f}); ok {
		t.Fatal("osFile unwrapped a caller's type")
	}
	if _, ok := osFile(bytes.NewReader(nil)); ok {
		t.Fatal("osFile took a bytes.Reader for a file")
	}
}

type readerCapture struct{ r *io.Reader }

func (c readerCapture) Write(p []byte) (int, error) { return len(p), nil }

func (c readerCapture) ReadFrom(r io.Reader) (int64, error) {
	*c.r = r
	return 0, nil
}

// Writer waits on the progress it subscribes to before anyone can find it:
// a Writer a second goroutine gets is never one whose wakes went unheard.
func TestStateWriterSubscribesBeforePublishing(t *testing.T) {
	src := newFakeSource(1024, 1024)
	src.state = NewState(src)
	var watched atomic.Bool
	src.state.src = watchingSource{src, &watched}
	if w := src.state.Writer(0); w == nil || !watched.Load() {
		t.Fatal("Writer returned before it watched the source's progress")
	}
}

type watchingSource struct {
	*fakeSource
	watched *atomic.Bool
}

func (s watchingSource) WatchWrite(fn func(int)) bool {
	if s.fakeSource.state.writer.Load() != nil {
		panic("Writer published before it watched")
	}
	s.watched.Store(true)
	return s.fakeSource.WatchWrite(fn)
}
