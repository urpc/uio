package shardpool

import (
	"sync"
	"testing"
)

// Every phase crosses both tier thresholds with simultaneous lifecycle calls.
// In particular, a late first split/merge must not undo the second one.
func TestConcurrentKeyLifecyclesEndAtCorrectTier(t *testing.T) {
	const base, workers, perWorker, rounds = 4, 16, 256, 16
	pool := New(base, 2, taskKey, func(*task) {})
	defer pool.Stop()
	parallel := func(change func()) {
		var group sync.WaitGroup
		start := make(chan struct{})
		for range workers {
			group.Add(1)
			go func() {
				defer group.Done()
				<-start
				for range perWorker {
					change()
				}
			}()
		}
		close(start)
		group.Wait()
	}
	for round := range rounds {
		parallel(pool.AddKey)
		if got := pool.keys.Load(); got != workers*perWorker {
			t.Fatalf("round %d after grow: keys=%d", round, got)
		}
		if got := pool.active.Load(); got != 4*base {
			t.Fatalf("round %d after grow: active=%d, want %d", round, got, 4*base)
		}
		parallel(pool.RemoveKey)
		if got := pool.keys.Load(); got != 0 {
			t.Fatalf("round %d after shrink: keys=%d", round, got)
		}
		if got := pool.active.Load(); got != base {
			t.Fatalf("round %d after shrink: active=%d, want %d", round, got, base)
		}
	}
}
