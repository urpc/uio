//go:build race

package plocal

import "sync"

// Cache is the pool alone in race-enabled builds; see the package comment.
type Cache[T any] struct {
	pool sync.Pool
	new  func() T
}

func (c *Cache[T]) Init(newValue func() T) { c.new = newValue }

func (c *Cache[T]) Get() T {
	if value, ok := c.pool.Get().(T); ok {
		return value
	}
	if c.new != nil {
		return c.new()
	}
	var zero T
	return zero
}

func (c *Cache[T]) Put(value T) { c.pool.Put(value) }
