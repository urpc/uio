//go:build !windows

package fdmap

import (
	"iter"
	"sync/atomic"
	"syscall"
	"unsafe"
)

// Unix fds are small integers, so all event loops can share one direct index.
const UseSingleInstance = true

const maxOpenFilesCeiling = 1 << 20

// MaxOpenFiles covers the conventional Unix per-process fd ceiling. It is
// reduced to the process soft limit during initialization when necessary.
var MaxOpenFiles = maxOpenFilesCeiling

func init() {
	var limit syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &limit); err == nil {
		MaxOpenFiles = openFileCapacity(uint64(limit.Cur))
	}
}

func openFileCapacity(limit uint64) int {
	if limit > 0 && limit < maxOpenFilesCeiling {
		return int(limit)
	}
	return maxOpenFilesCeiling
}

type Map[V any] struct {
	// Direct fd indexing avoids hashing. Entries are atomic because registration
	// and lookup can occur on different loops.
	store []*V
}

func NewMap[V any]() *Map[V] {
	return &Map[V]{
		store: make([]*V, MaxOpenFiles),
	}
}

func (m *Map[V]) Put(k int, v *V) error {
	if uint(k) >= uint(len(m.store)) {
		return ErrOutOfRange
	}
	atomic.StorePointer((*unsafe.Pointer)(unsafe.Pointer(&m.store[k])), unsafe.Pointer(v))
	return nil
}

func (m *Map[V]) Get(k int) *V {
	if uint(k) >= uint(len(m.store)) {
		return nil
	}
	if val := atomic.LoadPointer((*unsafe.Pointer)(unsafe.Pointer(&m.store[k]))); nil != val {
		return (*V)(val)
	}
	return nil
}

func (m *Map[V]) Range() iter.Seq2[int, *V] {
	return func(yield func(int, *V) bool) {
		for i := 0; i < len(m.store); i++ {
			if pointer := atomic.LoadPointer((*unsafe.Pointer)(unsafe.Pointer(&m.store[i]))); pointer != nil && !yield(i, (*V)(pointer)) {
				return
			}
		}
	}
}

func (m *Map[V]) Delete(k int) {
	if uint(k) >= uint(len(m.store)) {
		return
	}
	// Swap publishes absence before the caller closes and potentially reuses fd.
	atomic.SwapPointer((*unsafe.Pointer)(unsafe.Pointer(&m.store[k])), unsafe.Pointer(nil))
}

func (m *Map[V]) Clear() {
	for i := 0; i < len(m.store); i++ {
		atomic.SwapPointer((*unsafe.Pointer)(unsafe.Pointer(&m.store[i])), unsafe.Pointer(nil))
	}
}
