//go:build windows || stdio

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

// The portable backend's close walk: the drain before a graceful close, the
// loop-side teardown, and the descriptor and cancellation edges.

package uio

import (
	"io"
	"net"
	"sync/atomic"
	"time"

	"github.com/urpc/uio/internal/blockio"
)

func (fc *fdConn) closeUnregistered() {
	fc.closing.Store(true)
	atomic.StoreInt32(&fc.closed, 1)
	if fc.conn != nil {
		_ = fc.conn.Close()
	}
	if fc.udp != nil {
		_ = fc.udp.sock.Close()
	}
}

// closeListenerSocket closes a UDP listener's server connection on the
// Listener.Close caller, so its address is free when Close returns. A Control
// on the socket holds a descriptor reference the socket close waits for: one
// by the caller itself would wait forever, and one that outlasts
// socketCloseWait may be waiting for this very Close, so both fall back to
// the counted close task, which closes the socket once the Control returns.
func (fc *fdConn) closeListenerSocket(err error) {
	events := fc.events()
	if fc.udp.inControl() || !fc.udp.controlsQuiet() {
		fc.requestClose(err)
		return
	}
	// Count the close as shutdown counts a close task: it may still deliver
	// the peers' OnClose, which Serve's return must not precede.
	events.mux.Lock()
	if events.closing.Load() {
		events.mux.Unlock()
		fc.requestClose(err)
		return
	}
	events.callbackWG.Add(1)
	events.mux.Unlock()
	defer events.callbackWG.Done()
	fc.closeOnLoop(err)
}

// Close refuses new writes and lets the writer drain what was accepted,
// bounded by LingerTimeout without progress, before releasing the connection.
func (fc *fdConn) Close() error {
	fc.localClosed.Store(true)
	return fc.closeGracefully(nil)
}

func (fc *fdConn) closeGracefully(cause error) error {
	if fc.udp != nil || fc.loop == nil {
		return fc.enqueueClose(cause)
	}
	fc.mux.Lock()
	if fc.isClosing() || !fc.draining.CompareAndSwap(false, true) {
		fc.mux.Unlock()
		return net.ErrClosed
	}
	fc.drainCause = cause
	fc.drainWait = lingerTimeout
	queued := fc.outbound.Len() + fc.writeBytes
	if queued > 0 {
		fc.drainTimer = time.AfterFunc(fc.drainWait, fc.finishDrain)
	}
	fc.mux.Unlock()
	if queued > 0 {
		select {
		case fc.writeSig <- struct{}{}:
		default:
		}
		return nil
	}
	fc.finishDrain()
	return nil
}

// finishDrain ends a graceful close's wait — its queue drained, or the bound
// arrived — closing as it asked. Any goroutine may call it.
func (fc *fdConn) finishDrain() {
	fc.mux.Lock()
	if !fc.draining.Load() || fc.drainEnding {
		fc.mux.Unlock()
		return
	}
	// Claim the end; draining stays set. Clearing it here would reopen
	// admission for the instant before closing is published — writers and
	// the close publish both take mux, so publishing closing first keeps
	// that instant from existing.
	fc.drainEnding = true
	cause := fc.drainCause
	fc.drainCause = nil
	if fc.drainTimer != nil {
		fc.drainTimer.Stop()
		fc.drainTimer = nil
	}
	fc.mux.Unlock()
	if fc.isClosing() {
		return // an abort, or the Events closing, ended it already
	}
	_ = fc.enqueueClose(cause)
}

// Abort immediately closes the connection with cause, cutting any graceful
// Close drain short at once.
func (fc *fdConn) Abort(cause error) error {
	if cause == nil {
		cause = net.ErrClosed
	}
	fc.localClosed.Store(true)
	return fc.enqueueClose(cause)
}

func (fc *fdConn) enqueueClose(err error) error {
	fc.mux.Lock()
	if fc.isClosing() || (fc.udp != nil && fc.udp.svr != nil && fc.udp.svr.isClosing()) {
		fc.mux.Unlock()
		return net.ErrClosed
	}
	if fc.drainTimer != nil {
		fc.drainTimer.Stop()
		fc.drainTimer = nil
	}
	// Publish the first request's cause together with closing. A listener's
	// release may overtake the task, but must retain that requested cause.
	fc.err = err
	// The request joins the lifecycle before closing is published: until
	// then the connection's own count holds Serve's join, and once it is
	// published a shutdown leaves the close to this request, which must
	// still be counted when it hands the close on.
	fc.events().callbackWG.Add(1)
	fc.closing.Store(true)
	// The drain, if one ran, is over: closing is published under the
	// admission lock, so clearing draining here cannot reopen the window
	// finishDrain guards.
	fc.draining.Store(false)
	if fc.udp != nil && fc.internal {
		fc.udp.closingPeers = fc.udp.peers
		fc.udp.peers = nil
	}
	fc.mux.Unlock()
	if hook := testHookStdCloseRequested.Load(); hook != nil {
		(*hook)(fc)
	}
	// Prefer the loop's ordered close task for resource release and OnClose.
	t := acquireTask(closeTask, fc)
	t.err = err
	if fc.loop != nil && fc.loop.submitTask(t) {
		fc.events().callbackWG.Done() // the loop, which Serve joins, has it
		return nil
	}
	releaseTask(t)
	// A sealed queue transfers the close, with the request's count, to a
	// goroutine. Keep cleanup off the caller: it may hold a RawConn.Control
	// descriptor reference, or another resource that closing the socket must
	// wait for it to release.
	go func() {
		defer fc.events().callbackWG.Done()
		fc.closeOnLoop(err)
	}()
	return nil
}

func (fc *fdConn) requestClose(err error) { _ = fc.enqueueClose(err) }

func (fc *fdConn) closeOnLoop(err error) {
	fc.stopWriteDeadline()
	fc.mux.Lock()
	if atomic.LoadInt32(&fc.closed) == 1 {
		fc.mux.Unlock()
		return
	}
	if fc.closing.Load() {
		err = fc.err
	} else {
		fc.err = err
		fc.closing.Store(true)
	}
	if fc.udp != nil && fc.udp.svr != nil {
		fc.stopChildDeadlines()
		// A child only leaves the peer map, under its server's lock; DialPeer
		// may have replaced it there already. The child's own lock always
		// comes before its server's.
		fc.udp.svr.mux.Lock()
		if peers := fc.udp.svr.udp.peers; peers != nil && peers[peerKey(fc.remoteAddr)] == fc {
			delete(peers, peerKey(fc.remoteAddr))
		}
		fc.udp.svr.mux.Unlock()
		atomic.StoreInt32(&fc.closed, 1)
		fc.mux.Unlock()
		fc.finishClose(err)
		return
	}
	var children []*fdConn
	if fc.udp != nil && (fc.udp.peers != nil || fc.udp.closingPeers != nil) {
		peers := fc.udp.peers
		if peers == nil {
			peers = fc.udp.closingPeers
		}
		children = make([]*fdConn, 0, len(peers))
		for _, child := range peers {
			children = append(children, child)
		}
		fc.udp.peers = nil
		fc.udp.closingPeers = nil
	}
	atomic.StoreInt32(&fc.closed, 1)
	fc.mux.Unlock()
	fc.closeDescriptor()
	for _, child := range children {
		child.closeOnLoop(err)
	}
	fc.finishClose(err)
}

// finishClose is the resource owner's terminal hand-off. Opening may only
// arrange OnClose once this cleanup has finished, never just from closed.
func (fc *fdConn) finishClose(err error) {
	// Every close path ends here, a datagram child's included: an armed wake
	// timer, a periodic one above all, would keep re-arming itself against a
	// closed connection forever.
	fc.clearWakeTimer()
	if hook := testHookStdCloseReleased.Load(); hook != nil {
		(*hook)(fc)
	}
	fc.notifyBlockClose(err)
	fc.scheduleCloseCallback(err)
}

// closeDescriptor releases a claimed connection's descriptor and its queued
// output. It runs once per connection, after closeOnLoop's exclusive claim,
// with no lock held: the closed flag keeps every other closer out.
func (fc *fdConn) closeDescriptor() {
	// Wake the writer so it observes the close and stops. A notification that
	// is already pending is just as good a wake as this one, so a full channel
	// needs no second signal.
	if fc.writeSig != nil {
		select {
		case fc.writeSig <- struct{}{}:
		default:
		}
	}
	// A reader held by MaxInboundBuffered waits for a resume; the close is one.
	if fc.readResume != nil {
		select {
		case fc.readResume <- struct{}{}:
		default:
		}
	}

	// delete connection fd-mapping.
	if fc.loop != nil {
		fc.loop.delConn(fc)
	}

	// Close the socket before waiting on mux so blocked writes are interrupted.
	switch {
	case nil != fc.conn:
		_ = fc.conn.Close()
	case nil != fc.udp:
		_ = fc.udp.sock.Close()
	}

	// Writers may still reference pooled outbound blocks until they return.
	fc.writeMu.Lock()
	fc.mux.Lock()
	fc.outbound.Reset()
	fc.writeBatch.Reset()
	fc.writeBytes = 0
	if fc.drainTimer != nil {
		fc.drainTimer.Stop()
		fc.drainTimer = nil
	}
	fc.mux.Unlock()
	fc.writeMu.Unlock()
}

func (fc *fdConn) scheduleCloseCallback(err error) {
	fc.closeReady.Store(true)
	if !fc.closeScheduled.CompareAndSwap(false, true) {
		return
	}
	// The connection's lifecycle reservation, or its still-counted owner
	// for an unregistered/internal connection, precedes this Add. Shutdown
	// cannot finish between resource release and this hand-off.
	fc.events().callbackWG.Add(1)
	go func() {
		defer fc.events().callbackWG.Done()
		fc.deliverCloseCallback(err)
	}()
}

// deliverCloseCallback runs one connection's OnClose under its callback
// mutex, exactly once per connection.
func (fc *fdConn) deliverCloseCallback(err error) {
	blockio.EnterTurn()
	defer blockio.LeaveTurn()
	fc.callbackMu.Lock()
	defer fc.callbackMu.Unlock()
	if !fc.internal && !fc.opened {
		// Its counted opener still owes OnOpen. Give that opener the close
		// delivery back, keeping the connection's lifecycle reservation.
		fc.closeScheduled.Store(false)
		return
	}
	if !atomic.CompareAndSwapInt32(&fc.closeDelivered, 0, 1) {
		return
	}
	if fc.lifecycle {
		fc.lifecycle = false
		defer fc.events().callbackWG.Done()
	}
	if callback := fc.callbacks().onClose; callback != nil && !fc.internal {
		fc.inbound.Reset()
		fc.inboundTail = nil
		started := fc.beginInboundCallback()
		callback(fc, err)
		fc.endInboundCallback(started)
	}
}

// halfCloser is a net.Conn that can shut its writing half, as *net.TCPConn
// and *net.UnixConn can.
type halfCloser interface{ CloseWrite() error }

// CloseWrite shuts the writing half once the writer has sent everything
// accepted before it; see Conn.CloseWrite.
func (fc *fdConn) CloseWrite() error {
	if fc.udp != nil {
		return ErrUnsupported
	}
	if _, ok := fc.conn.(halfCloser); !ok {
		return ErrUnsupported
	}
	fc.mux.Lock()
	if fc.isClosing() {
		fc.mux.Unlock()
		return net.ErrClosed
	}
	if fc.writeShut {
		fc.mux.Unlock()
		return nil
	}
	fc.writeShut = true
	fc.mux.Unlock()
	select {
	case fc.writeSig <- struct{}{}:
	default:
	}
	return nil
}

// shutWriteIfDrained runs on the writer after a drain and shuts the writing
// half a CloseWrite requested once nothing accepted is left. Output is no
// longer accepted then, so what is left only drains.
func (fc *fdConn) shutWriteIfDrained() error {
	if fc.writeShutDone.Load() {
		return nil
	}
	fc.mux.Lock()
	ready := fc.writeShut && fc.outbound.Len() == 0 && fc.writeBytes == 0
	fc.mux.Unlock()
	if !ready {
		return nil
	}
	if fc.entry.directConsumer && fc.readEnded.Load() {
		// Both halves end: closing sends the FIN behind what was sent.
		fc.writeShutDone.Store(true)
		fc.requestClose(io.EOF)
		return nil
	}
	err := fc.conn.(halfCloser).CloseWrite()
	fc.writeShutDone.Store(true)
	if err != nil {
		return err
	}
	if fc.entry.directConsumer && fc.readEnded.Load() {
		fc.requestClose(io.EOF)
	}
	return nil
}
