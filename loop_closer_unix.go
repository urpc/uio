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
	"sync"
	"syscall"
)

// loopCloser closes the descriptors a loop's turns release on a goroutine of
// its own. Closing a descriptor the poller watches takes the poller's lock,
// which its waiters hold while they collect readiness and register accepted
// connections; under connection churn, turns on many CPUs closing at once
// spun on that lock for 6% of the server's cycles. One goroutine per loop
// closing them in turn leaves the lock two contenders, and measured a churn
// load 7% faster at 4096 connections and level at 512.
//
// A released connection has left the fd table before its descriptor is
// queued, so readiness the poller still reports for it finds no connection,
// and the descriptor number is not reused before the closer closes it. Only
// descriptors whose close cannot block are queued (see fdConn.closesOffTurn):
// a close that lingers would hold up every one behind it.
type loopCloser struct {
	mu    sync.Mutex
	open  bool
	queue []int32
	spare []int32
	kick  chan struct{}
	done  chan struct{}
}

func newLoopCloser() *loopCloser {
	c := &loopCloser{open: true, kick: make(chan struct{}, 1), done: make(chan struct{})}
	go c.run()
	return c
}

// deferClose queues fd for the loop's closer and reports whether it did; the
// caller closes fd itself when the loop has none, or it has stopped.
func (loop *eventLoop) deferClose(fd int) bool {
	c := loop.closer
	if c == nil {
		return false
	}
	c.mu.Lock()
	if !c.open {
		c.mu.Unlock()
		return false
	}
	c.queue = append(c.queue, int32(fd))
	select {
	case c.kick <- struct{}{}:
	default: // a kick is pending: the closer takes this one with it
	}
	c.mu.Unlock()
	return true
}

func (c *loopCloser) run() {
	defer close(c.done)
	for range c.kick {
		c.drain()
	}
}

// drain closes what is queued. Only the closer drains while it runs, and stop
// only once it has returned, so the two buffers are never shared.
func (c *loopCloser) drain() {
	c.mu.Lock()
	queued := c.queue
	c.queue = c.spare[:0]
	c.spare = nil
	c.mu.Unlock()
	for _, fd := range queued {
		_ = syscall.Close(int(fd))
	}
	c.mu.Lock()
	if c.spare == nil {
		c.spare = queued[:0]
	}
	c.mu.Unlock()
}

// stop refuses further descriptors, waits for the closer to return and
// closes what it left.
func (c *loopCloser) stop() {
	c.mu.Lock()
	wasOpen := c.open
	c.open = false
	if wasOpen {
		close(c.kick)
	}
	c.mu.Unlock()
	if wasOpen {
		<-c.done
	}
	c.drain()
}
