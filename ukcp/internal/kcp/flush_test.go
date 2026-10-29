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

package kcp

import (
	"encoding/binary"
	"testing"
)

// testSegment builds one KCP segment for the tests that speak the wire
// dialect by hand.
func testSegment(conv uint32, cmd, frg byte, wnd uint16, sn, una uint32, payload []byte) []byte {
	seg := make([]byte, IKCP_OVERHEAD+len(payload))
	binary.LittleEndian.PutUint32(seg[0:], conv)
	seg[4] = cmd
	seg[5] = frg
	binary.LittleEndian.PutUint16(seg[6:], wnd)
	binary.LittleEndian.PutUint32(seg[8:], currentMs())
	binary.LittleEndian.PutUint32(seg[12:], sn)
	binary.LittleEndian.PutUint32(seg[16:], una)
	binary.LittleEndian.PutUint32(seg[20:], uint32(len(payload)))
	copy(seg[IKCP_OVERHEAD:], payload)
	return seg
}

// An ACK-only flush answers what arrived without moving queued data into the
// send window unsent: what a full flush then moves, it sends.
func TestAckOnlyFlushDoesNotMoveData(t *testing.T) {
	var pushes int
	engine := NewKCP(7, func(buf []byte, size int) {
		if size >= IKCP_OVERHEAD && buf[4] == IKCP_CMD_PUSH {
			pushes++
		}
	})
	engine.SetStreamMode(true)
	engine.WndSize(64, 64)

	// The peer speaks with a zero window: data queues behind it.
	engine.Input(testSegment(7, IKCP_CMD_PUSH, 0, 0, 0, 0, []byte("x")), IKCP_PACKET_REGULAR, true)
	if ret := engine.Send([]byte("hello")); ret != 0 {
		t.Fatalf("send: %d", ret)
	}
	if engine.Queued() != 1 {
		t.Fatalf("queued = %d, want 1", engine.Queued())
	}
	engine.FlushAckOnly()
	if engine.Queued() != 1 || engine.Inflight() != 0 {
		t.Fatalf("ACK-only flush moved queued data: queued=%d inflight=%d", engine.Queued(), engine.Inflight())
	}

	// The peer reopens its window: a full flush moves and sends the data.
	engine.Input(testSegment(7, IKCP_CMD_PUSH, 0, 64, 1, 0, []byte("y")), IKCP_PACKET_REGULAR, true)
	pushes = 0
	engine.Flush()
	if engine.Queued() != 0 || engine.Inflight() != 1 {
		t.Fatalf("full flush did not move the data: queued=%d inflight=%d", engine.Queued(), engine.Inflight())
	}
	if pushes == 0 {
		t.Fatal("the moved data was never sent")
	}
}
