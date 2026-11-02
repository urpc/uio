package shardpool

import (
	"testing"
	"time"
)

// A shard whose woken worker never arrives gets another from Rescue, but only
// once its queue has held a task without moving through rescueStalls scans.
func TestRescueBringsAWorkerToAStalledShard(t *testing.T) {
	ran := make(chan *task, 2)
	pool := New(1, 16, func(*task) uint32 { return 0 }, func(one *task) { ran <- one })
	defer pool.Stop()
	s := &pool.shards[0]
	ms := int64(time.Millisecond)
	// A worker was woken for the shard and is held up elsewhere: submitters
	// see it coming and bring nobody else.
	s.coming.Store(1)
	defer s.coming.Add(-1)
	s.rescueHead = 99
	first, second := &task{key: 1}, &task{key: 2}
	if !s.push(first) || !s.push(second) {
		t.Fatal("push refused")
	}
	if pool.Rescue(ms) {
		t.Fatal("the first scan rescued a queue it had not seen before")
	}
	if pool.Rescue(ms + ms/2) {
		t.Fatal("a scan ran before the interval was up")
	}
	if got, ok := s.pop(); !ok || got != first {
		t.Fatal("pop failed")
	}
	if pool.Rescue(2 * ms) {
		t.Fatal("a queue that moved since the last scan was rescued")
	}
	for scan := 1; scan < rescueStalls; scan++ {
		if pool.Rescue(int64(2+scan) * ms) {
			t.Fatalf("rescued after %d stalled scans, want %d", scan, rescueStalls)
		}
	}
	if !pool.Rescue(int64(2+rescueStalls) * ms) {
		t.Fatal("a queue stalled through rescueStalls scans was not rescued")
	}
	waitTask(t, ran, second)
}

// One scan brings one worker: the stalled shards it leaves are taken by the
// scans after it.
func TestRescueBringsOneWorkerPerScan(t *testing.T) {
	ran := make(chan *task, 2)
	pool := New(2, 16, func(one *task) uint32 { return uint32(one.key) }, func(one *task) { ran <- one })
	defer pool.Stop()
	ms := int64(time.Millisecond)
	tasks := []*task{{key: 0}, {key: 1}}
	for i, one := range tasks {
		s := &pool.shards[i]
		s.coming.Store(1)
		defer s.coming.Add(-1)
		s.rescueHead = 99
		if !s.push(one) {
			t.Fatal("push refused")
		}
	}
	// The first scan records the queues; each later one counts a stall.
	scan := int64(0)
	for range rescueStalls {
		scan++
		if pool.Rescue(scan * ms) {
			t.Fatalf("rescued at scan %d, before rescueStalls stalled scans", scan)
		}
	}
	woke := 0
	for range 2 {
		scan++
		if pool.Rescue(scan * ms) {
			woke++
			select {
			case <-ran:
			case <-time.After(5 * time.Second):
				t.Fatal("a rescued task never ran")
			}
		}
		select {
		case <-ran:
			t.Fatal("one scan brought workers to two shards")
		default:
		}
	}
	if woke != 2 {
		t.Fatalf("%d scans brought a worker, want 2", woke)
	}
}

func waitTask(t *testing.T, ran <-chan *task, want *task) {
	t.Helper()
	select {
	case got := <-ran:
		if got != want {
			t.Fatalf("ran task %d, want %d", got.key, want.key)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the stalled task never ran")
	}
}

// A rescue that finds no worker anywhere leaves no task behind. While its
// raised count stood, the worker that was coming arrived, drained and parked,
// and a submitter queued another task, each skipping its notify because a
// worker seemed to be coming; the failed rescue takes their part, so the
// parked worker comes back for the task, and the rescue reports it.
func TestRescueThatFindsNoWorkerLeavesNoTaskBehind(t *testing.T) {
	ran := make(chan *task, 4)
	pool := New(1, 16, func(*task) uint32 { return 0 }, func(one *task) { ran <- one })
	// Every shard has its maximum of workers and none is idle, so neither
	// waking nor starting nor borrowing one succeeds. No goroutines stand
	// behind these counts.
	for i := range pool.shards {
		s := &pool.shards[i]
		s.mu.Lock()
		s.live = MaxWorkers
		s.mu.Unlock()
	}
	ms := int64(time.Millisecond)
	s := &pool.shards[0]
	s.coming.Store(1) // the worker woken for the shard, held up elsewhere
	first, second := &task{key: 1}, &task{key: 2}
	s.push(first)
	for scan := int64(1); scan < rescueStalls; scan++ {
		pool.Rescue(scan * ms)
	}
	// Hold the rescue inside its borrow, after it raised the count.
	other := &pool.shards[1]
	other.mu.Lock()
	result := make(chan bool, 1)
	go func() { result <- pool.Rescue(rescueStalls * ms) }()
	for deadline := time.Now().Add(5 * time.Second); s.coming.Load() != 2; {
		if time.Now().After(deadline) {
			other.mu.Unlock()
			t.Fatal("the rescue never raised the count")
		}
		time.Sleep(100 * time.Microsecond)
	}
	// The coming worker arrives and drains the shard, as worker.run does.
	s.running.Add(1)
	s.coming.Add(-1)
	for one, ok := s.pop(); ok; one, ok = s.pop() {
		pool.run(one)
	}
	// A submitter queues the second task; the count says a worker is coming.
	s.push(second)
	if s.notify() {
		other.mu.Unlock()
		t.Fatal("a submitter woke a worker while one was counted as coming")
	}
	// The worker finishes and parks in its shard, as worker.park does.
	s.running.Add(-1)
	parked := &worker[*task]{home: s, wake: make(chan *shard[*task], 1)}
	s.mu.Lock()
	s.idle = append(s.idle, parked)
	s.idleCount.Add(1)
	s.mu.Unlock()
	s.notify()
	go func() {
		if target := <-parked.wake; target != nil {
			target.running.Add(1)
			target.coming.Add(-1)
			for one, ok := target.pop(); ok; one, ok = target.pop() {
				pool.run(one)
			}
			target.running.Add(-1)
		}
	}()
	other.mu.Unlock()
	if !<-result {
		t.Fatal("the rescue brought the parked worker back but did not report it")
	}
	waitTask(t, ran, first)
	waitTask(t, ran, second)
}
