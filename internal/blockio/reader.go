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
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/urpc/uio/internal/bytebuf"
	"github.com/urpc/uio/internal/readcontrol"
)

// SpliceSource is an optional capability of native stream sources whose socket
// input SpliceTo can take over with Linux splice(2).
type SpliceSource interface {
	// RequestSplice asks the connection to stop reading its socket and to
	// hand what it already read to the Reader; false when it cannot be
	// spliced.
	RequestSplice() bool
	// SpliceGranted reports whether the connection handed its socket over,
	// and whether its input ended or it is closing, so it never will.
	SpliceGranted() (granted, ended bool)
	// SpliceIn moves up to n bytes from the granted socket into the pipe
	// whose write end is pipeW. It reports 0 and no error at the end of the
	// stream, which the connection's turns read again and pass on once the
	// socket is theirs, and net.ErrClosed once the connection closed it. A
	// failed socket closes the connection with its error.
	SpliceIn(pipeW, n int) (int, error)
	// EndSplice gives the socket back to the connection's turns.
	EndSplice()
}

// Reader is a stream connection's pull-mode blocking inbound endpoint.
type Reader struct {
	src     Source
	rmu     sync.Mutex // serializes concurrent Read / WriteTo callers
	pauseMu sync.Mutex // serializes transport Pause/Resume transitions against r.paused
	mu      sync.Mutex // guards pipe, direct handoff, pause, and close state
	waiter  waiter

	customWin    atomic.Int64
	spliceActive atomic.Bool
	pipe         bytebuf.CompositeBuffer
	pause        *readcontrol.Pause
	paused       bool

	directDst   []byte
	directN     int
	directReady bool

	peerEOF    bool
	closed     bool
	localAbort bool // true on local Close() or Abort(): drops unread pipe immediately
	closeErr   error
}

func newReader(src Source, window int) *Reader {
	r := &Reader{
		src:    src,
		waiter: newWaiter(),
		pause:  src.NewReadPause(),
	}
	if window > 0 {
		r.customWin.Store(int64(window))
	}
	return r
}

func (r *Reader) setWindow(n int) {
	if n > 0 {
		r.customWin.Store(int64(n))
		r.mu.Lock()
		resume := r.checkResumeLocked()
		r.mu.Unlock()
		if resume {
			r.resumeTransport()
		}
	}
}

func (r *Reader) window() int {
	if cw := int(r.customWin.Load()); cw > 0 {
		return cw
	}
	if maxIn := r.src.MaxInbound(); maxIn > 0 {
		return maxIn
	}
	return DefaultWindow
}

func (r *Reader) syncPause(wakeIfUnpaused bool) bool {
	r.pauseMu.Lock()
	defer r.pauseMu.Unlock()
	r.mu.Lock()
	wantPaused := r.paused && !r.closed
	r.mu.Unlock()
	if r.pause == nil {
		if !wantPaused && wakeIfUnpaused {
			_ = r.src.Wake()
		}
		return false
	}
	if wantPaused {
		if !r.pause.Paused() {
			_ = r.pause.Pause()
		}
		return r.pause.Paused()
	}
	if r.pause.Paused() {
		_ = r.pause.Resume()
	} else if wakeIfUnpaused {
		_ = r.src.Wake()
	}
	return false
}

func (r *Reader) resumeTransport() {
	r.syncPause(true)
}

// Pump runs on the connection's turn in place of Handler.OnRead, moving inbound
// bytes directly into a parked reader's slice or into the bounded pipe.
func (r *Reader) Pump() {
	if r.spliceActive.Load() && r.src.InboundBuffered() == 0 {
		if r.src.ReadEnded() {
			r.mu.Lock()
			r.peerEOF = true
			r.mu.Unlock()
		}
		r.waiter.notify()
		return
	}
	for {
		chunk := r.src.PeekChunk()
		if len(chunk) == 0 {
			break
		}
		r.mu.Lock()
		if r.closed && r.localAbort {
			r.mu.Unlock()
			_, _ = r.src.Discard(len(chunk))
			return
		}
		// Fast path: 1-copy direct handoff into a parked reader's destination buffer.
		if len(r.directDst) > 0 && !r.directReady {
			n := copy(r.directDst, chunk)
			r.directN = n
			r.directDst = nil
			r.directReady = true
			r.mu.Unlock()
			_, _ = r.src.Discard(n)
			r.waiter.notify()
			continue
		}

		limit := r.window()
		cur := r.pipe.Len()
		if cur >= limit {
			r.paused = true
			r.mu.Unlock()
			r.syncPause(false)
			return
		}
		take := min(len(chunk), limit-cur)
		_, _ = r.pipe.Write(chunk[:take])
		nowFull := r.pipe.Len() >= limit
		if nowFull {
			r.paused = true
		}
		r.mu.Unlock()

		_, _ = r.src.Discard(take)
		paused := nowFull && r.syncPause(false)
		r.waiter.notify()
		if paused {
			return
		}
	}

	if r.src.InboundBuffered() == 0 && r.src.ReadEnded() {
		r.mu.Lock()
		wasEOF := r.peerEOF
		r.peerEOF = true
		r.mu.Unlock()
		if !wasEOF {
			r.waiter.notify()
		}
	}
}

// testHookPipeDrained, when set, runs as SpliceTo found the pipe drained and
// is about to look at the end of the stream.
var testHookPipeDrained atomic.Pointer[func()]

// Notify wakes a goroutine blocked in Read or SpliceTo so it looks at its
// source again.
func (r *Reader) Notify() { r.waiter.notify() }

func (r *Reader) onClose(localClose bool, err error) {
	r.mu.Lock()
	if !r.closed {
		r.closed = true
		switch {
		case localClose:
			r.localAbort = true
			r.closeErr = net.ErrClosed
			r.pipe.Reset()
		case err == nil || errors.Is(err, io.EOF):
			r.peerEOF = true
			r.closeErr = io.EOF
		default:
			r.closeErr = err
		}
		r.paused = false
	}
	r.mu.Unlock()
	r.syncPause(false)
	r.waiter.closeWake()
}

// Buffered reports unread bytes currently held in the blocking pipe.
func (r *Reader) Buffered() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.pipe.Len()
}

// SetReadDeadline sets the deadline for blocked Read and WriteTo calls without
// closing the underlying connection on expiry.
func (r *Reader) SetReadDeadline(t time.Time) error {
	return r.waiter.setDeadline(t)
}

func (r *Reader) checkResumeLocked() bool {
	if r.paused && r.pipe.Len() <= r.window()/2 {
		r.paused = false
		return true
	}
	return false
}

// Read reads up to len(p) bytes from the connection, blocking until data
// arrives, the peer ends the stream, the connection closes, or the read
// deadline expires.
func (r *Reader) Read(p []byte) (int, error) {
	if OnTurn() {
		return 0, ErrBlockingOnTurn
	}
	r.rmu.Lock()
	defer r.rmu.Unlock()

	r.mu.Lock()
	if len(p) == 0 {
		var err error
		if r.closed && r.localAbort {
			err = r.closeErr
		} else {
			err = r.waiter.checkDeadline()
		}
		r.mu.Unlock()
		return 0, err
	}

	for {
		tok := r.waiter.prepare()
		if r.closed && r.localAbort {
			r.directDst = nil
			r.directReady = false
			err := r.closeErr
			r.mu.Unlock()
			return 0, err
		}
		if r.directReady {
			n := r.directN
			r.directReady = false
			r.directN = 0
			r.mu.Unlock()
			return n, nil
		}
		if err := r.waiter.checkDeadline(); err != nil {
			r.directDst = nil
			r.mu.Unlock()
			return 0, err
		}
		if !r.pipe.Empty() {
			r.directDst = nil
			n, _ := r.pipe.Read(p)
			resume := r.checkResumeLocked()
			r.mu.Unlock()
			if resume {
				r.resumeTransport()
			}
			return n, nil
		}
		if r.peerEOF {
			r.directDst = nil
			r.mu.Unlock()
			return 0, io.EOF
		}
		if r.closed {
			r.directDst = nil
			err := r.closeErr
			r.mu.Unlock()
			return 0, err
		}
		r.directDst = p
		r.directN = 0
		r.directReady = false
		r.mu.Unlock()
		r.waiter.wait(tok)
		r.mu.Lock()
	}
}

// WriteTo writes inbound stream data to w until EOF or error. When w is a
// *Writer (or unwraps to one via InternalWriter), pooled blocks transfer with
// zero intermediate copying.
func (r *Reader) WriteTo(w io.Writer) (int64, error) {
	return r.SpliceTo(context.Background(), w, -1)
}

// SpliceTo transfers count bytes from r into w, or the stream up to EOF when
// count < 0, honoring ctx cancellation and read deadlines. A stream that ends
// before count bytes fails as io.ReadFull does: io.EOF when nothing moved,
// io.ErrUnexpectedEOF otherwise. On Linux, when r's source and w — a Writer
// of a native connection, or an *os.File — both take splice(2), the stream
// moves through a pooled kernel pipe without being copied into user space.
func (r *Reader) SpliceTo(ctx context.Context, w io.Writer, count int64) (int64, error) {
	if OnTurn() {
		return 0, ErrBlockingOnTurn
	}
	var dstWriter *Writer
	switch target := w.(type) {
	case *Writer:
		dstWriter = target
	case interface{ InternalWriter() *Writer }:
		dstWriter = target.InternalWriter()
	}

	r.rmu.Lock()
	defer r.rmu.Unlock()

	var scratch []byte
	// What the Reader holds goes first, then the native splice moves the rest
	// of the stream where it can; whatever it leaves is copied.
	total, done, err := r.drainPipeTo(ctx, w, dstWriter, &scratch, count, 0)
	if err != nil || done {
		return total, err
	}
	var fallback bool
	total, fallback, err = r.trySpliceNative(ctx, w, dstWriter, &scratch, count, total)
	if !fallback {
		return total, err
	}
	for count < 0 || total < count {
		tok := r.waiter.prepare()
		total, done, err = r.drainPipeTo(ctx, w, dstWriter, &scratch, count, total)
		if err != nil || done {
			return total, err
		}
		if hook := testHookPipeDrained.Load(); hook != nil {
			(*hook)()
		}
		r.mu.Lock()
		if !r.pipe.Empty() {
			// A Pump added bytes since the drain looked: they come before the
			// end it may have noted.
			r.mu.Unlock()
			continue
		}
		if r.peerEOF {
			r.mu.Unlock()
			if count >= 0 && total < count {
				if total == 0 {
					return 0, io.EOF
				}
				return total, io.ErrUnexpectedEOF
			}
			return total, nil
		}
		if r.closed {
			cerr := r.closeErr
			r.mu.Unlock()
			return total, cerr
		}
		r.mu.Unlock()
		if err := r.waiter.waitContext(ctx, tok); err != nil {
			return total, err
		}
	}
	return total, nil
}

func (r *Reader) drainPipeTo(
	ctx context.Context,
	w io.Writer,
	dstWriter *Writer,
	scratch *[]byte,
	count int64,
	total int64,
) (int64, bool, error) {
	for {
		r.mu.Lock()
		if r.closed && r.localAbort {
			err := r.closeErr
			r.mu.Unlock()
			return total, true, err
		}
		if ctx != nil {
			if err := ctx.Err(); err != nil {
				r.mu.Unlock()
				return total, true, err
			}
		}
		if err := r.waiter.checkDeadline(); err != nil {
			r.mu.Unlock()
			return total, true, err
		}
		if count >= 0 && total >= count {
			r.mu.Unlock()
			return total, true, nil
		}
		if r.pipe.Empty() {
			r.mu.Unlock()
			return total, false, nil
		}
		if dstWriter != nil {
			block := r.pipe.PopBlock()
			if block != nil && count >= 0 && int64(block.Len()) > count-total {
				want := int(count - total)
				head := bytebuf.CloneBuffer(block.Bytes()[:want])
				block.Discard(want)
				r.pipe.PrependOwned(block)
				block = head
			}
			resume := r.checkResumeLocked()
			r.mu.Unlock()
			if resume {
				r.resumeTransport()
			}
			if block != nil {
				nw, retained, werr := dstWriter.writeOwnedPreserveContext(ctx, block)
				total += int64(nw)
				if werr != nil {
					if retained {
						r.mu.Lock()
						if !r.closed || !r.localAbort {
							r.pipe.PrependOwned(block)
						} else {
							bytebuf.ReleaseBuffer(block)
						}
						r.mu.Unlock()
					}
					return total, true, werr
				}
			}
			continue
		}
		if *scratch == nil {
			*scratch = make([]byte, 32<<10)
		}
		maxRead := len(*scratch)
		if count >= 0 && int64(maxRead) > count-total {
			maxRead = int(count - total)
		}
		n, _ := r.pipe.Read((*scratch)[:maxRead])
		resume := r.checkResumeLocked()
		r.mu.Unlock()
		if resume {
			r.resumeTransport()
		}
		nw, werr := w.Write((*scratch)[:n])
		total += int64(nw)
		if werr != nil {
			return total, true, werr
		}
		if nw != n {
			return total, true, io.ErrShortWrite
		}
	}
}
