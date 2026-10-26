package timewheel

import (
	"testing"
	"time"
)

const (
	benchTick    = 4 * time.Millisecond
	benchBuckets = 16384
)

func BenchmarkArmCancel(b *testing.B) {
	start := time.Now()
	w := New[int](benchTick, benchBuckets, start, nil)
	entries := make([]*Entry[int], 1024)
	for i := range entries {
		entries[i] = &Entry[int]{Val: i}
	}
	b.ResetTimer()
	for i := 0; b.Loop(); i++ {
		e := entries[i%len(entries)]
		w.Arm(e, time.Now().Add(time.Duration(i%6000)*benchTick), 0)
		if i%4 == 0 {
			w.Cancel(e)
		}
	}
}

func BenchmarkAdvance(b *testing.B) {
	start := time.Now()
	w := New[int](benchTick, benchBuckets, start, nil)
	entries := make([]*Entry[int], 4096)
	for i := range entries {
		entries[i] = &Entry[int]{Val: i}
		w.Arm(entries[i], start.Add(time.Duration(i%4096)*benchTick), 10*benchTick)
	}
	now := start
	var due []*Entry[int]
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		now = now.Add(benchTick)
		due = w.AdvanceInto(now, due[:0])
	}
}

// BenchmarkArmSteady arms deadlines that are mostly later than the earliest —
// the heartbeat shape, where the fast path skips the slot scan.
func BenchmarkArmSteady(b *testing.B) {
	start := time.Now()
	w := New[int](benchTick, benchBuckets, start, nil)
	entries := make([]*Entry[int], 1024)
	for i := range entries {
		entries[i] = &Entry[int]{Val: i}
	}
	now := start
	// Fill the wheel so an earliest exists.
	for i, e := range entries {
		w.Arm(e, now.Add(time.Duration(64+i%64)*benchTick), 0)
	}
	b.ResetTimer()
	for i := 0; b.Loop(); i++ {
		now = now.Add(benchTick)
		w.Arm(entries[i%len(entries)], now.Add(time.Duration(64+i%64)*benchTick), 0)
	}
}

// BenchmarkAdvanceLongGap is the idle-server shape: a far deadline, then one
// Advance a day later. The fast-forward must keep it at the window's cost,
// not the gap's.
func BenchmarkAdvanceLongGap(b *testing.B) {
	start := time.Now()
	w := New[int](benchTick, benchBuckets, start, nil)
	entry := &Entry[int]{Val: 1}
	// Each round arms far ahead of where the last ended, so every Advance
	// crosses a fresh hour-long gap — well past the window, the fast-forward
	// is what it measures — and the wheel is rebuilt long before the base
	// could leave the range time.Time carries.
	const gap = time.Hour
	base := start
	b.ResetTimer()
	for b.Loop() {
		w.Arm(entry, base.Add(gap/2), 0)
		w.Advance(base.Add(gap))
		base = base.Add(gap)
		if base.Sub(start) > 10*365*24*time.Hour {
			b.StopTimer()
			// A fresh wheel with the clock it will run on: the base resets
			// with it every decade, far inside UnixNano's 2262 ceiling
			// whatever -benchtime asks for.
			base = start
			w = New[int](benchTick, benchBuckets, base, nil)
			b.StartTimer()
		}
	}
}

// BenchmarkOverflowChurn arms and cancels an idle-timeout crowd on the
// overflow heap — deadlines past the ring's span.
func BenchmarkOverflowChurn(b *testing.B) {
	start := time.Now()
	w := New[int](benchTick, benchBuckets, start, nil)
	entries := make([]*Entry[int], 8192)
	for i := range entries {
		entries[i] = &Entry[int]{Val: i}
	}
	b.ResetTimer()
	for i := 0; b.Loop(); i++ {
		e := entries[i%len(entries)]
		w.Arm(e, start.Add(time.Duration(60+i%600)*time.Second), 0)
		if i%4 == 0 {
			w.Cancel(e)
		}
	}
}

// no-op arm: the benchmarks above measure the wheel without its callback;
// this one adds the recompute and callback dispatch the integration pays.
func noOpArm(time.Time) {}

// BenchmarkArmCancelArmed and BenchmarkArmSteadyArmed run with the arm
// callback installed: every earliest move pays the summary scan and the
// callback, the shape the loop's kernel timer sees.
func BenchmarkArmCancelArmed(b *testing.B) {
	start := time.Now()
	w := New[int](benchTick, benchBuckets, start, noOpArm)
	entries := make([]*Entry[int], 1024)
	for i := range entries {
		entries[i] = &Entry[int]{Val: i}
	}
	b.ResetTimer()
	for i := 0; b.Loop(); i++ {
		e := entries[i%len(entries)]
		w.Arm(e, time.Now().Add(time.Duration(i%6000)*benchTick), 0)
		if i%4 == 0 {
			w.Cancel(e)
		}
	}
}

func BenchmarkArmSteadyArmed(b *testing.B) {
	start := time.Now()
	w := New[int](benchTick, benchBuckets, start, noOpArm)
	entries := make([]*Entry[int], 1024)
	for i := range entries {
		entries[i] = &Entry[int]{Val: i}
	}
	now := start
	for i, e := range entries {
		w.Arm(e, now.Add(time.Duration(64+i%64)*benchTick), 0)
	}
	b.ResetTimer()
	for i := 0; b.Loop(); i++ {
		now = now.Add(benchTick)
		w.Arm(entries[i%len(entries)], now.Add(time.Duration(64+i%64)*benchTick), 0)
	}
}
