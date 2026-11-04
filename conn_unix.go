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
	"sync"
	"sync/atomic"
	"syscall"

	"github.com/urpc/uio/internal/bytebuf"
	"github.com/urpc/uio/internal/poller"
	"github.com/urpc/uio/internal/socket"
)

// fdConn belongs to its turn. At most one connection turn at a time reads the
// socket and runs callbacks, and that turn also releases the connection once
// it is closing; an event loop only collects readiness, accepts and registers
// connections, and hands them over. submitMu admits
// concurrent producers into the connection-owned outbound queue, and holding
// it keeps the descriptor open for a syscall made outside the turn. Sending is
// owned separately by the write claim in writeState: the turn sends its own
// replies inline when the claim is free, and output from other goroutines is
// sent by a write turn that runs beside the reading one.
//
// The declaration order packs the state exactly into the 256-byte size class
// whose objects are cache-line aligned. The first line holds what a poller
// reads and writes for every event it dispatches — the descriptor, its
// registration tag, the read flags, the close phase and the scheduling word,
// then commonConn's loop, entry, userdata and addresses — so dispatching an
// event touches one line, and the turn's own state, written on every turn by
// another CPU, stays off it. Do not add or reorder a field without checking
// TestUnixConnectionColdStateIsLazy.
const (
	// readStalledFlag marks ET read work owed after a yield or a read round
	// that ran out of budget.
	readStalledFlag uint32 = 1 << iota
	// readHeldFlag marks reads stopped because callbacks left
	// MaxInboundBuffered bytes unconsumed. Only the connection's turn sets or
	// clears it; see holdsRead and resumeHeldRead.
	readHeldFlag
	// readEOFFlag marks a peer that has shut its writing half on a connection
	// that allows half-open: nothing more is read, Read reports io.EOF once
	// the buffer is drained, and the connection closes once its own writing
	// half is shut too. Only the connection's turn sets it.
	readEOFFlag
	// controlAliasFlag records an external Control alias. Teardown explicitly
	// removes this socket's poller registration before closing its original
	// descriptor, since an alias can keep the socket alive beyond that close.
	controlAliasFlag
	// offTurnCloseFlag marks an accepted descriptor whose close cannot block,
	// so it may close on the loop's closer (see eventLoop.deferClose). The
	// accept sets it unless the listener ran a SocketControl; SetLinger with a
	// linger time and Control clear it, as a lingering close would hold up
	// every other descriptor the closer has queued.
	offTurnCloseFlag
)

type fdConn struct {
	inputReadControlAccess
	fd int32
	// taskState coalesces readiness and synthetic events in its low bits and
	// carries, in its top bit, the claim that exactly one task is queued or
	// running: a producer sets both with one atomic and only the first setter
	// submits, and the running turn keeps the bit while it takes the events.
	taskState atomic.Uint32
	pollTag   atomic.Uint32 // registration generation echoed by the poller
	flags     atomic.Uint32 // readStalledFlag, readHeldFlag; see below
	close     connCloseState
	commonConn

	ioOwner atomic.Int64 // current task goroutine, zero while idle
	pending atomic.Int64 // accepted payload not yet written

	// submitMu orders cross-goroutine submissions and protects outbound while a
	// worker round and an external producer overlap.
	submitMu sync.Mutex
	outbound bytebuf.CompositeBuffer // protected by submitMu
	udp      *unixUDPState           // nil for stream connections

	// inflight holds a multi-block batch a flush detached from outbound, so
	// its sends and the release of its sent blocks run without submitMu.
	// Every byte in it was accepted before anything still in outbound. It
	// belongs to the holder of the write claim; it is borrowed from a pool
	// only while bytes remain unsent.
	inflight *bytebuf.CompositeBuffer

	deadlines *deadlineState // allocated by the first nonzero deadline
	// wake lazily holds timer, graceful-drain and input-control state, so
	// connections using none of them retain the compact hot allocation.
	wake atomic.Pointer[wakeState]

	// scratch is the read round's reply space while the turn reads: what the
	// round's callbacks reserve is laid out in it and leaves in one send when
	// the round ends; see ReserveOutbound. It belongs to the turn.
	scratch *readBuffer

	writeState atomic.Uint32 // send state and the write claim; see writeOwnerFlag
	turn       uint8         // turn-owned bits: turnCorked, turnFlushing, sticky turnHangup
	internal   bool          // framework-owned endpoint, not a user connection
}

const (
	// writeBlockedFlag means the last write reached EAGAIN. Only a new writable
	// edge may clear it; eager retries would spin a worker on a full socket.
	writeBlockedFlag uint32 = 1 << iota
	// writeFailedFlag suppresses the close-time flush after a fatal syscall.
	writeFailedFlag
	// writeOwnerFlag is the write claim: its holder alone writes the stream
	// socket, discards sent bytes and touches inflight. It is taken with
	// compare-and-swap and never waited for. Whoever releases it checks for
	// output that arrived meanwhile, so a producer that found it taken can
	// leave its bytes to the holder.
	writeOwnerFlag
	// writeOpenedFlag is set once OnOpen has returned. Before that, output
	// waits for the open turn, which sends it after the callback.
	writeOpenedFlag
	// writeTurnHeldFlag marks a claim the connection's turn keeps until it
	// ends, because ReserveOutbound handed out outbound bytes that its
	// callback fills after the reservation returns.
	writeTurnHeldFlag
	// writeTurnFlushFlag marks a claim the connection's turn took for one
	// flush, so the turn's cleanup releases it if OnBytesWritten panics there.
	writeTurnFlushFlag
	// writeCloseWaitFlag asks the claim holder to hand a deferred close back to
	// the connection's turn when it releases the claim.
	writeCloseWaitFlag
	// writeEdgeFlag records a writable edge the connection's turn handled
	// while the current claim was held. Its holder may have got EAGAIN but not
	// yet recorded writeBlockedFlag when the edge arrived, and that edge will
	// not come again, so the holder consumes this flag and retries instead of
	// waiting for it. Taking the claim clears it: an earlier edge says nothing
	// about sends the new holder has yet to make.
	writeEdgeFlag
	// writeContendedFlag records that a goroutine other than the one the
	// claim was taken for queued output while it was held: several producers
	// write to this connection now, so a write turn lets them add to its
	// batch before sending. Taking the claim clears it, so a lone producer,
	// however many writes it makes, is never kept waiting.
	writeContendedFlag
	// writeShutFlag records a CloseWrite: no more output is accepted, and the
	// connection's turn shuts the socket's writing half once everything
	// accepted before it has been sent. It is set under submitMu, so a
	// producer either queued its bytes first or finds it set.
	writeShutFlag
	// writeShutDoneFlag records that the writing half is shut.
	writeShutDoneFlag
)

// Bits in fdConn.turn, read and written only by the running turn. The cork
// and flush bits live for one turn; the hangup bit, once set, is kept for
// the connection's life.
const (
	// turnCorked batches a read round's replies until the round ends.
	turnCorked uint8 = 1 << iota
	// turnFlushing marks the turn inside its own flush, where OnBytesWritten
	// runs: a Write the callback makes is queued rather than sent, and a
	// flush it starts returns at once, so OnBytesWritten never runs inside itself.
	turnFlushing
	// turnHangup records a hangup the poller reported, for the rest of the
	// connection's life: the end of stream or error behind the bytes still
	// unread raises no further edge, and the rounds that read those bytes,
	// redelivered ones included, must read on until the socket reports it.
	turnHangup
	// turnReadToEnd makes rounds read on like turnHangup does, for a hangup
	// that may not be this connection's, until a read finds the socket empty
	// rather than at its end.
	turnReadToEnd
	// turnReadPastYield lets the turn's first socket read go ahead although
	// the consumer owes itself a continuation: its continuation moved no
	// input, so it waits for bytes, and only a read brings them.
	turnReadPastYield
	// turnReadDrained records a read that reached what the socket holds —
	// would-block, end of stream or an error — so a hangup this turn needs
	// no separate look at the socket error.
	turnReadDrained
)

// writeClaimerMask holds the low bits of the goroutine id a write turn's claim
// was taken for, which tells its own further writes from other producers'.
const (
	writeClaimerShift        = 16
	writeClaimerMask  uint32 = 0xffff << writeClaimerShift
)

const (
	// Close is split so a close can be requested from anywhere while the
	// connection's own turn releases it and delivers OnClose. A graceful
	// close passes through closeDraining first: accepted output leaves the
	// socket before the release, bounded by LingerTimeout.
	closeOpen uint32 = iota
	closeDraining
	closeRequested
	closeResourcesReleased
	closeCallbackDelivered
)

var (
	closeWithoutCause = &closeRequest{}
	closeAtEOF        = &closeRequest{cause: io.EOF}
	closeAtReset      = &closeRequest{cause: syscall.ECONNRESET}
)

// maxWriteFlushes bounds one write turn, like the read budget bounds a read
// round, so a connection that keeps producing returns its worker to the queue.
const maxWriteFlushes = 8

// openOutputAfterOnOpen runs OnOpen, then lets output from other goroutines
// start write turns; what they queued before leaves with this turn's flush.
// It does so even when OnOpen panics and an Executor recovers the panic,
// or that output would wait for an unrelated turn.
func (conn *fdConn) openOutputAfterOnOpen() {
	defer conn.writeState.Or(writeOpenedFlag)
	conn.fireOnOpen()
}

// readStalled is one bit in its own flags word rather than a padded atomic
// field, so it costs four bytes in the turn's scalar block.

func (conn *fdConn) readStalled() bool { return conn.flags.Load()&readStalledFlag != 0 }

func (conn *fdConn) setReadStalled(v bool) {
	if v {
		conn.flags.Or(readStalledFlag)
		return
	}
	conn.flags.And(^readStalledFlag)
}

// clearReadStalled reports whether the owed read was still owed; exactly one
// caller clears it. Every writer of the bit is the connection's owning
// goroutine — the read round that owes the read, YieldRead inside a callback,
// and this clear in the turn's epilogue — so one atomic read-modify-write
// settles it: the And returns the word the clear replaced, and the bit it
// reports was set by this same goroutine's earlier round.
func (conn *fdConn) clearReadStalled() bool {
	return conn.flags.And(^readStalledFlag)&readStalledFlag != 0
}

// readHeld reports reads stopped by MaxInboundBuffered. The poller reads it
// to drop read edges; only the connection's turn changes it.
func (conn *fdConn) readHeld() bool { return conn.flags.Load()&readHeldFlag != 0 }

// holdsRead stops reads once callbacks have left MaxInboundBuffered bytes
// unconsumed, so TCP flow control slows the peer instead of the buffer
// growing. It runs only after a callback left bytes behind.
func (conn *fdConn) holdsRead() bool {
	if limit := conn.inputLimit(conn.readHeld()); limit > 0 && conn.inbound.Len() >= limit {
		conn.flags.Or(readHeldFlag)
		return true
	}
	return false
}

// resumeHeldRead reads again once a callback has consumed the inbound buffer
// below MaxInboundBuffered. Edges that arrived while reads were held were
// dropped, so the read is scheduled here rather than left to the next edge.
func (conn *fdConn) resumeHeldRead() {
	if conn.inbound.Len() >= conn.inputLimit(true) || conn.isClosing() {
		return
	}
	conn.flags.And(^readHeldFlag)
	conn.taskState.Or(ioEventRead)
}

func (conn *fdConn) Fd() int { return int(conn.fd) }

// readEOF reports a peer that has shut its writing half, on a connection
// that allows half-open.
func (conn *fdConn) readEOF() bool { return conn.flags.Load()&readEOFFlag != 0 }

// readEnded reports a reading half that is over for the application as well:
// the peer shut its writing half and callbacks consumed everything it sent,
// so Read reports io.EOF. Bytes a callback leaves behind keep it open.
func (conn *fdConn) readEnded() bool {
	return conn.readEOF() && conn.inbound.Empty() && len(conn.inboundTail) == 0
}

// State reports where the connection's streams stand; see ConnState.
func (conn *fdConn) State() ConnState {
	var state ConnState
	if conn.readEnded() {
		state |= StatePeerEnded
	}
	if conn.writeState.Load()&writeShutFlag != 0 {
		state |= StateWriteShut
	}
	phase := conn.close.phase.Load()
	if phase >= closeDraining {
		state |= StateClosing
	}
	if phase >= closeResourcesReleased {
		state |= StateClosed
	}
	return state
}

// outputClosed reports a connection that accepts no more output: closing, or
// with its writing half shut by CloseWrite.
func (conn *fdConn) outputClosed() bool {
	// One load: anything past open — draining, or closing — refuses writes.
	return conn.close.phase.Load() != closeOpen || conn.writeState.Load()&writeShutFlag != 0
}

// Read is commonConn.Read that reports io.EOF once a half-open connection's
// peer has shut its writing half and the buffered bytes are consumed.
func (conn *fdConn) Read(b []byte) (int, error) {
	n, err := conn.commonConn.Read(b)
	if n == 0 && err == nil && conn.readEnded() {
		return 0, io.EOF
	}
	return n, err
}

// onReadEOF handles a peer that shut its writing half. A plain connection
// without half-open ends at once when its consumer has nothing left to see:
// nothing retained, no pause, no continuation owed. Otherwise reads stop, the
// end is recorded and OnRead runs once more, so a consumer sees what it
// retained and a half-open one sees Read report io.EOF. The connection then
// closes when the consumer no longer holds the end back
// (closeDeferredReadEOF), when both halves of a half-open one ended
// (closeIfEnded), or — layered — when the stack handed the end up to its
// Handler (stack.ended).
func (conn *fdConn) onReadEOF() error {
	if !conn.events().AllowHalfOpen && !conn.entry.layered && !conn.readStalled() &&
		!conn.currentReadControl().Held() && conn.inbound.Empty() && len(conn.inboundTail) == 0 {
		return io.EOF
	}
	conn.flags.Or(readEOFFlag)
	conn.inboundTail = nil
	if err := conn.fireOnRead(); err != nil {
		return err
	}
	conn.closeDeferredReadEOF()
	conn.closeIfEnded()
	return nil
}

// probeAfterEOF checks a half-open connection whose peer has shut its writing
// half when a hangup arrives: a reset raises the hangup, and nothing else
// would read the socket to report it. Linux records that reset only as the
// socket error, since a receive past the end of the stream keeps returning
// zero; the BSDs report it on the receive.
func (conn *fdConn) probeAfterEOF() error {
	if err := conn.pendingSocketError(); err != nil {
		return err
	}
	var b [1]byte
	n, err := socket.Recv(int(conn.fd), b[:])
	if err != nil && !isWouldBlock(err) {
		return err
	}
	if n > 0 {
		return errors.New("uio: data after end of stream")
	}
	return nil
}

// pendingSocketError observes a fatal transport failure without reading any
// payload. A zero result also covers an orderly FIN, whose queued bytes and
// EOF must remain subject to the input consumer's pause.
func (conn *fdConn) pendingSocketError() error {
	errno, err := syscall.GetsockoptInt(int(conn.fd), syscall.SOL_SOCKET, syscall.SO_ERROR)
	if err != nil {
		return err
	}
	if errno != 0 {
		return syscall.Errno(errno)
	}
	return nil
}

// RemoteAddr builds the net.Addr on demand: the connection keeps only the
// 32-byte value, so an accepted connection allocates nothing for its peer
// address. Datagram connections report it as a UDP address, streams as TCP.
func (conn *fdConn) RemoteAddr() net.Addr {
	if conn.remoteAddr.IsValid() {
		if conn.isDatagram() {
			return net.UDPAddrFromAddrPort(conn.remoteAddr)
		}
		return net.TCPAddrFromAddrPort(conn.remoteAddr)
	}
	// Non-IP peers (Unix sockets) keep their address object in the pair.
	if conn.addr != nil {
		return conn.addr.remote
	}
	return nil
}

// initialInterest is what a stream registers for good: read and write
// interest armed together, once. Under edge triggering an always-armed write
// interest reports a socket only when it goes from full back to writable,
// which is the only write event a refused send waits for, so no connection
// turn ever changes the registration and nothing in the I/O path issues
// epoll_ctl.
func (conn *fdConn) initialInterest() poller.Interest {
	if conn.isDatagram() {
		return poller.Readable
	}
	return poller.Readable | poller.Writable
}

// currentInterest is the interest the connection registered with; nothing
// changes it after Add, so it is what initialInterest returns.
func (conn *fdConn) currentInterest() poller.Interest { return conn.initialInterest() }

func (conn *fdConn) isClosing() bool { return conn.close.isClosing() }

func (conn *fdConn) isReleased() bool { return conn.close.isReleased() }

func (conn *fdConn) beginShutdown() { conn.close.request() }

func (conn *fdConn) isDatagram() bool { return conn.udp != nil }

func (conn *fdConn) afterRegister() {}

// skipsEdge reports a readiness event its connection's turn would have
// nothing to do with, so the poller drops it instead of scheduling a turn.
//   - A read left owed by a yield or a spent budget is redelivered on its own,
//     and a read held by MaxInboundBuffered is resumed by the callback that
//     consumes below it, so read edges until then carry nothing new. Writable
//     and hangup edges still pass: queued output waits for the one, and a
//     hangup must reach the turn to mark the rounds that read on to the end of
//     the stream.
//   - A write-only edge with nothing to send: registration arms every stream
//     with one, and kqueue raises one each time an acknowledgement frees send
//     space. Output is sent without an edge; only a write-blocked socket, or
//     queued output whose sender may be waiting on an edge, needs it. A sender
//     holding the write claim may be between a refused send and publishing its
//     bytes and writeBlocked; that edge is the only thing that will ever move
//     them, so it passes while the claim is held too.
func (conn *fdConn) skipsEdge(events poller.Events) bool {
	if events&(poller.WriteEvents|poller.HangupEvents) == 0 {
		return conn.flags.Load()&(readStalledFlag|readHeldFlag|readEOFFlag) != 0 || conn.ReadsPaused()
	}
	return events == poller.WriteEvents && conn.outboundEmpty() &&
		conn.writeState.Load()&(writeOwnerFlag|writeBlockedFlag) == 0
}

// Direct I/O belongs to this connection's active turn. A UDP child's turn is
// its server's: the server's turns read every child's datagrams and run their
// callbacks.
func (conn *fdConn) directOwner() bool {
	// Most external writes arrive while no connection turn is running. Avoid
	// the relatively expensive goroutine-id lookup in that case; an active
	// owner is the only state that can make this goroutine's write direct.
	connectionOwner := conn.ioOwner.Load()
	serverOwner := int64(0)
	if conn.udp != nil && conn.udp.server != nil {
		serverOwner = conn.udp.server.ioOwner.Load()
	}
	if connectionOwner == 0 && serverOwner == 0 {
		return false
	}
	owner := currentGoroutineID()
	return connectionOwner == owner || serverOwner == owner
}

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

// testHookScratchSend runs on the connection's turn once a read round has
// decided to send its reply space straight to the socket. Tests use it to queue
// another goroutine's write during that send; it is nil outside tests.
var testHookScratchSend func(*fdConn)

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
	settle, ended := false, false
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
		if conn.scratch != nil {
			// A panic left the round's reservations in its reply space, with
			// bytes the panicking callback never filled: dropped, not sent,
			// and the read buffer returns to the pool.
			conn.dropScratch()
		}
		if conn.turnOwnsClaim() {
			// The round's reservations are filled and flushed, or a callback
			// panic left the claim taken for a reservation or a flush: other
			// senders may take the socket again, starting with output that
			// waited for them.
			conn.releaseWrite()
			if conn.pending.Load() != 0 {
				conn.kickWriter()
			}
		} else if !ended && conn.pending.Load() != 0 {
			// After a panic in OnOpen, output queued by other goroutines in
			// the meantime has no sender until someone kicks it.
			conn.kickWriter()
		}
		if settle && conn.readStalled() {
			conn.settleInterest()
		}
		if conn.close.isDraining() && conn.pending.Load() == 0 {
			// The turn drained a graceful close's queue itself.
			conn.escalateDrain()
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
	settle = true
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
		// A continuation — one YieldRead asked for, or a spent read budget —
		// or a released pause: what the consumer retained goes to it before
		// the socket is read again, so the end of the stream cannot overtake
		// it. A consumer that moved its input on and yielded again gets its
		// next continuation before any read. One that moved nothing waits
		// for bytes: this turn reads them, and it is owed the continuation
		// still.
		before := conn.inputMark()
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
		switch {
		case conn.ReadsPaused():
			// The release asks for the next resume turn.
		case !conn.readStalled():
			events |= ioEventRead
		case conn.inputMark() == before:
			if conn.readEOF() {
				// The stream ended and this continuation moved nothing: no
				// byte can arrive to complete the unit its consumer waits
				// for. It had its chance; its unconsumed input goes as a
				// close's does.
				conn.requestClose(io.EOF)
			} else {
				conn.turn |= turnReadPastYield
				events |= ioEventRead
			}
		default:
			events &^= ioEventRead
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
			err = conn.onRead() // a pause or a yield owed stops the round before its first read
		} else if flags&readEOFFlag != 0 && events&ioEventHangup != 0 {
			conn.turn |= turnReadDrained
			err = conn.probeAfterEOF()
		}
		if err != nil {
			conn.requestClose(err)
		}
	}
	if reportedHangup && conn.turn&turnReadDrained == 0 && !conn.isDatagram() && !conn.isClosing() {
		// No read reached the hangup: a pause, a hold or a yield left the
		// socket unread. A reset must still end the connection, which only
		// the socket error tells without consuming input; zero leaves an
		// orderly end of stream to the reads that come for it. It belongs
		// to this live descriptor even if untagged open stripped a stale
		// hangup edge.
		if err := conn.pendingSocketError(); err != nil {
			conn.requestClose(err)
		}
	}
	if events&ioEventWake != 0 && !conn.isClosing() && !conn.readStalled() {
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

// settleInterest hands read work the turn left owed — a yield or a spent read
// budget — to the next turn. Outbound backlog never pauses reads, so there is
// nothing else to wait for.
func (conn *fdConn) settleInterest() {
	if !conn.readStalled() || conn.isClosing() {
		return
	}
	if conn.clearReadStalled() {
		if conn.isDatagram() {
			// A datagram leaves nothing unread behind its callback.
			conn.taskState.Or(ioEventRead)
			return
		}
		if conn.ReadsPaused() {
			return // the release asks for the resume turn, which continues it
		}
		// The continuation runs even when the socket's buffer is empty: a
		// protocol may hold decoder work of its own behind the yield.
		conn.taskState.Or(ioEventResume)
	}
}

// RunTask implements IOTask for injected executors. The executor contract
// guarantees asynchronous dispatch; the loop's turn count covers the turn.
func (conn *fdConn) RunTask() {
	if conn.loop == nil || conn.loop.ioPool == nil {
		return
	}
	conn.runIOTask()
}

// testHookSocketClosePublished, when set, runs as a socket's close has been
// published and before it starts: tests hold it there.
var testHookSocketClosePublished atomic.Pointer[func(*fdConn)]

func (conn *fdConn) fireOnOpen() {
	if callback := conn.callbacks().onOpen; callback != nil {
		started := conn.beginInboundCallback()
		callback(conn)
		conn.endInboundCallback(started)
	}
	if conn.taskState.Load()&taskScheduledBit == 0 {
		if err := conn.finishCallback(); err != nil {
			conn.requestClose(err)
		}
	}
}

// fireOnRead establishes the inbound-access scope around application code.
// Unread stream bytes remain available to a later callback; unread UDP bytes
// are discarded by the datagram caller because packet lifetime is one turn.
func (conn *fdConn) fireOnRead() error {
	var err error
	started := conn.beginInboundCallback()
	var skipRead bool
	skipRead, err = conn.fireWakeTimer()
	if err == nil && !skipRead && conn.consumerMayRun() {
		if callback := conn.callbacks().onRead; callback != nil {
			err = callback(conn)
		} else {
			_, _ = conn.Discard(-1)
		}
	}
	conn.endInboundCallback(started)
	if err != nil {
		return err
	}
	if conn.isClosing() {
		return nil
	}
	if conn.isDatagram() {
		return conn.finishCallback()
	}
	return nil
}

func (conn *fdConn) finishCallback() error {
	if conn.isClosing() {
		return nil
	}
	// The threshold batches within this callback; EAGAIN tails remain poller-driven.
	_, err := conn.flushOnLoop()
	return err
}

func (conn *fdConn) fireReadEvent() error {
	if conn.isDatagram() {
		return conn.onRecvUDP()
	}
	return conn.onRead()
}

func (conn *fdConn) fireWriteEvent() error {
	if conn.isDatagram() {
		return nil
	}
	conn.noteWritable()
	_, err := conn.flushOnLoop()
	return err
}

// ReserveInbound implements Conn.ReserveInbound.
func (conn *fdConn) ReserveInbound(n int) error {
	if conn.isDatagram() || n <= 0 {
		return nil
	}
	return conn.reserveInbound(n, conn.events().MaxInboundBuffered)
}

// onRead drains an edge-triggered stream with bounded work per task. The
// borrowed read buffer is exposed directly during OnRead; only a callback that
// leaves bytes unread pays the copy into the persistent inbound buffer.
func (conn *fdConn) onRead() error {
	holder := conn.events().readPool.Get()
	conn.scratch = holder
	// A read round is corked: its replies accumulate in outbound and are
	// flushed once when the event ends, or at the mid-round flush once they
	// pass midFlushThreshold, so a burst of reads costs one writev instead of
	// one syscall per reply.
	conn.turn |= turnCorked
	err := conn.readRound(holder.bytes)
	conn.turn &^= turnCorked
	conn.scratch = nil
	// Unbind the borrowed tail before its holder returns to the pool: an
	// error exit leaves it aliasing the holder, and the close it asks for can
	// wait out a write in flight, long enough for another round to Get the
	// buffer back and overwrite it.
	conn.inboundTail = nil
	if out := holder.out; out != nil && out.Len() > 0 {
		if sendErr := conn.sendScratch(holder); err == nil {
			err = sendErr
		}
	}
	conn.events().readPool.Put(holder)
	return err
}

// readRound is onRead's body. It has many exits, which would keep deferred
// cleanup from being open-coded, so onRead does the cleanup around it.
func (conn *fdConn) readRound(buffer []byte) error {
	// A pause stops reading the socket; so does a continuation the consumer
	// owes itself, unless that continuation moved nothing and waits for
	// bytes. Within the round the callbacks' own pauses and yields are
	// checked as they return.
	if conn.ReadsPaused() || conn.readStalled() && conn.turn&turnReadPastYield == 0 {
		return nil
	}
	totalRead := 0
	for calls := 0; calls < 256 && totalRead < 1<<20; calls++ {
		n, err := socket.Recv(int(conn.fd), buffer)
		if err != nil {
			if isWouldBlock(err) {
				conn.turn = conn.turn&^turnReadToEnd | turnReadDrained
				return nil
			}
			conn.turn |= turnReadDrained
			return err
		}
		if n == 0 {
			conn.turn |= turnReadDrained
			return conn.onReadEOF()
		}
		totalRead += n
		// inboundTail aliases the pooled read buffer. Unconsumed bytes are copied
		// into inbound before the buffer is reused by the next syscall.
		conn.inboundTail = buffer[:n]
		if conn.callbacks().onBytesRead != nil {
			conn.events().onSocketBytesRead(conn, n)
		}
		if conn.isClosing() {
			conn.inboundTail = nil
			return nil
		}
		if err = conn.fireOnRead(); err != nil {
			return err
		}
		if conn.isClosing() {
			conn.inboundTail = nil
			return nil
		}
		if len(conn.inboundTail) > 0 {
			_, _ = conn.inbound.Write(conn.inboundTail)
			conn.inboundTail = nil
			if conn.holdsRead() {
				return nil
			}
		}
		if conn.readStalled() || conn.ReadsPaused() {
			return nil
		}
		if n < len(buffer) && conn.turn&(turnHangup|turnReadToEnd) == 0 {
			// The socket is drained, and bytes that arrive later raise a new
			// edge. A hangup already queued behind these bytes raises none,
			// which is why the round reads on once the poller reported one.
			return nil
		}
		// The round reads on. Once its replies fill four read buffers, send
		// them before reading further: a peer that keeps a window of requests
		// in flight is otherwise left waiting for the whole round, which can
		// run to 256 reads, and its pipeline empties. Smaller bursts still
		// leave in one write when the round ends. The threshold is four read
		// buffers rather than the coalescing block size: a mid-round send pays
		// a syscall and a writev of two blocks, and at one block of replies it
		// cost a pipeline 4-6% against waiting for the round's end.
		if conn.pending.Load() >= int64(conn.midFlushThreshold()) {
			if _, err := conn.flushOnLoop(); err != nil {
				return err
			}
		}
	}
	if totalRead > 0 {
		conn.setReadStalled(true)
	}
	return nil
}

// udpReadBatch is the most datagrams one read round asks the kernel for; the
// round then runs one turn per datagram, in arrival order.
const udpReadBatch = 32

// grow doubles the batch after a full round, up to udpReadBatch slots.
func (batch *udpBatch) grow(slotSize int) {
	count := len(batch.slots) * 2
	if count > udpReadBatch {
		return
	}
	*batch = *newUDPBatch(slotSize, count)
}

func isWouldBlock(err error) bool {
	return err == syscall.EAGAIN || err == syscall.EWOULDBLOCK
}

func (conn *fdConn) Wake() error {
	if conn.isClosing() || conn.loop == nil {
		return net.ErrClosed
	}
	if conn.udp != nil && conn.udp.server != nil {
		conn.udp.server.queuePeer(conn, true)
		return nil
	}
	conn.scheduleIO(ioEventWake)
	return nil
}

func (conn *fdConn) YieldRead() error {
	if conn.isClosing() {
		return net.ErrClosed
	}
	if !conn.directOwner() {
		return ErrUnsupported
	}
	conn.setReadStalled(true)
	return nil
}
