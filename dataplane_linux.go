//go:build linux && !stdio

package uio

import (
	"net"
	"runtime"
	"sync"

	"github.com/urpc/uio/internal/fdmap"
	"github.com/urpc/uio/internal/poller"
)

// The data plane shards stream readiness over several independent pollers.
// Each connection belongs to one shard, picked by descriptor at registration,
// and that shard's waiters are the only ones that ever report it. What they
// hand over still goes to the io pool's one shared queue: turn queues per
// shard were measured against it and came out 4% slower, so the pool is
// untouched by this.
//
// One shared poller behind a handful of waiters measured well until the
// machine got wide: on a 64-core host, wake-ups from a single queue landed on
// the submitting waiter's P and every worker crossed cores to reach them
// (runtime/trace: ~23 core-seconds per second runnable-but-unscheduled against
// ~30 running). Several pollers keep a connection's events in one cluster of
// cores, so its state stays in one L3 domain and a wake-up no longer races
// every other core for a P. The shard count defaults to the number of L3
// domains a wide AMD part tends to have; measurements on the 64-core host put
// it at 8.
const defaultDataShards = 8

// dataPlaneSharded reports that this backend's connections are collected by
// the sharded stream poller, so the io pool must keep one turn queue per
// shard. Backends without one keep a single queue.
const dataPlaneSharded = true

// dataShardsOverride fixes the shard count in tests; zero means automatic.
var dataShardsOverride int

// dataWaitersOverride fixes the per-shard waiter count in tests; zero means
// automatic.
var dataWaitersOverride int

// dataShardCount scales the shard count with the machine: about four Ps per
// shard, at least one and at most defaultDataShards. A small host keeps a
// single collector, so it pays nothing for a structure it cannot fill, and a
// wide one gets the locality without a count tied to any particular chip.
func dataShardCount(procs int) int {
	if dataShardsOverride > 0 {
		return dataShardsOverride
	}
	count := procs / 4
	if count < 1 {
		count = 1
	}
	if count > defaultDataShards {
		count = defaultDataShards
	}
	return count
}

// dataPoller owns one epoll instance per shard.
type dataPoller struct {
	shards []*dataShard
	pool   *ioTaskPool
	wg     sync.WaitGroup
}

// dataShard is one shard's poller and lookup table. On Unix newFdMap hands
// every caller the process-wide table, so the shards in fact resolve events
// through one shared table — which registerConn relies on: a connection is
// published there before its descriptor is watched, so a waiter is never
// handed an event for a connection no table holds yet. Per-shard tables would
// reopen that gap, since data.register runs after the watch starts.
type dataShard struct {
	poller *poller.NetPoller
	fdMap  *fdmap.Map[fdConn]
}

// dataWaiter is one waiter goroutine's private dispatch state.
type dataWaiter struct {
	batch poller.Batch
	evbuf []poller.Event
	ready []*fdConn
	args  []IOTask
}

func newDataPoller(ev *Events) (*dataPoller, error) {
	data := &dataPoller{pool: ev.ioPool}
	for range ev.dataShards {
		netPoller, err := poller.NewNetPoller()
		if err != nil {
			for _, shard := range data.shards {
				_ = shard.poller.Close(err)
			}
			return nil, err
		}
		data.shards = append(data.shards, &dataShard{poller: netPoller, fdMap: newFdMap()})
	}
	return data, nil
}

// shard returns the shard owning fd. Descriptors arrive in allocation order,
// so consecutive connections land on different shards and a reuse of a
// descriptor lands on the shard the connection was watched by.
func (data *dataPoller) shard(fd int) *dataShard {
	return data.shards[fd%len(data.shards)]
}

// watcherFor returns the poller watching fd.
func (data *dataPoller) watcherFor(fd int) *poller.NetPoller {
	return data.shard(fd).poller
}

// register publishes conn in its shard's lookup table.
func (data *dataPoller) register(conn *fdConn) error {
	return data.shard(conn.fd).fdMap.Put(conn.fd, conn)
}

// unregister drops fd from its shard's lookup table.
func (data *dataPoller) unregister(fd int) {
	data.shard(fd).fdMap.Delete(fd)
}

// dataWaiters is how many waiter goroutines each shard runs. epoll hands each
// ready edge to one waiter, so a second waiter on the same poller collects the
// next batch while the first is still folding its events in and handing them
// to the executor. The process-wide total scales with the machine — one per
// four Ps, at least two — and the shards divide it, so a small host whose
// single shard keeps them all behaves as it always did, and a wide one keeps
// two per shard while its shards are what feed the queues. Measurements on a
// 64-core host put the knee there: a third waiter per shard lost throughput.
func dataWaiters(shards int) int {
	if dataWaitersOverride > 0 {
		return dataWaitersOverride
	}
	procs := runtime.GOMAXPROCS(0)
	if procs < 4 {
		return 1
	}
	total := max(2, procs/4)
	perShard := total / shards
	if perShard < 1 {
		perShard = 1
	}
	return perShard
}

// parkedWaiters is the most waiters that still benefit from a parked wait.
// The runtime's netpoller wakes a parked goroutine only when a P runs out of
// work or its 10ms tick lands, so once enough waiters park at once some wait
// minutes-long stretches in wall-clock terms and closed-loop echo pays it in
// round trips: measured on a 64-logical-CPU host, one waiter per shard kept
// pace while two per shard — sixteen parked waiters over eight epoll
// descriptors — halved throughput (3.7M to 1.5M requests a second). Up to the
// eight waiters a 48-CPU host runs, park measured level or ahead everywhere,
// and on a small host it is what fills the cores at all. Past that the waiter
// blocks in epoll_wait with the raw probe as before.
const parkedWaiters = 8

func (data *dataPoller) start(ev *Events) {
	perShard := dataWaiters(len(data.shards))
	parked := perShard*len(data.shards) <= parkedWaiters
	for _, shard := range data.shards {
		for range perShard {
			waiter := &dataWaiter{
				evbuf: make([]poller.Event, eventBatch),
				ready: make([]*fdConn, 0, eventBatch),
				args:  make([]IOTask, 0, eventBatch),
			}
			data.wg.Add(1)
			go func(shard *dataShard) {
				defer data.wg.Done()
				if ev.LockOSThread {
					runtime.LockOSThread()
					defer runtime.UnlockOSThread()
				}
				if err := data.serve(shard, waiter, perShard > 1, parked); err != nil {
					ev.initiateClose(err)
				}
			}(shard)
		}
	}
}

// serve dispatches one shard's readiness until the poller is closed. Each
// event carries the tag the connection was registered with: a loop may close a
// descriptor, and the kernel reuse its number for a new connection, after
// epoll_wait has returned an event for it. The tag keeps such an event from
// reaching the new connection before its open task.
//
// With more than one waiter on a shard, a waiter that handed tasks to the
// executor yields before it waits again. The executor's wake-ups queue the
// workers on this waiter's P, and epoll_wait would keep that P in a system
// call until the runtime retakes it; yielding runs them at once while another
// waiter keeps watching the poller. A lone waiter must not yield: it would
// queue behind the very workers it woke.
func (data *dataPoller) serve(shard *dataShard, waiter *dataWaiter, yield, parked bool) error {
	for {
		var n int
		var err error
		if parked {
			n, err = shard.poller.WaitBatchParked(&waiter.batch, waiter.evbuf)
		} else {
			n, err = shard.poller.WaitBatch(&waiter.batch, waiter.evbuf, -1)
		}
		if shard.poller.Closed() {
			return nil
		}
		if err != nil {
			return err
		}
		data.dispatch(shard, waiter, waiter.evbuf[:n])
		if data.submit(waiter) && yield {
			runtime.Gosched()
		}
	}
}

// dispatch folds readiness into the connections this shard watches and
// collects the ones that became runnable.
func (data *dataPoller) dispatch(shard *dataShard, waiter *dataWaiter, events []poller.Event) {
	for _, event := range events {
		conn := shard.fdMap.Get(event.FD)
		if conn == nil || conn.pollTag != event.Tag || conn.isClosing() || conn.skipsEdge(event.Events) {
			continue
		}
		if conn.noteIO(uint32(event.Events)) {
			waiter.ready = append(waiter.ready, conn)
		}
	}
}

func (data *dataPoller) submit(waiter *dataWaiter) bool {
	if len(waiter.ready) == 0 {
		return false
	}
	connections := waiter.ready
	waiter.ready = waiter.ready[:0]
	if !data.pool.submitConnBatch(connections, waiter.args) {
		for _, conn := range connections {
			conn.handleIOSubmitFailure(net.ErrClosed)
		}
	}
	waiter.args = waiter.args[:0]
	clear(connections)
	return true
}

// close stops every shard's waiter. Close raises each shard's waiter on its
// own wake descriptor, so every waiter returns from epoll_wait and sees the
// closed poller regardless of what the others drained.
func (data *dataPoller) close(err error) {
	for _, shard := range data.shards {
		_ = shard.poller.Close(err)
	}
	data.wg.Wait()
}
