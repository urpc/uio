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
	"runtime"
	"sync/atomic"

	"github.com/urpc/uio/internal/blockio"
	"github.com/urpc/uio/internal/bytebuf"
)

// zeroCopyState is what a connection's SendFile and splice paths keep, built
// the first time one of them runs, so a connection that uses neither pays one
// pointer of its wakeState.
type zeroCopyState struct {
	files fileQueue // guarded by submitMu
	// spliceMode is who owns the socket's input; see spliceModeNone.
	spliceMode atomic.Uint32
	// spliceGate counts a SpliceTo's splice(2) calls on the socket; it is
	// retired before the socket closes.
	spliceGate descriptorGate
	// unwritten counts bytes a send outside the connection's turns put on the
	// wire, which the write turn it hands its claim to reports.
	unwritten atomic.Int64
	// withdrawn holds what a release dropped of each SendFileContext call's
	// ranges, for its CancelQueuedFiles to report; guarded by submitMu.
	withdrawn map[uint64]int64
	// unread counts bytes a SpliceTo took from the socket, which the
	// connection's next read round reports.
	unread atomic.Int64
}

// fileSegment is a file range queued behind the bytes accepted before it.
// trailing holds what was written after it, so it leaves after the range.
type fileSegment struct {
	fd       int
	offset   int64
	remain   int64
	dev, ino uint64 // the file's identity, to extend the range a later SendFile continues
	// owner is the SendFileContext call that queued the range, which may
	// withdraw it; 0 for SendFile, which never does.
	owner uint64
	// sending is the part of the range a sendfile(2) is reading now: a
	// withdrawal leaves it queued, since it may be on the wire already.
	sending int64
	// reads marks a range sendfile(2) refused, sent with reads instead.
	reads    bool
	trailing bytebuf.CompositeBuffer
}

// fileQueue holds the file ranges queued behind a connection's output, oldest
// first. Sending the front one advances head rather than moving the rest, so
// a long queue costs no more per range than a short one.
type fileQueue struct {
	segs []fileSegment
	head int
}

func (q *fileQueue) len() int { return len(q.segs) - q.head }

func (q *fileQueue) front() *fileSegment { return &q.segs[q.head] }

func (q *fileQueue) back() *fileSegment { return &q.segs[len(q.segs)-1] }

func (q *fileQueue) pushBack(seg fileSegment) {
	if q.head > 0 && len(q.segs) == cap(q.segs) {
		// Reuse the room sent ranges left before growing.
		n := copy(q.segs, q.segs[q.head:])
		clear(q.segs[n:])
		q.segs, q.head = q.segs[:n], 0
	}
	q.segs = append(q.segs, seg)
}

func (q *fileQueue) pushFront(seg fileSegment) {
	if q.head > 0 {
		q.head--
		q.segs[q.head] = seg
		return
	}
	q.segs = append(q.segs, fileSegment{})
	copy(q.segs[1:], q.segs)
	q.segs[0] = seg
}

// popFront drops the front range, whose trailing bytes its caller moved.
func (q *fileQueue) popFront() {
	q.segs[q.head] = fileSegment{fd: -1}
	q.head++
	if q.head == len(q.segs) {
		q.segs, q.head = q.segs[:0], 0
	}
}

// moveBlocks moves src's blocks to the end of dst.
func moveBlocks(dst, src *bytebuf.CompositeBuffer) {
	if dst.Empty() {
		*dst, *src = *src, *dst
		return
	}
	for !src.Empty() {
		if blk := src.PopBlock(); blk != nil {
			dst.AppendOwned(blk)
		}
	}
}

// loadZeroCopy returns the connection's zero-copy state, nil when it has none.
func (conn *fdConn) loadZeroCopy() *zeroCopyState {
	if w := conn.wake.Load(); w != nil {
		return w.zeroCopy.Load()
	}
	return nil
}

// zeroCopyState returns the connection's zero-copy state, built on first use.
func (conn *fdConn) zeroCopyState() *zeroCopyState {
	w := conn.wakeState()
	if zc := w.zeroCopy.Load(); zc != nil {
		return zc
	}
	zc := &zeroCopyState{}
	if w.zeroCopy.CompareAndSwap(nil, zc) {
		return zc
	}
	return w.zeroCopy.Load()
}

// finishDirectSend ends a send made outside the connection's turns by a
// caller holding the write claim. A caller on a turn — another connection's,
// relaying into this one — reports what it sent itself: a Write there never
// blocks. Any other caller may hold locks of its own, or a BlockingWriter, so
// OnBytesWritten never runs on it: what it sent is reported by the write turn
// it hands the claim to, as output other goroutines queue is.
func (conn *fdConn) finishDirectSend(sent int) {
	if sent > 0 && (conn.callbacks().onBytesWritten != nil || conn.hasWriteControl()) {
		if blockio.OnTurn() {
			conn.reportSentOnOtherTurn(sent)
		} else {
			conn.zeroCopyState().unwritten.Add(int64(sent))
			if conn.handOffClaim() {
				return
			}
		}
	}
	conn.releaseWriteAndKick()
}

// handOffClaim gives the write claim its caller, outside the turns, holds to
// a write turn, which reports what the caller sent and sends what is queued.
// It reports false when no write turn may start: the caller then releases the
// claim, and the close under way reports instead. When the write turn needed
// a worker woken, the caller yields, as kickWriterOutside does.
func (conn *fdConn) handOffClaim() bool {
	if conn.isClosing() || !conn.loop.acquireIO() {
		return false
	}
	accepted, woke := conn.loop.ioPool.submitWriteWaking(conn)
	if !accepted {
		conn.handleWriteSubmitFailure()
	} else if woke {
		runtime.Gosched()
	}
	return true
}

// reportSentOnTurn reports what the connection's own turn sent with
// sendfile(2), while it holds the write claim: OnBytesWritten runs inside a
// flush's scope, so a Write it makes is queued and a Flush it starts returns
// at once, and a panic in it gives back a claim the turn took — held reports
// a claim the turn keeps, which the turn's own cleanup releases.
func (conn *fdConn) reportSentOnTurn(sent int, held bool) {
	if sent <= 0 {
		return
	}
	conn.turn |= turnFlushing
	defer func() {
		conn.turn &^= turnFlushing
		if !held {
			if r := recover(); r != nil {
				conn.releaseWrite()
				panic(r)
			}
		}
	}()
	conn.events().onSocketBytesWrite(conn, sent)
}

// reportSentOnOtherTurn reports what a send on another connection's turn put
// on the wire; a panic in the callback gives the claim it holds back first.
func (conn *fdConn) reportSentOnOtherTurn(sent int) {
	defer func() {
		if r := recover(); r != nil {
			conn.releaseWrite()
			panic(r)
		}
	}()
	conn.events().onSocketBytesWrite(conn, sent)
}

// reportUnwritten reports to OnBytesWritten what sends outside the
// connection's turns put on the wire. Its caller holds the write claim.
func (conn *fdConn) reportUnwritten(zc *zeroCopyState) {
	if zc.unwritten.Load() == 0 {
		return
	}
	if n := zc.unwritten.Swap(0); n > 0 {
		conn.events().onSocketBytesWrite(conn, int(n))
	}
}

// reportUnread reports to OnBytesRead what a SpliceTo took from the socket.
// It runs on the connection's turn, or as the connection is released.
func (conn *fdConn) reportUnread(zc *zeroCopyState) {
	if zc.unread.Load() == 0 {
		return
	}
	if n := zc.unread.Swap(0); n > 0 {
		conn.events().onSocketBytesRead(conn, int(n))
	}
}
