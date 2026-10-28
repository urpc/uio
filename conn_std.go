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
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"github.com/urpc/uio/internal/bytebuf"
	"github.com/urpc/uio/internal/poller"
)

// Keep tiny writes coalesced while bounding payload work done under mux.
const stdOwnedWriteThreshold = 4 << 10
const stdWriteVecLimit = 16

// fdConn uses one blocking reader and one blocking writer per stream. mux
// protects producer admission and counters; writeMu protects the detached
// batch while the writer performs blocking net.Conn writes; callbackMu keeps
// OnRead, Wake, and OnClose serialized for the connection.
// stdUDPState carries the datagram fields off the stream connections, which
// are the vast majority: sock is the shared packet socket of a server's peers
// or a connected client's socket, svr links a logical peer back to its
// server, and peers is the server's logical-peer map.
type stdUDPState struct {
	sock  *net.UDPConn
	svr   *fdConn
	peers map[netip.AddrPort]*fdConn

	// deadlines are a server child's own: the socket it shares with its
	// server and every other peer must not carry them. Allocated by the
	// first nonzero deadline; see applyChildDeadline.
	deadlines *childDeadlines

	// Datagrams are sent on whichever goroutine writes them, and the bytes
	// they report wait in outboundPending for whoever holds outboundClaim; see
	// reportOutbound.
	outboundPending atomic.Int64
	outboundClaim   atomic.Bool

	// A server's accepted close detaches the address table immediately so
	// DialPeer and WriteTo refuse it; the counted close task owns it until
	// physical release, even when the close queue has already stopped.
	closingPeers map[netip.AddrPort]*fdConn

	// controlling lists the goroutines inside a Control on a server's
	// socket, its peers' included. Closing the socket waits for their
	// descriptor references, so Listener.Close cannot close it on one of
	// them; see closeListenerSocket.
	controlMu   sync.Mutex
	controlling []int64
	// sealed refuses new Controls once a close found none running and went
	// on to close the socket: a Control that started after the check would
	// hold a descriptor reference that close waits for, and a Close inside
	// it would wait for that close in turn.
	sealed bool
}

type fdConn struct {
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
	// asked for. draining is set once; drainCause, drainWait and drainTimer
	// are guarded by mux and live only while it is set.
	draining   atomic.Bool
	drainCause error
	drainWait  time.Duration
	drainTimer *time.Timer

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
func (fc *fdConn) setInterest(poller.Interest)      {}
func (fc *fdConn) currentInterest() poller.Interest { return poller.Readable }
func (fc *fdConn) isClosing() bool                  { return fc.closing.Load() || fc.IsClosed() }
func (fc *fdConn) beginShutdown()                   {}
func (fc *fdConn) isDatagram() bool                 { return fc.udp != nil }
func (fc *fdConn) isClosedOnLoop() bool             { return fc.IsClosed() }

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
	fc.events.callbackWG.Add(1)
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
		err := fc.events.onRead(fc)
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
	if callback := fc.events.cb.onOpen; callback != nil {
		started := fc.beginInboundCallback()
		callback(fc)
		fc.endInboundCallback(started)
	}
}

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
			return errUnsupported
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

// enterControl records the calling goroutine as inside a Control on the
// socket, until leaveControl; it refuses once a close sealed the socket.
func (udp *stdUDPState) enterControl() (int64, bool) {
	id := currentGoroutineID()
	udp.controlMu.Lock()
	defer udp.controlMu.Unlock()
	if udp.sealed {
		return 0, false
	}
	udp.controlling = append(udp.controlling, id)
	return id, true
}

func (udp *stdUDPState) leaveControl(id int64) {
	udp.controlMu.Lock()
	for i, held := range udp.controlling {
		if held == id {
			udp.controlling = append(udp.controlling[:i], udp.controlling[i+1:]...)
			break
		}
	}
	udp.controlMu.Unlock()
}

// controlsQuiet waits, bounded, for the Controls on the socket to end, and
// seals it against new ones as it finds none: the check and the seal are one
// step, so no Control slips in between it and the socket's close.
func (udp *stdUDPState) controlsQuiet() bool {
	deadline := time.Now().Add(socketCloseWait)
	for {
		udp.controlMu.Lock()
		held := len(udp.controlling)
		if held == 0 {
			udp.sealed = true
		}
		udp.controlMu.Unlock()
		if held == 0 {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(time.Millisecond)
	}
}

// inControl reports whether the calling goroutine is inside a Control on the
// socket.
func (udp *stdUDPState) inControl() bool {
	id := currentGoroutineID()
	udp.controlMu.Lock()
	defer udp.controlMu.Unlock()
	for _, held := range udp.controlling {
		if held == id {
			return true
		}
	}
	return false
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
	if fc.events.cb.onTimer == nil {
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
	if err := fc.events.cb.onTimer(fc); err != nil {
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

// closeListenerSocket closes a UDP listener's server connection on the
// Listener.Close caller, so its address is free when Close returns. A Control
// on the socket holds a descriptor reference the socket close waits for: one
// by the caller itself would wait forever, and one that outlasts
// socketCloseWait may be waiting for this very Close, so both fall back to
// the counted close task, which closes the socket once the Control returns.
func (fc *fdConn) closeListenerSocket(err error) {
	events := fc.events
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
	return errUnsupported
}

func (fc *fdConn) SetNoDelay(nodelay bool) error {
	if fc.isClosing() {
		return net.ErrClosed
	}
	if tcpConn, ok := fc.conn.(*net.TCPConn); ok {
		return tcpConn.SetNoDelay(nodelay)
	}
	return errUnsupported
}

func (fc *fdConn) SetKeepAlive(keepalive bool) error {
	if fc.isClosing() {
		return net.ErrClosed
	}
	if tcpConn, ok := fc.conn.(*net.TCPConn); ok {
		return tcpConn.SetKeepAlive(keepalive)
	}
	return errUnsupported
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
	return errUnsupported
}

func (fc *fdConn) SetReadBuffer(size int) error {
	if fc.isClosing() {
		return net.ErrClosed
	}
	if tcpConn, ok := fc.conn.(*net.TCPConn); ok {
		return tcpConn.SetReadBuffer(size)
	}
	return errUnsupported
}

func (fc *fdConn) SetWriteBuffer(size int) error {
	if fc.isClosing() {
		return net.ErrClosed
	}
	if tcpConn, ok := fc.conn.(*net.TCPConn); ok {
		return tcpConn.SetWriteBuffer(size)
	}
	return errUnsupported
}

func (fc *fdConn) SetDeadline(deadline time.Time) error {
	return fc.applyDeadline(deadlineBoth, deadline)
}

func (fc *fdConn) SetReadDeadline(deadline time.Time) error {
	return fc.applyDeadline(deadlineRead, deadline)
}

func (fc *fdConn) SetWriteDeadline(deadline time.Time) error {
	return fc.applyDeadline(deadlineWrite, deadline)
}

// readDeadlineNotedForTest, when a test sets it, runs between setting a read
// deadline on the socket and recording it for the held wait.
var readDeadlineNotedForTest func()

// heldDeadlineExpiredForTest, when a test sets it, runs once a held wait has
// found its read deadline passed and before it acts on that.
var heldDeadlineExpiredForTest func()

// applyDeadline delegates directly to the blocking net.Conn; unlike native
// timers, the Go network poller owns interruption and reset semantics. A read
// deadline the socket accepted is also recorded for a held read, which reads
// no socket. deadlineMu orders the two updates against other setters and
// against the held wait's expiry, which closes the connection under it: a
// setter either finds the connection closing or replaces the deadline before
// the wait can act on the old one.
func (fc *fdConn) applyDeadline(kind deadlineKind, deadline time.Time) error {
	if fc.isClosing() {
		return net.ErrClosed
	}
	if fc.udp != nil && fc.udp.svr != nil {
		return fc.applyChildDeadline(kind, deadline)
	}
	var target net.Conn
	if fc.conn != nil {
		target = fc.conn
	} else if fc.udp != nil {
		target = fc.udp.sock
	}
	if target == nil {
		return errUnsupported
	}
	fc.deadlineMu.Lock()
	defer fc.deadlineMu.Unlock()
	if fc.isClosing() {
		return net.ErrClosed
	}
	var err error
	switch kind {
	case deadlineBoth:
		err = target.SetDeadline(deadline)
	case deadlineRead:
		err = target.SetReadDeadline(deadline)
	default:
		err = target.SetWriteDeadline(deadline)
	}
	if err != nil || kind == deadlineWrite || fc.conn == nil {
		return err
	}
	if readDeadlineNotedForTest != nil {
		readDeadlineNotedForTest()
	}
	fc.readDeadline = deadline
	// Wake a held wait to re-time against the new deadline.
	select {
	case fc.readResume <- struct{}{}:
	default:
	}
	return nil
}

// childDeadlines times a UDP server child's deadlines, under the child's
// deadlineMu. The child reads no socket of its own — its server's listener
// reads the shared one for every peer — so a deadline there would end the
// server and all its peers. As on the native backend, a deadline that passes
// closes only this child, with an error that wraps os.ErrDeadlineExceeded.
type childDeadlines struct {
	stopped bool
	read    childDeadline
	write   childDeadline
}

type childDeadline struct {
	at         time.Time
	generation uint64
	timer      *time.Timer
}

func (fc *fdConn) applyChildDeadline(kind deadlineKind, deadline time.Time) error {
	fc.deadlineMu.Lock()
	defer fc.deadlineMu.Unlock()
	state := fc.udp.deadlines
	if state == nil {
		if deadline.IsZero() {
			return nil
		}
		state = &childDeadlines{}
		fc.udp.deadlines = state
	}
	if state.stopped || fc.isClosing() {
		return net.ErrClosed
	}
	if kind == deadlineBoth || kind == deadlineRead {
		fc.armChildDeadline(&state.read, deadlineRead, deadline)
	}
	if kind == deadlineBoth || kind == deadlineWrite {
		fc.armChildDeadline(&state.write, deadlineWrite, deadline)
	}
	return nil
}

// armChildDeadline replaces one deadline; it requires deadlineMu. The
// generation makes a timer that already fired for the old deadline harmless.
func (fc *fdConn) armChildDeadline(slot *childDeadline, kind deadlineKind, deadline time.Time) {
	slot.generation++
	slot.at = deadline
	if slot.timer != nil {
		slot.timer.Stop()
	}
	if deadline.IsZero() {
		return
	}
	generation := slot.generation
	slot.timer = time.AfterFunc(max(time.Until(deadline), 0), func() {
		fc.expireChildDeadline(slot, kind, generation)
	})
}

func (fc *fdConn) expireChildDeadline(slot *childDeadline, kind deadlineKind, generation uint64) {
	fc.deadlineMu.Lock()
	expired := !fc.udp.deadlines.stopped && slot.generation == generation &&
		!slot.at.IsZero() && !time.Now().Before(slot.at)
	fc.deadlineMu.Unlock()
	if expired {
		fc.requestClose(fmt.Errorf("uio: %s deadline: %w", deadlineName(kind), os.ErrDeadlineExceeded))
	}
}

// stopChildDeadlines cancels a closing child's timers and refuses new ones.
func (fc *fdConn) stopChildDeadlines() {
	fc.deadlineMu.Lock()
	defer fc.deadlineMu.Unlock()
	state := fc.udp.deadlines
	if state == nil {
		return
	}
	state.stopped = true
	for _, slot := range []*childDeadline{&state.read, &state.write} {
		if slot.timer != nil {
			slot.timer.Stop()
		}
	}
}

func (fc *fdConn) applySocketOption(socketOptionKind, int) error { return errUnsupported }

func (fc *fdConn) WriteByte(b byte) error {
	var bb [1]byte
	bb[0] = b
	_, err := fc.Write(bb[:])
	return err
}

func (fc *fdConn) WriteString(s string) (n int, err error) {
	var data = unsafe.Slice(unsafe.StringData(s), len(s))
	return fc.Write(data)
}

// Write copies tiny payloads into the shared outbound tail and transfers larger
// payloads as owned blocks, keeping allocation and copying outside mux.
func (fc *fdConn) Write(p []byte) (n int, err error) {
	if fc.isClosing() {
		return 0, net.ErrClosed
	}

	if fc.udp != nil {

		if nil == fc.udp.svr && nil == fc.udp.peers {
			// connected udp client.
			n, err = fc.udp.sock.Write(p)
		} else {
			// udp child connection.
			n, err = fc.udp.sock.WriteTo(p, net.UDPAddrFromAddrPort(fc.remoteAddr))
		}

		if n > 0 {
			fc.reportOutbound(n)
		}
		return
	}
	if len(p) == 0 {
		return 0, nil
	}
	if len(p) >= stdOwnedWriteThreshold {
		if limit := fc.events.socketLimit(); limit > 0 && len(p) > limit {
			return 0, ErrOutboundOverflow
		}
		owned := bytebuf.CloneBuffer(p)
		n, err := fc.WriteOwned(owned)
		if errors.Is(err, ErrOutboundOverflow) {
			// The clone never entered the queue, so Write still owns it.
			bytebuf.ReleaseBuffer(owned)
		}
		return n, err
	}

	fc.mux.Lock()
	if fc.isClosing() || fc.draining.Load() || fc.writeShut {
		fc.mux.Unlock()
		return 0, net.ErrClosed
	}
	if limit := fc.events.socketLimit(); limit > 0 && fc.outbound.Len()+fc.writeBytes > limit-len(p) {
		fc.mux.Unlock()
		return 0, ErrOutboundOverflow
	}
	n, err = fc.outbound.Write(p)
	fc.mux.Unlock()

	if nil != err {
		// unreachable here.
		return
	}

	select {
	case fc.writeSig <- struct{}{}:
	default:
	}

	return
}

// reportOutbound runs OnBytesWritten for a datagram send. Datagrams are sent on
// whichever goroutine writes them, so the calls for one connection go through
// a claim: a sender adds its bytes and reports them itself if it takes the
// claim, and otherwise leaves them to the holder, which reports until nothing
// is pending, so the calls never overlap and a send made inside OnBytesWritten is
// reported once that call returns. Nobody waits for the claim, so OnBytesWritten
// callbacks that write to each other cannot deadlock.
func (fc *fdConn) reportOutbound(n int) {
	if fc.events.cb.onBytesWritten == nil {
		return
	}
	udp := fc.udp
	udp.outboundPending.Add(int64(n))
	for udp.outboundClaim.CompareAndSwap(false, true) {
		fc.drainOutboundReports()
		// Bytes added between the last drain and the release found the
		// claim taken; look again.
		if udp.outboundPending.Load() == 0 {
			return
		}
	}
}

// drainOutboundReports reports pending bytes while holding the claim, and
// releases it even when OnBytesWritten panics.
func (fc *fdConn) drainOutboundReports() {
	udp := fc.udp
	defer udp.outboundClaim.Store(false)
	for {
		pending := udp.outboundPending.Swap(0)
		if pending == 0 {
			return
		}
		fc.events.cb.onBytesWritten(fc.events.observed(fc), int(pending))
	}
}

// Writev preserves large source segments as separate owned blocks when the
// fixed writer vector can carry them; larger segment counts collapse into one
// block to avoid retaining an unbounded pointer list.
func (fc *fdConn) Writev(vec [][]byte) (n int, err error) {

	if fc.isClosing() {
		return 0, net.ErrClosed
	}

	if fc.udp != nil {
		return 0, errUnsupported
	}

	total := 0
	segments := 0
	for _, segment := range vec {
		if len(segment) > int(^uint(0)>>1)-total {
			return 0, ErrOutboundOverflow
		}
		total += len(segment)
		if len(segment) > 0 {
			segments++
		}
	}
	if total == 0 {
		return 0, nil
	}
	if total >= stdOwnedWriteThreshold {
		if limit := fc.events.socketLimit(); limit > 0 && total > limit {
			return 0, ErrOutboundOverflow
		}
		var inline [8]*bytebuf.Buffer
		owned := inline[:0]
		if segments > len(inline) {
			owned = append(owned, bytebuf.CloneBuffers(vec, total))
		} else {
			for _, segment := range vec {
				if len(segment) > 0 {
					owned = append(owned, bytebuf.CloneBuffer(segment))
				}
			}
		}
		fc.mux.Lock()
		if fc.isClosing() || fc.writeShut {
			fc.mux.Unlock()
			releaseStdBuffers(owned)
			return 0, net.ErrClosed
		}
		if limit := fc.events.socketLimit(); limit > 0 && fc.outbound.Len()+fc.writeBytes > limit-total {
			fc.mux.Unlock()
			releaseStdBuffers(owned)
			return 0, ErrOutboundOverflow
		}
		for _, buffer := range owned {
			fc.outbound.AppendOwned(buffer)
		}
		fc.mux.Unlock()
		select {
		case fc.writeSig <- struct{}{}:
		default:
		}
		return total, nil
	}
	fc.mux.Lock()
	if fc.isClosing() || fc.writeShut {
		fc.mux.Unlock()
		return 0, net.ErrClosed
	}
	if limit := fc.events.socketLimit(); limit > 0 && fc.outbound.Len()+fc.writeBytes > limit-total {
		fc.mux.Unlock()
		return 0, ErrOutboundOverflow
	}
	n, err = fc.outbound.Writev(vec)
	fc.mux.Unlock()

	if nil != err {
		// unreachable here.
		return
	}

	select {
	case fc.writeSig <- struct{}{}:
	default:
	}

	return
}

func releaseStdBuffers(buffers []*bytebuf.Buffer) {
	for _, buffer := range buffers {
		bytebuf.ReleaseBuffer(buffer)
	}
}

// ReserveOutbound is unsupported: the dedicated writer goroutine may detach
// outbound at any moment, so bytes cannot be left in it to be filled later.
func (fc *fdConn) ReserveOutbound(int) ([]byte, error) {
	if fc.isClosing() {
		return nil, net.ErrClosed
	}
	return nil, ErrReserveUnsupported
}

func (fc *fdConn) WriteOwned(owned *Buffer) (n int, err error) {
	if owned == nil {
		return 0, nil
	}
	size := owned.Len()
	if size == 0 {
		bytebuf.ReleaseBuffer(owned)
		return 0, nil
	}
	if fc.isClosing() {
		bytebuf.ReleaseBuffer(owned)
		return 0, net.ErrClosed
	}
	if fc.udp != nil {
		defer bytebuf.ReleaseBuffer(owned)
		return fc.Write(owned.Bytes())
	}

	fc.mux.Lock()
	if fc.isClosing() || fc.draining.Load() || fc.writeShut {
		fc.mux.Unlock()
		bytebuf.ReleaseBuffer(owned)
		return 0, net.ErrClosed
	}
	if limit := fc.events.socketLimit(); limit > 0 && fc.outbound.Len()+fc.writeBytes > limit-size {
		fc.mux.Unlock()
		// An overflowed buffer was never accepted, so it remains the caller's.
		return 0, ErrOutboundOverflow
	}
	fc.outbound.AppendOwned(owned)
	fc.mux.Unlock()

	select {
	case fc.writeSig <- struct{}{}:
	default:
	}
	return size, nil
}

func (fc *fdConn) Flush() error {
	if fc.isClosing() {
		return net.ErrClosed
	}
	// Writes are FIFO in outbound. A coalesced wake is enough to make the
	// dedicated writer drain everything accepted before this call.
	select {
	case fc.writeSig <- struct{}{}:
	default:
	}
	return nil
}

// stdWriteScratch is the fixed scratch one drain fills and hands over. It
// lives in a pool rather than on the writer goroutine's stack: both the
// vector and the net.Buffers wrapper reach net.Buffers.WriteTo as values
// whose address the compiler must assume escapes, which would move the vector
// to the heap once per connection, for the connection's whole life, and
// allocate the wrapper once per drain. Pooled, an idle connection holds
// neither.
type stdWriteScratch struct {
	vec  [stdWriteVecLimit][]byte
	bufs net.Buffers
}

var stdWriteScratchPool = sync.Pool{New: func() any { return new(stdWriteScratch) }}

// drainOutbound swaps the producer buffer into writeBatch before blocking on
// the network. Producers immediately continue on a fresh CompositeBuffer;
// writeMu prevents close cleanup from returning in-flight blocks to the pool.
func (fc *fdConn) drainOutbound() (int, error) {
	scratch := stdWriteScratchPool.Get().(*stdWriteScratch)
	defer func() {
		clear(scratch.vec[:])
		scratch.bufs = nil
		stdWriteScratchPool.Put(scratch)
	}()
	fc.writeMu.Lock()
	defer fc.writeMu.Unlock()

	fc.mux.Lock()
	if fc.isClosing() {
		fc.mux.Unlock()
		return 0, net.ErrClosed
	}
	fc.writeBytes = fc.outbound.Len()
	if fc.writeBytes == 0 {
		fc.mux.Unlock()
		if fc.draining.Load() {
			// A graceful close waits for an empty queue; the writer just
			// found one, or drained the last of it elsewhere.
			fc.finishDrain()
		}
		return 0, nil
	}
	// The detached batch is immutable while producers append to a fresh buffer.
	fc.outbound, fc.writeBatch = fc.writeBatch, fc.outbound
	fc.mux.Unlock()

	totalWritten := 0
	var writeErr error
	for !fc.writeBatch.Empty() {
		buffers, size := fc.writeBatch.PeekVecN(scratch.vec[:], len(scratch.vec))
		scratch.bufs = buffers
		written, err := scratch.bufs.WriteTo(fc.conn)
		clear(buffers)
		if written > 0 {
			n := int(written)
			fc.writeBatch.Discard(n)
			totalWritten += n
			fc.mux.Lock()
			fc.writeBytes -= n
			if fc.draining.Load() && fc.drainTimer != nil {
				// A drain's queue is moving: its bound starts over, as the
				// native backend's does on a send report.
				fc.drainTimer.Reset(fc.drainWait)
			}
			fc.mux.Unlock()
		}
		if err != nil {
			writeErr = err
			break
		}
		if written < int64(size) {
			writeErr = io.ErrShortWrite
			break
		}
	}
	fc.writeBatch.Reset()
	fc.mux.Lock()
	fc.writeBytes = 0
	empty := fc.draining.Load() && fc.outbound.Len() == 0
	fc.mux.Unlock()
	if empty {
		// The writer drained everything queued: a graceful close's wait is
		// over. Producers are refused while draining, so nothing follows.
		fc.finishDrain()
	}
	return totalWritten, writeErr
}

func (fc *fdConn) Close() error {
	return fc.CloseWith(io.ErrUnexpectedEOF)
}

// CloseGracefully closes as CloseWith does: this backend's writer flushes
// what it holds before the close, so nothing accepted waits on a queue.
func (fc *fdConn) CloseGracefully(cause error) error {
	if fc.udp != nil || fc.loop == nil {
		return fc.CloseWith(cause)
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
	if !fc.draining.Swap(false) {
		return
	}
	fc.mux.Lock()
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

func (fc *fdConn) CloseWith(err error) error {
	return fc.enqueueClose(err)
}

func (fc *fdConn) enqueueClose(err error) error {
	fc.mux.Lock()
	if fc.isClosing() || (fc.udp != nil && fc.udp.svr != nil && fc.udp.svr.isClosing()) {
		fc.mux.Unlock()
		return net.ErrClosed
	}
	// Publish the first request's cause together with closing. A listener's
	// release may overtake the task, but must retain that requested cause.
	fc.err = err
	// The request joins the lifecycle before closing is published: until
	// then the connection's own count holds Serve's join, and once it is
	// published a shutdown leaves the close to this request, which must
	// still be counted when it hands the close on.
	fc.events.callbackWG.Add(1)
	fc.closing.Store(true)
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
		fc.events.callbackWG.Done() // the loop, which Serve joins, has it
		return nil
	}
	releaseTask(t)
	// A sealed queue transfers the close, with the request's count, to a
	// goroutine. Keep cleanup off the caller: it may hold a RawConn.Control
	// descriptor reference, or another resource that closing the socket must
	// wait for it to release.
	go func() {
		defer fc.events.callbackWG.Done()
		fc.closeOnLoop(err)
	}()
	return nil
}

func (fc *fdConn) requestClose(err error) { _ = fc.enqueueClose(err) }

// peerKey is the canonical key of a UDP peer: an IPv4-mapped address
// collapses to its IPv4 form, so the dialing and receiving sides of one
// endpoint name it the same whatever form either was given.
func peerKey(addr netip.AddrPort) netip.AddrPort {
	return netip.AddrPortFrom(addr.Addr().Unmap(), addr.Port())
}

// dialPeer adds a child for addr to a UDP server; see Listener.DialPeer.
// The child is published under the server's mutex, and its OnOpen runs on a
// goroutine of its own serialized with the reader by the child's callback
// mutex; a datagram from the peer that arrives first runs it there instead.
func (fc *fdConn) dialPeer(addr netip.AddrPort, userdata any) (Conn, error) {
	// The open joins the lifecycle before the peer is visible to any cleanup,
	// in one section with the shutdown publication: either the Events is
	// closing and none of this runs, or the open is counted and Serve's join
	// waits for it, so no callback slips past Serve's return.
	if err := fc.checkPeerAddr(addr); err != nil {
		return nil, err
	}
	events := fc.events
	events.mux.Lock()
	if events.closing.Load() {
		events.mux.Unlock()
		return nil, net.ErrClosed
	}
	events.callbackWG.Add(1)
	events.mux.Unlock()

	fc.mux.Lock()
	if fc.udp == nil || fc.udp.peers == nil {
		fc.mux.Unlock()
		events.callbackWG.Done()
		return nil, net.ErrClosed
	}
	rAddr := peerKey(addr)
	if existing := fc.udp.peers[rAddr]; existing != nil && !existing.isClosing() {
		fc.mux.Unlock()
		events.callbackWG.Done()
		return existing, ErrPeerExists
	}
	// A closing peer's counted task owns its end even after replacement.
	child := &fdConn{}
	child.udp = &stdUDPState{sock: fc.udp.sock, svr: fc}
	child.addr = fc.addr
	child.setRemoteAddr(net.UDPAddrFromAddrPort(rAddr))
	child.loop = fc.loop
	child.events = fc.events
	child.SetUserdata(userdata)
	child.retainLifecycle()
	fc.udp.peers[rAddr] = child
	fc.mux.Unlock()
	go func() {
		defer events.callbackWG.Done()
		child.openOnReader()
	}()
	return child, nil
}

// checkPeerAddr reports whether addr can be a peer of this UDP socket: the
// native backend's DialPeer refuses it in the address translation, and a
// child for an unusable address would come back to life only to fail every
// send.
func (fc *fdConn) checkPeerAddr(addr netip.AddrPort) error {
	ip := addr.Addr()
	if !ip.IsValid() {
		return fmt.Errorf("uio: dial peer %v: %w", addr, syscall.EINVAL)
	}
	local := fc.LocalAddr().(*net.UDPAddr)
	if local.IP.To4() != nil && !ip.Unmap().Is4() {
		return fmt.Errorf("uio: dial peer %v on udp4: %w", addr, syscall.EAFNOSUPPORT)
	}
	return nil
}

// writeToPeer sends one datagram to addr from a UDP server's socket; see
// Listener.WriteTo.
func (fc *fdConn) writeToPeer(b []byte, addr netip.AddrPort) (int, error) {
	fc.mux.Lock()
	peers := fc.udp.peers
	fc.mux.Unlock()
	if peers == nil {
		return 0, net.ErrClosed
	}
	return fc.udp.sock.WriteTo(b, net.UDPAddrFromAddrPort(addr))
}

// closeOnLoop owns resource teardown and detaches UDP children. The user
// callback is scheduled separately so a slow OnClose cannot block the logical
// event loop or race an active OnRead callback.
//
// Closers arrive from several goroutines at once — a Listener.Close on its
// caller's, Events.Close, a loop's close task — so fc.mux claims the close
// exclusively: the first one in publishes the reason and tears down, and the
// rest find the connection closed and return without touching anything. The
// reason publishes before the closed flag: a reader that sees closed observes
// err without further synchronization.
func (fc *fdConn) closeOnLoop(err error) {
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
	fc.scheduleCloseCallback(err)
}

// Tests pause the two ownership hand-offs without instrumenting I/O paths.
var testHookStdCloseRequested atomic.Pointer[func(*fdConn)]
var testHookStdCloseReleased atomic.Pointer[func(*fdConn)]

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

// testHookUDPChildCreated runs on the listener goroutine after a UDP child
// was created and published, before its OnOpen. Tests use it to pin the
// close-while-opening race; it is nil outside tests.
var testHookUDPChildCreated func(*fdConn)

func (fc *fdConn) scheduleCloseCallback(err error) {
	fc.closeReady.Store(true)
	if !fc.closeScheduled.CompareAndSwap(false, true) {
		return
	}
	// The connection's lifecycle reservation, or its still-counted owner
	// for an unregistered/internal connection, precedes this Add. Shutdown
	// cannot finish between resource release and this hand-off.
	fc.events.callbackWG.Add(1)
	go func() {
		defer fc.events.callbackWG.Done()
		fc.deliverCloseCallback(err)
	}()
}

// deliverCloseCallback runs one connection's OnClose under its callback
// mutex, exactly once per connection.
func (fc *fdConn) deliverCloseCallback(err error) {
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
		defer fc.events.callbackWG.Done()
	}
	if callback := fc.events.cb.onClose; callback != nil && !fc.internal {
		fc.inbound.Reset()
		fc.inboundTail = nil
		started := fc.beginInboundCallback()
		callback(fc, err)
		fc.endInboundCallback(started)
	}
}

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

func (fc *fdConn) YieldRead() error { return fc.Wake() }

// runWakeTask runs on the event loop, in submission order, and hands the
// callback to its own goroutine instead of waiting for the connection's
// callback mutex: a callback that dials synchronously waits for the loop to
// register the dialed connection, so the loop waiting here would close the
// cycle. The callback still cannot race the blocking read goroutine or
// OnClose — callbackMu serializes them — and it counts as a lifecycle callback
// so Wait joins it.
func (fc *fdConn) runWakeTask() error {
	fc.events.callbackWG.Add(1)
	go fc.deliverWake()
	return nil
}

func (fc *fdConn) deliverWake() {
	defer fc.events.callbackWG.Done()
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
	if err := fc.events.onRead(fc); err != nil {
		fc.requestClose(err)
		return
	}
	fc.noteReadEnded()
	if fc.readHeld && fc.inbound.Len() < fc.events.MaxInboundBuffered {
		fc.readHeld = false
		// A close may have filled the slot already; either signal resumes.
		select {
		case fc.readResume <- struct{}{}:
		default:
		}
	}
}

func (fc *fdConn) flushOnLoop() (int, error)          { return 0, nil }
func (fc *fdConn) handleTimeout(deadlineKind, uint64) {}

// writeLoop drains every coalesced notification to completion. writeSig is a
// level signal for "output exists", not one acknowledgement per Write.
// writeLoop drains every coalesced notification to completion until the
// connection closes: a close wakes it through the same notification channel,
// and the drain then reports the closed transport.
func (fc *fdConn) writeLoop() {
	defer fc.events.callbackWG.Done()
	for range fc.writeSig {
		written, err := fc.drainOutbound()
		if written > 0 {
			fc.events.onSocketBytesWrite(fc, written)
		}
		if err == nil {
			err = fc.shutWriteIfDrained()
		}
		if err != nil {
			fc.events.closeConn(fc, err)
			return
		}
	}
}

// halfCloser is a net.Conn that can shut its writing half, as *net.TCPConn
// and *net.UnixConn can.
type halfCloser interface{ CloseWrite() error }

// CloseWrite shuts the writing half once the writer has sent everything
// accepted before it; see Conn.CloseWrite.
func (fc *fdConn) CloseWrite() error {
	if fc.udp != nil {
		return errUnsupported
	}
	if _, ok := fc.conn.(halfCloser); !ok {
		return errUnsupported
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
	if fc.readEnded.Load() {
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
	if fc.readEnded.Load() {
		fc.requestClose(io.EOF)
	}
	return nil
}

// noteReadEnded runs under callbackMu after an OnRead past the peer's end of
// stream. Once callbacks have consumed everything the peer sent, the reading
// half is over and the connection closes if its writing half is shut too.
func (fc *fdConn) noteReadEnded() {
	if !fc.readEOF.Load() || fc.readEnded.Load() || !fc.inbound.Empty() || len(fc.inboundTail) != 0 {
		return
	}
	fc.readEnded.Store(true)
	if fc.writeShutDone.Load() {
		fc.requestClose(io.EOF)
	}
}

// onReadEOF ends the read goroutine of a half-open connection whose peer shut
// its writing half: OnRead runs once more, so the application sees Read
// report io.EOF, and the connection closes once its own writing half is shut
// as well and callbacks consumed what the peer sent.
func (fc *fdConn) onReadEOF() {
	fc.callbackMu.Lock()
	if fc.isClosing() {
		fc.callbackMu.Unlock()
		return
	}
	fc.readEOF.Store(true)
	started := fc.beginInboundCallback()
	err := fc.events.onRead(fc)
	fc.endInboundCallback(started)
	if err == nil {
		fc.noteReadEnded()
	}
	fc.callbackMu.Unlock()
	if err != nil {
		fc.events.closeConn(fc, err)
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
	return fc.reserveInbound(n, fc.events.MaxInboundBuffered)
}

// readUDPLoop owns a connected UDP socket. A datagram is borrowed for one
// callback and always discarded before the next blocking read.
func (fc *fdConn) readUDPLoop() {
	defer fc.events.callbackWG.Done()

	holder := fc.events.readPool.Get()
	defer fc.events.readPool.Put(holder)
	buffer := holder.bytes
	for {
		n, _, err := fc.udp.sock.ReadFrom(buffer)
		if nil != err {
			// close on error.
			fc.events.closeConn(fc, err)
			return
		}

		fc.callbackMu.Lock()
		if fc.isClosing() {
			// OnClose may already have run; see readLoop.
			fc.callbackMu.Unlock()
			return
		}
		fc.inboundTail = buffer[:n]

		// trigger inbound event.
		fc.events.onSocketBytesRead(fc, n)

		// fire data callback.
		started := fc.beginInboundCallback()
		if err = fc.events.onRead(fc); nil != err {
			fc.endInboundCallback(started)
			fc.callbackMu.Unlock()
			// close on error.
			fc.events.closeConn(fc, err)
			break
		}

		// drop unread udp packet.
		_, _ = fc.Discard(-1)
		fc.endInboundCallback(started)
		fc.callbackMu.Unlock()
	}
}

// readLoop delivers n > 0 before a simultaneous terminal error, as required by
// io.Reader. callbackMu protects both the borrowed slice and retained inbound
// storage for the full callback lifetime.
func (fc *fdConn) readLoop() {
	defer fc.events.callbackWG.Done()

	holder := fc.events.readPool.Get()
	defer fc.events.readPool.Put(holder)
	buffer := holder.bytes
	for {
		n, readErr := fc.conn.Read(buffer)
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
			fc.events.onSocketBytesRead(fc, n)
			started := fc.beginInboundCallback()
			if callbackErr := fc.events.onRead(fc); callbackErr != nil {
				fc.endInboundCallback(started)
				fc.callbackMu.Unlock()
				fc.events.closeConn(fc, callbackErr)
				return
			}
			if len(fc.inboundTail) > 0 {
				_, _ = fc.inbound.Write(fc.inboundTail)
				fc.inboundTail = fc.inboundTail[:0]
				if limit := fc.events.MaxInboundBuffered; limit > 0 && fc.inbound.Len() >= limit {
					fc.readHeld = true
				}
			}
			held := fc.readHeld
			fc.endInboundCallback(started)
			fc.callbackMu.Unlock()
			if held && !fc.waitHeldRead() {
				return
			}
		}
		if readErr != nil {
			if readErr == io.EOF && fc.events.AllowHalfOpen {
				fc.onReadEOF()
				return
			}
			fc.events.closeConn(fc, readErr)
			return
		}
	}
}

// waitHeldRead keeps a held read in the socket until a Wake consumes below
// MaxInboundBuffered, and reports whether the read loop goes on. A close ends
// it, and so does the read deadline, which closes the connection as an
// expired Read would. A changed deadline also signals readResume, so the wait
// re-times against it.
func (fc *fdConn) waitHeldRead() bool {
	var timer *time.Timer
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	for {
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
			fc.events.closeConn(fc, os.ErrDeadlineExceeded)
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
		fc.callbackMu.Lock()
		held := fc.readHeld
		fc.callbackMu.Unlock()
		if !held {
			return true
		}
	}
}

func (fc *fdConn) fireWriteEvent() error {
	if nil == fc.conn {
		return nil // udp client nothing to do.
	}

	fc.events.callbackWG.Add(1)
	go fc.writeLoop()

	return nil
}

// fireReadEvent starts the connection's read goroutine, which runs OnOpen
// before its first read; see openOnReader.
func (fc *fdConn) fireReadEvent() error {
	fc.events.callbackWG.Add(1)
	go func() {
		if !fc.openOnReader() {
			fc.events.callbackWG.Done()
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

// listenUDP owns the shared packet socket and logical peer map. Each packet is
// borrowed for one callback and discarded before the receive buffer is reused.
func (fc *fdConn) listenUDP() error {
	// UDP peer callbacks all run on this listener goroutine.
	defer fc.events.callbackWG.Done()

	holder := fc.events.readPool.Get()
	defer fc.events.readPool.Put(holder)
	buffer := holder.bytes

	for {
		n, addr, err := fc.udp.sock.ReadFrom(buffer)
		if nil != err {
			_ = fc.CloseWith(err)
			return err
		}

		// remote address, in the one form every side of a peer agrees on.
		rAddr := peerKey(remoteAddrFrom(addr))

		// udp server
		fc.mux.Lock()
		udpConn, ok := fc.udp.peers[rAddr]
		// A closed server leaves peers nil; a lookup on the nil map misses,
		// and inserting would panic. The datagram is dropped instead.
		if !ok && fc.udp.peers != nil {
			udpConn = &fdConn{}
			udpConn.udp = &stdUDPState{sock: fc.udp.sock, svr: fc}
			udpConn.addr = fc.addr
			udpConn.setRemoteAddr(addr)
			udpConn.loop = fc.loop
			udpConn.events = fc.events
			udpConn.SetUserdata(fc.Userdata()) // the listener's
			udpConn.retainLifecycle()

			fc.udp.peers[rAddr] = udpConn
		}
		fc.mux.Unlock()
		if udpConn == nil {
			continue
		}
		udpConn.callbackMu.Lock()
		// The open a new peer owes — or the one DialPeer still owes, when the
		// peer's datagram arrived before its opener — runs here, once.
		if !udpConn.opened {
			if !ok && testHookUDPChildCreated != nil {
				testHookUDPChildCreated(udpConn)
			}
			udpConn.ensureOpenLocked()
		}
		if udpConn.isClosing() {
			// The peer closed while this packet waited for its callback
			// mutex, and OnClose may already have run.
			udpConn.callbackMu.Unlock()
			continue
		}
		udpConn.inboundTail = buffer[:n]

		// trigger inbound event
		fc.events.onSocketBytesRead(udpConn, n)

		// fire udp on-data event.
		started := udpConn.beginInboundCallback()
		err = fc.events.onRead(udpConn)

		// drop unread udp packet.
		_, _ = udpConn.Discard(-1)
		udpConn.endInboundCallback(started)
		udpConn.callbackMu.Unlock()

		if nil != err {
			// close udp connection
			_ = udpConn.CloseWith(err)
		}
	}
}
