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

package umux

import (
	"io"
	"time"

	"github.com/urpc/uio"
)

// rearmTimer points the transport's wake deadline at the earliest stream timer
// or the keepalive. Safe from any goroutine. The deadline is computed and
// submitted under one lock: two concurrent arms that computed at different
// times must not let the older value land last and cancel a live timer.
func (s *Session) rearmTimer() {
	if s.conn == nil {
		return
	}
	s.timerMu.Lock()
	defer s.timerMu.Unlock()
	now := time.Now()
	s.mu.Lock()
	next := s.nextTimerLocked(now)
	s.mu.Unlock()
	s.armedAt = next
	_ = s.conn.SetWakeDeadline(next)
}

// armNotLaterThan makes the transport fire by when at the latest, skipping the
// full rescan when a wake no later than when is already armed. Opening a
// stream uses it: scanning every stream per Open turns an open-rate workload
// into quadratic work.
func (s *Session) armNotLaterThan(when time.Time) {
	if s.conn == nil || when.IsZero() {
		return
	}
	s.timerMu.Lock()
	defer s.timerMu.Unlock()
	if !s.armedAt.IsZero() && !s.armedAt.After(when) {
		return
	}
	s.armedAt = when
	_ = s.conn.SetWakeDeadline(when)
}

// queuePending puts a stream on the session's round-robin flush queue.
func (s *Session) queuePending(id uint32) {
	s.mu.Lock()
	s.pending = append(s.pending, id)
	s.mu.Unlock()
}

// onTimer fires due stream timers, open and half-close timeouts, and the
// keepalive. It runs on the transport's turn.
func (s *Session) onTimer(c uio.Conn) error {
	s.timerMu.Lock()
	s.armedAt = time.Time{} // the armed wake just fired
	s.timerMu.Unlock()
	now := time.Now()
	var (
		due        []*Stream
		openFails  []chan error
		resetCause = make(map[*Stream]error)
	)
	s.mu.Lock()
	for id, st := range s.streams {
		st.mu.Lock()
		if !st.wakeAt.IsZero() && !now.Before(st.wakeAt) {
			st.wakeAt = time.Time{}
			if st.wakeDu > 0 {
				st.wakeAt = now.Add(st.wakeDu)
			}
			due = append(due, st)
		}
		opening := st.opening && now.Sub(st.openedAt) > s.cfg.StreamOpenTimeout
		half := st.peerFIN != st.localFIN && !st.halfSince.IsZero() &&
			now.Sub(st.halfSince) > s.cfg.StreamCloseTimeout
		st.mu.Unlock()
		if opening {
			delete(s.streams, id)
			st.dropOutLocked()
			if wait := s.openWait[id]; wait != nil {
				delete(s.openWait, id)
				openFails = append(openFails, wait)
			}
			resetCause[st] = ErrStreamOpenTimeout
			continue
		}
		if half {
			delete(s.streams, id)
			st.dropOutLocked()
			s.sendControlLocked(typeWindowUpdate, flagRST, id, 0)
			resetCause[st] = ErrStreamReset
		}
	}
	keepalivePing, keepaliveDead := false, false
	if s.cfg.KeepAlive > 0 && !s.closed {
		if s.pingSent.IsZero() && now.Sub(s.lastPing) >= s.cfg.KeepAlive {
			s.pingSent = now
			s.lastPing = now
			s.pingSeq++
			keepalivePing = true
		} else if !s.pingSent.IsZero() && now.Sub(s.pingSent) > s.cfg.KeepAliveMax {
			keepaliveDead = true
		}
	}
	if keepalivePing {
		s.sendControlLocked(typePing, flagSYN, 0, s.pingSeq)
	}
	err := s.flushLocked()
	handler := s.cfg.Handler
	s.mu.Unlock()

	// The timed-out streams and their waiters left the tables above, so the
	// transport's close path will never find them: release them here, before
	// any early return, or an Open would wait forever.
	for _, wait := range openFails {
		select {
		case wait <- ErrStreamOpenTimeout:
		default:
		}
	}
	for st, cause := range resetCause {
		st.resetLocal(cause)
	}
	if keepaliveDead {
		s.drainCloses()
		_ = c.CloseWith(ErrKeepAlive)
		return ErrKeepAlive
	}
	if err != nil {
		s.drainCloses()
		_ = c.CloseWith(err)
		return err
	}
	for _, st := range due {
		if handler != nil {
			if terr := handler.OnTimer(st); terr != nil {
				s.abortStream(st, terr)
			}
		}
	}
	// Re-arm from the state the callbacks left: a callback may have set a new
	// deadline, and a deadline computed before them would overwrite it.
	s.rearmTimer()
	s.drainCloses()
	return nil
}

// nextTimerLocked returns the earliest armed timer, or the zero time when the
// session has none. The caller holds s.mu; stream locks nest inside it.
func (s *Session) nextTimerLocked(now time.Time) time.Time {
	var next time.Time
	after := func(t time.Time) {
		if t.IsZero() {
			return
		}
		if next.IsZero() || t.Before(next) {
			next = t
		}
	}
	for _, st := range s.streams {
		st.mu.Lock()
		after(st.wakeAt)
		if st.opening {
			after(st.openedAt.Add(s.cfg.StreamOpenTimeout))
		}
		if st.peerFIN != st.localFIN && !st.halfSince.IsZero() {
			after(st.halfSince.Add(s.cfg.StreamCloseTimeout))
		}
		st.mu.Unlock()
	}
	if s.cfg.KeepAlive > 0 && !s.closed {
		if s.pingSent.IsZero() {
			after(s.lastPing.Add(s.cfg.KeepAlive))
		} else {
			after(s.pingSent.Add(s.cfg.KeepAliveMax))
		}
	}
	// A deadline already due fires on the next turn.
	if !next.IsZero() && !next.After(now) {
		next = now.Add(time.Millisecond)
	}
	return next
}

// onTransportClose closes every stream with the transport's cause.
func (s *Session) onTransportClose(err error) {
	if err == nil {
		err = io.EOF
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	s.closeErr = err
	// No transport turn can consume these again. Drop their references even
	// when the final timer/flush pass failed after submitting earlier frames.
	s.finFlushed = nil
	s.drained = nil
	s.wakeQ = nil
	s.control = nil
	s.pending = nil
	s.readHeld = false
	s.flushReq.Store(false)
	if s.done != nil {
		close(s.done)
	}
	if s.acceptCh != nil {
		close(s.acceptCh)
	}
	streams := make([]*Stream, 0, len(s.streams))
	for id, st := range s.streams {
		streams = append(streams, st)
		delete(s.streams, id)
		st.dropOutLocked()
	}
	var waits []chan error
	for id, wait := range s.openWait {
		delete(s.openWait, id)
		waits = append(waits, wait)
	}
	s.mu.Unlock()

	for _, wait := range waits {
		select {
		case wait <- err:
		default:
		}
	}
	for _, st := range streams {
		st.resetRemote(err)
	}
	s.drainCloses()
}
