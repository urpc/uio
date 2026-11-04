package readcontrol

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
)

func TestRejectedPauseDoesNotChangeState(t *testing.T) {
	closed := errors.New("boundary closed")
	c := New(func() error { return closed }, func() error {
		t.Fatal("a rejected pause scheduled a resume")
		return nil
	})
	p := c.NewPause()
	for _, operation := range []func() error{c.Check, p.Pause, c.PauseReads} {
		if err := operation(); !errors.Is(err, closed) {
			t.Fatalf("rejected operation = %v, want lifecycle error", err)
		}
	}
	// Releasing what was never held is not a lifecycle question.
	for _, operation := range []func() error{p.Resume, c.ResumeReads} {
		if err := operation(); err != nil {
			t.Fatalf("idle resume = %v", err)
		}
	}
	if c.ReadsPaused() || p.Paused() {
		t.Fatal("rejected operations changed boundary state")
	}
	var unavailable *Control
	for _, operation := range []func() error{unavailable.Check, unavailable.PauseReads, unavailable.ResumeReads} {
		if err := operation(); !errors.Is(err, ErrUnsupported) {
			t.Fatalf("unavailable boundary = %v", err)
		}
	}
}

func TestIdleResumeSchedulesNothing(t *testing.T) {
	var resumes atomic.Int64
	c := New(nil, func() error { resumes.Add(1); return nil })
	for range 10 {
		if err := c.ResumeReads(); err != nil {
			t.Fatal(err)
		}
		if err := c.NewPause().Resume(); err != nil {
			t.Fatal(err)
		}
	}
	if resumes.Load() != 0 || c.ReadsPaused() {
		t.Fatalf("idle resumes scheduled %d turns", resumes.Load())
	}
}

// A pause made after the release scheduled its turn is not undone by it:
// the turn finds the boundary paused again, and the next release schedules
// another.
func TestPauseAfterReleaseSchedulesAgain(t *testing.T) {
	var resumes atomic.Int64
	c := New(nil, func() error { resumes.Add(1); return nil })
	for i := 1; i <= 3; i++ {
		if err := c.PauseReads(); err != nil {
			t.Fatal(err)
		}
		if !c.ReadsPaused() {
			t.Fatal("pause not visible")
		}
		if err := c.ResumeReads(); err != nil {
			t.Fatal(err)
		}
		if c.ReadsPaused() || resumes.Load() != int64(i) {
			t.Fatalf("release %d: paused=%v resumes=%d", i, c.ReadsPaused(), resumes.Load())
		}
	}
}

func TestResumeHookErrorIsReported(t *testing.T) {
	gone := errors.New("gone")
	c := New(nil, func() error { return gone })
	if err := c.PauseReads(); err != nil {
		t.Fatal(err)
	}
	if err := c.ResumeReads(); !errors.Is(err, gone) {
		t.Fatalf("resume = %v, want the hook's error", err)
	}
	if c.ReadsPaused() {
		t.Fatal("a failed hook kept the pause")
	}
}

func TestConcurrentDefaultPauseHasOneOwnerAndOneResume(t *testing.T) {
	var resumes atomic.Int64
	c := New(nil, func() error { resumes.Add(1); return nil })
	const workers = 64
	var ready, done sync.WaitGroup
	ready.Add(workers)
	done.Add(workers)
	start := make(chan struct{})
	for range workers {
		go func() {
			defer done.Done()
			ready.Done()
			<-start
			if err := c.PauseReads(); err != nil {
				t.Error(err)
			}
		}()
	}
	ready.Wait()
	close(start)
	done.Wait()
	if !c.ReadsPaused() {
		t.Fatal("concurrent default pauses failed to hold input")
	}
	for range workers {
		done.Add(1)
		go func() { defer done.Done(); _ = c.ResumeReads() }()
	}
	done.Wait()
	if c.ReadsPaused() || resumes.Load() != 1 {
		t.Fatalf("default slot was not idempotent: paused=%v resumes=%d", c.ReadsPaused(), resumes.Load())
	}
}

// Owners racing pause and release on one boundary never leave it paused by
// nobody, and every final release asks for exactly one turn.
func TestConcurrentOwnersBalance(t *testing.T) {
	var resumes atomic.Int64
	c := New(nil, func() error { resumes.Add(1); return nil })
	const owners = 8
	var done sync.WaitGroup
	for range owners {
		p := c.NewPause()
		done.Add(1)
		go func() {
			defer done.Done()
			for range 1000 {
				_ = p.Pause()
				_ = p.Resume()
			}
		}()
	}
	done.Wait()
	if c.ReadsPaused() || c.owners.Load() != 0 {
		t.Fatalf("owners left = %d", c.owners.Load())
	}
	if resumes.Load() == 0 {
		t.Fatal("no final release scheduled a turn")
	}
}

// The end of the stream waits for a pause and for the turn its release
// owes; the consumer running clears what is owed, never a pause still held.
func TestHeldCoversPauseAndOwedResumeTurn(t *testing.T) {
	var unavailable *Control
	unavailable.ConsumerRuns()
	if unavailable.Held() {
		t.Fatal("a missing control held the end")
	}
	c := New(nil, func() error { return nil })
	c.ConsumerRuns() // nothing owed: nothing changes
	if c.Held() {
		t.Fatal("an unused control held the end")
	}
	first, second := c.NewPause(), c.NewPause()
	_ = first.Pause()
	_ = second.Pause()
	if !c.Held() {
		t.Fatal("a pause did not hold the end")
	}
	_ = first.Resume()
	c.ConsumerRuns()
	if !c.Held() {
		t.Fatal("the consumer running cleared another owner's pause")
	}
	_ = second.Resume()
	if !c.Held() || c.ReadsPaused() {
		t.Fatal("the last release owed no resume turn")
	}
	c.ConsumerRuns()
	if c.Held() {
		t.Fatal("the consumer's turn did not clear what the release owed")
	}
}
