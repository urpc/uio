package writecontrol

import "testing"

type testAccess = Access[testGetter]

type testBoundary struct {
	testAccess
	control *Control
}

func (b *testBoundary) boundaryControl() *Control { return b.control }

type testGetter struct{}

func (testGetter) GetWriteControl(source any) *Control {
	if boundary, ok := source.(interface{ boundaryControl() *Control }); ok {
		return boundary.boundaryControl()
	}
	return nil
}

func TestAccessResolvesTheSameBoundaryThroughWrappers(t *testing.T) {
	if ControlOf(struct{}{}) != nil {
		t.Fatal("unsupported source returned control")
	}
	if Watch(struct{}{}, func(int) {}) {
		t.Fatal("Watch succeeded on unsupported source")
	}
	var getter testGetter
	if getter.GetWriteControl(123) != nil {
		t.Fatal("unexpected control for non-boundary source")
	}

	control := New(false)
	boundary := &testBoundary{control: control}
	wrapped := &struct{ *testBoundary }{boundary}
	if ControlOf(boundary) != control || ControlOf(wrapped) != control {
		t.Fatal("capability did not preserve the underlying boundary")
	}
	if Watch(boundary, nil) {
		t.Fatal("Watch succeeded with nil callback")
	}
	var sumA, sumB int
	if !Watch(boundary, func(n int) { sumA += n }) {
		t.Fatal("Watch on boundary failed")
	}
	if !Watch(wrapped, func(n int) { sumB += n * 2 }) {
		t.Fatal("Watch on wrapper failed")
	}
	control.Notify(7)
	if sumA != 7 || sumB != 14 {
		t.Fatalf("sums = (%d, %d), want (7, 14)", sumA, sumB)
	}
}
