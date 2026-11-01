package shardpool

import (
	"fmt"
	"testing"
	"time"
)

// A batch arriving while its shard's worker is held in a task gets a worker
// of its own at once, not one the monitor brings in later: with the monitor
// effectively disabled it still runs while the first task blocks. This covers
// both the single-word and the multiword touched bitmap.
func TestSubmitBatchDoesNotWaitBehindABlockedTask(t *testing.T) {
	for _, active := range []int{1, 96} {
		t.Run(fmt.Sprintf("active=%d", active), func(t *testing.T) {
			release := make(chan struct{})
			started := make(chan struct{}, 1)
			fast := make(chan struct{}, 1)
			pool := New(max(1, active/4), 64, func(*task) uint32 { return 0 }, func(one *task) {
				if one.key == 0 {
					started <- struct{}{}
					<-release
					return
				}
				fast <- struct{}{}
			})
			pool.active.Store(int32(active))
			pool.tick = time.Hour
			defer pool.Stop()
			defer close(release)
			pool.Submit(&task{key: 0})
			receive(t, started, "the blocked task")
			pool.SubmitBatch([]*task{{key: 1}})
			receive(t, fast, "the batch submitted behind the blocked task")
		})
	}
}

// A batch that already has a backlog still expands in worker.pop, even with
// the monitor effectively disabled. New workers can therefore run callbacks
// in parallel when an earlier task blocks.
func TestSubmitBatchBacklogStillExpands(t *testing.T) {
	for _, active := range []int{1, 96} {
		t.Run(fmt.Sprintf("active=%d", active), func(t *testing.T) {
			release := make(chan struct{})
			fast := make(chan struct{}, 1)
			pool := New(max(1, active/4), 64, func(*task) uint32 { return 0 }, func(one *task) {
				if one.key == 0 {
					<-release
					return
				}
				fast <- struct{}{}
			})
			pool.active.Store(int32(active))
			pool.tick = time.Hour
			defer pool.Stop()
			defer close(release)
			pool.SubmitBatch([]*task{{key: 0}, {key: 1}})
			receive(t, fast, "the task served by backlog expansion")
		})
	}
}
