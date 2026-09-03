//go:build windows || stdio

package poller

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

type stdTestHandler struct {
	eventCh chan stdEvent
	closeCh chan error
}

type stdEvent struct {
	fd     int
	events Events
}

func (handler *stdTestHandler) OnEvent(_ *NetPoller, fd int, events Events) {
	handler.eventCh <- stdEvent{fd: fd, events: events}
}

func (handler *stdTestHandler) OnClose(_ *NetPoller, err error) { handler.closeCh <- err }

func TestStdPollerInterestAndClose(t *testing.T) {
	poller, err := NewNetPoller()
	if err != nil {
		t.Fatal(err)
	}
	handler := &stdTestHandler{eventCh: make(chan stdEvent, 1), closeCh: make(chan error, 1)}
	serveDone := make(chan error, 1)
	go func() { serveDone <- poller.Serve(true, handler) }()

	addDone := make(chan error, 1)
	go func() { addDone <- poller.Add(42, Readable) }()
	select {
	case err = <-addDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Add did not complete")
	}
	if err = poller.Modify(42, Readable, Readable); err != nil {
		t.Fatal(err)
	}
	if err = poller.Modify(42, Readable, Writable); err != nil {
		t.Fatal(err)
	}
	if err = poller.Modify(42, Writable, Readable|Writable); err != nil {
		t.Fatal(err)
	}

	closeErr := errors.New("std poller closed")
	if err = poller.Close(closeErr); err != nil {
		t.Fatal(err)
	}
	if err = poller.Close(errors.New("ignored")); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-handler.closeCh:
		if !errors.Is(got, closeErr) {
			t.Fatalf("OnClose error = %v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("OnClose was not called")
	}
	select {
	case got := <-serveDone:
		if !errors.Is(got, closeErr) {
			t.Fatalf("Serve error = %v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("Serve did not stop")
	}
	if err = poller.Add(99, Readable); err == nil {
		t.Fatal("Add succeeded after Close")
	}
}

func TestStdPollerCloseBeforeServe(t *testing.T) {
	poller, err := NewNetPoller()
	if err != nil {
		t.Fatal(err)
	}
	closeErr := errors.New("closed before serve")
	if err = poller.Close(closeErr); err != nil {
		t.Fatal(err)
	}
	handler := &stdTestHandler{eventCh: make(chan stdEvent, 1), closeCh: make(chan error, 1)}
	if err = poller.Serve(false, handler); !errors.Is(err, closeErr) {
		t.Fatalf("Serve error = %v", err)
	}
	if got := <-handler.closeCh; !errors.Is(got, closeErr) {
		t.Fatalf("OnClose error = %v", got)
	}
}

func TestStdPollerWaitTimeout(t *testing.T) {
	poller, err := NewNetPoller()
	if err != nil {
		t.Fatal(err)
	}
	defer poller.Close(nil)
	started := time.Now()
	var events [1]Event
	if n, waitErr := poller.Wait(events[:], 10); waitErr != nil || n != 0 {
		t.Fatalf("Wait = %d, %v", n, waitErr)
	}
	if elapsed := time.Since(started); elapsed < 5*time.Millisecond {
		t.Fatalf("Wait returned too early after %v", elapsed)
	}
}

func TestStdPollerWaitModesAndValidation(t *testing.T) {
	poller, err := NewNetPoller()
	if err != nil {
		t.Fatal(err)
	}
	defer poller.Close(nil)
	if poller.Closed() {
		t.Fatal("new poller is closed")
	}
	if err = poller.Add(1, 0); !errors.Is(err, errInvalidInterest) {
		t.Fatalf("Add empty interest error = %v", err)
	}
	var events [1]Event
	if n, waitErr := poller.Wait(events[:], 0); n != 0 || waitErr != nil {
		t.Fatalf("empty zero-timeout Wait = %d, %v", n, waitErr)
	}
	if err = poller.Wake(); err != nil {
		t.Fatal(err)
	}
	if err = poller.Wake(); err != nil {
		t.Fatal(err)
	}
	if n, waitErr := poller.Wait(events[:], 0); n != 0 || waitErr != nil {
		t.Fatalf("wake zero-timeout Wait = %d, %v", n, waitErr)
	}
	if err = poller.Add(1, Readable|Writable); err != nil {
		t.Fatal(err)
	}
	if err = poller.Modify(1, Readable|Writable, Writable); err != nil {
		t.Fatal(err)
	}
	if n, waitErr := poller.Wait(nil, 0); n != 0 || waitErr != nil {
		t.Fatalf("watch generated an event: %d, %v", n, waitErr)
	}
	if err = poller.Remove(1, Writable); err != nil {
		t.Fatal(err)
	}
	if err = poller.Add(2, Readable); err != nil {
		t.Fatal(err)
	}
	if n, waitErr := poller.Wait(events[:], 10); n != 0 || waitErr != nil {
		t.Fatalf("positive-timeout Wait = %d, %v", n, waitErr)
	}
	if err = poller.Wake(); err != nil {
		t.Fatal(err)
	}
	if n, waitErr := poller.Wait(events[:], 100); n != 0 || waitErr != nil {
		t.Fatalf("positive-timeout wake Wait = %d, %v", n, waitErr)
	}
}

func TestStdPollerClosedWaitModes(t *testing.T) {
	for _, timeout := range []int{0, 10, -1} {
		t.Run(fmt.Sprintf("timeout_%d", timeout), func(t *testing.T) {
			poller, err := NewNetPoller()
			if err != nil {
				t.Fatal(err)
			}
			closeErr := errors.New("closed wait")
			if err = poller.Close(closeErr); err != nil {
				t.Fatal(err)
			}
			var events [1]Event
			if n, waitErr := poller.Wait(events[:], timeout); n != 0 || !errors.Is(waitErr, closeErr) {
				t.Fatalf("Wait = %d, %v", n, waitErr)
			}
		})
	}
}

func TestStdPollerBurstAddDoesNotBlock(t *testing.T) {
	poller, err := NewNetPoller()
	if err != nil {
		t.Fatal(err)
	}
	defer poller.Close(nil)
	const total = 4096
	result := make(chan error, 1)
	go func() {
		for fd := 0; fd < total; fd++ {
			if watchErr := poller.Add(fd, Readable); watchErr != nil {
				result <- watchErr
				return
			}
		}
		result <- nil
	}()
	select {
	case watchErr := <-result:
		if watchErr != nil {
			t.Fatal(watchErr)
		}
	case <-time.After(time.Second):
		t.Fatal("burst Add blocked without a waiter")
	}
}

func TestStdCallerOwnedInterestTransitions(t *testing.T) {
	poller, err := NewNetPoller()
	if err != nil {
		t.Fatal(err)
	}
	defer poller.Close(nil)
	if err = poller.Add(1, Readable); err != nil {
		t.Fatal(err)
	}
	if err = poller.Modify(1, Readable, Readable|Writable); err != nil {
		t.Fatal(err)
	}
	if err = poller.Remove(1, Readable|Writable); err != nil {
		t.Fatal(err)
	}
}
