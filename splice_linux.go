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

package uio

import (
	"errors"
	"io"
	"net"
	"os"
	"runtime"
	"sync/atomic"
	"syscall"

	"github.com/urpc/uio/internal/blockio"
	"github.com/urpc/uio/internal/bytebuf"
	"github.com/urpc/uio/internal/relay"
	"github.com/urpc/uio/internal/socket"
)

// Values of zeroCopyState.spliceMode, who owns the socket's input. Setting a
// mode other than none sets spliceReadFlag, which routes the connection's
// read rounds through readRoundSplice; only a read round clears the flag,
// once it finds the mode none.
const (
	spliceModeNone uint32 = iota
	// spliceModeRequested: a blocking SpliceTo asked for the socket's input.
	// The turns read the socket no more; the next read round hands what the
	// connection still holds to the reader and grants the request.
	spliceModeRequested
	// spliceModeGranted: the SpliceTo goroutine reads the socket with
	// splice(2) until it ends the request.
	spliceModeGranted
	// spliceModeEnded: the SpliceTo gave the socket back. The next read round
	// reports what it read and reads the socket again.
	spliceModeEnded
	// spliceModeHandler: the Handler relays the socket's input with
	// relay.TrySplice on its turns.
	spliceModeHandler
)

const (
	// spliceStepMax is the most one relay batch moves through its pipe.
	spliceStepMax = 256 << 10
	// relayRoundMax and relayRoundCalls bound what one relay moves on a turn.
	relayRoundMax   = 1 << 20
	relayRoundCalls = 64
)

// testHookSpliceRead, when set, sees the bytes each splice(2) moved from a
// connection's socket into a pipe.
var testHookSpliceRead atomic.Pointer[func(*fdConn, int)]

func (s fdBlockSource) TransferSplice(req blockio.SpliceRequest) (int64, bool, error) {
	return s.conn.transferSplice(req)
}
func (s fdBlockSource) SpliceFromPipe(p, n int) (int, error) { return s.conn.spliceFromPipe(p, n) }

// testHookSpliceFilled, when set, runs as a blocking splice has moved a batch
// into its pipe and is about to forward it.
var testHookSpliceFilled atomic.Pointer[func()]

// readRoundSplice is the read round of a connection whose spliceReadFlag is
// set. It reports whether the round is over.
func (conn *fdConn) readRoundSplice(zc *zeroCopyState) (bool, error) {
	for {
		switch mode := zc.spliceMode.Load(); mode {
		case spliceModeRequested, spliceModeGranted:
			conn.reportUnread(zc)
			if conn.turn&turnHangup != 0 {
				conn.turn |= turnReadDrained
			}
			// Bytes an earlier round read go to the reader first; the socket
			// is granted only once none are left, so the splice never moves
			// bytes ahead of them. A round of this mode reads nothing, so
			// none are added after the grant.
			started := conn.beginInboundCallback()
			conn.pumpBlockReader()
			if conn.inbound.Empty() && len(conn.inboundTail) == 0 &&
				zc.spliceMode.CompareAndSwap(spliceModeRequested, spliceModeGranted) {
				conn.notifyBlockReader()
			}
			conn.endInboundCallback(started)
			return true, nil
		case spliceModeHandler:
			if !conn.inbound.Empty() || len(conn.inboundTail) > 0 || conn.isClosing() {
				return false, nil
			}
			err := conn.fireOnRead()
			relayed := conn.turn&turnRelayed != 0
			conn.turn &^= turnRelayed
			return err != nil || conn.isClosing() || conn.ReadPaused() || relayed, err
		case spliceModeEnded, spliceModeNone:
			if mode == spliceModeEnded && !zc.spliceMode.CompareAndSwap(spliceModeEnded, spliceModeNone) {
				continue
			}
			// The socket is the turns' again: this round reads it.
			conn.reportUnread(zc)
			conn.flags.And(^spliceReadFlag)
			if zc.spliceMode.Load() == spliceModeNone {
				return false, nil
			}
			// A request came in meanwhile; it keeps the flag.
			conn.flags.Or(spliceReadFlag)
		default:
			return false, nil
		}
	}
}

// requestSplice asks the connection's turns to leave the socket's input to a
// blocking SpliceTo, and starts the read round that grants it; spliceGranted
// reports when one did. It refuses a connection already spliced either way.
func (conn *fdConn) requestSplice() bool {
	if conn == nil || conn.isDatagram() || conn.internal || conn.isClosing() || conn.loop == nil {
		return false
	}
	zc := conn.zeroCopyState()
	for {
		mode := zc.spliceMode.Load()
		if mode != spliceModeNone && mode != spliceModeEnded {
			return false
		}
		if zc.spliceMode.CompareAndSwap(mode, spliceModeRequested) {
			break
		}
	}
	// After the mode: a round that sees the flag finds the request.
	conn.flags.Or(spliceReadFlag)
	conn.scheduleIO(ioEventRead)
	return true
}

// spliceGranted reports whether a turn granted the socket to the SpliceTo
// goroutine, and whether the input ended or the connection is closing, in
// which case no grant comes and the turns deliver the rest.
func (conn *fdConn) spliceGranted() (granted, ended bool) {
	if zc := conn.loadZeroCopy(); zc != nil && zc.spliceMode.Load() == spliceModeGranted {
		return true, false
	}
	return false, conn.readEOF() || conn.isClosing()
}

// spliceIn moves up to n bytes from the socket into the pipe pipeW for the
// granted SpliceTo. The descriptor gate holds the socket's close off while the
// syscall runs, so its number is never reused under it; once a close retired
// the gate, it reports net.ErrClosed. What it moves is reported to
// OnBytesRead by the next read round. A failed socket closes the connection
// with the error the splice took from it.
func (conn *fdConn) spliceIn(pipeW, n int) (int, error) {
	zc := conn.loadZeroCopy()
	if zc == nil || zc.spliceMode.Load() != spliceModeGranted || !zc.spliceGate.enter() {
		return 0, net.ErrClosed
	}
	moved, err := socket.Splice(int(conn.fd), nil, pipeW, nil, n)
	if moved > 0 {
		if hook := testHookSpliceRead.Load(); hook != nil {
			(*hook)(conn, moved)
		}
		if conn.callbacks().onBytesRead != nil {
			// Counted inside the gate, so a close retires the gate only
			// after the bytes it reports are counted.
			zc.unread.Add(int64(moved))
		}
	}
	zc.spliceGate.leave()
	if err != nil && !isWouldBlock(err) && !socket.IsSpliceUnsupported(err) {
		conn.requestClose(err)
	}
	return moved, err
}

// endSplice gives the socket's input back to the connection's turns: the next
// read round reports what the splice read and reads the socket again,
// including bytes that came while the splice owned it, whose edges raised
// rounds that read nothing.
func (conn *fdConn) endSplice() {
	zc := conn.loadZeroCopy()
	if zc == nil {
		return
	}
	for {
		mode := zc.spliceMode.Load()
		if mode != spliceModeRequested && mode != spliceModeGranted {
			break
		}
		if zc.spliceMode.CompareAndSwap(mode, spliceModeEnded) {
			break
		}
	}
	if !conn.isClosing() && conn.loop != nil {
		conn.scheduleIO(ioEventRead)
	}
}

// notifyBlockReader wakes the blocking reader to look at the connection again.
func (conn *fdConn) notifyBlockReader() {
	if w := conn.wake.Load(); w != nil {
		if st := w.block.Load(); st != nil {
			if r := st.ActiveReader(); r != nil {
				r.Notify()
			}
		}
	}
}

// spliceFromPipe sends count bytes the pipe pipeR holds through the socket:
// with splice(2) straight from the pipe when nothing is queued, and what the
// socket does not take read from the pipe and queued — at the head, ahead of
// what others queued meanwhile, when the send began on an empty queue. It
// runs outside the connection's turns, so what it sends is reported by a
// write turn (see finishDirectSend). It takes all count bytes from the pipe,
// whatever it returns: a failed connection drops them.
func (conn *fdConn) spliceFromPipe(pipeR int, count int) (int, error) {
	if conn == nil || conn.isDatagram() || count <= 0 {
		return 0, ErrUnsupported
	}
	if conn.outputClosed() {
		drainPipeDiscard(pipeR, count)
		return 0, net.ErrClosed
	}
	if conn.writeState.Load()&writeOpenedFlag == 0 || !conn.outboundEmpty() || conn.writeBlocked() ||
		!conn.claimDirectSend() {
		return conn.queuePipe(pipeR, count)
	}
	sent := 0
	blocked := false
	var serr error
	for sent < count {
		n, err := socket.Splice(pipeR, nil, int(conn.fd), nil, count-sent)
		sent += n
		if err != nil {
			if isWouldBlock(err) {
				blocked = true
			} else if !socket.IsSpliceUnsupported(err) {
				serr = err
			}
			break
		}
		if n == 0 {
			break
		}
	}
	if serr == nil && sent < count {
		serr = conn.queuePipeFirst(pipeR, count-sent)
		if serr == nil && blocked {
			conn.markWriteBlocked()
		}
	}
	if serr != nil {
		drainPipeDiscard(pipeR, count-sent)
		if sent > 0 || serr != net.ErrClosed {
			conn.failDirectWrite(serr)
		}
	}
	conn.finishDirectSend(sent)
	if serr != nil {
		return sent, serr
	}
	return count, nil
}

// queuePipeFirst queues the rest of a pipe a direct splice began, ahead of
// what others queued while it held the claim; a connection draining a close
// still takes it, as it does a sendfile range's rest.
func (conn *fdConn) queuePipeFirst(pipeR int, rest int) error {
	buf, err := readPipeIntoBuffer(pipeR, rest)
	if err != nil {
		return err
	}
	conn.submitMu.Lock()
	if !conn.acceptsDirectRestLocked() {
		conn.submitMu.Unlock()
		bytebuf.ReleaseBuffer(buf)
		return net.ErrClosed
	}
	conn.pending.Add(int64(buf.Len()))
	conn.outbound.PrependOwned(buf)
	conn.submitMu.Unlock()
	return nil
}

// queuePipe queues what the pipe holds behind everything accepted before it.
func (conn *fdConn) queuePipe(pipeR int, count int) (int, error) {
	buf, err := readPipeIntoBuffer(pipeR, count)
	if err != nil {
		return 0, err
	}
	conn.submitMu.Lock()
	if conn.refusesQueueLocked() {
		conn.submitMu.Unlock()
		bytebuf.ReleaseBuffer(buf)
		return 0, net.ErrClosed
	}
	conn.pending.Add(int64(buf.Len()))
	conn.outboundTargetLocked().AppendOwned(buf)
	conn.submitMu.Unlock()
	if conn.kickWriter() && !blockio.OnTurn() {
		runtime.Gosched()
	}
	return count, nil
}

func readPipeIntoBuffer(pipeR int, count int) (*bytebuf.Buffer, error) {
	buf := bytebuf.AcquireBuffer(count)
	dst := buf.AvailableBuffer()[:count]
	readTotal, err := socket.ReadSplicePipe(pipeR, dst)
	if err != nil {
		bytebuf.ReleaseBuffer(buf)
		return nil, err
	}
	buf.CommitWrite(readTotal)
	return buf, nil
}

func drainPipeDiscard(pipeR int, count int) {
	if count <= 0 {
		return
	}
	buf, err := readPipeIntoBuffer(pipeR, count)
	if err == nil && buf != nil {
		bytebuf.ReleaseBuffer(buf)
	}
}

func resolveNativeSpliceConn(c any) (*fdConn, bool) {
	cur, ok := c.(Lower)
	if !ok {
		return nil, false
	}
	for cur != nil {
		if fdc, ok := cur.(*fdConn); ok {
			if fdc == nil || fdc.isDatagram() || fdc.internal {
				return nil, false
			}
			return fdc, true
		}
		if _, isLayer := cur.(*layerNode); isLayer {
			return nil, false
		}
		cur = Unwrap(cur)
	}
	return nil, false
}

// splicer is the connection's relay; see internal/relay.
func (conn *fdConn) splicer() relay.Splicer {
	if conn == nil || conn.isDatagram() || conn.internal {
		return nil
	}
	return fdRelay{conn}
}

// fdRelay relays a Linux native stream connection's input with splice(2).
type fdRelay struct{ conn *fdConn }

// EnableSplice leaves a connection a blocking SpliceTo owns alone. Switched
// off, the relay hands the socket back at the next read round.
func (r fdRelay) EnableSplice(enabled bool) bool {
	conn := r.conn
	zc := conn.zeroCopyState()
	if !enabled {
		zc.spliceMode.CompareAndSwap(spliceModeHandler, spliceModeNone)
		return zc.spliceMode.Load() != spliceModeHandler
	}
	if zc.spliceMode.CompareAndSwap(spliceModeNone, spliceModeHandler) {
		conn.flags.Or(spliceReadFlag)
		return true
	}
	return zc.spliceMode.Load() == spliceModeHandler
}

// TrySpliceTo moves the socket's input into dst's socket on the connection's
// own turn, while dst has nothing queued; see relay.Splicer.
func (r fdRelay) TrySpliceTo(dst any, maxBytes int) (int, bool, error) {
	conn := r.conn
	if maxBytes <= 0 {
		return 0, false, nil
	}
	dstConn, ok := resolveNativeSpliceConn(dst)
	if !ok || conn == dstConn {
		return 0, false, nil
	}
	if !conn.directOwner() || !conn.inbound.Empty() || len(conn.inboundTail) > 0 {
		return 0, false, nil
	}
	if conn.readEOF() {
		// The end of the stream was read already: nothing is left to move.
		return 0, true, nil
	}
	if !dstConn.outboundEmpty() || dstConn.writeBlocked() || dstConn.outputClosed() ||
		dstConn.writeState.Load()&(writeOpenedFlag|writeOwnerFlag) != writeOpenedFlag {
		return 0, false, nil
	}
	limit := min(maxBytes, relayRoundMax)
	if dstLim := dstConn.payloadLimit(); dstLim > 0 {
		room := dstLim - dstConn.OutboundBuffered()
		if room <= 0 {
			return 0, false, nil
		}
		limit = min(limit, room)
	}
	pipe, perr := socket.AcquireSplicePipe()
	if perr != nil {
		return 0, false, nil
	}
	moved := 0
	drained, eof := false, false
	var retErr error
	for calls := 0; calls < relayRoundCalls && moved < limit && !drained && !eof && retErr == nil; calls++ {
		inPipe := 0
		for inPipe < spliceStepMax && moved < limit {
			n, rerr := socket.Splice(int(conn.fd), nil, pipe.W, nil, min(limit-moved, spliceStepMax-inPipe))
			if n > 0 {
				inPipe += n
				moved += n
				if hook := testHookSpliceRead.Load(); hook != nil {
					(*hook)(conn, n)
				}
			}
			if rerr != nil {
				switch {
				case isWouldBlock(rerr) && inPipe > 0:
					// A full pipe refuses as an empty socket does: forward
					// what it holds and look again.
				case isWouldBlock(rerr):
					conn.turn = conn.turn&^turnReadToEnd | turnReadDrained
					drained = true
				case socket.IsSpliceUnsupported(rerr) && moved == 0:
					socket.ReleaseSplicePipe(pipe)
					return 0, false, nil
				default:
					conn.turn |= turnReadDrained
					drained = true
					retErr = rerr
				}
				break
			}
			if n == 0 {
				// The end of the stream: the round reads it again and passes
				// it on, as any end it reads.
				eof = true
				break
			}
		}
		if inPipe > 0 {
			conn.events().onSocketBytesRead(conn, inPipe)
			if _, werr := dstConn.spliceFromPipe(pipe.R, inPipe); werr != nil && retErr == nil {
				retErr = werr
			}
		}
		if !dstConn.outboundEmpty() || dstConn.writeBlocked() {
			break
		}
	}
	if retErr != nil {
		// A failed send may have left bytes in the pipe.
		socket.CloseSplicePipe(pipe)
	} else {
		socket.ReleaseSplicePipe(pipe)
	}
	handled := moved > 0 || drained
	if handled && !eof {
		// The round ends here; one that stopped short of the socket's end
		// goes on in the next.
		conn.turn |= turnRelayed
		if !drained && !conn.isClosing() && !conn.ReadPaused() && !conn.readHeld() {
			conn.taskState.Or(ioEventRead)
		}
	}
	return moved, handled, retErr
}

// closeSpliceGate stops the splice(2) calls of a blocking SpliceTo and waits
// for the one in progress, which counts what it reads before it leaves.
func (zc *zeroCopyState) closeSpliceGate() {
	if zc.spliceMode.Load() == spliceModeGranted {
		zc.spliceGate.markClosed()
		zc.spliceGate.waitDrain(0)
	}
}

// transferSplice moves the socket's input into req's destination with
// splice(2). It owns the whole transfer — the request/grant handshake, the
// kernel pipe, the syscalls, the byte accounting and the hand-back of the
// socket — while the Reader supplies, through req.Hooks, what only it knows:
// how to park, and how to drain and rescue its own buffered bytes. It reports
// fallback when the caller should go on with user-space copies: the stream
// ended, which the connection's turns take up, or splice(2) cannot serve the
// descriptors.
func (conn *fdConn) transferSplice(req blockio.SpliceRequest) (int64, bool, error) {
	// Exactly one destination: a connection sink or a file. Anything else is
	// not a native splice; the caller copies.
	if (req.Sink == nil) == (req.File == nil) {
		return req.Total, true, nil
	}
	e := spliceEngine{conn: conn, req: req, total: req.Total}
	if req.File != nil {
		rc, err := req.File.SyscallConn()
		if err != nil {
			return req.Total, true, nil
		}
		e.fileConn = rc
	}
	return e.run()
}

// spliceEngine is one blocking SpliceTo moving a socket's input into a native
// connection or a file through a kernel pipe.
type spliceEngine struct {
	conn     *fdConn
	req      blockio.SpliceRequest
	fileConn syscall.RawConn
	pipe     *socket.SplicePipe
	inPipe   int // bytes in the pipe not yet forwarded
	total    int64
}

func (e *spliceEngine) run() (int64, bool, error) {
	conn, req, h := e.conn, e.req, e.req.Hooks
	if !conn.requestSplice() {
		return e.total, true, nil
	}
	defer e.releasePipe()
	defer conn.endSplice()
	granted := false
	for req.Count < 0 || e.total < req.Count {
		tok := h.Prepare()
		total, more, ended, err := h.Drain(e.total)
		e.total = total
		if err != nil {
			return e.total, false, err
		}
		if req.Count >= 0 && e.total >= req.Count {
			return e.total, false, nil
		}
		if more {
			// A Pump added bytes since the drain looked: they go first.
			continue
		}
		if ended {
			return spliceEOF(e.total, req.Count)
		}
		if !granted {
			var grantEnded bool
			if granted, grantEnded = conn.spliceGranted(); !granted {
				if grantEnded {
					// No grant comes: the turns deliver the rest.
					return e.total, true, nil
				}
				if err := h.Wait(tok); err != nil {
					return e.total, false, err
				}
			}
			// The round that granted the socket handed what it held to the
			// Reader's pipe after the drain above: that goes first.
			continue
		}
		if e.pipe == nil {
			pipe, err := socket.AcquireSplicePipe()
			if err != nil {
				return e.total, true, nil
			}
			e.pipe = pipe
		}
		eof, rerr := e.fill()
		if e.inPipe > 0 {
			if hook := testHookSpliceFilled.Load(); hook != nil {
				(*hook)()
			}
			fallback, err := e.forward()
			if err != nil || fallback {
				return e.total, fallback, err
			}
			// EAGAIN may only mean the pipe filled: read on until the socket
			// itself has nothing.
			if !eof && (rerr == nil || isWouldBlock(rerr)) {
				continue
			}
		}
		if eof {
			// The connection's turns read the end of the stream again once the
			// socket is theirs, and pass it on.
			return e.total, true, nil
		}
		if rerr != nil && !isWouldBlock(rerr) && !errors.Is(rerr, net.ErrClosed) {
			if socket.IsSpliceUnsupported(rerr) {
				return e.total, true, nil
			}
			return e.total, false, rerr
		}
		// Drained, or the socket was closed and its notice is on its way:
		// either wakes this wait, which holds no pipe.
		e.releasePipe()
		if err := h.Wait(tok); err != nil {
			return e.total, false, err
		}
	}
	return e.total, false, nil
}

// fill moves socket bytes into the pipe, one batch at most: no more than the
// destination's window, so what it cannot send at once queues within it. A
// window of 0 means no bound.
func (e *spliceEngine) fill() (eof bool, err error) {
	batch := spliceStepMax
	if e.req.Sink != nil {
		if win := e.req.Sink.Window(); win > 0 {
			batch = min(batch, win)
		}
	}
	if e.req.Count >= 0 {
		batch = min(batch, int(e.req.Count-e.total))
	}
	for e.inPipe < batch {
		n, err := e.conn.spliceIn(e.pipe.W, batch-e.inPipe)
		e.inPipe += n
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
func (e *spliceEngine) forward() (fallback bool, err error) {
	if e.req.Sink != nil {
		n, taken, err := e.req.Sink.SpliceFromPipe(e.pipe.R, e.inPipe)
		e.total += int64(n)
		if taken {
			e.inPipe = 0
		}
		return false, err
	}
	n, fallback, err := splicePipeToFile(e.pipe.R, e.req.File, e.fileConn, e.inPipe)
	e.inPipe = 0
	e.total += int64(n)
	return fallback, err
}

// testHookSpliceRescue, when set, runs as a splice hands bytes its pipe still
// holds back to the Reader.
var testHookSpliceRescue atomic.Pointer[func()]

// releasePipe returns the pipe, handing what it still holds back to the Reader
// first: a pipe that may hold bytes still is closed, not pooled.
func (e *spliceEngine) releasePipe() {
	if e.pipe == nil {
		return
	}
	if e.inPipe > 0 {
		buf, complete := readSplicePipeBuf(e.pipe.R, e.inPipe)
		if complete {
			socket.ReleaseSplicePipe(e.pipe)
		} else {
			socket.CloseSplicePipe(e.pipe)
		}
		if buf != nil {
			if hook := testHookSpliceRescue.Load(); hook != nil {
				(*hook)()
			}
			e.req.Hooks.Rescue(buf)
		}
	} else {
		socket.ReleaseSplicePipe(e.pipe)
	}
	e.inPipe = 0
	e.pipe = nil
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

// readSplicePipeBuf reads the count bytes a pipe holds into a pooled buffer,
// reporting whether it took them all.
func readSplicePipeBuf(pipeR, count int) (*bytebuf.Buffer, bool) {
	buf := bytebuf.AcquireBuffer(count)
	dst := buf.AvailableBuffer()[:count]
	nr, err := socket.ReadSplicePipe(pipeR, dst)
	if nr <= 0 {
		bytebuf.ReleaseBuffer(buf)
		return nil, false
	}
	buf.CommitWrite(nr)
	return buf, err == nil && nr == count
}

// splicePipeToFile moves count bytes from the pipe into dstFile, inside Control
// so the file's descriptor cannot close under the syscalls. A file splice(2)
// cannot write takes the rest with write(2), and reports true: the caller goes
// on in user space. Whatever it returns, the pipe is empty.
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
