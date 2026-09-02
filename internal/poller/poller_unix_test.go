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

func (handler *unixTestHandler) OnEvent(_ *NetPoller, fd int, events Events) {
	handler.mu.Lock()
	handler.fds = append(handler.fds, fd)
	handler.masks = append(handler.masks, events)
	handler.mu.Unlock()
	var buffer [1]byte
	_, _ = unix.Read(fd, buffer[:])
	handler.eventCh <- struct{}{}
}

func (handler *unixTestHandler) OnClose(_ *NetPoller, err error) {
	handler.closeCh <- err
}

func TestNetPollerRegistrationAndDispatch(t *testing.T) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fds[0])
	defer unix.Close(fds[1])

	poller, err := NewNetPoller()
	if err != nil {
		t.Fatal(err)
	}
	if err = poller.AddReadWrite(fds[0]); err != nil {
		t.Fatal(err)
	}
	if err = poller.ModReadWrite(fds[0]); err != nil {
		t.Fatal(err)
	}
	if err = poller.ModWrite(fds[0]); err != nil {
		t.Fatal(err)
	}
	if err = poller.ModRead(fds[0]); err != nil {
		t.Fatal(err)
	}
	if err = poller.AddRead(-1); err == nil {
		t.Fatal("AddRead(-1) succeeded")
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

func TestNetPollerCloseBeforeServe(t *testing.T) {
	poller, err := NewNetPoller()
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
