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

// Read and write deadlines: their timers, generations and the connection
// timeout run on the turn.

package uio

import (
	"fmt"
	"net"
	"os"
	"sync/atomic"
	"time"
)

// deadlineState is guarded by submitMu except for its atomic timer generations.
type deadlineState struct {
	readTimer       *time.Timer
	writeTimer      *time.Timer
	readGeneration  uint64
	writeGeneration uint64
	readTimerGen    atomic.Uint64
	writeTimerGen   atomic.Uint64
	readDeadline    time.Time
	writeDeadline   time.Time
}

func (conn *fdConn) SetDeadline(deadline time.Time) error {
	return conn.setDeadline(deadlineBoth, deadline)
}

func (conn *fdConn) SetReadDeadline(deadline time.Time) error {
	return conn.setDeadline(deadlineRead, deadline)
}

func (conn *fdConn) SetWriteDeadline(deadline time.Time) error {
	return conn.setDeadline(deadlineWrite, deadline)
}

func (conn *fdConn) setDeadline(kind deadlineKind, deadline time.Time) error {
	if conn.isClosing() {
		return net.ErrClosed
	}
	return conn.applyDeadline(kind, deadline)
}

// applyDeadline updates the deadline state under submitMu and advances a
// generation. A timer callback carries that generation so a stale callback
// cannot close a connection after the deadline was reset or cleared.
func (conn *fdConn) applyDeadline(kind deadlineKind, deadline time.Time) error {
	conn.submitMu.Lock()
	defer conn.submitMu.Unlock()
	if conn.close.isReleased() {
		return net.ErrClosed
	}
	state := conn.deadlines
	if state == nil {
		if deadline.IsZero() {
			return nil
		}
		state = &deadlineState{}
		conn.deadlines = state
	}
	// Generations make callbacks from a stopped or reset timer harmless.
	if kind == deadlineBoth || kind == deadlineRead {
		state.readGeneration++
		state.readDeadline = deadline
		state.readTimerGen.Store(state.readGeneration)
		state.readTimer = conn.resetDeadlineTimer(state.readTimer, deadlineRead, deadline)
	}
	if kind == deadlineBoth || kind == deadlineWrite {
		state.writeGeneration++
		state.writeDeadline = deadline
		state.writeTimerGen.Store(state.writeGeneration)
		state.writeTimer = conn.resetDeadlineTimer(state.writeTimer, deadlineWrite, deadline)
	}
	return nil
}

func (conn *fdConn) resetDeadlineTimer(timer *time.Timer, kind deadlineKind, deadline time.Time) *time.Timer {
	if timer != nil {
		timer.Stop()
	}
	if deadline.IsZero() {
		return timer
	}
	delay := time.Until(deadline)
	if delay < 0 {
		delay = 0
	}
	if timer == nil {
		return time.AfterFunc(delay, func() { conn.expireDeadline(kind) })
	}
	timer.Reset(delay)
	return timer
}

// expireDeadline runs on the timer's goroutine and closes the connection if
// the deadline the timer was armed for still stands.
func (conn *fdConn) expireDeadline(kind deadlineKind) {
	conn.submitMu.Lock()
	state := conn.deadlines
	if conn.isClosing() || state == nil {
		conn.submitMu.Unlock()
		return
	}
	generation := state.writeTimerGen.Load()
	if kind == deadlineRead {
		generation = state.readTimerGen.Load()
	}
	conn.submitMu.Unlock()
	conn.handleTimeout(kind, generation)
}

// handleTimeout rechecks both generation and wall time. The wall-time check
// covers a reset racing with a timer that already fired.
func (conn *fdConn) handleTimeout(kind deadlineKind, generation uint64) {
	conn.submitMu.Lock()
	state := conn.deadlines
	if conn.isClosing() || state == nil {
		conn.submitMu.Unlock()
		return
	}
	var current uint64
	var deadline time.Time
	if kind == deadlineRead {
		current, deadline = state.readGeneration, state.readDeadline
	} else {
		current, deadline = state.writeGeneration, state.writeDeadline
	}
	conn.submitMu.Unlock()
	// Reset races can submit the current generation early, so check time too.
	if generation != current || deadline.IsZero() || time.Now().Before(deadline) {
		return
	}
	conn.requestClose(fmt.Errorf("uio: %s deadline: %w", deadlineName(kind), os.ErrDeadlineExceeded))
}

func (conn *fdConn) stopDeadlines() {
	conn.submitMu.Lock()
	defer conn.submitMu.Unlock()
	state := conn.deadlines
	if state == nil {
		return
	}
	if state.readTimer != nil {
		state.readTimer.Stop()
	}
	if state.writeTimer != nil {
		state.writeTimer.Stop()
	}
}
