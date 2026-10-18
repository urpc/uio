/*
 * Copyright 2024 the urpc project
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *      https://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

// Package shardpool runs tasks on a pool of workers grouped in shards, with
// every task queued on the shard its key picks. A shard's queue is served by
// one worker while that worker keeps up with it, and a busy goroutine stays on
// its P, so the tasks of one key keep running on the same CPU. That is the
// point of the pool: when a key is a connection, the kernel then finds the
// connection's socket and timers in that CPU's caches. Spread over every CPU,
// as a single shared queue spreads them, a loopback echo's turns ran on
// another CPU than the connection's previous turn 96% of the time (12% here,
// with 512 connections on 24 Ps, and 27% with 4096), its delayed-ACK and
// retransmit timers migrated between timer bases on every send, and the
// kernel's share of each request, which is most of its cost, measured 3%
// higher.
//
// A shard takes more workers only for a backlog: a worker that takes a task
// and finds another queued behind it first brings in a second worker, so the
// backlog never waits behind a task that runs long or blocks, and a task that
// blocks costs its shard one worker. When a shard has no idle worker and no
// room for another, an idle worker of another shard serves it, or a new
// worker starts in a shard with room; when the whole pool has neither, the
// next worker to go idle serves every shard once. Workers stay parked while
// they are needed and exit after MaxIdle without work, one per shard staying.
//
// SubmitBehind queues a task without bringing in a worker when one is
// draining its shard already: that worker takes it once its current task
// ends. It suits tasks submitted one at a time from outside the pool, which
// would otherwise wake a second worker for nearly every task: write turns
// submitted by 24 goroutines replying to echo requests ran 3-5% slower that
// way than on a shared queue whose workers rarely park, and as fast or faster
// with SubmitBehind. A monitor that runs while such tasks are queued brings in
// a worker for a shard whose head has not moved for watchTick, so a task that
// blocks holds up the tasks behind it for about that long.
//
// The pool keeps one shard per P until the caller reports, through AddKey,
// SplitKeysPerShard keys per shard, and doubles its shards there and again at
// SplitKeysPerShard4, until the count drops back through the matching merge
// thresholds. A shard serving many keys keeps a backlog, and every worker the
// backlog brings in takes its next task from whichever key is next, so the
// more keys a shard has the more of their tasks change workers: with 4096
// echoing connections on 24 Ps a task ran on the worker of its key's previous
// task 71% of the time with a shard per P and 83% with two, and throughput
// rose 2%. Longer queues are also a longer wait for everyone in them, which
// is why the doubling goes on: with 10000 echoing connections the second
// doubling halved the median round trip and raised throughput 2%. With 512
// the extra shards only park and wake their workers more often, which cost
// 3%, and the first doubling waits for 64 keys per base shard. A key whose
// shard changes when the count does is the caller's to keep consistent: the
// pool orders nothing across shards.
package shardpool

import (
	"math/bits"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// DefaultCapacity is the queue capacity of a shard New is given none
	// for. A task that finds its shard full gets a goroutine of its own,
	// which then helps drain the shard.
	DefaultCapacity = 4096
	// MaxWorkers bounds a shard's workers, and so the tasks of one shard
	// that can block at once without holding up its others.
	MaxWorkers = 512
	// MaxIdle is how long a parked worker beyond the first of its shard
	// waits for work before it exits.
	MaxIdle = 30 * time.Second
	// The keys per base shard at which the pool doubles its active shards,
	// and the counts it goes back at. SplitKeysPerShard and MergeKeysPerShard
	// are the first doubling, SplitKeysPerShard4 and MergeKeysPerShard4 the
	// second; the gap between a split and its merge keeps a count that hovers
	// around one from switching back and forth. More shards mean shorter
	// queues per shard and more of them drained at once, which is what a
	// connection's wait divides over: with 10000 echoing connections on 24 Ps
	// (417 per base shard), the second doubling cut the median round trip from
	// 3.1ms to 1.6ms and raised throughput 2%; at 6000 connections, and on
	// four and eight Ps at the same connections per shard, it was a wash,
	// which is why it waits for 256. Every tier's queues are allocated up
	// front, 16 KiB per shard.
	SplitKeysPerShard  = 64
	MergeKeysPerShard  = 32
	SplitKeysPerShard4 = 256
	MergeKeysPerShard4 = 192

	cacheLineSize = 64
	janitorTick   = time.Second
	// watchTick is how long a task may hold up the tasks queued behind it on
	// its shard before another worker is brought in for them.
	watchTick      = time.Millisecond
	watchIdleTicks = 50
	idleTicks      = int64(MaxIdle / janitorTick)
)

// Pool runs tasks of type T. Create it with New.
type Pool[T any] struct {
	shards []shard[T]
	key    func(T) uint32
	run    func(T)
	// active is the number of shards keys are spread over: base, or every
	// shard once the keys are many (see AddKey).
	active atomic.Int32
	base   int32
	keys   atomic.Int64
	_      [cacheLineSize]byte
	// starving is set when a shard with queued tasks found no worker
	// anywhere; the next worker to park clears it and notifies every shard.
	starving atomic.Bool
	_        [cacheLineSize - 4]byte
	stopped  atomic.Bool
	clock    atomic.Int64 // janitor ticks, read when a worker parks
	done     chan struct{}
	janitor  sync.Once      // started with the first shard that grows past one worker
	workers  sync.WaitGroup // every started worker until it exits
	// watching is set while the monitor runs (see watch), which ticks every
	// tick: watchTick, or longer in tests.
	watching atomic.Bool
	tick     time.Duration
	// janitorEvery and idleAfter pace idle-worker retirement: janitorTick
	// and MaxIdle in janitor ticks, or shorter in tests.
	janitorEvery time.Duration
	idleAfter    int64
}

type shard[T any] struct {
	owner *Pool[T]
	index int
	slots []slot[T]
	mask  uint64
	_     [cacheLineSize]byte
	tail  atomic.Uint64 // the position the next task is written to
	_     [cacheLineSize - 8]byte
	head  atomic.Uint64 // the position the next task is taken from
	_     [cacheLineSize - 8]byte
	// coming counts workers woken or started for this shard that have not
	// reached its queue yet: they will take what is queued, so a submitter
	// that finds one need not bring another.
	coming    atomic.Int32
	running   atomic.Int32 // workers draining this shard's queue now
	idleCount atomic.Int32 // len(idle), for borrowers that look without mu
	mu        sync.Mutex
	idle      []*worker[T] // parked workers, the most recently parked last
	live      int32        // started workers not yet exited; guarded by mu
	_         [cacheLineSize]byte
}

// slot is one cell of a shard's queue: seq equal to the position means it can
// be written, the position plus one that it holds a task, and the position
// plus the capacity that it is free for the next lap.
type slot[T any] struct {
	seq  atomic.Uint64
	task T
}

type worker[T any] struct {
	home   *shard[T]
	wake   chan *shard[T] // the shard to serve next; nil to exit
	parked int64          // janitor clock when it last parked; guarded by home.mu
}

// New creates a pool for procs Ps: procs shards, and as many more for when
// the keys are many, each queueing up to capacity tasks, rounded up to a
// power of two; DefaultCapacity when capacity is not positive. Every shard's
// queue is allocated up front. key picks a task's shard, run runs it. A panic
// in run is not recovered.
func New[T any](procs, capacity int, key func(T) uint32, run func(T)) *Pool[T] {
	base := max(procs, 1)
	if capacity <= 0 {
		capacity = DefaultCapacity
	}
	capacity = 1 << bits.Len(uint(max(capacity, 2)-1))
	pool := &Pool[T]{
		shards: make([]shard[T], 4*base),
		key:    key,
		run:    run,
		base:   int32(base),
		done:   make(chan struct{}),
		tick:   watchTick,

		janitorEvery: janitorTick,
		idleAfter:    idleTicks,
	}
	pool.active.Store(int32(base))
	for i := range pool.shards {
		s := &pool.shards[i]
		s.owner = pool
		s.index = i
		s.slots = make([]slot[T], capacity)
		for j := range s.slots {
			s.slots[j].seq.Store(uint64(j))
		}
		s.mask = uint64(capacity - 1)
	}
	return pool
}

// AddKey and RemoveKey count the keys the pool serves, such as open
// connections, and switch the number of active shards when the count reaches
// a threshold.
func (pool *Pool[T]) AddKey() {
	switch pool.keys.Add(1) {
	case int64(pool.base) * SplitKeysPerShard:
		pool.active.Store(2 * pool.base)
	case int64(pool.base) * SplitKeysPerShard4:
		pool.active.Store(int32(len(pool.shards)))
	}
}

func (pool *Pool[T]) RemoveKey() {
	switch pool.keys.Add(-1) {
	case int64(pool.base) * MergeKeysPerShard4:
		pool.active.Store(2 * pool.base)
	case int64(pool.base) * MergeKeysPerShard:
		pool.active.Store(pool.base)
	}
}

func (pool *Pool[T]) shard(task T) *shard[T] {
	return &pool.shards[pool.key(task)%uint32(pool.active.Load())]
}

// Submit queues one task on its shard. It never blocks and never runs the
// task on the caller's goroutine. It reports whether it woke or started a
// worker for the task.
//
// The runtime queues a goroutine woken by another on the waker's P, where it
// waits until the waker blocks, yields or is preempted, up to 10ms later. A
// worker submitting from a task returns to its queue soon after, but a
// goroutine that goes on with other work should yield when Submit reports a
// wakeup, so the worker runs now: on a host whose Ps were all busy, write
// turns woken by 24 replying goroutines waited 3.6ms on average before
// running.
func (pool *Pool[T]) Submit(task T) bool {
	s := pool.shard(task)
	if !s.push(task) {
		go s.overflow(task)
		return false
	}
	return s.notify()
}

// SubmitBehind is Submit for a task that may wait for the one its shard is
// running: when a worker drains the shard, it is left to that worker, and
// SubmitBehind reports false. See the package comment.
func (pool *Pool[T]) SubmitBehind(task T) bool {
	s := pool.shard(task)
	if !s.push(task) {
		go s.overflow(task)
		return false
	}
	return s.notifyBehind()
}

// notifyBehind is notify for a task submitted from outside the shard's
// workers: a worker draining the shard takes it once its current task ends,
// so none is woken for it. The monitor wakes one if that task does not end
// (see watch).
func (s *shard[T]) notifyBehind() bool {
	if s.running.Load() > 0 {
		s.owner.watch()
		return false
	}
	return s.notify()
}

// watch starts the monitor unless it runs.
func (pool *Pool[T]) watch() {
	if !pool.watching.Load() && pool.watching.CompareAndSwap(false, true) {
		go pool.monitor()
	}
}

// monitor brings a worker to every shard whose queue held tasks through a
// whole tick without its head moving: the worker draining it is held in a
// task that runs long or blocks. It stops after watchIdleTicks ticks that
// found no task queued.
func (pool *Pool[T]) monitor() {
	ticker := time.NewTicker(pool.tick)
	defer ticker.Stop()
	heads := make([]uint64, len(pool.shards))
	for i := range pool.shards {
		heads[i] = pool.shards[i].head.Load()
	}
	idle := 0
	for {
		select {
		case <-pool.done:
			pool.watching.Store(false)
			return
		case <-ticker.C:
		}
		queued := false
		for i := range pool.shards {
			s := &pool.shards[i]
			head := s.head.Load()
			if s.hasTask() {
				queued = true
				if head == heads[i] {
					s.notify()
				}
			}
			heads[i] = head
		}
		if queued {
			idle = 0
			continue
		}
		if idle++; idle < watchIdleTicks {
			continue
		}
		pool.watching.Store(false)
		// A submitter that skipped a wakeup after the last tick found the flag
		// still set and started nothing: look once more.
		if !pool.anyQueued() || !pool.watching.CompareAndSwap(false, true) {
			return
		}
		idle = 0
	}
}

func (pool *Pool[T]) anyQueued() bool {
	for i := range pool.shards {
		if pool.shards[i].hasTask() {
			return true
		}
	}
	return false
}

// SubmitBatch queues tasks and notifies each shard they touched once. It does
// not retain the slice.
func (pool *Pool[T]) SubmitBatch(tasks []T) {
	active := uint32(pool.active.Load())
	if active > 64 {
		for _, task := range tasks {
			pool.Submit(task)
		}
		return
	}
	var touched uint64
	for _, task := range tasks {
		index := pool.key(task) % active
		if !pool.shards[index].push(task) {
			go pool.shards[index].overflow(task)
			continue
		}
		touched |= 1 << index
	}
	for touched != 0 {
		index := bits.TrailingZeros64(touched)
		touched &= touched - 1
		pool.shards[index].notify()
	}
}

func (s *shard[T]) push(task T) bool {
	pos := s.tail.Load()
	for {
		cell := &s.slots[pos&s.mask]
		switch seq := cell.seq.Load(); {
		case seq == pos:
			if s.tail.CompareAndSwap(pos, pos+1) {
				cell.task = task
				cell.seq.Store(pos + 1)
				return true
			}
			pos = s.tail.Load()
		case int64(seq-pos) < 0: // the task from the previous lap is still there
			return false
		default: // another submitter claimed the cell
			pos = s.tail.Load()
		}
	}
}

func (s *shard[T]) pop() (T, bool) {
	pos := s.head.Load()
	for {
		cell := &s.slots[pos&s.mask]
		switch seq := cell.seq.Load(); {
		case seq == pos+1:
			if s.head.CompareAndSwap(pos, pos+1) {
				task := cell.task
				var zero T
				cell.task = zero
				cell.seq.Store(pos + s.mask + 1)
				return task, true
			}
			pos = s.head.Load()
		case int64(seq-(pos+1)) < 0: // not written yet
			var zero T
			return zero, false
		default: // another worker took the cell
			pos = s.head.Load()
		}
	}
}

func (s *shard[T]) hasTask() bool {
	pos := s.head.Load()
	return s.slots[pos&s.mask].seq.Load() == pos+1
}

// notify makes sure a worker comes for the tasks queued on the shard, unless
// one is on its way already. The worker it finds takes over the coming count
// it raised; when there is none anywhere it marks the pool starving.
//
// No wakeup is lost. Within a shard, a submitter publishes its task before it
// looks for a worker, and a worker registers as idle before it looks at the
// queue again, so one of them sees the other. Across shards, the count is
// dropped before starving is set, and the idle workers are looked for again
// after; a worker registers as idle before it reads starving, so either it
// sees the flag, with the count already free for its notify, or the second
// look finds it and the loop retries.
//
// It reports whether it brought in a worker.
func (s *shard[T]) notify() bool {
	pool := s.owner
	for s.hasTask() && s.coming.Load() == 0 && s.coming.CompareAndSwap(0, 1) {
		if s.wakeIdle(s) || s.start(s) || pool.borrow(s) {
			return true
		}
		s.coming.Add(-1)
		if !pool.starving.Load() {
			pool.starving.Store(true)
		}
		if !pool.hasIdle() {
			return false
		}
	}
	return false
}

// wakeIdle sends the most recently parked worker of s, whose stack is the
// warmest, to serve target.
func (s *shard[T]) wakeIdle(target *shard[T]) bool {
	if s.idleCount.Load() == 0 {
		return false
	}
	s.mu.Lock()
	last := len(s.idle) - 1
	if last < 0 {
		s.mu.Unlock()
		return false
	}
	w := s.idle[last]
	s.idle[last] = nil
	s.idle = s.idle[:last]
	s.idleCount.Add(-1)
	s.mu.Unlock()
	w.wake <- target
	return true
}

// start starts a worker of s to serve target, unless s has its maximum or the
// pool stopped.
func (s *shard[T]) start(target *shard[T]) bool {
	pool := s.owner
	s.mu.Lock()
	if s.live >= MaxWorkers || pool.stopped.Load() {
		s.mu.Unlock()
		return false
	}
	s.live++
	grown := s.live > 1
	s.mu.Unlock()
	if grown {
		pool.janitor.Do(func() { go pool.retireIdleWorkers() })
	}
	w := &worker[T]{home: s, wake: make(chan *shard[T], 1)}
	pool.workers.Add(1)
	go w.run(target)
	return true
}

// borrow finds target a worker in another shard: an idle one, or a new one
// in a shard with room.
func (pool *Pool[T]) borrow(target *shard[T]) bool {
	count := len(pool.shards)
	first := target.index + 1
	for i := range count {
		if s := &pool.shards[(first+i)%count]; s != target && s.wakeIdle(target) {
			return true
		}
	}
	for i := range count {
		if s := &pool.shards[(first+i)%count]; s != target && s.start(target) {
			return true
		}
	}
	return false
}

func (pool *Pool[T]) hasIdle() bool {
	for i := range pool.shards {
		if pool.shards[i].idleCount.Load() != 0 {
			return true
		}
	}
	return false
}

// run serves target, then parks in its own shard until it is sent to serve a
// shard again. A worker that ends any other way, through runtime.Goexit in a
// task, starts its replacement on the way out.
func (w *worker[T]) run(target *shard[T]) {
	pool := w.home.owner
	exited := false
	defer func() {
		if !exited {
			target.coming.Add(1)
			go w.run(target)
		}
	}()
	for {
		target.running.Add(1)
		target.coming.Add(-1) // arrived
		for task, ok := target.pop(); ok; task, ok = target.pop() {
			// A task may take a while: the ones queued behind it get a worker
			// of their own first.
			if target.hasTask() {
				target.notify()
			}
			pool.run(task)
		}
		target.running.Add(-1)
		// A submitter that saw this worker running left its task to it; one
		// queued after the last pop is taken here.
		if target.hasTask() {
			target.notify()
		}
		if target = w.park(pool); target == nil {
			exited = true
			pool.workers.Done()
			return
		}
	}
}

// park registers the worker as idle in its shard and waits for the next shard
// to serve, or nil when it is retired.
func (w *worker[T]) park(pool *Pool[T]) *shard[T] {
	home := w.home
	home.mu.Lock()
	if pool.stopped.Load() {
		home.live--
		home.mu.Unlock()
		return nil
	}
	w.parked = pool.clock.Load()
	home.idle = append(home.idle, w)
	home.idleCount.Add(1)
	home.mu.Unlock()
	// A task queued after the last pop, before the worker was visible as
	// idle, found nobody: take it.
	if home.hasTask() {
		home.notify()
	}
	if pool.starving.Load() && pool.starving.CompareAndSwap(true, false) {
		for i := range pool.shards {
			pool.shards[i].notify()
		}
	}
	return <-w.wake
}

// overflow runs a task its full shard could not queue, then helps drain the
// shard.
func (s *shard[T]) overflow(task T) {
	for ok := true; ok; task, ok = s.pop() {
		s.owner.run(task)
	}
}

// retireIdleWorkers advances the clock parked workers record and retires the
// workers that have waited MaxIdle, keeping one in each shard.
func (pool *Pool[T]) retireIdleWorkers() {
	ticker := time.NewTicker(pool.janitorEvery)
	defer ticker.Stop()
	for {
		select {
		case <-pool.done:
			return
		case <-ticker.C:
		}
		now := pool.clock.Add(1)
		for i := range pool.shards {
			pool.shards[i].retireIdle(now - pool.idleAfter)
		}
	}
}

// retireIdle sends the workers that parked before deadline away, the longest
// parked first, keeping one.
func (s *shard[T]) retireIdle(deadline int64) {
	if s.idleCount.Load() <= 1 {
		return
	}
	s.mu.Lock()
	count := 0
	for count < len(s.idle)-1 && s.idle[count].parked < deadline {
		count++
	}
	retired := append([]*worker[T](nil), s.idle[:count]...)
	if count > 0 {
		rest := copy(s.idle, s.idle[count:])
		clear(s.idle[rest:])
		s.idle = s.idle[:rest]
		s.idleCount.Add(int32(-count))
		s.live -= int32(count)
	}
	s.mu.Unlock()
	for _, w := range retired {
		w.wake <- nil
	}
}

// Stop runs the tasks still queued and waits for every worker to exit: the
// parked ones are sent to drain their shards, and each worker exits when it
// next finds nothing to do. No worker starts any more, so tasks a shard could
// not hand to one are run on the caller's goroutine. Tasks submitted after
// Stop returns are never run; the caller must not submit any.
func (pool *Pool[T]) Stop() {
	if !pool.stopped.Swap(true) {
		close(pool.done)
		for i := range pool.shards {
			s := &pool.shards[i]
			s.mu.Lock()
			parked := s.idle
			s.idle = nil
			s.idleCount.Store(0)
			s.mu.Unlock()
			for _, w := range parked {
				s.coming.Add(1)
				w.wake <- s
			}
		}
	}
	pool.workers.Wait()
	for i := range pool.shards {
		s := &pool.shards[i]
		for task, ok := s.pop(); ok; task, ok = s.pop() {
			pool.run(task)
		}
	}
}
