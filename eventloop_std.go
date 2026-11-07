//go:build windows || stdio

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

	"github.com/urpc/uio/internal/blockio"
	"github.com/urpc/uio/internal/taskqueue"
)

const taskBudget = 256

// eventLoop serializes connection registration, Wake, and teardown on the
// portable blocking backend without synthetic descriptors or OS pollers.
type eventLoop struct {
	events *Events
	tasks  *taskqueue.Queue[*task]
	waker  chan struct{}
	closed chan struct{}

	closeOnce sync.Once
	closeMu   sync.Mutex
	closeErr  error

	loopGoid    atomic.Int64
	wakePending atomic.Bool
	stopping    atomic.Bool
	stopErr     error
	taskBatch   *taskqueue.Node[*task]

	connsMu sync.Mutex
	conns   map[*fdConn]struct{}
	stats   loopStats
}

func newEventLoop(events *Events) (*eventLoop, error) {
	return &eventLoop{
		events: events,
		tasks:  taskqueue.New[*task](),
		waker:  make(chan struct{}, 1),
		closed: make(chan struct{}),
		conns:  make(map[*fdConn]struct{}),
	}, nil
}

func (loop *eventLoop) closeLoop(err error) {
	loop.closeOnce.Do(func() {
		loop.closeMu.Lock()
		loop.closeErr = err
		loop.closeMu.Unlock()
		close(loop.closed)
	})
}

func (loop *eventLoop) loopClosed() bool {
	select {
	case <-loop.closed:
		return true
	default:
		return false
	}
}

func (loop *eventLoop) loopErr() error {
	loop.closeMu.Lock()
	defer loop.closeMu.Unlock()
	return loop.closeErr
}

// inLoop prevents synchronous control methods from waiting on their own queue.
func (loop *eventLoop) inLoop() bool {
	id := loop.loopGoid.Load()
	return id != 0 && id == currentGoroutineID()
}

func (loop *eventLoop) submitTask(t *task) bool {
	if !loop.tasks.Push(&t.node) {
		return false
	}
	loop.notify()
	return true
}

func (loop *eventLoop) notify() {
	if loop.wakePending.CompareAndSwap(false, true) {
		select {
		case loop.waker <- struct{}{}:
		default:
		}
	}
}

func (loop *eventLoop) beginStop(err error) {
	t := acquireTask(stopTask, nil)
	t.err = err
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

func (loop *eventLoop) runTask(t *task) {
	done := t.done
	var result error
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
	result := loop.watchConn(t.conn)
	if request.state.CompareAndSwap(registerPending, registerCompleted) {
		if result == nil {
			t.conn.afterRegister()
		}
		return result
	}
	if result == nil {
		t.conn.closeOnLoop(request.cause())
	}
	return request.cause()
}

// Serve drains control tasks until stopped or closed, then releases all
// connections owned by this loop.
func (loop *eventLoop) Serve(lockOSThread bool, onClose func(error)) (result error) {
	blockio.EnterTurn()
	defer blockio.LeaveTurn()
	if lockOSThread {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
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
		if loop.hasPendingTasks() {
			continue
		}
		loop.wakePending.Store(false)
		if loop.hasPendingTasks() {
			continue
		}
		select {
		case <-loop.closed:
			result = loop.loopErr()
			loop.beginStop(result)
		case <-loop.waker:
		}
	}

	if result == nil {
		result = loop.stopErr
	}
	loop.shutdown(result)
	if onClose != nil {
		onClose(result)
	}
	loop.closeLoop(result)
	return result
}

func (loop *eventLoop) shutdown(err error) {
	loop.connsMu.Lock()
	conns := make([]*fdConn, 0, len(loop.conns))
	for conn := range loop.conns {
		conns = append(conns, conn)
	}
	clear(loop.conns)
	loop.connsMu.Unlock()

	for _, conn := range conns {
		conn.closeOnLoop(err)
	}
}

func (loop *eventLoop) delConn(conn *fdConn) {
	loop.connsMu.Lock()
	delete(loop.conns, conn)
	loop.connsMu.Unlock()
}

// registeredForTest, when a test sets it, runs on the loop after a connection
// is recorded on its loop and before the loop starts its read goroutine.
var registeredForTest func(*fdConn)

func (loop *eventLoop) registerConn(conn *fdConn) error {
	if err := loop.watchConn(conn); err != nil {
		return err
	}
	conn.afterRegister()
	return nil
}

func (loop *eventLoop) watchConn(conn *fdConn) error {
	if loop.stopping.Load() || loop.loopClosed() {
		conn.closeUnregistered()
		if err := loop.loopErr(); err != nil {
			return err
		}
		return net.ErrClosed
	}
	loop.connsMu.Lock()
	loop.conns[conn] = struct{}{}
	loop.connsMu.Unlock()
	if registeredForTest != nil {
		registeredForTest(conn)
	}
	return nil
}
