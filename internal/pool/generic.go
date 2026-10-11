// Package pool implements bounded power-of-two object size classes.
package pool

import (
	"math/bits"
	"sync"

	"github.com/urpc/uio/internal/plocal"
)

const minimumPooledSize = 512

// maxSlottedSize is the largest size class that keeps an object per P in
// front of its pool (see plocal). Above it a class uses the pool alone, so
// what the per-P slots retain while idle stays within twice this size per P:
// one object of each class up to it.
const maxSlottedSize = 16 << 10

// Pool reuses objects in power-of-two size classes up to a configured maximum.
// It does not retain oversized objects.
type Pool[T any] struct {
	cached  []plocal.Cache[T] // the classes up to maxSlottedSize, smallest first
	pooled  []sync.Pool       // every class, used for those above it
	minSize int
	maxSize int
}

// New creates a size-classed pool whose largest retained object is capacity.
func New[T any](capacity int) *Pool[T] {
	maxSize := CeilToPowerOfTwo(Max(capacity, 1))
	minSize := 1
	if maxSize >= minimumPooledSize {
		minSize = minimumPooledSize
	}
	classes := classIndex(maxSize) - classIndex(minSize) + 1
	cached := 0
	if minSize <= maxSlottedSize {
		cached = min(classes, classIndex(maxSlottedSize)-classIndex(minSize)+1)
	}
	p := &Pool[T]{
		cached:  make([]plocal.Cache[T], cached),
		pooled:  make([]sync.Pool, classes),
		minSize: minSize,
		maxSize: maxSize,
	}
	for i := range p.cached {
		p.cached[i].Init(nil)
	}
	return p
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
	if idx < len(p.cached) {
		return p.cached[idx].Get(), n
	}
	if v := p.pooled[idx].Get(); v != nil {
		return v.(T), n
	}
	var zero T
	return zero, n
}

// Put retains x only when size names one of this pool's exact size classes.
func (p *Pool[T]) Put(x T, size int) {
	if size < p.minSize || size > p.maxSize || !IsPowerOfTwo(size) {
		return
	}
	idx := classIndex(size) - classIndex(p.minSize)
	if idx < len(p.cached) {
		p.cached[idx].Put(x)
		return
	}
	p.pooled[idx].Put(x)
}
