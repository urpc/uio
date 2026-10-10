//go:build (darwin || netbsd || freebsd || openbsd || dragonfly) && !stdio

package poller

import (
	"errors"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const (
	readEvents  = unix.EVFILT_READ
	writeEvents = unix.EVFILT_WRITE
	errorEvents = unix.EV_EOF | unix.EV_ERROR
)

// Tagged reports whether events carry the tag a descriptor was registered
// with. kqueue could carry it in udata, but that field is a pointer, and a
// tag in it would look like a heap pointer to the garbage collector on
// 32-bit platforms. kqueue drops a descriptor's pending events when the
// descriptor closes, so only an event another waiter has already collected
// can reach a connection that reused the number, and readiness reported in
// error costs that connection one empty read.
const Tagged = false

// Poller wraps kqueue plus a non-blocking pipe used to wake its waiters.
// Several goroutines may wait on one Poller at once, each with its own Batch.
// waiters delays descriptor release until every wait has finished converting
// its events.
type Poller struct {
	kqfd      int
	wakeRead  int
	wakeWrite int

	mu      sync.Mutex // protects waiters, parked and descriptor lifetime
	waiters int        // includes readiness-event conversion after kevent
	parked  []*Batch   // batches holding a park descriptor, released with the poller

	// ctl keeps the kqueue descriptor open under control operations, which
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
type Batch struct {
	rawEvents [1024]unix.Kevent_t

	// parkFile holds a non-blocking duplicate of the kqueue descriptor,
	// registered with the runtime's netpoller so its owner can wait parked
	// (see WaitBatchParked); parkConn is its raw connection. Each waiter has
	// its own, because the runtime wakes every goroutine parked on one
	// descriptor.
	parkFile  *os.File
	parkConn  syscall.RawConn
	parkTried bool
}

// New creates kqueue and registers the read end of its wake pipe.
func New() (*Poller, error) {
	kqfd, err := unix.Kqueue()
	if err != nil {
		return nil, err
	}
	unix.CloseOnExec(kqfd)
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
	poller := &Poller{kqfd: kqfd, wakeRead: waker[0], wakeWrite: waker[1]}
	if err = poller.change(waker[0], readEvents, unix.EV_ADD, false); err != nil {
		poller.release()
		return nil, err
	}
	return poller, nil
}

// Add registers a descriptor. Its readiness is level-triggered; Register
// covers connections.
func (poller *Poller) Add(fd int, want Interest) error {
	return poller.modify(fd, 0, want, false)
}

// Modify changes filters using the caller-owned previous interest.
func (poller *Poller) Modify(fd int, previous, want Interest) error {
	return poller.modify(fd, previous, want, false)
}

// Register adds a connection's descriptor in one call: want is the interest
// and edge selects EV_CLEAR, kqueue's edge-triggered readiness. The tag is
// accepted for interface parity; see Tagged.
func (poller *Poller) Register(fd int, want Interest, edge bool, _ uint32) error {
	return poller.modify(fd, 0, want, edge)
}

func (poller *Poller) modify(fd int, previous, want Interest, edge bool) error {
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
	return poller.modifyLocked(fd, previous, want, edge)
}

// modifyLocked translates one logical interest transition into the minimum set
// of independent kqueue filter changes.
func (poller *Poller) modifyLocked(fd int, previous, want Interest, edge bool) error {
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
		if err := poller.change(fd, readEvents, unix.EV_ADD, edge); err != nil {
			return err
		}
	}
	if previous&Writable == 0 && want&Writable != 0 {
		if err := poller.change(fd, writeEvents, unix.EV_ADD, edge); err != nil {
			return err
		}
	}
	return nil
}

// Remove deletes filters using the caller-owned previous interest.
func (poller *Poller) Remove(fd int, previous Interest) error {
	poller.ctl.RLock()
	defer poller.ctl.RUnlock()
	if poller.closed.Load() {
		return nil
	}
	return poller.removeLocked(fd, previous)
}

func (poller *Poller) removeLocked(fd int, previous Interest) error {
	var errs []error
	if previous&Readable != 0 {
		errs = append(errs, poller.deleteFilter(fd, readEvents))
	}
	if previous&Writable != 0 {
		errs = append(errs, poller.deleteFilter(fd, writeEvents))
	}
	return errors.Join(errs...)
}

func (poller *Poller) change(fd int, filter int64, flags int64, edge bool) error {
	if edge && flags&unix.EV_ADD != 0 {
		flags |= unix.EV_CLEAR
	}
	event := makeKevent(fd, filter, flags)
	_, err := unix.Kevent(poller.kqfd, []unix.Kevent_t{event}, nil, nil)
	return err
}

func (poller *Poller) deleteFilter(fd int, filter int64) error {
	err := poller.change(fd, filter, unix.EV_DELETE, false)
	// A missing filter already represents the requested state.
	if errors.Is(err, unix.ENOENT) || errors.Is(err, unix.EBADF) {
		return nil
	}
	return err
}

// Wait converts one kqueue batch into normalized events. timeout is in
// milliseconds; a negative value blocks indefinitely and zero polls.
func (poller *Poller) Wait(out []Event, timeout int) (int, error) {
	return poller.WaitBatch(&poller.batch, out, timeout)
}

// WaitBatch is Wait with a caller-owned kernel buffer. kqueue hands each
// edge to one waiter, so several goroutines can share one poller this way.
func (poller *Poller) WaitBatch(batch *Batch, out []Event, timeout int) (int, error) {
	if registered, err := poller.beginWait(); !registered {
		return 0, err
	}
	defer poller.finishWait()
	return poller.wait(batch, out, timeout, false)
}

// WaitBatchParked is WaitBatch with the blocking wait parked in the runtime's
// netpoller rather than spent in kevent. A goroutine blocked in a system call
// keeps its P until the runtime's monitor takes it back, which can leave the
// workers a waiter just woke queued behind it; parked, the waiter hands its P
// back the moment it finds nothing. A kqueue descriptor is itself readable
// while it has events pending, so a non-blocking duplicate registered with the
// runtime is what the waiter parks on. Where that duplicate cannot be set up,
// the wait blocks in kevent instead.
func (poller *Poller) WaitBatchParked(batch *Batch, out []Event) (int, error) {
	if registered, err := poller.beginWait(); !registered {
		return 0, err
	}
	defer poller.finishWait()
	return poller.wait(batch, out, -1, true)
}

// beginWait registers a waiter before it enters the kernel, so Close cannot
// release descriptors it can still access. A closed poller registers nothing.
func (poller *Poller) beginWait() (bool, error) {
	poller.mu.Lock()
	defer poller.mu.Unlock()
	if poller.closed.Load() {
		// A waiter still registered releases when it leaves; descriptors
		// must outlive every kevent call that can still read them.
		if poller.waiters == 0 {
			poller.release()
		}
		return false, poller.closeError()
	}
	poller.waiters++
	return true, nil
}

var zeroTimeout unix.Timespec

func (poller *Poller) wait(batch *Batch, out []Event, timeout int, park bool) (int, error) {
	var n int
	var err error
	if park && !batch.parkTried {
		poller.mountPark(batch)
	}
	if park && batch.parkConn != nil {
		readErr := batch.parkConn.Read(func(uintptr) bool {
			n, err = unix.Kevent(poller.kqfd, nil, batch.rawEvents[:], &zeroTimeout)
			return n != 0 || (err != nil && err != unix.EINTR)
		})
		if readErr != nil {
			// The runtime cannot poll the duplicate after all. Drop it and
			// block instead, as the wait always could.
			poller.dropPark(batch)
			n, err = 0, nil
		}
	}
	if !park || batch.parkConn == nil {
		var timeoutSpec *unix.Timespec
		if timeout >= 0 {
			spec := unix.NsecToTimespec(int64(time.Duration(timeout) * time.Millisecond))
			timeoutSpec = &spec
		}
		n, err = unix.Kevent(poller.kqfd, nil, batch.rawEvents[:], timeoutSpec)
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
		if event.Flags&errorEvents != 0 {
			events |= HangupEvents
		}
		if event.Filter == writeEvents {
			events |= WriteEvents
		}
		out[count] = Event{FD: fd, Events: events}
		count++
	}
	return count, nil
}

// mountPark duplicates the kqueue descriptor as non-blocking and hands the
// duplicate to the runtime's netpoller. On any failure the batch is left
// without a park and its owner blocks in kevent instead.
func (poller *Poller) mountPark(batch *Batch) {
	batch.parkTried = true
	fd, err := unix.FcntlInt(uintptr(poller.kqfd), unix.F_DUPFD_CLOEXEC, 0)
	if err != nil {
		return
	}
	if err = unix.SetNonblock(fd, true); err != nil {
		_ = unix.Close(fd)
		return
	}
	file := os.NewFile(uintptr(fd), "uio-kqueue")
	conn, err := file.SyscallConn()
	if err != nil {
		file.Close()
		return
	}
	poller.mu.Lock()
	batch.parkFile, batch.parkConn = file, conn
	poller.parked = append(poller.parked, batch)
	poller.mu.Unlock()
}

func (poller *Poller) dropPark(batch *Batch) {
	poller.mu.Lock()
	if batch.parkFile != nil {
		batch.parkFile.Close()
	}
	batch.parkFile, batch.parkConn = nil, nil
	poller.mu.Unlock()
}

// Wake interrupts one waiter by writing one byte to the non-blocking wake pipe.
func (poller *Poller) Wake() error {
	poller.mu.Lock()
	defer poller.mu.Unlock()
	if poller.closed.Load() {
		return nil
	}
	return poller.wakeLocked()
}

func (poller *Poller) wakeLocked() error {
	_, err := unix.Write(poller.wakeWrite, []byte{1})
	if errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EWOULDBLOCK) {
		return nil
	}
	return err
}

// drainWake empties the wake pipe. Once the poller is closed the pipe stays
// readable, so every waiter, whichever of them saw the byte first, finds it on
// its next kevent: a waiter that drained the byte Close wrote raises it again.
func (poller *Poller) drainWake() {
	if poller.closed.Load() {
		return
	}
	var buffer [64]byte
	for {
		if _, err := unix.Read(poller.wakeRead, buffer[:]); err != nil {
			break
		}
	}
	if poller.closed.Load() {
		_, _ = unix.Write(poller.wakeWrite, []byte{1})
	}
}

// Close publishes the terminal reason and interrupts every waiter. The last
// waiter releases kqueue and both pipe descriptors.
func (poller *Poller) Close(err error) error {
	poller.mu.Lock()
	defer poller.mu.Unlock()
	if poller.closed.Load() {
		return nil
	}
	// Publish the reason before closed, and closed before the wake byte: a
	// waiter that drains the byte checks closed afterwards and raises it again.
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
		for _, batch := range poller.parked {
			if batch.parkFile != nil {
				batch.parkFile.Close()
				batch.parkFile, batch.parkConn = nil, nil
			}
		}
		poller.parked = nil
		// Close published closed before any release, so a control operation
		// that has not started sees it; one in progress finishes first.
		poller.ctl.Lock()
		_ = unix.Close(poller.wakeRead)
		_ = unix.Close(poller.wakeWrite)
		_ = unix.Close(poller.kqfd)
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
