//go:build (linux || darwin || netbsd || freebsd || openbsd || dragonfly) && !stdio

package uio

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"runtime"
	"testing"
	"time"

	"github.com/urpc/uio/internal/fdmap"
	"golang.org/x/sys/unix"
)

func newContractNativeLoop(t *testing.T) (*Events, *eventLoop) {
	t.Helper()
	ev := &Events{Pollers: 1}
	if err := ev.initConfig(); err != nil {
		t.Fatal(err)
	}
	loop, err := newEventLoop(ev)
	if err != nil {
		t.Fatal(err)
	}
	ev.loops = []*eventLoop{loop}
	t.Cleanup(func() { loop.closePoller(nil); loop.shutdown(net.ErrClosed) })
	return ev, loop
}
func contractNativeSocket(t *testing.T, ev *Events, loop *eventLoop) *fdConn {
	t.Helper()
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = unix.Close(fds[1]) })
	c := &fdConn{fd: int32(fds[0])}
	c.entry = ev.plainEntry()
	c.loop = loop
	t.Cleanup(func() { c.closeUnregistered() })
	return c
}
func assertNativeDescriptorClosed(t *testing.T, fd int) {
	t.Helper()
	if _, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0); !errors.Is(err, unix.EBADF) {
		t.Fatalf("rejected descriptor remains open: %v", err)
	}
}

func TestNativeRegistrationWithdrawsAfterShutdownPublication(t *testing.T) {
	ev, loop := newContractNativeLoop(t)
	conn := contractNativeSocket(t, ev, loop)
	fd := conn.Fd()
	loop.stopping.Store(true)
	if err := loop.registerConn(conn); !errors.Is(err, net.ErrClosed) {
		t.Fatal(err)
	}
	if loop.fdMap.Get(fd) != nil || loop.ioTurnCount() != 0 {
		t.Fatal("registration left a mapping or a claimed turn after shutdown")
	}
	assertNativeDescriptorClosed(t, fd)
}

func TestNativeUDPAdmissionAndWatchFailuresReleaseOwnership(t *testing.T) {
	for _, mode := range []string{"stopped", "stopping", "closed-server", "retired-socket", "fd-range", "closed-poller"} {
		t.Run(mode, func(t *testing.T) {
			ev, loop := newContractNativeLoop(t)
			c := contractNativeSocket(t, ev, loop)
			c.udp = &unixUDPState{}
			fd := c.Fd()
			switch mode {
			case "stopped":
				loop.stopIO()
			case "stopping":
				loop.stopping.Store(true)
			case "closed-server":
				c.closeUnregistered()
			case "retired-socket":
				c.udp.gate.retire()
			case "fd-range":
				old := fdmap.MaxOpenFiles
				fdmap.MaxOpenFiles = 1
				loop.fdMap = fdmap.NewMap[fdConn]()
				fdmap.MaxOpenFiles = old
			case "closed-poller":
				_ = loop.poller.Close(nil)
			}
			var err error
			if mode == "stopped" || mode == "stopping" {
				err = loop.trackListener(c)
			} else {
				err = loop.registerListener(c)
			}
			if err == nil {
				t.Fatal("invalid UDP registration succeeded")
			}
			if mode != "fd-range" && mode != "closed-poller" && !errors.Is(err, net.ErrClosed) {
				t.Fatalf("registration error=%v", err)
			}
			if loop.fdMap.Get(fd) != nil || loop.ioTurnCount() != 0 || len(loop.udpServers) != 0 {
				t.Fatal("failed UDP admission retained a mapping, turn, or lifetime owner")
			}
			if mode == "stopped" || mode == "stopping" {
				assertNativeDescriptorClosed(t, fd)
			}
		})
	}
}

func TestNativeListenerWatchFailureRollsBackRegistry(t *testing.T) {
	ev, loop := newContractNativeLoop(t)
	l := &listener{fd: -1, loop: loop}
	if err := loop.addListener(l); err == nil {
		t.Fatal("invalid descriptor was registered")
	}
	if loop.listener(l.fd) != nil {
		t.Fatal("failed listener remained published")
	}
	a := &acceptor{events: ev, listeners: map[int]*listener{-1: l}}
	if err := a.startAccepting(); err == nil || l.started {
		t.Fatalf("start=%v started=%v", err, l.started)
	}
	l.descriptorGate.retire()
	if more, err := loop.accept(l, new(loopWaiter)); more || err != nil {
		t.Fatalf("closed listener accept=%v,%v", more, err)
	}
	loop.resumeAccept(l)
	loop.wakeTimer = time.AfterFunc(time.Hour, func() { t.Error("shutdown retained its fallback timer") })
	loop.shutdown(net.ErrClosed)
	if loop.wakeTimer != nil {
		t.Fatal("shutdown retained wake timer")
	}
	standalone := new(eventLoop)
	standalone.stopIO()
	if standalone.acquireIO() {
		t.Fatal("uninitialized stopped loop admitted a turn")
	}
}

func TestNativeAdoptionRejectsUnsupportedAndInvalidLayers(t *testing.T) {
	ev := new(Events)
	ev.ready.Store(true)
	if _, err := ev.Adopt(nil); !errors.Is(err, ErrUnsupported) {
		t.Fatal(err)
	}
	for _, mode := range []string{"unsupported-transport", "invalid-layer", "event-loop"} {
		t.Run(mode, func(t *testing.T) {
			a, b := net.Pipe()
			defer b.Close()
			var opts []AdoptOption
			if mode == "invalid-layer" {
				opts = []AdoptOption{WithLayers(nil)}
			}
			if mode == "event-loop" {
				opts = []AdoptOption{WithLayers(PassThroughLayer)}
				id := currentGoroutineID()
				activeEventLoops.Store(id, struct{}{})
				defer activeEventLoops.Delete(id)
			}
			_, err := ev.AdoptContext(context.Background(), a, opts...)
			if err == nil {
				t.Fatal("invalid adoption succeeded")
			}
			if mode == "event-loop" && !errors.Is(err, ErrDialOnEventLoop) {
				t.Fatal(err)
			}
			if _, err := b.Read(make([]byte, 1)); !errors.Is(err, net.ErrClosed) && !errors.Is(err, io.EOF) {
				t.Fatalf("rejected adoption kept transport open: %v", err)
			}
		})
	}
}

// Startup must roll back pollers that succeeded before a later allocation
// failed. Exhaust real descriptors so the test also detects resource leaks.
func TestNativeStartupResourceFailureRollsBackEarlierLoops(t *testing.T) {
	// Pollers is capped at GOMAXPROCS, and the rollback needs a second loop.
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(max(2, runtime.GOMAXPROCS(0))))
	ev := &Events{Pollers: 2}
	if err := ev.initConfig(); err != nil {
		t.Fatal(err)
	}
	if ev.Pollers != 2 {
		t.Fatalf("Pollers = %d, want two loops", ev.Pollers)
	}
	var original unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_NOFILE, &original); err != nil {
		t.Fatal(err)
	}
	limit := original
	limit.Cur = min(limit.Cur, 64)
	if err := unix.Setrlimit(unix.RLIMIT_NOFILE, &limit); err != nil {
		t.Fatal(err)
	}
	var opened []int
	defer func() {
		_ = unix.Setrlimit(unix.RLIMIT_NOFILE, &original)
		for _, fd := range opened {
			_ = unix.Close(fd)
		}
	}()
	fill := func() (int, error) {
		n := 0
		for {
			fd, err := unix.Open("/dev/null", unix.O_RDONLY|unix.O_CLOEXEC, 0)
			if errors.Is(err, unix.EMFILE) {
				return n, nil
			}
			if err != nil {
				return n, err
			}
			opened = append(opened, fd)
			n++
		}
	}
	// Finalizers and closes left from earlier tests can free a descriptor
	// between the fill and the startup, which then fits; that attempt proves
	// nothing either way, so it is repeated.
	var failure, initErr error
	for attempt := 0; attempt < 10; attempt++ {
		var unexpected bool
		unexpected, initErr, failure = startupRollbackAttempt(ev, fill, &opened)
		if !unexpected {
			break
		}
		failure = errors.New("startup unexpectedly exceeded the descriptor limit")
	}
	restoreErr := unix.Setrlimit(unix.RLIMIT_NOFILE, &original)
	for _, fd := range opened {
		_ = unix.Close(fd)
	}
	opened = nil
	if restoreErr != nil {
		t.Fatal(restoreErr)
	}
	if failure != nil {
		t.Fatalf("resource rollback: %v (startup error %v)", failure, initErr)
	}
}

// startupRollbackAttempt fills the descriptor table, frees three slots —
// one poller's worth — and starts two loops, which must fail and release
// the first loop's descriptors. unexpected reports a startup that fit after
// all.
func startupRollbackAttempt(ev *Events, fill func() (int, error), opened *[]int) (unexpected bool, initErr, failure error) {
	if _, err := fill(); err != nil {
		return false, nil, err
	}
	if len(*opened) < 3 {
		return false, nil, errors.New("not enough descriptors to exercise partial startup")
	}
	// One poller needs three descriptors on both epoll and kqueue. No waiter
	// starts until all pollers exist, so the second allocation must fail.
	var released []int
	for range 3 {
		last := len(*opened) - 1
		released = append(released, (*opened)[last])
		_ = unix.Close((*opened)[last])
		*opened = (*opened)[:last]
	}
	initErr = ev.initLoops()
	if initErr == nil {
		ev.shutdownLoops(nil)
		ev.loops = nil
		return true, nil, nil
	}
	if !errors.Is(initErr, unix.EMFILE) || ev.loops != nil {
		// Another freed descriptor let the first loop fail instead: this
		// attempt says nothing about the rollback either.
		return true, initErr, nil
	}
	for _, fd := range released {
		if _, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0); !errors.Is(err, unix.EBADF) {
			failure = fmt.Errorf("startup retained released descriptor %d: %v", fd, err)
		}
	}
	reclaimed, err := fill()
	// More available slots are harmless; all three startup slots must
	// nevertheless be reusable, and were checked individually above.
	if err != nil || reclaimed < len(released) {
		failure = fmt.Errorf("startup reclaimed %d slots, want at least %d: %v", reclaimed, len(released), err)
	}
	return false, initErr, failure
}

func TestNativeListenerGroupRollsBackASecondBindFailure(t *testing.T) {
	cause := errors.New("second listener refused by socket policy")
	a := &acceptor{events: &Events{ReusePort: true}}
	calls := 0
	firstFD := -1
	var bound string
	group, err := a.bindGroup("tcp4://127.0.0.1:0", func(_, addr string, fd uintptr) error {
		calls++
		if calls == 1 {
			firstFD = int(fd)
			return nil
		}
		bound = addr
		return cause
	})
	if !errors.Is(err, cause) || group != nil || calls < 2 {
		t.Fatalf("group=%v calls=%d err=%v", group, calls, err)
	}
	assertNativeDescriptorClosed(t, firstFD)
	probe, err := net.Listen("tcp4", bound)
	if err != nil {
		t.Fatalf("failed group retained its port: %v", err)
	}
	probe.Close()
}

func TestNativeDynamicListenerFailureDoesNotPublishSockets(t *testing.T) {
	ev, loop := newContractNativeLoop(t)
	_ = loop.poller.Close(nil)
	a := &acceptor{events: ev, accepting: true}
	if _, err := a.listenGroup("tcp4://127.0.0.1:0", listenConfig{}); err == nil {
		t.Fatal("listener started on a closed poller")
	}
	if len(a.listeners) != 0 || len(*loop.listeners.Load()) != 0 {
		t.Fatal("failed dynamic listen left a published socket")
	}
}
