//go:build (linux || darwin || netbsd || freebsd || openbsd || dragonfly) && !stdio

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

package uio

import (
	"bytes"
	"testing"

	"github.com/urpc/uio/internal/bytebuf"
)

func ownedString(s string) *bytebuf.Buffer {
	b := bytebuf.AcquireBuffer(len(s))
	_, _ = b.WriteString(s)
	return b
}

func queueRangeForTest(c *fdConn, remain int64) *fileSegment {
	zc := c.zeroCopyState()
	zc.files.pushBack(fileSegment{fd: -1, remain: remain})
	c.writeState.Or(writeFileTailFlag)
	return zc.files.back()
}

// With no file range queued, an append lands in the byte run.
func TestOutQueueAppendWithoutRanges(t *testing.T) {
	var c fdConn
	c.outAppendOwned(ownedString("abcd"))
	if c.out.bytes.Len() != 4 || !bytes.Equal(c.out.bytes.Peek(make([]byte, 4)), []byte("abcd")) {
		t.Fatalf("byte run = %d bytes, want abcd", c.out.bytes.Len())
	}
}

// While a file range is queued, an append goes behind it — into its trailing
// bytes — never into the byte run ahead of it. This is the routing that keeps
// the wire order the accept order; a producer cannot bypass it.
func TestOutQueueAppendRoutesBehindQueuedRanges(t *testing.T) {
	var c fdConn
	seg := queueRangeForTest(&c, 100)
	c.outAppendOwned(ownedString("after"))
	if !c.out.bytes.Empty() {
		t.Fatalf("an append while a range is queued landed in the byte run (%d bytes), ahead of the range", c.out.bytes.Len())
	}
	if seg.trailing.Len() != 5 || !bytes.Equal(seg.trailing.Peek(make([]byte, 5)), []byte("after")) {
		t.Fatalf("trailing = %d bytes, want after", seg.trailing.Len())
	}
	// A second range takes the tail: the next append goes behind it, not the first.
	seg2 := queueRangeForTest(&c, 50)
	c.outAppendOwned(ownedString("later"))
	if seg.trailing.Len() != 5 || seg2.trailing.Len() != 5 {
		t.Fatalf("appends did not follow the last range: first trailing %d, last trailing %d", seg.trailing.Len(), seg2.trailing.Len())
	}
}

// The coalescing append routes the same way: behind the last queued range.
func TestOutQueueCoalescedAppendRoutesBehindRanges(t *testing.T) {
	var c fdConn
	seg := queueRangeForTest(&c, 100)
	c.outAppendOwnedTailCoalesced(ownedString("xyz"), 64<<10)
	if !c.out.bytes.Empty() || seg.trailing.Len() != 3 {
		t.Fatalf("coalesced append bypassed the range: byte run %d, trailing %d", c.out.bytes.Len(), seg.trailing.Len())
	}
}

// A direct send's rest goes to the head of the byte run, ahead of what others
// queued while it sent.
func TestOutQueuePrependRestGoesToHead(t *testing.T) {
	var c fdConn
	_, _ = c.out.bytes.WriteString("queued-during-send")
	c.outPrependRest(ownedString("rest-"))
	got := c.out.bytes.Peek(make([]byte, c.out.bytes.Len()))
	if !bytes.Equal(got, []byte("rest-queued-during-send")) {
		t.Fatalf("byte run = %q, want the rest at the head", got)
	}
}

// Write and reserve reach the same routed tail as append.
func TestOutQueueWriteAndReserveRouteBehindRanges(t *testing.T) {
	var c fdConn
	seg := queueRangeForTest(&c, 100)
	c.outWriteCoalesced([]byte("wr"), 64<<10)
	dst := c.outReserve(2, 64<<10)
	copy(dst, "rv")
	if !c.out.bytes.Empty() {
		t.Fatalf("write/reserve bypassed the range: byte run %d bytes", c.out.bytes.Len())
	}
	if got := seg.trailing.Peek(make([]byte, seg.trailing.Len())); !bytes.Equal(got, []byte("wrrv")) {
		t.Fatalf("trailing = %q, want wrrv", got)
	}
}

// Writev routes behind the last queued range too, not into the byte run.
func TestOutQueueWritevRoutesBehindRanges(t *testing.T) {
	var c fdConn
	seg := queueRangeForTest(&c, 100)
	c.outWritevCoalesced([][]byte{[]byte("wv"), []byte("ec")}, 64<<10)
	if !c.out.bytes.Empty() {
		t.Fatalf("writev bypassed the range: byte run %d bytes", c.out.bytes.Len())
	}
	if got := seg.trailing.Peek(make([]byte, seg.trailing.Len())); !bytes.Equal(got, []byte("wvec")) {
		t.Fatalf("trailing = %q, want wvec", got)
	}
}

// The coalescing append joins the tail block's spare room instead of adding a
// block. Tail coalescing needs at least two blocks (it never merges into the
// first), so prefill two, the second with room to spare.
func TestOutQueueTailCoalescedAppendJoinsTailBlock(t *testing.T) {
	var c fdConn
	first := bytebuf.AcquireBuffer(64)
	_, _ = first.WriteString("first")
	c.outAppendOwned(first)
	second := bytebuf.AcquireBuffer(64 << 10)
	_, _ = second.WriteString("second")
	c.outAppendOwned(second)
	if c.out.bytes.Blocks() != 2 {
		t.Fatalf("prefill blocks = %d, want 2", c.out.bytes.Blocks())
	}
	c.outAppendOwnedTailCoalesced(ownedString("xy"), 64<<10)
	if c.out.bytes.Blocks() != 2 {
		t.Fatalf("a small append added a block (%d) instead of joining the tail's spare room", c.out.bytes.Blocks())
	}
	if got := c.out.bytes.Peek(make([]byte, c.out.bytes.Len())); !bytes.Equal(got, []byte("firstsecondxy")) {
		t.Fatalf("byte run = %q, want firstsecondxy", got)
	}
}

// A direct send's rest goes to the head of the byte run even while a file range
// is queued: it never lands in the range's trailing bytes.
func TestOutQueuePrependRestIgnoresQueuedRanges(t *testing.T) {
	var c fdConn
	seg := queueRangeForTest(&c, 100)
	_, _ = c.out.bytes.WriteString("AAAA") // queued by others while the send ran
	c.outPrependRest(ownedString("rrrrr"))
	if got := c.out.bytes.Peek(make([]byte, c.out.bytes.Len())); !bytes.Equal(got, []byte("rrrrrAAAA")) {
		t.Fatalf("byte run = %q, want the rest at its head", got)
	}
	if !seg.trailing.Empty() {
		t.Fatalf("the rest landed in the range's trailing bytes (%d)", seg.trailing.Len())
	}
}
