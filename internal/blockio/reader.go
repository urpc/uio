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
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/urpc/uio/internal/bytebuf"
	"github.com/urpc/uio/internal/readcontrol"
)

// Reader is a stream connection's pull-mode blocking inbound endpoint.
type Reader struct {
	src     Source
	rmu     sync.Mutex // serializes concurrent Read / WriteTo callers
	pauseMu sync.Mutex // serializes transport Pause/Resume transitions against r.paused
	mu      sync.Mutex // guards pipe, direct handoff, pause, and close state
	waiter  waiter

	customWin atomic.Int64
	pipe      bytebuf.CompositeBuffer
	pause     *readcontrol.Pause
	paused    bool

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
			r.waiter.cancelPrepare()
			return 0, err
		}
		if r.directReady {
			n := r.directN
			r.directReady = false
			r.directN = 0
			r.mu.Unlock()
			r.waiter.cancelPrepare()
			return n, nil
		}
		if err := r.waiter.checkDeadline(); err != nil {
			r.directDst = nil
			r.mu.Unlock()
			r.waiter.cancelPrepare()
			return 0, err
		}
		if !r.pipe.Empty() {
			r.directDst = nil
			n, _ := r.pipe.Read(p)
			resume := r.checkResumeLocked()
			r.mu.Unlock()
			r.waiter.cancelPrepare()
			if resume {
				r.resumeTransport()
			}
			return n, nil
		}
		if r.peerEOF {
			r.directDst = nil
			r.mu.Unlock()
			r.waiter.cancelPrepare()
			return 0, io.EOF
		}
		if r.closed {
			r.directDst = nil
			err := r.closeErr
			r.mu.Unlock()
			r.waiter.cancelPrepare()
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

	var total int64
	var scratch []byte
	for {
		tok := r.waiter.prepare()
		r.mu.Lock()
		if r.closed && r.localAbort {
			err := r.closeErr
			r.mu.Unlock()
			r.waiter.cancelPrepare()
			return total, err
		}
		if err := r.waiter.checkDeadline(); err != nil {
			r.mu.Unlock()
			r.waiter.cancelPrepare()
			return total, err
		}
		if !r.pipe.Empty() {
			if dstWriter != nil {
				block := r.pipe.PopBlock()
				resume := r.checkResumeLocked()
				r.mu.Unlock()
				r.waiter.cancelPrepare()
				if resume {
					r.resumeTransport()
				}
				if block != nil {
					nw, retained, werr := dstWriter.writeOwnedPreserveOnAbort(block)
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
						return total, werr
					}
				}
				continue
			}
			if scratch == nil {
				scratch = make([]byte, 32<<10)
			}
			n, _ := r.pipe.Read(scratch)
			resume := r.checkResumeLocked()
			r.mu.Unlock()
			r.waiter.cancelPrepare()
			if resume {
				r.resumeTransport()
			}
			nw, werr := w.Write(scratch[:n])
			total += int64(nw)
			if werr != nil {
				return total, werr
			}
			if nw != n {
				return total, io.ErrShortWrite
			}
			continue
		}
		if r.peerEOF {
			r.mu.Unlock()
			r.waiter.cancelPrepare()
			return total, nil
		}
		if r.closed {
			err := r.closeErr
			r.mu.Unlock()
			r.waiter.cancelPrepare()
			return total, err
		}
		r.mu.Unlock()
		r.waiter.wait(tok)
	}
}
