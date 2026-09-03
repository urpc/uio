package pool

import (
	"math/bits"
	"sync"
)

const minimumPooledSize = 512

// Pool contains logic of reusing objects distinguishable by size in generic
// way.
type Pool[T any] struct {
	pool    []sync.Pool
	minSize int
	maxSize int
}

// New creates new Pool that reuses objects which size
func New[T any](capacity int) *Pool[T] {
	maxSize := CeilToPowerOfTwo(Max(capacity, 1))
	minSize := 1
	if maxSize >= minimumPooledSize {
		minSize = minimumPooledSize
	}
	return &Pool[T]{
		pool:    make([]sync.Pool, classIndex(maxSize)-classIndex(minSize)+1),
		minSize: minSize,
		maxSize: maxSize,
	}
}

func classIndex(size int) int { return bits.Len(uint(size)) - 1 }

func (p *Pool[T]) sizeClass(size int) int {
	if size <= p.minSize {
		return p.minSize
	}
	return CeilToPowerOfTwo(size)
}

// Get pulls object whose generic size is at least of given size.
// It also returns a real size of x for further pass to Put() even if x is nil.
// Pooled sizes are rounded to the next power of two; larger requests retain
// their exact size because they are not pooled.
func (p *Pool[T]) Get(size int) (T, int) {
	if size > p.maxSize {
		var zero T
		return zero, size
	}
	n := p.sizeClass(size)
	idx := classIndex(n) - classIndex(p.minSize)
	if v := p.pool[idx].Get(); v != nil {
		return v.(T), n
	}

	var zero T
	return zero, n
}

// Put takes x and its size for future reuse.
func (p *Pool[T]) Put(x T, size int) {
	if size < p.minSize || size > p.maxSize || !IsPowerOfTwo(size) {
		return
	}
	p.pool[classIndex(size)-classIndex(p.minSize)].Put(x)
}
