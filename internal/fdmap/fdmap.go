//go:build windows

package fdmap

import (
	"iter"
	"sync"
)

const UseSingleInstance = false

type Map[V any] struct {
	store sync.Map
}

func NewMap[V any]() *Map[V] {
	return &Map[V]{}
}

func (m *Map[V]) Put(k int, v *V) {
	m.store.Store(k, v)
}

func (m *Map[V]) Get(k int) *V {
	if v, loaded := m.store.Load(k); loaded {
		return v.(*V)
	}
	return nil
}

func (m *Map[V]) Range() iter.Seq2[int, *V] {
	return func(yield func(int, *V) bool) {
		m.store.Range(func(key, value any) bool {
			return yield(key.(int), value.(*V))
		})
	}
}

func (m *Map[V]) Delete(k int) {
	m.store.Delete(k)
}
