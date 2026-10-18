package fdmap

import (
	"runtime"
	"sync"
	"testing"
)

// sink defeats dead-code elimination of the map calls.
var sink *int

func capacity(n int) int { return min(n, MaxOpenFiles) }

// fill publishes n contiguous descriptors and returns their values.
func fill(m *Map[int], n int) []*int {
	vals := make([]*int, n)
	for i := range vals {
		v := new(int)
		*v = i
		vals[i] = v
		mustPut(m, i, v)
	}
	return vals
}

func mustPut(m *Map[int], k int, v *int) {
	if err := m.Put(k, v); err != nil {
		panic(err)
	}
}

// BenchmarkGetSame measures the dispatch hot path: one descriptor, read over
// and over as one connection's events are.
func BenchmarkGetSame(b *testing.B) {
	m := NewMap[int]()
	fill(m, 1)
	b.ReportAllocs()
	b.ResetTimer()
	var s *int
	for i := 0; i < b.N; i++ {
		s = m.Get(0)
	}
	sink = s
}

// BenchmarkGetRoundRobin walks a table of descriptors, as a loop dispatching
// many connections does.
func BenchmarkGetRoundRobin(b *testing.B) {
	for _, n := range []int{capacity(1000), capacity(10000), capacity(100000)} {
		if n < 2 {
			continue
		}
		m := NewMap[int]()
		fill(m, n)
		b.Run(name(n), func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			var s *int
			for i := 0; i < b.N; i++ {
				s = m.Get(i % n)
			}
			sink = s
		})
	}
}

func name(n int) string {
	switch n {
	case 1000:
		return "1k"
	case 10000:
		return "10k"
	case 100000:
		return "100k"
	}
	return "n"
}

// BenchmarkGetParallel has every P dispatch its own slice of the table, as
// the event loops do.
func BenchmarkGetParallel(b *testing.B) {
	const n = 10000
	if capacity(n) < n {
		b.Skip("descriptor limit too small")
	}
	m := NewMap[int]()
	fill(m, n)
	workers := runtime.GOMAXPROCS(0)
	per := b.N / workers
	sinks := make([]*int, workers)
	b.ReportAllocs()
	b.ResetTimer()
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			lo := w * (n / workers)
			span := n / workers
			var s *int
			for i := 0; i < per; i++ {
				s = m.Get(lo + i%span)
			}
			sinks[w] = s
		}(w)
	}
	wg.Wait()
	sink = sinks[0]
}

// BenchmarkPutDeleteSame republishes one descriptor, as accept and close of
// the same fd slot do.
func BenchmarkPutDeleteSame(b *testing.B) {
	m := NewMap[int]()
	value := 1
	mustPut(m, 0, &value)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		mustPut(m, 0, &value)
		m.Delete(0)
	}
}

// BenchmarkPutDeleteParallel has every P churn its own slice, as loops
// accepting and closing connections concurrently do.
func BenchmarkPutDeleteParallel(b *testing.B) {
	const n = 10000
	if capacity(n) < n {
		b.Skip("descriptor limit too small")
	}
	m := NewMap[int]()
	values := fill(m, n)
	workers := runtime.GOMAXPROCS(0)
	per := b.N / workers
	b.ReportAllocs()
	b.ResetTimer()
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			lo := w * (n / workers)
			span := n / workers
			for i := 0; i < per; i++ {
				k := lo + i%span
				m.Delete(k)
				mustPut(m, k, values[k])
			}
		}(w)
	}
	wg.Wait()
}

// BenchmarkDeleteValue removes only while the slot still holds the value.
func BenchmarkDeleteValue(b *testing.B) {
	m := NewMap[int]()
	value := 1
	other := 2
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		mustPut(m, 7, &value)
		m.DeleteValue(7, &other) // no match, keeps it
		m.DeleteValue(7, &value) // matches
	}
}

// BenchmarkRange walks every live descriptor, as shutdown does once.
func BenchmarkRange(b *testing.B) {
	const n = 10000
	if capacity(n) < n {
		b.Skip("descriptor limit too small")
	}
	m := NewMap[int]()
	fill(m, n)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		count := 0
		for range m.Range() {
			count++
		}
		if count != n {
			b.Fatalf("Range saw %d of %d", count, n)
		}
	}
}
