package readcontrol

import (
	"errors"
	"testing"
)

type testAccess = Access[testGetter]

type testBoundary struct {
	testAccess
	control *Control
}

func TestIdleResumeKeepsForeignPauseOwners(t *testing.T) {
	resumes := 0
	control := New(nil, func() error { resumes++; return nil })
	foreign := control.NewPause()
	if err := foreign.Pause(); err != nil {
		t.Fatal(err)
	}
	for range 10 {
		if err := control.ResumeRead(); err != nil {
			t.Fatal(err)
		}
	}
	if !foreign.Paused() || !control.ReadPaused() || resumes != 0 {
		t.Fatal("an idle application resume bypassed a foreign owner")
	}
	if err := foreign.Resume(); err != nil {
		t.Fatal(err)
	}
	if control.ReadPaused() || resumes != 1 {
		t.Fatal("the foreign owner's final release did not schedule a turn")
	}
}

func (b *testBoundary) boundaryControl() *Control { return b.control }

type testGetter struct{}

func (testGetter) GetReadControl(source any) *Control {
	if boundary, ok := source.(interface{ boundaryControl() *Control }); ok {
		return boundary.boundaryControl()
	}
	return nil
}

// A wrapper receives its own owner of the same logical input, rather than a
// new controller detached from the application's pause or the underlying I/O.
func TestAccessResolvesTheSameBoundaryThroughWrappers(t *testing.T) {
	resumes := 0
	control := New(nil, func() error { resumes++; return nil })
	boundary := &testBoundary{control: control}
	wrapped := &struct{ *testBoundary }{boundary}
	if ControlOf(boundary) != control || ControlOf(wrapped) != control {
		t.Fatal("capability did not preserve the underlying boundary")
	}
	first, second := NewPause(boundary), NewPause(wrapped)
	for _, pause := range []func() error{control.PauseRead, first.Pause, second.Pause} {
		if err := pause(); err != nil {
			t.Fatal(err)
		}
	}
	for _, resume := range []func() error{control.ResumeRead, first.Resume} {
		if err := resume(); err != nil {
			t.Fatal(err)
		}
	}
	if !control.ReadPaused() || !second.Paused() || resumes != 0 {
		t.Fatal("one owner released another owner's pause")
	}
	if err := second.Resume(); err != nil {
		t.Fatal(err)
	}
	if control.ReadPaused() || resumes != 1 {
		t.Fatal("last owner did not release and schedule a resume")
	}
}

func TestAccessRejectsUnrelatedSources(t *testing.T) {
	for _, source := range []any{nil, &struct{}{}, New(nil, nil)} {
		if ControlOf(source) != nil || NewPause(source) != nil {
			t.Fatalf("unrelated source %T acquired an owner", source)
		}
	}
	var unsupported *Pause
	if !errors.Is(unsupported.Pause(), ErrUnsupported) || !errors.Is(unsupported.Resume(), ErrUnsupported) || unsupported.Paused() {
		t.Fatal("an unavailable owner must consistently report unsupported")
	}
}

func TestCheckPreservesLifecycleErrorAndPauseState(t *testing.T) {
	closed := errors.New("boundary closed")
	var lifecycleErr error
	control := New(func() error { return lifecycleErr }, nil)
	if err := control.PauseRead(); err != nil {
		t.Fatal(err)
	}
	lifecycleErr = closed
	if err := control.Check(); !errors.Is(err, closed) {
		t.Fatalf("Check = %v, want lifecycle error", err)
	}
	if !control.ReadPaused() {
		t.Fatal("lifecycle check changed pause state")
	}
	lifecycleErr = nil
	if err := control.Check(); err != nil {
		t.Fatal(err)
	}
	if !control.ReadPaused() {
		t.Fatal("successful check released a pause")
	}
	var missing *Control
	if !errors.Is(missing.Check(), ErrUnsupported) {
		t.Fatal("nil controller Check must report unsupported")
	}
}
