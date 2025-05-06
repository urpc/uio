//go:build !windows

package fdmap

import (
	"iter"
	"sync/atomic"
	"syscall"
	"unsafe"
)

const UseSingleInstance = true

var MaxOpenFiles = 1_000_000

func init() {
	var limit syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &limit); err == nil {
		if n := int(limit.Max); n > 0 && n < MaxOpenFiles {
			MaxOpenFiles = n
		}
	}
}

type Map[V any] struct {
	store []*V
}

func NewMap[V any]() *Map[V] {
	return &Map[V]{
		store: make([]*V, MaxOpenFiles),
	}
}

func (m *Map[V]) Put(k int, v *V) {
	atomic.StorePointer((*unsafe.Pointer)(unsafe.Pointer(&m.store[k])), unsafe.Pointer(v))
}

func (m *Map[V]) Get(k int) *V {
	if val := atomic.LoadPointer((*unsafe.Pointer)(unsafe.Pointer(&m.store[k]))); nil != val {
		return (*V)(val)
	}
	return nil
}

func (m *Map[V]) Range() iter.Seq2[int, *V] {
	return func(yield func(int, *V) bool) {
		for i := 0; i < len(m.store); i++ {
			if v := m.store[i]; v != nil && !yield(i, v) {
				return
			}
		}
	}
}

func (m *Map[V]) Delete(k int) {
	atomic.SwapPointer((*unsafe.Pointer)(unsafe.Pointer(&m.store[k])), unsafe.Pointer(nil))
}

func (m *Map[V]) Clear() {
	for i := 0; i < len(m.store); i++ {
		atomic.SwapPointer((*unsafe.Pointer)(unsafe.Pointer(&m.store[i])), unsafe.Pointer(nil))
	}
}
