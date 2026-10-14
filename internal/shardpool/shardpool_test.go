package shardpool

import (
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/petermattis/goid"
)

type task struct {
	key  uint32
	name string
}

func taskKey(t *task) uint32 { return t.key }

func tasks(count int) []*task {
	all := make([]*task, count)
	for i := range all {
		all[i] = &task{key: uint32(i)}
	}
	return all
}

func waitFor(t *testing.T, what string, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !done() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Microsecond)
	}
}

func receive(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

func (s *shard[T]) liveWorkers() int32 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.live
}

func TestRunsEveryTask(t *testing.T) {
	const total = 20000
	all := tasks(64)
	var ran atomic.Int64
	var wg sync.WaitGroup
	wg.Add(total)
	pool := New(4, 0, taskKey, func(*task) {
		ran.Add(1)
		wg.Done()
	})
	defer pool.Stop()
	var submitters sync.WaitGroup
	for s := range 4 {
		submitters.Add(1)
		go func() {
			defer submitters.Done()
			batch := make([]*task, 0, 8)
			for i := s; i < total; i += 4 {
				batch = append(batch, all[i%len(all)])
				if len(batch) == cap(batch) || i+4 >= total {
					if i%3 == 0 {
						pool.SubmitBatch(batch)
					} else {
						for _, one := range batch {
							pool.Submit(one)
						}
					}
					batch = batch[:0]
				}
			}
		}()
	}
	submitters.Wait()
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("ran %d of %d tasks", ran.Load(), total)
	}
}

// A task that blocks must not hold up the tasks queued behind it on its own
// shard, however many of them block.
func TestBlockedTasksDoNotStallTheirShard(t *testing.T) {
	const blocked = 5
	release := make(chan struct{})
	started := make(chan struct{}, blocked)
	fast := make(chan struct{}, 1)
	all := tasks(blocked + 1)
	pool := New(1, 0, func(*task) uint32 { return 0 }, func(one *task) {
		if int(one.key) < blocked {
			started <- struct{}{}
			<-release
			return
		}
		fast <- struct{}{}
	})
	defer pool.Stop()
	defer close(release)
	for _, one := range all[:blocked] {
		pool.Submit(one)
	}
	for range blocked {
		receive(t, started, "the blocked tasks to start")
	}
	pool.Submit(all[blocked])
	receive(t, fast, "a task queued behind blocked tasks of its shard")
}

// A shard whose workers are all at their maximum is served by another shard's
// worker.
func TestBorrowsFromOtherShards(t *testing.T) {
	ran := make(chan struct{}, 1)
	pool := New(2, 0, taskKey, func(*task) { ran <- struct{}{} })
	defer pool.Stop()
	full := &pool.shards[0]
	full.mu.Lock()
	full.live = MaxWorkers
	full.mu.Unlock()
	pool.Submit(&task{key: 0})
	receive(t, ran, "a full shard's task")
	if live := pool.shards[1].liveWorkers(); live != 1 {
		t.Fatalf("the next shard started %d workers, want 1", live)
	}
}

func TestRetiresIdleWorkersKeepingOne(t *testing.T) {
	pool := New(1, 0, taskKey, func(*task) {})
	s := &pool.shards[0]
	for range 3 {
		s.coming.Add(1)
		if !s.start(s) {
			t.Fatal("start refused")
		}
	}
	waitFor(t, "three idle workers", func() bool { return s.idleCount.Load() == 3 })
	s.retireIdle(pool.clock.Load() + 1)
	s.mu.Lock()
	live, idle := s.live, len(s.idle)
	s.mu.Unlock()
	if live != 1 || idle != 1 || s.idleCount.Load() != 1 {
		t.Fatalf("after retiring: live %d, idle %d, idleCount %d; want 1 each", live, idle, s.idleCount.Load())
	}
	pool.Stop()
	if live := s.liveWorkers(); live != 0 || s.idleCount.Load() != 0 {
		t.Fatalf("after Stop: live %d, idleCount %d; want 0", live, s.idleCount.Load())
	}
	if s.start(s) {
		t.Fatal("a stopped pool started a worker")
	}
}

// Tasks of one key run on the same worker while its shard keeps up.
func TestKeepsAKeyOnOneWorker(t *testing.T) {
	var mu sync.Mutex
	workers := map[int64]bool{}
	done := make(chan struct{}, 1)
	one := &task{key: 3}
	pool := New(4, 0, taskKey, func(*task) {
		mu.Lock()
		workers[goid.Get()] = true
		mu.Unlock()
		done <- struct{}{}
	})
	defer pool.Stop()
	s := pool.shard(one)
	for range 100 {
		pool.Submit(one)
		receive(t, done, "the task")
		// The next task finds the worker parked, as a connection's next
		// request finds it once the reply went out.
		waitFor(t, "the worker to park", func() bool { return s.idleCount.Load() == 1 })
	}
	mu.Lock()
	defer mu.Unlock()
	if len(workers) != 1 {
		t.Fatalf("sequential tasks ran on %d workers, want 1", len(workers))
	}
}

func TestSplitsAndMergesShardsWithTheKeyCount(t *testing.T) {
	pool := New(2, 0, taskKey, func(*task) {})
	defer pool.Stop()
	if got := pool.active.Load(); got != 2 {
		t.Fatalf("active shards = %d, want 2", got)
	}
	split := 2 * SplitKeysPerShard
	for range split - 1 {
		pool.AddKey()
	}
	if got := pool.active.Load(); got != 2 {
		t.Fatalf("active shards below the split = %d, want 2", got)
	}
	pool.AddKey()
	if got := pool.active.Load(); got != 4 {
		t.Fatalf("active shards at the split = %d, want 4", got)
	}
	if s := pool.shard(&task{key: 3}); s.index != 3 {
		t.Fatalf("key 3 went to shard %d, want 3", s.index)
	}
	for range split - 2*MergeKeysPerShard - 1 {
		pool.RemoveKey()
	}
	if got := pool.active.Load(); got != 4 {
		t.Fatalf("active shards above the merge = %d, want 4", got)
	}
	pool.RemoveKey()
	if got := pool.active.Load(); got != 2 {
		t.Fatalf("active shards at the merge = %d, want 2", got)
	}
}

// Stop runs what is queued, including tasks no worker was found for, and
// returns only once every worker has exited.
func TestStopDrainsQueuedTasksAndWaitsForWorkers(t *testing.T) {
	release := make(chan struct{})
	var ran atomic.Int32
	pool := New(1, 0, func(*task) uint32 { return 0 }, func(one *task) {
		if one.key == 0 {
			<-release
		}
		ran.Add(1)
	})
	s := &pool.shards[0]
	pool.Submit(&task{key: 0})
	waitFor(t, "the blocking task to start", func() bool { return s.liveWorkers() == 1 && !s.hasTask() })
	// Starved of workers, the next tasks stay queued for Stop.
	s.mu.Lock()
	s.live = MaxWorkers
	s.mu.Unlock()
	pool.shards[1].mu.Lock()
	pool.shards[1].live = MaxWorkers
	pool.shards[1].mu.Unlock()
	for key := range uint32(3) {
		pool.Submit(&task{key: key + 1})
	}
	stopped := make(chan struct{})
	go func() {
		pool.Stop()
		close(stopped)
	}()
	select {
	case <-stopped:
		t.Fatal("Stop returned while a task was running")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	receive(t, stopped, "Stop")
	if got := ran.Load(); got != 4 {
		t.Fatalf("ran %d tasks by Stop, want 4", got)
	}
}

// A full shard runs the task that found it full on a goroutine of its own.
func TestOverflowRunsTasksAFullShardRefused(t *testing.T) {
	release := make(chan struct{})
	var ran atomic.Int32
	pool := New(1, 0, func(*task) uint32 { return 0 }, func(one *task) {
		if one.key == 0 {
			<-release
		}
		ran.Add(1)
	})
	defer pool.Stop()
	s := &pool.shards[0]
	s.mu.Lock()
	s.live = MaxWorkers - 1 // room for the one worker that blocks
	s.mu.Unlock()
	pool.shards[1].mu.Lock()
	pool.shards[1].live = MaxWorkers
	pool.shards[1].mu.Unlock()
	pool.Submit(&task{key: 0})
	waitFor(t, "the blocking task to start", func() bool { return !s.hasTask() })
	for range DefaultCapacity + 1 {
		pool.Submit(&task{key: 1})
	}
	close(release)
	waitFor(t, "every task", func() bool { return ran.Load() == DefaultCapacity+2 })
}

// A task that ends its goroutine with runtime.Goexit loses the pool no worker.
func TestGoexitInATaskKeepsTheWorker(t *testing.T) {
	done := make(chan struct{}, 1)
	pool := New(1, 0, func(*task) uint32 { return 0 }, func(one *task) {
		if one.key == 0 {
			runtime.Goexit()
		}
		done <- struct{}{}
	})
	defer pool.Stop()
	s := &pool.shards[0]
	pool.Submit(&task{key: 0})
	waitFor(t, "the replacement to park", func() bool { return s.idleCount.Load() == 1 })
	pool.Submit(&task{key: 1})
	receive(t, done, "a task after Goexit")
	if live := s.liveWorkers(); live != 1 {
		t.Fatalf("live workers = %d, want 1", live)
	}
}

// SubmitBehind leaves a task to the worker draining its shard instead of
// waking another.
func TestSubmitBehindWaitsForTheRunningWorker(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	ran := make(chan int64, 2)
	pool := New(1, 0, func(*task) uint32 { return 0 }, func(one *task) {
		if one.key == 0 {
			started <- struct{}{}
			<-release
		}
		ran <- goid.Get()
	})
	pool.tick = time.Hour // the monitor must not step in while the test looks
	defer pool.Stop()
	s := &pool.shards[0]
	if !pool.SubmitBehind(&task{key: 0}) {
		t.Fatal("SubmitBehind to an idle shard woke no worker")
	}
	receive(t, started, "the first task")
	if pool.anyQueued() {
		t.Fatal("anyQueued with nothing queued")
	}
	if pool.SubmitBehind(&task{key: 1}) {
		t.Fatal("SubmitBehind woke a worker while one was draining the shard")
	}
	if !pool.anyQueued() {
		t.Fatal("anyQueued missed the task left behind")
	}
	if live := s.liveWorkers(); live != 1 {
		t.Fatalf("live workers = %d, want 1", live)
	}
	close(release)
	first, second := <-ran, <-ran
	if first != second {
		t.Fatal("the task queued behind ran on another worker")
	}
}

// A task left behind one that blocks is served by another worker once the
// monitor sees the shard's head stuck.
func TestMonitorServesTasksBehindABlockedOne(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	fast := make(chan struct{}, 1)
	pool := New(1, 0, func(*task) uint32 { return 0 }, func(one *task) {
		if one.key == 0 {
			started <- struct{}{}
			<-release
			return
		}
		fast <- struct{}{}
	})
	defer pool.Stop()
	defer close(release)
	pool.SubmitBehind(&task{key: 0})
	receive(t, started, "the blocking task")
	pool.SubmitBehind(&task{key: 1})
	receive(t, fast, "the task queued behind a blocked one")
}

// The monitor stops once nothing has been queued for a while, and starts
// again for the next task left behind a running worker.
func TestMonitorStopsWhenIdleAndRestarts(t *testing.T) {
	release := make(chan struct{}, 2)
	started := make(chan struct{}, 2)
	done := make(chan struct{}, 4)
	pool := New(1, 0, func(*task) uint32 { return 0 }, func(one *task) {
		if one.key == 0 {
			started <- struct{}{}
			<-release
		}
		done <- struct{}{}
	})
	defer pool.Stop()
	for range 2 {
		pool.SubmitBehind(&task{key: 0})
		receive(t, started, "the blocking task")
		pool.SubmitBehind(&task{key: 1})
		if !pool.watching.Load() {
			t.Fatal("no monitor for a task left behind a running worker")
		}
		receive(t, done, "the task behind")
		release <- struct{}{}
		receive(t, done, "the blocking task to end")
		waitFor(t, "the monitor to stop", func() bool { return !pool.watching.Load() })
	}
}

func TestNewRoundsTheCapacityUp(t *testing.T) {
	for _, tc := range []struct{ capacity, want int }{{0, DefaultCapacity}, {-1, DefaultCapacity}, {1, 2}, {3, 4}, {1000, 1024}, {1024, 1024}} {
		pool := New(1, tc.capacity, taskKey, func(*task) {})
		if got := len(pool.shards[0].slots); got != tc.want || pool.shards[0].mask != uint64(tc.want-1) {
			t.Errorf("New(capacity %d): %d slots, mask %d; want %d", tc.capacity, got, pool.shards[0].mask, tc.want)
		}
		pool.Stop()
	}
}

// With every worker of the pool busy and no room for another, a task waits
// with the pool marked starving, and the next worker to go idle, of whichever
// shard, serves it.
func TestStarvedTaskIsServedByTheNextIdleWorker(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	served := make(chan int64, 1)
	var blocker atomic.Int64
	pool := New(2, 0, taskKey, func(one *task) {
		if one.key == 0 {
			blocker.Store(goid.Get())
			started <- struct{}{}
			<-release
			return
		}
		served <- goid.Get()
	})
	defer pool.Stop()
	pool.Submit(&task{key: 0}) // shard 0's only worker blocks in it
	receive(t, started, "the blocking task")
	// No shard has room for another worker, and no worker is idle.
	for i := range pool.shards {
		s := &pool.shards[i]
		s.mu.Lock()
		s.live = MaxWorkers
		s.mu.Unlock()
	}
	if pool.Submit(&task{key: 1}) {
		t.Fatal("Submit found a worker in a pool that has none to give")
	}
	if !pool.starving.Load() {
		t.Fatal("the pool is not marked starving")
	}
	select {
	case <-served:
		t.Fatal("the starved task ran with every worker busy")
	case <-time.After(20 * time.Millisecond):
	}
	// The blocked worker finishes, finds its own shard empty and goes idle:
	// that is when it must take the starved shard's task.
	close(release)
	select {
	case worker := <-served:
		if worker != blocker.Load() {
			t.Fatal("the starved task ran on a worker other than the one that went idle")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the starved task did not run once a worker went idle")
	}
	if pool.starving.Load() {
		t.Fatal("the pool is still marked starving")
	}
}

// Workers a burst brought in exit once they have been idle for idleAfter
// janitor ticks, one per shard staying, and the one left still serves.
func TestIdleWorkersRetireOverTime(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{}, 3)
	done := make(chan struct{}, 4)
	pool := New(1, 0, func(*task) uint32 { return 0 }, func(one *task) {
		if one.key < 3 {
			started <- struct{}{}
			<-release
		}
		done <- struct{}{}
	})
	pool.janitorEvery, pool.idleAfter = time.Millisecond, 3
	defer pool.Stop()
	s := &pool.shards[0]
	for key := range uint32(3) {
		pool.Submit(&task{key: key})
		receive(t, started, "a blocking task") // each brings in a worker
	}
	if live := s.liveWorkers(); live != 3 {
		t.Fatalf("live workers = %d, want 3", live)
	}
	close(release)
	for range 3 {
		receive(t, done, "a released task")
	}
	waitFor(t, "idle workers to retire", func() bool { return s.liveWorkers() == 1 })
	if got := s.idleCount.Load(); got != 1 {
		t.Fatalf("idle workers = %d, want 1", got)
	}
	pool.Submit(&task{key: 3})
	receive(t, done, "a task after the burst")
}

// A host with more than 32 Ps, once its keys are many, has more than 64
// active shards, past what SubmitBatch's bitmap tracks: it submits one by
// one instead, and every task still runs.
func TestSubmitBatchOverMoreThan64Shards(t *testing.T) {
	const procs, total = 40, 400
	var ran atomic.Int32
	pool := New(procs, 16, taskKey, func(*task) { ran.Add(1) })
	defer pool.Stop()
	for range procs * SplitKeysPerShard {
		pool.AddKey()
	}
	if got := pool.active.Load(); got != 2*procs {
		t.Fatalf("active shards = %d, want %d", got, 2*procs)
	}
	batch := tasks(total)
	pool.SubmitBatch(batch)
	waitFor(t, "every task", func() bool { return ran.Load() == total })
	for i := range pool.shards[:2*procs] {
		if pool.shards[i].head.Load() == 0 {
			t.Fatalf("shard %d ran nothing", i)
		}
	}
}

// Tasks that find their shard's queue full, whichever way they were
// submitted, run on goroutines of their own, each exactly once.
func TestEverySubmitOverflowsAFullShard(t *testing.T) {
	const total = 30
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	var counts [total + 1]atomic.Int32
	pool := New(1, 2, func(*task) uint32 { return 0 }, func(one *task) {
		if one.key == total {
			started <- struct{}{}
			<-release
		}
		counts[one.key].Add(1)
	})
	pool.tick = time.Hour
	defer pool.Stop()
	// The shard's only worker blocks, and no other can be started or
	// borrowed, so the queue of two fills.
	pool.Submit(&task{key: total})
	receive(t, started, "the blocking task")
	for i := range pool.shards {
		s := &pool.shards[i]
		s.mu.Lock()
		s.live = MaxWorkers
		s.mu.Unlock()
	}
	all := tasks(total)
	pool.SubmitBatch(all[:10])
	for _, one := range all[10:20] {
		pool.SubmitBehind(one)
	}
	for _, one := range all[20:] {
		pool.Submit(one)
	}
	close(release)
	waitFor(t, "every task", func() bool {
		for i := range counts {
			if counts[i].Load() == 0 {
				return false
			}
		}
		return true
	})
	for i := range counts {
		if got := counts[i].Load(); got != 1 {
			t.Fatalf("task %d ran %d times", i, got)
		}
	}
}

// Tasks queued on a shard no worker can come to are run by Stop itself, and a
// second Stop returns at once.
func TestStopRunsTasksNoWorkerCouldTake(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	var ran atomic.Int32
	pool := New(2, 0, taskKey, func(one *task) {
		if one.key == 0 {
			started <- struct{}{}
			<-release
			return
		}
		ran.Add(1)
	})
	pool.Submit(&task{key: 0}) // shard 0's worker blocks
	receive(t, started, "the blocking task")
	for i := range pool.shards {
		s := &pool.shards[i]
		s.mu.Lock()
		s.live = MaxWorkers
		s.mu.Unlock()
	}
	pool.Submit(&task{key: 1}) // shard 1: no worker anywhere, the pool starves
	pool.Submit(&task{key: 1})
	stopped := make(chan struct{})
	go func() {
		pool.Stop()
		close(stopped)
	}()
	select {
	case <-stopped:
		t.Fatal("Stop returned while a task ran")
	case <-time.After(20 * time.Millisecond):
	}
	// Released into a stopped pool, the worker exits without serving the
	// starved shard, which is left to Stop.
	close(release)
	receive(t, stopped, "Stop")
	if got := ran.Load(); got != 2 {
		t.Fatalf("Stop left %d of 2 starved tasks unrun", 2-got)
	}
	again := make(chan struct{})
	go func() {
		pool.Stop()
		close(again)
	}()
	receive(t, again, "a second Stop")
}

// Tasks still queued on a shard when the keys drop and the pool merges its
// shards are not lost: the shard's worker still serves them, while new tasks
// of the same key go to the merged shard. The pool orders nothing across
// shards, so the new task runs first.
func TestTasksQueuedBeforeAMergeStillRun(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	ran := make(chan string, 3)
	pool := New(1, 0, taskKey, func(one *task) {
		if one.name == "blocker" {
			started <- struct{}{}
			<-release
		}
		ran <- one.name
	})
	pool.tick = time.Hour // only the blocked worker may serve what waits behind it
	defer pool.Stop()
	for range SplitKeysPerShard {
		pool.AddKey()
	}
	if got := pool.active.Load(); got != 2 {
		t.Fatalf("active shards = %d, want 2", got)
	}
	pool.Submit(&task{key: 1, name: "blocker"}) // shard 1's worker blocks
	receive(t, started, "the blocking task")
	pool.SubmitBehind(&task{key: 1, name: "behind"}) // left to that worker
	for range SplitKeysPerShard - MergeKeysPerShard {
		pool.RemoveKey()
	}
	if got := pool.active.Load(); got != 1 {
		t.Fatalf("active shards = %d, want 1", got)
	}
	pool.Submit(&task{key: 1, name: "after"}) // the same key, now on shard 0
	if got := <-ran; got != "after" {
		t.Fatalf("first to run: %s, want the task submitted after the merge", got)
	}
	close(release)
	got := map[string]bool{<-ran: true, <-ran: true}
	if !got["blocker"] || !got["behind"] {
		t.Fatalf("after the release ran %v, want the blocker and the task behind it", got)
	}
}

// Many submitters, every way of submitting, small queues and tasks that now
// and then block: every task runs exactly once, and Stop returns once they
// all have. Run with the race detector to check the queue's ordering too.
func TestConcurrentSubmittersRunEveryTaskOnce(t *testing.T) {
	const submitters, perSubmitter = 16, 3000
	const total = submitters * perSubmitter
	counts := make([]atomic.Int32, total)
	var ran atomic.Int32
	pool := New(4, 8, taskKey, func(one *task) {
		if one.key%97 == 0 {
			time.Sleep(200 * time.Microsecond) // a callback that blocks a while
		}
		counts[one.key].Add(1)
		ran.Add(1)
	})
	pool.tick = 200 * time.Microsecond
	for range 4 * SplitKeysPerShard / 2 {
		pool.AddKey()
	}
	var wg sync.WaitGroup
	for s := range submitters {
		wg.Add(1)
		go func() {
			defer wg.Done()
			batch := make([]*task, 0, 5)
			for i := range perSubmitter {
				one := &task{key: uint32(s*perSubmitter + i)}
				switch i % 3 {
				case 0:
					pool.Submit(one)
				case 1:
					pool.SubmitBehind(one)
				default:
					if batch = append(batch, one); len(batch) == cap(batch) {
						pool.SubmitBatch(batch)
						batch = batch[:0]
					}
				}
				if i%500 == 0 {
					// Keys come and go meanwhile, moving the active count.
					pool.AddKey()
					pool.RemoveKey()
				}
			}
			pool.SubmitBatch(batch)
		}()
	}
	wg.Wait()
	waitDeadline := time.Now().Add(10 * time.Second)
	for ran.Load() != total && time.Now().Before(waitDeadline) {
		time.Sleep(time.Millisecond)
	}
	pool.Stop()
	for i := range counts {
		if got := counts[i].Load(); got != 1 {
			t.Fatalf("task %d ran %d times (%d of %d ran)", i, got, ran.Load(), total)
		}
	}
}

// A borrower that read an idle count from before another took the shard's
// last idle worker finds the list empty, and looks elsewhere.
func TestWakeIdleWithAStaleCountFindsNone(t *testing.T) {
	pool := New(1, 0, taskKey, func(*task) {})
	defer pool.Stop()
	s := &pool.shards[0]
	s.idleCount.Store(1) // as read just before another took the worker
	if s.wakeIdle(s) {
		t.Fatal("wakeIdle sent a worker it did not have")
	}
	s.idleCount.Store(0)
}

// The pool doubles its shards a second time once the keys are many enough,
// and goes back through the matching merge threshold.
func TestSecondDoublingAndItsMerge(t *testing.T) {
	pool := New(2, 0, taskKey, func(*task) {})
	defer pool.Stop()
	for range 2 * SplitKeysPerShard4 {
		pool.AddKey()
	}
	if got := pool.active.Load(); got != 8 {
		t.Fatalf("active shards at the second split = %d, want 8", got)
	}
	for range 2*SplitKeysPerShard4 - 2*MergeKeysPerShard4 - 1 {
		pool.RemoveKey()
	}
	if got := pool.active.Load(); got != 8 {
		t.Fatalf("active shards above the second merge = %d, want 8", got)
	}
	pool.RemoveKey()
	if got := pool.active.Load(); got != 4 {
		t.Fatalf("active shards at the second merge = %d, want 4", got)
	}
	// Tasks of every key still run whichever tier is active.
	var ran atomic.Int32
	runner := New(2, 0, taskKey, func(*task) { ran.Add(1) })
	defer runner.Stop()
	for range 2 * SplitKeysPerShard4 {
		runner.AddKey()
	}
	batch := tasks(200)
	runner.SubmitBatch(batch)
	waitFor(t, "every task", func() bool { return ran.Load() == 200 })
}
