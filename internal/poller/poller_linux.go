//go:build linux && !stdio

package poller

import (
	"encoding/binary"
	"errors"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// rawProbeMinProcs is the smallest GOMAXPROCS at which the raw readiness
// probe pays; see WaitBatch.
const rawProbeMinProcs = 8

// rawProbeEnabled caches that comparison. runtime.GOMAXPROCS takes the
// runtime's scheduler lock, which has no place on a waiter's path.
var rawProbeEnabled = runtime.GOMAXPROCS(0) >= rawProbeMinProcs

const (
	readEvents  = unix.EPOLLIN
	writeEvents = unix.EPOLLOUT
	errorEvents = unix.EPOLLERR | unix.EPOLLHUP | unix.EPOLLRDHUP | unix.EPOLLPRI
	// hangupEvents leaves out EPOLLPRI: urgent data ends nothing.
	hangupEvents = unix.EPOLLERR | unix.EPOLLHUP | unix.EPOLLRDHUP
)

// Tagged reports whether events carry the tag a descriptor was registered
// with: epoll returns it in the event data.
const Tagged = true

// Poller wraps epoll plus an eventfd used to wake its waiters. mu coordinates
// Close with active waits through waiters; descriptors are released only
// after the final waiter finishes conversion.
type Poller struct {
	epfd   int
	wakefd int

	mu      sync.Mutex // protects waiters, wakers and descriptor lifetime
	waiters int        // includes readiness-event conversion after epoll_wait
	wakers  []*Batch   // waiters Close raises on their private descriptor

	// ctl keeps the epoll descriptor open under control operations, which
	// connections' turns issue concurrently: they share it, and only the
	// release of the descriptors takes it exclusively. It is separate from mu
	// so that a registration never waits behind another one's system call.
	ctl sync.RWMutex

	closed      atomic.Bool
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

	// parkFile holds a non-blocking duplicate of the epoll descriptor,
	// registered with the runtime's netpoller so its owner can wait parked
	// (see WaitBatchParked); parkConn is its raw connection. Both live here,
	// per waiter, because the runtime wakes every goroutine parked on the
	// descriptor, and several waiters share one poller.
	parkFile  *os.File
	parkConn  syscall.RawConn
	parkTried bool // a failed setup is not retried on every wait
}

// New creates epoll and registers its internal level-triggered waker.
func New() (*Poller, error) {
	epfd, err := unix.EpollCreate1(unix.EPOLL_CLOEXEC)
	if err != nil {
		return nil, err
	}
	wakefd, err := unix.Eventfd(0, unix.EFD_NONBLOCK|unix.EFD_CLOEXEC)
	if err != nil {
		_ = unix.Close(epfd)
		return nil, err
	}
	poller := &Poller{epfd: epfd, wakefd: wakefd}
	if err = unix.EpollCtl(epfd, unix.EPOLL_CTL_ADD, wakefd, &unix.EpollEvent{
		Fd: int32(wakefd), Events: readEvents,
	}); err != nil {
		_ = unix.Close(wakefd)
		_ = unix.Close(epfd)
		return nil, err
	}
	return poller, nil
}

// Add registers a descriptor that is not already watched. Its readiness is
// level-triggered and its events carry no tag; Register covers connections.
func (poller *Poller) Add(fd int, want Interest) error {
	return poller.control(fd, want, unix.EPOLL_CTL_ADD)
}

// Register adds a connection's descriptor in one call: want is the interest,
// edge selects edge-triggered readiness, and every event the poller reports
// for fd carries tag. Streams and datagrams both register this way, once —
// nothing modifies their registration afterwards — so the poller keeps no
// per-descriptor state for triggering or tags.
func (poller *Poller) Register(fd int, want Interest, edge bool, tag uint32) error {
	if want == 0 {
		return errInvalidInterest
	}
	if poller.closed.Load() {
		return poller.closedError()
	}
	poller.ctl.RLock()
	defer poller.ctl.RUnlock()
	if poller.closed.Load() {
		return poller.closedError()
	}
	return unix.EpollCtl(poller.epfd, unix.EPOLL_CTL_ADD, fd, &unix.EpollEvent{
		Fd: int32(fd), Pad: int32(tag), Events: epollEvents(want, edge),
	})
}

// Modify changes the interest of an already watched descriptor.
func (poller *Poller) Modify(fd int, _ Interest, want Interest) error {
	return poller.control(fd, want, unix.EPOLL_CTL_MOD)
}

// Remove unregisters a descriptor. previous is used by kqueue backends.
func (poller *Poller) Remove(fd int, _ Interest) error {
	poller.ctl.RLock()
	defer poller.ctl.RUnlock()
	if poller.closed.Load() {
		return nil
	}
	err := unix.EpollCtl(poller.epfd, unix.EPOLL_CTL_DEL, fd, nil)
	if errors.Is(err, unix.ENOENT) || errors.Is(err, unix.EBADF) {
		return nil
	}
	return err
}

func (poller *Poller) control(fd int, want Interest, operation int) error {
	if want == 0 {
		return errInvalidInterest
	}
	if poller.closed.Load() {
		return poller.closedError()
	}
	poller.ctl.RLock()
	defer poller.ctl.RUnlock()
	if poller.closed.Load() {
		return poller.closedError()
	}
	if err := unix.EpollCtl(poller.epfd, operation, fd, &unix.EpollEvent{
		Fd: int32(fd), Events: epollEvents(want, false),
	}); err != nil {
		return err
	}
	return nil
}

// epollEvents maps an interest to epoll bits. Stream descriptors use
// edge-triggered readiness because connection tasks drain each round until
// EAGAIN; the wake descriptor is registered without that bit.
func epollEvents(want Interest, edge bool) uint32 {
	events := uint32(errorEvents)
	if want&Readable != 0 {
		events |= readEvents
	}
	if want&Writable != 0 {
		events |= writeEvents
	}
	if edge {
		events |= unix.EPOLLET
	}
	return events
}

// Wait converts one epoll batch into normalized events. timeout is in
// milliseconds; a negative value blocks indefinitely and zero polls.
func (poller *Poller) Wait(out []Event, timeout int) (int, error) {
	return poller.WaitBatch(&poller.batch, out, timeout)
}

// WaitBatch is Wait with a caller-owned kernel buffer. epoll hands each ready
// edge to one waiter, so several goroutines can share one poller this way.
func (poller *Poller) WaitBatch(batch *Batch, out []Event, timeout int) (int, error) {
	if registered, err := poller.beginWait(batch); !registered {
		return 0, err
	}
	defer poller.finishWait()
	return poller.wait(batch, out, timeout, false)
}

// WaitBatchParked is WaitBatch with the blocking wait parked in the runtime's
// netpoller rather than spent in epoll_wait. A goroutine blocked in a system
// call keeps its P until the runtime's monitor takes it back, which can leave
// the workers a waiter just woke queued behind it; parked, the waiter hands
// its P back the moment it finds nothing, and the runtime wakes it as soon as
// the epoll descriptor reports events. A non-blocking duplicate of the
// descriptor, registered with the runtime, is what it parks on; each Read
// callback takes whatever is ready with a zero-timeout wait, so a waiter with
// events pending still makes exactly one syscall. Where that duplicate cannot
// be set up, the wait falls back to blocking in epoll_wait as before.
func (poller *Poller) WaitBatchParked(batch *Batch, out []Event) (int, error) {
	if registered, err := poller.beginWait(batch); !registered {
		return 0, err
	}
	defer poller.finishWait()
	return poller.wait(batch, out, -1, true)
}

// beginWait registers a waiter before it enters the kernel, so Close wakes it
// instead of releasing descriptors it can still access. The registration and
// the private wake descriptor are published under the same lock Close reads,
// so a waiter is either seen by Close or observes the closed poller. Only a
// registered waiter may enter the kernel and run finishWait; a closed poller
// registers nothing, and its close reason, the error returned then, may be nil.
func (poller *Poller) beginWait(batch *Batch) (bool, error) {
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
		return false, err
	}
	if !batch.haveWake {
		wakefd, err := unix.Eventfd(0, unix.EFD_NONBLOCK|unix.EFD_CLOEXEC)
		if err != nil {
			poller.mu.Unlock()
			return false, err
		}
		if err = unix.EpollCtl(poller.epfd, unix.EPOLL_CTL_ADD, wakefd, &unix.EpollEvent{
			Fd: int32(wakefd), Events: readEvents,
		}); err != nil {
			_ = unix.Close(wakefd)
			poller.mu.Unlock()
			return false, err
		}
		batch.wakefd = wakefd
		batch.haveWake = true
		poller.wakers = append(poller.wakers, batch)
	}
	poller.waiters++
	poller.mu.Unlock()
	return true, nil
}

// wait takes one round of events and converts them. park selects the parked
// wait of WaitBatchParked over the probe-and-block of WaitBatch.
func (poller *Poller) wait(batch *Batch, out []Event, timeout int, park bool) (int, error) {
	var n int
	var err error
	if park {
		if !batch.parkTried {
			poller.mountPark(batch)
		}
		if batch.parkConn != nil {
			readErr := batch.parkConn.Read(func(uintptr) bool {
				n, err = unix.EpollWait(poller.epfd, batch.rawEvents[:], 0)
				return n != 0 || (err != nil && err != unix.EINTR)
			})
			if readErr != nil {
				// The runtime cannot poll the duplicate after all. Drop it and
				// block instead, as the wait always could.
				batch.parkFile.Close()
				batch.parkFile, batch.parkConn = nil, nil
				n, err = 0, nil
			}
		}
		if batch.parkConn == nil {
			n, err = unix.EpollWait(poller.epfd, batch.rawEvents[:], timeout)
		}
	} else if timeout != 0 && rawProbeEnabled {
		// A raw zero-timeout probe first: while readiness is pending, which under
		// load it almost always is, this takes the batch without entering the
		// runtime's syscall accounting, so the waiter keeps its P and the events
		// are handed to the executor a hand-off earlier. The probe costs one
		// syscall on an idle wait, and the blocking call still sees any readiness
		// that arrives after an empty probe. It stays off small hosts, where
		// holding a P through a syscall costs more than the hand-off it saves.
		r, _, errno := unix.RawSyscall6(unix.SYS_EPOLL_PWAIT, uintptr(poller.epfd),
			uintptr(unsafe.Pointer(&batch.rawEvents[0])), uintptr(len(batch.rawEvents)), 0, 0, 0)
		if errno == 0 && r > 0 {
			n = int(r)
		} else {
			n, err = unix.EpollWait(poller.epfd, batch.rawEvents[:], timeout)
		}
	} else {
		n, err = unix.EpollWait(poller.epfd, batch.rawEvents[:], timeout)
	}
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

// mountPark duplicates the epoll descriptor as non-blocking and hands the
// duplicate to the runtime's netpoller, which only polls descriptors that are.
// epoll_wait itself is unaffected by the flag. On any failure the batch is
// left without a park and the caller blocks in epoll_wait instead.
func (poller *Poller) mountPark(batch *Batch) {
	batch.parkTried = true
	// F_DUPFD_CLOEXEC: the duplicate must not survive into a child process,
	// and setting the flag after a plain dup would leave a fork/exec window.
	fd, err := unix.FcntlInt(uintptr(poller.epfd), unix.F_DUPFD_CLOEXEC, 0)
	if err != nil {
		return
	}
	if err = unix.SetNonblock(fd, true); err != nil {
		_ = unix.Close(fd)
		return
	}
	file := os.NewFile(uintptr(fd), "uio-epoll")
	conn, err := file.SyscallConn()
	if err != nil {
		file.Close()
		return
	}
	batch.parkFile = file
	batch.parkConn = conn
}

// Wake interrupts Wait. The eventfd counter is coalesced by the event loop, so
// its numeric value never represents a task count.
func (poller *Poller) Wake() error {
	poller.mu.Lock()
	defer poller.mu.Unlock()
	if poller.closed.Load() {
		return nil
	}
	return poller.wakeLocked()
}

func (poller *Poller) wakeLocked() error {
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

func (poller *Poller) drainWake() {
	drainWakeFD(poller.wakefd)
}

func drainWakeFD(fd int) {
	var value [8]byte
	_, _ = unix.Read(fd, value[:])
}

// Close publishes the terminal reason and interrupts Wait. Descriptor release
// is deferred until no goroutine can still read or convert epoll events.
func (poller *Poller) Close(err error) error {
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
func (poller *Poller) Closed() bool { return poller.closed.Load() }

func (poller *Poller) closeError() error {
	if reason := poller.closeReason.Load(); reason != nil {
		return *reason
	}
	return nil
}

func (poller *Poller) closedError() error {
	if err := poller.closeError(); err != nil {
		return err
	}
	return unix.EBADF
}

func (poller *Poller) release() {
	poller.releaseOnce.Do(func() {
		for _, waiter := range poller.wakers {
			_ = unix.Close(waiter.wakefd)
			// The owner can no longer be inside WaitBatch, so the batch can
			// forget the descriptor instead of ever touching it again.
			waiter.wakefd = 0
			waiter.haveWake = false
			if waiter.parkFile != nil {
				waiter.parkFile.Close()
				waiter.parkFile = nil
				waiter.parkConn = nil
			}
		}
		poller.wakers = nil
		// Close published closed before any release, so a control operation
		// that has not started sees it; one in progress finishes first.
		poller.ctl.Lock()
		_ = unix.Close(poller.wakefd)
		_ = unix.Close(poller.epfd)
		poller.ctl.Unlock()
	})
}

func (poller *Poller) finishWait() {
	poller.mu.Lock()
	poller.waiters--
	if poller.waiters == 0 && poller.closed.Load() {
		poller.release()
	}
	poller.mu.Unlock()
}

// Serve is retained as a compatibility wrapper around Wait.
func (poller *Poller) Serve(lockOSThread bool, handler EventHandler) error {
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
