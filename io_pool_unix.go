//go:build (linux || darwin || netbsd || freebsd || openbsd || dragonfly) && !stdio

package uio

import (
	"context"
	"net"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/limpo1989/taskgo"
)

const ioRejectWorkers = 4

// ioTaskPool wraps the default taskgo scheduler or a caller-supplied Executor.
// It owns UIO shutdown/rejection accounting, but input and output bytes always
// remain owned by their connection rather than the scheduler queue.
type ioTaskPool struct {
	executor Executor
	owned    *taskgo.Task[*fdConn]
	rejected *taskgo.Task[IOTask]
	mu       sync.Mutex
	stopped  atomic.Bool
	track    bool // external executors need a join counter; owned taskgo drains itself
	tasks    sync.WaitGroup
	stopOnce sync.Once
}

func newIOTaskPool(executor Executor) *ioTaskPool {
	pool := &ioTaskPool{executor: executor}
	if pool.executor == nil {
		// taskgo keeps a small resident worker set for short callbacks and
		// expands only when slow callbacks leave queued connections behind.
		// The high ceiling protects unrelated connections without imposing a
		// fixed goroutine cost on the normal echo path.
		pool.owned = taskgo.NewTask(func(conn *fdConn) { pool.run(conn) },
			taskgo.WithConcurrency(512*runtime.GOMAXPROCS(0)),
			taskgo.WithMaxIdle(30*time.Second),
		)
	} else {
		pool.track = true
	}
	return pool
}

// submit schedules one already-serialized connection through the typed task
// interface without allocating a closure.
func (pool *ioTaskPool) submit(conn *fdConn) bool {
	if pool == nil || conn == nil {
		return false
	}
	if pool.owned != nil {
		if pool.stopped.Load() {
			return false
		}
		if !pool.owned.Submit(conn) {
			pool.rejectBatch([]IOTask{conn})
		}
		return true
	}
	if !pool.start(1) {
		return false
	}
	if !pool.executor.Submit(conn) {
		pool.rejectBatch([]IOTask{conn})
	}
	return true
}

// submitBatch hands one complete readiness batch to the executor. A short
// acceptance closes only the rejected suffix and preserves prefix ordering.
func (pool *ioTaskPool) submitBatch(tasks []IOTask) bool {
	if pool == nil || len(tasks) == 0 {
		return pool != nil
	}
	if pool.owned != nil {
		if pool.stopped.Load() {
			return false
		}
		for _, task := range tasks {
			if !pool.owned.Submit(task.(*fdConn)) {
				pool.rejectBatch([]IOTask{task})
			}
		}
		return true
	}
	if !pool.start(len(tasks)) {
		return false
	}
	accepted := pool.executor.SubmitBatch(tasks)
	accepted = max(0, min(accepted, len(tasks)))
	pool.rejectBatch(tasks[accepted:])
	return true
}

// submitConnBatch is the allocation-free readiness path for the owned typed
// queue. External executors still receive the public IOTask slice through the
// loop-owned scratch buffer.
func (pool *ioTaskPool) submitConnBatch(conns []*fdConn, args []IOTask) bool {
	if pool == nil || len(conns) == 0 {
		return pool != nil
	}
	if pool.owned != nil {
		if pool.stopped.Load() {
			return false
		}
		accepted := pool.owned.SubmitBatch(conns)
		if accepted != len(conns) {
			pool.rejectBatch(connTasks(conns[accepted:]))
		}
		return true
	}
	if cap(args) < len(conns) {
		args = make([]IOTask, len(conns))
	} else {
		args = args[:len(conns)]
	}
	for index, conn := range conns {
		args[index] = conn
	}
	return pool.submitBatch(args)
}

func (pool *ioTaskPool) start(count int) bool {
	if !pool.track {
		return !pool.stopped.Load()
	}
	pool.mu.Lock()
	defer pool.mu.Unlock()
	if pool.stopped.Load() {
		return false
	}
	pool.tasks.Add(count)
	return true
}

// run executes one task accepted by an injected executor and releases its
// shutdown accounting reference.
func (pool *ioTaskPool) run(conn *fdConn) {
	if pool.track {
		defer pool.tasks.Done()
	}
	conn.runIOTask()
}

func (pool *ioTaskPool) rejectBatch(tasks []IOTask) {
	if len(tasks) == 0 {
		return
	}
	if !pool.track {
		// The owned queue drains accepted work during Stop. A false admission
		// result can therefore only be a stop race. Before the private failure
		// queue is stopped, keep rejected callbacks off the poller; after the
		// owned queue has begun stopping, finish the close handoff directly.
		if pool.stopped.Load() {
			for _, task := range tasks {
				task.(*fdConn).handleIOSubmitFailure(net.ErrClosed)
			}
			return
		}
		pool.mu.Lock()
		if pool.rejected == nil {
			pool.rejected = taskgo.NewTask(func(task IOTask) {
				task.(*fdConn).handleIOSubmitFailure(net.ErrClosed)
			}, taskgo.WithConcurrency(ioRejectWorkers))
		}
		queue := pool.rejected
		pool.mu.Unlock()
		if accepted := queue.SubmitBatch(tasks); accepted != len(tasks) {
			panic("uio: owned I/O failure queue stopped before rejected tasks completed")
		}
		return
	}
	pool.mu.Lock()
	if pool.rejected == nil {
		// A rejected task may call OnClose. Keep callbacks off the poller without
		// creating one goroutine per connection during executor overload.
		pool.rejected = taskgo.NewTask(func(task IOTask) {
			defer pool.tasks.Done()
			task.(*fdConn).handleIOSubmitFailure(net.ErrClosed)
		}, taskgo.WithConcurrency(ioRejectWorkers))
	}
	queue := pool.rejected
	pool.mu.Unlock()
	// The failure queue is private, admission-unlimited, and stopped only after
	// tasks.Wait; every task counted by start must retain a completion owner.
	if accepted := queue.SubmitBatch(tasks); accepted != len(tasks) {
		panic("uio: I/O failure queue stopped before rejected tasks completed")
	}
}

func connTasks(conns []*fdConn) []IOTask {
	tasks := make([]IOTask, len(conns))
	for index, conn := range conns {
		tasks[index] = conn
	}
	return tasks
}

// stop rejects future work and waits for every accepted task. UIO stops only
// its own scheduler; an injected executor's lifecycle belongs to caller.
func (pool *ioTaskPool) stop() {
	if pool == nil {
		return
	}
	pool.stopOnce.Do(func() {
		pool.mu.Lock()
		pool.stopped.Store(true)
		pool.mu.Unlock()
		if pool.track {
			pool.tasks.Wait()
		}
		if pool.owned != nil {
			_ = pool.owned.Stop(context.Background())
		}
		if pool.rejected != nil {
			_ = pool.rejected.Stop(context.Background())
		}
	})
}
