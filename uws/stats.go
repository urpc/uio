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
	"sync/atomic"
	"unsafe"

	"github.com/urpc/uio"
)

// Stats is a point-in-time, zero-allocation runtime snapshot of a [Server] or
// [Dialer]. Counters live on cache-line-isolated stripes updated only at
// connection handshake/close boundaries and cold backpressure rejections, so
// message reads and writes pay zero overhead.
type Stats struct {
	// Transport is the underlying uio.Events runtime snapshot.
	Transport uio.Stats
	// ActiveConnections is the number of open WebSocket connections whose
	// upgrade handshake has completed.
	ActiveConnections int64
	// HandshakingConnections is the number of connections currently in the
	// HTTP upgrade handshake.
	HandshakingConnections int64
	// TotalUpgrades counts connections that have completed the WebSocket
	// upgrade handshake.
	TotalUpgrades uint64
	// HandshakeFailures counts upgrade handshakes that failed validation,
	// origin/authorization checks, or timed out.
	HandshakeFailures uint64
	// BackpressureDrops counts non-blocking sends rejected with ErrBackpressure.
	BackpressureDrops uint64
}

const wsStatsStripes = 16

type wsStatsStripe struct {
	active            atomic.Int64
	handshaking       atomic.Int64
	totalUpgrades     atomic.Uint64
	handshakeFailures atomic.Uint64
	backpressureDrops atomic.Uint64
	_                 [24]byte // pad stripe to 64-byte cache line
}

type wsStatsCollector struct {
	stripes [wsStatsStripes]wsStatsStripe
	rr      atomic.Uint32
}

func (sc *wsStatsCollector) stripeFor(c *Conn) *wsStatsStripe {
	idx := (uintptr(unsafe.Pointer(c)) >> 6) & (wsStatsStripes - 1)
	return &sc.stripes[idx]
}

func (sc *wsStatsCollector) noteHandshaking(c *Conn) {
	sc.stripeFor(c).handshaking.Add(1)
}

func (sc *wsStatsCollector) noteUpgraded(c *Conn, wasHandshaking bool) {
	s := sc.stripeFor(c)
	if wasHandshaking {
		s.handshaking.Add(-1)
	}
	s.active.Add(1)
	s.totalUpgrades.Add(1)
}

func (sc *wsStatsCollector) noteHandshakeFailed(c *Conn, wasHandshaking bool) {
	s := sc.stripeFor(c)
	if wasHandshaking {
		s.handshaking.Add(-1)
	}
	s.handshakeFailures.Add(1)
}

func (sc *wsStatsCollector) noteRejectedUpgrade() {
	idx := sc.rr.Add(1) & (wsStatsStripes - 1)
	sc.stripes[idx].handshakeFailures.Add(1)
}

func (sc *wsStatsCollector) noteClosed(c *Conn) {
	sc.stripeFor(c).active.Add(-1)
}

func (sc *wsStatsCollector) noteBackpressure(c *Conn) {
	sc.stripeFor(c).backpressureDrops.Add(1)
}

func (sc *wsStatsCollector) snapshot(tr uio.Stats) Stats {
	st := Stats{Transport: tr}
	if sc == nil {
		return st
	}
	for i := range sc.stripes {
		s := &sc.stripes[i]
		st.ActiveConnections += s.active.Load()
		st.HandshakingConnections += s.handshaking.Load()
		st.TotalUpgrades += s.totalUpgrades.Load()
		st.HandshakeFailures += s.handshakeFailures.Load()
		st.BackpressureDrops += s.backpressureDrops.Load()
	}
	st.ActiveConnections = max(st.ActiveConnections, 0)
	st.HandshakingConnections = max(st.HandshakingConnections, 0)
	return st
}
