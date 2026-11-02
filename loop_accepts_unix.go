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
	"runtime"
	"sync/atomic"
)

// acceptRunner accepts on a loop's listeners on a goroutine of its own.
// Accepting costs a few microseconds of syscalls per connection, which under
// connection churn filled the waiters that used to accept between their
// collections: on a 64-P host, accept4 and registration were 40% of the
// waiters' time, and connections waited for their first turn while other Ps
// sat idle. With a runner per loop a churn load at 512 connections ran 9-11%
// faster at 64 Ps, 23% at 32 and 6% at 24, and level at 16 and 8.
//
// The listeners are edge-triggered. A waiter that collects a listener's edge
// hands it over (handOff); the runner accepts until the socket would block,
// so the edge is spent, unless another edge arrived meanwhile, which
// acceptState records.
type acceptRunner struct {
	kick    chan struct{}
	done    chan struct{}
	stopped atomic.Bool
}

// A listener's acceptState: idle, handed over and accepting, or accepting
// with an edge collected since it was handed over.
const (
	acceptIdle int32 = iota
	acceptRunning
	acceptAgain
)

func newAcceptRunner(loop *eventLoop) *acceptRunner {
	r := &acceptRunner{kick: make(chan struct{}, 1), done: make(chan struct{})}
	go r.run(loop)
	return r
}

// handOff passes a listener edge to the runner. It reports whether the runner
// was woken: the waiter then yields, so the runner starts at once.
func (r *acceptRunner) handOff(l *listener) bool {
	for {
		switch state := l.acceptState.Load(); state {
		case acceptIdle:
			if l.acceptState.CompareAndSwap(acceptIdle, acceptRunning) {
				select {
				case r.kick <- struct{}{}:
				default: // a kick is pending: the runner finds l with it
				}
				return true
			}
		case acceptRunning:
			if l.acceptState.CompareAndSwap(acceptRunning, acceptAgain) {
				return false
			}
		default:
			return false
		}
	}
}

func (r *acceptRunner) run(loop *eventLoop) {
	defer close(r.done)
	scratch := &loopWaiter{ready: make([]*fdConn, 0, acceptBatchSize)}
	if loop.events.Executor != nil {
		scratch.args = make([]IOTask, 0, acceptBatchSize)
	}
	for range r.kick {
		// Rounds of one batch per handed-over listener, so a flooded listener
		// does not hold up the others of the loop, until none has more.
		for !r.stopped.Load() {
			listeners := loop.listeners.Load()
			if listeners == nil {
				break
			}
			more, submitted := false, false
			for _, l := range *listeners {
				if l.acceptState.Load() != acceptIdle {
					lmore, lsubmitted := r.acceptBatch(loop, l, scratch)
					more = more || lmore
					submitted = submitted || lsubmitted
				}
			}
			if !more {
				break
			}
			if submitted {
				// Run the workers the round woke before taking the next.
				runtime.Gosched()
			}
		}
		if r.stopped.Load() {
			return
		}
	}
}

// acceptBatch takes one batch from l and hands its first turns over. It
// reports whether l has more to take: the batch ended at its bound, or an edge
// arrived after it began, and the connection behind that edge may have missed
// the accepts that would block. Otherwise l goes idle until its next edge.
func (r *acceptRunner) acceptBatch(loop *eventLoop, l *listener, scratch *loopWaiter) (more, submitted bool) {
	more, err := loop.accept(l, scratch)
	submitted = loop.submit(scratch)
	if err != nil {
		loop.events.initiateClose(err)
		more = false
	}
	if more {
		return true, submitted
	}
	if testHookAcceptDrained != nil {
		testHookAcceptDrained(l)
	}
	if l.acceptState.CompareAndSwap(acceptRunning, acceptIdle) {
		return false, submitted
	}
	l.acceptState.Store(acceptRunning)
	return true, submitted
}

// testHookAcceptDrained runs when a batch found l would block, before it
// settles l's state; nil outside tests.
var testHookAcceptDrained func(l *listener)

// stop ends the runner once its current batch is handed over, and waits for
// it. Serve stops it after the loop's waiters, before the loop releases its
// connections, so nothing is accepted after that.
func (r *acceptRunner) stop() {
	r.stopped.Store(true)
	select {
	case r.kick <- struct{}{}:
	default:
	}
	<-r.done
}
