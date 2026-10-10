//go:build (linux || darwin || netbsd || freebsd || openbsd || dragonfly) && !stdio

package poller

import (
	"errors"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

type unixTestHandler struct {
	mu      sync.Mutex
	fds     []int
	masks   []Events
	eventCh chan struct{}
	closeCh chan error
}

func newUnixTestHandler() *unixTestHandler {
	return &unixTestHandler{eventCh: make(chan struct{}, 4), closeCh: make(chan error, 1)}
}

func (handler *unixTestHandler) OnEvent(_ *Poller, fd int, events Events) {
	handler.mu.Lock()
	handler.fds = append(handler.fds, fd)
	handler.masks = append(handler.masks, events)
	handler.mu.Unlock()
	var buffer [1]byte
	_, _ = unix.Read(fd, buffer[:])
	handler.eventCh <- struct{}{}
}

func (handler *unixTestHandler) OnClose(_ *Poller, err error) {
	handler.closeCh <- err
}

func TestPollerRegistrationAndDispatch(t *testing.T) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fds[0])
	defer unix.Close(fds[1])

	poller, err := New()
	if err != nil {
		t.Fatal(err)
	}
	if err = poller.Add(fds[0], Readable|Writable); err != nil {
		t.Fatal(err)
	}
	if err = poller.Modify(fds[0], Readable|Writable, Readable|Writable); err != nil {
		t.Fatal(err)
	}
	if err = poller.Modify(fds[0], Readable|Writable, Writable); err != nil {
		t.Fatal(err)
	}
	if err = poller.Modify(fds[0], Writable, Readable); err != nil {
		t.Fatal(err)
	}
	if err = poller.Add(-1, Readable); err == nil {
		t.Fatal("Add(-1) succeeded")
	}

	handler := newUnixTestHandler()
	serveDone := make(chan error, 1)
	go func() { serveDone <- poller.Serve(true, handler) }()
	if _, err = unix.Write(fds[1], []byte("x")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-handler.eventCh:
	case <-time.After(time.Second):
		t.Fatal("poller did not dispatch readable event")
	}
	handler.mu.Lock()
	gots := append([]int(nil), handler.fds...)
	masks := append([]Events(nil), handler.masks...)
	handler.mu.Unlock()
	if len(gots) == 0 || gots[0] != fds[0] || masks[0]&ReadEvents == 0 {
		t.Fatalf("events = fds:%v masks:%v, want readable event for fd %d", gots, masks, fds[0])
	}

	closeErr := errors.New("poller closed")
	if err = poller.Close(closeErr); err != nil {
		t.Fatal(err)
	}
	if err = poller.Close(errors.New("ignored")); err != nil {
		t.Fatal(err)
	}
	select {
	case gotErr := <-handler.closeCh:
		if !errors.Is(gotErr, closeErr) {
			t.Fatalf("OnClose error = %v, want %v", gotErr, closeErr)
		}
	case <-time.After(time.Second):
		t.Fatal("poller did not call OnClose")
	}
	select {
	case gotErr := <-serveDone:
		if !errors.Is(gotErr, closeErr) {
			t.Fatalf("Serve error = %v, want %v", gotErr, closeErr)
		}
	case <-time.After(time.Second):
		t.Fatal("Serve did not return")
	}
}

func TestPollerCloseBeforeServe(t *testing.T) {
	poller, err := New()
	if err != nil {
		t.Fatal(err)
	}
	closeErr := errors.New("closed before serve")
	if err = poller.Close(closeErr); err != nil {
		t.Fatal(err)
	}
	handler := newUnixTestHandler()
	if err = poller.Serve(false, handler); !errors.Is(err, closeErr) {
		t.Fatalf("Serve error = %v, want %v", err, closeErr)
	}
	select {
	case gotErr := <-handler.closeCh:
		if !errors.Is(gotErr, closeErr) {
			t.Fatalf("OnClose error = %v, want %v", gotErr, closeErr)
		}
	case <-time.After(time.Second):
		t.Fatal("Serve did not call OnClose")
	}
}

func TestPollerInterestLifecycleAndWake(t *testing.T) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fds[0])
	defer unix.Close(fds[1])
	poller, err := New()
	if err != nil {
		t.Fatal(err)
	}
	defer poller.Close(nil)

	if err = poller.Add(fds[0], Readable); err != nil {
		t.Fatal(err)
	}
	if err = poller.Remove(fds[0], Readable); err != nil {
		t.Fatal(err)
	}
	if err = poller.Remove(fds[0], Readable); err != nil {
		t.Fatalf("idempotent Remove failed: %v", err)
	}
	if err = poller.Add(fds[0], Readable); err != nil {
		t.Fatal(err)
	}

	waitDone := make(chan struct {
		n   int
		err error
	}, 1)
	go func() {
		var events [1]Event
		n, waitErr := poller.Wait(events[:], -1)
		waitDone <- struct {
			n   int
			err error
		}{n: n, err: waitErr}
	}()
	if err = poller.Wake(); err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-waitDone:
		if result.n != 0 || result.err != nil {
			t.Fatalf("Wait after Wake = %d, %v", result.n, result.err)
		}
	case <-time.After(time.Second):
		t.Fatal("Wake did not unblock Wait")
	}
}

func TestPollerValidationAndClosedOperations(t *testing.T) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fds[0])
	defer unix.Close(fds[1])
	poller, err := New()
	if err != nil {
		t.Fatal(err)
	}
	if err = poller.Add(fds[0], 0); !errors.Is(err, errInvalidInterest) {
		t.Fatalf("Add empty interest error = %v", err)
	}
	if err = poller.closeError(); err != nil {
		t.Fatalf("closeError before Close = %v", err)
	}
	if err = poller.closedError(); !errors.Is(err, unix.EBADF) {
		t.Fatalf("closedError without reason = %v", err)
	}
	closeErr := errors.New("closed operations")
	if err = poller.Close(closeErr); err != nil {
		t.Fatal(err)
	}
	if !poller.Closed() {
		t.Fatal("Closed returned false")
	}
	if err = poller.Add(fds[0], Readable); !errors.Is(err, closeErr) {
		t.Fatalf("Add after Close error = %v", err)
	}
	if err = poller.Remove(fds[0], Readable); err != nil {
		t.Fatalf("Remove after Close error = %v", err)
	}
	if err = poller.Wake(); err != nil {
		t.Fatalf("Wake after Close error = %v", err)
	}
	var events [1]Event
	if n, waitErr := poller.Wait(events[:], 0); n != 0 || !errors.Is(waitErr, closeErr) {
		t.Fatalf("Wait after Close = %d, %v", n, waitErr)
	}
}

func TestPollerWaitModesAndEventMasks(t *testing.T) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fds[0])
	defer unix.Close(fds[1])
	poller, err := New()
	if err != nil {
		t.Fatal(err)
	}
	defer poller.Close(nil)
	if err = poller.Add(fds[0], Readable); err != nil {
		t.Fatal(err)
	}
	var events [1]Event
	if n, waitErr := poller.Wait(events[:], 0); n != 0 || waitErr != nil {
		t.Fatalf("zero-timeout Wait = %d, %v", n, waitErr)
	}
	if _, err = unix.Write(fds[1], []byte("x")); err != nil {
		t.Fatal(err)
	}
	if n, waitErr := poller.Wait(nil, 100); n != 0 || waitErr != nil {
		t.Fatalf("Wait with no output capacity = %d, %v", n, waitErr)
	}
	if n, waitErr := poller.Wait(events[:], 100); n != 1 || waitErr != nil || events[0].Events&ReadEvents == 0 {
		t.Fatalf("readable Wait = %#v, %v", events[:n], waitErr)
	}
	var buffer [1]byte
	_, _ = unix.Read(fds[0], buffer[:])
	if err = poller.Modify(fds[0], Readable, Readable|Writable); err != nil {
		t.Fatal(err)
	}
	if n, waitErr := poller.Wait(events[:], 100); n != 1 || waitErr != nil || events[0].Events&WriteEvents == 0 {
		t.Fatalf("writable Wait = %#v, %v", events[:n], waitErr)
	}
	if err = poller.Remove(fds[0], Readable|Writable); err != nil {
		t.Fatal(err)
	}
}

// Edge-triggered readers stop at a short read unless a hangup is reported, so
// the bit must come with the peer's end of stream, also when it is queued
// behind bytes, and never with bytes alone, which would cost every read round
// a syscall.
func TestPollerReportsHangupApartFromData(t *testing.T) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fds[0])
	defer unix.Close(fds[1])
	poller, err := New()
	if err != nil {
		t.Fatal(err)
	}
	defer poller.Close(nil)
	if err = poller.Register(fds[0], Readable, true, 0); err != nil {
		t.Fatal(err)
	}
	wait := func() Events {
		t.Helper()
		var events [4]Event
		n, waitErr := poller.Wait(events[:], 1000)
		if n != 1 || waitErr != nil || events[0].FD != fds[0] {
			t.Fatalf("Wait = %#v, %v; want one event for fd %d", events[:n], waitErr, fds[0])
		}
		return events[0].Events
	}
	if _, err = unix.Write(fds[1], []byte("x")); err != nil {
		t.Fatal(err)
	}
	if got := wait(); got&ReadEvents == 0 || got&HangupEvents != 0 {
		t.Fatalf("bytes alone reported %b, want ReadEvents without HangupEvents", got)
	}
	var buffer [8]byte
	if _, err = unix.Read(fds[0], buffer[:]); err != nil {
		t.Fatal(err)
	}
	if _, err = unix.Write(fds[1], []byte("y")); err != nil {
		t.Fatal(err)
	}
	if err = unix.Shutdown(fds[1], unix.SHUT_WR); err != nil {
		t.Fatal(err)
	}
	if got := wait(); got&(ReadEvents|HangupEvents) != ReadEvents|HangupEvents {
		t.Fatalf("bytes followed by end of stream reported %b, want ReadEvents|HangupEvents", got)
	}
}

// Several goroutines wait on one poller, parked and blocking alike: an edge
// reaches one of them, and Close returns every one.
func TestPollerSharedByWaiters(t *testing.T) {
	for _, parked := range []bool{false, true} {
		poller, err := New()
		if err != nil {
			t.Fatal(err)
		}
		pipe := make([]int, 2)
		if err := unix.Pipe(pipe); err != nil {
			t.Fatal(err)
		}
		_ = unix.SetNonblock(pipe[0], true)
		if err := poller.Register(pipe[0], Readable, true, 7); err != nil {
			t.Fatal(err)
		}
		events := make(chan Event, 16)
		var waiters sync.WaitGroup
		for range 4 {
			waiters.Add(1)
			go func() {
				defer waiters.Done()
				var batch Batch
				out := make([]Event, 8)
				for {
					var n int
					var err error
					if parked {
						n, err = poller.WaitBatchParked(&batch, out)
					} else {
						n, err = poller.WaitBatch(&batch, out, -1)
					}
					if poller.Closed() || err != nil {
						return
					}
					for _, event := range out[:n] {
						events <- event
					}
				}
			}()
		}
		if _, err := unix.Write(pipe[1], []byte{1}); err != nil {
			t.Fatal(err)
		}
		select {
		case event := <-events:
			if event.FD != pipe[0] || event.Events&ReadEvents == 0 {
				t.Fatalf("parked=%v event=%+v", parked, event)
			}
			if Tagged && event.Tag != 7 {
				t.Fatalf("parked=%v tag=%d", parked, event.Tag)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("parked=%v: no waiter saw the edge", parked)
		}
		_ = poller.Close(nil)
		done := make(chan struct{})
		go func() { waiters.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatalf("parked=%v: Close left a waiter asleep", parked)
		}
		_ = unix.Close(pipe[0])
		_ = unix.Close(pipe[1])
	}
}
