package uio

import (
	"net"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/urpc/uio/internal/fdmap"
	"github.com/urpc/uio/internal/poller"
	"github.com/urpc/uio/internal/taskqueue"
)

const (
	taskBudget              = 256
	eventBatch              = 1024
	defaultTCPKeepAliveSecs = 15
	ioStopBit               = uint64(1 << 63)
)

var unixFdMap *fdmap.Map[fdConn]
var unixFdMapOnce sync.Once

func newFdMap() *fdmap.Map[fdConn] {
	// Unix can index directly by fd, so all loops share one sparse table.
	// Windows falls back to a typed, mutex-protected map per loop.
	if fdmap.UseSingleInstance {
		unixFdMapOnce.Do(func() { unixFdMap = fdmap.NewMap[fdConn]() })
		return unixFdMap
	}
	return fdmap.NewMap[fdConn]()
}

// cacheLineSize separates fields that every connection turn writes from the
// read-mostly fields next to them, so reading the latter does not keep
// missing on a line other CPUs just modified.
const cacheLineSize = 64

// eventLoop is the sole owner of poller registration, descriptor teardown,
// socket options, deadlines, and interest changes for its connections. Native
// stream I/O and callbacks run in ioPool tasks and communicate back through the
// MPSC task queue; they never call epoll/kqueue control operations directly.
type eventLoop struct {
	// Read-mostly state consulted by connection turns on every CPU.
	events      *Events
	poller      *poller.NetPoller
	fdMap       *fdmap.Map[fdConn]
	tasks       *taskqueue.Queue[*task] // public MPSC queue
	ioPool      *ioTaskPool
	ioPoolOwner bool
	stopping    atomic.Bool
	loopGoid    atomic.Int64

	_       [cacheLineSize]byte
	ioState atomic.Uint64 // stop bit plus scheduled connection-turn count
	_       [cacheLineSize - 8]byte

	wakePending atomic.Bool // coalesces producer wakeups

	// Loop-owned.
	yield       bool // hand the P to just-woken workers; see Serve
	buffer      []byte
	evbuf       []poller.Event
	ioIdle      chan struct{}
	ioIdleOnce  sync.Once
	taskBatch   *taskqueue.Node[*task] // private FIFO remainder owned by this loop
	ioReady     []*fdConn              // newly runnable connections in this poll batch
	ioReadyArgs []IOTask               // scratch only when an external Executor is used
	stopErr     error
}

// newEventLoop allocates one poller and its reusable batches. Events normally
// shares one ioPool across all loops; tests and standalone loops may own one.
func newEventLoop(events *Events) (*eventLoop, error) {
	netPoller, err := poller.NewNetPoller()
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
		poller:      netPoller,
		ioPool:      ioPool,
		ioPoolOwner: owner,
		yield:       events.Pollers > 1,
		buffer:      make([]byte, events.MaxBufferSize),
		fdMap:       newFdMap(),
		evbuf:       make([]poller.Event, eventBatch),
		tasks:       taskqueue.New[*task](),
		ioReady:     make([]*fdConn, 0, eventBatch),
		ioIdle:      make(chan struct{}),
	}
	if events.Executor != nil {
		// Only the external-executor path hands the readiness batch over as
		// an []IOTask; the owned typed queue takes the connections directly.
		loop.ioReadyArgs = make([]IOTask, 0, eventBatch)
	}
	return loop, nil
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

// inLoop prevents synchronous control methods from waiting on their own queue.
// User stream callbacks run in connection tasks; UDP callbacks still run here.
func (loop *eventLoop) inLoop() bool {
	id := loop.loopGoid.Load()
	return id != 0 && id == currentGoroutineID()
}

func (loop *eventLoop) pushTask(t *task) bool {
	return loop.tasks.Push(&t.node)
}

func (loop *eventLoop) submitTask(t *task) bool {
	if !loop.pushTask(t) {
		return false
	}
	loop.notify()
	return true
}

func (loop *eventLoop) notify() {
	// One unread wake is enough regardless of how many tasks were submitted.
	if loop.wakePending.CompareAndSwap(false, true) {
		_ = loop.poller.Wake()
	}
}

func (loop *eventLoop) beginStop(err error) {
	t := acquireTask(stopTask, nil)
	t.err = err
	// Stop atomically rejects future pushes and appends behind accepted work.
	if !loop.tasks.Stop(&t.node) {
		releaseTask(t)
		return
	}
	loop.notify()
}

func (loop *eventLoop) hasPendingTasks() bool {
	return loop.taskBatch != nil || loop.tasks.HasPending()
}

func (loop *eventLoop) runTasks(limit int) {
	processed := 0
	for processed < limit {
		// Finish the private remainder before draining newer public tasks.
		if loop.taskBatch == nil {
			loop.taskBatch = loop.tasks.Drain()
			if loop.taskBatch == nil {
				break
			}
		}
		node := loop.taskBatch
		loop.taskBatch = node.TakeNext()
		loop.runTask(node.Value)
		processed++
	}
}

// runTask applies one control-plane command on the loop goroutine. Data-plane
// readiness is deliberately absent: it is folded into fdConn.taskState and
// submitted to ioPool after the entire poll batch has been collected.
func (loop *eventLoop) runTask(t *task) {
	done := t.done
	udpDone := t.udpDone
	var result error
	var datagram udpWriteResult
	switch t.kind {
	case closeTask:
		t.conn.closeOnLoop(t.err)
	case wakeTask:
		result = t.conn.runWakeTask()
		if result != nil {
			t.conn.requestClose(result)
		}
	case registerTask:
		result = loop.runRegisterTask(t)
	case optionTask:
		result = t.conn.applySocketOption(t.optionKind, t.optionValue)
	case deadlineTask:
		result = t.conn.applyDeadline(t.deadlineKind, t.deadline)
	case timeoutTask:
		t.conn.handleTimeout(t.deadlineKind, t.generation)
	case stopTask:
		loop.stopErr = t.err
		loop.stopping.Store(true)
	case udpWriteTask:
		datagram = t.conn.runUDPWriteTask(t.udpPayload)
	}
	releaseTask(t)
	if done != nil {
		done <- result
	}
	if udpDone != nil {
		udpDone <- datagram
	}
}

func (loop *eventLoop) runRegisterTask(t *task) error {
	if t.acceptedTCP {
		t.conn.prepareAccepted()
	}
	request := t.registration
	if request == nil {
		return loop.registerConn(t.conn)
	}
	if request.state.Load() == registerCanceled {
		t.conn.closeUnregistered()
		return request.cause()
	}
	result := loop.registerConn(t.conn)
	if request.state.CompareAndSwap(registerPending, registerCompleted) {
		return result
	}
	// The caller returned after canceling while registration was in progress.
	if result == nil {
		t.conn.closeOnLoop(request.cause())
	}
	return request.cause()
}

// Serve alternates bounded command draining with poller waits. Clearing
// wakePending before the second queue check closes the classic lost-wakeup
// race between a producer enqueue and the loop entering a blocking wait.
func (loop *eventLoop) Serve(lockOSThread bool, handler poller.EventHandler) (result error) {
	if lockOSThread {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
	}
	if handler == nil {
		handler = loop
	}
	owner := currentGoroutineID()
	loop.loopGoid.Store(owner)
	activeEventLoops.Store(owner, struct{}{})
	defer func() {
		activeEventLoops.Delete(owner)
		loop.loopGoid.Store(0)
	}()

	for !loop.stopping.Load() {
		loop.runTasks(taskBudget)
		if loop.stopping.Load() {
			break
		}
		timeout := -1
		if loop.hasPendingTasks() {
			timeout = 0
		} else {
			// Clear before the second queue check to close the lost-wakeup race.
			loop.wakePending.Store(false)
			if loop.hasPendingTasks() {
				continue
			}
		}

		n, err := loop.poller.Wait(loop.evbuf, timeout)
		if err != nil || loop.poller.Closed() {
			result = err
			loop.beginStop(err)
			continue
		}
		// Dispatch the complete batch. Close requests only enqueue closeTask.
		for _, event := range loop.evbuf[:n] {
			handler.OnEvent(loop.poller, event.FD, event.Events)
		}
		if loop.submitIOReady() && loop.yield {
			// As the shared data poller's waiters do: run the workers just
			// woken on this P now instead of holding it in the next wait,
			// while the other loops keep watching their connections.
			runtime.Gosched()
		}
	}

	if result == nil {
		result = loop.stopErr
	}
	loop.shutdown(result)
	if handler != loop {
		handler.OnClose(loop.poller, result)
	}
	_ = loop.poller.Close(result)
	return result
}

// submitIOReady hands one readiness batch to the connection scheduler. The
// slice is loop-owned and immediately reused, so external batch executors must
// copy any task references retained after SubmitBatch returns.
func (loop *eventLoop) submitIOReady() bool {
	if len(loop.ioReady) == 0 {
		return false
	}
	connections := loop.ioReady
	loop.ioReady = loop.ioReady[:0]
	if !loop.ioPool.submitConnBatch(connections, loop.ioReadyArgs) {
		for _, conn := range connections {
			conn.handleIOSubmitFailure(net.ErrClosed)
		}
	}
	loop.ioReadyArgs = loop.ioReadyArgs[:0]
	clear(connections)
	return true
}

func (loop *eventLoop) shutdown(err error) {
	// Stop has sealed the control queue. Prevent new connection turns, then
	// wait for current tasks to return ownership before touching loop-owned fd
	// registration and UDP peer maps.
	for _, conn := range loop.fdMap.Range() {
		if conn.loop == loop {
			conn.beginShutdown()
		}
	}
	loop.stopIO()
	// fdMap is shared on Unix, so only close entries owned by this loop.
	for fd, conn := range loop.fdMap.Range() {
		if conn.loop == loop {
			loop.fdMap.Delete(fd)
			conn.closeOnLoop(err)
		}
	}
	if loop.ioPoolOwner {
		loop.waitIODrained()
		loop.ioPool.stop()
	}
}

// OnEvent folds stream readiness into the connection's single scheduled task.
// UDP remains on-loop because its peer map and shared listener socket require
// one owner.
func (loop *eventLoop) OnEvent(_ *poller.NetPoller, fd int, events poller.Events) {
	conn := loop.getConn(fd)
	if conn == nil || conn.isClosing() {
		return
	}
	// UDP peer management remains loop-owned until the stream task model is
	// complete for datagrams; do not route shared listener state through the TCP
	// connection worker.
	if conn.isDatagram() {
		if events&poller.ReadEvents != 0 {
			if err := conn.fireReadEvent(); err != nil {
				conn.requestClose(err)
			}
		}
		return
	}
	if conn.skipsEdge(events) {
		return
	}
	if conn.noteIO(uint32(events)) {
		loop.ioReady = append(loop.ioReady, conn)
	}
}

// OnClose releases connections when eventLoop is used through poller.Serve.
func (loop *eventLoop) OnClose(_ *poller.NetPoller, err error) { loop.shutdown(err) }

func (loop *eventLoop) getBuffer() []byte      { return loop.buffer }
func (loop *eventLoop) getConn(fd int) *fdConn { return loop.fdMap.Get(fd) }
func (loop *eventLoop) listen(fd int) error    { return loop.poller.Add(fd, poller.Readable) }
func (loop *eventLoop) delConn(conn *fdConn) {
	loop.fdMap.Delete(conn.Fd())
	if data := loop.events.data; data != nil && !conn.isDatagram() {
		data.unregister(conn.Fd())
	}
	_ = conn.watcher().Remove(conn.Fd(), conn.currentInterest())
}

// registeredForTest, when a test sets it, runs on the loop after a connection
// is added to its poller and before the loop schedules its open event.
var registeredForTest func(*fdConn)

// registerConn publishes the fd before poller registration so every delivered
// event can resolve it. Failure unwinds both publications before closing the fd.
func (loop *eventLoop) registerConn(conn *fdConn) error {
	// Publish before Watch so every delivered event can resolve the fd.
	fd := conn.Fd()
	watcher := conn.watcher()
	tag := conn.assignWatchTag()
	if err := loop.fdMap.Put(fd, conn); err != nil {
		conn.closeUnregistered()
		return err
	}
	interest := conn.initialInterest()
	if !conn.isDatagram() {
		conn.markOpenPending()
	}
	if err := watcher.Register(fd, interest, !conn.isDatagram(), tag); err != nil {
		conn.clearOpenPending()
		loop.fdMap.Delete(fd)
		_ = watcher.Remove(fd, interest)
		conn.closeUnregistered()
		return err
	}
	if data := loop.events.data; data != nil && !conn.isDatagram() {
		if err := data.register(conn); err != nil {
			conn.clearOpenPending()
			loop.fdMap.Delete(fd)
			_ = watcher.Remove(fd, interest)
			conn.closeUnregistered()
			return err
		}
	}
	if registeredForTest != nil {
		registeredForTest(conn)
	}
	// Datagram callbacks and shared socket state stay on the loop. Stream
	// callbacks run in their serialized connection task. Stdio's scheduleOpen
	// starts its dedicated blocking I/O loops after OnOpen below.
	if conn.isDatagram() {
		conn.fireOnOpen()
	} else {
		conn.scheduleOpen()
	}
	if !conn.isClosing() {
		conn.afterRegister()
	}
	return nil
}
