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
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/urpc/uio/internal/timewheel"
)

// wakeState is a connection's wake timer: one entry on its loop's wheel. The
// wheel's own membership is the timer's validity — Arm detaches before it
// re-arms, so a due entry the wheel reports is the deadline that was armed —
// and the wake turn's existing guards (isClosing, the held-read pause) carry
// the rest. The wheel's mutex serializes its lists; submitMu guards only the
// entry's allocation. A setter that loses the race with the close path —
// armed after the close's cancel ran — undoes itself by re-checking
// isClosing after the arm.
type wakeState struct {
	entry *timewheel.Entry[*fdConn]
	// mu holds the wheel's arm and the mirror — armed, due and period — as
	// one step, so a setter and a consuming turn can never see the pair
	// split: a due published with a stale period would consume the timer as
	// a one-shot and stop an interval forever. Only conns that arm a wake
	// timer pay for any of this. The user callback never runs under mu.
	mu     sync.Mutex
	armed  bool
	due    int64 // unix nanos; 0 is a legal deadline, so armed says whether one is set
	period int64
	// drain bounds a graceful close's wait for its output to leave. It lives
	// on this lazily-allocated state, not the connection, so a connection
	// that never closes gracefully pays nothing. The pointer is set once, by
	// the close that claimed the drain, and swapped away by whatever ends it;
	// the record is read-only after that, so progress may reset the bound
	// from any goroutine without a lock.
	drain atomic.Pointer[drainBound]
}

// drainBound is a graceful close's timer and the span it waits for progress.
type drainBound struct {
	timer *time.Timer
	wait  time.Duration
}

// SetWakeDeadline arms the connection's wake timer for when; see the Conn
// interface for the contract. A zero time cancels.
func (conn *fdConn) SetWakeDeadline(when time.Time) error {
	if conn.isClosing() || conn.loop == nil {
		return net.ErrClosed
	}
	if when.IsZero() {
		conn.cancelWakeTimer()
		return nil
	}
	conn.armWake(when, 0)
	if conn.isClosing() {
		conn.cancelWakeTimer()
	}
	return nil
}

// SetWakeInterval arms the connection's wake timer to fire every interval;
// see the Conn interface for the contract. A zero or negative interval
// cancels.
func (conn *fdConn) SetWakeInterval(interval time.Duration) error {
	if conn.isClosing() || conn.loop == nil {
		return net.ErrClosed
	}
	if interval <= 0 {
		conn.cancelWakeTimer()
		return nil
	}
	conn.armWake(time.Now().Add(interval), interval)
	if conn.isClosing() {
		conn.cancelWakeTimer()
	}
	return nil
}

// wakeState returns the connection's wake state, allocated on first use
// under submitMu so two setters cannot allocate two.
func (conn *fdConn) wakeState() *wakeState {
	conn.submitMu.Lock()
	defer conn.submitMu.Unlock()
	if w := conn.wake.Load(); w != nil {
		return w
	}
	w := &wakeState{entry: &timewheel.Entry[*fdConn]{Val: conn}}
	conn.wake.Store(w)
	return w
}

// fireWakeTimer runs the connection's own timer callback when its deadline
// passed, computing due against the clock like a stack's takeDue does: an
// earlier wake or read runs it just the same. It costs nothing without an
// OnTimer. skipRead reports a close the timer callback made, which asked for
// no further callbacks on the connection.
func (conn *fdConn) fireWakeTimer() (skipRead bool, err error) {
	if conn.callbacks().onTimer == nil {
		return false, nil
	}
	wake := conn.wake.Load()
	if wake == nil {
		return false, nil
	}
	wake.mu.Lock()
	if !wake.armed {
		wake.mu.Unlock()
		return false, nil
	}
	now := time.Now().UnixNano()
	if now < wake.due {
		wake.mu.Unlock()
		return false, nil
	}
	if wake.period > 0 {
		// Skip the missed periods arithmetically: a tight interval backed up
		// behind a long stall must not loop them off one by one.
		wake.due += ((now-wake.due)/wake.period + 1) * wake.period
	} else {
		wake.armed = false
	}
	wake.mu.Unlock()
	if conn.isClosing() {
		return false, nil
	}
	if err := conn.callbacks().onTimer(conn); err != nil {
		return true, err
	}
	return conn.isClosing(), nil
}

// armWake arms the wheel and sets the mirror as one step, so a concurrent
// setter or a consuming turn cannot split them.
func (conn *fdConn) armWake(when time.Time, every time.Duration) {
	w := conn.wakeState()
	w.mu.Lock()
	conn.loop.wakeWheel().Arm(w.entry, when, every)
	w.armed, w.due, w.period = true, when.UnixNano(), int64(every)
	w.mu.Unlock()
}

// cancelWakeTimer removes the entry from its wheel; the close path calls it
// once the connection's last flush has left, beside stopDeadlines.
func (conn *fdConn) cancelWakeTimer() {
	wake := conn.wake.Load()
	if wake == nil || conn.loop == nil {
		return
	}
	wake.mu.Lock()
	wake.armed = false
	conn.loop.wakeWheel().Cancel(wake.entry)
	wake.mu.Unlock()
}

// expireWakeTimers collects the wheel's due deadlines and Wakes each
// connection — through Wake, which routes a datagram server's child to the
// server's own turn, so one peer's callbacks never overlap another's. It
// runs on whichever waiter goroutine the armed kernel timer woke. A due
// entry of a closing connection is cancelled, not skipped: a periodic entry
// would otherwise re-arm itself forever.
func (loop *eventLoop) expireWakeTimers() {
	wheel := loop.wakeWheel()
	// Waiters of one loop can expire at once, so the slice comes from a pool
	// rather than the loop; it goes back cleared, holding no connection.
	buffer := dueEntries.Get().(*[]*timewheel.Entry[*fdConn])
	due := wheel.AdvanceInto(time.Now(), (*buffer)[:0])
	for _, e := range due {
		conn := e.Val
		if conn.isClosing() {
			wheel.Cancel(e)
			continue
		}
		_ = conn.Wake()
	}
	clear(due)
	*buffer = due[:0]
	dueEntries.Put(buffer)
}

// dueEntries recycles the slices expireWakeTimers collects into.
var dueEntries = sync.Pool{New: func() any {
	due := make([]*timewheel.Entry[*fdConn], 0, 64)
	return &due
}}

// wakeWheel returns the loop's wheel, built on first use: a loop whose
// connections arm no wake timers holds none. The wheel's arm callback runs
// under the wheel's own mutex; the fallback timer it may use lives under
// wakeMu, so a runtime-timer wake and the wheel it serves cannot cross.
func (loop *eventLoop) wakeWheel() *timewheel.Wheel[*fdConn] {
	loop.wakeMu.Lock()
	defer loop.wakeMu.Unlock()
	if loop.wake != nil {
		return loop.wake
	}
	loop.wake = timewheel.New[*fdConn](wheelTick, wheelBuckets, time.Now(), loop.armWakeTimerLocked)
	return loop.wake
}

// armWakeTimerLocked aims the kernel timer that wakes the loop at the
// wheel's earliest deadline — a timerfd on epoll, an EVFILT_TIMER kevent on
// kqueue — whose expiry arrives through the poller's wait like any
// readiness; a poller with no kernel timer falls back to a runtime timer
// with the same wake. A deadline already past still arms the timer at the
// retry granularity: disarming here would strand an entry parked inside the
// wheel's current tick, whose time only this timer's expiry would reveal.
// It must not block: it runs under the wheel's mutex.
func (loop *eventLoop) armWakeTimerLocked(when time.Time) {
	delay := time.Duration(0)
	if !when.IsZero() {
		delay = time.Until(when)
		if delay < wakeRetryDelay {
			delay = wakeRetryDelay
		}
	}
	// Any failure falls back, not only the missing-kernel sentinel: a
	// one-shot kernel timer that failed to arm idles this loop's wake timers
	// until the next arm, so the runtime timer takes the wake instead.
	if err := armKernelTimer(loop, delay); err == nil {
		loop.stopRuntimeWakeTimer()
		return
	}
	loop.armRuntimeWakeTimer(delay)
}

// armKernelTimerOverride is the test seam for armKernelTimer: a non-nil
// function replaces the kernel step, forcing the runtime fallback. It is
// atomic so parallel tests cannot race it.
var armKernelTimerOverride atomic.Pointer[func(*eventLoop, time.Duration) error]

func armKernelTimer(loop *eventLoop, delay time.Duration) error {
	if hook := armKernelTimerOverride.Load(); hook != nil {
		return (*hook)(loop, delay)
	}
	return loop.poller.ArmTimer(delay)
}

// stopRuntimeWakeTimer retires a fallback timer after a kernel arm
// succeeded: it may fire once more, harmlessly, before this stops it.
func (loop *eventLoop) stopRuntimeWakeTimer() {
	loop.wakeMu.Lock()
	if loop.wakeTimer != nil {
		loop.wakeTimer.Stop()
		loop.wakeTimer = nil
	}
	loop.wakeMu.Unlock()
}

// armRuntimeWakeTimer is the fallback a kernel without its timer carries:
// one runtime timer per armed wheel, run under wakeMu with the wheel it
// serves.
func (loop *eventLoop) armRuntimeWakeTimer(delay time.Duration) {
	loop.wakeMu.Lock()
	defer loop.wakeMu.Unlock()
	if delay <= 0 {
		if loop.wakeTimer != nil {
			loop.wakeTimer.Stop()
			loop.wakeTimer = nil
		}
		return
	}
	if loop.wakeTimer == nil {
		loop.wakeTimer = time.AfterFunc(delay, loop.expireWakeTimers)
		return
	}
	loop.wakeTimer.Reset(delay)
}

const (
	// wheelTick is the time one wheel slot covers. Wake timers are
	// connection-level — heartbeats, protocol updates, idle timeouts — so
	// millisecond-scale slots size them, and finer deadlines land within one
	// slot of their arm.
	wheelTick = 4 * time.Millisecond
	// wheelBuckets is the ring's slots: at wheelTick, a span of about 65 s,
	// which keeps a typical heartbeat or idle timeout in the ring and the
	// overflow heap for the deadlines beyond it.
	wheelBuckets = 16384
	// wakeRetryDelay is what a deadline already past arms the kernel timer
	// at: the wheel reports it on the expiry's Advance, so the entry is at
	// most one retry late.
	wakeRetryDelay = time.Millisecond
)
