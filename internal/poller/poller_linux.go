//go:build linux && !stdio

package poller

import (
	"encoding/binary"
	"errors"
	"runtime"
	"sync"
	"sync/atomic"

	"golang.org/x/sys/unix"
)

const (
	readEvents  = unix.EPOLLIN
	writeEvents = unix.EPOLLOUT
	errorEvents = unix.EPOLLERR | unix.EPOLLHUP | unix.EPOLLRDHUP | unix.EPOLLPRI
)

type NetPoller struct {
	epfd   int
	wakefd int

	mu        sync.Mutex // protects interests, waiters, and descriptor lifetime
	interests map[int]Interest
	waiters   int // includes readiness-event conversion after epoll_wait

	closed      atomic.Bool
	closeReason atomic.Pointer[error]
	releaseOnce sync.Once
	rawEvents   [1024]unix.EpollEvent
}

func NewNetPoller() (*NetPoller, error) {
	epfd, err := unix.EpollCreate1(unix.EPOLL_CLOEXEC)
	if err != nil {
		return nil, err
	}
	wakefd, err := unix.Eventfd(0, unix.EFD_NONBLOCK|unix.EFD_CLOEXEC)
	if err != nil {
		_ = unix.Close(epfd)
		return nil, err
	}
	poller := &NetPoller{epfd: epfd, wakefd: wakefd, interests: make(map[int]Interest)}
	if err = unix.EpollCtl(epfd, unix.EPOLL_CTL_ADD, wakefd, &unix.EpollEvent{
		Fd: int32(wakefd), Events: readEvents,
	}); err != nil {
		_ = unix.Close(wakefd)
		_ = unix.Close(epfd)
		return nil, err
	}
	return poller, nil
}

func (poller *NetPoller) Watch(fd int, want Interest) error {
	if want == 0 {
		return errInvalidInterest
	}
	if poller.closed.Load() {
		return poller.closedError()
	}
	poller.mu.Lock()
	defer poller.mu.Unlock()
	if poller.closed.Load() {
		return poller.closedError()
	}
	previous, exists := poller.interests[fd]
	if exists && previous == want {
		return nil
	}
	operation := unix.EPOLL_CTL_ADD
	if exists {
		operation = unix.EPOLL_CTL_MOD
	}
	if err := unix.EpollCtl(poller.epfd, operation, fd, &unix.EpollEvent{
		Fd: int32(fd), Events: epollEvents(want),
	}); err != nil {
		return err
	}
	poller.interests[fd] = want
	return nil
}

func epollEvents(want Interest) uint32 {
	events := uint32(errorEvents)
	if want&Readable != 0 {
		events |= readEvents
	}
	if want&Writable != 0 {
		events |= writeEvents
	}
	return events
}

func (poller *NetPoller) Unwatch(fd int) error {
	poller.mu.Lock()
	defer poller.mu.Unlock()
	// Forget the fd even if the kernel already removed it after close.
	delete(poller.interests, fd)
	if poller.closed.Load() {
		return nil
	}
	err := unix.EpollCtl(poller.epfd, unix.EPOLL_CTL_DEL, fd, nil)
	if errors.Is(err, unix.ENOENT) || errors.Is(err, unix.EBADF) {
		return nil
	}
	return err
}

func (poller *NetPoller) Wait(out []Event, timeout int) (int, error) {
	// Register before entering epoll_wait so Close wakes instead of releasing
	// descriptors that this call can still access.
	poller.mu.Lock()
	if poller.closed.Load() {
		poller.release()
		err := poller.closeError()
		poller.mu.Unlock()
		return 0, err
	}
	poller.waiters++
	poller.mu.Unlock()
	defer poller.finishWait()
	n, err := unix.EpollWait(poller.epfd, poller.rawEvents[:], timeout)
	if poller.closed.Load() {
		return 0, poller.closeError()
	}
	if errors.Is(err, unix.EINTR) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	count := 0
	for _, event := range poller.rawEvents[:n] {
		fd := int(event.Fd)
		if fd == poller.wakefd {
			poller.drainWake()
			continue
		}
		if count == len(out) {
			// Level triggering will report readiness again on the next Wait.
			continue
		}
		var events Events
		if event.Events&(readEvents|errorEvents) != 0 {
			events |= ReadEvents
		}
		if event.Events&writeEvents != 0 {
			events |= WriteEvents
		}
		out[count] = Event{FD: fd, Events: events}
		count++
	}
	return count, nil
}

func (poller *NetPoller) Wake() error {
	poller.mu.Lock()
	defer poller.mu.Unlock()
	if poller.closed.Load() {
		return nil
	}
	return poller.wakeLocked()
}

func (poller *NetPoller) wakeLocked() error {
	// eventfd is only a wake signal; its counter carries no application data.
	var value [8]byte
	binary.NativeEndian.PutUint64(value[:], 1)
	_, err := unix.Write(poller.wakefd, value[:])
	if errors.Is(err, unix.EAGAIN) {
		return nil
	}
	return err
}

func (poller *NetPoller) drainWake() {
	var value [8]byte
	_, _ = unix.Read(poller.wakefd, value[:])
}

func (poller *NetPoller) Close(err error) error {
	poller.mu.Lock()
	defer poller.mu.Unlock()
	if poller.closed.Load() {
		return nil
	}
	// Publish the reason before closed, then wake an active waiter. The last
	// waiter releases descriptors after it finishes converting events.
	reason := err
	poller.closeReason.Store(&reason)
	poller.closed.Store(true)
	if poller.waiters == 0 {
		poller.release()
		return nil
	}
	return poller.wakeLocked()
}

func (poller *NetPoller) Closed() bool { return poller.closed.Load() }

func (poller *NetPoller) closeError() error {
	if reason := poller.closeReason.Load(); reason != nil {
		return *reason
	}
	return nil
}

func (poller *NetPoller) closedError() error {
	if err := poller.closeError(); err != nil {
		return err
	}
	return unix.EBADF
}

func (poller *NetPoller) release() {
	poller.releaseOnce.Do(func() {
		_ = unix.Close(poller.wakefd)
		_ = unix.Close(poller.epfd)
	})
}

func (poller *NetPoller) finishWait() {
	poller.mu.Lock()
	poller.waiters--
	if poller.waiters == 0 && poller.closed.Load() {
		poller.release()
	}
	poller.mu.Unlock()
}

// Serve is retained as a compatibility wrapper around Wait.
func (poller *NetPoller) Serve(lockOSThread bool, handler EventHandler) error {
	if lockOSThread {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
	}
	events := make([]Event, 1024)
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

func (poller *NetPoller) AddReadWrite(fd int) error { return poller.Watch(fd, Readable|Writable) }
func (poller *NetPoller) AddRead(fd int) error      { return poller.Watch(fd, Readable) }
func (poller *NetPoller) ModRead(fd int) error      { return poller.Watch(fd, Readable) }
func (poller *NetPoller) ModWrite(fd int) error     { return poller.Watch(fd, Writable) }
func (poller *NetPoller) ModReadWrite(fd int) error { return poller.Watch(fd, Readable|Writable) }
