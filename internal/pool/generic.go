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
func New[T any](max int) *Pool[T] {
	maxSize := CeilToPowerOfTwo(Max(max, 1))
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
// Note that size could be ceiled to the next power of two.
func (p *Pool[T]) Get(size int) (T, int) {
	n := p.sizeClass(size)

	// Sizes above the configured maximum deliberately bypass pooling.
	if idx := classIndex(n) - classIndex(p.minSize); n <= p.maxSize {
		if v := p.pool[idx].Get(); v != nil {
			return v.(T), n
		}
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
