package pool

import (
	"sync"
)

// Pool contains logic of reusing objects distinguishable by size in generic
// way.
type Pool[T any] struct {
	pool     []sync.Pool
	size     func(int) int
	stepSize int
}

// New creates new Pool that reuses objects which size
func New[T any](max int) *Pool[T] {
	maxSize := CeilToPowerOfTwo(Max(max, 1))

	shardSize := Max(1, Min(maxSize, 64))
	stepSize := CeilToPowerOfTwo((maxSize) / shardSize)
	if stepSize*shardSize < maxSize {
		shardSize++
	}

	return &Pool[T]{
		pool: make([]sync.Pool, shardSize),
		size: func(i int) int {
			if i <= stepSize {
				return stepSize
			}
			return CeilToPowerOfTwo(i)
		},
		stepSize: stepSize,
	}
}

// Get pulls object whose generic size is at least of given size.
// It also returns a real size of x for further pass to Put() even if x is nil.
// Note that size could be ceiled to the next power of two.
func (p *Pool[T]) Get(size int) (T, int) {
	n := p.size(size)

	if idx := (n - 1) / p.stepSize; idx < len(p.pool) {
		if v := p.pool[idx].Get(); v != nil {
			return v.(T), n
		}
	}

	var zero T
	return zero, n
}

// Put takes x and its size for future reuse.
func (p *Pool[T]) Put(x T, size int) {
	if size < p.stepSize {
		return
	}

	if idx := (size - 1) / p.stepSize; idx < len(p.pool) {
		p.pool[idx].Put(x)
	}
}
