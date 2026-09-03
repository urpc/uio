//go:build !windows

package fdmap

import (
	"errors"
	"testing"
)

func TestOpenFileCapacity(t *testing.T) {
	for _, test := range []struct {
		limit uint64
		want  int
	}{
		{limit: 0, want: maxOpenFilesCeiling},
		{limit: 1024, want: 1024},
		{limit: maxOpenFilesCeiling, want: maxOpenFilesCeiling},
		{limit: maxOpenFilesCeiling * 2, want: maxOpenFilesCeiling},
	} {
		if got := openFileCapacity(test.limit); got != test.want {
			t.Fatalf("openFileCapacity(%d) = %d, want %d", test.limit, got, test.want)
		}
	}
}

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
