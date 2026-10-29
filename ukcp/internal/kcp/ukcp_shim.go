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

// This file is ukcp's own; the vendored files beside it are xtaci/kcp-go's
// (MIT) and carry their headers. See README.md for the two patches made to
// kcp.go and for the origin of every file.

package kcp

import "time"

// mtuLimit is the size of the buffers defaultBufferPool hands out. Upstream
// keeps the constant in sess.go, which ukcp does not vendor.
const mtuLimit = 1500

// clockNow, when set, replaces the wall clock currentMs reads: the engine's
// timers — RTO, flush interval, window probing — run on it. Tests set it to
// drive a session deterministically; production leaves it nil.
var clockNow func() uint32

// SetStreamMode switches message mode (false) to stream mode (true, the mode
// ukcp runs: the transport above sees one byte stream). Upstream keeps this
// setter on UDPSession.
func (kcp *KCP) SetStreamMode(enable bool) {
	if enable {
		kcp.stream = 1
	} else {
		kcp.stream = 0
	}
}

// Flush runs one full flush now — ACKs, window probes, new segments from the
// send queue and the retransmissions due — and reports how long until the
// next flush is due at the latest (the flush interval, or the nearest
// retransmission deadline). It is upstream's update step without the interval
// bookkeeping: ukcp schedules it with its own timer and flushes whenever it
// has something to send.
func (kcp *KCP) Flush() time.Duration {
	return time.Duration(kcp.flush(IKCP_FLUSH_FULL)) * time.Millisecond
}

// AskWindow schedules one window probe: the engine's own WASK segment, with
// this end's window and unacknowledged position filled in, so a keepalive
// never tells the peer anything false about flow control. The next flush
// sends it.
func (kcp *KCP) AskWindow() {
	kcp.probe |= IKCP_ASK_SEND
}

// FlushAckOnly answers what arrived — the ACKs owed and the window probes a
// drained receive queue owes — without walking the send window for
// retransmissions, which a turn that only took a datagram does not need. It
// is the flush a read turn runs.
func (kcp *KCP) FlushAckOnly() {
	kcp.flush(IKCP_FLUSH_ACKONLY)
}

// IsDead reports the dead link the engine declared: a segment retransmitted
// dead_link times.
func (kcp *KCP) IsDead() bool { return kcp.state == 0xFFFFFFFF }

// SetDeadLink sets how many retransmissions of one segment mark the link
// dead. A non-positive n leaves the current bound.
func (kcp *KCP) SetDeadLink(n int) {
	if n > 0 {
		kcp.dead_link = uint32(n)
	}
}

// QueuedBytes is the payload the send queue holds: what the engine took from
// the writer and has not moved into the send window. A segment a small write
// built carries that write's bytes, not a full MTU, so counting bytes is
// exact where segments times the MTU would charge a one-byte write a
// kilobyte.
func (kcp *KCP) QueuedBytes() int {
	n := 0
	kcp.snd_queue.ForEach(func(seg *segment) bool {
		n += len(seg.data)
		return true
	})
	return n
}

// Queued is how many segments wait in the send queue, accepted from above and
// not yet inside the send window.
func (kcp *KCP) Queued() int { return kcp.snd_queue.Len() }

// Inflight is how many segments are in the send window: sent, not yet
// acknowledged.
func (kcp *KCP) Inflight() int { return kcp.snd_buf.Len() }

// Conv reports the conversation id the engine was built with.
func (kcp *KCP) Conv() uint32 { return kcp.conv }

// RTT reports the smoothed round-trip time and the retransmission timeout the
// engine currently runs with.
func (kcp *KCP) RTT() (srtt, rto time.Duration) {
	return time.Duration(kcp.rx_srtt) * time.Millisecond, time.Duration(kcp.rx_rto) * time.Millisecond
}

// CWnd reports the congestion window, in segments.
func (kcp *KCP) CWnd() uint32 { return kcp.cwnd }

// RemoteWnd reports the window the peer last advertised, in segments.
func (kcp *KCP) RemoteWnd() uint32 { return kcp.rmt_wnd }
