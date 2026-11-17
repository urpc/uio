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
	"errors"
	"io"
	"net"
	"unsafe"

	"github.com/urpc/uio/internal/blockio"
	"github.com/urpc/uio/internal/bytebuf"
	"github.com/urpc/uio/internal/socket"
)

func (conn *fdConn) WriteByte(value byte) error {
	var data [1]byte
	data[0] = value
	_, err := conn.Write(data[:])
	return err
}

func (conn *fdConn) WriteString(value string) (int, error) {
	data := unsafe.Slice(unsafe.StringData(value), len(value))
	return conn.Write(data)
}

func (conn *fdConn) Write(data []byte) (int, error) {
	if !conn.isDatagram() && !conn.directOwner() && conn.entry.directConsumer && conn.events().BlockingWrite && !blockio.OnTurn() {
		return conn.ensureBlockWriter().Write(data)
	}
	return conn.writeNonBlocking(data)
}

func (conn *fdConn) writeNonBlocking(data []byte) (int, error) {
	if len(data) == 0 {
		return 0, nil
	}
	if conn.outputClosed() {
		return 0, net.ErrClosed
	}
	if conn.directOwner() {
		return conn.writeOnLoop(data)
	}
	if conn.isDatagram() {
		return conn.sendUDPOutside(data)
	}
	if limit := conn.payloadLimit(); limit > 0 && len(data) > limit {
		return 0, conn.errOutboundOverflow()
	}
	if err := conn.precheckOutbound(len(data)); err != nil {
		return 0, err
	}
	owned := bytebuf.CloneBuffer(data)
	n, err := conn.queueOwnedWrite(owned, len(data))
	if errors.Is(err, ErrOutboundOverflow) {
		// The clone never entered the queue, so Write still owns it.
		bytebuf.ReleaseBuffer(owned)
	}
	return n, err
}

func (conn *fdConn) Writev(vec [][]byte) (int, error) {
	if !conn.isDatagram() && !conn.directOwner() && conn.entry.directConsumer && conn.events().BlockingWrite && !blockio.OnTurn() {
		return conn.ensureBlockWriter().Writev(vec)
	}
	return conn.writevNonBlocking(vec)
}

func (conn *fdConn) writevNonBlocking(vec [][]byte) (int, error) {
	total := 0
	for _, segment := range vec {
		if len(segment) > int(^uint(0)>>1)-total {
			return 0, conn.errOutboundOverflow()
		}
		total += len(segment)
	}
	if total == 0 {
		return 0, nil
	}
	if conn.outputClosed() {
		return 0, net.ErrClosed
	}
	if conn.isDatagram() {
		// Each buffer is one datagram, sent in one sendmmsg where the system
		// has it; see writevUDP.
		return conn.writevUDP(vec)
	}
	if conn.directOwner() {
		return conn.writevOnLoop(vec, total)
	}
	if limit := conn.payloadLimit(); limit > 0 && total > limit {
		return 0, conn.errOutboundOverflow()
	}
	if err := conn.precheckOutbound(total); err != nil {
		return 0, err
	}
	owned := bytebuf.CloneBuffers(vec, total)
	n, err := conn.queueOwnedWrite(owned, total)
	if errors.Is(err, ErrOutboundOverflow) {
		// The clone never entered the queue, so Writev still owns it.
		bytebuf.ReleaseBuffer(owned)
	}
	return n, err
}

func (conn *fdConn) WriteOwned(owned *Buffer) (int, error) {
	if owned != nil && !conn.isDatagram() && !conn.directOwner() && conn.entry.directConsumer && conn.events().BlockingWrite && !blockio.OnTurn() {
		return conn.ensureBlockWriter().WriteOwned(owned)
	}
	return conn.writeOwnedNonBlocking(owned)
}

func (conn *fdConn) writeOwnedNonBlocking(owned *Buffer) (int, error) {
	if owned == nil {
		return 0, nil
	}
	size := owned.Len()
	if size == 0 {
		bytebuf.ReleaseBuffer(owned)
		return 0, nil
	}
	if conn.outputClosed() {
		bytebuf.ReleaseBuffer(owned)
		return 0, net.ErrClosed
	}
	if conn.directOwner() {
		if conn.isDatagram() {
			defer bytebuf.ReleaseBuffer(owned)
			return conn.sendUDPOnLoop(owned.Bytes())
		}
		return conn.writeOwnedOnLoop(owned, size)
	}
	if conn.isDatagram() {
		n, err := conn.sendUDPOutside(owned.Bytes())
		if !errors.Is(err, ErrOutboundOverflow) {
			bytebuf.ReleaseBuffer(owned)
		}
		return n, err
	}
	return conn.queueOwnedWrite(owned, size)
}

// coalesceBlockSize is the block size that small writes batched in one task
// share: one read round's replies usually fit in one of them.
// midFlushThreshold is how many queued replies a read round accumulates
// before it sends them mid-round (see readRound).
func (conn *fdConn) midFlushThreshold() int {
	return min(64<<10, conn.events().readBufSize*4)
}

func (conn *fdConn) coalesceBlockSize() int {
	return min(64<<10, conn.events().readBufSize*2)
}

// coalesceOwnedMax is the largest owned buffer a corked round copies into its
// outbound tail for its size alone: up to here the copy costs less than the
// writev segment it saves.
const coalesceOwnedMax = 4 << 10

// coalesceOwned reports whether a corked round copies owned, holding size
// bytes, into its outbound blocks rather than queueing it as a segment of its
// own: a small one, and one that is mostly empty, which a queue the peer is
// slow to drain would otherwise hold on to many times over. A large, dense
// buffer keeps its segment. Copying such buffers - write batches of a whole
// read's replies, three of every four reads of a pipelining round - was 8% of
// the server's user time, to save two writev segments a round.
func coalesceOwned(owned *bytebuf.Buffer, size, block int) bool {
	if size > block/2 {
		return false
	}
	return size <= coalesceOwnedMax || 2*size < owned.Cap()
}

func (conn *fdConn) precheckOutbound(size int) error {
	if limit := int64(conn.payloadLimit()); limit > 0 {
		pending := conn.pending.Load()
		if int64(size) > limit-pending {
			return conn.errOutboundOverflow()
		}
	}
	return nil
}

// queueOwnedWrite is the cross-goroutine write path. Ownership has already
// moved into owned, so submitMu only covers admission, accounting, and pointer
// insertion; payload allocation and copying never happen under the lock. The
// bytes are sent by a write turn, beside whatever the connection's turn is
// reading. ErrOutboundOverflow accepts nothing and returns owned to the caller.
func (conn *fdConn) queueOwnedWrite(owned *bytebuf.Buffer, size int) (int, error) {
	if limit := conn.payloadLimit(); limit > 0 && size > limit {
		return 0, conn.errOutboundOverflow()
	}
	// Allocation and the only payload copy have already happened off-lock.
	conn.submitMu.Lock()
	if conn.loop == nil || conn.outputClosed() || conn.events().closing.Load() || conn.loop.stopping.Load() {
		conn.submitMu.Unlock()
		bytebuf.ReleaseBuffer(owned)
		return 0, net.ErrClosed
	}
	if !conn.reservePending(int64(size)) {
		conn.submitMu.Unlock()
		return 0, conn.errOutboundOverflow()
	}
	conn.outAppendOwnedTailCoalesced(owned, conn.coalesceBlockSize())
	conn.submitMu.Unlock()
	conn.kickWriterOutside()
	return size, nil
}

func (conn *fdConn) reservePending(size int64) bool {
	// Both callback partial writes and external producers reserve this counter.
	limit := int64(conn.payloadLimit())
	if limit <= 0 {
		conn.pending.Add(size)
		return true
	}
	for {
		old := conn.pending.Load()
		if size > limit-old {
			return false
		}
		if conn.pending.CompareAndSwap(old, old+size) {
			return true
		}
	}
}

func (conn *fdConn) reservePendingAfterFlush(size int64) (bool, error) {
	if conn.reservePending(size) {
		return true, nil
	}
	if conn.outboundEmpty() || conn.writeBlocked() {
		return false, nil
	}
	if _, err := conn.flushOnLoop(); err != nil {
		return false, err
	}
	return conn.reservePending(size), nil
}

// claimDirectSend reports whether the turn may send directly, holding the
// claim it took for that. A claim the turn keeps for a reservation, or holds
// for its own flush while OnBytesWritten runs, is never lent to a direct send: the
// bytes are queued and that flush sends them, so OnBytesWritten never runs inside
// itself. Once it holds the claim the turn must also find nothing queued:
// another sender may have left part of its output there and released the
// claim between the turn's check and its taking it, and those bytes go first.
// A direct send therefore starts with nothing queued or in flight, which is
// what lets it put an unsent suffix at the head of outbound.
func (conn *fdConn) claimDirectSend() bool {
	if !conn.tryClaimWrite(0) {
		// Another sender, or this turn's own flush or reservation, owns the
		// socket; its release or flush sends these bytes.
		return false
	}
	if !conn.outboundEmpty() {
		conn.releaseWriteAndKick()
		return false
	}
	return true
}

// writeOnLoop is the task-owner fast path. It lends caller memory directly to
// the non-blocking syscall when no batching is active and copies only an unsent
// suffix that must outlive the call.
func (conn *fdConn) writeOnLoop(data []byte) (int, error) {
	if conn.isDatagram() {
		return conn.sendUDPOnLoop(data)
	}
	threshold := conn.events().WriteBufferedThreshold
	queued := !conn.outboundEmpty() || conn.turn&turnCorked != 0 || (threshold > 0 && len(data) < threshold)
	if !queued && !conn.claimDirectSend() {
		queued = true
	}
	if queued {
		return conn.writeQueuedOnLoop(data)
	}
	defer conn.releaseWriteAndKick()

	// The common callback path lends caller memory directly to the kernel.
	written, err := socket.Send(int(conn.fd), data)
	if written < 0 {
		written = 0
	}
	if err != nil {
		if isWouldBlock(err) {
			written, err = 0, nil
		} else {
			conn.failDirectWrite(err)
			return written, err
		}
	}
	if written == 0 && testHookDirectSendRefused != nil {
		testHookDirectSendRefused(conn)
	}
	if written == len(data) {
		conn.events().onSocketBytesWrite(conn, written)
		return written, nil
	}
	remaining := data[written:]
	if !conn.reservePending(int64(len(remaining))) {
		if written == 0 {
			// Nothing left the socket: pure overflow.
			return 0, conn.errOutboundOverflow()
		}
		// A partial payload is on the wire and its suffix cannot be queued:
		// the stream is broken, so report a short write, not an overflow.
		conn.events().onSocketBytesWrite(conn, written)
		conn.failDirectWrite(ErrOutboundOverflow)
		return written, io.ErrShortWrite
	}
	// Only the unsent suffix must survive after Write returns.
	conn.queueDirectSuffix(bytebuf.CloneBuffer(remaining))
	conn.setWriteBlocked(written == 0)
	// Reported once the suffix is accounted, so OnBytesWritten never observes a
	// transient empty queue in the middle of this write.
	conn.events().onSocketBytesWrite(conn, written)
	return len(data), nil
}

// writevOnLoop mirrors writeOnLoop for scatter/gather input. A partial syscall
// is collapsed into one owned suffix so later retries do not retain caller
// slices or an unbounded vector list.
func (conn *fdConn) writevOnLoop(vec [][]byte, total int) (int, error) {
	if conn.isDatagram() {
		return 0, ErrUnsupported
	}
	threshold := conn.events().WriteBufferedThreshold
	queued := !conn.outboundEmpty() || conn.turn&turnCorked != 0 || (threshold > 0 && total < threshold)
	if !queued && !conn.claimDirectSend() {
		queued = true
	}
	if queued {
		return conn.writevQueuedOnLoop(vec, total)
	}
	defer conn.releaseWriteAndKick()
	written, err := socket.Writev(int(conn.fd), vec)
	if written < 0 {
		written = 0
	}
	if err != nil {
		if isWouldBlock(err) {
			written, err = 0, nil
		} else {
			conn.failDirectWrite(err)
			return written, err
		}
	}
	if written == total {
		conn.events().onSocketBytesWrite(conn, written)
		return written, nil
	}
	remaining := total - written
	if !conn.reservePending(int64(remaining)) {
		if written == 0 {
			// Nothing left the socket: pure overflow.
			return 0, conn.errOutboundOverflow()
		}
		// A partial vector is on the wire and its suffix cannot be queued:
		// the stream is broken, so report a short write, not an overflow.
		conn.events().onSocketBytesWrite(conn, written)
		conn.failDirectWrite(ErrOutboundOverflow)
		return written, io.ErrShortWrite
	}
	owned := bytebuf.CloneBuffersFrom(vec, written, remaining)
	conn.queueDirectSuffix(owned)
	conn.setWriteBlocked(written == 0)
	conn.events().onSocketBytesWrite(conn, written)
	return total, nil
}

// writeOwnedOnLoop consumes owned on every return path except
// ErrOutboundOverflow, which accepts nothing and returns the buffer to the
// caller; a partial direct write that cannot queue its suffix reports
// io.ErrShortWrite and consumes the buffer instead. A corked read round's
// first output goes straight to the socket when the write is one the
// threshold sends directly anyway — a batch handed over mid-round leaves
// without waiting for the round's end, and the threshold's promise for
// smaller writes (join the outbound batch) holds in the first position too.
// The frames that follow keep zero-copy ownership as outbound's first
// segment or are coalesced into pooled blocks, to keep the final writev batch
// short; see coalesceOwned for which are copied.
func (conn *fdConn) writeOwnedOnLoop(owned *bytebuf.Buffer, size int) (int, error) {
	threshold := conn.events().WriteBufferedThreshold
	// A corked read round never sends directly, exactly as writevOnLoop does
	// not: its replies queue, coalesced, and leave together when the round
	// ends, or at the mid-round flush once they pass midFlushThreshold.
	// Sending the round's first reply from inside its callback measured a
	// pipeline 4-6% slower - it made every later reply of the round its own
	// writev segment - and no faster at echo, whose round has one reply.
	queued := !conn.outboundEmpty() || conn.turn&turnCorked != 0 || (threshold > 0 && size < threshold)
	if !queued && !conn.claimDirectSend() {
		queued = true
	}
	if queued {
		reserved, err := conn.reservePendingAfterFlush(int64(size))
		if err != nil {
			bytebuf.ReleaseBuffer(owned)
			return 0, err
		}
		if !reserved {
			return 0, conn.errOutboundOverflow()
		}
		conn.handOverScratch()
		conn.submitMu.Lock()
		out := conn.outTargetLocked()
		if conn.turn&turnCorked != 0 && !out.Empty() && coalesceOwned(owned, size, conn.coalesceBlockSize()) {
			out.AppendOwnedCoalesced(owned, conn.coalesceBlockSize())
		} else {
			out.AppendOwned(owned)
		}
		conn.submitMu.Unlock()
		return size, nil
	}
	defer conn.releaseWriteAndKick()

	written, err := socket.Send(int(conn.fd), owned.Bytes())
	if written < 0 {
		written = 0
	}
	if err != nil {
		if isWouldBlock(err) {
			written, err = 0, nil
		} else {
			bytebuf.ReleaseBuffer(owned)
			conn.failDirectWrite(err)
			return written, err
		}
	}
	if written == size {
		bytebuf.ReleaseBuffer(owned)
		conn.events().onSocketBytesWrite(conn, written)
		return written, nil
	}
	remaining := size - written
	if !conn.reservePending(int64(remaining)) {
		if written == 0 {
			// Nothing left the socket: pure overflow, so the buffer returns
			// to the caller like every other ErrOutboundOverflow.
			return 0, conn.errOutboundOverflow()
		}
		// A partial frame is on the wire and its suffix cannot be queued:
		// the stream is broken, so the buffer is consumed and the caller is
		// told the write was short rather than merely overflowed.
		bytebuf.ReleaseBuffer(owned)
		conn.events().onSocketBytesWrite(conn, written)
		conn.failDirectWrite(ErrOutboundOverflow)
		return written, io.ErrShortWrite
	}
	if written > 0 {
		owned.Discard(written)
	}
	conn.queueDirectSuffix(owned)
	conn.setWriteBlocked(written == 0)
	conn.events().onSocketBytesWrite(conn, written)
	return size, nil
}

// queueDirectSuffix queues owned at the head of outbound: what a direct send
// left unsent, or a read round's reply space. Its holder has held the write
// claim since it started with nothing queued or in flight, so bytes in
// outbound now came from producers that wrote after it; behind them, they
// would land inside a half-sent payload or ahead of replies accepted first.
func (conn *fdConn) queueDirectSuffix(owned *bytebuf.Buffer) {
	conn.submitMu.Lock()
	conn.outPrependRest(owned)
	conn.submitMu.Unlock()
}

func (conn *fdConn) failDirectWrite(err error) {
	// Once a syscall has made the byte stream unusable, queued writes must not
	// be appended ahead of the close task or flushed during close.
	conn.markWriteFailed()
	conn.requestClose(err)
}

func (conn *fdConn) Flush() error {
	if conn.isClosing() {
		return net.ErrClosed
	}
	if conn.loop == nil || conn.loop.stopping.Load() {
		return net.ErrClosed
	}
	if conn.isDatagram() {
		return nil
	}
	if conn.directOwner() {
		_, err := conn.flushOnLoop()
		return err
	}
	// Outside the task, Flush is a FIFO barrier and returns after giving the
	// queued bytes a sender.
	conn.kickWriterOutside()
	return nil
}

func (conn *fdConn) outboundEmpty() bool {
	// Queue admission reserves pending bytes before appending the owned buffer;
	// flush retires the bytes only after removing them. This makes pending a
	// conservative, lock-free empty check for stream connections. Callers still
	// take submitMu before inspecting or mutating the segment list itself.
	return conn.pending.Load() == 0
}

func (conn *fdConn) OutboundBuffered() int { return int(conn.pending.Load()) }

// prepareScratch claims turn-local reply space in holder.out when nothing else
// is queued ahead of it, or falls back with pending already reserved.
func (conn *fdConn) prepareScratch(n int) (out *bytebuf.Buffer, reserved bool, err error) {
	if holder := conn.scratch; holder != nil && conn.turn&turnFlushing == 0 {
		took := !conn.turnHoldsWrite()
		if !took || conn.tryClaimWrite(writeTurnHeldFlag) {
			ok, ferr := conn.reservePendingAfterFlush(int64(n))
			if ferr == nil && !ok {
				ferr = conn.errOutboundOverflow()
			}
			if ferr != nil {
				if took {
					conn.releaseWriteAndKick()
				}
				return nil, false, ferr
			}
			buf := holder.out
			used := 0
			if buf != nil {
				used = buf.Len()
			}
			if conn.pending.Load() == int64(used+n) {
				if buf == nil {
					buf = bytebuf.AcquireBuffer(conn.coalesceBlockSize())
					holder.out = buf
				}
				if buf.Available() >= n {
					return buf, true, nil
				}
				conn.handOverScratch()
			}
			return nil, true, nil
		}
	}
	ok, ferr := conn.reservePendingAfterFlush(int64(n))
	if ferr != nil {
		return nil, false, ferr
	}
	if !ok {
		return nil, false, conn.errOutboundOverflow()
	}
	return nil, true, nil
}

func (conn *fdConn) writeQueuedOnLoop(data []byte) (int, error) {
	n := len(data)
	out, _, err := conn.prepareScratch(n)
	if err != nil {
		return 0, err
	}
	if out != nil {
		dst := out.AvailableBuffer()[:n]
		copy(dst, data)
		out.CommitWrite(n)
		return n, nil
	}
	conn.submitMu.Lock()
	conn.outWriteCoalesced(data, conn.coalesceBlockSize())
	conn.submitMu.Unlock()
	return n, nil
}

func (conn *fdConn) writevQueuedOnLoop(vec [][]byte, total int) (int, error) {
	out, _, err := conn.prepareScratch(total)
	if err != nil {
		return 0, err
	}
	if out != nil {
		dst := out.AvailableBuffer()[:total]
		off := 0
		for _, seg := range vec {
			off += copy(dst[off:], seg)
		}
		out.CommitWrite(total)
		return total, nil
	}
	conn.submitMu.Lock()
	conn.outWritevCoalesced(vec, conn.coalesceBlockSize())
	conn.submitMu.Unlock()
	return total, nil
}

func (conn *fdConn) handOverScratch() {
	if holder := conn.scratch; holder != nil && holder.out != nil && holder.out.Len() > 0 {
		conn.queueDirectSuffix(holder.out)
		holder.out = nil
	}
}

func (conn *fdConn) dropScratch() {
	holder := conn.scratch
	if holder == nil {
		return
	}
	conn.scratch = nil
	if out := holder.out; out != nil {
		if n := out.Len(); n > 0 {
			conn.pending.Add(-int64(n))
		}
		bytebuf.ReleaseBuffer(out)
		holder.out = nil
	}
	conn.events().readPool.Put(holder)
}

func (conn *fdConn) sendScratch(holder *readBuffer) error {
	out := holder.out
	if conn.pending.Load() != int64(out.Len()) || conn.writeBlocked() || conn.isClosing() {
		conn.queueDirectSuffix(out)
		holder.out = nil
		return nil
	}
	var sendErr error
	unsent := out.Bytes()
	for len(unsent) > 0 {
		written, err := socket.Send(int(conn.fd), unsent)
		if err != nil && !isWouldBlock(err) {
			sendErr = err
			break
		}
		if err != nil || written <= 0 {
			if conn.markWriteBlocked() {
				continue
			}
			break
		}
		unsent = unsent[written:]
	}
	sent := out.Len() - len(unsent)
	if len(unsent) > 0 {
		if sent > 0 {
			out.Discard(sent)
		}
		conn.queueDirectSuffix(out)
		holder.out = nil
	} else {
		out.Reset()
	}
	if sent > 0 {
		conn.turn |= turnFlushing
		_, _ = conn.retireFlush(sent, nil)
		conn.turn &^= turnFlushing
	}
	return sendErr
}
