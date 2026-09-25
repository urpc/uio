//go:build !linux || stdio

package uio

import "github.com/urpc/uio/internal/poller"

// dataPoller exists only on Linux, where epoll events can carry the
// registration tag that makes one shared stream poller safe. Other backends
// keep polling streams on their owning event loop.
type dataPoller struct{}

func newDataPoller(*Events) (*dataPoller, error) { return nil, nil }

func (data *dataPoller) start(*Events) {}

func (data *dataPoller) close(error) {}

func (data *dataPoller) watcher() *poller.NetPoller { return nil }
