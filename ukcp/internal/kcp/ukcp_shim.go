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

import (
	"container/heap"
	"time"
)

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

// InputBatch feeds one or more concatenated KCP segments without running an
// internal flush mid-batch, returning whether window/fastack progress calls
// for a full flush at the end of the turn.
func (kcp *KCP) InputBatch(data []byte) (needFullFlush bool, ret int) {
	flushSegments, ret := kcp.inputCore(data, IKCP_PACKET_REGULAR)
	return flushSegments != 0, ret
}

// HasPendingAckOrProbe reports whether ACKs or window probes are waiting to leave.
func (kcp *KCP) HasPendingAckOrProbe() bool {
	return len(kcp.acklist) > 0 || kcp.probe != 0
}

// PendingAckCount reports the number of queued ACK entries.
func (kcp *KCP) PendingAckCount() int { return len(kcp.acklist) }

// PopStreamSegment pops the next in-order stream segment without an
// intermediate scratch copy; the caller returns non-nil data via ReleasePayload.
func (kcp *KCP) PopStreamSegment() (data []byte, ok bool) {
	if kcp.rcv_queue.IsEmpty() {
		return nil, false
	}
	fastRecover := kcp.rcv_queue.Len() >= int(kcp.rcv_wnd)
	seg, _ := kcp.rcv_queue.Pop()
	for kcp.rcv_buf.Len() > 0 && kcp.rcv_queue.Len() < int(kcp.rcv_wnd) && kcp.rcv_buf.segments[0].sn == kcp.rcv_nxt {
		next := heap.Pop(kcp.rcv_buf).(segment)
		kcp.rcv_queue.Push(next)
		kcp.rcv_nxt++
	}
	if kcp.rcv_queue.Len() < int(kcp.rcv_wnd) && fastRecover {
		kcp.probe |= IKCP_ASK_TELL
	}
	return seg.data, true
}

// ReleasePayload returns a payload slice obtained from PopStreamSegment to the pool.
func ReleasePayload(buf []byte) {
	if buf != nil {
		defaultBufferPool.Put(buf)
	}
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
	return kcp.queuedBytes
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
