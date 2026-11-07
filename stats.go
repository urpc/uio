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

import "sync/atomic"

// Stats is a point-in-time, zero-allocation runtime snapshot of an [Events]
// instance. All fields are read lock-free from per-loop and per-shard counters,
// adding zero allocations and zero data-path contention.
type Stats struct {
	// Pollers is the number of active event loops (0 before Serve starts).
	Pollers int
	// ActiveConnections is the number of currently open connections (stream
	// connections and active UDP peer connections).
	ActiveConnections int64
	// TotalConnections counts connections that have opened since Serve started.
	TotalConnections uint64
	// TurnQueue is the number of connection turns currently queued in UIO's
	// owned scheduler (0 on the stdio backend or with a custom Executor).
	TurnQueue int64
	// WriteQueue is the number of background write turns currently queued in
	// UIO's owned scheduler.
	WriteQueue int64
	// ActiveTurnShards is the active shard count of UIO's owned turn scheduler.
	ActiveTurnShards int
	// RunningWorkers is the number of owned scheduler workers currently
	// executing connection or write turns.
	RunningWorkers int
	// IdleWorkers is the number of parked owned scheduler workers ready to
	// serve turns without spawning a goroutine.
	IdleWorkers int
	// OutboundOverflows counts writes rejected with ErrOutboundOverflow.
	OutboundOverflows uint64
}

// loopStats is embedded in each eventLoop on its own cache line so connection
// open/close and cold-path overflow counters never contend across loops or
// pollute hot poller state.
type loopStats struct {
	_                 [64]byte
	activeConns       atomic.Int64
	totalConns        atomic.Uint64
	outboundOverflows atomic.Uint64
}

func (ls *loopStats) noteConnOpened() {
	ls.activeConns.Add(1)
	ls.totalConns.Add(1)
}

func (ls *loopStats) noteConnClosed() {
	ls.activeConns.Add(-1)
}

func (ls *loopStats) noteOutboundOverflow() {
	ls.outboundOverflows.Add(1)
}

func (fc *commonConn) errOutboundOverflow() error {
	if fc.loop != nil {
		fc.loop.stats.noteOutboundOverflow()
	}
	return ErrOutboundOverflow
}

// Stats returns a point-in-time runtime snapshot of ev without allocating or
// taking any mutex. Safe to call concurrently from any goroutine before,
// during, or after Serve.
func (ev *Events) Stats() Stats {
	if ev == nil || !ev.ready.Load() {
		return Stats{}
	}
	return ev.collectStats()
}
