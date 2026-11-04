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

package uio

import (
	"io"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/urpc/uio/internal/bytebuf"
	"github.com/urpc/uio/internal/poller"
	"github.com/urpc/uio/internal/readcontrol"
)

// Keep tiny writes coalesced while bounding payload work done under mux.
const stdOwnedWriteThreshold = 4 << 10

const stdWriteVecLimit = 16

type fdConn struct {
	inputReadControlAccess
	readControl atomic.Pointer[readcontrol.Control] // optional consumer pauses, independent of readHeld
	// yieldPending marks a YieldRead whose continuation callback has not run
	// yet: the end of the stream waits for it.
	yieldPending atomic.Bool
	commonConn
	conn           net.Conn
	udp            *stdUDPState
	writeSig       chan struct{} // coalesced "output exists" wake; see writeLoop
	closed         int32
	closeDelivered int32
	err            error
	mux            sync.Mutex
	outbound       bytebuf.CompositeBuffer
	writeMu        sync.Mutex // keeps close cleanup from racing the detached write batch
	writeBatch     bytebuf.CompositeBuffer
	writeBytes     int
	closing        atomic.Bool
	callbackMu     sync.Mutex // serializes OnOpen, OnRead, Wake, and OnClose
	internal       bool       // framework-owned endpoint, not a user connection

	// A graceful close refuses writes and waits, bounded, for the writer to
	// drain what was accepted — this backend's queue — before the close it
	// asked for. draining is set once and stays set until enqueueClose
	// publishes closing under mux: admission checks read both under the same
	// lock, so no write can slip through between the drain ending and the
	// close beginning. drainEnding marks the end already claimed; drainCause,
	// drainWait, drainTimer and drainEnding are guarded by mux.
	draining    atomic.Bool
	drainEnding bool
	drainCause  error
	drainWait   time.Duration
	drainTimer  *time.Timer

	// opened records that OnOpen has run, which a connection's read goroutine
	// does first, closing or not; only an opened connection gets OnClose. wakesAfterOpen
	// counts the Wakes that arrived before OnOpen, for the read goroutine to
	// deliver right after it, one OnRead each. Both are guarded by callbackMu.
	opened         bool
	wakesAfterOpen int

	// readHeld stops the read goroutine once callbacks leave
	// MaxInboundBuffered bytes unconsumed; it waits on readResume until a Wake
	// consumes below the limit, the read deadline passes, or the connection
	// closes. readHeld is guarded by callbackMu; readResume, buffered by one so
	// no signal waits for the reader, is made with the stream connection. The
	// held wait reads no socket, so it keeps the read deadline the socket
	// enforces on a Read: deadlineMu orders each setter's two updates, so
	// readDeadline and the socket always hold the same value.
	readHeld     bool
	readResume   chan struct{}
	deadlineMu   sync.Mutex
	readDeadline time.Time
	// A write deadline is a connection liveness bound: one that passes
	// closes the connection even with no write in flight — the Go poller's
	// socket deadline alone would never fire on an idle connection, while the
	// native backend closes. writeGeneration makes a replaced timer harmless.
	// Guarded by deadlineMu.
	writeDeadline   time.Time
	writeGeneration uint64
	writeTimer      *time.Timer

	// wake is the connection's wake timer on the stdio backend, where no
	// event loop carries a wheel: one runtime timer per armed connection,
	// replaced by each setter. wakeMu orders the setters against one another
	// and the close path's stop; the generation makes a timer that fires for
	// a replaced or cleared deadline harmless.
	wakeMu    sync.Mutex
	wakeGen   uint64
	wakeAt    time.Time
	wakeEvery time.Duration
	wakeTimer *time.Timer
	// wakeArmed/wakeDue/wakePeriod mirror the armed wake timer for the
	// turn-time OnTimer routing, all under wakeMu as one record: expireWake
	// consumes wakeAt before the turn runs, so the due check needs a state
	// of its own, published and consumed whole. See fireWakeTimer.
	wakeArmed  bool
	wakeDue    int64
	wakePeriod int64

	// writeShut records a CloseWrite, guarded by mux: no more output is
	// accepted, and the writer shuts the socket's writing half once what was
	// accepted before it is sent, then sets writeShutDone. readEOF records,
	// on a connection that allows half-open, that the peer shut its own, and
	// readEnded, set under callbackMu, that callbacks consumed everything it
	// sent; the side that sets the second of writeShutDone and readEnded
	// closes the connection.
	writeShut     bool
	writeShutDone atomic.Bool
	readEOF       atomic.Bool
	readEnded     atomic.Bool

	// A published user connection holds one lifecycle reservation until its
	// terminal callback completes, including while a close has left the peer
	// map but has not yet scheduled that callback. lifecycle is initialized
	// before publication and cleared under callbackMu.
	lifecycle      bool
	closeReady     atomic.Bool // resource cleanup finished; OnClose may run
	closeScheduled atomic.Bool // one close delivery, including duplicates from opening
}

func (fc *fdConn) IsClosed() bool { return atomic.LoadInt32(&fc.closed) != 0 }

func (fc *fdConn) OutboundBuffered() int {
	fc.mux.Lock()
	defer fc.mux.Unlock()
	return fc.outbound.Len() + fc.writeBytes
}

func (fc *fdConn) initialInterest() poller.Interest { return poller.Readable }

func (fc *fdConn) setInterest(poller.Interest) {}

func (fc *fdConn) currentInterest() poller.Interest { return poller.Readable }

func (fc *fdConn) isClosing() bool { return fc.closing.Load() || fc.IsClosed() }

func (fc *fdConn) beginShutdown() {}

func (fc *fdConn) isDatagram() bool { return fc.udp != nil }

func (fc *fdConn) isClosedOnLoop() bool { return fc.IsClosed() }

// afterRegister starts the connection's read goroutine, which runs OnOpen and
// then starts the writer; see openOnReader. OnOpen stays off the event loop,
// so it may Dial and a slow one holds up no other connection.
func (fc *fdConn) afterRegister() {
	fc.retainLifecycle()
	_ = fc.fireReadEvent()
}

// retainLifecycle runs before a connection is published, or on its registering
// loop before the loop can stop. Readers and openers have their own counts:
// this count covers the connection after it leaves those owners' tables.
func (fc *fdConn) retainLifecycle() {
	fc.events().callbackWG.Add(1)
	fc.lifecycle = true
}

// openOnReader runs OnOpen on the connection's read goroutine before its
// first read, then starts the writer, so output written in OnOpen leaves after
// it returns, as it did when OnOpen ran on the loop. Wakes that came first are
// delivered right after, one OnRead each. It reports whether the connection is
// still open. A connection closed before this goroutine ran still gets OnOpen
// and then OnClose, as on the native backend: a Dial that returned it has
// handed its caller a connection whose end only OnClose reports.
func (fc *fdConn) openOnReader() bool {
	fc.callbackMu.Lock()
	fc.ensureOpenLocked()
	for ; fc.wakesAfterOpen > 0 && !fc.isClosing(); fc.wakesAfterOpen-- {
		started := fc.beginInboundCallback()
		err := fc.events().onRead(fc)
		fc.endInboundCallback(started)
		if err != nil {
			fc.requestClose(err)
		}
	}
	closing := fc.isClosing()
	fc.callbackMu.Unlock()
	if closing {
		return false
	}
	_ = fc.fireWriteEvent()
	return true
}

// ensureOpenLocked claims the connection's first open and runs OnOpen; it
// requires callbackMu. Whichever of a connection's openers reaches it first —
// its reader, the goroutine of a DialPeer, a close catching it unopened —
// runs OnOpen, exactly once, before any OnRead or OnClose of it.
func (fc *fdConn) ensureOpenLocked() {
	if fc.opened {
		return
	}
	fc.fireOnOpen()
	fc.opened = true
	if fc.closeReady.Load() {
		// Torn down before OnOpen ran, so the close callback may have skipped
		// it as unopened; deliver it now. closeDelivered keeps it to once.
		fc.scheduleCloseCallback(fc.err)
	}
}

func (fc *fdConn) fireOnOpen() {
	if callback := fc.callbacks().onOpen; callback != nil {
		started := fc.beginInboundCallback()
		callback(fc)
		fc.endInboundCallback(started)
	}
}

// Control runs fn with the connection's descriptor, for the socket options
// the dedicated setters do not carry; see Conn.Control. A UDP child's options
// land on the socket it shares with its server.
func (fc *fdConn) Control(fn func(fd uintptr)) error {
	// A closed datagram peer shares a socket that stays open, but its own
	// close refuses options all the same.
	if fc.isClosing() {
		return net.ErrClosed
	}
	var conn syscall.Conn
	if fc.udp != nil {
		if fc.udp.sock == nil {
			return net.ErrClosed
		}
		conn = fc.udp.sock
	} else {
		if fc.conn == nil {
			return net.ErrClosed
		}
		holder, ok := fc.conn.(syscall.Conn)
		if !ok {
			return ErrUnsupported
		}
		conn = holder
	}
	raw, err := conn.SyscallConn()
	if err != nil {
		return err
	}
	if fc.udp != nil {
		server := fc
		if fc.udp.svr != nil {
			server = fc.udp.svr
		}
		id, ok := server.udp.enterControl()
		if !ok {
			return net.ErrClosed // the socket is closing
		}
		defer server.udp.leaveControl(id)
	}
	return raw.Control(func(fd uintptr) { fn(fd) })
}

// SetWakeDeadline arms the connection's wake timer for when; see the Conn
// interface for the contract. A zero time cancels.
func (fc *fdConn) SetWakeDeadline(when time.Time) error {
	if fc.isClosing() {
		return net.ErrClosed
	}
	if when.IsZero() {
		fc.clearWakeTimer()
		return nil
	}
	fc.armWake(when, 0)
	if fc.isClosing() {
		fc.clearWakeTimer()
	}
	return nil
}

// SetWakeInterval arms the connection's wake timer to fire every interval;
// see the Conn interface for the contract. A zero or negative interval
// cancels.
func (fc *fdConn) SetWakeInterval(interval time.Duration) error {
	if fc.isClosing() {
		return net.ErrClosed
	}
	if interval <= 0 {
		fc.clearWakeTimer()
		return nil
	}
	fc.armWake(time.Now().Add(interval), interval)
	if fc.isClosing() {
		fc.clearWakeTimer()
	}
	return nil
}

// armWake runs one runtime timer for the connection's wake deadline. The
// generation makes a timer whose deadline was replaced or cleared while it
// fired harmless: the callback re-checks both under the mutex.
func (fc *fdConn) armWake(when time.Time, every time.Duration) {
	fc.wakeMu.Lock()
	defer fc.wakeMu.Unlock()
	fc.wakeGen++
	fc.wakeAt, fc.wakeEvery = when, every
	fc.wakeArmed, fc.wakeDue, fc.wakePeriod = true, when.UnixNano(), int64(every)
	generation := fc.wakeGen
	// A fresh timer each arm, never Reset: the callback carries this arm's
	// generation, which Reset would leave stale in the old closure. A timer
	// stopped while its callback was already dequeued fires once more; the
	// generation check makes that harmless.
	if fc.wakeTimer != nil {
		fc.wakeTimer.Stop()
	}
	delay := time.Until(when)
	if delay < 0 {
		delay = 0
	}
	fc.wakeTimer = time.AfterFunc(delay, func() { fc.expireWake(generation) })
}

// clearWakeTimer disarms the timer.
// fireWakeTimer runs the connection's own timer callback when its wake
// deadline passed; see the Conn interface's OnTimer and the native
// fireWakeTimer for skipRead's meaning.
func (fc *fdConn) fireWakeTimer() (skipRead bool, err error) {
	if fc.callbacks().onTimer == nil {
		return false, nil
	}
	fc.wakeMu.Lock()
	if !fc.wakeArmed {
		fc.wakeMu.Unlock()
		return false, nil
	}
	now := time.Now().UnixNano()
	if now < fc.wakeDue {
		fc.wakeMu.Unlock()
		return false, nil
	}
	if fc.wakePeriod > 0 {
		fc.wakeDue += ((now-fc.wakeDue)/fc.wakePeriod + 1) * fc.wakePeriod
	} else {
		fc.wakeArmed = false
	}
	fc.wakeMu.Unlock()
	if fc.isClosing() {
		return false, nil
	}
	if err := fc.callbacks().onTimer(fc); err != nil {
		return true, err
	}
	return fc.isClosing(), nil
}

func (fc *fdConn) clearWakeTimer() {
	fc.wakeMu.Lock()
	fc.wakeArmed = false
	defer fc.wakeMu.Unlock()
	fc.wakeGen++
	fc.wakeAt, fc.wakeEvery = time.Time{}, 0
	if fc.wakeTimer != nil {
		fc.wakeTimer.Stop()
		fc.wakeTimer = nil
	}
}

// expireWake is the timer's callback: still-armed, it Wakes the connection —
// OnRead with no socket read — and re-arms an interval.
func (fc *fdConn) expireWake(generation uint64) {
	fc.wakeMu.Lock()
	if generation != fc.wakeGen || fc.wakeAt.IsZero() {
		fc.wakeMu.Unlock()
		return
	}
	every := fc.wakeEvery
	if every > 0 {
		when := fc.wakeAt.Add(every)
		if !when.After(time.Now()) {
			when = time.Now().Add(every)
		}
		fc.wakeAt = when
		fc.wakeTimer.Reset(time.Until(when))
	} else {
		fc.wakeAt = time.Time{}
		fc.wakeEvery = 0
		fc.wakeTimer = nil
	}
	fc.wakeMu.Unlock()
	_ = fc.Wake()
}

func (fc *fdConn) RemoteAddr() net.Addr {
	if fc.remoteAddr.IsValid() {
		if fc.isDatagram() {
			return net.UDPAddrFromAddrPort(fc.remoteAddr)
		}
		return net.TCPAddrFromAddrPort(fc.remoteAddr)
	}
	// Non-IP peers (Unix sockets) keep their address object in the pair.
	if fc.addr != nil {
		return fc.addr.remote
	}
	return nil
}

func (fc *fdConn) Fd() int {
	var rc syscall.Conn

	if fc.udp != nil {
		rc = net.PacketConn(fc.udp.sock).(syscall.Conn)
	} else {
		rc = fc.conn.(syscall.Conn)
	}

	sc, err := rc.SyscallConn()
	if err != nil {
		return -1
	}

	var fd int
	err = sc.Control(func(h uintptr) { fd = int(h) })
	if nil != err {
		return -1
	}
	return fd
}

func (fc *fdConn) SetLinger(secs int) error {
	if fc.isClosing() {
		return net.ErrClosed
	}
	if tcpConn, ok := fc.conn.(*net.TCPConn); ok {
		return tcpConn.SetLinger(secs)
	}
	return ErrUnsupported
}

func (fc *fdConn) SetNoDelay(nodelay bool) error {
	if fc.isClosing() {
		return net.ErrClosed
	}
	if tcpConn, ok := fc.conn.(*net.TCPConn); ok {
		return tcpConn.SetNoDelay(nodelay)
	}
	return ErrUnsupported
}

func (fc *fdConn) SetKeepAlive(keepalive bool) error {
	if fc.isClosing() {
		return net.ErrClosed
	}
	if tcpConn, ok := fc.conn.(*net.TCPConn); ok {
		return tcpConn.SetKeepAlive(keepalive)
	}
	return ErrUnsupported
}

func (fc *fdConn) SetKeepAlivePeriod(secs int) error {
	if fc.isClosing() {
		return net.ErrClosed
	}
	if tcpConn, ok := fc.conn.(*net.TCPConn); ok {
		if err := tcpConn.SetKeepAlive(true); nil != err {
			return err
		}

		if err := tcpConn.SetKeepAlivePeriod(time.Duration(secs) * time.Second); nil != err {
			_ = tcpConn.SetKeepAlive(false)
			return err
		}
		return nil
	}
	return ErrUnsupported
}

func (fc *fdConn) SetReadBuffer(size int) error {
	if fc.isClosing() {
		return net.ErrClosed
	}
	if tcpConn, ok := fc.conn.(*net.TCPConn); ok {
		return tcpConn.SetReadBuffer(size)
	}
	return ErrUnsupported
}

func (fc *fdConn) SetWriteBuffer(size int) error {
	if fc.isClosing() {
		return net.ErrClosed
	}
	if tcpConn, ok := fc.conn.(*net.TCPConn); ok {
		return tcpConn.SetWriteBuffer(size)
	}
	return ErrUnsupported
}

// readDeadlineNotedForTest, when a test sets it, runs between setting a read
// deadline on the socket and recording it for the held wait.
var readDeadlineNotedForTest func()

// heldDeadlineExpiredForTest, when a test sets it, runs once a held wait has
// found its read deadline passed and before it acts on that.
var heldDeadlineExpiredForTest func()

func (fc *fdConn) applySocketOption(socketOptionKind, int) error { return ErrUnsupported }

func releaseStdBuffers(buffers []*bytebuf.Buffer) {
	for _, buffer := range buffers {
		bytebuf.ReleaseBuffer(buffer)
	}
}

var stdWriteScratchPool = sync.Pool{New: func() any { return new(stdWriteScratch) }}

// State reports where the connection's streams stand; see ConnState.
func (fc *fdConn) State() ConnState {
	var state ConnState
	// The same condition Read reports io.EOF by.
	if fc.readEOF.Load() && fc.inbound.Empty() && len(fc.inboundTail) == 0 {
		state |= StatePeerEnded
	}
	fc.mux.Lock()
	if fc.writeShut {
		state |= StateWriteShut
	}
	fc.mux.Unlock()
	if fc.isClosing() {
		state |= StateClosing
	}
	if fc.IsClosed() {
		state |= StateClosed
	}
	return state
}

// Tests pause the two ownership hand-offs without instrumenting I/O paths.
var testHookStdCloseRequested atomic.Pointer[func(*fdConn)]

var testHookStdCloseReleased atomic.Pointer[func(*fdConn)]

// testHookUDPChildCreated runs on the listener goroutine after a UDP child
// was created and published, before its OnOpen. Tests use it to pin the
// close-while-opening race; it is nil outside tests.
var testHookUDPChildCreated func(*fdConn)

func (fc *fdConn) Wake() error {
	if fc.isClosing() {
		return net.ErrClosed
	}
	// The wake still travels through the loop's queue, so it starts after
	// previously submitted tasks and one callback is scheduled per call. The
	// loop only dequeues it; see runWakeTask for why the callback itself runs
	// elsewhere.
	t := acquireTask(wakeTask, fc)
	if !fc.loop.submitTask(t) {
		releaseTask(t)
		return net.ErrClosed
	}
	return nil
}

// YieldRead asks for a continuation callback through the loop's queue. The
// reader goes on reading meanwhile, so a callback that waits for more bytes
// gets them; the end of the stream waits for the continuation.
func (fc *fdConn) YieldRead() error {
	if !fc.isDatagram() {
		fc.yieldPending.Store(true)
	}
	return fc.Wake()
}

// runWakeTask runs on the event loop, in submission order, and hands the
// callback to its own goroutine instead of waiting for the connection's
// callback mutex: a callback that dials synchronously waits for the loop to
// register the dialed connection, so the loop waiting here would close the
// cycle. The callback still cannot race the blocking read goroutine or
// OnClose — callbackMu serializes them — and it counts as a lifecycle callback
// so Wait joins it.
func (fc *fdConn) runWakeTask() error {
	fc.events().callbackWG.Add(1)
	go fc.deliverWake()
	return nil
}

func (fc *fdConn) deliverWake() {
	defer fc.events().callbackWG.Done()
	if fc.isClosing() {
		return
	}
	// A Wake callback must not race the blocking read goroutine's inbound view.
	fc.callbackMu.Lock()
	defer fc.callbackMu.Unlock()
	if fc.isClosing() {
		// The connection closed while this callback waited its turn.
		return
	}
	if !fc.opened {
		// OnOpen has not run yet; the read goroutine delivers this after it.
		fc.wakesAfterOpen++
		return
	}
	started := fc.beginInboundCallback()
	defer fc.endInboundCallback(started)
	before := fc.inputMark()
	if err := fc.events().onRead(fc); err != nil {
		fc.requestClose(err)
		return
	}
	if fc.readEOF.Load() && fc.yieldPending.Load() && fc.inputMark() == before {
		// The stream ended and this continuation moved nothing: no byte can
		// arrive to complete the unit its consumer waits for. It had its
		// chance; its unconsumed input goes as a close's does.
		fc.requestClose(io.EOF)
		return
	}
	fc.noteReadEnded()
	fc.closeDeferredReadEOF()
	if fc.readHeld && fc.inbound.Len() < fc.inputLimit(true) {
		fc.readHeld = false
	}
	// A resume or a drained hold lets a waiting reader go on; a close may
	// have filled the slot already, and either signal wakes it.
	fc.signalReadResume()
}

func (fc *fdConn) flushOnLoop() (int, error) { return 0, nil }

// writeLoop drains every coalesced notification to completion. writeSig is a
// level signal for "output exists", not one acknowledgement per Write.
// writeLoop drains every coalesced notification to completion until the
// connection closes: a close wakes it through the same notification channel,
// and the drain then reports the closed transport.
func (fc *fdConn) writeLoop() {
	defer fc.events().callbackWG.Done()
	for range fc.writeSig {
		written, err := fc.drainOutbound()
		if written > 0 {
			fc.events().onSocketBytesWrite(fc, written)
		}
		if err == nil {
			err = fc.shutWriteIfDrained()
		}
		if err != nil {
			fc.events().closeConn(fc, err)
			return
		}
	}
}

// noteReadEnded runs under callbackMu after an OnRead past the peer's end of
// stream. Once callbacks have consumed everything the peer sent, the reading
// half is over and the connection closes if its writing half is shut too.
func (fc *fdConn) noteReadEnded() {
	if !fc.readEOF.Load() || fc.readEnded.Load() || !fc.inbound.Empty() || len(fc.inboundTail) != 0 || !fc.readCloseReady() {
		return
	}
	fc.readEnded.Store(true)
	if fc.writeShutDone.Load() {
		fc.requestClose(io.EOF)
	}
}

// onReadEOF ends the read goroutine at the peer's end of stream. A plain
// connection without half-open closes at once when its consumer has nothing
// left to see: nothing retained, no pause, no continuation owed. Otherwise
// the end is recorded and OnRead runs once more, so a consumer sees what it
// retained and a half-open one sees Read report io.EOF. The connection then
// closes when the consumer no longer holds the end back (closeDeferredReadEOF),
// when both halves of a half-open one ended, or — layered — when the stack
// handed the end up (stack.ended).
func (fc *fdConn) onReadEOF() {
	fc.callbackMu.Lock()
	if fc.isClosing() {
		fc.callbackMu.Unlock()
		return
	}
	if !fc.events().AllowHalfOpen && !fc.entry.layered && !fc.yieldPending.Load() && !fc.currentReadControl().Held() &&
		fc.inbound.Empty() && len(fc.inboundTail) == 0 {
		fc.callbackMu.Unlock()
		fc.events().closeConn(fc, io.EOF)
		return
	}
	fc.readEOF.Store(true)
	started := fc.beginInboundCallback()
	err := fc.events().onRead(fc)
	fc.endInboundCallback(started)
	if err == nil {
		fc.noteReadEnded()
		fc.closeDeferredReadEOF()
	}
	fc.callbackMu.Unlock()
	if err != nil {
		fc.events().closeConn(fc, err)
	}
}

// Read is commonConn.Read that reports io.EOF once a half-open connection's
// peer has shut its writing half and the buffered bytes are consumed.
func (fc *fdConn) Read(b []byte) (int, error) {
	n, err := fc.commonConn.Read(b)
	if n == 0 && err == nil && fc.readEOF.Load() && fc.inbound.Empty() && len(fc.inboundTail) == 0 {
		return 0, io.EOF
	}
	return n, err
}

// ReserveInbound implements Conn.ReserveInbound.
func (fc *fdConn) ReserveInbound(n int) error {
	if fc.isDatagram() || n <= 0 {
		return nil
	}
	return fc.reserveInbound(n, fc.events().MaxInboundBuffered)
}

// readLoop delivers n > 0 before a simultaneous terminal error, as required by
// io.Reader. callbackMu protects both the borrowed slice and retained inbound
// storage for the full callback lifetime.
func (fc *fdConn) readLoop() {
	defer fc.events().callbackWG.Done()

	holder := fc.events().readPool.Get()
	defer fc.events().readPool.Put(holder)
	buffer := holder.bytes
	// OnOpen may have paused before the first read.
	wait := true
	for {
		if wait && !fc.waitReadResume() {
			return
		}
		n, readErr := fc.conn.Read(buffer)
		wait = false
		if n > 0 {
			// A Reader may return payload and a terminal error together. Deliver the
			// payload before closing so the final stream segment is not lost.
			fc.callbackMu.Lock()
			if fc.isClosing() {
				// OnClose may already have run: a close requested while this
				// read was returning ends the stream here.
				fc.callbackMu.Unlock()
				return
			}
			fc.inboundTail = buffer[:n]
			fc.events().onSocketBytesRead(fc, n)
			started := fc.beginInboundCallback()
			if callbackErr := fc.events().onRead(fc); callbackErr != nil {
				fc.endInboundCallback(started)
				fc.callbackMu.Unlock()
				fc.events().closeConn(fc, callbackErr)
				return
			}
			if len(fc.inboundTail) > 0 {
				_, _ = fc.inbound.Write(fc.inboundTail)
				fc.inboundTail = fc.inboundTail[:0]
				if limit := fc.inputLimit(fc.readHeld); limit > 0 && fc.inbound.Len() >= limit {
					fc.readHeld = true
				}
			}
			wait = fc.readHeld || fc.ReadsPaused()
			fc.endInboundCallback(started)
			fc.callbackMu.Unlock()
		}
		if readErr != nil {
			// The end of the stream is the consumer's to see, paused or not;
			// a fatal error ends the connection at once, as CloseWith can
			// discard input not yet consumed.
			if readErr == io.EOF {
				fc.onReadEOF()
				return
			}
			fc.events().closeConn(fc, readErr)
			return
		}
	}
}

// waitReadResume keeps the reader off the socket while the consumer is
// paused or a held read waits for a callback to consume below
// MaxInboundBuffered, and reports whether the read loop goes on. A close ends
// the wait, and so does the read deadline, which closes the connection as an
// expired Read would. A resume, a drained hold and a changed deadline signal
// readResume, so the wait looks again.
func (fc *fdConn) waitReadResume() bool {
	var timer *time.Timer
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	for {
		fc.callbackMu.Lock()
		held := fc.readHeld || fc.ReadsPaused()
		fc.callbackMu.Unlock()
		if fc.isClosing() {
			return false
		}
		if !held {
			return true
		}
		var expired <-chan time.Time
		fc.deadlineMu.Lock()
		deadline := fc.readDeadline
		if !deadline.IsZero() && !time.Now().Before(deadline) {
			// Closed under the lock, so a setter that replaces this deadline
			// either comes first or finds the connection closing. The close
			// request only queues a task, so holding the lock costs nothing.
			if heldDeadlineExpiredForTest != nil {
				heldDeadlineExpiredForTest()
			}
			fc.events().closeConn(fc, os.ErrDeadlineExceeded)
			fc.deadlineMu.Unlock()
			return false
		}
		fc.deadlineMu.Unlock()
		if !deadline.IsZero() {
			wait := time.Until(deadline)
			if timer == nil {
				timer = time.NewTimer(wait)
			} else {
				timer.Reset(wait)
			}
			expired = timer.C
		}
		select {
		case <-fc.readResume:
		case <-expired:
			continue // rechecked above, against a deadline moved meanwhile
		}
		if fc.isClosing() {
			return false
		}
	}
}

func (fc *fdConn) fireWriteEvent() error {
	if nil == fc.conn {
		return nil // udp client nothing to do.
	}

	fc.events().callbackWG.Add(1)
	go fc.writeLoop()

	return nil
}

// fireReadEvent starts the connection's read goroutine, which runs OnOpen
// before its first read; see openOnReader.
func (fc *fdConn) fireReadEvent() error {
	fc.events().callbackWG.Add(1)
	go func() {
		if !fc.openOnReader() {
			fc.events().callbackWG.Done()
			return
		}
		if fc.udp != nil {
			fc.readUDPLoop() // connected UDP
		} else {
			fc.readLoop()
		}
	}()
	return nil
}
