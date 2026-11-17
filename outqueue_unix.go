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

// The outbound queue: one FIFO of byte runs and queued file ranges. A file
// range is trailed by the bytes accepted after it, so the order on the wire is
// the order bytes were accepted, whatever mix of writes and SendFile ranges
// produced it. Producers reach the queue only through the methods here, which
// route every append behind the last queued range; the raw byte run is the
// queue's own, touched directly only by its send and range machinery.

package uio

import "github.com/urpc/uio/internal/bytebuf"

// outQueue is a connection's outbound byte run, the head of its outbound FIFO.
// Queued file ranges hang off the lazily-built zero-copy state, each trailed by
// the bytes accepted after it; outTargetLocked routes a new append to the tail
// of that FIFO. The byte run is inline so a connection that queues output pays
// no extra allocation and the connection stays in its 256-byte size class.
type outQueue struct {
	bytes bytebuf.CompositeBuffer // protected by submitMu
}

// outTargetLocked returns where bytes accepted now are queued: behind the last
// queued file range's trailing bytes, or the byte run when no range is queued.
// It is the only way a producer reaches the append target, so an append cannot
// land ahead of a queued range by bypassing the routing.
func (conn *fdConn) outTargetLocked() *bytebuf.CompositeBuffer {
	if conn.writeState.Load()&writeFileTailFlag != 0 {
		return &conn.loadZeroCopy().files.back().trailing
	}
	return &conn.out.bytes
}

// outAppendOwned queues an owned buffer behind everything accepted before it.
func (conn *fdConn) outAppendOwned(buf *bytebuf.Buffer) {
	conn.outTargetLocked().AppendOwned(buf)
}

// outAppendOwnedTailCoalesced queues an owned buffer, joining the tail block's
// spare room when it is small enough to coalesce, as queueOwnedWrite does.
func (conn *fdConn) outAppendOwnedTailCoalesced(buf *bytebuf.Buffer, block int) {
	out := conn.outTargetLocked()
	if !out.Empty() && coalesceOwned(buf, buf.Len(), block) {
		out.AppendOwnedTailCoalesced(buf, block)
	} else {
		out.AppendOwned(buf)
	}
}

// outWriteCoalesced copies data into the queue's tail, coalescing small writes.
func (conn *fdConn) outWriteCoalesced(data []byte, block int) {
	conn.outTargetLocked().WriteCoalesced(data, block)
}

// outWritevCoalesced copies vec into the queue's tail, coalescing small writes.
func (conn *fdConn) outWritevCoalesced(vec [][]byte, block int) {
	conn.outTargetLocked().WritevCoalesced(vec, block)
}

// outReserve reserves n contiguous bytes at the queue's tail.
func (conn *fdConn) outReserve(n, block int) []byte {
	return conn.outTargetLocked().Reserve(n, block)
}

// outPrependRest puts buf at the head of the byte run. Only a direct send that
// began with nothing queued may do this: it held the write claim from an empty
// queue, so its unsent rest goes ahead of whatever others queued while it sent.
// See queueDirectSuffix, queueRangeFirst and queuePipeFirst.
func (conn *fdConn) outPrependRest(buf *bytebuf.Buffer) {
	conn.out.bytes.PrependOwned(buf)
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
