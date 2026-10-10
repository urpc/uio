//go:build (linux || darwin || netbsd || freebsd || openbsd || dragonfly) && !stdio

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

// flushEvery bounds how many readiness events a waiter folds before the
// connections collected so far are handed to the executor, so a connection at
// the head of a deep batch reaches running workers without waiting out the
// tail's checks. Steady-state batches never reach the mark.
const flushEvery = 16

// eventLoop is one poller and the goroutines that wait on it. Connections,
// stream listeners and UDP sockets are registered with the loop their
// descriptor picks; its waiters collect their readiness, accept on its
// listeners, and hand runnable connections to the executor. A waiter never
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
	listeners   atomic.Pointer[[]*listener] // stream listeners this loop accepts on

	_       [cacheLineSize]byte
	ioState atomic.Uint64 // stop bit plus scheduled connection-turn count
	_       [cacheLineSize - 8]byte

	ioIdle     chan struct{}
	ioIdleOnce sync.Once
	waiters    sync.WaitGroup
}

// loopWaiter is one waiter goroutine's private dispatch state.
type loopWaiter struct {
	batch poller.Batch
	evbuf []poller.Event
	ready []*fdConn
	args  []IOTask
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
	}, nil
}

func (loop *eventLoop) acquireIO() bool {
	// One add rather than a load and compare-and-swap: this line moves
	// between CPUs on every turn, and a failed CAS would move it again.
	if loop.ioState.Add(1)&ioStopBit != 0 {
		loop.releaseIO()
		return false
	}
	return true
}

// Final close callbacks may be scheduled after the shutdown barrier. Their
// lifetime is joined by waitIODrained, rather than by the fd teardown barrier.
func (loop *eventLoop) acquireCloseIO() { loop.ioState.Add(1) }

func (loop *eventLoop) releaseIO() {
	if loop.ioState.Add(^uint64(0)) == ioStopBit {
		loop.ioIdleOnce.Do(func() { close(loop.ioIdle) })
	}
}

func (loop *eventLoop) ioStopped() bool { return loop.ioState.Load()&ioStopBit != 0 }

// waitIODrained joins every connection turn counted by this loop, including
// close callbacks scheduled after the shutdown barrier. It runs once per loop
// during shutdown, so a short polling backoff costs nothing on the data path.
func (loop *eventLoop) waitIODrained() {
	delay := 20 * time.Microsecond
	for loop.ioState.Load()&^ioStopBit != 0 {
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
	for {
		state := loop.ioState.Load()
		if state&ioStopBit != 0 {
			break
		}
		if loop.ioState.CompareAndSwap(state, state|ioStopBit) {
			if state == 0 {
				loop.ioIdleOnce.Do(func() { close(loop.ioIdle) })
			}
			break
		}
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

// start runs count waiters on the poller.
func (loop *eventLoop) start(count int, parked, lockOSThread bool) {
	for range count {
		waiter := loop.newWaiter()
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
			if err := loop.serve(waiter, count > 1, parked); err != nil {
				loop.events.initiateClose(err)
			}
		}()
	}
}

// testHookWaiterStarted runs at the top of every waiter goroutine with the
// thread association it took; nil outside tests.
var testHookWaiterStarted func(locked bool)

// serve dispatches the loop's readiness until the poller is closed.
//
// With more than one waiter on the loop, a waiter that handed tasks to the
// executor yields before it waits again. The executor's wake-ups queue the
// workers on this waiter's P, and a wait in the kernel would keep that P in a
// system call until the runtime retakes it; yielding runs them at once while
// another waiter keeps watching the poller. A lone waiter must not yield: it
// would queue behind the very workers it woke.
func (loop *eventLoop) serve(waiter *loopWaiter, yield, parked bool) error {
	for {
		var n int
		var err error
		if parked {
			n, err = loop.poller.WaitBatchParked(&waiter.batch, waiter.evbuf)
		} else {
			n, err = loop.poller.WaitBatch(&waiter.batch, waiter.evbuf, -1)
		}
		if loop.poller.Closed() {
			loop.submit(waiter)
			return nil
		}
		if err != nil {
			return err
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
		conn.fd = nfd
		conn.events = loop.events
		conn.loop = loop.events.selectLoop(nfd)
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
	if err := loop.fdMap.Put(server.fd, server); err != nil {
		return err
	}
	if err := server.watch(); err != nil {
		loop.fdMap.DeleteValue(server.fd, server)
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
