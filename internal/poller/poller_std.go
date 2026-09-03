//go:build windows || stdio

package poller

import (
	"fmt"
	"runtime"
	"sync"
	"time"
)

type NetPoller struct {
	waker  chan struct{} // capacity one coalesces repeated wakeups
	closed chan struct{}

	mu          sync.Mutex
	closeReason error
	closeOnce   sync.Once
}

func NewNetPoller() (*NetPoller, error) {
	return &NetPoller{
		waker: make(chan struct{}, 1), closed: make(chan struct{}),
	}, nil
}

func (poller *NetPoller) Add(_ int, want Interest) error {
	return poller.validateInterest(want)
}

func (poller *NetPoller) Modify(_ int, _ Interest, want Interest) error {
	return poller.validateInterest(want)
}

func (poller *NetPoller) validateInterest(want Interest) error {
	if want == 0 {
		return errInvalidInterest
	}
	poller.mu.Lock()
	defer poller.mu.Unlock()
	select {
	case <-poller.closed:
		return fmt.Errorf("poller closed")
	default:
		return nil
	}
}

func (poller *NetPoller) Remove(int, Interest) error { return nil }

func (poller *NetPoller) Wait(_ []Event, timeout int) (int, error) {
	if timeout == 0 {
		select {
		case <-poller.closed:
			poller.mu.Lock()
			err := poller.closeReason
			poller.mu.Unlock()
			return 0, err
		case <-poller.waker:
			return 0, nil
		default:
			return 0, nil
		}
	}
	if timeout > 0 {
		timer := time.NewTimer(time.Duration(timeout) * time.Millisecond)
		defer timer.Stop()
		select {
		case <-poller.closed:
			poller.mu.Lock()
			err := poller.closeReason
			poller.mu.Unlock()
			return 0, err
		case <-poller.waker:
			return 0, nil
		case <-timer.C:
			return 0, nil
		}
	}
	select {
	case <-poller.closed:
		poller.mu.Lock()
		err := poller.closeReason
		poller.mu.Unlock()
		return 0, err
	case <-poller.waker:
		return 0, nil
	}
}

func (poller *NetPoller) Wake() error {
	select {
	case poller.waker <- struct{}{}:
	default:
	}
	return nil
}

func (poller *NetPoller) Close(err error) error {
	poller.closeOnce.Do(func() {
		poller.mu.Lock()
		poller.closeReason = err
		poller.mu.Unlock()
		close(poller.closed)
	})
	return nil
}

func (poller *NetPoller) Closed() bool {
	select {
	case <-poller.closed:
		return true
	default:
		return false
	}
}

func (poller *NetPoller) Serve(lockOSThread bool, handler EventHandler) error {
	if lockOSThread {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
	}
	events := make([]Event, 1)
	for {
		n, err := poller.Wait(events, -1)
		if err != nil || poller.Closed() {
			handler.OnClose(poller, err)
			return err
		}
		for _, event := range events[:n] {
			handler.OnEvent(poller, event.FD, event.Events)
		}
	}
}
