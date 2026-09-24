//go:build (linux || darwin || netbsd || freebsd || openbsd || dragonfly) && !stdio

package uio

import (
	"context"
	"net"
	"runtime"
	"sync"
	"time"

	"github.com/limpo1989/taskgo"
)

const ioRejectWorkers = 4

// ioTaskPool wraps the default typed taskgo queue or a caller-supplied Executor.
// It owns UIO shutdown/rejection accounting, but input and output bytes always
// remain owned by their connection rather than the scheduler queue.
type ioTaskPool struct {
	executor Executor
	owned    *taskgo.Task[IOTask]
	rejected *taskgo.Task[IOTask]
	mu       sync.Mutex
	stopped  bool
	tasks    sync.WaitGroup
	stopOnce sync.Once
}

func newIOTaskPool(executor Executor) *ioTaskPool {
	pool := &ioTaskPool{executor: executor}
	if pool.executor == nil {
		workers := runtime.NumCPU() * 512
		// taskgo starts near 2*GOMAXPROCS workers and can grow toward this
		// hard ceiling when callbacks block. Retain grown stacks across bursts.
		pool.owned = taskgo.NewTask(
			func(task IOTask) {
				defer pool.tasks.Done()
				task.(*fdConn).runIOTask()
			},
			taskgo.WithConcurrency(workers),
			taskgo.WithMaxIdle(30*time.Second),
		)
		pool.executor = pool.owned
	}
	return pool
}

// submit schedules one already-serialized connection through the typed task
// interface without allocating a closure.
func (pool *ioTaskPool) submit(conn *fdConn) bool {
	if pool == nil || conn == nil {
		return false
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
	if !pool.start(len(tasks)) {
		return false
	}
	accepted := pool.executor.SubmitBatch(tasks)
	accepted = max(0, min(accepted, len(tasks)))
	pool.rejectBatch(tasks[accepted:])
	return true
}

func (pool *ioTaskPool) start(count int) bool {
	pool.mu.Lock()
	defer pool.mu.Unlock()
	if pool.stopped {
		return false
	}
	pool.tasks.Add(count)
	return true
}

// run executes one task accepted by an injected executor and releases its
// shutdown accounting reference.
func (pool *ioTaskPool) run(conn *fdConn) {
	defer pool.tasks.Done()
	conn.runIOTask()
}

func (pool *ioTaskPool) rejectBatch(tasks []IOTask) {
	if len(tasks) == 0 {
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

// stop rejects future work and waits for every accepted task. UIO stops only
// the default taskgo queue; an injected executor's lifecycle belongs to caller.
func (pool *ioTaskPool) stop() {
	if pool == nil {
		return
	}
	pool.stopOnce.Do(func() {
		pool.mu.Lock()
		pool.stopped = true
		pool.mu.Unlock()
		pool.tasks.Wait()
		if pool.owned != nil {
			_ = pool.owned.Stop(context.Background())
		}
		if pool.rejected != nil {
			_ = pool.rejected.Stop(context.Background())
		}
	})
}
