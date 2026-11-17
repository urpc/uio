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
	"net"
	"sync/atomic"

	"github.com/urpc/uio/internal/blockio"
	"github.com/urpc/uio/internal/poller"
)

// Readiness bits are the poller's own: noteIO folds them in unchanged.
const (
	ioEventRead   = uint32(poller.ReadEvents)
	ioEventWrite  = uint32(poller.WriteEvents)
	ioEventHangup = uint32(poller.HangupEvents)
)

const (
	ioEventOpen  uint32 = 1 << 16
	ioEventWake  uint32 = 1 << 17
	ioEventClose uint32 = 1 << 18
	// ioEventAccepted rides with ioEventOpen for an accepted TCP connection,
	// whose open turn applies the default socket options.
	ioEventAccepted uint32 = 1 << 19
	// ioEventPeers asks a UDP server's turn to take the children queued for
	// close or wake-up.
	ioEventPeers uint32 = 1 << 20
	// ioEventTeardown asks the turn of a closing connection to release it.
	ioEventTeardown uint32 = 1 << 21
	// ioEventShutWrite asks the connection's turn to shut the writing half a
	// CloseWrite requested; see shutWrite.
	ioEventShutWrite uint32 = 1 << 22
	// ioEventResume delivers retained input before reading the socket again:
	// an EOF queued behind it must not close the connection before delivery.
	ioEventResume uint32 = 1 << 23
	// taskScheduledBit claims the connection's one queued or running task. It
	// shares taskState with the event bits so a producer sets claim and events
	// with one atomic.
	taskScheduledBit uint32 = 1 << 31
)

// scheduleIO records readiness or a synthetic event and submits the connection
// only when it transitions from idle to scheduled.
func (conn *fdConn) scheduleIO(events uint32) {
	if !conn.noteIO(events) {
		return
	}
	if !conn.loop.ioPool.submit(conn) {
		conn.handleIOSubmitFailure(net.ErrClosed)
	}
}

// noteIO folds readiness into this connection and reports whether the caller
// acquired responsibility for submitting its single in-flight task. The turn
// reservation is taken before the scheduling claim becomes visible: shutdown
// joins turns through this count, so every published claim must be one the
// stop barrier can wait out. Events are folded before ownership is decided, so
// a finishing turn resubmits for them and a released claim stays free to
// take. A closing connection takes nothing but the request to release it.
func (conn *fdConn) noteIO(events uint32) bool {
	if conn.loop == nil || conn.loop.stopping.Load() || conn.loop.ioPool == nil {
		return false
	}
	if conn.isClosing() && (events&ioEventTeardown == 0 || conn.close.isReleased()) {
		return false
	}
	if events != 0 {
		// A caller may only want the task (claiming a first turn folds
		// nothing); an empty word needs no read-modify-write.
		conn.taskState.Or(events)
	}
	if conn.taskState.Load()&taskScheduledBit != 0 {
		return false
	}
	if !conn.loop.acquireIO() {
		// The loop stopped before this turn was reserved. Its close pass
		// covers the connection, whatever the events would have done with it.
		return false
	}
	if conn.taskState.Or(taskScheduledBit)&taskScheduledBit != 0 {
		// A concurrent producer claimed first; the events are already folded
		// into the word for its turn, and this reservation is not needed.
		conn.loop.releaseIO()
		return false
	}
	if testHookTaskClaimed != nil {
		testHookTaskClaimed(conn)
	}
	return true
}

// testHookTaskClaimed runs on the producer's goroutine right after a turn's
// scheduling claim becomes visible, with its lifetime reservation already
// held. Tests use it to pin the shutdown race window; it is nil outside tests.
var testHookTaskClaimed func(*fdConn)

// testHookDirectSendRefused runs on the sender's goroutine right after a
// direct send was refused by a full socket, before its bytes are published.
// Tests use it to pin the writable-edge race window; it is nil outside tests.
var testHookDirectSendRefused func(*fdConn)

// watchTags generates registration tags. Zero means untagged.
var watchTags atomic.Uint32

// watcher returns the poller watching this descriptor.
func (conn *fdConn) watcher() *poller.Poller { return conn.loop.poller }

// assignWatchTag gives a new registration its own tag, so readiness reported
// for an earlier connection on the same descriptor number is not delivered to
// this one.
func (conn *fdConn) assignWatchTag() uint32 {
	tag := watchTags.Add(1)
	if tag == 0 {
		tag = watchTags.Add(1)
	}
	conn.pollTag.Store(tag)
	return tag
}

// watch registers the descriptor with its loop's poller, once and for good.
func (conn *fdConn) watch() error {
	tag := conn.assignWatchTag()
	return conn.watcher().Register(int(conn.fd), conn.initialInterest(), true, tag)
}

// admitAccepted claims the first turn of a connection a waiter just accepted,
// publishes it and watches its socket, and reports whether the caller must
// submit the turn, which runs OnOpen and then reads what the peer already
// sent. The claim comes first: from the fd table entry on, readiness can
// reach the connection, and a backend without registration tags can report
// an event its poller collected for the previous owner of the descriptor
// number; the claim leaves any such event nothing but to fold into the turn
// already queued. The waiter registers rather than that turn: the
// registrations of a loop's poller then come from its few waiters, not from
// every worker at once, and the kernel serializes them per poller.
//
// An accepted TCP connection whose default socket options are still to be
// applied also gets ioEventAccepted: its open turn applies them, off the
// waiter. Where the platform copies the listening socket's options to what it
// accepts (Linux; see setListenerOptions), there is nothing left to apply and
// accepting a connection costs no setsockopt at all.
func (conn *fdConn) admitAccepted(tcp bool) bool {
	if conn.loop == nil || conn.events().closing.Load() {
		conn.closeUnregistered()
		return false
	}
	events := ioEventOpen
	if tcp && !inheritAcceptedOptions {
		events |= ioEventAccepted
	}
	conn.taskState.Store(events)
	if !conn.noteIO(0) {
		conn.closeUnregistered()
		return false
	}
	if err := conn.loop.fdMap.Put(int(conn.fd), conn); err != nil {
		conn.abandonClaim()
		conn.closeUnregistered()
		return false
	}
	if err := conn.watch(); err != nil {
		conn.loop.fdMap.DeleteValue(int(conn.fd), conn)
		conn.closeUnregistered()
		conn.abandonClaim()
		return false
	}
	conn.loop.ioPool.connOpened()
	return true
}

// abandonClaim gives up a first turn that was claimed but will never be
// submitted, before anything ran.
func (conn *fdConn) abandonClaim() {
	conn.taskState.Store(0)
	conn.loop.releaseIO()
}

func (conn *fdConn) applyAcceptedOptions() {
	_ = conn.applySocketOption(optionNoDelay, 1)
	_ = conn.applySocketOption(optionKeepAlive, 1)
	_ = conn.applySocketOption(optionKeepAlivePeriod, defaultTCPKeepAliveSecs)
}

// takeIOEvents hands the pending events to the turn and keeps the task claim
// set: producers that arrive while the turn runs see the claim and leave their
// events for finishIOTask to pick up.
func (conn *fdConn) takeIOEvents() uint32 {
	return conn.taskState.Swap(taskScheduledBit)
}

// finishIOTask closes the lost-work race between the task's final event check
// and a concurrent producer. The claim is given up with the same atomic that
// checks for events, so either the current task resubmits itself or the
// producer that set them owns the next turn.
func (conn *fdConn) finishIOTask() {
	if conn.taskState.Load()&^taskScheduledBit != 0 && !conn.loop.ioStopped() && conn.close.phase.Load() != closeCallbackDelivered {
		if !conn.loop.ioPool.submit(conn) {
			conn.handleIOSubmitFailure(net.ErrClosed)
		}
		return
	}
	left := conn.taskState.And(^taskScheduledBit)
	rescheduled := false
	if left&^taskScheduledBit != 0 && !conn.loop.ioStopped() && conn.close.phase.Load() != closeCallbackDelivered &&
		conn.taskState.Or(taskScheduledBit)&taskScheduledBit == 0 {
		rescheduled = true // the current reservation moves to the next turn
		if !conn.loop.ioPool.submit(conn) {
			conn.handleIOSubmitFailure(net.ErrClosed)
		}
	}
	// Released last: shutdown joins turns through this count, so nothing of
	// this turn may touch the connection or its loop afterwards.
	if !rescheduled {
		conn.loop.releaseIO()
	}
}

// handleIOSubmitFailure ends a turn the executor refused. That turn will never
// run, so its handler stands in for it, the close it owes included: the
// connection closes, and is released here unless the loop is shutting down,
// whose pass releases it then. A close handed back while the handler held the
// claim found the claim taken and left its request in taskState, so the
// handler hands it on once it gives the claim up.
func (conn *fdConn) handleIOSubmitFailure(err error) {
	// Deferred, so a panic in OnClose that the rejection queue's executor
	// recovers still ends the turn and returns its reservation.
	defer func() {
		left := conn.taskState.And(^taskScheduledBit)
		if left&ioEventTeardown != 0 && conn.isClosing() && !conn.close.isReleased() {
			conn.scheduleTeardown()
		} else if left&ioEventClose != 0 && conn.close.phase.Load() == closeResourcesReleased &&
			conn.taskState.Or(taskScheduledBit)&taskScheduledBit == 0 {
			// The close flush left OnClose to a turn; this claim is that turn's.
			conn.loop.acquireCloseIO()
			if !conn.loop.ioPool.submit(conn) {
				conn.taskState.And(^taskScheduledBit)
				conn.fireCloseCallback()
				conn.loop.releaseIO()
			}
		}
		if conn.loop != nil {
			conn.loop.releaseIO()
		}
	}()
	phase := conn.close.phase.Load()
	if phase < closeResourcesReleased {
		conn.beginClose(err)
		if conn.loop != nil && !conn.loop.stopping.Load() {
			if finalErr, ok := conn.teardown(nil); ok {
				conn.deliverClose(finalErr)
			}
		}
	} else if phase == closeResourcesReleased && conn.taskState.Load()&ioEventClose != 0 {
		// scheduleCloseCallback publishes the final cause before setting
		// ioEventClose. A rejected earlier task must not deliver OnClose
		// during that gap.
		conn.fireCloseCallback()
	}
}

// runIOTask performs one serialized connection turn. Ordering matters: close
// is terminal, writable readiness drains old output before new input is read,
// read callbacks may enqueue corked replies, and one final flush publishes the
// complete round before the loop updates interest. The deferred epilogue is
// the only way out, so a callback panic that an Executor recovers ends the
// turn like a return does: the claim goes back and the loop learns what the
// turn left behind.
//
// A stream's open turn runs OnOpen and then reads whatever the peer already
// sent. A turn that finds its connection closing releases it as it ends and
// delivers OnClose in the same turn.
func (conn *fdConn) runIOTask() {
	conn.ioOwner.Store(currentGoroutineID())
	var events uint32
	ended := false
	// Registered first, so it runs last: a callback panic that an Executor
	// recovers, OnClose's included, still ends the turn and returns its
	// reservation, which shutdown joins.
	defer func() {
		conn.ioOwner.Store(0)
		conn.finishIOTask()
	}()
	defer func() {
		// A callback panic may have left a round corked or a flush marked;
		// only what the rounds must read on for outlives the turn.
		conn.turn &= turnHangup | turnReadToEnd
		conn.inboundTail = nil
		if conn.scratch != nil {
			conn.dropScratch()
		}
		if conn.turnOwnsClaim() {
			// A callback panic left the claim taken for a flush: other senders
			// may take the socket again, starting with output that waited.
			conn.releaseWrite()
			if conn.pending.Load() != 0 {
				conn.kickWriter()
			}
		} else if !ended && conn.pending.Load() != 0 {
			// After a panic in OnOpen, output queued by other goroutines in
			// the meantime has no sender until someone kicks it.
			conn.kickWriter()
		}
		if conn.close.isDraining() {
			if conn.pending.Load() == 0 {
				// The turn drained a graceful close's queue itself.
				conn.escalateDrain()
			} else {
				conn.ensureDrainTimer()
			}
		}
		if conn.isClosing() {
			conn.closeInTurn()
		}
	}()
	events = conn.takeIOEvents()
	reportedHangup := events&ioEventHangup != 0
	if events&ioEventClose != 0 {
		conn.fireCloseCallback()
		return
	}
	if conn.close.isReleased() {
		return
	}
	if events&ioEventOpen != 0 && !conn.isDatagram() {
		// The peer's first bytes are usually here already.
		events |= ioEventRead
		if !poller.Tagged && events&ioEventHangup != 0 {
			// Without registration tags the hangup may have been reported for
			// an earlier owner of the descriptor number, so it does not stick;
			// the round reads to the end of what the socket holds instead.
			events &^= ioEventHangup
			conn.turn |= turnReadToEnd
		}
	}
	if events&ioEventOpen != 0 {
		if events&ioEventAccepted != 0 {
			conn.applyAcceptedOptions()
		}
		conn.openOutputAfterOnOpen()
	}
	// The event bits are tested before the state the branches read: most
	// turns carry one event, and a branch whose bit is clear costs nothing
	// further.
	if events&ioEventWrite != 0 && !conn.isClosing() {
		conn.noteWritable()
		if _, err := conn.flushOnLoop(); err != nil {
			conn.requestClose(err)
		}
	}
	if events&ioEventResume != 0 && !conn.isClosing() {
		// A released pause: what the consumer retained goes to it before the
		// socket is read again, so the end of the stream cannot overtake it.
		conn.turn |= turnCorked
		if err := conn.fireOnRead(); err != nil {
			conn.requestClose(err)
		}
		conn.turn &^= turnCorked
		if conn.readHeld() {
			conn.resumeHeldRead()
		}
		conn.closeDeferredReadEOF()
		conn.closeIfEnded()
		if !conn.ReadPaused() {
			events |= ioEventRead
		}
	}
	if events&ioEventHangup != 0 && !conn.isDatagram() {
		// Kept even on a turn that reads nothing, so the rounds that later
		// read the bytes ahead of the hangup read on to its end.
		conn.turn |= turnHangup
	}
	if events&ioEventRead != 0 && !conn.isClosing() {
		var err error
		if conn.isDatagram() {
			err = conn.onRecvUDP()
		} else if flags := conn.flags.Load(); flags&(readHeldFlag|readEOFFlag) == 0 {
			err = conn.onRead() // a pause stops the round before its first read
		} else if flags&readEOFFlag != 0 && events&ioEventHangup != 0 {
			conn.turn |= turnReadDrained
			err = conn.probeAfterEOF()
		}
		if err != nil {
			conn.requestClose(err)
		}
	}
	if reportedHangup && conn.turn&turnReadDrained == 0 && !conn.isDatagram() && !conn.isClosing() {
		// No read reached the hangup: a pause or a capacity hold left the
		// socket unread. A reset must still end the connection, which only
		// the socket error tells without consuming input; zero leaves an
		// orderly end of stream to the reads that come for it.
		if err := conn.pendingSocketError(); err != nil {
			conn.requestClose(err)
		}
	}
	if events&ioEventWake != 0 && !conn.isClosing() {
		conn.turn |= turnCorked
		if err := conn.fireOnRead(); err != nil {
			conn.requestClose(err)
		}
		conn.turn &^= turnCorked
		if flags := conn.flags.Load(); flags&readHeldFlag != 0 {
			conn.resumeHeldRead()
		} else if flags&readEOFFlag != 0 {
			conn.closeIfEnded()
		}
		conn.closeDeferredReadEOF()
	}
	if conn.udp != nil && conn.udp.peers != nil {
		conn.settlePeers()
	}
	if conn.pending.Load() != 0 && !conn.isClosing() {
		if _, err := conn.flushOnLoop(); err != nil {
			conn.requestClose(err)
		}
	}
	if !conn.isClosing() && (events&ioEventShutWrite != 0 || conn.taskState.Load()&ioEventShutWrite != 0) {
		// A CloseWrite this turn's own callbacks made is carried out now,
		// not by a turn of its own: a half-open connection answering its
		// peer's end of stream then closes in the turn that read it.
		conn.taskState.And(^ioEventShutWrite)
		conn.shutWrite()
	}
	ended = true
}

// RunTask implements IOTask for injected executors. The executor contract
// guarantees asynchronous dispatch; the loop's turn count covers the turn.
func (conn *fdConn) RunTask() {
	if conn.loop == nil || conn.loop.ioPool == nil {
		return
	}
	blockio.EnterTurn()
	defer blockio.LeaveTurn()
	conn.runIOTask()
}
