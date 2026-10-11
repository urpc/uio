//go:build race

package uio

// The race detector cannot see that pinning a goroutine to its P orders the
// accesses to a per-P slot, so race-enabled builds borrow read buffers from
// the pool alone.
type readBufferCache struct{}

func (*readBufferCache) init() {}

func (ev *Events) borrowReadBuffer() *readBuffer { return ev.readPool.Get().(*readBuffer) }

func (ev *Events) returnReadBuffer(buffer *readBuffer) { ev.readPool.Put(buffer) }
