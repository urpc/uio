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
	// hangupEvents leaves out EPOLLPRI: urgent data ends nothing.
	hangupEvents = unix.EPOLLERR | unix.EPOLLHUP | unix.EPOLLRDHUP
)

// NetPoller wraps epoll plus an eventfd used to wake the owner for queued
// control work. mu coordinates Close with an active Wait through waiters;
// descriptors are released only after the final waiter finishes conversion.
type NetPoller struct {
	epfd   int
	wakefd int

	mu      sync.Mutex // protects waiters, wakers and descriptor lifetime
	waiters int        // includes readiness-event conversion after epoll_wait
	wakers  []*Batch   // waiters Close raises on their private descriptor

	closed      atomic.Bool
	edgeFD      map[int]bool
	tags        map[int]uint32
	closeReason atomic.Pointer[error]
	releaseOnce sync.Once
	batch       Batch
}

// Batch is a kernel event buffer. Wait uses the poller's own; goroutines that
// wait on one poller concurrently each pass their own to WaitBatch, and a
// Batch belongs to the poller it first waited on.
//
// A batch also owns the waiter's private wake descriptor. The shared wakefd is
// drained by whichever waiter observes it first, and a wake-up that another
// waiter consumes between this waiter's kernel wake-up and its readiness
// re-check would put it back to sleep — with an unbounded wait, forever. Close
// therefore raises every waiter on a descriptor only that waiter drains.
type Batch struct {
	rawEvents [1024]unix.EpollEvent
	wakefd    int
	haveWake  bool
}

// NewNetPoller creates epoll and registers its internal level-triggered waker.
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
	poller := &NetPoller{epfd: epfd, wakefd: wakefd, edgeFD: make(map[int]bool), tags: make(map[int]uint32)}
	if err = unix.EpollCtl(epfd, unix.EPOLL_CTL_ADD, wakefd, &unix.EpollEvent{
		Fd: int32(wakefd), Events: readEvents,
	}); err != nil {
		_ = unix.Close(wakefd)
		_ = unix.Close(epfd)
		return nil, err
	}
	return poller, nil
}

// SetEdgeTriggered selects edge-triggered readiness for future registrations.
// It must be called before adding application descriptors.
func (poller *NetPoller) SetEdgeTriggered(fd int, enabled bool) {
	poller.mu.Lock()
	if enabled {
		poller.edgeFD[fd] = true
	} else {
		delete(poller.edgeFD, fd)
	}
	poller.mu.Unlock()
}

// SetTag attaches tag to future registrations and modifications of fd. Wait
// reports it with every event for fd; zero clears it.
func (poller *NetPoller) SetTag(fd int, tag uint32) {
	poller.mu.Lock()
	if tag != 0 {
		poller.tags[fd] = tag
	} else {
		delete(poller.tags, fd)
	}
	poller.mu.Unlock()
}

// Add registers a descriptor that is not already watched.
func (poller *NetPoller) Add(fd int, want Interest) error {
	return poller.control(fd, want, unix.EPOLL_CTL_ADD)
}

// Modify changes the interest of an already watched descriptor.
func (poller *NetPoller) Modify(fd int, _ Interest, want Interest) error {
	return poller.control(fd, want, unix.EPOLL_CTL_MOD)
}

// Remove unregisters a descriptor. previous is used by kqueue backends.
func (poller *NetPoller) Remove(fd int, _ Interest) error {
	poller.mu.Lock()
	defer poller.mu.Unlock()
	if poller.closed.Load() {
		return nil
	}
	err := unix.EpollCtl(poller.epfd, unix.EPOLL_CTL_DEL, fd, nil)
	delete(poller.edgeFD, fd)
	delete(poller.tags, fd)
	if errors.Is(err, unix.ENOENT) || errors.Is(err, unix.EBADF) {
		return nil
	}
	return err
}

func (poller *NetPoller) control(fd int, want Interest, operation int) error {
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
	if err := unix.EpollCtl(poller.epfd, operation, fd, &unix.EpollEvent{
		Fd: int32(fd), Pad: int32(poller.tags[fd]), Events: poller.epollEvents(fd, want),
	}); err != nil {
		return err
	}
	return nil
}

func (poller *NetPoller) epollEvents(fd int, want Interest) uint32 {
	events := uint32(errorEvents)
	// Stream descriptors use edge-triggered readiness because connection tasks
	// drain each round until EAGAIN; the wake descriptor remains level-triggered.
	// The wake descriptor is intentionally registered without this bit.
	if want&Readable != 0 {
		events |= readEvents
	}
	if want&Writable != 0 {
		events |= writeEvents
	}
	if poller.edgeFD[fd] {
		events |= unix.EPOLLET
	}
	return events
}

// Wait converts one epoll batch into normalized events. timeout is in
// milliseconds; a negative value blocks indefinitely and zero polls.
func (poller *NetPoller) Wait(out []Event, timeout int) (int, error) {
	return poller.WaitBatch(&poller.batch, out, timeout)
}

// WaitBatch is Wait with a caller-owned kernel buffer. epoll hands each ready
// edge to one waiter, so several goroutines can share one poller this way.
func (poller *NetPoller) WaitBatch(batch *Batch, out []Event, timeout int) (int, error) {
	// Register before entering epoll_wait so Close wakes instead of releasing
	// descriptors that this call can still access. The registration and the
	// private wake descriptor are published under the same lock Close reads,
	// so a waiter is either seen by Close or observes the closed poller.
	poller.mu.Lock()
	if poller.closed.Load() {
		// Another waiter may still be registered: Close woke it on its private
		// descriptor, and closing that descriptor before it collects the
		// event takes the event out of epoll, leaving it asleep for good.
		// The last registered waiter releases instead.
		if poller.waiters == 0 {
			poller.release()
		}
		err := poller.closeError()
		poller.mu.Unlock()
		return 0, err
	}
	if !batch.haveWake {
		wakefd, err := unix.Eventfd(0, unix.EFD_NONBLOCK|unix.EFD_CLOEXEC)
		if err != nil {
			poller.mu.Unlock()
			return 0, err
		}
		if err = unix.EpollCtl(poller.epfd, unix.EPOLL_CTL_ADD, wakefd, &unix.EpollEvent{
			Fd: int32(wakefd), Events: readEvents,
		}); err != nil {
			_ = unix.Close(wakefd)
			poller.mu.Unlock()
			return 0, err
		}
		batch.wakefd = wakefd
		batch.haveWake = true
		poller.wakers = append(poller.wakers, batch)
	}
	poller.waiters++
	poller.mu.Unlock()
	defer poller.finishWait()
	n, err := unix.EpollWait(poller.epfd, batch.rawEvents[:], timeout)
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
	for _, event := range batch.rawEvents[:n] {
		fd := int(event.Fd)
		if fd == poller.wakefd {
			poller.drainWake()
			continue
		}
		if fd == batch.wakefd {
			// The private descriptor belongs to this waiter alone, so
			// draining it can never take a wake-up from anyone else.
			drainWakeFD(fd)
			continue
		}
		if count == len(out) {
			// Event loops provide an output batch as large as rawEvents. Smaller
			// callers intentionally discard the excess notification.
			continue
		}
		var events Events
		if event.Events&(readEvents|errorEvents) != 0 {
			events |= ReadEvents
		}
		if event.Events&hangupEvents != 0 {
			events |= HangupEvents
		}
		if event.Events&writeEvents != 0 {
			events |= WriteEvents
		}
		out[count] = Event{FD: fd, Events: events, Tag: uint32(event.Pad)}
		count++
	}
	return count, nil
}

// Wake interrupts Wait. The eventfd counter is coalesced by the event loop, so
// its numeric value never represents a task count.
func (poller *NetPoller) Wake() error {
	poller.mu.Lock()
	defer poller.mu.Unlock()
	if poller.closed.Load() {
		return nil
	}
	return poller.wakeLocked()
}

func (poller *NetPoller) wakeLocked() error {
	return raiseWake(poller.wakefd)
}

// raiseWake makes fd readable for epoll. eventfd is only a wake signal; its
// counter carries no application data, so a saturated counter (EAGAIN) already
// guarantees the wake.
func raiseWake(fd int) error {
	var value [8]byte
	binary.NativeEndian.PutUint64(value[:], 1)
	_, err := unix.Write(fd, value[:])
	if errors.Is(err, unix.EAGAIN) {
		return nil
	}
	return err
}

func (poller *NetPoller) drainWake() {
	drainWakeFD(poller.wakefd)
}

func drainWakeFD(fd int) {
	var value [8]byte
	_, _ = unix.Read(fd, value[:])
}

// Close publishes the terminal reason and interrupts Wait. Descriptor release
// is deferred until no goroutine can still read or convert epoll events.
func (poller *NetPoller) Close(err error) error {
	poller.mu.Lock()
	defer poller.mu.Unlock()
	if poller.closed.Load() {
		return nil
	}
	// Publish the reason before closed, then raise every registered waiter on
	// its own descriptor: the shared wakefd is drained by whichever waiter
	// observes it first, and a wake-up taken by one waiter between another's
	// kernel wake-up and its readiness re-check would park that waiter for
	// good. The last waiter releases descriptors after it finishes converting
	// events.
	reason := err
	poller.closeReason.Store(&reason)
	poller.closed.Store(true)
	if poller.waiters == 0 {
		poller.release()
		return nil
	}
	for _, waiter := range poller.wakers {
		_ = raiseWake(waiter.wakefd)
	}
	return nil
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
		for _, waiter := range poller.wakers {
			_ = unix.Close(waiter.wakefd)
			// The owner can no longer be inside WaitBatch, so the batch can
			// forget the descriptor instead of ever touching it again.
			waiter.wakefd = 0
			waiter.haveWake = false
		}
		poller.wakers = nil
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
