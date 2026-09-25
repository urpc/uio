//go:build linux && !stdio

package uio

import (
	"net"
	"runtime"
	"sync"

	"github.com/urpc/uio/internal/fdmap"
	"github.com/urpc/uio/internal/poller"
)

// dataPoller watches the readiness of every stream connection through one
// shared epoll instance and hands runnable connections to the task pool.
//
// Event loops keep owning registration, interest, deadlines, close, UDP and
// accept, but they no longer wait for stream readiness themselves. A loop that
// only saw a share of the traffic kept draining its share and blocking in
// epoll_wait; every time it blocked it gave up its P, and under load it then
// waited for another one while its connections' input sat in the kernel. That
// made a connection's latency depend on which loop owned it. A busy waiter
// rarely blocks, and one shared ready list hands input to the pool in arrival
// order no matter how many loops there are. Large hosts run a few waiters on
// the same instance; epoll gives each ready edge to one of them.
type dataPoller struct {
	poller *poller.NetPoller
	fdMap  *fdmap.Map[fdConn]
	pool   *ioTaskPool
	wg     sync.WaitGroup
}

// dataWaiter is one goroutine's private dispatch state.
type dataWaiter struct {
	batch poller.Batch
	evbuf []poller.Event
	ready []*fdConn
	args  []IOTask
}

func newDataPoller(ev *Events) (*dataPoller, error) {
	netPoller, err := poller.NewNetPoller()
	if err != nil {
		return nil, err
	}
	return &dataPoller{poller: netPoller, fdMap: newFdMap(), pool: ev.ioPool}, nil
}

// dataWaitersOverride fixes the waiter count in tests; zero means automatic.
var dataWaitersOverride int

// dataWaiters is how many goroutines wait on the shared poller. One of them
// dispatches about a million events per second, roughly what 24 CPUs of the
// lightest request/response traffic produce, so most servers need exactly one.
// More waiters only split the load into goroutines that block more often.
func dataWaiters() int {
	if dataWaitersOverride > 0 {
		return dataWaitersOverride
	}
	return max(1, runtime.GOMAXPROCS(0)/24)
}

func (data *dataPoller) start(ev *Events) {
	for range dataWaiters() {
		waiter := &dataWaiter{
			evbuf: make([]poller.Event, eventBatch),
			ready: make([]*fdConn, 0, eventBatch),
			args:  make([]IOTask, 0, eventBatch),
		}
		data.wg.Add(1)
		go func() {
			defer data.wg.Done()
			if ev.LockOSThread {
				runtime.LockOSThread()
				defer runtime.UnlockOSThread()
			}
			if err := data.serve(waiter); err != nil {
				ev.initiateClose(err)
			}
		}()
	}
}

// serve dispatches readiness until the poller is closed. Each event carries
// the tag the connection was registered with: a loop may close a descriptor,
// and the kernel reuse its number for a new connection, after epoll_wait has
// returned an event for it. The tag keeps such an event from reaching the new
// connection before its open task.
func (data *dataPoller) serve(waiter *dataWaiter) error {
	for {
		n, err := data.poller.WaitBatch(&waiter.batch, waiter.evbuf, -1)
		if data.poller.Closed() {
			return nil
		}
		if err != nil {
			return err
		}
		data.dispatch(waiter, waiter.evbuf[:n])
		data.submit(waiter)
	}
}

// dispatch folds readiness into the connections still registered under the
// reported tag and collects the ones that became runnable.
func (data *dataPoller) dispatch(waiter *dataWaiter, events []poller.Event) {
	for _, event := range events {
		conn := data.fdMap.Get(event.FD)
		if conn == nil || conn.pollTag != event.Tag || conn.isClosing() {
			continue
		}
		if conn.noteIO(uint32(event.Events)) {
			waiter.ready = append(waiter.ready, conn)
		}
	}
}

func (data *dataPoller) submit(waiter *dataWaiter) {
	if len(waiter.ready) == 0 {
		return
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
}

// close stops the waiters once every loop has deregistered its connections.
// The wake descriptor is level-triggered and nobody drains it after Close, so
// each waiter in turn returns from epoll_wait and sees the closed poller.
func (data *dataPoller) close(err error) {
	_ = data.poller.Close(err)
	data.wg.Wait()
}

func (data *dataPoller) watcher() *poller.NetPoller { return data.poller }
