//go:build !race

package plocal

import (
	"runtime"
	"runtime/debug"
	"sync/atomic"
	"testing"
)

// The slot tests run on one P, so every Get and Put reaches the same slot, and
// with the GC off, so the pool behind it keeps what it is given.
func onOneP(t *testing.T) {
	t.Helper()
	setProcs(t, 1)
	previous := debug.SetGCPercent(-1)
	t.Cleanup(func() { debug.SetGCPercent(previous) })
}

func TestGetReturnsWhatThisPPut(t *testing.T) {
	onOneP(t)
	var created atomic.Int64
	var cache Cache[*item]
	cache.Init(newItems(&created))
	first := cache.Get()
	cache.Put(first)
	if again := cache.Get(); again != first {
		t.Fatalf("Get after Put on the same P = %d, want %d", again.id, first.id)
	}
	if created.Load() != 1 {
		t.Fatalf("created %d values, want 1", created.Load())
	}
	// The slot gave its value away and keeps no copy.
	if other := cache.Get(); other == first {
		t.Fatal("Get handed out a value that was still borrowed")
	}
}

func TestPutToAFullSlotGoesToThePool(t *testing.T) {
	onOneP(t)
	var created atomic.Int64
	var cache Cache[*item]
	cache.Init(newItems(&created))
	a, b := cache.Get(), cache.Get()
	cache.Put(a)
	cache.Put(b)
	if got := cache.Get(); got != a {
		t.Fatalf("first Get = %d, want the slot's %d", got.id, a.id)
	}
	if got := cache.Get(); got != b {
		t.Fatalf("second Get = %d, want the pool's %d", got.id, b.id)
	}
	if created.Load() != 2 {
		t.Fatalf("created %d values, want 2", created.Load())
	}
}

func TestSlotsSurviveGC(t *testing.T) {
	setProcs(t, 1)
	var cache Cache[*item]
	cache.Init(func() *item { return new(item) })
	value := cache.Get()
	cache.Put(value)
	runtime.GC()
	runtime.GC()
	if got := cache.Get(); got != value {
		t.Fatal("two GCs emptied the slot")
	}
}

func TestSlotsCoverEveryCPU(t *testing.T) {
	setProcs(t, 1)
	var cache Cache[*item]
	cache.Init(nil)
	if want := max(1, runtime.NumCPU()); len(cache.slots) != want {
		t.Fatalf("slots = %d, want one per CPU (%d) though GOMAXPROCS is 1", len(cache.slots), want)
	}
	for _, pid := range []int{-1, len(cache.slots), len(cache.slots) + 63} {
		if cache.slotOf(pid) != nil {
			t.Fatalf("slotOf(%d) is a slot", pid)
		}
	}
	if cache.slotOf(0) == nil || cache.slotOf(len(cache.slots)-1) == nil {
		t.Fatal("a P the cache was sized for has no slot")
	}
}

func TestGetPutDoesNotAllocate(t *testing.T) {
	setProcs(t, 1)
	var cache Cache[*item]
	cache.Init(func() *item { return new(item) })
	cache.Put(cache.Get())
	if allocations := testing.AllocsPerRun(1000, func() { cache.Put(cache.Get()) }); allocations != 0 {
		t.Fatalf("Get and Put allocate %.2f times, want 0", allocations)
	}
}

func TestCacheWithoutInitIsThePool(t *testing.T) {
	onOneP(t)
	var cache Cache[*item]
	value := new(item)
	cache.Put(value)
	if got := cache.Get(); got != value {
		t.Fatal("a cache without slots lost what its pool was given")
	}
	cache.Put(value)
	runtime.GC()
	runtime.GC()
	if got := cache.Get(); got != nil {
		t.Fatal("a cache without slots kept a value through two GCs")
	}
}
