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
// Input and output bytes always remain owned by their connection rather than
// the scheduler queue. The pool keeps no per-task accounting of its own: every
// submitted turn is already counted by its loop's ioState, which Events joins
// before stopping the pool.
type ioTaskPool struct {
	executor Executor
	owned    *taskgo.Task[*fdConn]
	writes   *taskgo.Task[*writeTurn] // write turns on UIO's own scheduler
	rejected *taskgo.Task[IOTask]
	mu       sync.Mutex // publishes stopped and creates the failure queue
	stopped  atomic.Bool
	stopOnce sync.Once
}

func newIOTaskPool(executor Executor) *ioTaskPool {
	pool := &ioTaskPool{executor: executor}
	if pool.executor == nil {
		// taskgo keeps a small resident worker set for short callbacks and
		// expands only when slow callbacks leave queued connections behind.
		// The high ceiling protects unrelated connections without imposing a
		// fixed goroutine cost on the normal echo path.
		pool.owned = taskgo.NewTask(func(conn *fdConn) { conn.runIOTask() },
			taskgo.WithConcurrency(512*runtime.GOMAXPROCS(0)),
			taskgo.WithMaxIdle(30*time.Second),
		)
		// Write turns only move bytes, so they keep a queue of their own and
		// never wait behind callbacks that block.
		pool.writes = taskgo.NewTask(func(turn *writeTurn) { (*fdConn)(turn).runWriteTurn() },
			taskgo.WithConcurrency(512*runtime.GOMAXPROCS(0)),
			taskgo.WithMaxIdle(30*time.Second),
		)
	}
	return pool
}

// submitWrite schedules one write turn. Unlike connection turns, a refused
// write turn is reported to its caller, which still holds the write claim.
func (pool *ioTaskPool) submitWrite(conn *fdConn) bool {
	if pool == nil || conn == nil || pool.stopped.Load() {
		return false
	}
	if pool.writes != nil {
		return pool.writes.Submit((*writeTurn)(conn))
	}
	return pool.executor.Submit((*writeTurn)(conn))
}

// submit schedules one already-serialized connection through the typed task
// interface without allocating a closure.
func (pool *ioTaskPool) submit(conn *fdConn) bool {
	if pool == nil || conn == nil || pool.stopped.Load() {
		return false
	}
	var accepted bool
	if pool.owned != nil {
		accepted = pool.owned.Submit(conn)
	} else {
		accepted = pool.executor.Submit(conn)
	}
	if !accepted {
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
	if pool.stopped.Load() {
		return false
	}
	if pool.owned != nil {
		for _, task := range tasks {
			if !pool.owned.Submit(task.(*fdConn)) {
				pool.rejectBatch([]IOTask{task})
			}
		}
		return true
	}
	accepted := pool.executor.SubmitBatch(tasks)
	accepted = max(0, min(accepted, len(tasks)))
	pool.rejectBatch(tasks[accepted:])
	return true
}

// submitConnBatch is the allocation-free readiness path for the owned typed
// queue. External executors still receive the public IOTask slice through the
// caller-owned scratch buffer.
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
	ok := pool.submitBatch(args)
	clear(args)
	return ok
}

// rejectBatch finishes the scheduling handoff of tasks the executor refused.
// A refused task may call OnClose, so the failure queue keeps those callbacks
// off the poller without creating one goroutine per connection during
// executor overload. Once stop has begun, the caller finishes them directly.
func (pool *ioTaskPool) rejectBatch(tasks []IOTask) {
	if len(tasks) == 0 {
		return
	}
	pool.mu.Lock()
	if pool.stopped.Load() {
		pool.mu.Unlock()
		for _, task := range tasks {
			task.(*fdConn).handleIOSubmitFailure(net.ErrClosed)
		}
		return
	}
	if pool.rejected == nil {
		pool.rejected = taskgo.NewTask(func(task IOTask) {
			task.(*fdConn).handleIOSubmitFailure(net.ErrClosed)
		}, taskgo.WithConcurrency(ioRejectWorkers))
	}
	// Submitting under mu keeps stop from sealing the queue in between.
	accepted := pool.rejected.SubmitBatch(tasks)
	pool.mu.Unlock()
	for _, task := range tasks[accepted:] {
		task.(*fdConn).handleIOSubmitFailure(net.ErrClosed)
	}
}

func connTasks(conns []*fdConn) []IOTask {
	tasks := make([]IOTask, len(conns))
	for index, conn := range conns {
		tasks[index] = conn
	}
	return tasks
}

// stop rejects future work and drains UIO's own queues. Callers join accepted
// connection turns through their loops first; an injected executor's
// lifecycle belongs to its caller.
func (pool *ioTaskPool) stop() {
	if pool == nil {
		return
	}
	pool.stopOnce.Do(func() {
		pool.mu.Lock()
		pool.stopped.Store(true)
		rejected := pool.rejected
		pool.mu.Unlock()
		if pool.owned != nil {
			_ = pool.owned.Stop(context.Background())
		}
		if pool.writes != nil {
			_ = pool.writes.Stop(context.Background())
		}
		if rejected != nil {
			_ = rejected.Stop(context.Background())
		}
	})
}
