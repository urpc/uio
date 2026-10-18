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
	"context"
	"net"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/limpo1989/taskgo"
	"github.com/urpc/uio/internal/shardpool"
)

// ioRejectWorkers bounds the goroutines that finish turns an injected
// Executor refused, and so the OnClose callbacks they run at once.
const ioRejectWorkers = 4

// writeShardCapacity bounds a write scheduler shard's queue, which is
// allocated up front: 16 KiB at 1024 tasks, for each of 2×GOMAXPROCS shards. A
// connection has at most one write turn queued or running, holding its write
// claim, so a shard queues no more turns than it has connections; a fuller one
// runs the excess on goroutines of their own (see shardpool.Pool.Submit).
const writeShardCapacity = 1024

// turnShardsPerP is how many ingress shards the connection turn queue has per
// P. A worker keeps serving its shard until the shard runs dry, so the turns
// of a connection, and the kernel's state for its socket, stay on one CPU.
// On an echo server saturating 24 Ps, 48 shards took 9% more requests than
// one shared queue at the same CPU; 96 took as many, with a longer tail.
const turnShardsPerP = 2

// ioTaskPool runs connection turns on a taskgo queue and write turns on UIO's
// own sharded scheduler, both keyed by connection (see fdConn.turnKey,
// taskgo.NewTaskKeyed and internal/shardpool), or both on a caller-supplied
// Executor. Input and output
// bytes always remain owned by their connection rather than the scheduler
// queue. The pool keeps no per-task accounting of its own: every submitted
// turn is already counted by its loop's turn count, which Events joins before
// stopping the pool.
//
// Write turns have workers of their own. A write turn sends output from other
// goroutines, so it is submitted from outside, one at a time, and waits for a
// write worker draining its shard rather than waking another; queued behind
// connection turns on the same workers instead, replies sent from 24
// goroutines to 512 echoing connections measured 2-3% slower and their p99
// 60% higher.
type ioTaskPool struct {
	executor Executor
	owned    *taskgo.Task[*fdConn]
	writes   *shardpool.Pool[*writeTurn]
	rejected rejectQueue // turns the executor refused
	mu       sync.Mutex  // publishes stopped against rejectBatch
	stopped  atomic.Bool
	stopOnce sync.Once
}

func turnKey(conn *fdConn) uint32         { return conn.turnKey() }
func writeTurnKey(turn *writeTurn) uint32 { return (*fdConn)(turn).turnKey() }

func newIOTaskPool(executor Executor) *ioTaskPool {
	pool := &ioTaskPool{executor: executor}
	if pool.executor == nil {
		procs := runtime.GOMAXPROCS(0)
		// taskgo keeps about one worker per P running and adds workers, up to
		// the ceiling, only while callbacks block; parked workers wait long
		// enough to reuse their grown stacks.
		pool.owned = taskgo.NewTaskKeyed((*fdConn).runIOTask, turnKey, turnShardsPerP*procs,
			taskgo.WithConcurrency(512*procs),
			taskgo.WithMaxIdle(30*time.Second),
		)
		pool.writes = shardpool.New(procs, writeShardCapacity, writeTurnKey, func(turn *writeTurn) {
			(*fdConn)(turn).runWriteTurn()
		})
	}
	return pool
}

// connOpened and connClosed tell the write scheduler how many connections it
// serves.
func (pool *ioTaskPool) connOpened() {
	if pool.writes != nil {
		pool.writes.AddKey()
	}
}

func (pool *ioTaskPool) connClosed() {
	if pool.writes != nil {
		pool.writes.RemoveKey()
	}
}

// submitWrite schedules one write turn. Unlike connection turns, a refused
// write turn is reported to its caller, which still holds the write claim.
func (pool *ioTaskPool) submitWrite(conn *fdConn) bool {
	accepted, _ := pool.submitWriteWaking(conn)
	return accepted
}

// submitWriteWaking is submitWrite that also reports whether the owned
// scheduler woke a worker for the turn (see shardpool.Pool.Submit). A write
// turn is submitted from outside the scheduler, one at a time, so it waits
// for a worker already draining its shard rather than waking another.
func (pool *ioTaskPool) submitWriteWaking(conn *fdConn) (accepted, woke bool) {
	if pool == nil || conn == nil || pool.stopped.Load() {
		return false, false
	}
	if pool.writes != nil {
		return true, pool.writes.SubmitBehind((*writeTurn)(conn))
	}
	return pool.executor.Submit((*writeTurn)(conn)), false
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
		if accepted := pool.owned.SubmitBatch(conns); accepted != len(conns) {
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
	// Queueing under mu keeps stop from waiting out the queue in between.
	pool.rejected.push(tasks)
	pool.mu.Unlock()
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
		pool.mu.Unlock()
		if pool.owned != nil {
			_ = pool.owned.Stop(context.Background())
			pool.writes.Stop()
		}
		pool.rejected.wait()
	})
}

// connTasks boxes refused connections for rejectBatch.
func connTasks(conns []*fdConn) []IOTask {
	tasks := make([]IOTask, len(conns))
	for index, conn := range conns {
		tasks[index] = conn
	}
	return tasks
}

// rejectQueue runs refused turns' failure handling on at most
// ioRejectWorkers goroutines, started while the queue has work and gone once
// it is empty.
type rejectQueue struct {
	mu      sync.Mutex
	tasks   []IOTask
	next    int // index of the first task not yet taken
	running int
	done    sync.WaitGroup
}

func (queue *rejectQueue) push(tasks []IOTask) {
	queue.mu.Lock()
	queue.tasks = append(queue.tasks, tasks...)
	for queue.running < ioRejectWorkers && queue.running < len(queue.tasks)-queue.next {
		queue.running++
		queue.done.Add(1)
		go queue.drain()
	}
	queue.mu.Unlock()
}

func (queue *rejectQueue) drain() {
	defer queue.done.Done()
	for {
		queue.mu.Lock()
		if queue.next == len(queue.tasks) {
			clear(queue.tasks)
			queue.tasks, queue.next = queue.tasks[:0], 0
			queue.running--
			queue.mu.Unlock()
			return
		}
		task := queue.tasks[queue.next]
		queue.tasks[queue.next] = nil
		queue.next++
		queue.mu.Unlock()
		task.(*fdConn).handleIOSubmitFailure(net.ErrClosed)
	}
}

// wait returns once every queued task has been handled.
func (queue *rejectQueue) wait() { queue.done.Wait() }
