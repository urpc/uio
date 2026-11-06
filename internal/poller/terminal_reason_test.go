//go:build !windows

package poller

import (
	"errors"
	"testing"
	"time"
)

type terminalReasonHandler struct{ closed chan error }

func (terminalReasonHandler) OnEvent(*Poller, int, Events)   {}
func (h terminalReasonHandler) OnClose(_ *Poller, err error) { h.closed <- err }

func TestQueuedWakeCannotHidePublishedTerminalReason(t *testing.T) {
	for _, cause := range []error{nil, errors.New("terminal poller failure")} {
		for _, timeout := range []int{0, 10, -1} {
			p, err := New()
			if err != nil {
				t.Fatal(err)
			}
			if p.Err() != nil || p.Closed() {
				t.Fatal("new poller has a terminal reason")
			}
			if err := p.Wake(); err != nil {
				t.Fatal(err)
			}
			if err := p.Close(cause); err != nil {
				t.Fatal(err)
			}
			if n, err := p.Wait(nil, timeout); n != 0 || !errors.Is(err, cause) {
				t.Fatalf("queued wake after close=%d,%v, want %v", n, err, cause)
			}
			if !p.Closed() || !errors.Is(p.Err(), cause) {
				t.Fatalf("terminal snapshot=%v,%v", p.Closed(), p.Err())
			}
			h := terminalReasonHandler{closed: make(chan error, 1)}
			if err := p.Serve(false, h); !errors.Is(err, cause) {
				t.Fatalf("terminal Serve=%v want %v", err, cause)
			}
			if err := <-h.closed; !errors.Is(err, cause) {
				t.Fatalf("terminal callback=%v want %v", err, cause)
			}
		}
	}
}

func TestWakeRacingClosePreservesServeCause(t *testing.T) {
	for i := range 64 {
		p, err := New()
		if err != nil {
			t.Fatal(err)
		}
		cause := errors.New("concurrent close")
		h := terminalReasonHandler{closed: make(chan error, 1)}
		done := make(chan error, 1)
		go func() { done <- p.Serve(i%2 == 0, h) }()
		if err := p.Wake(); err != nil {
			t.Fatal(err)
		}
		if err := p.Close(cause); err != nil {
			t.Fatal(err)
		}
		select {
		case err := <-done:
			if !errors.Is(err, cause) {
				t.Fatalf("Serve lost close cause: %v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("Serve did not observe terminal close")
		}
		if err := <-h.closed; !errors.Is(err, cause) {
			t.Fatalf("callback lost close cause: %v", err)
		}
	}

	p, err := New()
	if err != nil {
		t.Fatal(err)
	}
	cause := errors.New("parked serve close")
	h := terminalReasonHandler{closed: make(chan error, 1)}
	done := make(chan error, 1)
	go func() { done <- p.Serve(true, h) }()
	time.Sleep(10 * time.Millisecond)
	_ = p.Close(cause)
	if err := <-done; !errors.Is(err, cause) {
		t.Fatalf("parked Serve = %v, want %v", err, cause)
	}
}
