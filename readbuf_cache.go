//go:build !race

package uio

import (
	"runtime"
	_ "unsafe" // linkname
)

//go:linkname procPin runtime.procPin
func procPin() int

//go:linkname procUnpin runtime.procUnpin
func procUnpin()

// readBufferCache keeps one read buffer per P in front of the shared pool. A
// connection turn borrows a buffer for its read round and returns it when the
// round ends, and the slot of the P it runs on takes the buffer without the
// pool's bookkeeping, which measured four times the instructions of the swap.
// Pinning the goroutine to its P for the swap keeps every other goroutine
// away from the slot meanwhile.
type readBufferCache struct {
	slots []readBufferSlot
}

type readBufferSlot struct {
	buffer *readBuffer
	_      [cacheLineSize - 8]byte
}

// init sizes the cache to the Ps the process has; a P added later uses the
// pool alone.
func (cache *readBufferCache) init() {
	cache.slots = make([]readBufferSlot, runtime.GOMAXPROCS(0))
}

// borrowReadBuffer lends a read round its buffer, from the slot of the P it
// runs on when that holds one.
func (ev *Events) borrowReadBuffer() *readBuffer {
	slots := ev.readCache.slots
	pid := procPin()
	if pid < len(slots) {
		if buffer := slots[pid].buffer; buffer != nil {
			slots[pid].buffer = nil
			procUnpin()
			return buffer
		}
	}
	procUnpin()
	return ev.readPool.Get().(*readBuffer)
}

func (ev *Events) returnReadBuffer(buffer *readBuffer) {
	slots := ev.readCache.slots
	pid := procPin()
	if pid < len(slots) && slots[pid].buffer == nil {
		slots[pid].buffer = buffer
		procUnpin()
		return
	}
	procUnpin()
	ev.readPool.Put(buffer)
}
