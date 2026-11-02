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
	"github.com/urpc/uio/internal/timewheel"
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
	if procs >= wideHostProcs {
		// A wide host's loops each run one waiter: a second one on the
		// same poller split its batches and cost echo 5-10% at 48 Ps.
		return 1
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

// wideHostProcs is the smallest GOMAXPROCS treated as a wide host. There a
// loop runs one waiter that blocks in the kernel, every loop accepts on a
// listener of its own, and connections close on a goroutine of their loop
// (see eventLoop.deferClose). Measured on a 96-core host at 48 Ps, against
// the parked or adaptive wait the narrower sizing picks: echo +30-40% and
// churn +35%. At 40 Ps blocking still won (echo at 512 connections +18%,
// churn +31%); at 32 the parked wait kept a small lead on echo, and at 24 it
// led by 4-6%, as on the 24-P host it was tuned on.
const wideHostProcs = 33

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
	case procs >= wideHostProcs, waiters*loops > parkedWaiters:
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
// loop's waiters collect their readiness, hand its listeners' to the loop's
// acceptRunner, and hand runnable connections to the executor. A waiter never
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

	// wakeMu guards the wake wheel's lazy construction and, on a loop whose
	// poller has no kernel timer, the runtime fallback timer; wake holds the
	// wheel once a connection of this loop has armed a wake timer. See
	// wake_unix.go.
	wakeMu    sync.Mutex
	wake      *timewheel.Wheel[*fdConn]
	wakeTimer *time.Timer

	// UDP lifetime ownership starts before poller registration. Keeping it
	// separate from fdMap also keeps stale untagged kqueue events from
	// reaching a listener before OnStart returns. Only create/close use it.
	udpMu      sync.Mutex
	udpServers map[*fdConn]struct{}

	// accepts accepts on the loop's listeners. closer closes the
	// descriptors its turns release where its waiters block in the kernel,
	// and is nil elsewhere; see deferClose.
	accepts *acceptRunner
	closer  *loopCloser
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

	// rescueIn counts down the rounds to the next look for stalled turn
	// queues; see ioTaskPool.rescue.
	rescueIn int

	// accepted counts the connections an accept runner's scratch accepted,
	// which go round the loops in turn (see Events.loopAt); a count of its
	// own keeps the runners off one shared counter.
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
	loop := &eventLoop{
		events:      events,
		poller:      loopPoller,
		fdMap:       newFdMap(),
		ioPool:      ioPool,
		ioPoolOwner: owner,
		ioIdle:      make(chan struct{}),
		count:       1,
	}
	// The wake timer's expiry sink before any waiter can wait, so an expiry
	// that lands first thing still reaches the loop.
	loopPoller.SetTimerSink(loop.expireWakeTimers)
	return loop, nil
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
	loop.accepts = newAcceptRunner(loop)
	if mode == waitBlock {
		loop.closer = newLoopCloser()
	}
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
		if waiter.rescueIn--; waiter.rescueIn <= 0 {
			waiter.rescueIn = rescueRounds
			if loop.ioPool.rescue() {
				submitted = true
			}
		}
		if submitted && yield {
			runtime.Gosched()
		}
	}
}

// dispatch folds readiness into the connections it names and hands the
// listeners it names to the accept runner, handing runnable connections over
// every flushEvery events. It reports whether anything was submitted before
// the trailing submit, or the runner woken, so serve yields for them.
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
				if loop.accepts.handOff(l) {
					// Yielding runs the runner at once.
					submitted = true
				}
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

// addListener starts accepting on l.
func (loop *eventLoop) addListener(l *listener) error {
	var next []*listener
	if current := loop.listeners.Load(); current != nil {
		next = append(next, *current...)
	}
	next = append(next, l)
	loop.listeners.Store(&next)
	if err := loop.watchListener(l); err != nil {
		loop.removeListener(l)
		return err
	}
	return nil
}

// watchListener registers l with the poller, edge-triggered: the accept
// runner accepts until the socket would block.
func (loop *eventLoop) watchListener(l *listener) error {
	return loop.poller.Register(l.fd, poller.Readable, true, 0)
}

// removeListener stops accepting on l. Its descriptor is a duplicate of the
// Go listener's, so the poller would keep watching the socket after it
// closes; it is removed explicitly. Waiters that looked l up already are
// waited out by listener.retire.
func (loop *eventLoop) removeListener(l *listener) {
	var next []*listener
	if current := loop.listeners.Load(); current != nil {
		for _, other := range *current {
			if other != l {
				next = append(next, other)
			}
		}
	}
	loop.listeners.Store(&next)
	_ = loop.poller.Remove(l.fd, poller.Readable)
}

// minAcceptPause and maxAcceptPause bound the pause after an accept that ran
// out of descriptors or memory.
const (
	minAcceptPause = 5 * time.Millisecond
	maxAcceptPause = time.Second
)

// pauseAccept stops watching l after an accept ran out of something the next
// connection needs as well, descriptors or memory: the level-triggered
// listener would report the waiting connection again at once and spin its
// waiters. It watches l again after a pause that doubles while the shortage
// lasts; the other listeners and connections carry on meanwhile.
func (loop *eventLoop) pauseAccept(l *listener) {
	if !l.acceptPaused.CompareAndSwap(false, true) {
		return
	}
	delay := time.Duration(l.acceptDelay.Load())
	if delay == 0 {
		delay = minAcceptPause
	} else {
		delay = min(2*delay, maxAcceptPause)
	}
	l.acceptDelay.Store(int64(delay))
	_ = loop.poller.Remove(l.fd, poller.Readable)
	time.AfterFunc(delay, func() { loop.resumeAccept(l) })
}

// resumeAccept watches a paused listener again, unless it is closing or has
// left the loop. A Listener.Close that overtakes this call removes l from the
// poller after it, or closes the descriptor, which drops the registration
// with the socket; and its retire waits for this call to leave.
func (loop *eventLoop) resumeAccept(l *listener) {
	if !l.enter() {
		return
	}
	defer l.leave()
	l.acceptPaused.Store(false)
	if loop.listener(l.fd) != l {
		return
	}
	_ = loop.watchListener(l)
	// Take the backlog now rather than wait for its edge: an accept that
	// paused again while this pause ran out may have left l registered, and
	// then registering it reports nothing.
	if loop.accepts != nil {
		loop.accepts.handOff(l)
	}
}

// accept takes a bounded batch of connections from l for the accept runner
// and queues each one's first turn on scratch, which the runner hands over
// before the next batch, so the first connections of a burst do not wait for
// its last. more reports a batch that ended at the bound rather than at a
// socket that would block, a pause or a closing listener.
func (loop *eventLoop) accept(l *listener, scratch *loopWaiter) (more bool, err error) {
	if !l.enter() {
		return false, nil // closing: its descriptor may close once nobody accepts
	}
	defer l.leave()
	for range acceptBatchSize {
		nfd, peer, sa, err := socket.AcceptPeer(l.fd, l.tcp)
		if err != nil {
			if isWouldBlock(err) {
				return false, nil
			}
			switch socket.ClassifyAcceptError(err) {
			case socket.AcceptRetry:
				continue
			case socket.AcceptBackoff:
				loop.pauseAccept(l)
				return false, nil
			}
			return false, err
		}
		if l.acceptDelay.Load() != 0 {
			l.acceptDelay.Store(0)
		}
		conn := &fdConn{}
		conn.entry = l.entry
		conn.fd = int32(nfd)
		if !l.controlled {
			conn.flags.Store(offTurnCloseFlag)
		}
		if l.acceptLocal {
			conn.loop = loop
		} else {
			conn.loop = loop.events.loopAt(scratch.accepted)
			scratch.accepted++
		}
		conn.addr = l.pair
		if l.userdataBox != nil {
			conn.userdata.Store(l.userdataBox)
		}
		if l.tcp {
			conn.remoteAddr = peer
		} else {
			// Unix peers keep their address object; only IP peers have a
			// value form.
			conn.setRemoteAddr(socket.SockaddrToAddr(sa, false))
		}
		if conn.admitAccepted(l.tcp) {
			scratch.ready = append(scratch.ready, conn)
		}
	}
	return true, nil
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
		// The first turn never ran, so the connection has no callbacks to
		// deliver and none ever will: report the refusal rather than hand
		// the caller a connection whose end nothing will tell.
		conn.handleIOSubmitFailure(net.ErrClosed)
		return ErrFirstTurnRefused
	}
	return nil
}

// registeredForTest, when a test sets it, runs after a dialed connection is
// added to its poller and before its claimed open turn is submitted.
var registeredForTest func(*fdConn)

// trackListener owns a UDP server from its creation, before accepting starts
// or DialPeer can publish children. The reservation joins a concurrent
// shutdown, just as registration does for dialed connections.
func (loop *eventLoop) trackListener(server *fdConn) error {
	if !loop.acquireIO() {
		server.closeUnregistered()
		return net.ErrClosed
	}
	defer loop.releaseIO()
	loop.udpMu.Lock()
	defer loop.udpMu.Unlock()
	if loop.stopping.Load() {
		server.closeUnregistered()
		return net.ErrClosed
	}
	if loop.udpServers == nil {
		loop.udpServers = make(map[*fdConn]struct{})
	}
	loop.udpServers[server] = struct{}{}
	return nil
}

// registerListener starts receiving for an already owned UDP server. Its
// descriptor may have been closed by a turn during OnStart, so watching it
// uses the same descriptor guard as socket options and teardown.
func (loop *eventLoop) registerListener(server *fdConn) error {
	server.submitMu.Lock()
	defer server.submitMu.Unlock()
	if server.isClosing() || loop.stopping.Load() {
		return net.ErrClosed
	}
	if !server.udp.gate.enter() {
		return net.ErrClosed
	}
	defer server.udp.gate.leave()
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
// stream's descriptor normally owns its socket alone, so closing it removes
// it from the poller without an extra syscall. Connections used by external
// Control calls need explicit removal, since an alias may still be open.
// A dialed UDP socket is removed explicitly too. A UDP listener's server
// leaves both in releaseSocket, which may already have run on the
// Listener.Close caller: its descriptor number may belong to another socket
// by now, so here it only leaves the registry of servers.
func (loop *eventLoop) forget(conn *fdConn) {
	if conn.internal {
		loop.udpMu.Lock()
		delete(loop.udpServers, conn)
		loop.udpMu.Unlock()
		return
	}
	loop.fdMap.DeleteValue(conn.Fd(), conn)
	if conn.isDatagram() {
		_ = loop.poller.Remove(conn.Fd(), conn.currentInterest())
	} else {
		if conn.flags.Load()&controlAliasFlag != 0 {
			_ = loop.poller.Remove(conn.Fd(), conn.currentInterest())
		}
		loop.ioPool.connClosed()
	}
}

// shutdown releases every connection of this loop once the poller and its
// waiters have stopped. It marks them closing so running turns stop working,
// waits for those turns, and then releases each connection itself; OnClose
// is still delivered by a final turn.
func (loop *eventLoop) shutdown(err error) {
	// The fallback wake timer, when a loop ran on one, stops here: entries
	// the connections' own release paths cancel cover the rest.
	loop.wakeMu.Lock()
	if loop.wakeTimer != nil {
		loop.wakeTimer.Stop()
		loop.wakeTimer = nil
	}
	loop.wakeMu.Unlock()
	loop.udpMu.Lock()
	for server := range loop.udpServers {
		server.beginShutdown()
	}
	loop.udpMu.Unlock()
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
	// Registered servers have already left this registry via forget. Take
	// the unregistered ones after joining every turn, and release with no
	// registry lock held: their callbacks may close another listener.
	loop.udpMu.Lock()
	servers := loop.udpServers
	loop.udpServers = nil
	loop.udpMu.Unlock()
	for server := range servers {
		if finalErr, ok := server.teardown(err); ok {
			server.scheduleCloseCallback(finalErr)
		}
	}
	if loop.ioPoolOwner {
		loop.waitIODrained()
		loop.ioPool.stop()
	}
}

// closePoller stops the loop's waiters and waits for them to return, and
// closes the descriptors its closer still holds; turns that release a
// connection from then on close its descriptor themselves.
func (loop *eventLoop) closePoller(err error) {
	_ = loop.poller.Close(err)
	loop.waiters.Wait()
	if loop.accepts != nil {
		loop.accepts.stop()
	}
	if loop.closer != nil {
		loop.closer.stop()
	}
}
