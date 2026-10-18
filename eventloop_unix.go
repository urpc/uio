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
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/urpc/uio/internal/fdmap"
	"github.com/urpc/uio/internal/poller"
	"github.com/urpc/uio/internal/socket"
)

// maxLoops caps the default loop count at the number of L3 domains a wide AMD
// part tends to have. One poller collecting for a whole 64-core host had every
// wake-up land on the collecting goroutine's P and every worker cross cores to
// reach it (runtime/trace: ~23 core-seconds per second runnable but
// unscheduled against ~30 running); several pollers keep a connection's events
// within one cluster of cores, and measurements on that host put the knee at
// eight.
const maxLoops = 8

// defaultPollers scales the loop count with the machine: one loop per four
// Ps, at least one and at most maxLoops. A small host keeps a single loop, so
// it pays nothing for a structure it cannot fill.
func defaultPollers(procs int) int {
	return min(max(1, procs/4), maxLoops)
}

// loopWaitersOverride fixes the number of waiters per loop in tests; zero
// means automatic.
var loopWaitersOverride int

// loopWaiters is how many goroutines wait on each of loops pollers. epoll and
// kqueue hand each ready edge to one waiter, so a second waiter on the same
// poller collects the next batch while the first is still folding its events
// in and handing them to the executor. The process-wide total scales with the
// machine — one per four Ps, at least two — and the loops divide it, so a
// small host whose single loop keeps them all behaves as before, and a wide
// one keeps two per loop. Measurements on a 64-core host put the knee there:
// a third waiter per loop lost throughput.
func loopWaiters(procs, loops int) int {
	if loopWaitersOverride > 0 {
		return loopWaitersOverride
	}
	if procs < 4 {
		// One or two Ps run a single loop, and its waiters are what feed
		// the executor: a lone waiter's next hand-off waits for that waiter
		// to get a P back, and on so few Ps there is no slack to absorb the
		// turn. A second waiter covers it; a third would only take turns on
		// the same loop.
		if procs >= 2 {
			return 2
		}
		return 1
	}
	return max(1, max(2, procs/4)/loops)
}

// parkedWaiters is the most waiters that still benefit from a parked wait.
// The runtime's netpoller wakes a parked goroutine only when a P runs out of
// work or its 10ms tick lands, so once enough waiters park at once some wait
// minutes-long stretches in wall-clock terms and closed-loop echo pays it in
// round trips: measured on a 64-logical-CPU host, one waiter per loop kept
// pace while two per loop — sixteen parked waiters over eight pollers —
// halved throughput (3.7M to 1.5M requests a second). Up to the eight waiters
// a 48-CPU host runs, park measured level or ahead everywhere, and on a small
// host it is what fills the cores at all. Past that a waiter blocks in the
// kernel as before.
const parkedWaiters = 8

// A waiter that is its loop's only waiter parks in the runtime's netpoller
// while its rounds are busy, and blocks in the kernel once they go quiet: an
// event wakes the blocked thread directly, without the runtime's netpoll pass
// a parked waiter pays on every event, a hand-off measured at one futex wake
// per request on a ten-connection echo. Under load the parked wait wins
// again, since a waiter that blocks holds its P through every wait; that cost
// up to nine percent of throughput on a saturated host. So quietWaits rounds
// in a row of quietBatch events or fewer, which a loaded waiter never sees,
// send it to block, and a round of parkBatch events or more sends it back to
// park. Rounds in between hold the mode and only break the quiet streak.
//
// Only a loop's only waiter takes this up, and only on a host of at least
// adaptiveMinProcs Ps (see waitMode): a waiter blocked in the kernel holds
// its P until the runtime takes it back, and a small host has none to spare —
// blocking cost the pipeline up to 34 and 17 percent on two- and four-P hosts,
// and was measured to pay from eight Ps up. A host that runs more waiters than
// parkedWaiters keeps the blocking wait it always had.
const (
	parkBatch  = 16
	quietBatch = 4
	quietWaits = 50

	adaptiveMinProcs = 8
)

// waitMode is how a loop's waiters wait; see the quietBatch block.
type waitMode uint8

const (
	// waitAdaptive parks while the loop is busy and blocks once it goes
	// quiet; see loopWaitMode for where it is used.
	waitAdaptive waitMode = iota
	// waitPark always parks in the runtime's netpoller.
	waitPark
	// waitBlock always blocks in the kernel.
	waitBlock
)

// loopWaitMode is the wait for the waiters of loops loops, waiters each, on
// procs Ps: a host past parkedWaiters blocks as it always did, a loop's only
// waiter tunes its wait to its own load where the host has the Ps for it, and
// everything else parks.
func loopWaitMode(procs, loops, waiters int) waitMode {
	switch {
	case waiters*loops > parkedWaiters:
		return waitBlock
	case waiters == 1 && procs >= adaptiveMinProcs:
		return waitAdaptive
	default:
		return waitPark
	}
}

// flushEvery bounds how many readiness events a waiter folds before the
// connections collected so far are handed to the executor, so a connection at
// the head of a deep batch reaches running workers without waiting out the
// tail's checks. Steady-state batches never reach the mark.
const flushEvery = 16

// eventLoop is one poller and the goroutines that wait on it. Connections,
// stream listeners and UDP sockets are spread over the loops in turn; a
// loop's waiters collect their readiness, accept on its listeners, and hand
// runnable connections to the executor. A waiter never
// runs a callback and never waits for a connection: everything a connection
// does, its registration and its close included, happens in its own turn.
// The fd table is process-wide, so every loop resolves events through the
// same one.
type eventLoop struct {
	// Read-mostly state consulted by connection turns on every CPU.
	events      *Events
	poller      *poller.Poller
	fdMap       *fdmap.Map[fdConn]
	ioPool      *ioTaskPool
	ioPoolOwner bool
	stopping    atomic.Bool
	ioStop      atomic.Bool                 // set once shutdown refuses new turns
	listeners   atomic.Pointer[[]*listener] // stream listeners this loop accepts on

	_       [cacheLineSize]byte
	ioTurns [ioStripes]ioStripe // scheduled connection turns, summed over stripes

	ioIdle     chan struct{}
	ioIdleOnce sync.Once
	waiters    sync.WaitGroup

	// index is the loop's position among count loops; together they key the
	// turns of the loop's connections (see fdConn.turnKey). A loop made on its
	// own counts as the only one.
	index uint32
	count uint32
}

// loopWaiter is one waiter goroutine's private dispatch state.
type loopWaiter struct {
	batch poller.Batch
	evbuf []poller.Event
	ready []*fdConn
	args  []IOTask

	// block selects the blocking wait over the parked one, adaptive lets the
	// waiter's own load switch between them, and quiet counts the quiet
	// rounds in a row while it parks. See the quietBatch block.
	block    bool
	adaptive bool
	quiet    int

	// accepted counts the connections the waiter accepted, which go round
	// the loops in turn (see Events.loopAt); a count of its own keeps the
	// accepting waiters off one shared counter.
	accepted uint32
}

// noteLoad folds one round of events into the waiter's wait mode.
func (waiter *loopWaiter) noteLoad(events int) {
	switch {
	case events >= parkBatch:
		waiter.block, waiter.quiet = false, 0
	case events > quietBatch:
		waiter.quiet = 0
	case !waiter.block:
		if waiter.quiet++; waiter.quiet == quietWaits {
			waiter.block, waiter.quiet = true, 0
		}
	}
}

// newEventLoop creates one poller. Events shares one ioPool across all loops;
// tests and standalone loops may own one.
func newEventLoop(events *Events) (*eventLoop, error) {
	loopPoller, err := poller.New()
	if err != nil {
		return nil, err
	}
	ioPool := events.ioPool
	owner := false
	if ioPool == nil {
		ioPool = newIOTaskPool(events.Executor)
		owner = true
	}
	return &eventLoop{
		events:      events,
		poller:      loopPoller,
		fdMap:       newFdMap(),
		ioPool:      ioPool,
		ioPoolOwner: owner,
		ioIdle:      make(chan struct{}),
		count:       1,
	}, nil
}

// ioStripes spreads the turn count over that many cache lines. Every turn is
// counted when it is scheduled and uncounted when it ends, usually on two
// different goroutines; one shared counter moved between CPUs twice a turn.
// Each goroutine counts on the stripe its id picks, so the waiter that
// schedules a loop's turns and the workers that end them mostly write lines
// of their own. Only the sum means anything: a turn may be counted on one
// stripe and uncounted on another.
const ioStripes = 16

type ioStripe struct {
	turns atomic.Int64
	_     [cacheLineSize - 8]byte
}

func (loop *eventLoop) ioStripe() *atomic.Int64 {
	return &loop.ioTurns[currentGoroutineID()&(ioStripes-1)].turns
}

// ioTurnCount sums the stripes. Once ioStop is set no turn is counted any
// more, except by an acquireIO that is about to give its count back, so a sum
// of zero then means every turn has ended.
func (loop *eventLoop) ioTurnCount() int64 {
	var turns int64
	for i := range loop.ioTurns {
		turns += loop.ioTurns[i].turns.Load()
	}
	return turns
}

// acquireIO counts a turn before checking for shutdown, and stopIO sets ioStop
// before summing, so either the turn sees the stop or the stop sees the turn.
func (loop *eventLoop) acquireIO() bool {
	loop.ioStripe().Add(1)
	if loop.ioStop.Load() {
		loop.releaseIO()
		return false
	}
	return true
}

// Final close callbacks may be scheduled after the shutdown barrier. Their
// lifetime is joined by waitIODrained, rather than by the fd teardown barrier.
func (loop *eventLoop) acquireCloseIO() { loop.ioStripe().Add(1) }

// releaseIO uncounts a turn. After shutdown began, the release that brings the
// sum to zero wakes stopIO: each release sums only after its own decrement, so
// the last one to end sees every other's.
func (loop *eventLoop) releaseIO() {
	loop.ioStripe().Add(-1)
	if loop.ioStop.Load() && loop.ioTurnCount() == 0 {
		loop.ioIdleOnce.Do(func() { close(loop.ioIdle) })
	}
}

func (loop *eventLoop) ioStopped() bool { return loop.ioStop.Load() }

// waitIODrained joins every connection turn counted by this loop, including
// close callbacks scheduled after the shutdown barrier. It runs once per loop
// during shutdown, so a short polling backoff costs nothing on the data path.
func (loop *eventLoop) waitIODrained() {
	delay := 20 * time.Microsecond
	for loop.ioTurnCount() != 0 {
		time.Sleep(delay)
		if delay < 2*time.Millisecond {
			delay *= 2
		}
	}
}

// stopIO refuses new turns and waits for the running ones.
func (loop *eventLoop) stopIO() {
	if loop.ioIdle == nil {
		loop.ioIdle = make(chan struct{})
	}
	loop.ioStop.Store(true)
	if loop.ioTurnCount() == 0 {
		loop.ioIdleOnce.Do(func() { close(loop.ioIdle) })
	}
	<-loop.ioIdle
}

// newWaiter allocates one waiter's dispatch state.
func (loop *eventLoop) newWaiter() *loopWaiter {
	waiter := &loopWaiter{
		evbuf: make([]poller.Event, eventBatch),
		ready: make([]*fdConn, 0, eventBatch),
	}
	if loop.events.Executor != nil {
		// Only the external-executor path hands the batch over as an
		// []IOTask; the owned typed queue takes the connections directly.
		waiter.args = make([]IOTask, 0, eventBatch)
	}
	return waiter
}

// start runs count waiters on the poller, waiting as mode says.
func (loop *eventLoop) start(count int, mode waitMode, lockOSThread bool) {
	// A waiter yields unless it is the process's only one.
	yield := count > 1 || len(loop.events.loops) > 1
	for range count {
		waiter := loop.newWaiter()
		waiter.block = mode == waitBlock
		waiter.adaptive = mode == waitAdaptive
		loop.waiters.Add(1)
		go func() {
			defer loop.waiters.Done()
			if lockOSThread {
				runtime.LockOSThread()
				defer runtime.UnlockOSThread()
			}
			if testHookWaiterStarted != nil {
				testHookWaiterStarted(lockOSThread)
			}
			if err := loop.serve(waiter, yield); err != nil {
				loop.events.initiateClose(err)
			}
		}()
	}
}

// testHookWaiterStarted runs at the top of every waiter goroutine with the
// thread association it took; nil outside tests.
var testHookWaiterStarted func(locked bool)

// serve dispatches the loop's readiness until the poller is closed. A waiter
// waits as its mode says: adapted to its own load, parked, or blocked; see
// the quietBatch block.
//
// Unless it is the only waiter in the process, a waiter that handed tasks to
// the executor yields before it waits again. The executor's wake-ups queue the
// workers on this waiter's P, where they wait until it blocks or is preempted,
// while other Ps may sit idle; yielding runs them at once while the other
// waiters keep collecting. The only waiter of a process must not yield: it
// would queue behind the very workers it woke, with nobody collecting.
func (loop *eventLoop) serve(waiter *loopWaiter, yield bool) error {
	for {
		var n int
		var err error
		if waiter.block {
			n, err = loop.poller.WaitBatch(&waiter.batch, waiter.evbuf, -1)
		} else {
			n, err = loop.poller.WaitBatchParked(&waiter.batch, waiter.evbuf)
		}
		if loop.poller.Closed() {
			loop.submit(waiter)
			return nil
		}
		if err != nil {
			return err
		}
		if waiter.adaptive {
			waiter.noteLoad(n)
		}
		submitted := loop.dispatch(waiter, waiter.evbuf[:n])
		if loop.submit(waiter) {
			submitted = true
		}
		if submitted && yield {
			runtime.Gosched()
		}
	}
}

// dispatch folds readiness into the connections it names and accepts on the
// listeners it names, handing runnable connections over every flushEvery
// events. It reports whether anything was submitted before the trailing
// submit, so serve keeps yielding for the workers it woke.
//
// A connection's events carry the tag it was registered with where the
// backend reports one: its descriptor may have been closed, and the number
// reused for a new connection, after the poller returned an event for it, and
// the tag keeps such an event from reaching the new connection. Listeners are
// registered without a tag.
func (loop *eventLoop) dispatch(waiter *loopWaiter, events []poller.Event) bool {
	submitted := false
	countdown := flushEvery
	for _, event := range events {
		if event.Tag == 0 {
			if l := loop.listener(event.FD); l != nil {
				if err := loop.accept(l, waiter); err != nil {
					loop.events.initiateClose(err)
				}
				submitted = loop.submit(waiter) || submitted
				countdown = flushEvery
				continue
			}
		}
		conn := loop.fdMap.Get(event.FD)
		if conn != nil && (!poller.Tagged || conn.pollTag.Load() == event.Tag) &&
			!conn.isClosing() && !conn.skipsEdge(event.Events) && conn.noteIO(uint32(event.Events)) {
			waiter.ready = append(waiter.ready, conn)
		}
		// Every checked event counts: a batch of stale or filtered events
		// costs the same lookups as a live one, and the interval bounds how
		// long the head of the batch waits behind the tail, not how many
		// connections were collected.
		if countdown--; countdown == 0 {
			countdown = flushEvery
			if loop.submit(waiter) {
				submitted = true
			}
		}
	}
	return submitted
}

func (loop *eventLoop) submit(waiter *loopWaiter) bool {
	if len(waiter.ready) == 0 {
		return false
	}
	connections := waiter.ready
	waiter.ready = waiter.ready[:0]
	if !loop.ioPool.submitConnBatch(connections, waiter.args) {
		for _, conn := range connections {
			conn.handleIOSubmitFailure(net.ErrClosed)
		}
	}
	waiter.args = waiter.args[:0]
	clear(connections)
	return true
}

// listener returns the stream listener this loop accepts on through fd.
func (loop *eventLoop) listener(fd int) *listener {
	listeners := loop.listeners.Load()
	if listeners == nil {
		return nil
	}
	for _, l := range *listeners {
		if l.fd == fd {
			return l
		}
	}
	return nil
}

// addListener starts accepting on l. Listener readiness is level-triggered,
// so a backlog left after one bounded batch is reported again.
func (loop *eventLoop) addListener(l *listener) error {
	var next []*listener
	if current := loop.listeners.Load(); current != nil {
		next = append(next, *current...)
	}
	next = append(next, l)
	loop.listeners.Store(&next)
	return loop.poller.Add(l.fd, poller.Readable)
}

// accept takes a bounded batch of connections from l and queues each one's
// first turn on the waiter, which hands them over with the readiness it
// collected. The bound keeps a busy listener from holding up the other
// events of this loop for long; the level-triggered listener reports the rest
// again.
func (loop *eventLoop) accept(l *listener, waiter *loopWaiter) error {
	for range acceptBatchSize {
		nfd, sa, err := socket.Accept(l.fd)
		if err != nil {
			if isWouldBlock(err) {
				return nil
			}
			return err
		}
		conn := &fdConn{}
		conn.fd = int32(nfd)
		conn.events = loop.events
		conn.loop = loop.events.loopAt(waiter.accepted)
		waiter.accepted++
		conn.addr = l.pair
		if l.tcp {
			conn.remoteAddr = socket.SockaddrToAddrPort(sa)
		} else {
			// Unix peers keep their address object; only IP peers have a
			// value form.
			conn.setRemoteAddr(socket.SockaddrToAddr(sa, false))
		}
		if conn.admitAccepted(l.tcp) {
			waiter.ready = append(waiter.ready, conn)
		}
	}
	return nil
}

// registerConn watches a dialed or adopted connection on the caller's
// goroutine and submits its first turn, which runs OnOpen. The turn is claimed
// before the connection is published in the fd table, so readiness the poller
// reports from then on, stale or not, folds into that turn, which runs OnOpen
// before it reads; and until the turn is submitted nothing but this call
// touches the connection, so a failure closes it here.
func (loop *eventLoop) registerConn(conn *fdConn) error {
	fd := conn.Fd()
	conn.taskState.Store(ioEventOpen)
	if !conn.noteIO(0) {
		conn.closeUnregistered()
		return net.ErrClosed
	}
	if err := loop.fdMap.Put(fd, conn); err != nil {
		conn.abandonClaim()
		conn.closeUnregistered()
		return err
	}
	// The shutdown pass marks every connection in the table. A connection
	// published after it looked would be missed, so one that finds the loop
	// stopping withdraws; the turn it holds keeps the pass waiting until then.
	if loop.stopping.Load() {
		loop.fdMap.DeleteValue(fd, conn)
		conn.closeUnregistered()
		conn.abandonClaim()
		return net.ErrClosed
	}
	if err := conn.watch(); err != nil {
		loop.fdMap.DeleteValue(fd, conn)
		conn.closeUnregistered()
		conn.abandonClaim()
		return err
	}
	loop.ioPool.connOpened()
	if registeredForTest != nil {
		registeredForTest(conn)
	}
	if !loop.ioPool.submit(conn) {
		conn.handleIOSubmitFailure(net.ErrClosed)
	}
	return nil
}

// registeredForTest, when a test sets it, runs after a dialed connection is
// added to its poller and before its claimed open turn is submitted.
var registeredForTest func(*fdConn)

// registerListener watches a UDP listener's server connection. It has no
// OnOpen; its turns read every peer's datagrams.
func (loop *eventLoop) registerListener(server *fdConn) error {
	if err := loop.fdMap.Put(int(server.fd), server); err != nil {
		return err
	}
	if err := server.watch(); err != nil {
		loop.fdMap.DeleteValue(int(server.fd), server)
		return err
	}
	return nil
}

// forget takes conn out of the fd table before its descriptor closes. A
// stream's descriptor is the only reference to its socket — accepted, or
// duplicated by Dial or Adopt, which close the original — so closing it takes
// it out of the poller, and an explicit removal would only add a syscall per
// connection. A UDP listener's descriptor shares its socket with the Go
// listener that is closed later, so it is removed explicitly.
func (loop *eventLoop) forget(conn *fdConn) {
	loop.fdMap.DeleteValue(conn.Fd(), conn)
	if conn.isDatagram() {
		_ = loop.poller.Remove(conn.Fd(), conn.currentInterest())
	} else {
		loop.ioPool.connClosed()
	}
}

// shutdown releases every connection of this loop once the poller and its
// waiters have stopped. It marks them closing so running turns stop working,
// waits for those turns, and then releases each connection itself; OnClose
// is still delivered by a final turn.
func (loop *eventLoop) shutdown(err error) {
	for _, conn := range loop.fdMap.Range() {
		if conn.loop == loop {
			conn.beginShutdown()
		}
	}
	loop.stopIO()
	for _, conn := range loop.fdMap.Range() {
		if conn.loop == loop {
			if finalErr, ok := conn.teardown(err); ok {
				conn.scheduleCloseCallback(finalErr)
			}
		}
	}
	if loop.ioPoolOwner {
		loop.waitIODrained()
		loop.ioPool.stop()
	}
}

// closePoller stops the loop's waiters and waits for them to return.
func (loop *eventLoop) closePoller(err error) {
	_ = loop.poller.Close(err)
	loop.waiters.Wait()
}
