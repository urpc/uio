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
	"sync"

	"github.com/urpc/uio/internal/bytebuf"
	"github.com/urpc/uio/internal/socket"
)

// nativeWriteVecLimit bounds stack use while allowing a read event's small
// owned writes to drain in one syscall.
const nativeWriteVecLimit = 64

// inflightPool lends flushes the buffer that holds a detached batch, so an idle
// connection keeps no second queue.
var inflightPool = sync.Pool{New: func() any { return new(bytebuf.CompositeBuffer) }}

// releaseInflight returns the borrowed batch. Only the holder of the write
// claim calls it; close keeps the claim it takes, so nothing sends afterwards.
func (conn *fdConn) releaseInflight() {
	if batch := conn.inflight; batch != nil {
		conn.inflight = nil
		batch.Reset()
		inflightPool.Put(batch)
	}
}

// flushOnLoop is the connection turn's flush. The turn sends inline when it
// holds or can take the write claim, so replies produced by its callbacks
// leave without a scheduler hop; when another sender holds the claim, that
// sender's release rechecks and these bytes go with it.
func (conn *fdConn) flushOnLoop() (int, error) {
	// pending is incremented before a producer publishes its buffer and
	// decremented only after the corresponding bytes are sent. A zero
	// value therefore proves that there is no stream payload to flush, while
	// avoiding any atomic read-modify-write on the common empty path. It
	// counts the round's reply space too, which has nothing to hand over then.
	if conn.isDatagram() || conn.pending.Load() == 0 {
		return 0, nil
	}
	conn.handOverScratch()
	// OnBytesWritten runs inside the flush, so the turn marks it: a Write that
	// callback makes is queued rather than sent, and a flush it starts returns
	// at once, so the callback never runs inside itself. A claim taken here
	// is marked too, so the turn's cleanup releases it if the callback panics.
	if conn.turn&turnFlushing != 0 {
		return 0, nil
	}
	held := conn.writeState.Load()&writeTurnHeldFlag != 0
	if !held && !conn.tryClaimWrite(writeTurnFlushFlag) {
		return 0, nil
	}
	conn.turn |= turnFlushing
	n, err := conn.flushWrite()
	conn.turn &^= turnFlushing
	if !held {
		conn.releaseWriteAndKick()
	}
	return n, err
}

// flushWrite drains a bounded number of bytes and writev calls. It requires
// the write claim, and submitMu never covers a send, so a producer appending
// to outbound never waits for the socket. A queue of one block, the usual
// shape after a round of replies, is sent in place: producers only append,
// into the tail block's spare room or as new blocks, and only the claim holder
// discards, so the bytes a send reads stay put while the lock is released.
// A longer queue is detached into inflight in one step instead, so its sends
// and the release of its sent blocks also run without the lock while producers
// start the next batch; inflight goes out ahead of everything accepted since.
// Queued file ranges go out once both are empty, within fileFlushBudget for
// the whole flush. EAGAIN leaves the bytes queued and sets writeBlocked until
// a writable edge. A failed stream sends nothing more.
func (conn *fdConn) flushWrite() (int, error) {
	if conn.pending.Load() == 0 {
		return 0, nil
	}
	if state := conn.writeState.Load(); state&(writeBlockedFlag|writeFailedFlag) != 0 {
		// Once EAGAIN is observed, only a Writable event should retry the fd.
		return 0, nil
	}
	var totalWritten int
	var writeErr error
	for {
		if conn.inflight == nil {
			conn.submitMu.Lock()
			if conn.out.bytes.Blocks() == 1 {
				var n int
				n, writeErr = conn.sendOutboundBlock()
				totalWritten += n
			}
			if writeErr != nil || conn.writeBlocked() {
				conn.submitMu.Unlock()
				return conn.retireFlush(totalWritten, writeErr)
			}
			if conn.out.bytes.Empty() {
				conn.submitMu.Unlock()
				if conn.writeState.Load()&writeFileTailFlag != 0 && totalWritten < fileFlushBudget {
					var fn int
					var more bool
					fn, more, writeErr = conn.sendQueuedFiles(fileFlushBudget - totalWritten)
					totalWritten += fn
					if more && writeErr == nil && !conn.writeBlocked() && totalWritten < fileFlushBudget {
						continue
					}
				}
				return conn.retireFlush(totalWritten, writeErr)
			}
			batch := inflightPool.Get().(*bytebuf.CompositeBuffer)
			// batch is empty, so the swap leaves outbound empty for producers.
			conn.out.bytes, *batch = *batch, conn.out.bytes
			conn.submitMu.Unlock()
			conn.inflight = batch
		}
		var written int
		if conn.inflight.Blocks() == 1 {
			written, writeErr = conn.sendInflightBlock()
		} else {
			written, writeErr = conn.sendInflightVec()
		}
		totalWritten += written
		if conn.inflight.Empty() {
			conn.releaseInflight()
			if writeErr == nil && !conn.writeBlocked() && conn.writeState.Load()&writeFileTailFlag != 0 &&
				totalWritten < fileFlushBudget {
				continue
			}
		}
		return conn.retireFlush(totalWritten, writeErr)
	}
}

// retireFlush accounts what a flush sent and reports it.
func (conn *fdConn) retireFlush(written int, err error) (int, error) {
	if written > 0 {
		conn.retireSent(written)
		conn.events().onSocketBytesWrite(conn, written)
		if conn.close.isDraining() {
			// A drain's queue is moving: its bound starts over, so a close
			// waits as long as its output keeps leaving, not a fixed time.
			if w := conn.wake.Load(); w != nil {
				if bound := w.drain.Load(); bound != nil {
					bound.timer.Reset(bound.wait)
				}
			}
		}
	}
	return written, err
}

// retireSent retires bytes the socket took. Producers only add to pending and
// every add and retire is atomic, so the decrement needs no further
// coordination.
func (conn *fdConn) retireSent(n int) {
	conn.pending.Add(-int64(n))
}

// sendOutboundBlock sends a queue of one block in place, with plain write and
// no vector. It is called and returns with submitMu held, releases it around
// each send, and stops once producers have queued a second block.
func (conn *fdConn) sendOutboundBlock() (int, error) {
	totalWritten := 0
	for calls := 0; calls < 16 && conn.out.bytes.Blocks() == 1; calls++ {
		chunk := conn.out.bytes.PeekChunk()
		conn.submitMu.Unlock()
		written, err := socket.Send(int(conn.fd), chunk)
		conn.submitMu.Lock()
		if err != nil && !isWouldBlock(err) {
			return totalWritten, err
		}
		if err != nil || written <= 0 {
			if conn.markWriteBlocked() {
				continue
			}
			return totalWritten, nil
		}
		conn.out.bytes.Discard(written)
		totalWritten += written
	}
	return totalWritten, nil
}

// sendInflightBlock sends a detached batch of one block with plain write.
func (conn *fdConn) sendInflightBlock() (int, error) {
	totalWritten := 0
	for calls := 0; calls < 16 && !conn.inflight.Empty(); calls++ {
		written, err := socket.Send(int(conn.fd), conn.inflight.PeekChunk())
		if err != nil && !isWouldBlock(err) {
			return totalWritten, err
		}
		if err != nil || written <= 0 {
			if conn.markWriteBlocked() {
				continue
			}
			return totalWritten, nil
		}
		conn.inflight.Discard(written)
		totalWritten += written
	}
	return totalWritten, nil
}

// sendInflightVec drains a detached multi-block batch in bounded writev
// batches. Its vector is kept out of flushWrite's frame, which would otherwise
// clear it on every flush.
//
//go:noinline
func (conn *fdConn) sendInflightVec() (int, error) {
	var vecStorage [nativeWriteVecLimit][]byte
	totalWritten := 0
	for calls := 0; calls < 16 && totalWritten < 1<<20 && !conn.inflight.Empty(); calls++ {
		vec, _ := conn.inflight.PeekVecN(vecStorage[:0], len(vecStorage))
		written, err := socket.Writev(int(conn.fd), vec)
		if err != nil && !isWouldBlock(err) {
			return totalWritten, err
		}
		if err != nil || written <= 0 {
			if conn.markWriteBlocked() {
				continue
			}
			break
		}
		conn.inflight.Discard(written)
		totalWritten += written
	}
	return totalWritten, nil
}
