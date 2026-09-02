package uio

import "testing"

func TestCurrentGoroutineIDIsStableAndAllocationFree(t *testing.T) {
	want := currentGoroutineID()
	if want == 0 {
		t.Fatal("current goroutine ID is zero")
	}
	allocations := testing.AllocsPerRun(1000, func() {
		if got := currentGoroutineID(); got != want {
			panic("goroutine ID changed")
		}
	})
	if allocations != 0 {
		t.Fatalf("currentGoroutineID allocations = %v", allocations)
	}
}
