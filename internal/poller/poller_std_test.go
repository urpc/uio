//go:build windows || stdio

package poller

import (
	"errors"
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

func TestStdPollerDispatchAndClose(t *testing.T) {
	poller, err := NewNetPoller()
	if err != nil {
		t.Fatal(err)
	}
	handler := &stdTestHandler{eventCh: make(chan stdEvent, 1), closeCh: make(chan error, 1)}
	serveDone := make(chan error, 1)
	go func() { serveDone <- poller.Serve(true, handler) }()

	addDone := make(chan error, 1)
	go func() { addDone <- poller.AddRead(42) }()
	select {
	case err = <-addDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("AddRead did not complete")
	}
	select {
	case event := <-handler.eventCh:
		if event.fd != 42 || event.events != ReadEvents|WriteEvents {
			t.Fatalf("event = %#v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("Serve did not dispatch event")
	}
	if err = poller.ModRead(42); err != nil {
		t.Fatal(err)
	}
	if err = poller.ModWrite(42); err != nil {
		t.Fatal(err)
	}
	if err = poller.ModReadWrite(42); err != nil {
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
	if err = poller.AddRead(99); err == nil {
		t.Fatal("AddRead succeeded after Close")
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
