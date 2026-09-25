//go:build (darwin || netbsd || freebsd || openbsd || dragonfly) && !stdio

package poller

import (
	"errors"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"
)

const (
	readEvents  = unix.EVFILT_READ
	writeEvents = unix.EVFILT_WRITE
	errorEvents = unix.EV_EOF | unix.EV_ERROR
)

// NetPoller wraps kqueue plus a non-blocking pipe used to wake the owner for
// queued control work. waiters delays descriptor release until event
// conversion has finished.
type NetPoller struct {
	kqfd      int
	wakeRead  int
	wakeWrite int

	mu      sync.Mutex // protects waiters and descriptor lifetime
	waiters int        // includes readiness-event conversion after kevent

	closed      atomic.Bool
	edgeFD      map[int]bool
	closeReason atomic.Pointer[error]
	releaseOnce sync.Once
	rawEvents   [1024]unix.Kevent_t
}

// NewNetPoller creates kqueue and registers the read end of its wake pipe.
func NewNetPoller() (*NetPoller, error) {
	kqfd, err := unix.Kqueue()
	if err != nil {
		return nil, err
	}
	waker := make([]int, 2)
	if err = unix.Pipe(waker); err != nil {
		_ = unix.Close(kqfd)
		return nil, err
	}
	for _, fd := range waker {
		unix.CloseOnExec(fd)
		if err = unix.SetNonblock(fd, true); err != nil {
			_ = unix.Close(waker[0])
			_ = unix.Close(waker[1])
			_ = unix.Close(kqfd)
			return nil, err
		}
	}
	poller := &NetPoller{kqfd: kqfd, wakeRead: waker[0], wakeWrite: waker[1], edgeFD: make(map[int]bool)}
	if err = poller.change(waker[0], readEvents, unix.EV_ADD); err != nil {
		poller.release()
		return nil, err
	}
	return poller, nil
}

// SetEdgeTriggered selects EV_CLEAR for future application registrations.
func (poller *NetPoller) SetEdgeTriggered(fd int, enabled bool) {
	poller.mu.Lock()
	if enabled {
		poller.edgeFD[fd] = true
	} else {
		delete(poller.edgeFD, fd)
	}
	poller.mu.Unlock()
}

// SetTag is accepted for interface parity; kqueue events report a zero tag.
func (poller *NetPoller) SetTag(int, uint32) {}

// Add registers a descriptor.
func (poller *NetPoller) Add(fd int, want Interest) error {
	return poller.modify(fd, 0, want)
}

// Modify changes filters using the caller-owned previous interest.
func (poller *NetPoller) Modify(fd int, previous, want Interest) error {
	return poller.modify(fd, previous, want)
}

func (poller *NetPoller) modify(fd int, previous, want Interest) error {
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
	return poller.modifyLocked(fd, previous, want)
}

// modifyLocked translates one logical interest transition into the minimum set
// of independent kqueue filter changes.
func (poller *NetPoller) modifyLocked(fd int, previous, want Interest) error {
	if previous&Readable != 0 && want&Readable == 0 {
		if err := poller.deleteFilter(fd, readEvents); err != nil {
			return err
		}
	}
	if previous&Writable != 0 && want&Writable == 0 {
		if err := poller.deleteFilter(fd, writeEvents); err != nil {
			return err
		}
	}
	if previous&Readable == 0 && want&Readable != 0 {
		if err := poller.change(fd, readEvents, unix.EV_ADD); err != nil {
			return err
		}
	}
	if previous&Writable == 0 && want&Writable != 0 {
		if err := poller.change(fd, writeEvents, unix.EV_ADD); err != nil {
			return err
		}
	}
	return nil
}

// Remove deletes filters using the caller-owned previous interest.
func (poller *NetPoller) Remove(fd int, previous Interest) error {
	poller.mu.Lock()
	defer poller.mu.Unlock()
	if poller.closed.Load() {
		return nil
	}
	return poller.removeLocked(fd, previous)
}

func (poller *NetPoller) removeLocked(fd int, previous Interest) error {
	var errs []error
	if previous&Readable != 0 {
		errs = append(errs, poller.deleteFilter(fd, readEvents))
	}
	if previous&Writable != 0 {
		errs = append(errs, poller.deleteFilter(fd, writeEvents))
	}
	delete(poller.edgeFD, fd)
	return errors.Join(errs...)
}

func (poller *NetPoller) change(fd int, filter, flags int64) error {
	if poller.edgeFD[fd] && flags&unix.EV_ADD != 0 {
		flags |= unix.EV_CLEAR
	}
	event := makeKevent(fd, filter, flags)
	_, err := unix.Kevent(poller.kqfd, []unix.Kevent_t{event}, nil, nil)
	return err
}

func (poller *NetPoller) deleteFilter(fd int, filter int64) error {
	err := poller.change(fd, filter, unix.EV_DELETE)
	// A missing filter already represents the requested state.
	if errors.Is(err, unix.ENOENT) || errors.Is(err, unix.EBADF) {
		return nil
	}
	return err
}

// Wait converts one kqueue batch into normalized events. timeout is in
// milliseconds; a negative value blocks indefinitely and zero polls.
func (poller *NetPoller) Wait(out []Event, timeout int) (int, error) {
	// Register before kevent so Close cannot release descriptors still in use.
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
	var timeoutSpec *unix.Timespec
	if timeout >= 0 {
		spec := unix.NsecToTimespec(int64(time.Duration(timeout) * time.Millisecond))
		timeoutSpec = &spec
	}
	n, err := unix.Kevent(poller.kqfd, nil, poller.rawEvents[:], timeoutSpec)
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
		fd := int(event.Ident)
		if fd == poller.wakeRead {
			poller.drainWake()
			continue
		}
		if count == len(out) {
			// Event loops provide an output batch as large as rawEvents. Smaller
			// callers intentionally discard the excess notification.
			continue
		}
		var events Events
		if event.Filter == readEvents || event.Flags&errorEvents != 0 {
			events |= ReadEvents
		}
		if event.Filter == writeEvents {
			events |= WriteEvents
		}
		out[count] = Event{FD: fd, Events: events}
		count++
	}
	return count, nil
}

// Wake interrupts Wait by writing one byte to the non-blocking wake pipe.
func (poller *NetPoller) Wake() error {
	poller.mu.Lock()
	defer poller.mu.Unlock()
	if poller.closed.Load() {
		return nil
	}
	return poller.wakeLocked()
}

func (poller *NetPoller) wakeLocked() error {
	// The pipe contains wake bytes only; tasks remain in the loop queue.
	_, err := unix.Write(poller.wakeWrite, []byte{1})
	if errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EWOULDBLOCK) {
		return nil
	}
	return err
}

func (poller *NetPoller) drainWake() {
	var buffer [64]byte
	for {
		if _, err := unix.Read(poller.wakeRead, buffer[:]); err != nil {
			return
		}
	}
}

// Close publishes the terminal reason and interrupts Wait. The last waiter
// releases kqueue and both pipe descriptors.
func (poller *NetPoller) Close(err error) error {
	poller.mu.Lock()
	defer poller.mu.Unlock()
	if poller.closed.Load() {
		return nil
	}
	// Publish the reason before closed, then let the final waiter release fds.
	reason := err
	poller.closeReason.Store(&reason)
	poller.closed.Store(true)
	if poller.waiters == 0 {
		poller.release()
		return nil
	}
	return poller.wakeLocked()
}

// Closed reports whether Close has published the terminal state.
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
		_ = unix.Close(poller.wakeRead)
		_ = unix.Close(poller.wakeWrite)
		_ = unix.Close(poller.kqfd)
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
