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
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

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
// whose objects are cache-line aligned: the descriptor and the turn's byte
// fields share one word, the four 32-bit words close the struct, and
// everything wider sits between them with no padding. Do not add or reorder a
// field without checking TestUnixConnectionColdStateIsLazy.
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
)

type fdConn struct {
	commonConn
	fd       int32
	turn     uint8 // turn-owned bits: turnCorked, turnFlushing, sticky turnHangup
	internal bool  // framework-owned endpoint, not a user connection

	ioOwner atomic.Int64 // current task goroutine, zero while idle
	pending atomic.Int64 // accepted payload not yet written

	// submitMu orders cross-goroutine submissions and protects outbound while a
	// worker round and an external producer overlap.
	submitMu sync.Mutex
	close    connCloseState
	outbound bytebuf.CompositeBuffer // protected by submitMu
	udp      *unixUDPState           // nil for stream connections

	// inflight holds a multi-block batch a flush detached from outbound, so
	// its sends and the release of its sent blocks run without submitMu.
	// Every byte in it was accepted before anything still in outbound. It
	// belongs to the holder of the write claim; it is borrowed from a pool
	// only while bytes remain unsent.
	inflight *bytebuf.CompositeBuffer

	deadlines *deadlineState // allocated by the first nonzero deadline
	// wake is the connection's wake timer on its loop's wheel, allocated by
	// the first SetWakeDeadline or SetWakeInterval.
	wake atomic.Pointer[wakeState]

	// scratch is the read round's reply space while the turn reads: what the
	// round's callbacks reserve is laid out in it and leaves in one send when
	// the round ends; see ReserveOutbound. It belongs to the turn.
	scratch *readBuffer

	// taskState coalesces readiness and synthetic events in its low bits and
	// carries, in its top bit, the claim that exactly one task is queued or
	// running: a producer sets both with one atomic and only the first setter
	// submits, and the running turn keeps the bit while it takes the events.
	taskState  atomic.Uint32
	writeState atomic.Uint32 // send state and the write claim; see writeOwnerFlag
	pollTag    atomic.Uint32 // registration generation echoed by the poller
	flags      atomic.Uint32 // readStalledFlag, readHeldFlag; see below
}

// unixUDPState contains fields that TCP connections never use. A UDP
// listener is a connection of its own, the server, and each peer it hears
// from is a child connection sharing its socket. The server's turns read
// every child's datagrams and run every child's callbacks, so peers belongs
// to them; a child closed or woken from elsewhere is queued for the server's
// next turn.
type unixUDPState struct {
	file   *os.File // owns a duplicated UDP listener fd when non-nil
	remote syscall.Sockaddr
	server *fdConn
	peers  map[socket.UDPAddress]*fdConn
	key    socket.UDPAddress

	family uint8 // server only: the socket's address family, 4 or 6

	// peerMu guards a server's peers, which its turns read for every
	// datagram and DialPeer adds to from any goroutine, and its queues.
	peerMu  sync.Mutex
	closing []*fdConn // children whose close waits for the server's turn
	waking  []*fdConn // children whose Wake waits for the server's turn
	opening []*fdConn // children DialPeer made, whose OnOpen is owed

	// openOwed marks a child DialPeer made whose OnOpen has not run: the
	// server's turn, a datagram from the peer, or its close runs it first,
	// whichever claims it.
	openOwed atomic.Bool

	// readBuffer belongs to the socket's turns, which read one datagram at a
	// time and lend it only for its callbacks. A child never reads.
	readBuffer []byte

	// Datagrams are sent on whichever goroutine writes them, and the bytes
	// they report wait in outboundPending for whoever holds outboundClaim; see
	// reportOutbound.
	outboundPending atomic.Int64
	outboundClaim   atomic.Bool

	// A server's socket closes on whichever goroutine gets there first —
	// Listener.Close's caller, so the address is free when it returns, or the
	// release in the server's turn — so every syscall on it, from any
	// goroutine, holds the gate; see releaseSocket.
	gate descriptorGate
	// closeMu and closed hold the socket close's single owner; see
	// releaseSocket.
	closeMu sync.Mutex
	closed  chan struct{}
}

// deadlineState is guarded by submitMu except for its atomic timer generations.
type deadlineState struct {
	readTimer       *time.Timer
	writeTimer      *time.Timer
	readGeneration  uint64
	writeGeneration uint64
	readTimerGen    atomic.Uint64
	writeTimerGen   atomic.Uint64
	readDeadline    time.Time
	writeDeadline   time.Time
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
)

// writeClaimerMask holds the low bits of the goroutine id a write turn's claim
// was taken for, which tells its own further writes from other producers'.
const (
	writeClaimerShift        = 16
	writeClaimerMask  uint32 = 0xffff << writeClaimerShift
)

func claimerTag() uint32 { return uint32(currentGoroutineID()) << writeClaimerShift }

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

// connCloseState separates lifecycle from event scheduling. deferred is
// protected by submitMu; a non-nil request can carry a nil close cause.
type connCloseState struct {
	phase    atomic.Uint32
	deferred *closeRequest
}

// closeRequest is never modified once recorded, so the requests for the
// causes most closes carry are shared instead of allocated per close.
type closeRequest struct{ cause error }

var (
	closeWithoutCause = &closeRequest{}
	closeAtEOF        = &closeRequest{cause: io.EOF}
	closeAtReset      = &closeRequest{cause: syscall.ECONNRESET}
)

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

func (state *connCloseState) isClosing() bool  { return state.phase.Load() >= closeRequested }
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

func (conn *fdConn) writeBlocked() bool { return conn.writeState.Load()&writeBlockedFlag != 0 }
func (conn *fdConn) writeFailed() bool  { return conn.writeState.Load()&writeFailedFlag != 0 }
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

// turnHoldsWrite reports a claim the connection's turn keeps for a reservation,
// outside its own flush. Only the turn calls it.
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
func (conn *fdConn) ShardKey() uint32    { return conn.turnKey() }
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
	conn.runWriteTurn()
}

// maxWriteFlushes bounds one write turn, like the read budget bounds a read
// round, so a connection that keeps producing returns its worker to the queue.
const maxWriteFlushes = 8

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
	if limit := conn.events.MaxInboundBuffered; limit > 0 && conn.inbound.Len() >= limit {
		conn.flags.Or(readHeldFlag)
		return true
	}
	return false
}

// resumeHeldRead reads again once a callback has consumed the inbound buffer
// below MaxInboundBuffered. Edges that arrived while reads were held were
// dropped, so the read is scheduled here rather than left to the next edge.
func (conn *fdConn) resumeHeldRead() {
	if conn.inbound.Len() >= conn.events.MaxInboundBuffered || conn.isClosing() {
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

// closeIfEnded closes a half-open connection whose two halves have both
// ended, once a callback consumed the last bytes its peer sent.
func (conn *fdConn) closeIfEnded() {
	if conn.writeState.Load()&writeShutDoneFlag != 0 && conn.readEnded() {
		conn.requestClose(io.EOF)
	}
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

// CloseWrite shuts the writing half once everything accepted before it has
// been sent; see Conn.CloseWrite.
func (conn *fdConn) CloseWrite() error {
	if conn.isDatagram() {
		return errUnsupported
	}
	if conn.loop == nil {
		return net.ErrClosed
	}
	conn.submitMu.Lock()
	if conn.isClosing() || conn.events.closing.Load() || conn.loop.stopping.Load() {
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
	if conn.readEnded() {
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

// onReadEOF handles a peer that shut its writing half. Without half-open it
// ends the connection as before. With it, reads stop, OnRead runs once more so
// the application sees Read report io.EOF, and the connection closes once its
// own writing half is shut as well and callbacks consumed what the peer sent.
func (conn *fdConn) onReadEOF() error {
	if !conn.events.AllowHalfOpen {
		return io.EOF
	}
	conn.flags.Or(readEOFFlag)
	conn.inboundTail = nil
	if err := conn.fireOnRead(); err != nil {
		return err
	}
	conn.closeIfEnded()
	return nil
}

// probeAfterEOF checks a half-open connection whose peer has shut its writing
// half when a hangup arrives: a reset raises the hangup, and nothing else
// would read the socket to report it. Linux records that reset only as the
// socket error, since a receive past the end of the stream keeps returning
// zero; the BSDs report it on the receive.
func (conn *fdConn) probeAfterEOF() error {
	if errno, err := syscall.GetsockoptInt(int(conn.fd), syscall.SOL_SOCKET, syscall.SO_ERROR); err != nil {
		return err
	} else if errno != 0 {
		return syscall.Errno(errno)
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
func (conn *fdConn) isClosing() bool                  { return conn.close.isClosing() }
func (conn *fdConn) isReleased() bool                 { return conn.close.isReleased() }
func (conn *fdConn) beginShutdown()                   { conn.close.request() }
func (conn *fdConn) isDatagram() bool                 { return conn.udp != nil }
func (conn *fdConn) afterRegister()                   {}

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
		return conn.flags.Load()&(readStalledFlag|readHeldFlag|readEOFFlag) != 0
	}
	return events == poller.WriteEvents && conn.outboundEmpty() &&
		conn.writeState.Load()&(writeOwnerFlag|writeBlockedFlag) == 0
}

// Direct I/O belongs to this connection's active turn. A UDP child's turn is
// its server's: the server's turns read every child's datagrams and run their
// callbacks.
func (conn *fdConn) directOwner() bool {
	owner := currentGoroutineID()
	return conn.ioOwner.Load() == owner || (conn.udp != nil && conn.udp.server != nil && conn.udp.server.ioOwner.Load() == owner)
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
	if conn.loop == nil || conn.events.closing.Load() {
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
	if events&ioEventRead != 0 && !conn.isClosing() {
		if events&ioEventHangup != 0 {
			conn.turn |= turnHangup
		}
		var err error
		if conn.isDatagram() {
			err = conn.onRecvUDP()
		} else if flags := conn.flags.Load(); flags&(readHeldFlag|readEOFFlag) == 0 {
			err = conn.onRead()
		} else if flags&readEOFFlag != 0 && events&ioEventHangup != 0 {
			err = conn.probeAfterEOF()
		}
		// A held read stays in the socket, a hangup behind it included, until
		// a callback consumes below the limit; see resumeHeldRead.
		if err != nil {
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
		conn.taskState.Or(ioEventRead | ioEventWake)
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

func (conn *fdConn) SetLinger(seconds int) error {
	return conn.setSocketOption(optionLinger, seconds)
}
func (conn *fdConn) SetNoDelay(noDelay bool) error {
	return conn.setSocketOption(optionNoDelay, boolInt(noDelay))
}
func (conn *fdConn) SetReadBuffer(size int) error {
	return conn.setSocketOption(optionReadBuffer, size)
}
func (conn *fdConn) SetWriteBuffer(size int) error {
	return conn.setSocketOption(optionWriteBuffer, size)
}
func (conn *fdConn) SetKeepAlive(keepAlive bool) error {
	return conn.setSocketOption(optionKeepAlive, boolInt(keepAlive))
}
func (conn *fdConn) SetKeepAlivePeriod(seconds int) error {
	return conn.setSocketOption(optionKeepAlivePeriod, seconds)
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

// setSocketOption applies an option on the caller's goroutine. The
// connection's own turn releases the descriptor, so it applies options
// directly; any other caller holds the owner's submitMu across the syscall,
// which teardown takes to mark the connection released before it closes the
// descriptor, so the option reaches this connection's socket or nothing.
func (conn *fdConn) setSocketOption(kind socketOptionKind, value int) error {
	if conn.isClosing() {
		return net.ErrClosed
	}
	if conn.directOwner() {
		return conn.applySocketOption(kind, value)
	}
	owner := conn.descriptorOwner()
	owner.submitMu.Lock()
	defer owner.submitMu.Unlock()
	return conn.applySocketOption(kind, value)
}

// Control runs fn with the connection's descriptor, for the socket options
// the dedicated setters do not carry; see Conn.Control. A UDP child's options
// land on the socket it shares with its server.
func (conn *fdConn) Control(fn func(fd uintptr)) error {
	if conn.isClosing() {
		return net.ErrClosed
	}
	if conn.directOwner() {
		return conn.applyControl(fn)
	}
	owner := conn.descriptorOwner()
	if owner.loop == nil || !owner.loop.acquireIO() {
		return net.ErrClosed
	}
	defer owner.loop.releaseIO()
	owner.submitMu.Lock()
	if conn.isClosing() || owner.isClosing() {
		owner.submitMu.Unlock()
		return net.ErrClosed
	}
	// Keep the socket alive without holding a submission lock across user
	// code: fn may close this connection or its listener. A temporary alias
	// applies the same socket options and cannot be reused under the call.
	gate, open := conn.enterDescriptor()
	if !open {
		owner.submitMu.Unlock()
		return net.ErrClosed
	}
	fd, err := socket.Dup(int(owner.fd))
	gate.leave()
	if err == nil {
		owner.flags.Or(controlAliasFlag)
	}
	owner.submitMu.Unlock()
	if err != nil {
		return err
	}
	defer syscall.Close(fd)
	fn(uintptr(fd))
	return nil
}

// applyControl runs fn on the turn that owns the descriptor. On a UDP
// server's socket the gate stays held across fn: a Listener.Close elsewhere
// waits for fn to return before closing the socket, and one from fn itself
// leaves the close to this turn.
func (conn *fdConn) applyControl(fn func(fd uintptr)) error {
	if conn.close.isReleased() || conn.descriptorOwner().close.isReleased() {
		return net.ErrClosed
	}
	gate, open := conn.enterDescriptor()
	if !open {
		return net.ErrClosed
	}
	defer gate.leave()
	fn(uintptr(conn.fd))
	return nil
}

// enterDescriptor counts a syscall on a UDP server's socket, which children
// and Listener.Close reach from any goroutine, and reports false once that
// socket is closing; see descriptorGate. Leave the returned gate when done; it
// is nil for a connection whose descriptor is its own, which closes only in
// its own turn.
func (conn *fdConn) enterDescriptor() (*descriptorGate, bool) {
	owner := conn.descriptorOwner()
	if !owner.internal || owner.udp == nil {
		return nil, true
	}
	gate := &owner.udp.gate
	return gate, gate.enter()
}

// testHookSocketClosePublished, when set, runs as a socket's close has been
// published and before it starts: tests hold it there.
var testHookSocketClosePublished atomic.Pointer[func(*fdConn)]

// releaseSocket hands the socket close to its one owner, started by whoever
// calls first, and returns a channel closed once the socket is closed: it
// stops new syscalls on the socket, then — once those in progress have ended
// — takes the descriptor out of the fd table and the poller, explicitly since
// a Control alias may keep the socket open, and closes it. A Control that
// outlasts socketCloseWait holds the close, which keeps retrying, so waiting
// on the channel is waiting on a fn that may be waiting for the close itself:
// bound the wait, and let the retry close once the fn returns.
func (server *fdConn) releaseSocket() <-chan struct{} {
	server.udp.closeMu.Lock()
	if server.udp.closed != nil {
		done := server.udp.closed
		server.udp.closeMu.Unlock()
		return done
	}
	done := make(chan struct{})
	// Counted before it is published: whoever finds the close under way —
	// the shutdown's own teardown among them — finds Serve's wait covering it
	// too, so Serve returns with the address free.
	server.events.socketCloses.add()
	server.udp.closed = done
	server.udp.closeMu.Unlock()
	if hook := testHookSocketClosePublished.Load(); hook != nil {
		(*hook)(server)
	}
	go func() {
		defer server.events.socketCloses.done()
		defer close(done)
		server.udp.gate.markClosed()
		for !server.udp.gate.waitDrain(socketCloseWait) {
			time.Sleep(socketCloseWait)
		}
		server.loop.fdMap.DeleteValue(int(server.fd), server)
		_ = server.loop.poller.Remove(int(server.fd), server.currentInterest())
		if server.udp.file != nil {
			_ = server.udp.file.Close()
		} else {
			_ = syscall.Close(int(server.fd))
		}
	}()
	return done
}

// releaseSocketNow closes a closing UDP server's socket on the caller, so the
// address is free once Listener.Close returns — unless a Control on the
// socket outlasts the wait. The server's own turn — a callback of one of its
// peers — may hold the gate in a Control, so there the release at the end of
// the turn closes it instead.
func (server *fdConn) releaseSocketNow() {
	if server.ioOwner.Load() == currentGoroutineID() {
		return
	}
	select {
	case <-server.releaseSocket():
	case <-time.After(2 * socketCloseWait):
	}
}

// descriptorOwner is the connection whose release closes this connection's
// descriptor: a UDP child shares its server's.
func (conn *fdConn) descriptorOwner() *fdConn {
	if conn.udp != nil && conn.udp.server != nil {
		return conn.udp.server
	}
	return conn
}

func (conn *fdConn) applySocketOption(kind socketOptionKind, value int) error {
	if conn.close.isReleased() || conn.descriptorOwner().close.isReleased() {
		return net.ErrClosed
	}
	gate, open := conn.enterDescriptor()
	if !open {
		return net.ErrClosed
	}
	defer gate.leave()
	switch kind {
	case optionLinger:
		return socket.SetLinger(int(conn.fd), value)
	case optionNoDelay:
		return socket.SetNoDelay(int(conn.fd), value != 0)
	case optionKeepAlive:
		return socket.SetKeepAlive(int(conn.fd), value != 0)
	case optionKeepAlivePeriod:
		return socket.SetKeepAlivePeriod(int(conn.fd), value)
	case optionReadBuffer:
		return socket.SetRecvBuffer(int(conn.fd), value)
	default:
		return socket.SetSendBuffer(int(conn.fd), value)
	}
}

func (conn *fdConn) fireOnOpen() {
	if callback := conn.events.cb.onOpen; callback != nil {
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
	if err == nil && !skipRead {
		if callback := conn.events.cb.onRead; callback != nil {
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
	return conn.reserveInbound(n, conn.events.MaxInboundBuffered)
}

// onRead drains an edge-triggered stream with bounded work per task. The
// borrowed read buffer is exposed directly during OnRead; only a callback that
// leaves bytes unread pays the copy into the persistent inbound buffer.
func (conn *fdConn) onRead() error {
	holder := conn.events.readPool.Get()
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
	conn.events.readPool.Put(holder)
	return err
}

// readRound is onRead's body. It has many exits, which would keep deferred
// cleanup from being open-coded, so onRead does the cleanup around it.
func (conn *fdConn) readRound(buffer []byte) error {
	totalRead := 0
	for calls := 0; calls < 256 && totalRead < 1<<20; calls++ {
		n, err := socket.Recv(int(conn.fd), buffer)
		if err != nil {
			if isWouldBlock(err) {
				conn.turn &^= turnReadToEnd
				return nil
			}
			return err
		}
		if n == 0 {
			return conn.onReadEOF()
		}
		totalRead += n
		// inboundTail aliases the pooled read buffer. Unconsumed bytes are copied
		// into inbound before the buffer is reused by the next syscall.
		conn.inboundTail = buffer[:n]
		if conn.events.cb.onBytesRead != nil {
			conn.events.onSocketBytesRead(conn, n)
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
		if conn.readStalled() {
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

// onRecvUDP drains the socket with bounded work per turn, and a read left
// owed when the budget runs out is redelivered to the next turn. A server's
// children share this socket and its peer map, so their callbacks run here.
func (conn *fdConn) onRecvUDP() error {
	buffer := conn.udp.readBuffer
	if buffer == nil {
		buffer = make([]byte, conn.events.readBufferSize)
		conn.udp.readBuffer = buffer
	}
	totalRead := 0
	var receive socket.UDPReceive
	for packets := 0; packets < 256 && totalRead < 1<<20; packets++ {
		gate, open := conn.enterDescriptor()
		if !open {
			return nil // The listener closed its socket; the release follows.
		}
		n, err := socket.RecvUDP(int(conn.fd), buffer, &receive)
		gate.leave()
		if err != nil {
			if isWouldBlock(err) {
				return nil
			}
			return err
		}
		totalRead += n
		conn.handleUDPPacket(buffer[:n], receive.Addr)
		if conn.isClosing() {
			return nil
		}
	}
	conn.setReadStalled(true)
	return nil
}

// handleUDPPacket resolves or creates the logical peer connection and lends
// the packet buffer only for the duration of its synchronous callbacks.
func (conn *fdConn) handleUDPPacket(packet []byte, source socket.UDPAddress) {
	packetConn := conn
	if conn.udp.peers != nil {
		// UDP children are logical connections that share the server fd.
		udp := conn.udp
		udp.peerMu.Lock()
		packetConn = udp.peers[source]
		created := false
		if packetConn == nil && udp.peers != nil {
			sockaddr := source.Sockaddr()
			if sockaddr == nil {
				udp.peerMu.Unlock()
				return
			}
			packetConn = &fdConn{fd: conn.fd, udp: &unixUDPState{remote: sockaddr, server: conn, key: source}}
			packetConn.events, packetConn.loop = conn.events, conn.loop
			packetConn.SetUserdata(conn.Userdata()) // the listener's
			packetConn.addr, packetConn.remoteAddr = conn.addr, source.NetAddr().AddrPort()
			udp.peers[source] = packetConn
			created = true
		}
		udp.peerMu.Unlock()
		if packetConn == nil {
			return
		}
		if created || packetConn.udp.openOwed.CompareAndSwap(true, false) {
			packetConn.fireOnOpen()
		}
	}
	if packetConn.isClosing() {
		return
	}
	packetConn.inboundTail = packet
	packetConn.events.onSocketBytesRead(packetConn, len(packet))
	err := packetConn.fireOnRead()
	started := packetConn.beginInboundCallback()
	_, _ = packetConn.Discard(-1)
	packetConn.endInboundCallback(started)
	packetConn.inboundTail = nil
	if err != nil {
		packetConn.requestClose(err)
	}
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

// dialPeer adds a child for addr to a UDP server; see Listener.DialPeer.
func (server *fdConn) dialPeer(addr netip.AddrPort, userdata any) (Conn, error) {
	udp := server.udp
	key, err := socket.UDPAddressFor(udp.family, addr)
	if err != nil {
		return nil, err
	}
	sockaddr := key.Sockaddr()
	udp.peerMu.Lock()
	if udp.peers == nil || server.isClosing() || server.events.closing.Load() {
		udp.peerMu.Unlock()
		return nil, net.ErrClosed
	}
	if existing := udp.peers[key]; existing != nil {
		if !existing.isClosing() {
			udp.peerMu.Unlock()
			return existing, ErrPeerExists
		}
		// Transfer ownership before replacing the address entry. The old
		// closer may still be between marking closing and queuePeer, so the
		// server must retain it even if shutdown wins before that hand-off.
		udp.closing = append(udp.closing, existing)
	}
	child := &fdConn{fd: server.fd, udp: &unixUDPState{remote: sockaddr, server: server, key: key}}
	child.events, child.loop = server.events, server.loop
	child.addr, child.remoteAddr = server.addr, key.NetAddr().AddrPort()
	child.SetUserdata(userdata)
	child.udp.openOwed.Store(true)
	// A closing child still in the map is replaced: its release leaves the
	// new one there.
	udp.peers[key] = child
	udp.opening = append(udp.opening, child)
	udp.peerMu.Unlock()
	server.scheduleIO(ioEventPeers)
	return child, nil
}

// writeToPeer sends one datagram to addr from a UDP server's socket; see
// Listener.WriteTo.
func (server *fdConn) writeToPeer(b []byte, addr netip.AddrPort) (int, error) {
	key, err := socket.UDPAddressFor(server.udp.family, addr)
	if err != nil {
		return 0, err
	}
	sockaddr := key.Sockaddr()
	server.submitMu.Lock()
	defer server.submitMu.Unlock()
	if server.isClosing() || server.close.isReleased() || server.events.closing.Load() {
		return 0, net.ErrClosed
	}
	if !server.udp.gate.enter() {
		return 0, net.ErrClosed
	}
	err = syscall.Sendto(int(server.fd), b, 0, sockaddr)
	server.udp.gate.leave()
	if err != nil {
		return 0, err
	}
	return len(b), nil
}

// queuePeer hands a child's close or Wake to the server's turn, which owns
// the children: wake selects which.
func (server *fdConn) queuePeer(child *fdConn, wake bool) {
	udp := server.udp
	udp.peerMu.Lock()
	if udp.peers == nil {
		udp.peerMu.Unlock()
		return // The release pass already owns every child and queued request.
	}
	if wake {
		udp.waking = append(udp.waking, child)
	} else {
		udp.closing = append(udp.closing, child)
	}
	udp.peerMu.Unlock()
	server.scheduleIO(ioEventPeers)
}

// settlePeers runs in a UDP server's turn and handles the children queued for
// it: a closing child is released and gets OnClose, and a woken one OnRead.
// Children queued meanwhile set ioEventPeers again for the next turn.
//
// Children are taken one at a time, and ioEventPeers stays set while more are
// queued: a callback panic that an Executor recovers ends this turn, and the
// next one takes the rest.
func (server *fdConn) settlePeers() {
	udp := server.udp
	// Process only the requests that were queued when this turn reached the
	// peer hand-off. A callback may call Wake, which queues another request for
	// the next turn; draining that request here would let a self-waking UDP
	// callback keep this turn alive forever and prevent shutdown from joining
	// it.
	udp.peerMu.Lock()
	remaining := len(udp.closing) + len(udp.waking)
	opening := udp.opening
	udp.opening = nil
	udp.peerMu.Unlock()
	for _, child := range opening {
		if child.udp.openOwed.CompareAndSwap(true, false) {
			child.fireOnOpen()
		}
	}
	for processed := 0; processed < remaining; processed++ {
		udp.peerMu.Lock()
		var child *fdConn
		wake := false
		if len(udp.closing) != 0 {
			child = udp.closing[0]
			udp.closing[0] = nil
			udp.closing = udp.closing[1:]
		} else if len(udp.waking) != 0 {
			child, wake = udp.waking[0], true
			udp.waking[0] = nil
			udp.waking = udp.waking[1:]
		}
		if child == nil {
			udp.peerMu.Unlock()
			break
		}
		udp.peerMu.Unlock()
		if !wake {
			if finalErr, ok := child.teardown(nil); ok {
				child.deliverClose(finalErr)
			}
		} else if !child.isClosing() {
			if err := child.fireOnRead(); err != nil {
				child.requestClose(err)
			}
		}
	}
	// Keep the synthetic event set when a callback queued more work while this
	// turn was running. finishIOTask will hand it to a fresh turn after the
	// current one returns.
	udp.peerMu.Lock()
	if len(udp.closing)+len(udp.waking)+len(udp.opening) == 0 {
		server.taskState.And(^ioEventPeers)
	} else {
		server.taskState.Or(ioEventPeers)
	}
	udp.peerMu.Unlock()
}

func (conn *fdConn) YieldRead() error {
	if conn.isClosing() {
		return net.ErrClosed
	}
	if !conn.directOwner() {
		return errUnsupported
	}
	conn.setReadStalled(true)
	return nil
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
	if conn.isClosing() || conn.events.closing.Load() || conn.loop.stopping.Load() || !conn.close.drain() {
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
	// that never closes gracefully carries nothing for it. A queue that
	// drains in the instant between the check and the arm leaves a stray
	// timer whose callback finds the phase moved on and does nothing.
	if conn.close.isDraining() {
		conn.wakeState().drain.Store(time.AfterFunc(lingerTimeout, conn.escalateDrain))
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
		if timer := w.drain.Swap(nil); timer != nil {
			timer.Stop()
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
	if conn.isClosing() || conn.descriptorOwner().isClosing() || conn.events.closing.Load() || conn.loop.stopping.Load() || !conn.close.request() {
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
			} else {
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
	return !conn.internal && conn.events.cb.onClose != nil && conn.taskState.Load()&ioEventOpen == 0
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
		if callback := conn.events.cb.onOpen; callback != nil {
			started := conn.beginInboundCallback()
			callback(conn)
			conn.endInboundCallback(started)
		}
	}
	if callback := conn.events.cb.onClose; callback != nil && !conn.internal {
		started := conn.beginInboundCallback()
		callback(conn, err)
		conn.endInboundCallback(started)
	}
}

func (conn *fdConn) SetDeadline(deadline time.Time) error {
	return conn.setDeadline(deadlineBoth, deadline)
}
func (conn *fdConn) SetReadDeadline(deadline time.Time) error {
	return conn.setDeadline(deadlineRead, deadline)
}
func (conn *fdConn) SetWriteDeadline(deadline time.Time) error {
	return conn.setDeadline(deadlineWrite, deadline)
}

func (conn *fdConn) setDeadline(kind deadlineKind, deadline time.Time) error {
	if conn.isClosing() {
		return net.ErrClosed
	}
	return conn.applyDeadline(kind, deadline)
}

// applyDeadline updates the deadline state under submitMu and advances a
// generation. A timer callback carries that generation so a stale callback
// cannot close a connection after the deadline was reset or cleared.
func (conn *fdConn) applyDeadline(kind deadlineKind, deadline time.Time) error {
	conn.submitMu.Lock()
	defer conn.submitMu.Unlock()
	if conn.close.isReleased() {
		return net.ErrClosed
	}
	state := conn.deadlines
	if state == nil {
		if deadline.IsZero() {
			return nil
		}
		state = &deadlineState{}
		conn.deadlines = state
	}
	// Generations make callbacks from a stopped or reset timer harmless.
	if kind == deadlineBoth || kind == deadlineRead {
		state.readGeneration++
		state.readDeadline = deadline
		state.readTimerGen.Store(state.readGeneration)
		state.readTimer = conn.resetDeadlineTimer(state.readTimer, deadlineRead, deadline)
	}
	if kind == deadlineBoth || kind == deadlineWrite {
		state.writeGeneration++
		state.writeDeadline = deadline
		state.writeTimerGen.Store(state.writeGeneration)
		state.writeTimer = conn.resetDeadlineTimer(state.writeTimer, deadlineWrite, deadline)
	}
	return nil
}

func (conn *fdConn) resetDeadlineTimer(timer *time.Timer, kind deadlineKind, deadline time.Time) *time.Timer {
	if timer != nil {
		timer.Stop()
	}
	if deadline.IsZero() {
		return timer
	}
	delay := time.Until(deadline)
	if delay < 0 {
		delay = 0
	}
	if timer == nil {
		return time.AfterFunc(delay, func() { conn.expireDeadline(kind) })
	}
	timer.Reset(delay)
	return timer
}

// expireDeadline runs on the timer's goroutine and closes the connection if
// the deadline the timer was armed for still stands.
func (conn *fdConn) expireDeadline(kind deadlineKind) {
	conn.submitMu.Lock()
	state := conn.deadlines
	if conn.isClosing() || state == nil {
		conn.submitMu.Unlock()
		return
	}
	generation := state.writeTimerGen.Load()
	if kind == deadlineRead {
		generation = state.readTimerGen.Load()
	}
	conn.submitMu.Unlock()
	conn.handleTimeout(kind, generation)
}

// handleTimeout rechecks both generation and wall time. The wall-time check
// covers a reset racing with a timer that already fired.
func (conn *fdConn) handleTimeout(kind deadlineKind, generation uint64) {
	conn.submitMu.Lock()
	state := conn.deadlines
	if conn.isClosing() || state == nil {
		conn.submitMu.Unlock()
		return
	}
	var current uint64
	var deadline time.Time
	if kind == deadlineRead {
		current, deadline = state.readGeneration, state.readDeadline
	} else {
		current, deadline = state.writeGeneration, state.writeDeadline
	}
	conn.submitMu.Unlock()
	// Reset races can submit the current generation early, so check time too.
	if generation != current || deadline.IsZero() || time.Now().Before(deadline) {
		return
	}
	conn.requestClose(fmt.Errorf("uio: %s deadline: %w", deadlineName(kind), os.ErrDeadlineExceeded))
}

func (conn *fdConn) stopDeadlines() {
	conn.submitMu.Lock()
	defer conn.submitMu.Unlock()
	state := conn.deadlines
	if state == nil {
		return
	}
	if state.readTimer != nil {
		state.readTimer.Stop()
	}
	if state.writeTimer != nil {
		state.writeTimer.Stop()
	}
}
