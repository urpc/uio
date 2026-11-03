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
)

const (
	readPauseInternal uint32 = 1 << iota
	readPauseExternal
)

// PauseReads stops consuming incoming WebSocket frames until ResumeReads.
// It is safe from any goroutine and from OnOpen or OnMessage. A callback
// already in progress may finish; a pause made by OnMessage takes effect
// before the next frame, including frames buffered in the same socket read.
// Bytes remain buffered, and the transport's inbound bound eventually stops
// socket reads. Writes continue normally. This also works over TLS.
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
	c.readPauses.Or(readPauseExternal)
	return nil
}

// ResumeReads clears the pause requested by PauseReads and schedules a read
// turn, so already buffered frames need no new network input to be delivered.
// It leaves automatic watermark backpressure in effect. It is safe from any
// goroutine, including TrafficObserver.OnOutbound, and is idempotent.
// The connection must have opened and must not be closing or closed.
func (c *Conn) ResumeReads() error {
	if err := c.checkReadControl(); err != nil {
		return err
	}
	if c.readPauses.And(^readPauseExternal)&readPauseExternal == 0 {
		return nil
	}
	if err := c.raw.Wake(); err != nil {
		if errors.Is(err, net.ErrClosed) {
			return ErrClosed
		}
		return err
	}
	return nil
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

func (c *Conn) externalReadsPaused() bool {
	return c.readPauses.Load()&readPauseExternal != 0 && !c.closing.Load()
}

func (c *Conn) internalReadsPaused() bool {
	return c.readPauses.Load()&readPauseInternal != 0
}

// setInternalReadPause changes only the automatic pause and reports whether
// it changed. In particular, a drain must never clear an application's pause.
func (c *Conn) setInternalReadPause(paused bool) bool {
	if paused {
		return c.readPauses.Or(readPauseInternal)&readPauseInternal == 0
	}
	return c.readPauses.And(^readPauseInternal)&readPauseInternal != 0
}
