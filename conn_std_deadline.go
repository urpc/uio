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

// Deadlines on the portable backend: the socket deadline mirror, the write
// liveness timer, and the child connections' deadlines.

package uio

import (
	"fmt"
	"net"
	"os"
	"time"
)

func (fc *fdConn) SetDeadline(deadline time.Time) error {
	return fc.applyDeadline(deadlineBoth, deadline)
}

func (fc *fdConn) SetReadDeadline(deadline time.Time) error {
	return fc.applyDeadline(deadlineRead, deadline)
}

func (fc *fdConn) SetWriteDeadline(deadline time.Time) error {
	return fc.applyDeadline(deadlineWrite, deadline)
}

// applyDeadline delegates directly to the blocking net.Conn; unlike native
// timers, the Go network poller owns interruption and reset semantics. A read
// deadline the socket accepted is also recorded for a held read, which reads
// no socket. deadlineMu orders the two updates against other setters and
// against the held wait's expiry, which closes the connection under it: a
// setter either finds the connection closing or replaces the deadline before
// the wait can act on the old one.
func (fc *fdConn) applyDeadline(kind deadlineKind, deadline time.Time) error {
	if fc.isClosing() {
		return net.ErrClosed
	}
	if fc.udp != nil && fc.udp.svr != nil {
		return fc.applyChildDeadline(kind, deadline)
	}
	var target net.Conn
	if fc.conn != nil {
		target = fc.conn
	} else if fc.udp != nil {
		target = fc.udp.sock
	}
	if target == nil {
		return ErrUnsupported
	}
	fc.deadlineMu.Lock()
	defer fc.deadlineMu.Unlock()
	if fc.isClosing() {
		return net.ErrClosed
	}
	var err error
	switch kind {
	case deadlineBoth:
		err = target.SetDeadline(deadline)
	case deadlineRead:
		err = target.SetReadDeadline(deadline)
	default:
		err = target.SetWriteDeadline(deadline)
	}
	if err != nil {
		return err
	}
	if kind == deadlineWrite || kind == deadlineBoth {
		fc.writeDeadline = deadline
		fc.writeGeneration++
		generation := fc.writeGeneration
		if fc.writeTimer != nil {
			fc.writeTimer.Stop()
			fc.writeTimer = nil
		}
		if !deadline.IsZero() {
			fc.writeTimer = time.AfterFunc(time.Until(deadline), func() { fc.expireWriteDeadline(generation) })
		}
		if kind == deadlineWrite {
			return nil
		}
	}
	if fc.conn == nil {
		// A connected datagram connection has no held read to wake: its
		// reads block on the socket, which carries the deadline itself. The
		// timer above is what keeps an idle write deadline live for it.
		return nil
	}
	if readDeadlineNotedForTest != nil {
		readDeadlineNotedForTest()
	}
	fc.readDeadline = deadline
	// Wake a held wait to re-time against the new deadline.
	select {
	case fc.readResume <- struct{}{}:
	default:
	}
	return nil
}

// childDeadlines times a UDP server child's deadlines, under the child's
// deadlineMu. The child reads no socket of its own — its server's listener
// reads the shared one for every peer — so a deadline there would end the
// server and all its peers. As on the native backend, a deadline that passes
// closes only this child, with an error that wraps os.ErrDeadlineExceeded.
type childDeadlines struct {
	stopped bool
	read    childDeadline
	write   childDeadline
}

type childDeadline struct {
	at         time.Time
	generation uint64
	timer      *time.Timer
}

func (fc *fdConn) applyChildDeadline(kind deadlineKind, deadline time.Time) error {
	fc.deadlineMu.Lock()
	defer fc.deadlineMu.Unlock()
	state := fc.udp.deadlines
	if state == nil {
		if deadline.IsZero() {
			return nil
		}
		state = &childDeadlines{}
		fc.udp.deadlines = state
	}
	if state.stopped || fc.isClosing() {
		return net.ErrClosed
	}
	if kind == deadlineBoth || kind == deadlineRead {
		fc.armChildDeadline(&state.read, deadlineRead, deadline)
	}
	if kind == deadlineBoth || kind == deadlineWrite {
		fc.armChildDeadline(&state.write, deadlineWrite, deadline)
	}
	return nil
}

// armChildDeadline replaces one deadline; it requires deadlineMu. The
// generation makes a timer that already fired for the old deadline harmless.
func (fc *fdConn) armChildDeadline(slot *childDeadline, kind deadlineKind, deadline time.Time) {
	slot.generation++
	slot.at = deadline
	if slot.timer != nil {
		slot.timer.Stop()
	}
	if deadline.IsZero() {
		return
	}
	generation := slot.generation
	slot.timer = time.AfterFunc(max(time.Until(deadline), 0), func() {
		fc.expireChildDeadline(slot, kind, generation)
	})
}

func (fc *fdConn) expireChildDeadline(slot *childDeadline, kind deadlineKind, generation uint64) {
	fc.deadlineMu.Lock()
	expired := !fc.udp.deadlines.stopped && slot.generation == generation &&
		!slot.at.IsZero() && !time.Now().Before(slot.at)
	fc.deadlineMu.Unlock()
	if expired {
		fc.requestClose(fmt.Errorf("uio: %s deadline: %w", deadlineName(kind), os.ErrDeadlineExceeded))
	}
}

// stopChildDeadlines cancels a closing child's timers and refuses new ones.
func (fc *fdConn) stopChildDeadlines() {
	fc.deadlineMu.Lock()
	defer fc.deadlineMu.Unlock()
	state := fc.udp.deadlines
	if state == nil {
		return
	}
	state.stopped = true
	for _, slot := range []*childDeadline{&state.read, &state.write} {
		if slot.timer != nil {
			slot.timer.Stop()
		}
	}
}

// closeOnLoop owns resource teardown and detaches UDP children. The user
// callback is scheduled separately so a slow OnClose cannot block the logical
// event loop or race an active OnRead callback.
//
// Closers arrive from several goroutines at once — a Listener.Close on its
// caller's, Events.Close, a loop's close task — so fc.mux claims the close
// exclusively: the first one in publishes the reason and tears down, and the
// rest find the connection closed and return without touching anything. The
// reason publishes before the closed flag: a reader that sees closed observes
// err without further synchronization.
// expireWriteDeadline runs on the write deadline's timer: a deadline that
// passed closes the connection, write in flight or not, as the native
// backend's does. The generation and the wall clock are both rechecked, since
// a setter may have replaced or cleared the deadline as the timer fired.
func (fc *fdConn) expireWriteDeadline(generation uint64) {
	fc.deadlineMu.Lock()
	deadline, current := fc.writeDeadline, fc.writeGeneration
	fc.deadlineMu.Unlock()
	if fc.isClosing() || generation != current || deadline.IsZero() || time.Now().Before(deadline) {
		return
	}
	fc.requestClose(fmt.Errorf("uio: %s deadline: %w", deadlineName(deadlineWrite), os.ErrDeadlineExceeded))
}

// stopWriteDeadline disarms the write deadline's timer as the connection
// closes, so a far deadline does not outlive it.
func (fc *fdConn) stopWriteDeadline() {
	fc.deadlineMu.Lock()
	if fc.writeTimer != nil {
		fc.writeTimer.Stop()
		fc.writeTimer = nil
	}
	fc.deadlineMu.Unlock()
}
