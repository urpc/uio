//go:build !race

package plocal

import (
	"runtime"
	"sync"
	_ "unsafe" // linkname
)

//go:linkname procPin runtime.procPin
func procPin() int

//go:linkname procUnpin runtime.procUnpin
func procUnpin()

// cacheLineSize pads every slot, so whatever T's size, two slots' values are
// at least a line apart and the Ps writing them never share one.
const cacheLineSize = 64

// Cache keeps a value of type T per P. Call Init before using it and do not
// copy it after; T is best a pointer, which keeps the exchange to one word.
type Cache[T any] struct {
	slots []slot[T]
	pool  sync.Pool
	new   func() T
}

type slot[T any] struct {
	value T
	full  bool
	_     [cacheLineSize]byte
}

// Init gives the cache a slot for each P the process may run, and newValue,
// which Get calls when neither the slot nor the pool has a value. The slots
// cover GOMAXPROCS or the CPU count, whichever is larger, so a cache created
// at package initialization still has a slot for every P after the program
// raises GOMAXPROCS up to the CPU count; a P beyond that uses the pool alone.
// Without Init the cache is the pool alone, at the pool's own cost, and Get
// returns T's zero value when the pool is empty.
func (c *Cache[T]) Init(newValue func() T) {
	c.slots = make([]slot[T], max(runtime.GOMAXPROCS(0), runtime.NumCPU()))
	c.new = newValue
}

// slotOf returns the slot of P pid, or nil for a P the cache has none for.
func (c *Cache[T]) slotOf(pid int) *slot[T] {
	if uint(pid) < uint(len(c.slots)) {
		return &c.slots[pid]
	}
	return nil
}

// Get takes the value from the slot of the caller's P, or else one from the
// pool or newValue. The value is the caller's until it passes it to Put.
func (c *Cache[T]) Get() T {
	if c.slots == nil {
		return c.getSlow()
	}
	if s := c.slotOf(procPin()); s != nil && s.full {
		value := s.value
		var zero T
		s.value, s.full = zero, false
		procUnpin()
		return value
	}
	procUnpin()
	return c.getSlow()
}

func (c *Cache[T]) getSlow() T {
	if value, ok := c.pool.Get().(T); ok {
		return value
	}
	if c.new != nil {
		return c.new()
	}
	var zero T
	return zero
}

// Put gives value back: to the slot of the caller's P when it is empty, or
// else to the pool. The caller must not use value afterwards.
func (c *Cache[T]) Put(value T) {
	if c.slots == nil {
		c.pool.Put(value)
		return
	}
	if s := c.slotOf(procPin()); s != nil && !s.full {
		s.value, s.full = value, true
		procUnpin()
		return
	}
	procUnpin()
	c.pool.Put(value)
}
