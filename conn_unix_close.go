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

// The close walk: one state machine from the close request through the drain,
// the descriptor release and the OnClose callback — with the half-close the
// read side shares.

package uio

import (
	"errors"
	"io"
	"net"
	"runtime"
	"sync/atomic"
	"syscall"
	"time"
)

// connCloseState separates lifecycle from event scheduling. deferred is
// protected by submitMu; a non-nil request can carry a nil close cause.
type connCloseState struct {
	phase    atomic.Uint32
	deferred *closeRequest
}

// closeRequest is never modified once recorded, so the requests for the
// causes most closes carry are shared instead of allocated per close.
type closeRequest struct{ cause error }

func newCloseRequest(err error) *closeRequest {
	switch err {
	case nil:
		return closeWithoutCause
	case io.EOF:
		return closeAtEOF
	case syscall.ECONNRESET:
		return closeAtReset
	}
	return &closeRequest{cause: err}
}

func (state *connCloseState) isClosing() bool { return state.phase.Load() >= closeRequested }

func (state *connCloseState) isDraining() bool { return state.phase.Load() == closeDraining }

func (state *connCloseState) isReleased() bool { return state.phase.Load() >= closeResourcesReleased }

// request advances to closeRequested from open or draining — the first call
// wins, and a close requested during a drain ends the wait at once, as an
// abort does a graceful close's.
func (state *connCloseState) request() bool {
	for {
		phase := state.phase.Load()
		if phase >= closeRequested {
			return false
		}
		if state.phase.CompareAndSwap(phase, closeRequested) {
			return true
		}
	}
}

// drain claims the connection for a graceful close: open moves to draining,
// where output still leaves and nothing new is accepted.
func (state *connCloseState) drain() bool {
	return state.phase.CompareAndSwap(closeOpen, closeDraining)
}

// escalate ends a drain by advancing it to closeRequested.
func (state *connCloseState) escalate() bool {
	return state.phase.CompareAndSwap(closeDraining, closeRequested)
}

func (state *connCloseState) abandon() bool {
	for {
		phase := state.phase.Load()
		if phase >= closeResourcesReleased {
			return false
		}
		if state.phase.CompareAndSwap(phase, closeCallbackDelivered) {
			return true
		}
	}
}

func (state *connCloseState) release() bool {
	for {
		phase := state.phase.Load()
		if phase >= closeResourcesReleased {
			return false
		}
		if state.phase.CompareAndSwap(phase, closeResourcesReleased) {
			return true
		}
	}
}

// closeIfEnded closes a half-open connection whose two halves have both
// ended, once a callback consumed the last bytes its peer sent.
func (conn *fdConn) closeIfEnded() {
	if conn.writeState.Load()&writeShutDoneFlag != 0 && conn.readEnded() && conn.readCloseReady() {
		conn.requestClose(io.EOF)
	}
}

// CloseWrite shuts the writing half once everything accepted before it has
// been sent; see Conn.CloseWrite.
func (conn *fdConn) CloseWrite() error {
	if conn.isDatagram() {
		return ErrUnsupported
	}
	if conn.loop == nil {
		return net.ErrClosed
	}
	conn.submitMu.Lock()
	if conn.isClosing() || conn.events().closing.Load() || conn.loop.stopping.Load() {
		conn.submitMu.Unlock()
		return net.ErrClosed
	}
	if conn.writeState.Load()&writeShutFlag != 0 {
		conn.submitMu.Unlock()
		return nil
	}
	conn.writeState.Or(writeShutFlag)
	conn.submitMu.Unlock()
	conn.scheduleIO(ioEventShutWrite)
	return nil
}

// shutWrite shuts the writing half a CloseWrite requested, once nothing
// accepted is left to send. It runs in the connection's turn, so no callback
// writes meanwhile, and it takes the write claim, so no other sender does;
// output still queued, or a sender holding the claim, schedules it again when
// the claim is released. A half-open connection whose reading half has ended
// as well closes now.
func (conn *fdConn) shutWrite() {
	state := conn.writeState.Load()
	if state&(writeShutFlag|writeShutDoneFlag) != writeShutFlag || conn.isClosing() || conn.pending.Load() != 0 {
		return
	}
	if !conn.tryClaimWrite(0) {
		return
	}
	if conn.pending.Load() != 0 {
		conn.releaseWriteAndKick()
		return
	}
	if conn.readEnded() && conn.readCloseReady() {
		// Both halves end: closing sends the FIN behind the output already
		// sent, without a shutdown whose acknowledgement would wake the
		// poller for a socket about to be released.
		conn.writeState.Or(writeShutDoneFlag)
		conn.releaseWrite()
		conn.requestClose(io.EOF)
		return
	}
	err := syscall.Shutdown(int(conn.fd), syscall.SHUT_WR)
	// Done before the release, so the release does not schedule this again.
	conn.writeState.Or(writeShutDoneFlag)
	conn.releaseWrite()
	if err != nil {
		conn.requestClose(err)
		return
	}
	conn.closeIfEnded()
}

func (conn *fdConn) takeDeferredCloseCause() error {
	conn.submitMu.Lock()
	defer conn.submitMu.Unlock()
	if conn.close.deferred == nil {
		return nil
	}
	err := conn.close.deferred.cause
	conn.close.deferred = nil
	return err
}

func (conn *fdConn) setDeferredCloseLocked(err error) {
	if conn.close.deferred == nil || conn.close.deferred.cause == nil && err != nil {
		conn.close.deferred = newCloseRequest(err)
	}
}

// closeInTurn releases a closing connection from the end of its own turn,
// which owns it, and delivers OnClose in the same turn. While the loop shuts
// down its pass releases every connection instead, with the shutdown cause.
func (conn *fdConn) closeInTurn() {
	if !conn.isClosing() || conn.close.isReleased() || conn.loop.stopping.Load() {
		return
	}
	// This turn handles any pending request to release the connection.
	conn.taskState.And(^ioEventTeardown)
	if finalErr, ok := conn.teardown(nil); ok {
		conn.deliverClose(finalErr)
	}
}

func (conn *fdConn) closeUnregistered() {
	if conn.close.abandon() {
		switch {
		case conn.internal:
			conn.releaseSocket()
		case conn.udp != nil && conn.udp.file != nil:
			_ = conn.udp.file.Close()
		default:
			_ = syscall.Close(int(conn.fd))
		}
	}
}

// closesOffTurn reports a descriptor the loop's closer may close: accepted,
// with no linger time set and no Control run on it.
func (conn *fdConn) closesOffTurn() bool {
	return conn.flags.Load()&(controlAliasFlag|offTurnCloseFlag) == offTurnCloseFlag
}

func (conn *fdConn) Close() error { return conn.CloseWith(io.ErrUnexpectedEOF) }

// CloseGracefully requests a close that lets what was accepted leave first,
// and returns. Writes are refused from the call; the connection's turn
// releases it once its queue is empty — or once LingerTimeout passes, a
// CloseWith at any time, or the Events closing ends the wait — and leftover
// output reaches OnClose as UnflushedError appended to cause. Reads go on
// until the release, so the peer's acknowledgements are seen. A datagram
// connection has no queue to wait for: it closes as CloseWith does.
func (conn *fdConn) CloseGracefully(cause error) error {
	if conn.loop == nil {
		return net.ErrClosed
	}
	if conn.isDatagram() {
		return conn.CloseWith(cause)
	}
	conn.submitMu.Lock()
	if conn.isClosing() || conn.events().closing.Load() || conn.loop.stopping.Load() || !conn.close.drain() {
		conn.submitMu.Unlock()
		return net.ErrClosed
	}
	conn.setDeferredCloseLocked(cause)
	pending := conn.pending.Load()
	conn.submitMu.Unlock()
	if pending == 0 {
		conn.escalateDrain()
		return nil
	}
	// Bound the wait, on the lazily-allocated timer state so a connection
	// that never closes gracefully carries nothing for it. The bound is
	// captured once: progress resets it by the same span without reading the
	// package variable again. A queue that drains in the instant between the
	// check and the arm leaves a stray timer whose callback finds the phase
	// moved on and does nothing.
	if conn.close.isDraining() {
		conn.wakeState().drain.Store(&drainBound{
			timer: time.AfterFunc(lingerTimeout, conn.escalateDrain),
			wait:  lingerTimeout,
		})
	}
	// Output that already waits may have no sender of its own.
	conn.kickWriterOutside()
	return nil
}

// escalateDrain ends a drain — its queue left, or its bound arrived — so the
// close proceeds as CloseWith's does. Any goroutine may call it.
func (conn *fdConn) escalateDrain() {
	if !conn.close.escalate() {
		return
	}
	conn.stopDrainTimer()
	conn.routeClose()
}

// stopDrainTimer drops a drain's bound, if it has not fired already. It runs
// wherever a connection ends.
func (conn *fdConn) stopDrainTimer() {
	if w := conn.wake.Load(); w != nil {
		if bound := w.drain.Swap(nil); bound != nil {
			bound.timer.Stop()
		}
	}
}

// CloseWith requests the close and returns: the connection is released, and
// OnClose delivered, by its own turn once the callback that closed it has
// returned. Requesting it under submitMu puts Close after external writes
// that have already reached their submission point.
func (conn *fdConn) CloseWith(err error) error {
	if conn.loop == nil {
		return net.ErrClosed
	}
	conn.submitMu.Lock()
	if conn.isClosing() || conn.descriptorOwner().isClosing() || conn.events().closing.Load() || conn.loop.stopping.Load() || !conn.close.request() {
		conn.submitMu.Unlock()
		return net.ErrClosed
	}
	conn.setDeferredCloseLocked(err)
	conn.submitMu.Unlock()
	conn.routeClose()
	return nil
}

// requestClose is the internal idempotent close path. The first cause is the
// one OnClose reports; while the loop shuts down a later one is kept too, for
// its pass to report alongside the shutdown cause.
func (conn *fdConn) requestClose(err error) {
	if conn.beginClose(err) {
		conn.routeClose()
	}
}

// beginClose marks the connection closing with err as its cause and reports
// whether this call started the close.
func (conn *fdConn) beginClose(err error) bool {
	conn.submitMu.Lock()
	defer conn.submitMu.Unlock()
	if conn.close.request() {
		conn.setDeferredCloseLocked(err)
		return true
	}
	if conn.loop != nil && conn.loop.stopping.Load() && !conn.close.isReleased() {
		conn.setDeferredCloseLocked(err)
	}
	return false
}

// routeClose hands a requested close to the turn that releases the
// connection. The running turn itself releases it as it ends; any other
// caller schedules a turn for it. A UDP child is released by its server's
// turn.
func (conn *fdConn) routeClose() {
	if conn.udp != nil && conn.udp.server != nil {
		conn.udp.server.queuePeer(conn, false)
		return
	}
	if conn.ioOwner.Load() == currentGoroutineID() {
		return
	}
	conn.scheduleTeardown()
}

// scheduleTeardown submits a turn to release a closing connection. A running
// turn takes the request when it ends; a stopped loop's shutdown pass
// releases the connection instead.
func (conn *fdConn) scheduleTeardown() {
	if conn.noteIO(ioEventTeardown) && !conn.loop.ioPool.submit(conn) {
		conn.handleIOSubmitFailure(net.ErrClosed)
	}
}

// teardown releases transport resources exactly once and reports the final
// cause. Only the connection's owner calls it: its turn, the handler of a
// refused turn, its UDP server's turn, or the loop's shutdown pass once every
// turn has stopped. A sender holding the write claim may be writing the
// socket, so a stream also needs the claim before its descriptor closes:
// otherwise the cause is recorded and the holder hands the release back to the
// connection's turn when it gives the claim up. The claim is then kept for
// good. The connection is marked released under submitMu, so a socket option
// set from outside the turn either completes before the descriptor closes or
// finds it released.
func (conn *fdConn) teardown(cause error) (error, bool) {
	if conn.close.isReleased() {
		return nil, false
	}
	if !conn.isDatagram() && !conn.claimWriteForClose(&cause) {
		return nil, false
	}
	conn.submitMu.Lock()
	released := conn.close.release()
	conn.submitMu.Unlock()
	if !released {
		return nil, false
	}
	// Close gets one bounded flush attempt; a slow peer cannot delay shutdown.
	var flushErr error
	if !conn.isDatagram() && !conn.writeFailed() {
		conn.setWriteBlocked(false)
		flushed := false
		defer func() {
			if !flushed {
				// OnBytesWritten panicked in the close flush. The connection is
				// released already, so nothing else would close it: finish
				// here, and leave OnClose to its next turn.
				conn.scheduleCloseCallback(conn.releaseResources(cause, nil))
			}
		}()
		_, flushErr = conn.flushWrite()
		flushed = true
	}
	return conn.releaseResources(cause, flushErr), true
}

// joinCloseErrors is errors.Join for the three causes a close collects. Most
// closes have one, which is returned as it is: errors.Join would allocate a
// wrapper and its slice for it on every connection that closes.
func joinCloseErrors(cause, deferred, flush error) error {
	switch {
	case deferred == nil && flush == nil:
		return cause
	case cause == nil && flush == nil:
		return deferred
	case cause == nil && deferred == nil:
		return flush
	}
	return errors.Join(cause, deferred, flush)
}

// releaseResources is teardown after the close flush: it closes the
// descriptor, drops the buffers, and returns the final cause.
func (conn *fdConn) releaseResources(cause, flushErr error) error {
	remaining := conn.pending.Load()
	deferredCloseErr := conn.takeDeferredCloseCause()
	finalErr := joinCloseErrors(cause, deferredCloseErr, flushErr)
	if remaining > 0 {
		finalErr = errors.Join(finalErr, UnflushedError{Remaining: remaining})
	}
	conn.stopDeadlines()
	conn.cancelWakeTimer()
	if conn.udp != nil && conn.udp.server != nil {
		// A child only leaves the peer map; its server owns the shared fd.
		// DialPeer may have replaced it there already.
		server := conn.udp.server.udp
		server.peerMu.Lock()
		if server.peers[conn.udp.key] == conn {
			delete(server.peers, conn.udp.key)
		}
		server.peerMu.Unlock()
	} else {
		if conn.udp != nil && conn.udp.peers != nil {
			conn.udp.peerMu.Lock()
			children := conn.udp.peers
			closing, opening, waking := conn.udp.closing, conn.udp.opening, conn.udp.waking
			conn.udp.peers = nil
			conn.udp.closing = nil
			conn.udp.opening = nil
			conn.udp.waking = nil
			conn.udp.peerMu.Unlock()
			for _, child := range children {
				if childErr, ok := child.teardown(finalErr); ok {
					child.scheduleCloseCallback(childErr)
				}
			}
			// A replaced child may only remain in a hand-off queue. Teardown
			// claims each child once, so overlapping entries need no set or
			// extra allocation in this terminal path.
			for _, queued := range [][]*fdConn{closing, opening, waking} {
				for _, child := range queued {
					if childErr, ok := child.teardown(finalErr); ok {
						child.scheduleCloseCallback(childErr)
					}
				}
			}
		}
		if conn.internal {
			conn.releaseSocket()
			conn.loop.forget(conn)
		} else {
			conn.loop.forget(conn)
			if conn.udp != nil && conn.udp.file != nil {
				_ = conn.udp.file.Close()
			} else if !conn.closesOffTurn() || !conn.loop.deferClose(int(conn.fd)) {
				_ = syscall.Close(int(conn.fd))
			}
		}
	}
	conn.submitMu.Lock()
	conn.outbound.Reset()
	conn.submitMu.Unlock()
	conn.stopDrainTimer()
	conn.releaseInflight()
	conn.inbound.Reset()
	conn.inboundTail = nil
	conn.pending.Store(0)
	return finalErr
}

// closeCallbackDue reports whether the connection gets OnClose: every user
// connection whose open turn has started does. One whose open turn is still
// owed, refused by the Executor, never got OnOpen, so it gets no OnClose
// either.
func (conn *fdConn) closeCallbackDue() bool {
	return !conn.internal && conn.callbacks().onClose != nil && conn.taskState.Load()&ioEventOpen == 0
}

// deliverClose runs OnClose for a released connection on the owner that
// released it, which runs no other callback of the connection meanwhile.
func (conn *fdConn) deliverClose(err error) {
	if !conn.closeCallbackDue() {
		conn.close.phase.CompareAndSwap(closeResourcesReleased, closeCallbackDelivered)
		return
	}
	conn.submitMu.Lock()
	conn.setDeferredCloseLocked(err)
	conn.submitMu.Unlock()
	conn.fireCloseCallback()
}

// claimWriteForClose takes the write claim for good before teardown, since a
// holder may be in the middle of a send. While it is taken, the cause is
// recorded, the holder is asked through writeCloseWaitFlag to hand the release
// back, and the claim is tried once more: a holder that released in between
// either saw the request or left the claim free. Recording the cause moves it
// into the deferred slot, so *cause is cleared when the retry wins. During
// shutdown every write turn has already been joined, and a holder can only be
// a producer that is giving the claim straight back, so the pass waits for it.
func (conn *fdConn) claimWriteForClose(cause *error) bool {
	if conn.tryClaimWrite(0) {
		return true
	}
	if conn.loop.ioStopped() {
		for !conn.tryClaimWrite(0) {
			runtime.Gosched()
		}
		return true
	}
	// A closing connection starts no further write turns, so the handoff ends.
	conn.close.request()
	conn.submitMu.Lock()
	conn.setDeferredCloseLocked(*cause)
	conn.submitMu.Unlock()
	conn.writeState.Or(writeCloseWaitFlag)
	if !conn.tryClaimWrite(0) {
		return false
	}
	conn.writeState.And(^writeCloseWaitFlag)
	*cause = nil
	return true
}

// scheduleCloseCallback publishes OnClose as the terminal synthetic event, for
// a connection released outside its turn. Connections without a user callback
// advance the phase immediately.
func (conn *fdConn) scheduleCloseCallback(err error) {
	if !conn.closeCallbackDue() {
		conn.close.phase.CompareAndSwap(closeResourcesReleased, closeCallbackDelivered)
		return
	}
	conn.submitMu.Lock()
	conn.setDeferredCloseLocked(err)
	conn.submitMu.Unlock()
	conn.taskState.Or(ioEventClose)
	if conn.taskState.Or(taskScheduledBit)&taskScheduledBit != 0 {
		return
	}
	if conn.loop != nil {
		conn.loop.acquireCloseIO()
	}
	if conn.loop == nil || conn.loop.ioPool == nil || !conn.loop.ioPool.submit(conn) {
		conn.taskState.And(^taskScheduledBit)
		conn.fireCloseCallback()
		if conn.loop != nil {
			conn.loop.releaseIO()
		}
	}
}

func (conn *fdConn) fireCloseCallback() {
	if !conn.close.phase.CompareAndSwap(closeResourcesReleased, closeCallbackDelivered) {
		return
	}
	err := conn.takeDeferredCloseCause()
	if conn.udp != nil && conn.udp.openOwed.CompareAndSwap(true, false) {
		// A DialPeer child closed before its OnOpen ran gets it first.
		if callback := conn.callbacks().onOpen; callback != nil {
			started := conn.beginInboundCallback()
			callback(conn)
			conn.endInboundCallback(started)
		}
	}
	if callback := conn.callbacks().onClose; callback != nil && !conn.internal {
		started := conn.beginInboundCallback()
		callback(conn, err)
		conn.endInboundCallback(started)
	}
}
