package kcp

import (
	"math/rand"
	"slices"
	"testing"
)

// Model the engine's queues as ordinary slices while repeatedly wrapping and
// growing the ring. Both iterator directions must preserve FIFO ordering.
func TestRingBufferMatchesFIFOModelAcrossWrapAndGrowth(t *testing.T) {
	rng := rand.New(rand.NewSource(391))
	for _, capacity := range []int{0, 11, 1025} {
		r := NewRingBuffer[int](capacity)
		var model []int
		for step := 0; step < 5000; step++ {
			switch rng.Intn(7) {
			case 0, 1, 2:
				count := 1
				if step%37 == 0 {
					count = 1200
				}
				for range count {
					n := rng.Int()
					r.Push(n)
					model = append(model, n)
				}
			case 3:
				got, ok := r.Pop()
				if ok != (len(model) != 0) || (ok && got != model[0]) {
					t.Fatalf("Pop = %d,%v, model=%v", got, ok, model)
				}
				if ok {
					model = model[1:]
				}
			case 4:
				n := rng.Intn(len(model) + 3)
				if got := r.Discard(n); got != min(n, len(model)) {
					t.Fatalf("Discard(%d)=%d, length=%d", n, got, len(model))
				}
				model = model[min(n, len(model)):]
			case 5:
				r.Clear()
				model = nil
			case 6:
				if head, ok := r.Peek(); ok != (len(model) != 0) || (ok && *head != model[0]) {
					t.Fatal("Peek disagreed with the head of the model")
				}
			}
			if r.Len() != len(model) || r.IsEmpty() != (len(model) == 0) || r.MaxLen() < len(model) {
				t.Fatalf("ring length=%d capacity=%d, model=%d", r.Len(), r.MaxLen(), len(model))
			}
			var forward, reverse []int
			r.ForEach(func(p *int) bool { forward = append(forward, *p); return true })
			r.ForEachReverse(func(p *int) bool { reverse = append(reverse, *p); return true })
			slices.Reverse(reverse)
			if !slices.Equal(forward, model) || !slices.Equal(reverse, model) {
				t.Fatal("iteration changed FIFO order")
			}
			if len(model) != 0 {
				i := rng.Intn(len(model))
				if *r.At(i) != model[i] {
					t.Fatal("indexed lookup changed FIFO order")
				}
				limit := 1 + rng.Intn(len(model))
				for _, visit := range []func(func(*int) bool){r.ForEach, r.ForEachReverse} {
					count := 0
					visit(func(*int) bool { count++; return count < limit })
					if count != limit {
						t.Fatalf("iterator did not stop at %d: visited %d", limit, count)
					}
				}
			}
		}
	}
}

func TestRingBufferReleasesReferencesAndPoolRejectsWrongCapacity(t *testing.T) {
	r := NewRingBuffer[*int](8)
	for i := 0; i < 50; i++ {
		value := i
		r.Push(&value)
	}
	r.Discard(17)
	r.Clear()
	for _, p := range r.elements {
		if p != nil {
			t.Fatal("Clear retained an object removed from the queue")
		}
	}
	pool := newBufferPool(mtuLimit)
	p := pool.Get()
	if len(p) != mtuLimit || pool.Put(p[:3]) != nil {
		t.Fatal("pool did not accept a shortened correctly sized packet")
	}
	if err := pool.Put(make([]byte, mtuLimit-1)); err != errBufferSizeMismatch {
		t.Fatalf("wrong packet capacity = %v", err)
	}
}

func TestRingBufferWrappedIterationDiscardAndGrowth(t *testing.T) {
	wrapped := func() *RingBuffer[int] {
		r := NewRingBuffer[int](8)
		for i := 0; i < 7; i++ {
			r.Push(i)
		}
		for range 5 {
			r.Pop()
		}
		for i := 7; i < 11; i++ {
			r.Push(i)
		}
		return r // FIFO 5..10 crosses the end of the backing array
	}
	r := wrapped()
	for _, limit := range []int{1, 4, 6} {
		for _, reverse := range []bool{false, true} {
			var got []int
			visit := r.ForEach
			if reverse {
				visit = r.ForEachReverse
			}
			visit(func(p *int) bool { got = append(got, *p); return len(got) < limit })
			want := []int{5, 6, 7, 8, 9, 10}
			if reverse {
				slices.Reverse(want)
			}
			if !slices.Equal(got, want[:limit]) {
				t.Fatalf("wrapped iteration = %v, want %v", got, want[:limit])
			}
		}
	}
	if r.Discard(4) != 4 {
		t.Fatal("wrapped discard did not advance by four")
	}
	for _, want := range []int{9, 10} {
		if got, ok := r.Pop(); !ok || got != want {
			t.Fatalf("wrapped discard left %d,%v, want %d", got, ok, want)
		}
	}
	r = wrapped()
	r.Push(11)
	r.Push(12) // grow while the logical queue wraps around the old array
	for want := 5; want <= 12; want++ {
		if got, ok := r.Pop(); !ok || got != want {
			t.Fatal("wrapped growth changed FIFO order")
		}
	}
	r = wrapped()
	r.Clear()
	if !r.IsEmpty() {
		t.Fatal("wrapped clear retained elements")
	}
}
