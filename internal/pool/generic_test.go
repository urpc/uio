package pool

import "testing"

func TestGenericPoolGet(t *testing.T) {
	for _, test := range []struct {
		name     string
		min, max int
		get      int
		expSize  int
	}{
		{
			max:     32,
			get:     10,
			expSize: 16,
		},
		{
			max:     16,
			get:     10,
			expSize: 16,
		},
		{max: 65536, get: 1, expSize: 512},
		{max: 65536, get: 512, expSize: 512},
		{max: 65536, get: 513, expSize: 1024},
		{max: 65536, get: 65536, expSize: 65536},
	} {
		t.Run(test.name, func(t *testing.T) {
			p := New[struct{}](test.max)
			_, n := p.Get(test.get)
			if n != test.expSize {
				t.Errorf("Get(%d) = _, %d; want %d", test.get, n, test.expSize)
			}
		})
	}
}

func TestGenericPoolPut(t *testing.T) {
	p := New[*int](65536)
	value := 42
	p.Put(&value, 512)
	got, size := p.Get(10)
	if size != 512 {
		t.Fatalf("Get size after Put = %d, want 512", size)
	}
	// sync.Pool may discard cached values at any garbage collection.
	if got != nil && *got != value {
		t.Fatalf("Get after Put = %d, want %d or nil", *got, value)
	}

	// Objects below the pool's minimum bucket are intentionally not retained.
	discardingPool := New[*int](65536)
	discardingPool.Put(&value, 1)
	if got, _ := discardingPool.Get(1); got != nil {
		t.Fatal("Put accepted an undersized bucket")
	}
}
