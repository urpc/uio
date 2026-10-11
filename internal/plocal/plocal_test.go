package plocal

import (
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
)

type item struct {
	owner atomic.Int64
	id    int64
}

func newItems(created *atomic.Int64) func() *item {
	return func() *item { return &item{id: created.Add(1)} }
}

func setProcs(t *testing.T, n int) {
	t.Helper()
	previous := runtime.GOMAXPROCS(n)
	t.Cleanup(func() { runtime.GOMAXPROCS(previous) })
}

// However goroutines move between Ps, and whatever the number of Ps the cache
// was sized for, a value is held by at most one borrower at a time.
func TestBorrowersNeverShareAValue(t *testing.T) {
	for _, procs := range []struct{ init, run int }{{1, 1}, {4, 4}, {2, 8}, {8, 2}} {
		setProcs(t, procs.init)
		var created atomic.Int64
		var cache Cache[*item]
		cache.Init(newItems(&created))
		runtime.GOMAXPROCS(procs.run)

		const borrowers, rounds = 16, 20000
		var wg sync.WaitGroup
		var shared atomic.Int64
		for b := range borrowers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				me := int64(b + 1)
				for r := range rounds {
					value := cache.Get()
					if !value.owner.CompareAndSwap(0, me) {
						shared.Add(1)
					}
					if r%64 == 0 {
						runtime.Gosched()
					}
					value.owner.Store(0)
					cache.Put(value)
				}
			}()
		}
		wg.Wait()
		if shared.Load() != 0 {
			t.Fatalf("procs %v: a value was lent to two borrowers %d times", procs, shared.Load())
		}
		if created.Load() == 0 || created.Load() > borrowers*rounds {
			t.Fatalf("procs %v: created %d values", procs, created.Load())
		}
	}
}

func TestGetWithoutAValueOrNewReturnsZero(t *testing.T) {
	var cache Cache[*item]
	if value := cache.Get(); value != nil {
		t.Fatalf("Get on a cache that was never given a value = %v, want nil", value)
	}
	cache.Init(nil)
	if value := cache.Get(); value != nil {
		t.Fatalf("Get without newValue = %v, want nil", value)
	}
}

func TestGetCallsNewWhenEmpty(t *testing.T) {
	var created atomic.Int64
	var cache Cache[*item]
	cache.Init(newItems(&created))
	if value := cache.Get(); value == nil || created.Load() != 1 {
		t.Fatalf("Get on an empty cache = %v after %d creations, want a new value", value, created.Load())
	}
}

func TestNonPointerValues(t *testing.T) {
	var cache Cache[[2]int]
	cache.Init(func() [2]int { return [2]int{7, 7} })
	if value := cache.Get(); value != [2]int{7, 7} {
		t.Fatalf("Get = %v, want the new value", value)
	}
	cache.Put([2]int{1, 2})
	if value := cache.Get(); value != [2]int{1, 2} && value != [2]int{7, 7} {
		t.Fatalf("Get after Put = %v", value)
	}
}

var sink int

// work stands in for what a borrower does between Get and Put, so the
// benchmarks measure the exchange as a borrower sees it rather than back-to-back
// pins, whose dependent updates of the same word dominate a tight loop.
func work(value *item) {
	sum := 0
	for i := range 32 {
		sum += i * int(value.id+1)
	}
	sink += sum
}

func BenchmarkCache(b *testing.B) {
	var cache Cache[*item]
	cache.Init(func() *item { return new(item) })
	for range b.N {
		value := cache.Get()
		work(value)
		cache.Put(value)
		work(value)
	}
}

func BenchmarkPool(b *testing.B) {
	pool := sync.Pool{New: func() any { return new(item) }}
	for range b.N {
		value := pool.Get().(*item)
		work(value)
		pool.Put(value)
		work(value)
	}
}
