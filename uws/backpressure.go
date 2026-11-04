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

package uws

import (
	"errors"
	"net"
	"sync"

	"github.com/urpc/uio/internal/readcontrol"
)

// The bits of Conn.readPaused.
const (
	readPauseInternal uint32 = 1 << iota // the automatic reply watermark's
	readPauseExternal                    // the application's
)

// connReadPauses holds this connection's two pauses of the transport, each
// its own owner there: neither ever releases the other, nor a pause another
// owner of the transport holds. The mutex orders each pause's transition and
// its mirror bit in Conn.readPaused against a local Close; it covers no
// protocol callback and no socket I/O.
type connReadPauses struct {
	mu       sync.Mutex
	external *readcontrol.Pause
	internal *readcontrol.Pause
}

func (c *Conn) ensureReadPauses() *connReadPauses {
	metadata := c.ensureMetadata()
	if pauses := metadata.readPauses.Load(); pauses != nil {
		return pauses
	}
	pauses := &connReadPauses{external: readcontrol.NewPause(c.raw), internal: readcontrol.NewPause(c.raw)}
	if metadata.readPauses.CompareAndSwap(nil, pauses) {
		return pauses
	}
	return metadata.readPauses.Load()
}

func (c *Conn) loadReadPauses() *connReadPauses {
	if metadata := c.metadata.Load(); metadata != nil {
		return metadata.readPauses.Load()
	}
	return nil
}

// PauseReads stops consuming incoming WebSocket frames until ResumeReads.
// It is safe from any goroutine and from OnOpen or OnMessage. A callback
// already in progress may finish; a pause made by OnMessage takes effect
// before the next frame, including frames buffered in the same socket read.
// The frames wait in the transport, which stops reading a plain socket at
// once and keeps the end of the stream behind them: a peer that sends and
// then closes its side is answered after ResumeReads, so a paused connection
// whose peer left lingers until then, or until a heartbeat or read deadline
// ends it. Writes and timers continue normally. This also works over TLS.
//
// Pausing delays control frames too, including Ping and Pong. Heartbeat and
// close deadlines still run; starting a local Close overrides the pause so
// the close handshake can finish. Calls are idempotent, not reference
// counted: callers sharing this switch must coordinate their ownership.
// The connection must have opened and must not be closing or closed.
func (c *Conn) PauseReads() error {
	if err := c.checkReadControl(); err != nil {
		return err
	}
	pauses := c.ensureReadPauses()
	pauses.mu.Lock()
	defer pauses.mu.Unlock()
	if err := c.checkReadControl(); err != nil {
		return err
	}
	if err := pauses.external.Pause(); err != nil {
		return readControlError(err)
	}
	c.readPaused.Or(readPauseExternal)
	return nil
}

// ResumeReads clears the pause requested by PauseReads and schedules a read
// turn, so already buffered frames need no new network input to be
// delivered. Without that pause it does nothing. It leaves automatic
// watermark backpressure in effect. It is safe from any goroutine, including
// TrafficObserver.OnOutbound, and is idempotent.
// The connection must have opened and must not be closing or closed.
func (c *Conn) ResumeReads() error {
	if err := c.checkReadControl(); err != nil {
		return err
	}
	pauses := c.loadReadPauses()
	if pauses == nil {
		return nil
	}
	pauses.mu.Lock()
	defer pauses.mu.Unlock()
	if err := c.checkReadControl(); err != nil {
		return err
	}
	if c.readPaused.Load()&readPauseExternal == 0 {
		return nil
	}
	c.readPaused.And(^readPauseExternal) // cleared first: the turn the release schedules must see it
	return readControlError(pauses.external.Resume())
}

func readControlError(err error) error {
	if errors.Is(err, net.ErrClosed) {
		return ErrClosed
	}
	return err
}

// OutboundBuffered returns accepted bytes still waiting in this connection's
// WebSocket write batch and the transport below it. It includes frame headers
// and, over TLS or other layers, what those layers hold and add. Bytes already
// written to the socket's kernel buffer are not included.
//
// This is a concurrent observation, not a write reservation: a handoff can
// briefly count bytes in both the batch and transport. MaxOutboundBuffered
// remains the hard admission limit. The method takes no write lock and is
// safe from any goroutine, including TrafficObserver.OnOutbound. An unattached
// connection reports zero.
func (c *Conn) OutboundBuffered() int {
	if c == nil || c.raw == nil {
		return 0
	}
	// Read the upper buffer first. During handoff its bytes enter the lower
	// queue before this snapshot is cleared, avoiding a gap between them.
	batch := c.batchBuffered.Load()
	if batch < 0 {
		batch = ^batch // a handoff retains the upper size until accepted
	}
	return int(batch) + c.raw.OutboundBuffered()
}

// observeOutbound defers user reports made inside a batch handoff. They must
// see the queue after handoff: otherwise its final drain could still include
// the upper batch and a watermark-based ResumeReads would miss the drain.
// Internal byte retirement is never deferred. Reports can race the writer,
// so publish deferred bytes before checking whether handoff already ended.
func (c *Conn) observeOutbound(n int) {
	if c.config == nil || !c.config.observeTraffic {
		return
	}
	metadata := c.ensureMetadata()
	if c.batchBuffered.Load() < 0 {
		metadata.outboundDeferred.Add(int64(n))
		if c.batchBuffered.Load() >= 0 {
			c.flushOutboundObservation()
		}
		return
	}
	if observer, ok := c.handler.(TrafficObserver); ok {
		metadata.outboundMu.Lock()
		defer metadata.outboundMu.Unlock()
		observer.OnOutbound(c, n)
	}
}

func (c *Conn) flushOutboundObservation() {
	metadata := c.metadata.Load()
	if metadata == nil || c.batchBuffered.Load() < 0 {
		return
	}
	if n := metadata.outboundDeferred.Swap(0); n != 0 {
		if observer, ok := c.handler.(TrafficObserver); ok {
			metadata.outboundMu.Lock()
			defer metadata.outboundMu.Unlock()
			observer.OnOutbound(c, int(n))
		}
	}
}

func (c *Conn) checkReadControl() error {
	if c == nil || c.raw == nil {
		return ErrNotReady
	}
	if c.closed.Load() || c.closing.Load() {
		return ErrClosed
	}
	if !c.opened.Load() {
		return ErrNotReady
	}
	return nil
}

func (c *Conn) externalReadsPaused() bool { return c.readPaused.Load()&readPauseExternal != 0 }
func (c *Conn) internalReadsPaused() bool { return c.readPaused.Load()&readPauseInternal != 0 }

// setInternalReadPause changes only the automatic pause and reports whether
// it changed. In particular, a drain must never clear an application's pause.
func (c *Conn) setInternalReadPause(paused bool) bool {
	pauses := c.loadReadPauses()
	if pauses == nil {
		if !paused || c.closing.Load() {
			return false
		}
		pauses = c.ensureReadPauses()
	}
	pauses.mu.Lock()
	defer pauses.mu.Unlock()
	if c.closing.Load() {
		paused = false
	}
	if c.internalReadsPaused() == paused {
		return false
	}
	if paused {
		if pauses.internal.Pause() != nil {
			return false
		}
		c.readPaused.Or(readPauseInternal)
		return true
	}
	c.readPaused.And(^readPauseInternal)
	_ = pauses.internal.Resume()
	return true
}

// releaseReadPauses lets the Close handshake consume again. It releases this
// connection's own pauses only, never one another owner of the transport
// holds, and counts the automatic one it ends as resumed.
func (c *Conn) releaseReadPauses() {
	pauses := c.loadReadPauses()
	if pauses == nil {
		return
	}
	pauses.mu.Lock()
	released := c.readPaused.Swap(0)
	_ = pauses.external.Resume()
	_ = pauses.internal.Resume()
	pauses.mu.Unlock()
	if released&readPauseInternal != 0 && c.config != nil && c.config.stats != nil {
		c.config.stats.resumed.Add(1)
	}
}
