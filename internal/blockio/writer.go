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
	src    Source
	wmu    sync.Mutex // serializes multi-goroutine Write / ReadFrom calls
	mu     sync.Mutex // protects closed/closeErr
	waiter waiter

	customWin atomic.Int64
	closed    bool
	closeErr  error
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
	w.wmu.Lock()
	defer w.wmu.Unlock()

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

// Write writes all of p to the connection, suspending when the outbound window
// is full. Concurrent calls on the same Writer are serialized atomically.
func (w *Writer) Write(p []byte) (int, error) {
	if OnTurn() {
		return 0, ErrBlockingOnTurn
	}
	w.wmu.Lock()
	defer w.wmu.Unlock()
	return w.writeLocked(p)
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
			n, err := w.src.Write(chunk)
			written += n
			if n > 0 && (err == nil || w.src.IsOverflow(err)) {
				w.waiter.cancelPrepare()
				continue
			}
			if err != nil && !w.src.IsOverflow(err) {
				w.waiter.cancelPrepare()
				return written, w.resolveWriteErr(err)
			}
		}
		w.waiter.wait(tok)
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
	w.wmu.Lock()
	defer w.wmu.Unlock()
	n, err := w.writeOwnedPreserveLocked(buf)
	if err != nil && buf.Len() > 0 {
		bytebuf.ReleaseBuffer(buf)
	}
	return n, err
}

func (w *Writer) writeOwnedPreserveOnAbort(buf *bytebuf.Buffer) (int, error) {
	w.wmu.Lock()
	defer w.wmu.Unlock()
	return w.writeOwnedPreserveLocked(buf)
}

func (w *Writer) writeOwnedPreserveLocked(buf *bytebuf.Buffer) (int, error) {
	size := buf.Len()
	if size > w.window() {
		n, err := w.writeLocked(buf.Bytes())
		if n > 0 {
			buf.Discard(n)
		}
		if buf.Len() == 0 {
			bytebuf.ReleaseBuffer(buf)
		}
		return n, err
	}
	for {
		tok := w.waiter.prepare()
		if err := w.terminalErr(); err != nil {
			w.waiter.cancelPrepare()
			return 0, err
		}
		if err := w.waiter.checkDeadline(); err != nil {
			w.waiter.cancelPrepare()
			return 0, err
		}
		limit := w.window()
		if buffered := w.src.OutboundBuffered(); buffered == 0 || buffered+size <= limit {
			n, err := w.src.WriteOwned(buf)
			if err == nil {
				w.waiter.cancelPrepare()
				return n, nil
			}
			if !w.src.IsOverflow(err) {
				w.waiter.cancelPrepare()
				buf.Reset()
				return n, w.resolveWriteErr(err)
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
	w.wmu.Lock()
	defer w.wmu.Unlock()

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
			nw, werr := w.writeOwnedPreserveLocked(buf)
			if werr != nil && buf.Len() > 0 {
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
