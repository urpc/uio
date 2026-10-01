//go:build (linux || darwin || netbsd || freebsd || openbsd || dragonfly) && !stdio

package uio

import (
	"errors"
	"fmt"
	"io"
	"net"
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

// fdConn separates control-plane and data-plane ownership. The event loop owns
// descriptor registration, deadlines, and interest. At most one connection
// task reads the stream socket and runs callbacks, while submitMu admits
// concurrent producers into the connection-owned outbound queue. Sending is
// owned separately by the write claim in writeState: the turn sends its own
// replies inline when the claim is free, and output from other goroutines is
// sent by a write turn that runs beside the reading one.
//
// The layout keeps what a connection turn touches in the first lines and the
// whole value within the 256-byte size class, whose objects are line aligned.
type fdConn struct {
	commonConn
	fd            int
	pendingEvents atomic.Uint32 // coalesced readiness and synthetic events
	scheduled     atomic.Bool   // exactly one task is queued or running
	ioOwner       atomic.Int64  // current task goroutine, zero while idle
	writeState    atomic.Uint32 // send state and the write claim; see writeOwnerFlag
	readStalled   atomic.Bool   // ET read work owed after yield/backpressure
	pending       atomic.Int64  // accepted payload not yet written

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

	pollTag   uint32          // registration generation echoed by the data poller
	interest  poller.Interest // event-loop-owned registered interest
	throttled bool            // loop-owned read-interest hysteresis
	corked    bool            // task-owned read-round batching flag
	internal  bool            // framework-owned endpoint, not a user connection

	deadlines *deadlineState // allocated by the first nonzero deadline
}

// unixUDPState contains fields that TCP connections never use.
type unixUDPState struct {
	file   *os.File // owns a duplicated UDP listener fd when non-nil
	remote syscall.Sockaddr
	server *fdConn
	peers  map[socket.UDPAddress]*fdConn
	key    socket.UDPAddress
}

// deadlineState remains loop-owned except for its atomic timer generations.
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
	// writeTurnFlushFlag marks the connection's turn flushing with the claim,
	// whether it took the claim for the flush or keeps it for a reservation.
	// OnOutbound runs inside that flush, so the turn's own writes from it are
	// queued rather than sent, and the turn's cleanup releases the claim if
	// the callback panics.
	writeTurnFlushFlag
	// writeCloseWaitFlag asks the claim holder to hand a deferred close back to
	// the loop when it releases the claim.
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
)

// writeClaimerMask holds the low bits of the goroutine id a write turn's claim
// was taken for, which tells its own further writes from other producers'.
const (
	writeClaimerShift        = 16
	writeClaimerMask  uint32 = 0xffff << writeClaimerShift
)

func claimerTag() uint32 { return uint32(currentGoroutineID()) << writeClaimerShift }

const (
	// Close is split so descriptor teardown remains loop-owned while OnClose is
	// delivered by the same serialized connection-task mechanism as OnData.
	closeOpen uint32 = iota
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

type closeRequest struct{ cause error }

func (state *connCloseState) isClosing() bool  { return state.phase.Load() >= closeRequested }
func (state *connCloseState) isReleased() bool { return state.phase.Load() >= closeResourcesReleased }
func (state *connCloseState) request() bool {
	return state.phase.CompareAndSwap(closeOpen, closeRequested)
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

// turnHoldsWrite reports a claim the connection's turn keeps for a reservation
// and may use for its own writes: not from inside its own flush.
func (conn *fdConn) turnHoldsWrite() bool {
	return conn.writeState.Load()&(writeTurnHeldFlag|writeTurnFlushFlag) == writeTurnHeldFlag
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
// writeCloseWaitFlag, to be handed back to the loop now.
func (conn *fdConn) releaseWrite() {
	old := conn.writeState.And(^(writeOwnerFlag | writeTurnHeldFlag | writeTurnFlushFlag))
	if old&writeCloseWaitFlag != 0 {
		conn.writeState.And(^writeCloseWaitFlag)
		t := acquireTask(closeTask, conn)
		if !conn.loop.submitTask(t) {
			releaseTask(t)
			// The stopped loop's shutdown pass owns final fd teardown.
		}
	}
}

// releaseWriteAndKick releases the claim and then sends whatever producers
// appended while it was held: they found the claim taken and left their
// bytes to this holder.
func (conn *fdConn) releaseWriteAndKick() {
	conn.releaseWrite()
	conn.kickWriter()
}

// kickWriter gives accepted output a sender. If the claim is free it takes it
// and hands it to a write turn; if it is taken, the holder's release rechecks.
// Output waits for the open turn before OnOpen returns, for a writable edge
// while the socket is full, and for the close flush once closing started.
func (conn *fdConn) kickWriter() {
	if conn.isDatagram() || conn.pending.Load() == 0 || conn.isClosing() {
		return
	}
	state := conn.writeState.Load()
	if state&writeOwnerFlag != 0 {
		// The holder sends these bytes. Output from a goroutine other than
		// the one its claim was taken for means several producers write now.
		if state&writeContendedFlag == 0 && state&writeClaimerMask != claimerTag() {
			conn.writeState.Or(writeContendedFlag)
		}
		return
	}
	if state&writeBlockedFlag != 0 || state&writeOpenedFlag == 0 {
		return
	}
	if !conn.tryClaimWrite(claimerTag()) {
		return
	}
	if !conn.loop.acquireIO() {
		// Shutting down: its close pass sends the bounded final flush.
		conn.releaseWrite()
		return
	}
	if !conn.loop.ioPool.submitWrite(conn) {
		conn.handleWriteSubmitFailure()
	}
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
// reads. It holds the write claim and one count of its loop's ioState, which
// shutdown joins. It never runs callbacks other than OnOutbound.
func (conn *fdConn) runWriteTurn() {
	done := false
	defer func() {
		if !done {
			// OnOutbound panicked during a flush. The claim and the count go
			// back, so the connection can still send and close, and shutdown
			// does not wait for this turn.
			conn.releaseWriteAndKick()
			conn.loop.releaseIO()
		}
	}()
	for flushes := 0; ; flushes++ {
		if conn.isClosing() || conn.pending.Load() == 0 || conn.writeBlocked() {
			break
		}
		if flushes == maxWriteFlushes {
			done = true
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
			break
		}
		if conn.writeBlocked() {
			// Arm writable interest; the edge restarts sending.
			conn.scheduleRefresh()
			break
		}
	}
	done = true
	conn.releaseWrite()
	// Bytes appended after the last flush found the claim taken.
	if conn.pending.Load() != 0 && !conn.writeBlocked() && !conn.isClosing() &&
		!conn.loop.ioStopped() && conn.tryClaimWrite(0) {
		// This turn's count moves to the next one.
		if !conn.loop.ioPool.submitWrite(conn) {
			conn.handleWriteSubmitFailure()
		}
		return
	}
	// Released last: shutdown joins write turns through this count.
	conn.loop.releaseIO()
}

func (conn *fdConn) Fd() int                              { return conn.fd }
func (conn *fdConn) initialInterest() poller.Interest     { return poller.Readable }
func (conn *fdConn) setInterest(interest poller.Interest) { conn.interest = interest }
func (conn *fdConn) currentInterest() poller.Interest     { return conn.interest }
func (conn *fdConn) isClosing() bool                      { return conn.close.isClosing() }
func (conn *fdConn) isClosedOnLoop() bool                 { return conn.close.isReleased() }
func (conn *fdConn) beginShutdown()                       { conn.close.request() }
func (conn *fdConn) isDatagram() bool                     { return conn.udp != nil }
func (conn *fdConn) afterRegister()                       {}

// readNeedsRedelivery runs on the loop after updateInterest. Reads the outbound
// limit throttled stay paused until a send retires the backlog to the resume
// mark and refreshes again: redelivered now, a read would only feed callbacks
// whose replies cannot be accepted.
func (conn *fdConn) readNeedsRedelivery() bool { return conn.readStalled.Load() && !conn.throttled }
func (conn *fdConn) clearReadRedelivery() bool { return conn.readStalled.CompareAndSwap(true, false) }
func (conn *fdConn) writeIsBlocked() bool      { return conn.writeBlocked() }

// Direct I/O belongs to this connection's active task. Datagrams additionally
// belong to their loop during delivery; a loop never owns a stream's I/O task,
// even when another connection's callback happens to run on that loop.
func (conn *fdConn) directOwner() bool {
	owner := currentGoroutineID()
	return conn.ioOwner.Load() == owner || (conn.isDatagram() && conn.loop != nil && conn.loop.loopGoid.Load() == owner)
}

const (
	ioEventRead  uint32 = 1
	ioEventWrite uint32 = 2
	ioEventOpen  uint32 = 1 << 16
	ioEventWake  uint32 = 1 << 17
	ioEventClose uint32 = 1 << 18
	// ioEventAccepted rides with ioEventOpen for an accepted TCP connection,
	// whose open turn applies the default socket options.
	ioEventAccepted uint32 = 1 << 19
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

// markOpenPending makes the open event pending before a stream becomes
// visible to its poller. The shared data poller may see the stream's first
// bytes and submit its task before the loop schedules the open event; that
// task then takes both events and still runs OnOpen before it reads.
func (conn *fdConn) markOpenPending() { conn.pendingEvents.Or(ioEventOpen) }

// clearOpenPending withdraws the open event of a stream that failed to
// register.
func (conn *fdConn) clearOpenPending() { conn.pendingEvents.And(^(ioEventOpen | ioEventAccepted)) }

// scheduleOpen submits the task for a pending open event unless the data
// poller already has. Noting ioEventOpen again would run OnOpen twice if that
// task has already taken it.
func (conn *fdConn) scheduleOpen() {
	if conn.pendingEvents.Load()&ioEventOpen != 0 {
		conn.scheduleIO(0)
	}
}

// noteIO folds readiness into this connection and reports whether the caller
// acquired responsibility for submitting its single in-flight task. The
// scheduled claim is the last step: once it is visible a task must run, since
// closeOnLoop hands teardown to whichever task holds it.
func (conn *fdConn) noteIO(events uint32) bool {
	if conn.isClosing() || conn.loop == nil || conn.loop.stopping.Load() || conn.loop.ioPool == nil {
		return false
	}
	conn.pendingEvents.Or(events)
	if conn.scheduled.Load() {
		return false
	}
	if !conn.loop.acquireIO() {
		return false
	}
	if conn.isClosing() || conn.loop.stopping.Load() || !conn.scheduled.CompareAndSwap(false, true) {
		conn.loop.releaseIO()
		return false
	}
	return true
}

// watchTags generates registration tags. Zero means untagged.
var watchTags atomic.Uint32

// watcher returns the poller watching this descriptor: the shared data poller
// for streams when the backend has one, otherwise the owning loop's poller.
func (conn *fdConn) watcher() *poller.NetPoller {
	if data := conn.events.dataPlane(); data != nil && !conn.isDatagram() {
		return data
	}
	return conn.loop.poller
}

// assignWatchTag gives a new registration its own tag, so readiness reported
// for an earlier connection on the same descriptor number is not delivered to
// this one.
func (conn *fdConn) assignWatchTag() uint32 {
	tag := watchTags.Add(1)
	if tag == 0 {
		tag = watchTags.Add(1)
	}
	conn.pollTag = tag
	return tag
}

// prepareAccepted marks an accepted TCP connection whose default socket
// options are still to be applied. The open task applies them, off the event
// loop and before OnOpen, so a loop registering a connection storm spends one
// epoll_ctl per connection instead of four syscalls. The mark is set before
// registration publishes the open event, so one turn takes both together.
func (conn *fdConn) prepareAccepted() { conn.pendingEvents.Or(ioEventAccepted) }

func (conn *fdConn) applyAcceptedOptions() {
	_ = conn.applySocketOption(optionNoDelay, 1)
	_ = conn.applySocketOption(optionKeepAlive, 1)
	_ = conn.applySocketOption(optionKeepAlivePeriod, defaultTCPKeepAliveSecs)
}

func (conn *fdConn) scheduleRefresh() {
	if conn.loop == nil || conn.loop.stopping.Load() {
		return
	}
	t := acquireTask(refreshTask, conn)
	if !conn.loop.submitTask(t) {
		releaseTask(t)
	}
}

func (conn *fdConn) takeIOEvents() uint32 {
	return conn.pendingEvents.Swap(0)
}

// finishIOTask closes the lost-work race between the task's final event check
// and a concurrent producer. scheduled is cleared under submitMu, then events
// are checked again; either the current task resubmits itself or the producer
// wins the idle-to-scheduled transition. Deferred close returns to the loop
// only after no task can still touch connection-owned buffers.
func (conn *fdConn) finishIOTask() {
	if conn.pendingEvents.Load() != 0 && !conn.loop.ioStopped() && conn.close.phase.Load() != closeCallbackDelivered {
		if !conn.loop.ioPool.submit(conn) {
			conn.handleIOSubmitFailure(net.ErrClosed)
		}
		return
	}
	conn.submitMu.Lock()
	conn.scheduled.Store(false)
	hasDeferredClose := !conn.close.isReleased() && conn.close.deferred != nil
	conn.submitMu.Unlock()
	rescheduled := false
	if conn.pendingEvents.Load() != 0 && !conn.loop.ioStopped() && conn.scheduled.CompareAndSwap(false, true) {
		rescheduled = true // the current reservation moves to the next turn
		if !conn.loop.ioPool.submit(conn) {
			conn.handleIOSubmitFailure(net.ErrClosed)
		}
	}
	if hasDeferredClose {
		t := acquireTask(closeTask, conn)
		// The close-phase winner takes the deferred cause. Keeping
		// it in the slot until then prevents shutdown from overtaking the handoff.
		if !conn.loop.submitTask(t) {
			releaseTask(t)
			// The stopped loop's shutdown pass owns final fd teardown.
		}
	}
	// Released last: shutdown joins turns through this count, so nothing of
	// this turn may touch the connection or its loop afterwards.
	if !rescheduled {
		conn.loop.releaseIO()
	}
}

func (conn *fdConn) handleIOSubmitFailure(err error) {
	// A rejected task will never run finishIOTask, so this ends its turn the
	// way finishIOTask does. Its scheduling claim is given up under submitMu:
	// closeOnLoop may already have seen the claim and deferred closure to this
	// turn, and then the closure goes back to the loop here, or the connection
	// would never close.
	conn.submitMu.Lock()
	conn.scheduled.Store(false)
	hasDeferredClose := !conn.close.isReleased() && conn.close.deferred != nil
	conn.submitMu.Unlock()
	phase := conn.close.phase.Load()
	if phase < closeResourcesReleased {
		conn.requestClose(err)
	} else if phase == closeResourcesReleased && conn.pendingEvents.Load()&ioEventClose != 0 {
		// closeOnLoop publishes the final cause before setting ioEventClose.
		// A rejected earlier task must not deliver OnClose during that gap.
		conn.fireCloseCallback()
	}
	if hasDeferredClose && conn.loop != nil {
		t := acquireTask(closeTask, conn)
		if !conn.loop.submitTask(t) {
			releaseTask(t)
			// The stopped loop's shutdown pass owns final fd teardown.
		}
	}
	if conn.loop != nil {
		conn.loop.releaseIO()
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
	if conn.close.deferred == nil {
		conn.close.deferred = &closeRequest{cause: err}
	} else if conn.close.deferred.cause == nil && err != nil {
		conn.close.deferred.cause = err
	}
}

// runIOTask performs one serialized connection turn. Ordering matters: close
// is terminal, writable readiness drains old output before new input is read,
// read callbacks may enqueue corked replies, and one final flush publishes the
// complete round before the loop updates interest.
func (conn *fdConn) runIOTask() {
	conn.ioOwner.Store(currentGoroutineID())
	defer func() {
		conn.corked = false // a callback panic may have left a round corked
		if conn.turnOwnsClaim() {
			// A callback panic may have left the claim taken for a
			// reservation or a flush.
			conn.releaseWriteAndKick()
		}
		conn.ioOwner.Store(0)
		conn.finishIOTask()
	}()
	events := conn.takeIOEvents()
	if events&ioEventClose != 0 {
		conn.fireCloseCallback()
		return
	}
	if conn.close.isReleased() {
		return
	}
	if events&ioEventOpen != 0 {
		if events&ioEventAccepted != 0 {
			conn.applyAcceptedOptions()
		}
		conn.fireOnOpen()
		// Output from other goroutines may start write turns from now on; what
		// they queued before this point leaves with this turn's flush below.
		conn.writeState.Or(writeOpenedFlag)
	}
	if !conn.isClosing() && events&ioEventWrite != 0 {
		conn.noteWritable()
		if _, err := conn.flushOnLoop(); err != nil {
			conn.requestClose(err)
		}
	}
	if !conn.isClosing() && events&ioEventRead != 0 {
		var err error
		if conn.isDatagram() {
			err = conn.onRecvUDP()
		} else {
			err = conn.onRead()
		}
		if err != nil {
			conn.requestClose(err)
		}
	}
	if !conn.isClosing() && !conn.readStalled.Load() && events&ioEventWake != 0 {
		conn.corked = true
		if err := conn.fireOnData(); err != nil {
			conn.requestClose(err)
		}
		conn.corked = false
	}
	if !conn.isClosing() {
		if _, err := conn.flushOnLoop(); err != nil {
			conn.requestClose(err)
		}
	}
	if conn.turnOwnsClaim() {
		// The round's reservations are filled and flushed: other senders may
		// take the socket again, starting with output that waited for them.
		conn.releaseWriteAndKick()
	}
	if conn.needsInterestRefresh(events) {
		if conn.canRedeliverRead(events) {
			// A read round that only yielded leaves nothing for the loop to
			// change: the next turn is queued here instead of through it.
			conn.readStalled.Store(false)
			conn.pendingEvents.Or(ioEventRead | ioEventWake)
		} else {
			conn.scheduleRefresh()
		}
	}
}

// canRedeliverRead reports whether a stalled read may be handed straight to
// the next turn. Only the refresh command may change registered interest, so
// this holds only when the interest the loop would compute is unchanged:
// no outbound limit to throttle reads, nothing left to write, and no writable
// edge whose interest may need disarming.
func (conn *fdConn) canRedeliverRead(events uint32) bool {
	return conn.readStalled.Load() && events&ioEventWrite == 0 && conn.events.MaxOutboundBuffered <= 0 &&
		conn.outboundEmpty() && !conn.writeBlocked() && !conn.isClosing()
}

// RunTask implements IOTask for injected executors. The executor contract
// guarantees asynchronous dispatch; the loop's ioState counts the turn.
func (conn *fdConn) RunTask() {
	if conn.loop == nil || conn.loop.ioPool == nil {
		return
	}
	conn.runIOTask()
}

func (conn *fdConn) needsInterestRefresh(events uint32) bool {
	if conn.readStalled.Load() || events&ioEventWrite != 0 {
		return true
	}
	// Remaining output needs writable interest only while the socket is full
	// or nobody holds the claim to send it; a write turn arms it itself when
	// its send blocks.
	if !conn.outboundEmpty() && (conn.writeBlocked() || !conn.writeClaimed()) {
		return true
	}
	if limit := int64(conn.events.MaxOutboundBuffered); limit > 0 {
		return conn.pending.Load() >= limit-limit/4
	}
	return false
}

func (conn *fdConn) closeUnregistered() {
	if conn.close.abandon() {
		if conn.udp != nil && conn.udp.file != nil {
			_ = conn.udp.file.Close()
		} else {
			_ = syscall.Close(conn.fd)
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

func (conn *fdConn) setSocketOption(kind socketOptionKind, value int) error {
	if conn.isClosing() {
		return net.ErrClosed
	}
	// The loop and the connection's own task may both apply options directly:
	// descriptor teardown waits for a running task, so the fd cannot change
	// under the task, and neither caller would wait on the loop queue.
	if conn.loop.inLoop() || conn.directOwner() {
		return conn.applySocketOption(kind, value)
	}
	// External callers wait for the loop result; no submission lock covers I/O.
	t := acquireTask(optionTask, conn)
	t.optionKind, t.optionValue = kind, value
	t.done = make(chan error, 1)
	done := t.done
	if !conn.loop.submitTask(t) {
		releaseTask(t)
		return net.ErrClosed
	}
	return <-done
}

func (conn *fdConn) applySocketOption(kind socketOptionKind, value int) error {
	if conn.close.isReleased() {
		return net.ErrClosed
	}
	switch kind {
	case optionLinger:
		return socket.SetLinger(conn.fd, value)
	case optionNoDelay:
		return socket.SetNoDelay(conn.fd, value != 0)
	case optionKeepAlive:
		return socket.SetKeepAlive(conn.fd, value != 0)
	case optionKeepAlivePeriod:
		return socket.SetKeepAlivePeriod(conn.fd, value)
	case optionReadBuffer:
		return socket.SetRecvBuffer(conn.fd, value)
	default:
		return socket.SetSendBuffer(conn.fd, value)
	}
}

func (conn *fdConn) fireOnOpen() {
	if callback := conn.events.OnOpen; callback != nil {
		started := conn.beginInboundCallback()
		callback(conn)
		conn.endInboundCallback(started)
	}
	if !conn.scheduled.Load() {
		if err := conn.finishCallback(); err != nil {
			conn.requestClose(err)
		}
	}
}

// fireOnData establishes the inbound-access scope around application code.
// Unread stream bytes remain available to a later callback; unread UDP bytes
// are discarded by the datagram caller because packet lifetime is one turn.
func (conn *fdConn) fireOnData() error {
	var err error
	if callback := conn.events.OnData; callback != nil {
		started := conn.beginInboundCallback()
		err = callback(conn)
		conn.endInboundCallback(started)
	} else {
		started := conn.beginInboundCallback()
		_, _ = conn.Discard(-1)
		conn.endInboundCallback(started)
	}
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
	if _, err := conn.flushOnLoop(); err != nil {
		return err
	}
	return conn.updateInterest()
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
	if _, err := conn.flushOnLoop(); err != nil {
		return err
	}
	return conn.updateInterest()
}

// onRead drains an edge-triggered stream with bounded work per task. The
// borrowed read buffer is exposed directly during OnData; only a callback that
// leaves bytes unread pays the copy into the persistent inbound buffer.
func (conn *fdConn) onRead() error {
	holder := conn.events.readPool.Get().(*readBuffer)
	// A read round is corked: replies the callbacks enqueue accumulate in
	// outbound and are flushed once when the event ends, or each time they
	// fill a coalescing block, so a burst of reads costs one writev instead
	// of one syscall per reply.
	conn.corked = true
	err := conn.readRound(holder.bytes)
	conn.corked = false
	conn.events.readPool.Put(holder)
	return err
}

// readRound is onRead's body. It has many exits, which would keep deferred
// cleanup from being open-coded, so onRead does the cleanup around it.
func (conn *fdConn) readRound(buffer []byte) error {
	totalRead := 0
	for calls := 0; calls < 256 && totalRead < 1<<20; calls++ {
		n, err := socket.Recv(conn.fd, buffer)
		if err != nil {
			if isWouldBlock(err) {
				return nil
			}
			return err
		}
		if n == 0 {
			return io.EOF
		}
		totalRead += n
		// inboundTail aliases the pooled read buffer. Unconsumed bytes are copied
		// into inbound before the buffer is reused by the next syscall.
		conn.inboundTail = buffer[:n]
		conn.events.onSocketBytesRead(conn, n)
		if conn.isClosing() {
			conn.inboundTail = nil
			return nil
		}
		if err = conn.fireOnData(); err != nil {
			return err
		}
		if conn.isClosing() {
			conn.inboundTail = nil
			return nil
		}
		if len(conn.inboundTail) > 0 {
			started := conn.beginInboundCallback()
			if limit := conn.events.MaxInboundBuffered; limit > 0 && conn.InboundBuffered() > limit {
				conn.inboundTail = nil
				conn.endInboundCallback(started)
				return ErrInboundOverflow
			}
			_, _ = conn.inbound.Write(conn.inboundTail)
			conn.inboundTail = nil
			conn.endInboundCallback(started)
		}
		if conn.readStalled.Load() {
			return nil
		}
		if conn.readShouldStop() {
			// The replies queued so far already fill this connection's limit.
			// Hand them to the socket before deciding to stop, so no accepted
			// byte waits on a paused read.
			if _, err := conn.flushOnLoop(); err != nil {
				return err
			}
			if conn.readShouldStop() {
				// The peer is behind. readStalled records that ET delivery is owed;
				// the refresh command resubmits the read after output drains.
				conn.readStalled.Store(true)
				return nil
			}
		}
		if n < len(buffer) {
			return nil
		}
		// The round reads on. Once its replies fill a coalescing block, send
		// them before reading further: a peer that keeps a window of requests
		// in flight is otherwise left waiting for the whole round, which can
		// run to 256 reads, and its pipeline empties. Smaller bursts still
		// leave in one write when the round ends.
		if conn.pending.Load() >= int64(conn.coalesceBlockSize()) {
			if _, err := conn.flushOnLoop(); err != nil {
				return err
			}
		}
	}
	if totalRead > 0 {
		conn.readStalled.Store(true)
	}
	return nil
}

// onRecvUDP bounds datagram work per loop turn. UDP stays loop-owned because
// logical peers share this socket and its peer map.
func (conn *fdConn) onRecvUDP() error {
	buffer := conn.loop.getBuffer()
	totalRead := 0
	var receive socket.UDPReceive
	for packets := 0; packets < 256 && totalRead < 1<<20; packets++ {
		n, err := socket.RecvUDP(conn.fd, buffer, &receive)
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
	return nil
}

// handleUDPPacket resolves or creates the logical peer connection and lends
// the packet buffer only for the duration of its synchronous callbacks.
func (conn *fdConn) handleUDPPacket(packet []byte, source socket.UDPAddress) {
	packetConn := conn
	if conn.udp.peers != nil {
		// UDP children are logical connections that share the server fd.
		packetConn = conn.udp.peers[source]
		if packetConn == nil {
			sockaddr := source.Sockaddr()
			if sockaddr == nil {
				return
			}
			packetConn = &fdConn{fd: conn.fd, udp: &unixUDPState{remote: sockaddr, server: conn, key: source}}
			packetConn.events, packetConn.loop = conn.events, conn.loop
			packetConn.localAddr, packetConn.remoteAddr = conn.localAddr, source.NetAddr()
			conn.udp.peers[source] = packetConn
			packetConn.fireOnOpen()
		}
	}
	if packetConn.isClosing() {
		return
	}
	packetConn.inboundTail = packet
	packetConn.events.onSocketBytesRead(packetConn, len(packet))
	err := packetConn.fireOnData()
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

func (conn *fdConn) runWakeTask() error {
	if conn.isClosing() {
		return net.ErrClosed
	}
	if conn.isDatagram() {
		return conn.fireOnData()
	}
	conn.scheduleIO(ioEventWake)
	return nil
}

func (conn *fdConn) Wake() error {
	if conn.isClosing() {
		return net.ErrClosed
	}
	if conn.isDatagram() {
		if conn.loop == nil {
			return net.ErrClosed
		}
		t := acquireTask(wakeTask, conn)
		if !conn.loop.submitTask(t) {
			releaseTask(t)
			return net.ErrClosed
		}
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
		return errUnsupported
	}
	conn.readStalled.Store(true)
	return nil
}

func (conn *fdConn) Close() error { return conn.CloseWith(io.ErrUnexpectedEOF) }

func (conn *fdConn) CloseWith(err error) error {
	t := acquireTask(closeTask, conn)
	t.err = err
	// Setting closing and linking closeTask under submitMu puts Close after
	// external writes that have already reached their submission point.
	conn.submitMu.Lock()
	if conn.isClosing() || conn.events.closing.Load() || conn.loop.stopping.Load() {
		conn.submitMu.Unlock()
		releaseTask(t)
		return net.ErrClosed
	}
	if !conn.close.request() {
		conn.submitMu.Unlock()
		releaseTask(t)
		return net.ErrClosed
	}
	if !conn.loop.pushTask(t) {
		conn.submitMu.Unlock()
		releaseTask(t)
		return net.ErrClosed
	}
	conn.submitMu.Unlock()
	conn.loop.notify()
	return nil
}

// requestClose is the internal idempotent close path. If the loop queue is
// already sealed, the cause is retained for loop shutdown to consume.
func (conn *fdConn) requestClose(err error) {
	if conn.isClosing() {
		if conn.loop != nil && conn.loop.stopping.Load() && !conn.close.isReleased() {
			conn.submitMu.Lock()
			conn.setDeferredCloseLocked(err)
			conn.submitMu.Unlock()
		}
		return
	}
	t := acquireTask(closeTask, conn)
	t.err = err
	conn.submitMu.Lock()
	if conn.isClosing() {
		conn.submitMu.Unlock()
		releaseTask(t)
		return
	}
	if !conn.close.request() {
		conn.submitMu.Unlock()
		releaseTask(t)
		return
	}
	if !conn.loop.pushTask(t) {
		// The queue is already stopping; preserve the I/O cause for shutdown.
		conn.setDeferredCloseLocked(err)
		conn.submitMu.Unlock()
		releaseTask(t)
		return
	}
	conn.submitMu.Unlock()
	conn.loop.notify()
}

// closeOnLoop releases transport resources exactly once. A running connection
// task keeps ownership of its buffers, so the loop records the cause and lets
// finishIOTask hand closure back after the task exits. A sender holding the
// write claim may be writing the socket, so a stream also needs the claim
// before its descriptor closes: otherwise the cause is recorded the same way
// and the holder hands closure back when it releases the claim. The claim is
// then kept for good. OnClose is emitted as a final connection task,
// preserving callback serialization.
func (conn *fdConn) closeOnLoop(cause error) {
	if conn.close.isReleased() {
		return
	}
	if conn.scheduled.Load() {
		conn.submitMu.Lock()
		if conn.scheduled.Load() && !conn.close.isReleased() {
			conn.setDeferredCloseLocked(cause)
			conn.submitMu.Unlock()
			return
		}
		conn.submitMu.Unlock()
	}
	if !conn.isDatagram() && !conn.claimWriteForClose(&cause) {
		return
	}
	if !conn.close.release() {
		return
	}
	// Close gets one bounded flush attempt; a slow peer cannot delay shutdown.
	var flushErr error
	if !conn.isDatagram() && !conn.writeFailed() {
		conn.setWriteBlocked(false)
		_, flushErr = conn.flushWrite()
	}
	remaining := conn.pending.Load()
	deferredCloseErr := conn.takeDeferredCloseCause()
	finalErr := errors.Join(cause, deferredCloseErr, flushErr)
	if remaining > 0 {
		finalErr = errors.Join(finalErr, UnflushedError{Remaining: remaining})
	}
	conn.stopDeadlines()
	if conn.udp != nil && conn.udp.server != nil {
		// A child only leaves the peer map; its server owns the shared fd.
		delete(conn.udp.server.udp.peers, conn.udp.key)
	} else {
		if conn.udp != nil && conn.udp.peers != nil {
			children := conn.udp.peers
			conn.udp.peers = nil
			for _, child := range children {
				child.closeOnLoop(finalErr)
			}
		}
		conn.loop.delConn(conn)
		if conn.udp != nil && conn.udp.file != nil {
			_ = conn.udp.file.Close()
		} else {
			_ = syscall.Close(conn.fd)
		}
	}
	conn.submitMu.Lock()
	conn.outbound.Reset()
	conn.submitMu.Unlock()
	conn.releaseInflight()
	conn.inbound.Reset()
	conn.inboundTail = nil
	conn.pending.Store(0)
	conn.scheduleCloseCallback(finalErr)
}

// claimWriteForClose takes the write claim for good before teardown, since a
// holder may be in the middle of a send. While it is taken, the cause is
// recorded, the holder is asked through writeCloseWaitFlag to hand closure
// back, and the claim is tried once more: a holder that released in between
// either saw the request or left the claim free. Recording the cause moves it
// into the deferred slot, so *cause is cleared when the retry wins. During
// shutdown every write turn has already been joined, and a holder can only be
// a producer that is giving the claim straight back, so the loop waits for it.
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

// scheduleCloseCallback publishes OnClose as the terminal synthetic event.
// Connections without a user callback advance the phase immediately.
func (conn *fdConn) scheduleCloseCallback(err error) {
	if conn.internal || conn.events.OnClose == nil {
		conn.close.phase.CompareAndSwap(closeResourcesReleased, closeCallbackDelivered)
		return
	}
	conn.submitMu.Lock()
	conn.setDeferredCloseLocked(err)
	conn.submitMu.Unlock()
	conn.pendingEvents.Or(ioEventClose)
	if !conn.scheduled.CompareAndSwap(false, true) {
		return
	}
	if conn.loop != nil {
		conn.loop.acquireCloseIO()
	}
	if conn.loop == nil || conn.loop.ioPool == nil || !conn.loop.ioPool.submit(conn) {
		conn.scheduled.Store(false)
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
	if callback := conn.events.OnClose; callback != nil && !conn.internal {
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
	if conn.loop.inLoop() {
		return conn.applyDeadline(kind, deadline)
	}
	// Deadline setters are synchronous even though application happens on-loop.
	t := acquireTask(deadlineTask, conn)
	t.deadlineKind, t.deadline = kind, deadline
	t.done = make(chan error, 1)
	done := t.done
	if !conn.loop.submitTask(t) {
		releaseTask(t)
		return net.ErrClosed
	}
	return <-done
}

// applyDeadline updates loop-owned deadline state and advances a generation.
// A timer callback carries that generation so a stale callback cannot close a
// connection after the deadline was reset or cleared.
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
		// The callback only submits work; it never touches loop-owned state.
		return time.AfterFunc(delay, func() { conn.submitTimeout(kind) })
	}
	timer.Reset(delay)
	return timer
}

func (conn *fdConn) submitTimeout(kind deadlineKind) {
	conn.submitMu.Lock()
	state := conn.deadlines
	if conn.isClosing() || state == nil {
		conn.submitMu.Unlock()
		return
	}
	t := acquireTask(timeoutTask, conn)
	t.deadlineKind = kind
	if kind == deadlineRead {
		t.generation = state.readTimerGen.Load()
	} else {
		t.generation = state.writeTimerGen.Load()
	}
	conn.submitMu.Unlock()
	if !conn.loop.submitTask(t) {
		releaseTask(t)
	}
}

// handleTimeout rechecks both generation and wall time on the event loop. The
// wall-time check covers a reset racing with an already submitted timer task.
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

func deadlineName(kind deadlineKind) string {
	if kind == deadlineRead {
		return "read"
	}
	return "write"
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
