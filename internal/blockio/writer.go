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
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/urpc/uio/internal/bytebuf"
)

const readFromChunkSize = 32 << 10

// Writer is a stream connection's blocking outbound endpoint.
type Writer struct {
	src     Source
	waiting atomic.Int32 // active or queued blocking writers; prevents fast-path barging
	wmu     sync.Mutex   // serializes multi-goroutine Write / Writev / ReadFrom calls
	mu      sync.Mutex   // protects closed/closeErr
	waiter  waiter

	customWin atomic.Int64
	closed    bool
	closeErr  error
}

// HasWaiters reports whether any blocking write call is currently in progress or waiting.
func (w *Writer) HasWaiters() bool {
	return w != nil && w.waiting.Load() > 0
}

func newWriter(src Source, window int) *Writer {
	w := &Writer{src: src, waiter: newWaiter()}
	if window > 0 {
		w.customWin.Store(int64(window))
	}
	return w
}

func (w *Writer) setWindow(n int) {
	if n > 0 {
		w.customWin.Store(int64(n))
		w.waiter.notify()
	}
}

func (w *Writer) window() int {
	maxOut := w.src.MaxOutbound()
	if cw := int(w.customWin.Load()); cw > 0 {
		if maxOut > 0 && cw > maxOut {
			return maxOut
		}
		return cw
	}
	if maxOut > 0 {
		return maxOut
	}
	return DefaultWindow
}

// NotifySpace wakes any writer waiting for outbound queue or flow-control room.
func (w *Writer) NotifySpace() { w.waiter.notify() }

func (w *Writer) onClose(localClose bool, err error) {
	w.mu.Lock()
	if !w.closed {
		w.closed = true
		if localClose || err == nil || err == io.EOF {
			w.closeErr = net.ErrClosed
		} else {
			w.closeErr = err
		}
	}
	w.mu.Unlock()
	w.waiter.closeWake()
}

func (w *Writer) terminalErr() error {
	w.mu.Lock()
	closed, err := w.closed, w.closeErr
	w.mu.Unlock()
	if closed {
		return err
	}
	if w.src.WriteEnded() || w.src.ClosingOrClosed() {
		return net.ErrClosed
	}
	return nil
}

func (w *Writer) resolveWriteErr(err error) error {
	if tErr := w.terminalErr(); tErr != nil {
		return tErr
	}
	return err
}

// Buffered reports accepted outbound bytes not yet flushed to the wire.
func (w *Writer) Buffered() int { return w.src.OutboundBuffered() }

// SetWriteDeadline sets the deadline for blocked Write and ReadFrom calls
// without closing the underlying connection on expiry.
func (w *Writer) SetWriteDeadline(t time.Time) error { return w.waiter.setDeadline(t) }

// Flush asks the underlying transport to flush queued outbound data and waits
// until the outbound buffer drains, the connection closes, or the write
// deadline expires.
func (w *Writer) Flush() error {
	if OnTurn() {
		return ErrBlockingOnTurn
	}
	w.enterWrite()
	defer w.leaveWrite()

	if err := w.src.Flush(); err != nil {
		return w.resolveWriteErr(err)
	}
	for {
		tok := w.waiter.prepare()
		if err := w.terminalErr(); err != nil {
			w.waiter.cancelPrepare()
			return err
		}
		if err := w.waiter.checkDeadline(); err != nil {
			w.waiter.cancelPrepare()
			return err
		}
		if w.src.OutboundBuffered() == 0 {
			w.waiter.cancelPrepare()
			return nil
		}
		w.waiter.wait(tok)
	}
}

func (w *Writer) enterWrite() {
	w.waiting.Add(1)
	w.wmu.Lock()
}

func (w *Writer) leaveWrite() {
	w.wmu.Unlock()
	w.waiting.Add(-1)
}

// Write writes all of p to the connection, suspending when the outbound window
// is full. Concurrent calls on the same Writer are serialized atomically.
func (w *Writer) Write(p []byte) (int, error) {
	if OnTurn() {
		return 0, ErrBlockingOnTurn
	}
	w.enterWrite()
	defer w.leaveWrite()
	return w.writeLocked(p)
}

// Writev writes all segments of vec atomically among blocking writers,
// suspending when the outbound window is full.
func (w *Writer) Writev(vec [][]byte) (int, error) {
	if OnTurn() {
		return 0, ErrBlockingOnTurn
	}
	total := 0
	for _, seg := range vec {
		total += len(seg)
	}
	w.enterWrite()
	defer w.leaveWrite()
	if err := w.terminalErr(); err != nil {
		return 0, err
	}
	if err := w.waiter.checkDeadline(); err != nil {
		return 0, err
	}
	if total == 0 {
		return 0, nil
	}
	limit := w.window()
	if total <= limit && w.src.OutboundBuffered()+total <= limit {
		n, err := w.src.Writev(vec)
		if err == nil && n == total {
			return n, nil
		}
		if err != nil && !w.src.IsOverflow(err) {
			return n, w.resolveWriteErr(err)
		}
		if n > 0 {
			// Advance past already accepted prefix of vec.
			rem := n
			for len(vec) > 0 && rem >= len(vec[0]) {
				rem -= len(vec[0])
				vec = vec[1:]
			}
			if rem > 0 && len(vec) > 0 {
				nw, werr := w.writeLocked(vec[0][rem:])
				n += nw
				if werr != nil {
					return n, werr
				}
				vec = vec[1:]
			}
			for _, seg := range vec {
				nw, werr := w.writeLocked(seg)
				n += nw
				if werr != nil {
					return n, werr
				}
			}
			return n, nil
		}
	}
	written := 0
	for _, seg := range vec {
		if len(seg) == 0 {
			continue
		}
		n, err := w.writeLocked(seg)
		written += n
		if err != nil {
			return written, err
		}
	}
	return written, nil
}

func (w *Writer) writeLocked(p []byte) (int, error) {
	if err := w.terminalErr(); err != nil {
		return 0, err
	}
	if err := w.waiter.checkDeadline(); err != nil {
		return 0, err
	}
	if len(p) == 0 {
		return 0, nil
	}

	written := 0
	for written < len(p) {
		tok := w.waiter.prepare()
		if err := w.terminalErr(); err != nil {
			w.waiter.cancelPrepare()
			return written, err
		}
		if err := w.waiter.checkDeadline(); err != nil {
			w.waiter.cancelPrepare()
			return written, err
		}
		limit := w.window()
		if buffered := w.src.OutboundBuffered(); buffered < limit {
			room := limit - buffered
			chunk := p[written : written+min(len(p)-written, room)]
			for {
				n, err := w.src.Write(chunk)
				written += n
				if n > 0 && (err == nil || w.src.IsOverflow(err)) {
					w.waiter.cancelPrepare()
					goto nextWrite
				}
				if err != nil && !w.src.IsOverflow(err) {
					w.waiter.cancelPrepare()
					return written, w.resolveWriteErr(err)
				}
				// If a lower layer enforces a smaller per-write cap than w.window()
				// on an empty queue, halve chunk until it fits!
				if buffered == 0 && len(chunk) > 1 {
					chunk = chunk[:len(chunk)/2]
					continue
				}
				break
			}
		}
		w.waiter.wait(tok)
	nextWrite:
	}
	return written, nil
}

// WriteOwned transfers ownership of buf to the connection, suspending while the
// outbound window has no room, and always releases buf on failure.
func (w *Writer) WriteOwned(buf *bytebuf.Buffer) (int, error) {
	if OnTurn() {
		bytebuf.ReleaseBuffer(buf)
		return 0, ErrBlockingOnTurn
	}
	if buf == nil || buf.Len() == 0 {
		bytebuf.ReleaseBuffer(buf)
		return 0, nil
	}
	w.enterWrite()
	defer w.leaveWrite()
	n, retained, err := w.writeOwnedPreserveLocked(buf)
	if retained {
		bytebuf.ReleaseBuffer(buf)
	}
	return n, err
}

func (w *Writer) writeOwnedPreserveOnAbort(buf *bytebuf.Buffer) (int, bool, error) {
	w.enterWrite()
	defer w.leaveWrite()
	return w.writeOwnedPreserveLocked(buf)
}

func (w *Writer) writeOwnedPreserveLocked(buf *bytebuf.Buffer) (int, bool, error) {
	size := buf.Len()
	if size > w.window() {
		n, err := w.writeLocked(buf.Bytes())
		if n > 0 {
			buf.Discard(n)
		}
		if buf.Len() == 0 {
			bytebuf.ReleaseBuffer(buf)
			return n, false, err
		}
		return n, true, err
	}
	for {
		tok := w.waiter.prepare()
		if err := w.terminalErr(); err != nil {
			w.waiter.cancelPrepare()
			return 0, true, err
		}
		if err := w.waiter.checkDeadline(); err != nil {
			w.waiter.cancelPrepare()
			return 0, true, err
		}
		limit := w.window()
		if buffered := w.src.OutboundBuffered(); buffered == 0 || buffered+size <= limit {
			n, err := w.src.WriteOwned(buf)
			if err == nil {
				w.waiter.cancelPrepare()
				return n, false, nil
			}
			if !w.src.IsOverflow(err) {
				w.waiter.cancelPrepare()
				// w.src.WriteOwned already released buf on non-overflow errors!
				return n, false, w.resolveWriteErr(err)
			}
			// If even an empty queue refused buf (lower layer MaxBuffered < buf.Len()),
			// fall back to chunked writeLocked so it never hangs!
			if buffered == 0 && buf.Len() > 1 {
				w.waiter.cancelPrepare()
				nw, werr := w.writeLocked(buf.Bytes())
				if nw > 0 {
					buf.Discard(nw)
				}
				if buf.Len() == 0 {
					bytebuf.ReleaseBuffer(buf)
					return n + nw, false, werr
				}
				return n + nw, true, werr
			}
		}
		w.waiter.wait(tok)
	}
}

// ReadFrom copies from r into the connection using pooled zero-copy WriteOwned
// buffers. When r implements io.WriterTo (such as *Reader), it delegates to
// WriteTo so pooled pipe blocks transfer directly without an intermediate copy.
func (w *Writer) ReadFrom(r io.Reader) (int64, error) {
	if OnTurn() {
		return 0, ErrBlockingOnTurn
	}
	if wt, ok := r.(io.WriterTo); ok {
		return wt.WriteTo(w)
	}
	w.enterWrite()
	defer w.leaveWrite()

	var total int64
	for {
		if err := w.terminalErr(); err != nil {
			return total, err
		}
		if err := w.waiter.checkDeadline(); err != nil {
			return total, err
		}
		chunkCap := min(readFromChunkSize, max(w.window(), 1))
		buf := bytebuf.AcquireBuffer(chunkCap)
		nr, rerr := r.Read(buf.AvailableBuffer()[:chunkCap])
		if nr > 0 {
			buf.CommitWrite(nr)
			nw, retained, werr := w.writeOwnedPreserveLocked(buf)
			if retained {
				bytebuf.ReleaseBuffer(buf)
			}
			total += int64(nw)
			if werr != nil {
				return total, werr
			}
		} else {
			bytebuf.ReleaseBuffer(buf)
		}
		if rerr != nil {
			if rerr == io.EOF {
				return total, nil
			}
			return total, rerr
		}
	}
}
