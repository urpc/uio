//go:build !race

package pool

import (
	"runtime"
	"testing"
)

func onOneP(t *testing.T) {
	t.Helper()
	previous := runtime.GOMAXPROCS(1)
	t.Cleanup(func() { runtime.GOMAXPROCS(previous) })
}

// Classes up to maxSlottedSize keep an object per P, which a GC leaves alone;
// larger ones use the pool alone, which two GCs empty.
func TestOnlySmallClassesKeepAnObjectPerP(t *testing.T) {
	onOneP(t)
	p := New[*int](64 << 10)
	if len(p.cached) != 6 || len(p.pooled) != 8 {
		t.Fatalf("cached %d and pooled %d classes, want 512 B to 16 KiB cached of 8", len(p.cached), len(p.pooled))
	}
	small, large := new(int), new(int)
	p.Put(small, maxSlottedSize)
	p.Put(large, 2*maxSlottedSize)
	runtime.GC()
	runtime.GC()
	if got, n := p.Get(maxSlottedSize); got != small || n != maxSlottedSize {
		t.Fatalf("Get(%d) = %p, %d after two GCs, want the slot's %p", maxSlottedSize, got, n, small)
	}
	if got, _ := p.Get(2 * maxSlottedSize); got != nil {
		t.Fatalf("Get(%d) after two GCs = %p, want nil from the emptied pool", 2*maxSlottedSize, got)
	}
}

func TestPoolsBelowTheSlotLimitCacheEveryClass(t *testing.T) {
	p := New[*int](32)
	if len(p.cached) != 6 {
		t.Fatalf("cached %d classes of a pool up to 32 B, want all 6", len(p.cached))
	}
	large := New[*int](maxSlottedSize >> 1)
	if len(large.cached) != len(large.pooled) {
		t.Fatalf("cached %d of %d classes of a pool below the slot limit, want all", len(large.cached), len(large.pooled))
	}
}
