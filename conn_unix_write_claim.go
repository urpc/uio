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

// The write claim and write turns: who writes the socket, when a turn hands
// on, and what a blocked or failed socket asks of the next turn.

package uio

import (
	"net"
	"runtime"

	"github.com/urpc/uio/internal/blockio"
)

func claimerTag() uint32 { return uint32(currentGoroutineID()) << writeClaimerShift }

func (conn *fdConn) writeBlocked() bool { return conn.writeState.Load()&writeBlockedFlag != 0 }

func (conn *fdConn) writeFailed() bool { return conn.writeState.Load()&writeFailedFlag != 0 }

func (conn *fdConn) setWriteBlocked(blocked bool) {
	conn.updateWriteState(writeBlockedFlag, blocked)
}

// noteWritable handles a writable edge: the socket has room again.
func (conn *fdConn) noteWritable() {
	for {
		old := conn.writeState.Load()
		next := old&^writeBlockedFlag | writeEdgeFlag
		if next == old || conn.writeState.CompareAndSwap(old, next) {
			return
		}
	}
}

// markWriteBlocked records the EAGAIN its caller just got. If a writable edge
// was handled since the claim was taken, the room it reported may have come
// after that send, so the edge is consumed and the caller retries.
func (conn *fdConn) markWriteBlocked() (retry bool) {
	for {
		old := conn.writeState.Load()
		next := old | writeBlockedFlag
		if old&writeEdgeFlag != 0 {
			next = old &^ writeEdgeFlag
		}
		if conn.writeState.CompareAndSwap(old, next) {
			return next&writeBlockedFlag == 0
		}
	}
}

func (conn *fdConn) markWriteFailed() { conn.updateWriteState(writeFailedFlag, true) }

func (conn *fdConn) updateWriteState(flag uint32, enabled bool) {
	for {
		old := conn.writeState.Load()
		next := old | flag
		if !enabled {
			next &^= flag
		}
		// Every flush clears the blocked flag; most find it clear already.
		if next == old || conn.writeState.CompareAndSwap(old, next) {
			return
		}
	}
}

func (conn *fdConn) writeClaimed() bool { return conn.writeState.Load()&writeOwnerFlag != 0 }

func (conn *fdConn) turnHoldsWrite() bool {
	return conn.writeState.Load()&writeTurnHeldFlag != 0 && conn.turn&turnFlushing == 0
}

// turnOwnsClaim reports any claim the connection's turn must release.
func (conn *fdConn) turnOwnsClaim() bool {
	return conn.writeState.Load()&(writeTurnHeldFlag|writeTurnFlushFlag) != 0
}

// tryClaimWrite takes the write claim, adding extra (turn flags or a claimer
// tag) in the same step. It never waits: a taken claim means its holder sends.
func (conn *fdConn) tryClaimWrite(extra uint32) bool {
	for {
		old := conn.writeState.Load()
		if old&writeOwnerFlag != 0 {
			return false
		}
		next := old&^(writeEdgeFlag|writeContendedFlag|writeClaimerMask) | writeOwnerFlag | extra
		if conn.writeState.CompareAndSwap(old, next) {
			return true
		}
	}
}

// releaseWrite gives the claim up. A close that found it taken asked, through
// writeCloseWaitFlag, to be handed back to the connection's turn now.
func (conn *fdConn) releaseWrite() {
	old := conn.writeState.And(^(writeOwnerFlag | writeTurnHeldFlag | writeTurnFlushFlag))
	if old&writeCloseWaitFlag != 0 {
		conn.writeState.And(^writeCloseWaitFlag)
		// A stopped loop's shutdown pass releases the connection instead.
		conn.scheduleTeardown()
	}
	if old&(writeShutFlag|writeShutDoneFlag) == writeShutFlag && conn.pending.Load() == 0 {
		// Every send happens under the claim, so the release that leaves
		// nothing pending is when a CloseWrite can shut the writing half.
		// Output still pending waits for a writable edge, and the release
		// after the send that drains it schedules this then; scheduling on
		// every release would spin turns while the socket is full.
		conn.scheduleIO(ioEventShutWrite)
	}
	if conn.close.isDraining() && conn.pending.Load() == 0 {
		// A graceful close's queue left the socket as this holder sent: a
		// turn ends the drain, since none is owed for empty output.
		conn.scheduleTeardown()
	}
}

// releaseWriteAndKick releases the claim and then sends whatever producers
// appended while it was held: they found the claim taken and left their
// bytes to this holder.
func (conn *fdConn) releaseWriteAndKick() {
	conn.releaseWrite()
	conn.kickWriter()
}

// kickWriterOutside is kickWriter for a goroutine outside the connection's
// turns, such as a Write from an RPC handler. When the write turn it starts
// needed a worker woken, the caller yields, so the worker runs now rather
// than once the caller next blocks (see shardpool.Pool.Submit).
func (conn *fdConn) kickWriterOutside() {
	if conn.kickWriter() {
		runtime.Gosched()
	}
}

// kickWriter gives accepted output a sender. If the claim is free it takes it
// and hands it to a write turn; if it is taken, the holder's release rechecks.
// Output waits for the open turn before OnOpen returns, for a writable edge
// while the socket is full, and for the close flush once closing started.
// It reports whether the write turn it started needed a worker woken.
func (conn *fdConn) kickWriter() bool {
	if conn.isDatagram() || conn.pending.Load() == 0 || conn.isClosing() {
		return false
	}
	state := conn.writeState.Load()
	if state&writeOwnerFlag != 0 {
		// The holder sends these bytes. Output from a goroutine other than
		// the one its claim was taken for means several producers write now.
		if state&writeContendedFlag == 0 && state&writeClaimerMask != claimerTag() {
			conn.writeState.Or(writeContendedFlag)
		}
		return false
	}
	if state&writeBlockedFlag != 0 || state&writeOpenedFlag == 0 {
		return false
	}
	if !conn.tryClaimWrite(claimerTag()) {
		return false
	}
	if !conn.loop.acquireIO() {
		// Shutting down: its close pass sends the bounded final flush.
		conn.releaseWrite()
		return false
	}
	accepted, woke := conn.loop.ioPool.submitWriteWaking(conn)
	if !accepted {
		conn.handleWriteSubmitFailure()
	}
	return woke
}

// handleWriteSubmitFailure ends a write turn the scheduler refused. Its bytes
// have no sender left, so the connection closes, as with a refused turn.
func (conn *fdConn) handleWriteSubmitFailure() {
	conn.releaseWrite()
	conn.requestClose(net.ErrClosed)
	conn.loop.releaseIO()
}

// writeTurn is a connection seen as its write task. It is the same value, so
// submitting a write turn allocates nothing.
type writeTurn fdConn

// ShardKey implements IOTask: a connection's turns and its write turns share
// one key; see turnKey.
func (conn *fdConn) ShardKey() uint32 { return conn.turnKey() }

func (turn *writeTurn) ShardKey() uint32 { return (*fdConn)(turn).turnKey() }

// turnKey keys a connection's turns by its loop first, its descriptor second.
// The loop collects their readiness and hands them over in one batch per
// round, and where the shards are a multiple of the loops, as they are by
// default, the loop's connections share a subset of the shards of their own:
// a round's batch then wakes the few workers serving that subset with several
// turns each, rather than one worker per turn. Keyed by descriptor alone, as
// the loops pick connections in turn (see Events.loopAt), a closed-loop echo's
// p99 at 512 connections rose by 60%.
func (conn *fdConn) turnKey() uint32 {
	if conn.loop == nil {
		return uint32(conn.fd)
	}
	return conn.loop.index + conn.loop.count*uint32(conn.fd)
}

// RunTask implements IOTask for the write turn.
func (turn *writeTurn) RunTask() {
	conn := (*fdConn)(turn)
	if conn.loop == nil || conn.loop.ioPool == nil {
		return
	}
	blockio.EnterTurn()
	defer blockio.LeaveTurn()
	conn.runWriteTurn()
}

// runWriteTurn sends output from other goroutines while the connection's turn
// reads. It holds the write claim and one of its loop's turn counts, which
// shutdown joins. It never runs callbacks other than OnBytesWritten.
func (conn *fdConn) runWriteTurn() {
	handedOn := false
	defer func() {
		// Every exit but the hand-on ends the turn here, including a panic in
		// OnBytesWritten, which runs after a flush may have left the socket
		// blocked: the claim and the count go back and writable interest is
		// armed whether or not the flush returned.
		if !handedOn {
			conn.finishWriteTurn()
		}
	}()
	for flushes := 0; ; flushes++ {
		if conn.isClosing() || conn.pending.Load() == 0 || conn.writeBlocked() {
			return
		}
		if flushes == maxWriteFlushes {
			handedOn = true
			// The claim and the count move to the next turn in the queue.
			if !conn.loop.ioPool.submitWrite(conn) {
				conn.handleWriteSubmitFailure()
			}
			return
		}
		if conn.writeState.Load()&writeContendedFlag != 0 &&
			conn.pending.Load() < int64(conn.coalesceBlockSize()) {
			// Other goroutines queued output while this turn held the claim,
			// so more are likely about to: those ready to run go first and add
			// to this batch, instead of costing a syscall each few messages.
			// A lone producer never sets the flag and is sent at once; a queue
			// that already fills a coalescing block is sent as it is.
			conn.writeState.And(^writeContendedFlag)
			runtime.Gosched()
		}
		if _, err := conn.flushWrite(); err != nil {
			conn.requestClose(err)
			return
		}
	}
}

// finishWriteTurn ends a write turn. A socket that refused its bytes needs no
// interest armed: write interest has been on since registration, so the edge
// that drains the socket starts the next turn by itself. Bytes appended after
// its last flush found the claim taken and were left to it, so it looks again
// and hands them, with its turn count, to a new write turn.
func (conn *fdConn) finishWriteTurn() {
	conn.releaseWrite()
	if conn.pending.Load() != 0 && !conn.writeBlocked() && !conn.isClosing() &&
		!conn.loop.ioStopped() && conn.tryClaimWrite(0) {
		if !conn.loop.ioPool.submitWrite(conn) {
			conn.handleWriteSubmitFailure()
		}
		return
	}
	// Released last: shutdown joins write turns through this count.
	conn.loop.releaseIO()
}
