package fdmap

import "testing"

func TestMapPutGetDeleteRangeAndClear(t *testing.T) {
	mapping := NewMap[string]()
	first := "first"
	second := "second"
	if err := mapping.Put(0, &first); err != nil {
		t.Fatal(err)
	}
	if err := mapping.Put(2, &second); err != nil {
		t.Fatal(err)
	}

	if got := mapping.Get(0); got == nil || *got != first {
		t.Fatalf("Get(0) = %v, want %q", got, first)
	}
	if got := mapping.Get(1); got != nil {
		t.Fatalf("Get(1) = %v, want nil", got)
	}

	seen := map[int]string{}
	for key, value := range mapping.Range() {
		seen[key] = *value
	}
	if len(seen) != 2 || seen[0] != first || seen[2] != second {
		t.Fatalf("Range() = %#v, want both entries", seen)
	}

	mapping.Delete(0)
	if got := mapping.Get(0); got != nil {
		t.Fatalf("Get(0) after Delete = %v, want nil", got)
	}

	var stopped int
	for key := range mapping.Range() {
		stopped = key
		break
	}
	if stopped != 2 {
		t.Fatalf("Range() after Delete visited key %d, want 2", stopped)
	}

}
