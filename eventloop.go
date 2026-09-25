package uio

import (
	"net"
	"runtime"
	"sync"
	"sync/atomic"

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

// eventLoop is the sole owner of poller registration, descriptor teardown,
// socket options, deadlines, and interest changes for its connections. Native
// stream I/O and callbacks run in ioPool tasks and communicate back through the
// MPSC task queue; they never call epoll/kqueue control operations directly.
type eventLoop struct {
	events *Events
	poller *poller.NetPoller
	buffer []byte
	fdMap  *fdmap.Map[fdConn]
	evbuf  []poller.Event

	tasks       *taskqueue.Queue[*task] // public MPSC queue
	ioPool      *ioTaskPool
	ioPoolOwner bool
	ioState     atomic.Uint64 // stop bit plus scheduled connection-turn count
	ioIdle      chan struct{}
	ioIdleOnce  sync.Once
	taskBatch   *taskqueue.Node[*task] // private FIFO remainder owned by this loop
	ioReady     []*fdConn              // newly runnable connections in this poll batch
	ioReadyArgs []IOTask               // scratch only when an external Executor is used
	wakePending atomic.Bool            // coalesces producer wakeups
	stopping    atomic.Bool
	loopGoid    atomic.Int64
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
	return &eventLoop{
		events:      events,
		poller:      netPoller,
		ioPool:      ioPool,
		ioPoolOwner: owner,
		buffer:      make([]byte, events.MaxBufferSize),
		fdMap:       newFdMap(),
		evbuf:       make([]poller.Event, eventBatch),
		tasks:       taskqueue.New[*task](),
		ioReady:     make([]*fdConn, 0, eventBatch),
		ioReadyArgs: make([]IOTask, 0, eventBatch),
		ioIdle:      make(chan struct{}),
	}, nil
}

func (loop *eventLoop) acquireIO() bool {
	for {
		state := loop.ioState.Load()
		if state&ioStopBit != 0 {
			return false
		}
		if loop.ioState.CompareAndSwap(state, state+1) {
			return true
		}
	}
}

// Final close callbacks may be scheduled after the shutdown barrier. Their
// lifetime is joined by ioPool.stop, rather than by the fd teardown barrier.
func (loop *eventLoop) acquireCloseIO() { loop.ioState.Add(1) }

func (loop *eventLoop) releaseIO() {
	if loop.ioState.Add(^uint64(0)) == ioStopBit {
		loop.ioIdleOnce.Do(func() { close(loop.ioIdle) })
	}
}

func (loop *eventLoop) ioStopped() bool { return loop.ioState.Load()&ioStopBit != 0 }

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
// readiness is deliberately absent: it is folded into fdConn.pendingEvents and
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
	case refreshTask:
		result = t.conn.updateInterest()
		if result == nil && t.conn.readNeedsRedelivery() && !t.conn.writeIsBlocked() && !t.conn.isClosing() {
			if t.conn.clearReadRedelivery() {
				// Wake drains bytes retained in the connection buffer; Read drains
				// the edge-triggered socket that may not produce another edge.
				t.conn.scheduleIO(ioEventRead | ioEventWake)
			}
		}
		if result != nil {
			t.conn.requestClose(result)
		}
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
		loop.submitIOReady()
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
func (loop *eventLoop) submitIOReady() {
	if len(loop.ioReady) == 0 {
		return
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
	_ = loop.poller.Remove(conn.Fd(), conn.currentInterest())
}
func (loop *eventLoop) modRead(conn *fdConn) error {
	return loop.modifyInterest(conn, poller.Readable)
}
func (loop *eventLoop) modWrite(conn *fdConn) error {
	return loop.modifyInterest(conn, poller.Writable)
}
func (loop *eventLoop) modReadWrite(conn *fdConn) error {
	return loop.modifyInterest(conn, poller.Readable|poller.Writable)
}
func (loop *eventLoop) modifyInterest(conn *fdConn, want poller.Interest) error {
	previous := conn.currentInterest()
	if previous == want {
		return nil
	}
	if err := loop.poller.Modify(conn.Fd(), previous, want); err != nil {
		return err
	}
	conn.setInterest(want)
	return nil
}

// registerConn publishes the fd before poller registration so every delivered
// event can resolve it. Failure unwinds both publications before closing the fd.
func (loop *eventLoop) registerConn(conn *fdConn) error {
	// Publish before Watch so every delivered event can resolve the fd.
	fd := conn.Fd()
	loop.poller.SetEdgeTriggered(fd, !conn.isDatagram())
	if err := loop.fdMap.Put(fd, conn); err != nil {
		conn.closeUnregistered()
		return err
	}
	interest := conn.initialInterest()
	if err := loop.poller.Add(fd, interest); err != nil {
		loop.fdMap.Delete(fd)
		_ = loop.poller.Remove(fd, interest)
		conn.closeUnregistered()
		return err
	}
	conn.setInterest(interest)
	// Datagram callbacks and shared socket state stay on the loop. Stream
	// callbacks run in their serialized connection task. Stdio's scheduleIO
	// starts its dedicated blocking I/O loops after OnOpen below.
	if conn.isDatagram() {
		conn.fireOnOpen()
	} else {
		conn.scheduleIO(ioEventOpen)
	}
	if !conn.isClosing() {
		conn.afterRegister()
	}
	return nil
}
