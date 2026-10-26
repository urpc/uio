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
	"time"
)

// The heartbeat runs on the connection itself: one wake timer on the
// transport (uio.Conn.SetWakeDeadline) aimed at whichever moment comes next
// — the ping interval or the deadline the outstanding Ping runs on — and a
// tick on every turn, data turns included, that pings, expires, and re-aims
// the timer. No scanner walks the server's connections, and a stuck
// connection delays no other.

// heartbeatBoundary is the moment a callback of this connection may block
// until: the next heartbeat decision — the next ping, or the deadline the
// outstanding one runs on. Zero when no heartbeat runs or has not begun; the
// pull client's queue wait is bounded by it, so a client nobody reads still
// reaches the tick that expires it.
func (c *Conn) heartbeatBoundary() time.Time {
	heartbeat := c.heartbeat
	if heartbeat == nil {
		return time.Time{}
	}
	heartbeat.mu.Lock()
	defer heartbeat.mu.Unlock()
	if heartbeat.nextPingAt == 0 {
		return time.Time{}
	}
	next := time.Unix(0, heartbeat.nextPingAt)
	// The same minimum heartbeatTick arms at: an outstanding or stalled Ping
	// ends before the next interval does.
	if deadline := heartbeat.expiryLocked(c.config.heartbeatTimeout); heartbeat.pingOutstanding.Load() {
		return deadline
	} else if !deadline.IsZero() && deadline.Before(next) {
		return deadline
	}
	return next
}

// beginHeartbeat arms the first ping interval of an opened connection.
func (c *Conn) beginHeartbeat() {
	next := time.Now().Add(c.config.heartbeatInterval)
	c.heartbeat.mu.Lock()
	c.heartbeat.nextPingAt = next.UnixNano()
	c.heartbeat.mu.Unlock()
	c.armHeartbeatWake(next)
}

// armHeartbeatWake aims the transport's wake timer at next and records it
// only once the transport took it. A refused arm leaves armedAt clear, so the
// steady fast path never mistakes a timer that does not exist for one that
// does, and the next turn tries again.
func (c *Conn) armHeartbeatWake(next time.Time) {
	armed := int64(0)
	if c.raw.SetWakeDeadline(next) == nil {
		armed = next.UnixNano()
	}
	c.heartbeat.mu.Lock()
	c.heartbeat.armedAt = armed
	c.heartbeat.mu.Unlock()
}

// heartbeatTick runs on every turn, at the turn's moment: the timed turns
// the wake timer guarantees and the data turns that arrive on their own. It
// expires the states the timeout ends, sends a Ping at the interval while
// none is outstanding, and re-aims the wake timer at whichever moment comes
// next.
func (c *Conn) heartbeatTick(now time.Time) {
	heartbeat := c.heartbeat
	if heartbeat == nil || c.closed.Load() || c.closing.Load() {
		return
	}
	// The steady case of a busy connection — no Ping in flight or stalled,
	// the next one not yet due, the timer already aimed at it — decides
	// nothing, and pays one lock for knowing so.
	heartbeat.mu.Lock()
	if heartbeat.nextPingAt != 0 && now.UnixNano() < heartbeat.nextPingAt &&
		heartbeat.armedAt == heartbeat.nextPingAt && heartbeat.sendStalledAt == 0 &&
		!heartbeat.pingOutstanding.Load() {
		heartbeat.mu.Unlock()
		return
	}
	heartbeat.mu.Unlock()
	if heartbeat.expirePing(now, c.config.heartbeatTimeout) {
		c.expireHeartbeat()
		return
	}
	heartbeat.mu.Lock()
	nextPingAt, armedAt := heartbeat.nextPingAt, heartbeat.armedAt
	heartbeat.mu.Unlock()
	if nextPingAt == 0 {
		return // beginHeartbeat has not run: no cadence to keep
	}
	nextPing := time.Unix(0, nextPingAt)
	if !heartbeat.pingOutstanding.Load() && !now.Before(nextPing) {
		switch err := c.sendHeartbeatPing(now); {
		case err == nil:
		case errors.Is(err, ErrBackpressure):
			// The Ping is not outstanding, so what ends this attempt is the
			// stall deadline noted here — and the interval moves on.
			heartbeat.noteSendStall(now)
		case !c.closed.Load() && !c.closing.Load():
			c.setCloseError(err)
			c.closing.Store(true)
			c.abortTransport(err)
			return
		}
		heartbeat.mu.Lock()
		heartbeat.nextPingAt = now.Add(c.config.heartbeatInterval).UnixNano()
		nextPing = time.Unix(0, heartbeat.nextPingAt) // the later interval, not the fired one
		heartbeat.mu.Unlock()
	}
	// While a Ping is outstanding the next wake is the moment it ends at;
	// otherwise the stall deadline, when one is pending, comes before the
	// next ping.
	next := nextPing
	if deadline := heartbeat.expiry(now, c.config.heartbeatTimeout); heartbeat.pingOutstanding.Load() {
		next = deadline
	} else if !deadline.IsZero() && deadline.Before(next) {
		next = deadline
	}
	// A wake still needed but not in the future must be armed too: the
	// one-shot that brought this turn has already fired, so a deadline equal
	// to it would never come again. The transport retries a past deadline at
	// its own granularity.
	if next.UnixNano() != armedAt || !next.After(now) {
		c.armHeartbeatWake(next)
	}
}
