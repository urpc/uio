//go:build !windows

package fdmap

import "testing"

func TestMapClear(t *testing.T) {
	mapping := NewMap[int]()
	first, second := 1, 2
	mapping.Put(0, &first)
	mapping.Put(2, &second)
	mapping.Clear()
	if mapping.Get(0) != nil || mapping.Get(2) != nil {
		t.Fatal("Clear left an entry")
	}
	for range mapping.Range() {
		t.Fatal("Range returned an entry after Clear")
	}
}
