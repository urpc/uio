//go:build windows || stdio

package poller

import (
	"errors"
	"testing"
)

func TestStdRegisterAndBlockedWakeLifecycle(t *testing.T) {
	p, err := New()
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Register(1, 0, true, 7); !errors.Is(err, errInvalidInterest) {
		t.Fatal(err)
	}
	if err := p.Register(1, Readable, true, 7); err != nil {
		t.Fatal(err)
	}
	if err := p.Wake(); err != nil {
		t.Fatal(err)
	}
	if n, err := p.Wait(nil, -1); n != 0 || err != nil {
		t.Fatalf("blocking wake = %d, %v", n, err)
	}
	cause := errors.New("registration closed")
	_ = p.Close(cause)
	if !p.Closed() {
		t.Fatal("close did not publish terminal state")
	}
	if err := p.Register(1, Readable, false, 0); err == nil {
		t.Fatal("closed poller accepted a registration")
	}
}
