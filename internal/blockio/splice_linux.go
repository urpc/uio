//go:build linux && !stdio

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
	"os"
	"sync/atomic"
	"syscall"

	"github.com/urpc/uio/internal/bytebuf"
	"github.com/urpc/uio/internal/socket"
)

// testHookSpliceFilled, when set, runs as a splice session has moved a batch
// into its pipe and is about to forward it.
var testHookSpliceFilled atomic.Pointer[func()]

const (
	// spliceBatchMax is the most one batch moves through the pipe.
	spliceBatchMax = 256 << 10
	// spliceMinCount is the shortest bounded transfer worth a splice: taking
	// the socket over costs the connection two turns.
	spliceMinCount = 64 << 10
)

func isWouldBlock(err error) bool {
	return err == syscall.EAGAIN || err == syscall.EWOULDBLOCK
}

// spliceToTarget hands count bytes of the pipe pipeR to target, the connection
// w writes, once its queue is empty, so they leave behind it and may go
// straight from the pipe. taken reports that the connection took the bytes
// from the pipe — sent, queued or, when it failed, dropped; until then they
// stay in the pipe for the caller.
func (w *Writer) spliceToTarget(ctx context.Context, target SpliceTarget, pipeR int, count int) (int, bool, error) {
	if err := w.enterWrite(ctx); err != nil {
		return 0, false, err
	}
	defer w.leaveWrite()
	if w.src.OutboundBuffered() > 0 {
		_ = w.src.Flush()
	}
	for {
		tok := w.waiter.prepare()
		if err := w.waitErr(ctx); err != nil {
			return 0, false, err
		}
		if w.src.OutboundBuffered() == 0 {
			break
		}
		if err := w.waiter.waitContext(ctx, tok); err != nil {
			return 0, false, err
		}
	}
	n, err := target.SpliceFromPipe(pipeR, count)
	if err != nil {
		return n, true, w.resolveWriteErr(err)
	}
	return n, true, nil
}

func (r *Reader) trySpliceNative(
	ctx context.Context,
	w io.Writer,
	dstWriter *Writer,
	scratch *[]byte,
	count int64,
	total int64,
) (int64, bool, error) {
	ss, ok := r.src.(SpliceSource)
	if !ok || (count >= 0 && count-total < spliceMinCount) {
		return total, true, nil
	}
	s := spliceSession{r: r, ctx: ctx, src: ss, w: w, dst: dstWriter, scratch: scratch, count: count, total: total}
	if dstWriter != nil {
		if s.target, ok = dstWriter.src.(SpliceTarget); !ok {
			return total, true, nil
		}
	} else {
		f, ok := w.(*os.File)
		if !ok {
			return total, true, nil
		}
		rc, err := f.SyscallConn()
		if err != nil {
			return total, true, nil
		}
		s.file, s.fileConn = f, rc
	}
	return s.run()
}

// spliceSession is one SpliceTo moving a socket's input into a native
// connection or a file with splice(2), through a kernel pipe.
type spliceSession struct {
	r       *Reader
	ctx     context.Context
	src     SpliceSource
	w       io.Writer
	scratch *[]byte
	count   int64
	total   int64

	dst      *Writer // the native connection's writer, with target
	target   SpliceTarget
	file     *os.File // or a file, written within fileConn's Control
	fileConn syscall.RawConn

	pipe   *socket.SplicePipe
	inPipe int // bytes in pipe not yet forwarded
}

// run asks the source for its socket, forwards in order what the source read
// before it granted it, then moves the socket's bytes through the pipe until
// the transfer is done. Once granted, nothing reaches the Reader's pipe any
// more. It reports true when the caller should go on with user-space copies:
// the stream ended, which the connection's turns take up, or splice(2)
// cannot serve the descriptors.
func (s *spliceSession) run() (int64, bool, error) {
	r := s.r
	r.spliceActive.Store(true)
	if !s.src.RequestSplice() {
		r.spliceActive.Store(false)
		return s.total, true, nil
	}
	defer s.releasePipe()
	defer func() {
		r.spliceActive.Store(false)
		s.src.EndSplice()
	}()
	granted := false
	for s.count < 0 || s.total < s.count {
		tok := r.waiter.prepare()
		total, done, err := r.drainPipeTo(s.ctx, s.w, s.dst, s.scratch, s.count, s.total)
		s.total = total
		if err != nil || done {
			return s.total, false, err
		}
		r.mu.Lock()
		pipeEmpty, peerEOF, closed, closeErr := r.pipe.Empty(), r.peerEOF, r.closed, r.closeErr
		r.mu.Unlock()
		switch {
		case !pipeEmpty:
			// A Pump added bytes since the drain looked.
			continue
		case peerEOF:
			return spliceEOF(s.total, s.count)
		case closed:
			return s.total, false, closeErr
		}
		if !granted {
			var ended bool
			if granted, ended = s.src.SpliceGranted(); !granted {
				if ended {
					// No grant comes: the turns deliver the rest.
					return s.total, true, nil
				}
				if err := r.waiter.waitContext(s.ctx, tok); err != nil {
					return s.total, false, err
				}
			}
			// The round that granted the socket handed what it held to the
			// Reader's pipe after the drain above: that goes first.
			continue
		}
		if s.pipe == nil {
			pipe, err := socket.AcquireSplicePipe()
			if err != nil {
				return s.total, true, nil
			}
			s.pipe = pipe
		}
		eof, rerr := s.fill()
		if s.inPipe > 0 {
			if hook := testHookSpliceFilled.Load(); hook != nil {
				(*hook)()
			}
			fallback, err := s.forward()
			if err != nil || fallback {
				return s.total, fallback, err
			}
			// EAGAIN may only mean the pipe filled: read on until the socket
			// itself has nothing.
			if !eof && (rerr == nil || isWouldBlock(rerr)) {
				continue
			}
		}
		if eof {
			// The connection's turns read the end of the stream again once
			// the socket is theirs, and pass it on.
			return s.total, true, nil
		}
		if rerr != nil && !isWouldBlock(rerr) && !errors.Is(rerr, net.ErrClosed) {
			if socket.IsSpliceUnsupported(rerr) {
				return s.total, true, nil
			}
			return s.total, false, rerr
		}
		// Drained, or the socket was closed and its notice is on its way:
		// either wakes this wait, which holds no pipe.
		s.releasePipe()
		if err := r.waiter.waitContext(s.ctx, tok); err != nil {
			return s.total, false, err
		}
	}
	return s.total, false, nil
}

// fill moves socket bytes into the pipe, one batch at most: no more than the
// destination's window, so what it cannot send at once queues within it.
func (s *spliceSession) fill() (eof bool, err error) {
	batch := spliceBatchMax
	if s.dst != nil {
		batch = min(batch, max(s.dst.window(), 1))
	}
	if s.count >= 0 {
		batch = min(batch, int(s.count-s.total))
	}
	for s.inPipe < batch {
		n, err := s.src.SpliceIn(s.pipe.W, batch-s.inPipe)
		s.inPipe += n
		if err != nil {
			return false, err
		}
		if n == 0 {
			return true, nil
		}
	}
	return false, nil
}

// forward sends the pipe's bytes on to the destination. fallback reports a
// file splice(2) cannot write, which took the rest with write(2).
func (s *spliceSession) forward() (fallback bool, err error) {
	if s.target != nil {
		n, taken, err := s.dst.spliceToTarget(s.ctx, s.target, s.pipe.R, s.inPipe)
		s.total += int64(n)
		if taken {
			s.inPipe = 0
		}
		return false, err
	}
	n, fallback, err := splicePipeToFile(s.pipe.R, s.file, s.fileConn, s.inPipe)
	s.inPipe = 0
	s.total += int64(n)
	return fallback, err
}

// releasePipe returns the pipe, handing what it still holds back to the
// Reader first: a pipe that may hold bytes still is closed, not pooled.
func (s *spliceSession) releasePipe() {
	if s.pipe == nil {
		return
	}
	if s.inPipe > 0 && !s.r.rescuePipeBytes(s.pipe.R, s.inPipe) {
		socket.CloseSplicePipe(s.pipe)
	} else {
		socket.ReleaseSplicePipe(s.pipe)
	}
	s.inPipe = 0
	s.pipe = nil
}

// spliceEOF reports an end of stream that came after total bytes: a bounded
// transfer it cut short fails as io.ReadFull does.
func spliceEOF(total, count int64) (int64, bool, error) {
	if count >= 0 && total < count {
		if total == 0 {
			return 0, false, io.EOF
		}
		return total, false, io.ErrUnexpectedEOF
	}
	return total, false, nil
}

// rescuePipeBytes puts the count bytes a pipe still holds back at the head of
// the Reader's pipe, ahead of anything a turn pumped since: they came first.
// It reports whether it took them all.
func (r *Reader) rescuePipeBytes(pipeR int, count int) bool {
	buf := bytebuf.AcquireBuffer(count)
	dst := buf.AvailableBuffer()[:count]
	nr, err := socket.ReadSplicePipe(pipeR, dst)
	if nr <= 0 {
		bytebuf.ReleaseBuffer(buf)
		return false
	}
	buf.CommitWrite(nr)
	r.mu.Lock()
	if !r.closed || !r.localAbort {
		r.pipe.PrependOwned(buf)
	} else {
		bytebuf.ReleaseBuffer(buf)
	}
	r.mu.Unlock()
	return err == nil && nr == count
}

// splicePipeToFile moves count bytes from the pipe into dstFile, inside
// Control so the file's descriptor cannot close under the syscalls. A file
// splice(2) cannot write takes the rest with write(2), and reports true: the
// caller goes on in user space. Whatever it returns, the pipe is empty.
func splicePipeToFile(pipeR int, dstFile *os.File, dstConn syscall.RawConn, count int) (int, bool, error) {
	rem := count
	written := 0
	userspace := false
	var serr error
	if cerr := dstConn.Control(func(fd uintptr) {
		for rem > 0 {
			n, err := socket.Splice(pipeR, nil, int(fd), nil, rem)
			if n > 0 {
				written += n
				rem -= n
			}
			if n == 0 || socket.IsSpliceUnsupported(err) || isWouldBlock(err) {
				userspace = true
				return
			}
			if err != nil {
				serr = err
				return
			}
		}
	}); cerr != nil {
		serr = cerr
	}
	if serr != nil {
		_, _ = drainPipeToWriter(pipeR, io.Discard, rem)
		return written, false, serr
	}
	if userspace && rem > 0 {
		nw, ferr := drainPipeToWriter(pipeR, dstFile, rem)
		return written + nw, ferr == nil, ferr
	}
	return written, false, nil
}

func drainPipeToWriter(pipeR int, w io.Writer, count int) (int, error) {
	if count <= 0 {
		return 0, nil
	}
	buf := bytebuf.AcquireBuffer(count)
	defer bytebuf.ReleaseBuffer(buf)
	dst := buf.AvailableBuffer()[:count]
	readTotal, err := socket.ReadSplicePipe(pipeR, dst)
	if err != nil {
		return 0, err
	}
	nw, werr := w.Write(dst[:readTotal])
	if werr == nil && nw != readTotal {
		werr = io.ErrShortWrite
	}
	return nw, werr
}
