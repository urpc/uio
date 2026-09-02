//go:build windows

package fdmap

import (
	"iter"
	"sync"
)

const UseSingleInstance = false

type Map[V any] struct {
	mu    sync.RWMutex
	store map[int]*V
}

func NewMap[V any]() *Map[V] {
	return &Map[V]{store: make(map[int]*V)}
}

func (m *Map[V]) Put(k int, v *V) error {
	m.mu.Lock()
	m.store[k] = v
	m.mu.Unlock()
	return nil
}

func (m *Map[V]) Get(k int) *V {
	m.mu.RLock()
	v := m.store[k]
	m.mu.RUnlock()
	return v
}

func (m *Map[V]) Range() iter.Seq2[int, *V] {
	return func(yield func(int, *V) bool) {
		// Snapshot because callers may delete entries from inside yield.
		m.mu.RLock()
		entries := make([]entry[V], 0, len(m.store))
		for key, value := range m.store {
			entries = append(entries, entry[V]{key: key, value: value})
		}
		m.mu.RUnlock()
		for _, entry := range entries {
			if !yield(entry.key, entry.value) {
				return
			}
		}
	}
}

func (m *Map[V]) Delete(k int) {
	m.mu.Lock()
	delete(m.store, k)
	m.mu.Unlock()
}

type entry[V any] struct {
	key   int
	value *V
}
