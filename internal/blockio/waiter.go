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

package blockio

import (
	"context"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// waiter coordinates one blocking consumer or producer with non-blocking
// progress notifications, independent deadlines, and terminal connection close.
type waiter struct {
	gen       atomic.Uint64
	parked    atomic.Bool
	sig       chan struct{}
	closed    chan struct{}
	closeOnce sync.Once

	deadlineMu  sync.Mutex
	deadline    time.Time
	deadlineGen uint64
	timer       *time.Timer
	expired     atomic.Bool
}

func newWaiter() waiter {
	return waiter{
		sig:    make(chan struct{}, 1),
		closed: make(chan struct{}),
	}
}

// prepare arms parked and snapshots the current generation before the caller
// checks its condition, preventing lost wakeups.
func (w *waiter) prepare() uint64 {
	tok := w.gen.Load()
	w.parked.Store(true)
	return tok
}

func (w *waiter) cancelPrepare() {
	w.parked.Store(false)
}

// notify advances the generation and wakes a parked caller if one is waiting.
func (w *waiter) notify() {
	w.gen.Add(1)
	if w.parked.Load() && w.parked.Swap(false) {
		select {
		case w.sig <- struct{}{}:
		default:
		}
	}
}

// wait blocks until notify, SetDeadline expiry, or closeWake fires, unless gen
// already advanced past tok.
func (w *waiter) wait(tok uint64) {
	_ = w.waitContext(nil, tok)
}

func (w *waiter) waitContext(ctx context.Context, tok uint64) error {
	if w.gen.Load() != tok {
		w.parked.Store(false)
		return nil
	}
	if ctx == nil || ctx.Done() == nil {
		select {
		case <-w.sig:
		case <-w.closed:
		}
		return nil
	}
	select {
	case <-w.sig:
		return nil
	case <-w.closed:
		return nil
	case <-ctx.Done():
		w.parked.Store(false)
		return ctx.Err()
	}
}

func (w *waiter) setDeadline(t time.Time) error {
	w.deadlineMu.Lock()
	w.deadlineGen++
	gen := w.deadlineGen
	if w.timer != nil {
		w.timer.Stop()
		w.timer = nil
	}
	w.deadline = t
	if t.IsZero() {
		w.expired.Store(false)
		w.deadlineMu.Unlock()
		return nil
	}
	d := time.Until(t)
	if d <= 0 {
		w.expired.Store(true)
		w.deadlineMu.Unlock()
		w.notify()
		return nil
	}
	w.expired.Store(false)
	w.timer = time.AfterFunc(d, func() {
		w.deadlineMu.Lock()
		if w.deadlineGen == gen {
			w.expired.Store(true)
		}
		w.deadlineMu.Unlock()
		w.notify()
	})
	w.deadlineMu.Unlock()
	return nil
}

func (w *waiter) checkDeadline() error {
	if !w.expired.Load() {
		return nil
	}
	return os.ErrDeadlineExceeded
}

func (w *waiter) closeWake() {
	w.closeOnce.Do(func() {
		w.deadlineMu.Lock()
		w.deadlineGen++
		if w.timer != nil {
			w.timer.Stop()
			w.timer = nil
		}
		w.deadlineMu.Unlock()
		w.gen.Add(1)
		close(w.closed)
	})
}
