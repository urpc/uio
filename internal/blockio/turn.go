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

// Package blockio implements symmetric bounded blocking Reader and Writer
// endpoints backed by non-blocking connection turns.
package blockio

import (
	"errors"
	"sync"
	"sync/atomic"

	"github.com/petermattis/goid"
)

// ErrBlockingOnTurn reports an attempt to invoke a blocking operation from a
// connection turn or event-loop goroutine.
var ErrBlockingOnTurn = errors.New("uio: blocking I/O cannot run on a connection turn")

// ErrOverflow matches the underlying transport's outbound overflow sentinel.
var ErrOverflow = errors.New("outbound buffer overflow")

var turnGoroutines sync.Map // int64 -> *atomic.Int32

// EnterTurn marks the calling goroutine as executing event-loop or connection-turn
// work. Nested EnterTurn/LeaveTurn pairs on the same goroutine are reference-counted.
func EnterTurn() {
	id := goid.Get()
	if v, ok := turnGoroutines.Load(id); ok {
		if v.(*atomic.Int32).Add(1) > 1 {
			return
		}
	}
	ref := &atomic.Int32{}
	ref.Store(1)
	if actual, loaded := turnGoroutines.LoadOrStore(id, ref); loaded {
		actual.(*atomic.Int32).Add(1)
	}
}

// LeaveTurn unmarks one EnterTurn call on the calling goroutine.
func LeaveTurn() {
	id := goid.Get()
	if v, ok := turnGoroutines.Load(id); ok {
		if v.(*atomic.Int32).Add(-1) <= 0 {
			turnGoroutines.Delete(id)
		}
	}
}

// OnTurn reports whether the calling goroutine is registered as a turn or
// event-loop goroutine.
func OnTurn() bool {
	if v, ok := turnGoroutines.Load(goid.Get()); ok {
		return v.(*atomic.Int32).Load() > 0
	}
	return false
}
