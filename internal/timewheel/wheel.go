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

// Package timewheel is a hashed timing wheel of deadlines, one instance per
// event loop, holding the wake timers of that loop's connections.
//
// A deadline is one Entry in one of buckets ring slots — O(1) to arm and to
// cancel, no map. The ring's slots each carry one tick of time, and every
// entry in a slot shares that slot's tick: a slot's entries are those of its
// tick within the window (cursor, cursor+buckets] the wheel currently
// covers, and the window holds exactly one tick per residue, so scanning
// slots cyclically from the cursor walks deadlines in time order. Deadlines
// past the window wait on an overflow min-heap, keyed by their time —
// O(log n) to arm, cancel and promote — and re-enter the ring as their time
// comes within it.
//
// The wheel reports due entries only to Advance, which its owner runs when
// the kernel timer it armed fires. Arm, Cancel and Advance each recompute
// the earliest deadline — a summary bitmap carries the occupied slots, so
// the first occupied one is found a word at a time — and tell the arm
// callback under the wheel's lock. That is the only kernel
// interaction, and a non-blocking one, so the lock never waits.
//
// The wheel carries no clock of its own: Arm and Advance take the current
// time, wall-clock as UIO's deadlines are, and a due entry is reported with
// its armed deadline untouched.
package timewheel

import (
	"math"
	"math/bits"
	"sync"
	"time"
)

// Entry is one deadline. Its owner allocates it and keeps it for the
// connection's life; Arm and Cancel move it, so it must not be copied while
// armed. When it is due, Advance reports it and, if Period is positive,
// re-arms it for When+Period — a periodic entry stays in the wheel until
// Cancelled.
type Entry[T any] struct {
	next, prev *Entry[T]
	// bucket is the ring slot the entry occupies plus one — zero, the
	// field's value on a fresh entry, means no wheel holds it — or
	// inOverflow. Guarded by the wheel's mutex.
	bucket int32
	// When and Period are the armed deadline and, for a periodic entry, its
	// interval. Guarded by the wheel's mutex.
	When   time.Time
	Period time.Duration
	// heap is the entry's index in the wheel's overflow heap plus one —
	// zero, like bucket, means the entry is in no heap. Guarded by the
	// wheel's mutex.
	heap int32
	// Val is the value the entry carries, set once by its owner.
	Val T
}

// maxBuckets bounds the ring: slots index int32 fields on entries.
const maxBuckets = math.MaxInt32

const (
	// notInWheel is the zero value of Entry.bucket: a freshly allocated
	// entry sits in no wheel until its first Arm, and detachLocked must read
	// that as free rather than as ring slot zero. A ring entry stores its
	// slot number plus one; inOverflow marks the overflow heap.
	notInWheel int32 = 0
	inOverflow int32 = -1
)

// Wheel is a hashed timing wheel. Every method is safe from any goroutine;
// the mutex orders arms, cancels and advances against one another.
type Wheel[T any] struct {
	mu      sync.Mutex
	tick    time.Duration
	buckets int32
	ring    []*Entry[T]
	// summary has one bit per ring slot, set while the slot holds entries,
	// so the earliest deadline needs a cyclic word scan, not an entry walk.
	summary []uint64
	// cursor is the last tick Advance processed; slots behind it are spent.
	cursor int64
	// overflow is a min-heap of the deadlines past the window, keyed by
	// When, with each entry's heap field carrying its place in it (slot plus
	// one, zero for none). The heap's top is the earliest of them, so arming,
	// cancelling and promoting a long deadline cost logarithmic work instead
	// of a list scan.
	overflow []*Entry[T]
	count    int
	// earliest is what the arm callback last heard, so an arm or cancel that
	// cannot move it — a later deadline arriving, an entry that was not the
	// earliest leaving — skips the cyclic slot scan.
	earliest time.Time
	// arm hears the earliest deadline whenever it moves, under the mutex;
	// a zero time disarms. It must not block, must not re-enter the wheel
	// (it would deadlock), and must not panic (the mutex would stay held);
	// the time may be in the past — the owner clamps it to its retry
	// granularity rather than disarming.
	arm func(when time.Time)
}

// New returns a wheel of buckets slots, each covering tick of time, with
// its window set to now — the owner passes the current time, and arms and
// advances carry theirs, so the wheel never reads a clock. arm, when not
// nil, receives the earliest deadline under the wheel's lock.
func New[T any](tick time.Duration, buckets int, now time.Time, arm func(when time.Time)) *Wheel[T] {
	// New panics on a non-positive tick or bucket count, a bucket count
	// past maxBuckets, or a zero now: the arithmetic below would divide by
	// zero, truncate the ring, or stretch the cursor to pre-history.
	if tick <= 0 || buckets <= 0 || buckets > maxBuckets || now.IsZero() {
		panic("timewheel: New needs a positive tick, a bucket count of at most maxBuckets, and a non-zero now")
	}
	w := &Wheel[T]{
		tick:    tick,
		buckets: int32(buckets),
		ring:    make([]*Entry[T], buckets),
		summary: make([]uint64, (buckets-1)/64+1),
		arm:     arm,
	}
	w.cursor = w.tickOf(now)
	return w
}

// Arm sets e's deadline; a past deadline is due at the next Advance, and the
// zero time cancels. A positive period makes the entry re-arm itself each
// time it is due.
func (w *Wheel[T]) Arm(e *Entry[T], when time.Time, period time.Duration) {
	if when.IsZero() {
		// The zero time is the wheel's "nothing armed": an entry carrying it
		// would read as the disarmed earliest and be stranded.
		w.Cancel(e)
		return
	}
	w.mu.Lock()
	wasEarliest := e.When.Equal(w.earliest)
	w.detachLocked(e)
	e.When, e.Period = when, period
	w.attachLocked(e)
	if w.earliest.IsZero() || when.Before(w.earliest) || wasEarliest {
		w.rearmEarliestLocked()
	}
	w.mu.Unlock()
}

// Cancel removes e, disarming the wheel when it held the last entry.
func (w *Wheel[T]) Cancel(e *Entry[T]) {
	w.mu.Lock()
	wasEarliest := e.When.Equal(w.earliest) && e.bucket != notInWheel
	w.detachLocked(e)
	if wasEarliest {
		w.rearmEarliestLocked()
	}
	w.mu.Unlock()
}

// reportSlotLocked pops one slot and moves its due entries — those whose
// time has passed — onto due; the rest re-enter where their time puts them,
// one tick ahead of the cursor when their tick has not come. It requires the
// mutex.
func (w *Wheel[T]) reportSlotLocked(due *[]*Entry[T], index int64, now time.Time) {
	slot := w.ring[index]
	if slot == nil {
		return
	}
	w.ring[index] = nil
	w.clearSummaryLocked(int32(index))
	// The slot is empty from here on, so entries are cut from their siblings
	// directly: unlinkLocked would write the remainder of the list back into
	// ring[index], and a relinked future entry pushed onto that rebuilding
	// list would be orphaned by the next cut.
	for e := slot; e != nil; {
		next := e.next
		e.prev, e.next = nil, nil
		e.bucket = notInWheel
		if !e.When.After(now) {
			w.count--
			*due = append(*due, e)
		} else {
			// The entry's tick is still ahead: linkLocked parks it one tick
			// ahead of the cursor, drifting forward an Advance at a time
			// until its time comes.
			w.linkLocked(e)
		}
		e = next
	}
}

// Advance collects the deadlines that have passed, moving the wheel's window
// to now. A periodic entry is reported and re-armed for its next period in
// the same step — from now when the elapsed time spans a whole period, so a
// late advance does not fire a burst.
func (w *Wheel[T]) Advance(now time.Time) []*Entry[T] {
	return w.AdvanceInto(now, nil)
}

// AdvanceInto is Advance appending to due, so an owner that advances on every
// expiry reuses one slice instead of allocating per call. It returns the
// extended slice; entries already in due are left as they are. The entries
// come out in no particular order — a long gap sweeps slots, not time —
// so an owner that needs time order sorts them by When.
func (w *Wheel[T]) AdvanceInto(now time.Time, due []*Entry[T]) []*Entry[T] {
	start := len(due) // only what this Advance appends is its own
	w.mu.Lock()
	current := w.tickOf(now)
	// A gap longer than the window passes over every slot the ring has:
	// each entry's tick is behind now, all of them are due, and walking the
	// gap tick by tick would cost the whole idle period. Rewind to one
	// window short of now, sweep the occupied slots the summary names — the
	// empty ones cost a word each, not a tick each — and land on now.
	if current-w.cursor > int64(w.buckets) {
		w.cursor = current - int64(w.buckets)
		for word := range w.summary {
			occupied := w.summary[word]
			for occupied != 0 {
				bit := bits.TrailingZeros64(occupied)
				occupied &^= uint64(1) << bit
				w.reportSlotLocked(&due, int64(word)*64+int64(bit), now)
			}
		}
		w.cursor = current
	} else {
		for w.cursor < current {
			w.cursor++
			w.reportSlotLocked(&due, w.cursor%int64(w.buckets), now)
		}
	}
	// Deadlines inside the cursor's own tick never cross it: an entry armed
	// past its tick's boundary, or in the past, waits in the slot ahead of
	// the cursor, and due ones there fire on this very Advance. The tracked
	// earliest says whether any is due, so a wheel of only-future deadlines
	// never pays the slot's churn.
	if w.count > 0 && !w.earliest.IsZero() && !w.earliest.After(now) {
		w.reportSlotLocked(&due, w.nextSlotLocked(), now)
	}
	// Overflow entries come due or move within the window, in time order
	// from the heap's top.
	for len(w.overflow) > 0 {
		e := w.overflow[0]
		if e.When.After(now) && w.tickOf(e.When)-w.cursor > int64(w.buckets) {
			break
		}
		w.heapPopLocked()
		e.bucket = notInWheel
		if !e.When.After(now) {
			w.count--
			due = append(due, e)
		} else {
			w.linkLocked(e)
		}
	}
	// Periodic entries re-arm from when they were due, or from now when the
	// elapsed time spans their period.
	for _, e := range due[start:] {
		if e.Period > 0 {
			e.When = e.When.Add(e.Period)
			if !e.When.After(now) {
				e.When = now.Add(e.Period)
			}
			w.attachLocked(e)
		}
	}
	w.rearmEarliestLocked()
	w.mu.Unlock()
	return due
}

// Empty reports whether no deadline is armed.
func (w *Wheel[T]) Empty() bool {
	w.mu.Lock()
	empty := w.count == 0
	w.mu.Unlock()
	return empty
}

// Len reports how many entries the wheel holds.
func (w *Wheel[T]) Len() int {
	w.mu.Lock()
	n := w.count
	w.mu.Unlock()
	return n
}

// nextSlotLocked is the slot of the tick after the cursor; it requires the
// mutex. It reduces before it adds: a cursor clamped to the last tick a time
// can carry would overflow on cursor+1.
func (w *Wheel[T]) nextSlotLocked() int64 {
	n := int64(w.buckets)
	return (w.cursor%n + 1) % n
}

// tickOf is the tick a time falls in. UnixNano cannot carry times before 1970
// or past 2262, so each side clamps to the tick it belongs with: an earlier
// time to tick zero — already passed, due at the next Advance — and a later
// one to the last tick, which keeps it on the overflow heap, out of the ring,
// for as long as the wheel runs.
func (w *Wheel[T]) tickOf(when time.Time) int64 {
	switch {
	case when.Before(unixEpoch):
		return 0
	case when.After(lastNano):
		return math.MaxInt64 / int64(w.tick)
	}
	return when.UnixNano() / int64(w.tick)
}

// unixEpoch and lastNano bound the times UnixNano represents.
var (
	unixEpoch = time.Unix(0, 0)
	lastNano  = time.Unix(0, math.MaxInt64)
)

// attachLocked links e, newly held by the wheel, and counts it; it requires
// the mutex.
func (w *Wheel[T]) attachLocked(e *Entry[T]) {
	w.linkLocked(e)
	w.count++
}

// linkLocked links e into the slot its deadline picks, or onto the overflow
// heap when its time is past the window, without counting it — the reattach
// paths of an entry the wheel already holds; it requires the mutex.
func (w *Wheel[T]) linkLocked(e *Entry[T]) {
	if w.tickOf(e.When)-w.cursor > int64(w.buckets) {
		e.bucket = inOverflow
		e.next, e.prev = nil, nil
		w.heapPushLocked(e)
		return
	}
	tick := w.tickOf(e.When)
	slot := tick % int64(w.buckets)
	if tick <= w.cursor {
		// Due now or stranded behind the cursor: park one tick ahead, where
		// the next Advance reports it. The tick, not the slot residue,
		// decides — a past deadline's residue can land ahead of the
		// cursor's.
		slot = w.nextSlotLocked()
	}
	e.bucket = int32(slot) + 1
	if head := w.ring[slot]; head != nil {
		head.prev = e
		e.next = head
	} else {
		e.next = nil
		w.setSummaryLocked(int32(slot))
	}
	e.prev = nil
	w.ring[slot] = e
}

// detachLocked removes e from wherever it sits; it requires the mutex.
func (w *Wheel[T]) detachLocked(e *Entry[T]) {
	switch e.bucket {
	case notInWheel:
		return
	case inOverflow:
		if !w.heapRemoveLocked(e) {
			e.bucket = notInWheel // a stale entry: off the heap and uncounted
			return
		}
		e.bucket = notInWheel
		w.count--
		return
	default:
		w.unlinkLocked(e)
		e.bucket = notInWheel
		w.count--
	}
}

// unlinkLocked cuts e out of its slot's list; it requires the mutex.
func (w *Wheel[T]) unlinkLocked(e *Entry[T]) {
	slot := e.bucket - 1
	if e.prev != nil {
		e.prev.next = e.next
	} else {
		w.ring[slot] = e.next
	}
	if e.next != nil {
		e.next.prev = e.prev
	}
	if w.ring[slot] == nil {
		w.clearSummaryLocked(slot)
	}
	e.prev, e.next = nil, nil
}

// rearmEarliestLocked tells the arm callback the earliest deadline, or
// disarms; it requires the mutex. Slots are scanned cyclically from the
// cursor — within the window every entry of a slot shares the slot's tick,
// so the first occupied slot in that order holds the earliest deadline — and
// the summary finds it a word at a time, not a slot at a time.
func (w *Wheel[T]) rearmEarliestLocked() {
	// The tracked earliest stands on its own — Advance reads it to decide
	// whether the parked slot holds anything due — so it is maintained
	// whether or not an arm callback is listening.
	if w.count == 0 {
		w.earliest = time.Time{}
	} else {
		var earliest time.Time
		if len(w.overflow) > 0 {
			earliest = w.overflow[0].When
		}
		if slot, ok := w.firstOccupiedLocked(); ok {
			for e := w.ring[slot]; e != nil; e = e.next {
				if earliest.IsZero() || e.When.Before(earliest) {
					earliest = e.When
				}
			}
		}
		w.earliest = earliest
	}
	if w.arm != nil {
		w.arm(w.earliest)
	}
}

// firstOccupiedLocked is the first occupied slot at or after the cursor's
// next tick, cyclically; it requires the mutex. The cyclic order starts
// mid-word, so it runs in three segments: the start word from its start bit
// up, every following word, and finally the start word's low bits — the
// wrap's tail, which the forward scan alone would never reach.
func (w *Wheel[T]) firstOccupiedLocked() (int64, bool) {
	start := w.nextSlotLocked()
	firstWord := start / 64
	bit := uint(start % 64)
	if occupied := w.summary[firstWord] >> bit; occupied != 0 {
		return firstWord*64 + int64(bit) + int64(bits.TrailingZeros64(occupied)), true
	}
	words := int64(len(w.summary))
	for word := (firstWord + 1) % words; word != firstWord; word = (word + 1) % words {
		if occupied := w.summary[word]; occupied != 0 {
			return word*64 + int64(bits.TrailingZeros64(occupied)), true
		}
	}
	if bit != 0 {
		if occupied := w.summary[firstWord] & ((uint64(1) << bit) - 1); occupied != 0 {
			return firstWord*64 + int64(bits.TrailingZeros64(occupied)), true
		}
	}
	return 0, false
}

// heapPushLocked adds e to the overflow heap; it requires the mutex.
func (w *Wheel[T]) heapPushLocked(e *Entry[T]) {
	w.overflow = append(w.overflow, e)
	e.heap = int32(len(w.overflow)) // slot plus one; zero means off the heap
	w.heapUpLocked(len(w.overflow) - 1)
}

// heapUpLocked restores the heap above i; it requires the mutex.
func (w *Wheel[T]) heapUpLocked(i int) {
	for i > 0 {
		parent := (i - 1) / 2
		if !w.overflow[parent].When.After(w.overflow[i].When) {
			return
		}
		w.overflow[i], w.overflow[parent] = w.overflow[parent], w.overflow[i]
		w.overflow[i].heap, w.overflow[parent].heap = int32(i+1), int32(parent+1)
		i = parent
	}
}

// heapPopLocked removes the heap's top; it requires the mutex.
func (w *Wheel[T]) heapPopLocked() {
	last := len(w.overflow) - 1
	if last == 0 {
		w.overflow[0].heap = 0
		w.overflow[0] = nil
		w.overflow = w.overflow[:0]
		return
	}
	moved := w.overflow[last]
	w.overflow[0].heap = 0
	w.overflow[last] = nil
	w.overflow = w.overflow[:last]
	w.overflow[0] = moved
	moved.heap = 1
	w.heapDownLocked(0)
}

// heapRemoveLocked removes e, wherever it sits, and reports whether it was
// on the heap at all; it requires the mutex.
func (w *Wheel[T]) heapRemoveLocked(e *Entry[T]) bool {
	i := int(e.heap) - 1
	if i < 0 || i >= len(w.overflow) || w.overflow[i] != e {
		return false // not on the heap; a stale entry asks twice
	}
	last := len(w.overflow) - 1
	if i == last {
		w.overflow[last] = nil
		w.overflow = w.overflow[:last]
		e.heap = 0
		return true
	}
	moved := w.overflow[last]
	w.overflow[last] = nil
	w.overflow = w.overflow[:last]
	w.overflow[i] = moved
	moved.heap = int32(i + 1)
	e.heap = 0
	// The moved entry goes one way only: up when it is earlier than its new
	// parent, down otherwise.
	if i > 0 && moved.When.Before(w.overflow[(i-1)/2].When) {
		w.heapUpLocked(i)
	} else {
		w.heapDownLocked(i)
	}
	return true
}

// heapDownLocked restores the heap under i; it requires the mutex.
func (w *Wheel[T]) heapDownLocked(i int) {
	for {
		left, right := 2*i+1, 2*i+2
		smallest := i
		if left < len(w.overflow) && w.overflow[left].When.Before(w.overflow[smallest].When) {
			smallest = left
		}
		if right < len(w.overflow) && w.overflow[right].When.Before(w.overflow[smallest].When) {
			smallest = right
		}
		if smallest == i {
			return
		}
		w.overflow[i], w.overflow[smallest] = w.overflow[smallest], w.overflow[i]
		w.overflow[i].heap, w.overflow[smallest].heap = int32(i+1), int32(smallest+1)
		i = smallest
	}
}

func (w *Wheel[T]) setSummaryLocked(slot int32) {
	w.summary[slot/64] |= 1 << uint(slot%64)
}

func (w *Wheel[T]) clearSummaryLocked(slot int32) {
	w.summary[slot/64] &^= 1 << uint(slot%64)
}
