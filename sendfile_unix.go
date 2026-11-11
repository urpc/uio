//go:build (linux || darwin || netbsd || freebsd || openbsd || dragonfly) && !stdio

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
	"io"
	"net"
	"os"
	"runtime"
	"sync/atomic"
	"syscall"

	"github.com/urpc/uio/internal/blockio"
	"github.com/urpc/uio/internal/bytebuf"
	"github.com/urpc/uio/internal/filerange"
	"github.com/urpc/uio/internal/socket"
)

const (
	// sendfileStepMax is the most one sendfile(2) is asked to send.
	sendfileStepMax = 1 << 20
	// fileFlushBudget bounds the file bytes one flush sends, as
	// sendInflightVec bounds its queued bytes: a write turn with more to send
	// flushes again, and in time hands on to the turns queued behind it.
	fileFlushBudget = 1 << 20
	// sendfileInlineMax is the longest range queued as bytes. Up to here a
	// read and a copy into the socket cost less than the descriptor a queued
	// range holds, or than flushing a turn's replies to send the range
	// directly; past it the copies cost more (on Linux the crossover is about
	// 32 KiB).
	sendfileInlineMax = 16 << 10
)

// testHookSendFileDirect, when set, runs while a SendFile that sent part of
// its range directly still holds the write claim.
var testHookSendFileDirect atomic.Pointer[func(*fdConn)]

// testHookQueuedSendfile, when set, runs as a queued range's sendfile(2) is
// about to read the part marked sending.
var testHookQueuedSendfile atomic.Pointer[func(*fdConn)]

// testHookQueueRangeDup, when set, runs as a range admitted to the queue is
// about to duplicate its file's descriptor, outside submitMu.
var testHookQueueRangeDup atomic.Pointer[func(*fdConn)]

func (conn *fdConn) hasNativeSendFile() bool {
	return conn != nil && !conn.isDatagram() && socket.SupportsSendfile
}

func (conn *fdConn) sendFile(file *os.File, offset, count int64) (int64, error) {
	if !conn.isDatagram() && !conn.directOwner() && conn.entry.directConsumer && conn.events().BlockingWrite && !blockio.OnTurn() {
		return conn.ensureBlockWriter().SendFile(file, offset, count)
	}
	return conn.sendFileNonBlocking(file, offset, count)
}

func (conn *fdConn) sendFileNonBlocking(file *os.File, offset, count int64) (int64, error) {
	if conn == nil || conn.isDatagram() {
		return 0, ErrUnsupported
	}
	if conn.loop == nil || conn.outputClosed() {
		return 0, net.ErrClosed
	}
	r, err := filerange.Resolve(file, offset, count)
	if err != nil || r.Len == 0 {
		return 0, err
	}
	var n int64
	if r.Native && socket.SupportsSendfile {
		n, err = conn.sendFileRange(file, r, 0)
	} else {
		n, err = sendFileBuffered(conn, file, r.Start, r.Len)
	}
	r.Finish(file, n)
	return n, err
}

// sendFileRange sends or queues a native range of file, admitted whole while
// the queue is under MaxOutboundBuffered. owner tells apart the range one
// SendFileContext call queued, which that call may withdraw; 0 is no such
// call. A range that finds nothing queued goes straight to the socket from
// the caller's goroutine, and what the socket does not take is queued.
func (conn *fdConn) sendFileRange(file *os.File, r filerange.Range, owner uint64) (int64, error) {
	if conn.loop == nil || conn.outputClosed() {
		return 0, net.ErrClosed
	}
	if conn.directOwner() {
		return conn.sendFileOnTurn(file, r, owner)
	}
	if conn.outboundEmpty() && !conn.writeBlocked() && conn.writeState.Load()&writeOpenedFlag != 0 &&
		conn.claimDirectSend() {
		return conn.sendFileDirect(file, r, owner, false)
	}
	return conn.queueRange(file, r, owner, true)
}

// sendFileOnTurn is sendFileRange on the connection's turn. A short range
// behind the turn's corked replies joins them as bytes and leaves with them;
// a longer one has them sent first and goes straight to the socket after
// them, without copies or the descriptor a queued range holds.
func (conn *fdConn) sendFileOnTurn(file *os.File, r filerange.Range, owner uint64) (int64, error) {
	conn.handOverScratch()
	if conn.pending.Load() > 0 {
		if r.Len <= sendfileInlineMax {
			return conn.queueRange(file, r, owner, false)
		}
		if !conn.writeBlocked() && conn.turn&turnFlushing == 0 {
			_, _ = conn.flushOnLoop()
		}
	}
	if conn.outboundEmpty() && !conn.writeBlocked() && conn.writeState.Load()&writeOpenedFlag != 0 {
		if held := conn.turnHoldsWrite(); held || conn.claimDirectSend() {
			return conn.sendFileDirect(file, r, owner, held)
		}
	}
	return conn.queueRange(file, r, owner, false)
}

// sendFileDirect sends what the socket takes of a range at once and queues the
// rest ahead of what others queued meanwhile. Its caller holds the write claim
// — for a turn, held reports a claim the turn keeps — and found nothing
// queued. A turn reports what it sent itself, once the rest is queued, as its
// Writes do; a send outside the turns hands the report to a write turn. A
// range sendfile(2) refuses is queued to be read instead, with no more memory
// than the copy it makes at a time.
func (conn *fdConn) sendFileDirect(file *os.File, r filerange.Range, owner uint64, held bool) (int64, error) {
	off := r.Start
	sent, serr := socket.Sendfile(int(conn.fd), int(file.Fd()), &off, int(min(r.Len, sendfileStepMax)))
	runtime.KeepAlive(file)
	rest := r
	rest.Start, rest.Len = off, r.Len-int64(sent)
	if hook := testHookSendFileDirect.Load(); hook != nil && rest.Len > 0 {
		(*hook)(conn)
	}
	var err error
	switch {
	case rest.Len == 0:
	case sent == 0 && serr == nil:
		// The file ended before the range: truncated since it was sized.
		err = io.ErrUnexpectedEOF
	case sent == 0 && serr != nil && !isWouldBlock(serr):
		if socket.IsSendfileUnsupported(serr) {
			rest.Reads = true
			if err = conn.queueRangeFirst(file, rest, owner); err != nil {
				err = serr
			}
			break
		}
		// Nothing left: a failure to read the file leaves the stream as it was.
		if !socket.IsSendfileSourceError(serr) {
			conn.failDirectWrite(serr)
		}
		err = serr
	case serr != nil && !isWouldBlock(serr):
		// Part of the range is on the wire and sendfile(2) cannot send the rest.
		err = serr
		conn.failDirectWrite(serr)
	default:
		if err = conn.queueRangeFirst(file, rest, owner); err != nil {
			if sent > 0 {
				// Part of the range is on the wire and the rest cannot follow.
				conn.failDirectWrite(err)
			}
		} else if serr != nil {
			conn.markWriteBlocked()
		}
	}
	if conn.directOwner() {
		conn.reportSentOnTurn(sent, held)
		if !held {
			conn.releaseWriteAndKick()
		}
	} else {
		conn.finishDirectSend(sent)
	}
	if err != nil {
		return int64(sent), err
	}
	return r.Len, nil
}

// queueRangeFirst queues the rest of a range a direct send began, ahead of
// everything others queued while that send held the claim: it started with
// nothing queued, so all of that came after the range. The range was
// admitted already, so only a closing connection refuses it.
func (conn *fdConn) queueRangeFirst(file *os.File, r filerange.Range, owner uint64) error {
	if r.Len <= sendfileInlineMax {
		buf, err := readFileRange(file, r.Start, r.Len)
		if err != nil {
			return err
		}
		conn.submitMu.Lock()
		if !conn.acceptsDirectRestLocked() {
			conn.submitMu.Unlock()
			bytebuf.ReleaseBuffer(buf)
			return net.ErrClosed
		}
		conn.pending.Add(r.Len)
		conn.outbound.PrependOwned(buf)
		conn.submitMu.Unlock()
		return nil
	}
	if err := checkReadable(file, r.Start); err != nil {
		return err
	}
	seg, err := newFileSegment(file, r, owner)
	if err != nil {
		return err
	}
	seg.reads = r.Reads
	zc := conn.zeroCopyState()
	conn.submitMu.Lock()
	if !conn.acceptsDirectRestLocked() {
		conn.submitMu.Unlock()
		_ = syscall.Close(seg.fd)
		return net.ErrClosed
	}
	conn.pending.Add(r.Len)
	moveBlocks(&seg.trailing, &conn.outbound)
	zc.files.pushFront(seg)
	conn.writeState.Or(writeFileTailFlag)
	conn.submitMu.Unlock()
	return nil
}

// queueRange queues a range behind everything accepted before it, admitted
// whole while the queue is under MaxOutboundBuffered. A short range is read
// into memory now, which costs less than a descriptor held until it is sent;
// a range that continues the last queued one of the same file and caller
// extends it, so a file sent in pieces holds one descriptor. Outside the
// connection's turns kick gives the queue a sender; a turn flushes its own
// output as it ends.
func (conn *fdConn) queueRange(file *os.File, r filerange.Range, owner uint64, kick bool) (int64, error) {
	if r.Len <= sendfileInlineMax {
		buf, err := readFileRange(file, r.Start, r.Len)
		if err != nil {
			return 0, err
		}
		conn.submitMu.Lock()
		if err := conn.admitRangeLocked(); err != nil {
			conn.submitMu.Unlock()
			bytebuf.ReleaseBuffer(buf)
			return 0, err
		}
		conn.pending.Add(r.Len)
		conn.outboundTargetLocked().AppendOwned(buf)
		conn.submitMu.Unlock()
	} else {
		zc := conn.zeroCopyState()
		conn.submitMu.Lock()
		err := conn.admitRangeLocked()
		if err == nil && conn.extendLastRangeLocked(zc, r, owner) {
			conn.pending.Add(r.Len)
		} else {
			conn.submitMu.Unlock()
			if err != nil {
				return 0, err
			}
			if hook := testHookQueueRangeDup.Load(); hook != nil {
				(*hook)(conn)
			}
			if err := checkReadable(file, r.Start); err != nil {
				return 0, err
			}
			seg, err := newFileSegment(file, r, owner)
			if err != nil {
				return 0, err
			}
			conn.submitMu.Lock()
			if err := conn.admitRangeLocked(); err != nil {
				conn.submitMu.Unlock()
				_ = syscall.Close(seg.fd)
				return 0, err
			}
			conn.pending.Add(r.Len)
			zc.files.pushBack(seg)
			conn.writeState.Or(writeFileTailFlag)
		}
		conn.submitMu.Unlock()
	}
	if kick {
		conn.kickWriterOutside()
	}
	return r.Len, nil
}

// admitRangeLocked admits a file range: one whole, whatever its length, while
// the queue is under the limit, since a range holds a descriptor, not memory.
func (conn *fdConn) admitRangeLocked() error {
	if conn.refusesQueueLocked() {
		return net.ErrClosed
	}
	if limit := int64(conn.payloadLimit()); limit > 0 && conn.pending.Load() >= limit {
		return conn.errOutboundOverflow()
	}
	return nil
}

// extendLastRangeLocked appends r to the last queued range when r continues
// it: the same file and caller, with nothing written in between.
func (conn *fdConn) extendLastRangeLocked(zc *zeroCopyState, r filerange.Range, owner uint64) bool {
	if zc.files.len() == 0 {
		return false
	}
	last := zc.files.back()
	if !last.trailing.Empty() || last.owner != owner || last.remain <= 0 || last.reads ||
		last.dev != r.Dev || last.ino != r.Ino || last.offset+last.remain != r.Start {
		return false
	}
	last.remain += r.Len
	return true
}

// checkReadable reports a file that cannot be read at off, which a range of it
// would fail on only once queued: the caller gets the error, and the stream
// stays as it was.
func checkReadable(file *os.File, off int64) error {
	var b [1]byte
	for {
		n, err := file.ReadAt(b[:], off)
		if err == syscall.EINTR {
			continue
		}
		if n > 0 {
			return nil
		}
		if err == io.EOF || err == nil {
			return io.ErrUnexpectedEOF
		}
		return err
	}
}

// refusesQueueLocked reports a connection that queues no more output.
func (conn *fdConn) refusesQueueLocked() bool {
	return conn.loop == nil || conn.outputClosed() || conn.events().closing.Load() || conn.loop.stopping.Load()
}

// acceptsDirectRestLocked reports a connection that may still take the rest of
// a range whose first part is on the wire. Such a range was accepted before a
// close that came while it was sent, so the close's drain, bounded by
// LingerTimeout, sends it; only a released connection, or one whose stream
// failed, refuses it.
func (conn *fdConn) acceptsDirectRestLocked() bool {
	return conn.loop != nil && !conn.writeFailed() && !conn.close.isReleased() &&
		!conn.events().closing.Load() && !conn.loop.stopping.Load()
}

// newFileSegment duplicates file's descriptor for a queued range, so the
// caller may close file.
func newFileSegment(file *os.File, r filerange.Range, owner uint64) (fileSegment, error) {
	fd, err := socket.Dup(int(file.Fd()))
	runtime.KeepAlive(file)
	if err != nil {
		return fileSegment{fd: -1}, err
	}
	return fileSegment{fd: fd, offset: r.Start, remain: r.Len, dev: r.Dev, ino: r.Ino, owner: owner}, nil
}

// readFileRange reads n bytes of file from off into a pooled buffer.
func readFileRange(file *os.File, off, n int64) (*bytebuf.Buffer, error) {
	buf := bytebuf.AcquireBuffer(int(n))
	got, err := file.ReadAt(buf.AvailableBuffer()[:n], off)
	if int64(got) < n {
		bytebuf.ReleaseBuffer(buf)
		if err == nil || err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return nil, err
	}
	buf.CommitWrite(int(n))
	return buf, nil
}

// outboundTargetLocked is where bytes accepted now are queued: behind the last
// queued file range once there is one, so they leave after it.
func (conn *fdConn) outboundTargetLocked() *bytebuf.CompositeBuffer {
	if conn.writeState.Load()&writeFileTailFlag != 0 {
		return &conn.loadZeroCopy().files.back().trailing
	}
	return &conn.outbound
}

// cancelQueuedFiles withdraws the ranges owner queued that have not been sent
// and reports the bytes they still held.
func (conn *fdConn) cancelQueuedFiles(owner uint64) int64 {
	zc := conn.loadZeroCopy()
	if zc == nil {
		return 0
	}
	conn.submitMu.Lock()
	cancelled := conn.dropRangesLocked(zc, owner, false)
	if n := zc.withdrawn[owner]; n > 0 {
		// Released first: the ranges are gone, and this is what of them was.
		cancelled += n
		delete(zc.withdrawn, owner)
	}
	conn.submitMu.Unlock()
	if cancelled > 0 {
		conn.retireOutsideClaim(cancelled)
	}
	return cancelled
}

// retireOutsideClaim retires queued bytes withdrawn rather than sent, by a
// caller without the write claim, and does what a release of the claim does
// once nothing is pending: a CloseWrite or a graceful close waiting for the
// queue to drain goes on.
func (conn *fdConn) retireOutsideClaim(n int64) {
	conn.retireSent(int(n))
	conn.notifyBlockWriteSpace()
	if conn.pending.Load() != 0 {
		return
	}
	if conn.writeState.Load()&(writeShutFlag|writeShutDoneFlag) == writeShutFlag {
		conn.scheduleIO(ioEventShutWrite)
	}
	if conn.close.isDraining() {
		conn.scheduleTeardown()
	}
}

// clearFilesLocked drops every queued range as the connection is released.
func (conn *fdConn) clearFilesLocked() {
	if zc := conn.loadZeroCopy(); zc != nil {
		_ = conn.dropRangesLocked(zc, 0, true)
	}
}

// dropRangesLocked withdraws the ranges owner queued, or every range when all
// is set, and returns the file bytes they still held. What was written after
// a withdrawn range joins what precedes it, so the stream keeps its order. Of
// a range a sendfile(2) is reading, the part that call reads stays queued: it
// may be on the wire already, and its rest follows it.
func (conn *fdConn) dropRangesLocked(zc *zeroCopyState, owner uint64, all bool) int64 {
	q := &zc.files
	var dropped int64
	kept := q.head
	for i := q.head; i < len(q.segs); i++ {
		seg := &q.segs[i]
		if drop := all || seg.owner == owner; drop && seg.sending == 0 {
			n := max(seg.remain, 0)
			dropped += n
			conn.recordWithdrawalLocked(zc, seg.owner, n, all)
			if seg.fd >= 0 {
				_ = syscall.Close(seg.fd)
			}
			before := &conn.outbound
			if kept > q.head {
				before = &q.segs[kept-1].trailing
			}
			moveBlocks(before, &seg.trailing)
			continue
		} else if drop {
			n := max(seg.remain-seg.sending, 0)
			dropped += n
			conn.recordWithdrawalLocked(zc, seg.owner, n, all)
			seg.remain = min(seg.remain, seg.sending)
			// Withdrawn: nothing extends it, and no later withdrawal counts it.
			seg.owner = 0
		}
		if kept != i {
			q.segs[kept] = *seg
		}
		kept++
	}
	clear(q.segs[kept:])
	q.segs = q.segs[:kept]
	if q.len() == 0 {
		q.segs, q.head = q.segs[:0], 0
		conn.writeState.And(^writeFileTailFlag)
	}
	return dropped
}

// recordWithdrawalLocked keeps what a release dropped of a SendFileContext
// call's ranges, for the call's CancelQueuedFiles to report: a withdrawal of
// one owner reports its own bytes, a release keeps them until the owner asks.
func (conn *fdConn) recordWithdrawalLocked(zc *zeroCopyState, owner uint64, n int64, all bool) {
	if !all || owner == 0 || n <= 0 {
		return
	}
	if zc.withdrawn == nil {
		zc.withdrawn = make(map[uint64]int64)
	}
	zc.withdrawn[owner] += n
}

// popRangeLocked removes the front range, which has been sent, and returns its
// descriptor for the caller to close once it released submitMu. What was
// written after it goes next; outbound is empty while a range is sent.
func (conn *fdConn) popRangeLocked(zc *zeroCopyState) int {
	seg := zc.files.front()
	fd := seg.fd
	moveBlocks(&conn.outbound, &seg.trailing)
	zc.files.popFront()
	if zc.files.len() == 0 {
		conn.writeState.And(^writeFileTailFlag)
	}
	return fd
}

// sendQueuedFiles sends queued file ranges once outbound and inflight have
// emptied, in at most 16 calls, until it has sent budget bytes. Its caller
// holds the write claim. more reports bytes written after a sent range, now
// in outbound. A range sendfile(2) refuses is sent with reads instead. A file
// that fails to read, or ends before its range, fails the stream: the bytes
// written after the range must not follow its part on the wire, where the
// peer would take them for the range's own.
func (conn *fdConn) sendQueuedFiles(budget int) (written int, more bool, err error) {
	zc := conn.loadZeroCopy()
	if zc == nil {
		conn.writeState.And(^writeFileTailFlag)
		return 0, false, nil
	}
	for calls := 0; ; calls++ {
		conn.submitMu.Lock()
		if !conn.outbound.Empty() || zc.files.len() == 0 {
			more = !conn.outbound.Empty()
			conn.submitMu.Unlock()
			return written, more, nil
		}
		seg := zc.files.front()
		if seg.remain <= 0 {
			fd := conn.popRangeLocked(zc)
			conn.submitMu.Unlock()
			_ = syscall.Close(fd)
			continue
		}
		if calls >= 16 || written >= budget {
			conn.submitMu.Unlock()
			return written, false, nil
		}
		chunk := min(seg.remain, sendfileStepMax)
		seg.sending = chunk
		fd, off, reads := seg.fd, seg.offset, seg.reads
		conn.submitMu.Unlock()

		if hook := testHookQueuedSendfile.Load(); hook != nil {
			(*hook)(conn)
		}
		var n int
		var serr error
		if reads {
			n, serr = conn.sendFileReads(fd, off, int(chunk))
		} else {
			n, serr = socket.Sendfile(int(conn.fd), fd, &off, int(chunk))
		}

		conn.submitMu.Lock()
		// Nothing removes the front range while it is being sent.
		seg = zc.files.front()
		seg.sending = 0
		if n > 0 {
			seg.offset += int64(n)
			seg.remain -= int64(n)
			written += n
		} else if serr != nil && !reads && socket.IsSendfileUnsupported(serr) {
			seg.reads, serr = true, nil
			conn.submitMu.Unlock()
			continue
		}
		closeFD := -1
		if seg.remain <= 0 {
			closeFD = conn.popRangeLocked(zc)
		}
		conn.submitMu.Unlock()
		if closeFD >= 0 {
			_ = syscall.Close(closeFD)
		}

		switch {
		case serr != nil && !isWouldBlock(serr):
			conn.failDirectWrite(serr)
			return written, false, serr
		case n > 0:
		case serr == nil:
			// The file ended before the range: truncated since it was queued.
			conn.failDirectWrite(io.ErrUnexpectedEOF)
			return written, false, io.ErrUnexpectedEOF
		case !conn.markWriteBlocked():
			return written, false, nil
		}
	}
}

// sendFileReads sends up to n bytes of a queued range from fd at off with a
// read and a send, for a file sendfile(2) cannot serve. What the socket does
// not take is read again next time. A file at its end reports 0 and no
// error, as sendfile(2) does.
func (conn *fdConn) sendFileReads(fd int, off int64, n int) (int, error) {
	n = min(n, sendfileReadChunk)
	buf := bytebuf.AcquireBuffer(n)
	defer bytebuf.ReleaseBuffer(buf)
	p := buf.AvailableBuffer()[:n]
	for {
		nr, err := syscall.Pread(fd, p, off)
		if err == syscall.EINTR {
			continue
		}
		if nr <= 0 {
			return 0, err
		}
		sent, err := socket.Send(int(conn.fd), p[:nr])
		return max(sent, 0), err
	}
}
