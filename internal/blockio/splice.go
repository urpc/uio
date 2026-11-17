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
	"os"

	"github.com/urpc/uio/internal/bytebuf"
)

// spliceMinCount is the shortest bounded transfer worth a splice: taking the
// socket over costs the connection two turns.
const spliceMinCount = 64 << 10

// SpliceTransfer is an optional capability of native stream sources that move
// their socket's input into a native destination with splice(2). The source
// owns the whole transfer — the request/grant handshake, the kernel pipe, the
// syscalls and the hand-back of the socket — while the Reader supplies, through
// SpliceHooks, what only it knows: how to park, and how to drain and rescue its
// own buffered bytes. A source without the capability leaves SpliceTo to copy.
type SpliceTransfer interface {
	TransferSplice(req SpliceRequest) (n int64, fallback bool, err error)
}

// SpliceRequest is one blocking splice transfer. Exactly one of Sink and File
// is set: a native connection's blocking writer, or a file to splice into. The
// transfer's context is honored through the Hooks, which capture it.
type SpliceRequest struct {
	Sink  SpliceSink
	File  *os.File
	Count int64 // bytes to move, <0 for the whole stream
	Total int64 // bytes the caller already delivered
	Hooks SpliceHooks
}

// SpliceSink is a native connection's blocking writer as a splice destination.
type SpliceSink interface {
	// SpliceFromPipe moves up to count bytes of the pipe into the connection,
	// waiting for its queue to empty first so they leave behind what it holds.
	// taken reports the bytes left the pipe — sent, queued, or dropped on
	// failure; until then they stay in the pipe for the caller.
	SpliceFromPipe(pipeR, count int) (n int, taken bool, err error)
	// Window is the most the connection takes at once, 0 for no bound.
	Window() int
}

// SpliceHooks are what the Reader supplies to the transfer engine.
type SpliceHooks struct {
	// Prepare snapshots the Reader's wait generation; Wait parks until it
	// advances, the read deadline expires, the transfer's context ends, or the
	// Reader closes.
	Prepare func() uint64
	Wait    func(tok uint64) error
	// Drain hands the Reader's buffered bytes to the destination ahead of
	// anything the splice moves, from the running total, and reports the
	// stream's state: more when the pipe holds bytes still (drain again before
	// anything else), ended when the peer's stream ended with the pipe empty,
	// and err when the transfer is over for the context, the deadline, a close,
	// or a write error.
	Drain func(total int64) (n int64, more, ended bool, err error)
	// Rescue puts bytes the pipe still holds back at the head of the Reader's
	// pipe, ahead of anything a turn pumped since: they came first. It takes
	// ownership of buf.
	Rescue func(buf *bytebuf.Buffer)
}

// trySpliceNative hands the transfer to the source's splice engine when it has
// one and the destination can take a splice; otherwise it reports fallback so
// SpliceTo copies in user space.
func (r *Reader) trySpliceNative(
	ctx context.Context,
	w io.Writer,
	dstWriter *Writer,
	scratch *[]byte,
	count int64,
	total int64,
) (int64, bool, error) {
	st, ok := r.src.(SpliceTransfer)
	if !ok || (count >= 0 && count-total < spliceMinCount) {
		return total, true, nil
	}
	req := SpliceRequest{Count: count, Total: total}
	if dstWriter != nil {
		target, ok := dstWriter.src.(SpliceTarget)
		if !ok {
			return total, true, nil
		}
		req.Sink = &writerSink{w: dstWriter, target: target, ctx: ctx}
	} else {
		f, ok := w.(*os.File)
		if !ok {
			return total, true, nil
		}
		req.File = f
	}
	req.Hooks = SpliceHooks{
		Prepare: r.waiter.prepare,
		Wait:    func(tok uint64) error { return r.waiter.waitContext(ctx, tok) },
		Drain: func(total int64) (int64, bool, bool, error) {
			return r.drainForSplice(ctx, w, dstWriter, scratch, count, total)
		},
		Rescue: r.rescueToPipe,
	}
	// While the splice owns the socket, a Pump only notifies; the engine drains
	// what the granting round hands over before it moves anything itself. The
	// reset is deferred so a panic upstream cannot leave the Reader splice-owned.
	r.spliceActive.Store(true)
	defer r.spliceActive.Store(false)
	return st.TransferSplice(req)
}

// drainForSplice drains the Reader's pipe to the destination and reports the
// stream's state for the splice engine; see SpliceHooks.Drain.
func (r *Reader) drainForSplice(
	ctx context.Context,
	w io.Writer,
	dstWriter *Writer,
	scratch *[]byte,
	count int64,
	total int64,
) (int64, bool, bool, error) {
	total, done, err := r.drainPipeTo(ctx, w, dstWriter, scratch, count, total)
	if err != nil || done {
		return total, false, false, err
	}
	r.mu.Lock()
	pipeEmpty, peerEOF, closed, closeErr := r.pipe.Empty(), r.peerEOF, r.closed, r.closeErr
	r.mu.Unlock()
	switch {
	case !pipeEmpty:
		return total, true, false, nil
	case peerEOF:
		return total, false, true, nil
	case closed:
		return total, false, false, closeErr
	}
	return total, false, false, nil
}

// rescueToPipe puts bytes a splice's pipe still holds back at the head of the
// Reader's pipe, ahead of anything a turn pumped since: they came first.
func (r *Reader) rescueToPipe(buf *bytebuf.Buffer) {
	r.mu.Lock()
	if !r.closed || !r.localAbort {
		r.pipe.PrependOwned(buf)
	} else {
		bytebuf.ReleaseBuffer(buf)
	}
	r.mu.Unlock()
}

// writerSink adapts a native connection's blocking Writer to SpliceSink.
type writerSink struct {
	w      *Writer
	target SpliceTarget
	ctx    context.Context
}

func (s *writerSink) Window() int { return s.w.window() }

func (s *writerSink) SpliceFromPipe(pipeR, count int) (int, bool, error) {
	return s.w.spliceToTarget(s.ctx, s.target, pipeR, count)
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
