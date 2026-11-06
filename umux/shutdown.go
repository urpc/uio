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
	"context"

	"github.com/urpc/uio"
)

// Shutdown drains the session gracefully. It sends a GoAway, refuses new
// streams — Open fails with ErrGoAway and the peer's opens are reset — and
// leaves the existing streams fully working until each has ended on its own:
// both directions drained, the FINs exchanged and accepted by the transport.
// A stream that never ends is reset with ErrShutdownTimeout when ctx ends,
// and the transport closes once the last stream leaves the session.
//
// It returns nil when every stream settled before ctx ended, ctx's error when
// the wait was cut short, and the transport's cause when the transport died
// first. Concurrent calls share one drain; Close aborts the drain at once.
//
// A nil ctx waits indefinitely. graceful close is cooperative: umux cannot
// know when an application's byte stream is complete, so streams the caller
// (or its peer) never end are the shutdown context's to cut.
func (s *Session) Shutdown(ctx context.Context) error {
	if s.onOwnTurn() {
		return uio.ErrBlockingOnTurn
	}
	if ctx == nil {
		ctx = context.Background()
	}
	s.mu.Lock()
	if s.draining {
		done := s.drainDone
		s.mu.Unlock()
		return s.waitDrain(ctx, done)
	}
	if s.closed {
		err := s.closeErr
		s.mu.Unlock()
		return err
	}
	s.draining = true
	s.drainDone = make(chan struct{})
	s.goAway = true
	if !s.goAwaySent {
		s.goAwaySent = true
		s.sendControlLocked(typeGoAway, 0, 0, goAwayNormal)
	}
	done := s.drainDone
	ferr := s.flushLocked()
	s.mu.Unlock()
	s.emitDrained()
	if ferr != nil {
		// The transport is broken; its close path ends the session and the
		// drain with it.
		_ = s.conn.Abort(ferr)
	}
	// A session with nothing to drain closes as soon as the GoAway was
	// accepted.
	s.maybeFinishDrain()
	return s.waitDrain(ctx, done)
}

// waitDrain blocks until the drain ends or ctx does, forcing the rest on ctx.
func (s *Session) waitDrain(ctx context.Context, done <-chan struct{}) error {
	select {
	case <-done:
		s.mu.Lock()
		err := s.drainErr
		s.mu.Unlock()
		return err
	case <-ctx.Done():
		s.forceDrain(ctx.Err())
		return ctx.Err()
	}
}

// forceDrain stops waiting: every remaining stream is reset with
// ErrShutdownTimeout and the transport closes with the shutdown's cause.
func (s *Session) forceDrain(cause error) {
	s.mu.Lock()
	if !s.draining || s.drainClosed {
		s.mu.Unlock()
		return
	}
	streams := make([]*Stream, 0, len(s.streams))
	for _, st := range s.streams {
		streams = append(streams, st)
	}
	s.finishDrainLocked(ErrShutdownTimeout)
	s.mu.Unlock()
	for _, st := range streams {
		s.abortStream(st, ErrShutdownTimeout)
	}
	_ = s.conn.Abort(cause)
}

// maybeFinishDrain closes the transport once a draining session settled every
// stream and the transport accepted its GoAway. It is called after each
// stream leaves the session and after a flush, from any goroutine; the lock is
// released before the transport is touched.
func (s *Session) maybeFinishDrain() {
	s.mu.Lock()
	ready := s.draining && !s.drainClosed && !s.closed &&
		len(s.streams) == 0 && s.goAwayFlushed
	if ready {
		// Finish first: closing the transport normally ends with a
		// transport-error callback, and a clean drain must report nil.
		s.finishDrainLocked(nil)
	}
	s.mu.Unlock()
	if !ready {
		return
	}
	_ = s.conn.Close()
}

// finishDrainLocked closes the drain's done channel once and records how it
// ended. The caller holds s.mu.
func (s *Session) finishDrainLocked(err error) {
	if !s.draining || s.drainClosed {
		return
	}
	s.drainClosed = true
	s.drainErr = err
	close(s.drainDone)
}

// handleGoAway records the peer's refusal of new streams. Existing streams
// keep running; the peer closes the transport once they end.
func (s *Session) handleGoAway(frameHeader) error {
	s.mu.Lock()
	s.goAway = true
	s.mu.Unlock()
	return nil
}
