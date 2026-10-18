//go:build race

/*
 * Copyright 2024 the urpc project
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *      https://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

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
