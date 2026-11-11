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
	"io"
	"net"
	"os"
	"reflect"
	"sync"
	"sync/atomic"
	"time"

	"github.com/urpc/uio/internal/bytebuf"
	"github.com/urpc/uio/internal/filerange"
)

// FileSender is an optional capability of native stream sources whose sockets
// send files with sendfile(2).
type FileSender interface {
	HasNativeSendFile() bool
	// SendFile sends or queues the native range r of file, admitted whole
	// while the source's queue is under its limit: a range holds a
	// descriptor, not memory. owner marks what it queued for
	// CancelQueuedFiles. A call queues what it does not send in one piece
	// behind what it sent, so withdrawing that piece leaves on the wire a
	// prefix of the range. A refusal for want of room is ErrOverflow; the
	// call moves bytes, or takes the range, whenever the queue had room.
	SendFile(file *os.File, r filerange.Range, owner uint64) (int64, error)
	// CancelQueuedFiles withdraws what owner queued and has not been sent,
	// and reports the bytes withdrawn. The part of a range a sendfile(2) is
	// reading stays: it may be on the wire already.
	CancelQueuedFiles(owner uint64) int64
}

// SpliceTarget is an optional capability implemented by native stream sources
// that can receive bytes directly from a Linux splice(2) pipe.
type SpliceTarget interface {
	SpliceFromPipe(pipeR int, count int) (int, error)
}

const readFromChunkSize = 32 << 10

// Writer is a stream connection's blocking outbound endpoint.
type Writer struct {
	src    Source
	wsem   chan struct{} // 1-slot FIFO semaphore serializing multi-goroutine writes with ctx cancellation
	mu     sync.Mutex    // protects closed/closeErr
	waiter waiter

	customWin atomic.Int64
	closed    atomic.Bool
	closeErr  error
	fileSends atomic.Uint64 // numbers SendFileContext calls; see FileSender
}

func newWriter(src Source, window int) *Writer {
	w := &Writer{src: src, wsem: make(chan struct{}, 1), waiter: newWaiter()}
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
	if !w.closed.Load() {
		if localClose || err == nil || err == io.EOF {
			w.closeErr = net.ErrClosed
		} else {
			w.closeErr = err
		}
		w.closed.Store(true)
	}
	w.mu.Unlock()
	w.waiter.closeWake()
}

func (w *Writer) terminalErr() error {
	if w.closed.Load() {
		w.mu.Lock()
		err := w.closeErr
		w.mu.Unlock()
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
	return w.FlushContext(context.Background())
}

// FlushContext asks the underlying transport to flush queued outbound data and
// waits until the outbound buffer drains, the connection closes, the write
// deadline expires, or ctx ends.
func (w *Writer) FlushContext(ctx context.Context) error {
	if OnTurn() {
		return ErrBlockingOnTurn
	}
	if err := w.enterWrite(ctx); err != nil {
		return err
	}
	defer w.leaveWrite()

	if err := w.src.Flush(); err != nil {
		return w.resolveWriteErr(err)
	}
	for {
		tok := w.waiter.prepare()
		if err := w.terminalErr(); err != nil {
			return err
		}
		if ctx != nil {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		if err := w.waiter.checkDeadline(); err != nil {
			return err
		}
		if w.src.OutboundBuffered() == 0 {
			return nil
		}
		if err := w.waiter.waitContext(ctx, tok); err != nil {
			return err
		}
	}
}

func (w *Writer) enterWrite(ctx context.Context) error {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	select {
	case w.wsem <- struct{}{}:
		return nil
	default:
	}
	if ctx == nil || ctx.Done() == nil {
		w.wsem <- struct{}{}
		return nil
	}
	select {
	case w.wsem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (w *Writer) leaveWrite() {
	<-w.wsem
}

// Write writes all of p to the connection, suspending when the outbound window
// is full. Concurrent calls on the same Writer are serialized atomically.
func (w *Writer) Write(p []byte) (int, error) {
	return w.WriteContext(context.Background(), p)
}

// WriteContext writes all of p to the connection, suspending until space is
// available, the connection closes, the write deadline expires, or ctx ends.
func (w *Writer) WriteContext(ctx context.Context, p []byte) (int, error) {
	if OnTurn() {
		return 0, ErrBlockingOnTurn
	}
	if err := w.enterWrite(ctx); err != nil {
		return 0, err
	}
	defer w.leaveWrite()
	return w.writeLocked(ctx, p)
}

// Writev writes all segments of vec atomically among blocking writers,
// suspending when the outbound window is full.
func (w *Writer) Writev(vec [][]byte) (int, error) {
	return w.WritevContext(context.Background(), vec)
}

// WritevContext writes all segments of vec atomically among blocking writers,
// suspending until space is available, the connection closes, the write
// deadline expires, or ctx ends.
func (w *Writer) WritevContext(ctx context.Context, vec [][]byte) (int, error) {
	if OnTurn() {
		return 0, ErrBlockingOnTurn
	}
	total := 0
	for _, seg := range vec {
		total += len(seg)
	}
	if err := w.enterWrite(ctx); err != nil {
		return 0, err
	}
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
				nw, werr := w.writeLocked(ctx, vec[0][rem:])
				n += nw
				if werr != nil {
					return n, werr
				}
				vec = vec[1:]
			}
			for _, seg := range vec {
				nw, werr := w.writeLocked(ctx, seg)
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
		n, err := w.writeLocked(ctx, seg)
		written += n
		if err != nil {
			return written, err
		}
	}
	return written, nil
}

func (w *Writer) writeLocked(ctx context.Context, p []byte) (int, error) {
	if err := w.terminalErr(); err != nil {
		return 0, err
	}
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
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
			return written, err
		}
		if ctx != nil {
			if err := ctx.Err(); err != nil {
				return written, err
			}
		}
		if err := w.waiter.checkDeadline(); err != nil {
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
					goto nextWrite
				}
				if err != nil && !w.src.IsOverflow(err) {
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
		if err := w.waiter.waitContext(ctx, tok); err != nil {
			return written, err
		}
	nextWrite:
	}
	return written, nil
}

// TryWriteOwned attempts a fast non-blocking write when no writer is currently
// contended or parked on w and no deadline has expired. If the outbound window
// is full or contended, it returns ErrOverflow without releasing buf so the
// caller can fall back to WriteOwnedContext.
func (w *Writer) TryWriteOwned(buf *bytebuf.Buffer) (int, error) {
	if buf == nil || buf.Len() == 0 {
		bytebuf.ReleaseBuffer(buf)
		return 0, nil
	}
	if !w.waiter.parked.Load() && !w.closed.Load() && !w.waiter.expired.Load() {
		select {
		case w.wsem <- struct{}{}:
			defer w.leaveWrite()
			if cw := int(w.customWin.Load()); cw <= 0 || w.src.OutboundBuffered()+buf.Len() <= cw {
				n, err := w.src.WriteOwned(buf)
				if err == nil {
					return n, nil
				}
				if !w.src.IsOverflow(err) {
					return n, w.resolveWriteErr(err)
				}
				return n, ErrOverflow
			}
		default:
		}
	}
	return 0, ErrOverflow
}

// WriteOwned transfers ownership of buf to the connection, suspending while the
// outbound window has no room, and always releases buf on failure.
func (w *Writer) WriteOwned(buf *bytebuf.Buffer) (int, error) {
	return w.WriteOwnedContext(context.Background(), buf)
}

// WriteOwnedContext transfers ownership of buf to the connection, suspending
// while the outbound window has no room until space drains, the connection
// closes, the write deadline expires, or ctx ends. Always consumes or releases buf.
func (w *Writer) WriteOwnedContext(ctx context.Context, buf *bytebuf.Buffer) (int, error) {
	if OnTurn() {
		bytebuf.ReleaseBuffer(buf)
		return 0, ErrBlockingOnTurn
	}
	if buf == nil || buf.Len() == 0 {
		bytebuf.ReleaseBuffer(buf)
		return 0, nil
	}
	if err := w.enterWrite(ctx); err != nil {
		bytebuf.ReleaseBuffer(buf)
		return 0, err
	}
	defer w.leaveWrite()
	n, retained, err := w.writeOwnedPreserveLocked(ctx, buf)
	if retained {
		bytebuf.ReleaseBuffer(buf)
	}
	return n, err
}

func (w *Writer) writeOwnedPreserveContext(ctx context.Context, buf *bytebuf.Buffer) (int, bool, error) {
	if err := w.enterWrite(ctx); err != nil {
		return 0, true, err
	}
	defer w.leaveWrite()
	return w.writeOwnedPreserveLocked(ctx, buf)
}

func (w *Writer) writeOwnedPreserveLocked(ctx context.Context, buf *bytebuf.Buffer) (int, bool, error) {
	size := buf.Len()
	if size > w.window() {
		n, err := w.writeLocked(ctx, buf.Bytes())
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
			return 0, true, err
		}
		if ctx != nil {
			if err := ctx.Err(); err != nil {
				return 0, true, err
			}
		}
		if err := w.waiter.checkDeadline(); err != nil {
			return 0, true, err
		}
		limit := w.window()
		if buffered := w.src.OutboundBuffered(); buffered == 0 || buffered+size <= limit {
			n, err := w.src.WriteOwned(buf)
			if err == nil {
				return n, false, nil
			}
			if !w.src.IsOverflow(err) {
				// w.src.WriteOwned already released buf on non-overflow errors!
				return n, false, w.resolveWriteErr(err)
			}
			// If even an empty queue refused buf (lower layer MaxBuffered < buf.Len()),
			// fall back to chunked writeLocked so it never hangs!
			if buffered == 0 && buf.Len() > 1 {
				nw, werr := w.writeLocked(ctx, buf.Bytes())
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
		if err := w.waiter.waitContext(ctx, tok); err != nil {
			return 0, true, err
		}
	}
}

// ReadFrom copies from r into the connection. From an *os.File — r itself,
// the wrapper os.File.WriteTo hands io.Copy, or an *io.LimitedReader of
// either — it sends the part of a regular file fstat sizes as SendFile does,
// with sendfile(2) on a native connection, then reads whatever follows as
// from any reader: pipes and files of synthetic file systems are read to
// their end. A reader that implements io.WriterTo, a Reader say, writes
// itself to w, handing its buffers over without a copy; any other is read
// through pooled buffers.
func (w *Writer) ReadFrom(r io.Reader) (int64, error) {
	if OnTurn() {
		return 0, ErrBlockingOnTurn
	}
	lr, limited := r.(*io.LimitedReader)
	src := r
	if limited {
		src = lr.R
	}
	f, isFile := osFile(src)
	if !isFile {
		if wt, ok := r.(io.WriterTo); ok {
			return wt.WriteTo(w)
		}
	}
	if err := w.enterWrite(nil); err != nil {
		return 0, err
	}
	defer w.leaveWrite()
	var total int64
	if isFile {
		limit := int64(-1)
		if limited {
			limit = lr.N
		}
		if rng, ok := filerange.Span(f, limit); ok {
			n, err := w.sendFileLocked(nil, f, rng)
			rng.Finish(f, n)
			total = n
			if limited {
				lr.N -= n
			}
			if err != nil {
				return total, err
			}
		}
	}
	n, err := w.copyLocked(nil, r)
	return total + n, err
}

// osFile returns the file r reads: r itself, or the file of the wrapper
// os.File.WriteTo hands io.Copy, which embeds it without its WriteTo.
func osFile(r io.Reader) (*os.File, bool) {
	if f, ok := r.(*os.File); ok {
		return f, f != nil
	}
	t := reflect.TypeOf(r)
	if t == nil || t.Kind() != reflect.Struct || t.PkgPath() != "os" || t.Name() != "fileWithoutWriteTo" {
		return nil, false
	}
	field := reflect.ValueOf(r).FieldByName("File")
	if !field.IsValid() || !field.CanInterface() {
		return nil, false
	}
	f, ok := field.Interface().(*os.File)
	return f, ok && f != nil
}

// copyLocked copies r into the connection through pooled buffers until r
// ends, waiting for the window as Write does. Its caller holds wsem.
func (w *Writer) copyLocked(ctx context.Context, r io.Reader) (int64, error) {
	var total int64
	for {
		if err := w.waitErr(ctx); err != nil {
			return total, err
		}
		chunkCap := min(readFromChunkSize, max(w.window(), 1))
		buf := bytebuf.AcquireBuffer(chunkCap)
		nr, rerr := r.Read(buf.AvailableBuffer()[:chunkCap])
		if nr > 0 {
			buf.CommitWrite(nr)
			nw, retained, werr := w.writeOwnedPreserveLocked(ctx, buf)
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

// waitErr reports what ends a wait: a closed connection, ctx, or the write
// deadline.
func (w *Writer) waitErr(ctx context.Context) error {
	if err := w.terminalErr(); err != nil {
		return err
	}
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	return w.waiter.checkDeadline()
}

// SendFile transmits count bytes of file starting at offset over w, suspending
// while the outbound window is full. If offset < 0, SendFile starts at file's
// current seek position and advances it by the number of bytes sent. If
// count < 0, bytes through the end of file are sent, as uio.SendFile defines
// the end of a file.
func (w *Writer) SendFile(file *os.File, offset, count int64) (int64, error) {
	return w.SendFileContext(context.Background(), file, offset, count)
}

// SendFileContext is SendFile honoring ctx: the transfer waits for the window,
// and returns once what is left of it fits there, as a Write of its last
// bytes would. Cut short by ctx, the write deadline or a closing connection,
// it reports the bytes that were sent or are being sent; on a native
// connection the rest of the range is withdrawn and never sent.
func (w *Writer) SendFileContext(ctx context.Context, file *os.File, offset, count int64) (int64, error) {
	if OnTurn() {
		return 0, ErrBlockingOnTurn
	}
	if err := w.enterWrite(ctx); err != nil {
		return 0, err
	}
	defer w.leaveWrite()
	if err := w.waitErr(ctx); err != nil {
		return 0, err
	}
	rng, err := filerange.Resolve(file, offset, count)
	if err != nil || rng.Len == 0 {
		return 0, err
	}
	n, err := w.sendFileLocked(ctx, file, rng)
	rng.Finish(file, n)
	return n, err
}

// sendFileLocked sends rng of file. A native range holds no memory, so it is
// queued whole once the window has room, and the call then waits until what
// is left of it fits in the window. Cut short, the call withdraws what of the
// range is still queued, so its count is what was sent or is being sent.
// Without sendfile(2) the range is read through pooled buffers. Its caller
// holds wsem.
func (w *Writer) sendFileLocked(ctx context.Context, file *os.File, rng filerange.Range) (int64, error) {
	fs, ok := w.src.(FileSender)
	if !ok || !rng.Native || !fs.HasNativeSendFile() {
		// Without sendfile(2) the range is read whole, to the end of a file
		// whose length is unknown, through the window.
		if rng.Len < 0 {
			file.Seek(rng.Start, io.SeekStart)
			return w.copyLocked(ctx, file)
		}
		n, err := w.copyLocked(ctx, io.NewSectionReader(file, rng.Start, rng.Len))
		if err == nil && n < rng.Len {
			err = io.ErrUnexpectedEOF
		}
		return n, err
	}
	owner := w.fileSends.Add(1)
	var total int64
	for total < rng.Len {
		tok := w.waiter.prepare()
		if err := w.waitErr(ctx); err != nil {
			return total, err
		}
		progressed := false
		if w.src.OutboundBuffered() < max(w.window(), 1) {
			part := rng
			part.Start, part.Len = rng.Start+total, rng.Len-total
			n, err := fs.SendFile(file, part, owner)
			total += n
			progressed = n > 0
			if err != nil && !w.src.IsOverflow(err) {
				return total, w.resolveWriteErr(err)
			}
			if progressed {
				continue
			}
		}
		if !progressed {
			if err := w.waiter.waitContext(ctx, tok); err != nil {
				return total, err
			}
		}
	}
	for {
		tok := w.waiter.prepare()
		err := w.waitErr(ctx)
		if err == nil {
			if w.src.OutboundBuffered() <= max(w.window(), 1) {
				return total, nil
			}
			err = w.waiter.waitContext(ctx, tok)
		}
		if err != nil {
			withdrawn := fs.CancelQueuedFiles(owner)
			total -= withdrawn
			if withdrawn == 0 && w.terminalErr() == nil {
				// Nothing of the range waits any more: it was sent, or is being.
				return total, nil
			}
			return total, err
		}
	}
}
