//go:build (linux || darwin || netbsd || freebsd || openbsd || dragonfly) && !stdio

package uio

import (
	"net"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// TestReusePortListenersShareOnePort pins the ReusePort accept layout:
// several listeners bound to the same concrete port (even from a :0 request),
// spread over the loops, all serving connections.
func TestReusePortListenersShareOnePort(t *testing.T) {
	const listeners = 3
	listenersOverride = listeners
	t.Cleanup(func() { listenersOverride = 0 })

	events := &Events{Pollers: 2, ReusePort: true}
	addrCh := make(chan string, 1)
	events.OnStart = func(ev *Events) {
		if len(ev.acceptor.listeners) != listeners {
			t.Errorf("listeners = %d, want %d", len(ev.acceptor.listeners), listeners)
		}
		first := ""
		perLoop := map[*eventLoop]int{}
		for _, l := range ev.acceptor.listeners {
			perLoop[l.loop]++
			if first == "" {
				first = l.ln.Addr().String()
			} else if got := l.ln.Addr().String(); got != first {
				t.Errorf("listener bound %s, want the shared %s", got, first)
			}
		}
		if len(perLoop) != len(ev.loops) {
			t.Errorf("listeners went to %d loops, want all %d", len(perLoop), len(ev.loops))
		}
		addrCh <- first
	}

	const dials = 16
	var opened atomic.Int64
	events.OnOpen = func(Conn) { opened.Add(1) }

	done := make(chan error, 1)
	go func() { done <- events.Serve("tcp://127.0.0.1:0") }()
	t.Cleanup(func() {
		_ = events.Close(nil)
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(3 * time.Second):
			t.Error("Serve did not stop")
		}
	})

	var addr string
	select {
	case addr = <-addrCh:
	case <-time.After(3 * time.Second):
		t.Fatal("Serve did not start")
	}
	if addr == "" {
		t.Fatal("no address published")
	}

	var conns []net.Conn
	defer func() {
		for _, c := range conns {
			_ = c.Close()
		}
	}()
	for i := 0; i < dials; i++ {
		c, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		conns = append(conns, c)
	}
	deadline := time.Now().Add(5 * time.Second)
	for opened.Load() < dials && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if got := opened.Load(); got != dials {
		t.Fatalf("OnOpen ran %d times, want %d", got, dials)
	}
}

// TestCloseReleasesEveryListener pins the shutdown of a ReusePort server
// serving several addresses: closing the server releases every listener's
// descriptor and stops every loop's poller.
func TestCloseReleasesEveryListener(t *testing.T) {
	events := &Events{Pollers: 2, ReusePort: true}
	var fds []int
	var loops []*eventLoop
	started := make(chan struct{})
	events.OnStart = func(ev *Events) {
		for _, l := range ev.acceptor.listeners {
			fds = append(fds, l.fd)
		}
		loops = ev.loops
		close(started)
	}
	done := make(chan error, 1)
	go func() { done <- events.Serve("tcp://127.0.0.1:0", "tcp://127.0.0.1:0") }()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("Serve did not start")
	}
	if len(fds) < 4 {
		t.Fatalf("listeners = %d, want at least two per address", len(fds))
	}
	_ = events.Close(nil)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Serve did not stop")
	}
	for _, loop := range loops {
		if !loop.poller.Closed() {
			t.Error("a loop's poller was never closed")
		}
	}
	for _, fd := range fds {
		if _, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0); err != unix.EBADF {
			t.Errorf("listener fd %d still open after Close: %v", fd, err)
		}
	}
}

// TestSingleListenerWithoutReusePort pins that the default layout binds one
// listener per address.
func TestSingleListenerWithoutReusePort(t *testing.T) {
	events := &Events{Pollers: 2}
	checked := make(chan struct{})
	events.OnStart = func(ev *Events) {
		if len(ev.acceptor.listeners) != 1 {
			t.Errorf("listeners = %d, want 1", len(ev.acceptor.listeners))
		}
		close(checked)
	}
	done := make(chan error, 1)
	go func() { done <- events.Serve("tcp://127.0.0.1:0") }()
	select {
	case <-checked:
	case <-time.After(3 * time.Second):
		t.Fatal("Serve did not start")
	}
	_ = events.Close(nil)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Serve did not stop")
	}
}

// TestAcceptingWaitsForOnStart pins that no connection is accepted — and so
// no OnOpen runs — before OnStart returns, on both layouts: the listeners are
// bound before OnStart, but their loops start accepting only afterwards.
func TestAcceptingWaitsForOnStart(t *testing.T) {
	cases := []struct {
		name      string
		reusePort bool
	}{
		{name: "master-listener"},
		{name: "reuseport-acceptors", reusePort: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.reusePort {
				listenersOverride = 2
				t.Cleanup(func() { listenersOverride = 0 })
			}

			var opened atomic.Int64
			var client net.Conn
			events := &Events{
				Pollers:   2,
				ReusePort: tc.reusePort,
				OnOpen:    func(Conn) { opened.Add(1) },
			}
			// Dial while OnStart is still running: the connection then sits
			// in the listener's backlog, and any OnOpen before OnStart
			// returns is the race this test pins.
			events.OnStart = func(ev *Events) {
				addr := ""
				ev.acceptor.mux.Lock()
				for _, l := range ev.acceptor.listeners {
					addr = l.ln.Addr().String()
					break
				}
				ev.acceptor.mux.Unlock()
				if addr == "" {
					t.Error("no listener to dial")
					return
				}
				c, err := net.DialTimeout("tcp", addr, 2*time.Second)
				if err != nil {
					t.Errorf("dial during OnStart: %v", err)
					return
				}
				client = c
				deadline := time.Now().Add(300 * time.Millisecond)
				for time.Now().Before(deadline) {
					if opened.Load() != 0 {
						t.Error("a connection was opened before OnStart returned")
						return
					}
					time.Sleep(5 * time.Millisecond)
				}
			}

			done := make(chan error, 1)
			go func() { done <- events.Serve("tcp://127.0.0.1:0") }()
			t.Cleanup(func() {
				_ = events.Close(nil)
				select {
				case err := <-done:
					if err != nil {
						t.Error(err)
					}
				case <-time.After(3 * time.Second):
					t.Error("Serve did not stop")
				}
				if client != nil {
					_ = client.Close()
				}
			})

			// OnStart has returned and accepting began: the connection dialed
			// during OnStart must now be opened.
			deadline := time.Now().Add(5 * time.Second)
			for opened.Load() == 0 && time.Now().Before(deadline) {
				time.Sleep(2 * time.Millisecond)
			}
			if opened.Load() == 0 {
				t.Fatal("the connection dialed during OnStart was never opened")
			}
		})
	}
}

// TestWaitersHonorLockOSThread pins that every waiter takes the
// Events.LockOSThread association. The hook runs in every waiter with the
// association it took, so a loop that stops consulting the configuration
// fails the locked case and the waiters themselves are still accounted for in
// both.
func TestWaitersHonorLockOSThread(t *testing.T) {
	cases := []struct {
		name string
		lock bool
	}{
		{name: "locked", lock: true},
		{name: "unlocked", lock: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Pollers is capped by GOMAXPROCS.
			loops, perLoop := int32(min(2, runtime.GOMAXPROCS(0))), int32(2)
			loopWaitersOverride = int(perLoop)
			t.Cleanup(func() { loopWaitersOverride = 0 })

			var started atomic.Int32
			var tookLock, skippedLock atomic.Bool
			testHookWaiterStarted = func(locked bool) {
				started.Add(1)
				if locked {
					tookLock.Store(true)
				} else {
					skippedLock.Store(true)
				}
			}
			t.Cleanup(func() { testHookWaiterStarted = nil })

			events := &Events{Pollers: int(loops), LockOSThread: tc.lock}
			done := make(chan error, 1)
			go func() { done <- events.Serve("tcp://127.0.0.1:0") }()
			// Registered last so it runs first: no waiter may outlive Serve,
			// and none may read the hook after it is cleared.
			t.Cleanup(func() {
				_ = events.Close(nil)
				select {
				case err := <-done:
					if err != nil {
						t.Error(err)
					}
				case <-time.After(3 * time.Second):
					t.Error("Serve did not stop")
				}
			})

			deadline := time.Now().Add(3 * time.Second)
			for started.Load() != loops*perLoop && time.Now().Before(deadline) {
				time.Sleep(2 * time.Millisecond)
			}
			if got := started.Load(); got != loops*perLoop {
				t.Fatalf("waiters started = %d, want %d", got, loops*perLoop)
			}
			if tookLock.Load() != tc.lock || skippedLock.Load() == tc.lock {
				t.Errorf("waiters took the thread association = %v/%v, want all %v", tookLock.Load(), skippedLock.Load(), tc.lock)
			}
		})
	}
}
