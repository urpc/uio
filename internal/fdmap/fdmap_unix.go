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

// The table is segmented: descriptors under 32 Ki index one 256 KiB segment,
// a large object that gets a span of its own, so the slots the loops touch
// never share a cache line with unrelated allocations. A segment is allocated
// the first time a descriptor in it is published, which keeps the live heap
// the collector sees at the size actually in use instead of the whole
// descriptor range.
const (
	segmentShift = 15
	segmentSlots = 1 << segmentShift
	maxSegments  = maxOpenFilesCeiling >> segmentShift
)

// segment is one contiguous range of the descriptor table.
type segment[V any] struct {
	slots [segmentSlots]*V
}

// Map is an atomic direct-index table shared by Unix event loops. Deletion must
// be published before a descriptor is closed because the kernel may reuse its
// integer immediately.
type Map[V any] struct {
	// Direct fd indexing avoids hashing. Entries are atomic because registration
	// and lookup can occur on different loops; segments are published the same
	// way, so a loop touching a new descriptor range allocates one without
	// locking.
	segments [maxSegments]atomic.Pointer[segment[V]]
	limit    int // the MaxOpenFiles this table was created for
}

// NewMap returns an empty descriptor table; segments allocate with the
// descriptors published into them.
func NewMap[V any]() *Map[V] {
	return &Map[V]{limit: min(MaxOpenFiles, maxOpenFilesCeiling)}
}

// slot bounds-checks k and resolves its segment, or returns nil.
func (m *Map[V]) slot(k int) (*unsafe.Pointer, bool) {
	if uint(k) >= uint(m.limit) {
		return nil, false
	}
	seg := m.segments[k>>segmentShift].Load()
	if seg == nil {
		return nil, false
	}
	return (*unsafe.Pointer)(unsafe.Pointer(&seg.slots[k&(segmentSlots-1)])), true
}

// Put atomically publishes v at descriptor k.
func (m *Map[V]) Put(k int, v *V) error {
	if uint(k) >= uint(m.limit) {
		return ErrOutOfRange
	}
	idx := k >> segmentShift
	seg := m.segments[idx].Load()
	if seg == nil {
		seg = new(segment[V])
		if !m.segments[idx].CompareAndSwap(nil, seg) {
			seg = m.segments[idx].Load()
		}
	}
	atomic.StorePointer((*unsafe.Pointer)(unsafe.Pointer(&seg.slots[k&(segmentSlots-1)])), unsafe.Pointer(v))
	return nil
}

// Get atomically resolves descriptor k, or returns nil when absent.
func (m *Map[V]) Get(k int) *V {
	p, ok := m.slot(k)
	if !ok {
		return nil
	}
	if val := atomic.LoadPointer(p); nil != val {
		return (*V)(val)
	}
	return nil
}

// Range visits a weakly consistent view of live descriptors without locking
// producers. Callers may delete the yielded entry.
func (m *Map[V]) Range() iter.Seq2[int, *V] {
	return func(yield func(int, *V) bool) {
		for s := 0; s < maxSegments; s++ {
			seg := m.segments[s].Load()
			if seg == nil {
				continue
			}
			for i := 0; i < segmentSlots && s*segmentSlots+i < MaxOpenFiles; i++ {
				if pointer := atomic.LoadPointer((*unsafe.Pointer)(unsafe.Pointer(&seg.slots[i]))); pointer != nil && !yield(s*segmentSlots+i, (*V)(pointer)) {
					return
				}
			}
		}
	}
}

// Delete atomically unpublishes descriptor k.
func (m *Map[V]) Delete(k int) {
	p, ok := m.slot(k)
	if !ok {
		return
	}
	// Swap publishes absence before the caller closes and potentially reuses fd.
	atomic.SwapPointer(p, unsafe.Pointer(nil))
}

// DeleteValue removes k only while it still maps to v, so a holder that lost
// the slot to another value leaves that value in place.
func (m *Map[V]) DeleteValue(k int, v *V) {
	p, ok := m.slot(k)
	if !ok {
		return
	}
	atomic.CompareAndSwapPointer(p, unsafe.Pointer(v), nil)
}

// Clear atomically removes every descriptor entry.
func (m *Map[V]) Clear() {
	for s := 0; s < maxSegments; s++ {
		seg := m.segments[s].Swap(nil)
		if seg == nil {
			continue
		}
		for i := 0; i < segmentSlots; i++ {
			atomic.StorePointer((*unsafe.Pointer)(unsafe.Pointer(&seg.slots[i])), unsafe.Pointer(nil))
		}
	}
}
