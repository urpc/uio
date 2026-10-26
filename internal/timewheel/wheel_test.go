package timewheel

import (
	"math/rand"
	"sync"
	"testing"
	"time"
)

// recorder hears the arm callback; tests read what the kernel would have
// been told.
type recorder struct {
	mu    sync.Mutex
	armed time.Time
	calls int
}

func (r *recorder) arm(when time.Time) {
	r.mu.Lock()
	r.armed, r.calls = when, r.calls+1
	r.mu.Unlock()
}

const testTick = time.Millisecond

// advanceDue runs Advance and returns the due entries' values.
func dueVals[T any](w *Wheel[T], now time.Time) []T {
	var vals []T
	for _, e := range w.Advance(now) {
		vals = append(vals, e.Val)
	}
	return vals
}

// A deadline fires on the Advance that passes it, and not before.
func TestDeadlineFiresOnAdvance(t *testing.T) {
	rec := &recorder{}
	start := time.Now()
	w := New[int](testTick, 64, start, rec.arm)
	e := &Entry[int]{Val: 7}
	w.Arm(e, start.Add(10*testTick), 0)

	if got := dueVals(w, start.Add(5*testTick)); got != nil {
		t.Fatalf("fired early: %v", got)
	}
	if got := dueVals(w, start.Add(10*testTick)); len(got) != 1 || got[0] != 7 {
		t.Fatalf("Advance at the deadline = %v, want [7]", got)
	}
	if !w.Empty() || w.Len() != 0 {
		t.Fatalf("wheel holds %d entries after a one-shot fired", w.Len())
	}
}

// A past deadline is due at the next Advance, at its own time.
func TestPastDeadlineDueAtNextAdvance(t *testing.T) {
	start := time.Now()
	w := New[int](testTick, 64, start, nil)
	e := &Entry[int]{Val: 1}
	w.Arm(e, start.Add(-time.Hour), 0)
	if got := dueVals(w, start); len(got) != 1 || got[0] != 1 {
		t.Fatalf("past deadline Advance = %v, want [1]", got)
	}
}

// Cancel keeps a deadline from firing, and the last cancel disarms.
func TestCancelDisarms(t *testing.T) {
	rec := &recorder{}
	start := time.Now()
	w := New[int](testTick, 64, start, rec.arm)
	e := &Entry[int]{Val: 1}
	w.Arm(e, start.Add(10*testTick), 0)
	if rec.armed.IsZero() {
		t.Fatal("arming the only entry did not arm the wheel")
	}
	w.Cancel(e)
	if !rec.armed.IsZero() {
		t.Fatalf("the last cancel left the wheel armed at %v", rec.armed)
	}
	if got := dueVals(w, start.Add(time.Hour)); got != nil {
		t.Fatalf("a cancelled entry fired: %v", got)
	}
}

// Re-arming moves the deadline; the old one never fires.
func TestRearmMovesDeadline(t *testing.T) {
	start := time.Now()
	w := New[int](testTick, 64, start, nil)
	e := &Entry[int]{Val: 3}
	w.Arm(e, start.Add(5*testTick), 0)
	w.Arm(e, start.Add(50*testTick), 0)
	if got := dueVals(w, start.Add(10*testTick)); got != nil {
		t.Fatalf("the first deadline fired after a re-arm: %v", got)
	}
	if got := dueVals(w, start.Add(50*testTick)); len(got) != 1 || got[0] != 3 {
		t.Fatalf("the re-armed deadline did not fire: %v", got)
	}
	if w.Len() != 0 {
		t.Fatalf("wheel holds %d after firing", w.Len())
	}
}

// A periodic entry fires every period until cancelled, without a burst after
// a late advance.
func TestPeriodicFiresUntilCancelled(t *testing.T) {
	start := time.Now()
	w := New[int](testTick, 64, start, nil)
	e := &Entry[int]{Val: 9}
	w.Arm(e, start.Add(2*testTick), 5*testTick)

	got := dueVals(w, start.Add(7*testTick))
	if len(got) != 1 || got[0] != 9 {
		t.Fatalf("first period = %v, want [9]", got)
	}
	// Two periods pass at once: one fires now, the next is armed from now.
	got = dueVals(w, start.Add(20*testTick))
	if len(got) != 1 {
		t.Fatalf("late advance fired %d entries, want 1", len(got))
	}
	w.Cancel(e)
	if got := dueVals(w, start.Add(time.Hour)); got != nil {
		t.Fatalf("a cancelled periodic fired: %v", got)
	}
	if w.Len() != 0 {
		t.Fatalf("wheel holds %d after cancel", w.Len())
	}
}

// Deadlines past the ring's span wait on overflow, fire on time, and re-enter
// the ring as their window comes.
func TestOverflowDeadlines(t *testing.T) {
	const buckets = 64
	start := time.Now()
	w := New[int](testTick, buckets, start, nil)
	span := time.Duration(buckets) * testTick
	far := &Entry[int]{Val: 5}
	w.Arm(far, start.Add(100*span), 0)
	if w.Len() != 1 {
		t.Fatalf("far deadline not held: %d", w.Len())
	}
	// A deadline just inside the span shares the wheel with it.
	near := &Entry[int]{Val: 6}
	w.Arm(near, start.Add(span/2), 0)

	if got := dueVals(w, start.Add(span/2)); len(got) != 1 || got[0] != 6 {
		t.Fatalf("near deadline = %v, want [6]", got)
	}
	if w.Len() != 1 {
		t.Fatalf("wheel holds %d with only the far deadline", w.Len())
	}
	if got := dueVals(w, start.Add(100*span)); len(got) != 1 || got[0] != 5 {
		t.Fatalf("far deadline = %v, want [5]", got)
	}
	if !w.Empty() {
		t.Fatalf("wheel holds %d after both fired", w.Len())
	}
}

// The arm callback always carries the earliest deadline: arming an earlier
// one moves it, disarming the last zeroes it, and an advance re-aims it at
// what remains.
func TestArmCallbackTracksEarliest(t *testing.T) {
	rec := &recorder{}
	start := time.Now()
	w := New[int](testTick, 64, start, rec.arm)
	late := &Entry[int]{Val: 1}
	early := &Entry[int]{Val: 2}
	w.Arm(late, start.Add(20*testTick), 0)
	if !rec.armed.Equal(start.Add(20 * testTick)) {
		t.Fatalf("armed at %v after the first arm", rec.armed)
	}
	w.Arm(early, start.Add(5*testTick), 0)
	if !rec.armed.Equal(start.Add(5 * testTick)) {
		t.Fatalf("armed at %v after an earlier arm", rec.armed)
	}
	dueVals(w, start.Add(5*testTick))
	if !rec.armed.Equal(start.Add(20 * testTick)) {
		t.Fatalf("armed at %v after the early deadline fired", rec.armed)
	}
	w.Cancel(late)
	if !rec.armed.IsZero() {
		t.Fatalf("armed at %v after the last cancel", rec.armed)
	}
}

// A random campaign against a brute-force oracle: every arm, cancel and
// advance is mirrored in a map of live deadlines, and each advance's due set
// must match the oracle exactly.
func TestWheelMatchesOracle(t *testing.T) {
	rec := &recorder{}
	const buckets = 128
	start := time.Now()
	w := New[int](testTick, buckets, start, rec.arm)
	rng := rand.New(rand.NewSource(42))
	now := start

	live := make(map[int]*Entry[int]) // val -> entry
	expected := make(map[int]time.Time)
	var fired int

	checkEarliest := func(step int) {
		want := time.Time{}
		for _, when := range expected {
			if want.IsZero() || when.Before(want) {
				want = when
			}
		}
		if !w.earliest.Equal(want) {
			t.Fatalf("step %d: earliest = %v, want the live minimum %v", step, w.earliest, want)
		}
	}

	for step := 0; step < 20000; step++ {
		switch rng.Intn(10) {
		case 0, 1, 2: // arm a new or existing deadline, past ones included
			val := rng.Intn(40)
			offset := rng.Intn(3*buckets) - buckets/8 // a slice of them behind now
			when := now.Add(time.Duration(offset) * testTick)
			e := live[val]
			if e == nil {
				e = &Entry[int]{Val: val}
				live[val] = e
			}
			w.Arm(e, when, 0)
			expected[val] = when
			checkEarliest(step)
		case 3: // cancel
			val := rng.Intn(40)
			if e := live[val]; e != nil {
				w.Cancel(e)
				delete(live, val)
				delete(expected, val)
				checkEarliest(step)
			}
		default: // advance, occasionally a long jump
			jump := time.Duration(1+rng.Intn(8)) * testTick
			if rng.Intn(500) == 0 {
				jump = time.Duration(1+rng.Intn(40*buckets)) * testTick
			}
			now = now.Add(jump)
			got := dueVals(w, now)
			var want []int
			for val, when := range expected {
				if !when.After(now) {
					want = append(want, val)
				}
			}
			if len(got) != len(want) {
				t.Fatalf("step %d: advance at +%v fired %v, want %v",
					step, now.Sub(start), got, want)
			}
			seen := make(map[int]bool)
			for _, val := range got {
				seen[val] = true
			}
			for _, val := range want {
				if !seen[val] {
					t.Fatalf("step %d: advance missed %d (fired %v, want %v)",
						step, val, got, want)
				}
				delete(live, val)
				delete(expected, val)
			}
			checkEarliest(step)
			fired += len(got)
		}
	}
	// The wheel ends empty of everything not live.
	if w.Len() != len(live) {
		t.Fatalf("wheel holds %d, oracle %d", w.Len(), len(live))
	}
	if fired == 0 {
		t.Fatal("the campaign never fired a deadline")
	}
}

func spanOf(buckets int) time.Duration { return time.Duration(buckets) * testTick }

// Concurrent arms, cancels and advances from many goroutines stay consistent:
// no lost entries, no phantom fires, no deadlock.
func TestConcurrentOperations(t *testing.T) {
	start := time.Now()
	w := New[int](testTick, 256, start, nil)
	entries := make([]*Entry[int], 64)
	for i := range entries {
		entries[i] = &Entry[int]{Val: i}
	}
	stop := make(chan struct{})
	var churners, advancers sync.WaitGroup

	for worker := 0; worker < 8; worker++ {
		churners.Add(1)
		go func(worker int) {
			defer churners.Done()
			rng := rand.New(rand.NewSource(int64(worker)))
			for range 20000 {
				e := entries[rng.Intn(len(entries))]
				if rng.Intn(4) == 0 {
					w.Cancel(e)
				} else {
					w.Arm(e, time.Now().Add(time.Duration(rng.Intn(600))*testTick), 0)
				}
			}
		}(worker)
	}
	for range 4 {
		advancers.Add(1)
		go func() {
			defer advancers.Done()
			now := start
			for {
				select {
				case <-stop:
					return
				default:
				}
				now = now.Add(testTick)
				w.Advance(now)
			}
		}()
	}
	churners.Wait()
	close(stop)
	advancers.Wait()
	// After the churn every entry is either armed or not; draining the wheel
	// must not panic or hang.
	w.Advance(start.Add(time.Hour))
}

// A deadline armed exactly one window ahead sits in the slot the cursor pops
// as it reaches its tick — a wall time still ahead at that moment — so it
// drifts forward a slot an Advance at a time and fires on time: never early,
// never lost.
func TestWindowWrapDriftsForward(t *testing.T) {
	const buckets = 64
	rec := &recorder{}
	start := time.Now()
	w := New[int](testTick, buckets, start, rec.arm)
	span := time.Duration(buckets) * testTick
	e := &Entry[int]{Val: 4}
	w.Arm(e, start.Add(span), 0)
	if !rec.armed.Equal(start.Add(span)) {
		t.Fatalf("armed at %v, want the window-wrapped deadline", rec.armed)
	}

	// Into the entry's tick, but before its wall time: it drifts, not fires.
	if got := dueVals(w, start.Add(span-testTick/2)); got != nil {
		t.Fatalf("fired early: %v", got)
	}
	if w.Len() != 1 {
		t.Fatalf("the drifting entry was lost: %d held", w.Len())
	}
	// Past its time: it fires.
	if got := dueVals(w, start.Add(span+2*testTick)); len(got) != 1 || got[0] != 4 {
		t.Fatalf("window-wrapped entry = %v, want [4]", got)
	}
	if !w.Empty() {
		t.Fatalf("wheel holds %d after firing", w.Len())
	}
}

// The summary scan must find an entry in the start word's low bits — the
// wrap's tail, which a forward-only scan misses. Deterministic: the cursor
// sits at bit b-1 of the summary word, so the cyclic scan starts at bit b,
// and the entry is armed into every slot below it.
func TestFirstOccupiedScansStartWordTail(t *testing.T) {
	tick := time.Millisecond
	const buckets = 64 // one summary word: every slot below the start bit is the tail
	for bit := 1; bit <= 64; bit++ {
		// cursor % 64 == bit-1, aligned on a word boundary.
		milli := (time.Now().UnixMilli()/64+1)*64 + int64(bit-1)
		now := time.UnixMilli(milli)
		for slot := 0; slot < bit-1; slot++ {
			rec := &recorder{}
			w := New[int](tick, buckets, now, rec.arm)
			// The slot sits bit-1-slot ticks behind the cursor in cyclic
			// order, one wrap ahead: 64 - (bit-1-slot) ticks from now.
			offset := 64 - (bit - 1 - slot)
			when := now.Add(time.Duration(offset) * tick)
			w.Arm(&Entry[int]{Val: 1}, when, 0)
			if !rec.armed.Equal(when) {
				t.Fatalf("cursor bit %d, slot %d: armed at %v, want %v",
					bit, slot, rec.armed, when)
			}
		}
	}
}

// A day-long idle gap, then one Advance: the fast-forward keeps it at the
// window's cost, and the far deadline the gap straddled still fires — as do
// the ring entries the fast-forward swept past.
func TestAdvanceLongGapFiresRingEntry(t *testing.T) {
	const buckets = 64
	start := time.Now()
	w := New[int](testTick, buckets, start, nil)
	span := time.Duration(buckets) * testTick
	near := &Entry[int]{Val: 1}
	far := &Entry[int]{Val: 2}
	w.Arm(near, start.Add(3*testTick), 0)
	w.Arm(far, start.Add(100*span), 0)

	began := time.Now()
	due := w.Advance(start.Add(24 * time.Hour))
	if elapsed := time.Since(began); elapsed > 5*time.Millisecond {
		t.Fatalf("24h Advance took %v, want the fast-forward", elapsed)
	}
	seen := map[int]bool{}
	for _, e := range due {
		seen[e.Val] = true
	}
	if !seen[1] {
		t.Fatal("the ring entry the gap swept past never fired")
	}
	if !seen[2] {
		t.Fatal("the overflow entry never fired")
	}
	if !w.Empty() {
		t.Fatalf("wheel holds %d after the gap", w.Len())
	}
}

// Times UnixNano cannot carry stay on their own side: a deadline past 2262 is
// far future — held, never fired — and one before 1970 is long past — due at
// the next Advance.
func TestDeadlinesBeyondUnixNano(t *testing.T) {
	start := time.Now()
	w := New[int](testTick, 64, start, nil)
	far := &Entry[int]{Val: 1}
	past := &Entry[int]{Val: 2}
	w.Arm(far, time.Date(2400, 1, 1, 0, 0, 0, 0, time.UTC), 0)
	w.Arm(past, time.Date(1900, 1, 1, 0, 0, 0, 0, time.UTC), 0)
	got := dueVals(w, start.Add(time.Hour))
	if len(got) != 1 || got[0] != 2 {
		t.Fatalf("Advance = %v, want only the pre-1970 deadline [2]", got)
	}
	if w.Len() != 1 || far.bucket != inOverflow {
		t.Fatalf("the year-2400 deadline left the overflow heap: held %d, bucket %d", w.Len(), far.bucket)
	}
}

// Arming the zero time cancels: the zero time is the wheel's "nothing armed",
// so an entry carrying it would be stranded under a disarmed callback.
func TestArmZeroTimeCancels(t *testing.T) {
	rec := &recorder{}
	start := time.Now()
	w := New[int](testTick, 64, start, rec.arm)
	e := &Entry[int]{Val: 1}
	w.Arm(e, start.Add(5*testTick), 0)
	w.Arm(e, time.Time{}, 0)
	if !w.Empty() {
		t.Fatalf("a zero-time Arm left %d entries", w.Len())
	}
	if !rec.armed.IsZero() {
		t.Fatalf("armed at %v after the zero-time Arm, want disarmed", rec.armed)
	}
}

// Removing from the middle of the overflow heap keeps it a heap whichever way
// the moved entry has to go.
func TestOverflowHeapRemoveKeepsOrder(t *testing.T) {
	start := time.Now()
	w := New[int](testTick, 8, start, nil)
	rng := rand.New(rand.NewSource(7))
	entries := make([]*Entry[int], 200)
	for i := range entries {
		entries[i] = &Entry[int]{Val: i}
		w.Arm(entries[i], start.Add(time.Duration(100+rng.Intn(5000))*testTick), 0)
	}
	for _, i := range rng.Perm(len(entries))[:120] {
		w.Cancel(entries[i])
		w.mu.Lock()
		for j := 1; j < len(w.overflow); j++ {
			parent := (j - 1) / 2
			if w.overflow[j].When.Before(w.overflow[parent].When) {
				w.mu.Unlock()
				t.Fatalf("heap order broken at %d after cancelling %d", j, i)
			}
			if int(w.overflow[j].heap) != j+1 {
				w.mu.Unlock()
				t.Fatalf("entry at %d records index %d", j, w.overflow[j].heap)
			}
		}
		w.mu.Unlock()
	}
}

// A wheel whose clock sits past UnixNano's range has its cursor clamped to
// the last tick; with a one-nanosecond tick that is MaxInt64, and the slot
// after it must not overflow into a negative index. The bucket count is not a
// power of two: with one, MinInt64 modulo it is zero and the overflow would
// land on the right slot by accident.
func TestCursorAtLastTick(t *testing.T) {
	now := time.Date(2400, 1, 1, 0, 0, 0, 0, time.UTC)
	rec := &recorder{}
	w := New[int](time.Nanosecond, 100, now, rec.arm)
	e := &Entry[int]{Val: 1}
	w.Arm(e, now.Add(time.Hour), 0)
	if rec.armed.IsZero() {
		t.Fatal("arming at the last tick left the wheel disarmed")
	}
	if got := dueVals(w, now.Add(2*time.Hour)); len(got) != 1 || got[0] != 1 {
		t.Fatalf("Advance at the last tick = %v, want [1]", got)
	}
	if !w.Empty() {
		t.Fatalf("wheel holds %d after firing", w.Len())
	}
}

// AdvanceInto appends: a periodic entry the caller already holds in due — an
// earlier batch it has not drained — is not re-armed a second time.
func TestAdvanceIntoLeavesPrefixAlone(t *testing.T) {
	start := time.Now()
	w := New[int](testTick, 64, start, nil)
	periodic := &Entry[int]{Val: 1}
	w.Arm(periodic, start.Add(2*testTick), 10*testTick)
	first := w.AdvanceInto(start.Add(3*testTick), nil)
	if len(first) != 1 || first[0] != periodic {
		t.Fatalf("first Advance = %d entries, want the periodic one", len(first))
	}
	rearmed := periodic.When

	// A second Advance with nothing due, appending after the first batch.
	other := &Entry[int]{Val: 2}
	w.Arm(other, start.Add(4*testTick), 0)
	both := w.AdvanceInto(start.Add(5*testTick), first)
	if len(both) != 2 || both[1] != other {
		t.Fatalf("second Advance = %d entries, want the prefix plus the new one", len(both))
	}
	if !periodic.When.Equal(rearmed) {
		t.Fatalf("the prefix's periodic entry was re-armed again: %v, want %v", periodic.When, rearmed)
	}
	if w.Len() != 1 {
		t.Fatalf("wheel holds %d, want only the periodic entry", w.Len())
	}
}
