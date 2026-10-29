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
	"bytes"
	"testing"
	"time"
)

// fakeClock drives the engine's timers; set through clockNow.
type fakeClock struct{ ms uint32 }

func (c *fakeClock) advance(d time.Duration) { c.ms += uint32(d / time.Millisecond) }

func newTestPair(t *testing.T) (*KCP, *KCP, *fakeClock, *[][]byte, *[][]byte) {
	t.Helper()
	clock := &fakeClock{ms: 1000}
	clockNow = func() uint32 { return clock.ms }
	t.Cleanup(func() { clockNow = nil })

	var a2b, b2a [][]byte
	a := NewKCP(7, func(buf []byte, size int) { a2b = append(a2b, bytes.Clone(buf[:size])) })
	b := NewKCP(7, func(buf []byte, size int) { b2a = append(b2a, bytes.Clone(buf[:size])) })
	for _, e := range []*KCP{a, b} {
		e.SetStreamMode(true)
		e.NoDelay(0, 30, 2, 1) // the Fast profile
		e.WndSize(64, 64)
	}
	return a, b, clock, &a2b, &b2a
}

func deliver(t *testing.T, pkts *[][]byte, dst *KCP) {
	t.Helper()
	for _, p := range *pkts {
		if ret := dst.Input(p, IKCP_PACKET_REGULAR, true); ret != 0 {
			t.Fatalf("input: %d", ret)
		}
	}
	*pkts = (*pkts)[:0]
}

func drain(t *testing.T, e *KCP) (data []byte, fin bool) {
	t.Helper()
	buf := make([]byte, 64<<10)
	for {
		n := e.Recv(buf)
		switch {
		case n < 0:
			return data, fin
		case n == 0: // an empty push: the end of the stream
			fin = true
		default:
			data = append(data, buf[:n]...)
		}
	}
}

func TestVendoredEngineDeliversAndEnds(t *testing.T) {
	a, b, _, a2b, b2a := newTestPair(t)

	if ret := a.Send([]byte("hello")); ret != 0 {
		t.Fatalf("send: %d", ret)
	}
	a.Flush()
	deliver(t, a2b, b)
	b.Flush() // ACKs went out inside Input; this flushes what is left
	deliver(t, b2a, a)

	if data, fin := drain(t, b); string(data) != "hello" || fin {
		t.Fatalf("b read %q fin=%v, want hello", data, fin)
	}
	if got := a.WaitSnd(); got != 0 {
		t.Fatalf("a still waits on %d segments after the ACK", got)
	}

	// The empty push is the end of the stream: ordered behind the data.
	if ret := a.Send(nil); ret != 0 {
		t.Fatalf("send FIN: %d", ret)
	}
	a.Flush()
	deliver(t, a2b, b)
	b.Flush()
	deliver(t, b2a, a)

	if data, fin := drain(t, b); len(data) != 0 || !fin {
		t.Fatalf("FIN read %q fin=%v, want an empty end", data, fin)
	}
	if a.IsDead() || b.IsDead() {
		t.Fatal("a healthy session was declared dead")
	}
}

func TestVendoredEngineRetransmitsLostSegments(t *testing.T) {
	a, b, clock, a2b, b2a := newTestPair(t)

	if ret := a.Send([]byte("lost-first")); ret != 0 {
		t.Fatalf("send: %d", ret)
	}
	a.Flush()
	// The network drops the first flight.
	*a2b = (*a2b)[:0]

	clock.advance(500 * time.Millisecond) // past the RTO
	a.Flush()
	if len(*a2b) == 0 {
		t.Fatal("nothing was retransmitted after the RTO")
	}
	deliver(t, a2b, b)
	b.Flush()
	deliver(t, b2a, a)

	if data, _ := drain(t, b); string(data) != "lost-first" {
		t.Fatalf("b read %q, want the message after retransmission", data)
	}
	if srtt, rto := a.RTT(); rto <= 0 || srtt < 0 {
		t.Fatalf("RTT reports srtt=%v rto=%v", srtt, rto)
	}
}

func TestVendoredEngineDeclaresDeadLink(t *testing.T) {
	a, _, clock, a2b, _ := newTestPair(t)
	a.SetDeadLink(2)

	if ret := a.Send([]byte("nobody-listens")); ret != 0 {
		t.Fatalf("send: %d", ret)
	}
	a.Flush()
	*a2b = (*a2b)[:0]

	for i := 0; i < 2 && !a.IsDead(); i++ {
		clock.advance(500 * time.Millisecond)
		a.Flush()
		*a2b = (*a2b)[:0]
	}
	if !a.IsDead() {
		t.Fatal("the link was never declared dead")
	}
}
