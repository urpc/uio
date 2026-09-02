//go:build !windows

package fdmap

import (
	"errors"
	"testing"
)

func TestMapClear(t *testing.T) {
	mapping := NewMap[int]()
	first, second := 1, 2
	if err := mapping.Put(0, &first); err != nil {
		t.Fatal(err)
	}
	if err := mapping.Put(2, &second); err != nil {
		t.Fatal(err)
	}
	mapping.Clear()
	if mapping.Get(0) != nil || mapping.Get(2) != nil {
		t.Fatal("Clear left an entry")
	}
	for range mapping.Range() {
		t.Fatal("Range returned an entry after Clear")
	}
}

func TestMapRejectsOutOfRangeDescriptors(t *testing.T) {
	mapping := NewMap[int]()
	value := 1
	for _, fd := range []int{-1, len(mapping.store), len(mapping.store) + 100} {
		if err := mapping.Put(fd, &value); !errors.Is(err, ErrOutOfRange) {
			t.Fatalf("Put(%d) error = %v", fd, err)
		}
		if got := mapping.Get(fd); got != nil {
			t.Fatalf("Get(%d) = %v, want nil", fd, got)
		}
		mapping.Delete(fd)
	}
}
