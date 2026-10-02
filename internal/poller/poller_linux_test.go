//go:build linux && !stdio

package poller

import (
	"sync"
	"testing"
	"time"
)

// TestCloseReleasesEveryWaiter runs a shared control wake first, so every
// waiter observes and drains it, and then blocks them again before closing.
// Each waiter must be released on its own descriptor: the shared wakefd a
// waiter consumed cannot be relied on to release the others.
func TestCloseReleasesEveryWaiter(t *testing.T) {
	for attempt := 0; attempt < 20; attempt++ {
		p, err := NewNetPoller()
		if err != nil {
			t.Fatal(err)
		}
		const waiters = 4
		var woke sync.WaitGroup
		for i := 0; i < waiters; i++ {
			batch := &Batch{}
			out := make([]Event, 8)
			woke.Add(1)
			go func() {
				defer woke.Done()
				for {
					_, _ = p.WaitBatch(batch, out, -1)
					if p.Closed() {
						return
					}
				}
			}()
		}
		// Let every waiter block, then run one shared wake round: each of
		// them returns, drains the shared descriptor, and blocks again.
		time.Sleep(50 * time.Millisecond)
		_ = p.Wake()
		time.Sleep(50 * time.Millisecond)
		_ = p.Close(nil)

		done := make(chan struct{})
		go func() { woke.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatalf("attempt %d: Close parked a waiter", attempt)
		}
		p.release()
	}
}

// A waiter that reaches WaitBatch after Close must leave the descriptors to
// the waiters still registered. Close wakes each of those on its private
// eventfd; closing that eventfd before the waiter collects the event takes
// the event out of epoll, and the waiter sleeps for good. The shared data
// poller meets this when one waiter starts late and Events closes at once.
func TestLateWaiterLeavesDescriptorsToRegisteredWaiters(t *testing.T) {
	for attempt := 0; attempt < 20; attempt++ {
		p, err := NewNetPoller()
		if err != nil {
			t.Fatal(err)
		}
		returned := make(chan struct{})
		go func() {
			_, _ = p.WaitBatch(&Batch{}, make([]Event, 8), -1)
			close(returned)
		}()
		for registered := 0; registered == 0; {
			p.mu.Lock()
			registered = p.waiters
			p.mu.Unlock()
			time.Sleep(10 * time.Microsecond)
		}
		// Let the registered waiter block in epoll_wait.
		time.Sleep(100 * time.Microsecond)
		_ = p.Close(nil)
		if _, err := p.WaitBatch(&Batch{}, make([]Event, 8), -1); err != nil {
			t.Fatalf("attempt %d: late WaitBatch: %v", attempt, err)
		}
		select {
		case <-returned:
		case <-time.After(2 * time.Second):
			t.Fatalf("attempt %d: the waiter registered before Close never returned", attempt)
		}
	}
}
