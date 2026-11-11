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
	"sync/atomic"

	"github.com/petermattis/goid"
)

// ErrBlockingOnTurn reports an attempt to invoke a blocking operation from a
// connection turn or event-loop goroutine.
var ErrBlockingOnTurn = errors.New("uio: blocking I/O cannot run on a connection turn")

// ErrOverflow matches the underlying transport's outbound overflow sentinel.
var ErrOverflow = errors.New("outbound buffer overflow")

const turnBuckets = 256

type turnEntry struct {
	goid  atomic.Int64
	depth atomic.Int32
	next  atomic.Pointer[turnEntry]
}

var turnTable [turnBuckets]struct {
	head atomic.Pointer[turnEntry]
	_    [56]byte
}

// EnterTurn marks the calling goroutine as executing event-loop or connection-turn
// work. Nested EnterTurn/LeaveTurn pairs on the same goroutine are reference-counted.
func EnterTurn() {
	id := goid.Get()
	b := &turnTable[uint64(id)%turnBuckets]
	var free *turnEntry
	for e := b.head.Load(); e != nil; e = e.next.Load() {
		if oid := e.goid.Load(); oid == id {
			e.depth.Add(1)
			return
		} else if oid == 0 && free == nil {
			free = e
		}
	}
	for e := free; e != nil; e = e.next.Load() {
		if e.goid.CompareAndSwap(0, id) {
			e.depth.Store(1)
			return
		}
	}
	e := &turnEntry{}
	e.goid.Store(id)
	e.depth.Store(1)
	for {
		old := b.head.Load()
		e.next.Store(old)
		if b.head.CompareAndSwap(old, e) {
			return
		}
	}
}

// LeaveTurn unmarks one EnterTurn call on the calling goroutine.
func LeaveTurn() {
	id := goid.Get()
	b := &turnTable[uint64(id)%turnBuckets]
	for e := b.head.Load(); e != nil; e = e.next.Load() {
		if e.goid.Load() == id {
			if e.depth.Add(-1) <= 0 {
				e.goid.Store(0)
			}
			return
		}
	}
}

// OnTurn reports whether the calling goroutine is registered as a turn or
// event-loop goroutine.
func OnTurn() bool {
	id := goid.Get()
	b := &turnTable[uint64(id)%turnBuckets]
	for e := b.head.Load(); e != nil; e = e.next.Load() {
		if e.goid.Load() == id {
			return true
		}
	}
	return false
}
