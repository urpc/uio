//go:build !linux || stdio

package uio

import "github.com/urpc/uio/internal/poller"

// dataShardCount is always one here: without a data plane there is nothing to
// shard, and the io pool keeps its single turn queue.
func dataShardCount(int) int { return 1 }

// dataPlaneSharded is false: without a data plane there is nothing to shard
// connection collection into, so the io pool keeps a single turn queue.
const dataPlaneSharded = false

// dataPoller exists only on Linux, where epoll events can carry the
// registration tag that makes one shared stream poller safe. Other backends
// keep polling streams on their owning event loop.
type dataPoller struct{}

func newDataPoller(*Events) (*dataPoller, error) { return nil, nil }

func (data *dataPoller) start(*Events) {}

func (data *dataPoller) close(error) {}

func (data *dataPoller) watcher() *poller.NetPoller { return nil }

// register and unregister exist so the registration path compiles on
// backends without a data plane; data is always nil there.
func (data *dataPoller) register(*fdConn) error { return nil }
func (data *dataPoller) unregister(int)         {}

// watcherFor exists so the shared registration path compiles on backends
// without a data plane; data is always nil there.
func (data *dataPoller) watcherFor(int) *poller.NetPoller { return nil }
