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
	// readHeldFlag marks reads stopped because callbacks left
	// MaxInboundBuffered bytes unconsumed. Only the connection's turn sets or
	// clears it; see holdsRead and resumeHeldRead.
	readHeldFlag uint32 = 1 << iota
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
	// linger time (SocketConn.SetLinger) and SocketConn.Control clear it, as a
	// lingering close would hold up every other descriptor the closer has queued.
	offTurnCloseFlag
	// localCloseFlag records a close requested locally via Close() or Abort().
	localCloseFlag
	// spliceReadFlag routes read rounds through readRoundSplice: a splice
	// owns, or owned until the next read round, the socket's input (see
	// zeroCopyState.spliceMode). Only the connection's turn clears it.
	spliceReadFlag
)

type fdConn struct {
	readControlAccess
	writeControlAccess
	blockControlAccess
	relayAccess
	fd int32
	// taskState coalesces readiness and synthetic events in its low bits and
	// carries, in its top bit, the claim that exactly one task is queued or
	// running: a producer sets both with one atomic and only the first setter
	// submits, and the running turn keeps the bit while it takes the events.
	taskState atomic.Uint32
	pollTag   atomic.Uint32 // registration generation echoed by the poller
	flags     atomic.Uint32 // readHeldFlag and the other flags above
	close     connCloseState
	commonConn

	ioOwner atomic.Int64 // current task goroutine, zero while idle
	pending atomic.Int64 // accepted payload not yet written

	// submitMu orders cross-goroutine submissions and protects the outbound
	// queue while a worker round and an external producer overlap.
	submitMu sync.Mutex
	out      outQueue      // the outbound FIFO; protected by submitMu
	udp      *unixUDPState // nil for stream connections

	// inflight holds a multi-block batch a flush detached from outbound, so
	// its sends and the release of its sent blocks run without submitMu.
	// Every byte in it was accepted before anything still in outbound. It
	// belongs to the holder of the write claim; it is borrowed from a pool
	// only while bytes remain unsent.
	inflight *bytebuf.CompositeBuffer

	deadlines *deadlineState // allocated by the first nonzero deadline
	// scratch is the running read round's reply space while onRead executes;
	// turn-local Write and Writev calls coalesce into it without taking submitMu.
	scratch *readBuffer
	// wake lazily holds timer, graceful-drain and flow-control state, so
	// connections using none of them retain the compact hot allocation.
	wake atomic.Pointer[wakeState]

	writeState atomic.Uint32 // send state and the write claim; see writeOwnerFlag
	turn       uint8         // turn-owned bits: turnCorked, turnFlushing, sticky turnHangup
	internal   bool          // framework-owned endpoint, not a user connection
	statOpened bool          // turn-owned: noteConnOpened ran on loop.stats
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
	// writeTurnHeldFlag marks a claim the connection's turn keeps across
	// onRead while scratch holds turn-local replies.
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
	// writeFileTailFlag records queued file ranges: bytes accepted now go
	// behind the last of them (see outTargetLocked). It changes under
	// submitMu, with the ranges.
	writeFileTailFlag
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
	// turnReadDrained records a read that reached what the socket holds —
	// would-block, end of stream or an error — so a hangup this turn needs
	// no separate look at the socket error.
	turnReadDrained
	// turnRelayed records a relay that took care of a read round's input
	// with splice(2), which ends the round (see readRoundSplice).
	turnRelayed
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

func (conn *fdConn) InOwnTurn() bool { return conn.directOwner() }

// State reports where the connection's streams stand; see ConnState.
func (conn *fdConn) State() ConnState {
	var state ConnState
	if conn.readEnded() {
		state |= StateReadEnded
	}
	if conn.writeState.Load()&writeShutFlag != 0 {
		state |= StateWriteEnded
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
// (closeIfEnded), or when a non-direct driver closes the connection itself.
func (conn *fdConn) onReadEOF() error {
	if conn.autoCloseOnEOF() && !conn.currentReadControl().Held() &&
		conn.inbound.Empty() && len(conn.inboundTail) == 0 {
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
		return conn.flags.Load()&(readHeldFlag|readEOFFlag) != 0 || conn.ReadPaused()
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
