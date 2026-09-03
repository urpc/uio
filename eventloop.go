package uio

import (
	"runtime"
	"sync"
	"sync/atomic"

	"github.com/urpc/uio/internal/fdmap"
	"github.com/urpc/uio/internal/poller"
	"github.com/urpc/uio/internal/taskqueue"
)

const (
	taskBudget = 256
	eventBatch = 1024
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

type eventLoop struct {
	events *Events
	poller *poller.NetPoller
	buffer []byte
	fdMap  *fdmap.Map[fdConn]
	evbuf  []poller.Event

	tasks       *taskqueue.Queue[*task] // public MPSC queue
	taskBatch   *taskqueue.Node[*task]  // private FIFO remainder owned by this loop
	wakePending atomic.Bool             // coalesces producer wakeups
	stopping    atomic.Bool
	loopGoid    atomic.Int64
	stopErr     error
	touched     []*fdConn
}

func newEventLoop(events *Events) (*eventLoop, error) {
	netPoller, err := poller.NewNetPoller()
	if err != nil {
		return nil, err
	}
	return &eventLoop{
		events:  events,
		poller:  netPoller,
		buffer:  make([]byte, events.MaxBufferSize),
		fdMap:   newFdMap(),
		evbuf:   make([]poller.Event, eventBatch),
		tasks:   taskqueue.New[*task](),
		touched: make([]*fdConn, 0, taskBudget),
	}, nil
}

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
	loop.flushTouched()
}

func (loop *eventLoop) runTask(t *task) {
	done := t.done
	var result error
	switch t.kind {
	case writeTask:
		t.conn.runWriteTask(t)
	case flushTask:
		result = t.conn.runFlushTask()
		if result != nil {
			t.conn.requestClose(result)
		}
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
	}
	releaseTask(t)
	if done != nil {
		done <- result
	}
}

func (loop *eventLoop) runRegisterTask(t *task) error {
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

func (loop *eventLoop) touch(conn *fdConn) {
	// A task batch may append many buffers to one connection; flush it once.
	if conn.markTouched() {
		loop.touched = append(loop.touched, conn)
	}
}

func (loop *eventLoop) flushTouched() {
	for _, conn := range loop.touched {
		conn.clearTouched()
		if conn.isClosedOnLoop() {
			continue
		}
		if _, err := conn.flushOnLoop(); err != nil {
			conn.requestClose(err)
			continue
		}
		if err := conn.updateInterest(); err != nil {
			conn.requestClose(err)
		}
	}
	loop.touched = loop.touched[:0]
}

func (loop *eventLoop) Serve(lockOSThread bool, handler poller.EventHandler) (result error) {
	if lockOSThread {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
	}
	if handler == nil {
		handler = loop
	}
	loop.loopGoid.Store(currentGoroutineID())
	defer loop.loopGoid.Store(0)

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

func (loop *eventLoop) shutdown(err error) {
	// fdMap is shared on Unix, so only close entries owned by this loop.
	for fd, conn := range loop.fdMap.Range() {
		if conn.loop == loop {
			loop.fdMap.Delete(fd)
			conn.closeOnLoop(err)
		}
	}
}

func (loop *eventLoop) OnEvent(_ *poller.NetPoller, fd int, events poller.Events) {
	conn := loop.getConn(fd)
	if conn == nil || conn.isClosing() {
		return
	}
	if events&poller.WriteEvents != 0 {
		if err := conn.fireWriteEvent(); err != nil {
			conn.requestClose(err)
			return
		}
	}
	if events&poller.ReadEvents != 0 && !conn.isClosing() {
		if err := conn.fireReadEvent(); err != nil {
			conn.requestClose(err)
		}
	}
}

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

func (loop *eventLoop) registerConn(conn *fdConn) error {
	// Publish before Watch so every delivered event can resolve the fd.
	fd := conn.Fd()
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
	conn.fireOnOpen()
	// Blocking std backends start I/O only after OnOpen; native pollers do nothing here.
	if !conn.isClosing() {
		conn.afterRegister()
	}
	return nil
}
