//go:build (linux || darwin || netbsd || freebsd || openbsd || dragonfly) && !stdio

package uio

import (
	"net"
	"sync/atomic"
	"testing"
	"time"
)

// TestMultiAcceptorSharesOnePort pins the ReusePort accept layout: several
// listeners bound to the same concrete port (even from a :0 request), one
// per acceptor goroutine, all serving connections.
func TestMultiAcceptorSharesOnePort(t *testing.T) {
	const acceptors = 3
	multiAcceptorsOverride = acceptors
	t.Cleanup(func() { multiAcceptorsOverride = 0 })

	events := &Events{Pollers: 2, ReusePort: true}
	addrCh := make(chan string, 1)
	events.OnStart = func(ev *Events) {
		if len(ev.acceptor.multis) != 1 {
			t.Errorf("acceptor groups = %d, want 1", len(ev.acceptor.multis))
			addrCh <- ""
			return
		}
		multi := ev.acceptor.multis[0]
		if len(multi.lns) != acceptors {
			t.Errorf("acceptors = %d, want %d", len(multi.lns), acceptors)
		}
		if len(ev.acceptor.listeners) != acceptors {
			t.Errorf("registered listeners = %d, want %d", len(ev.acceptor.listeners), acceptors)
		}
		first := multi.lns[0].ln.Addr().String()
		for _, l := range multi.lns[1:] {
			if got := l.ln.Addr().String(); got != first {
				t.Errorf("listener bound %s, want the shared %s", got, first)
			}
		}
		addrCh <- first
	}

	const dials = 16
	var opened, closed atomic.Int64
	events.OnOpen = func(Conn) { opened.Add(1) }
	events.OnClose = func(Conn, error) { closed.Add(1) }

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

// TestMultiAcceptorClosesEveryAddress pins the shutdown of a ReusePort server
// serving several addresses: every address keeps its own acceptor group, and
// closing the server must release all of them — one group's poller left open
// would park its acceptor goroutine and leak the descriptor, which the kernel
// never reports by closing the listener alone.
func TestMultiAcceptorClosesEveryAddress(t *testing.T) {
	events := &Events{Pollers: 2, ReusePort: true}
	var groups []*multiAcceptor
	started := make(chan struct{})
	events.OnStart = func(ev *Events) {
		groups = append(groups, ev.acceptor.multis...)
		close(started)
	}
	done := make(chan error, 1)
	go func() { done <- events.Serve("tcp://127.0.0.1:0", "tcp://127.0.0.1:0") }()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("Serve did not start")
	}
	if len(groups) != 2 {
		t.Fatalf("acceptor groups = %d, want one per address", len(groups))
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
	for i, multi := range groups {
		for j, np := range multi.polls {
			if !np.Closed() {
				t.Errorf("group %d poller %d was never closed", i, j)
			}
		}
	}
}

// TestMultiAcceptorRequiresReusePort pins that the default single-listener
// layout is untouched: no extra listeners, no acceptor goroutines.
func TestMultiAcceptorRequiresReusePort(t *testing.T) {
	events := &Events{Pollers: 2}
	checked := make(chan struct{})
	events.OnStart = func(ev *Events) {
		if len(ev.acceptor.multis) != 0 {
			t.Error("multi-acceptors created without ReusePort")
		}
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
// no OnOpen runs — before OnStart returns, on both layouts. The dedicated
// ReusePort acceptors used to start while the listeners were being set up,
// before Serve reached OnStart, so a client that knew the address could open
// a connection while the application was still initializing; the master
// listener never had that window, because it begins accepting only when its
// loop starts, after OnStart.
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
				multiAcceptorsOverride = 2
				t.Cleanup(func() { multiAcceptorsOverride = 0 })
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

// TestMultiAcceptorHonorsLockOSThread pins that the dedicated acceptors take
// the Events.LockOSThread association the master loop and the data waiters
// already take. The hook runs in every acceptor goroutine with the
// association that goroutine took, so a loop that stops consulting the
// configuration fails the locked case and the goroutines themselves are
// still accounted for in both.
func TestMultiAcceptorHonorsLockOSThread(t *testing.T) {
	cases := []struct {
		name string
		lock bool
	}{
		{name: "locked", lock: true},
		{name: "unlocked", lock: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			const acceptors = 2
			multiAcceptorsOverride = acceptors
			t.Cleanup(func() { multiAcceptorsOverride = 0 })

			var started atomic.Int32
			var tookLock atomic.Bool
			testHookAcceptorThreadStarted = func(locked bool) {
				started.Add(1)
				if locked {
					tookLock.Store(true)
				}
			}
			t.Cleanup(func() { testHookAcceptorThreadStarted = nil })

			events := &Events{Pollers: 2, ReusePort: true, LockOSThread: tc.lock}
			done := make(chan error, 1)
			go func() { done <- events.Serve("tcp://127.0.0.1:0") }()
			// Registered last so it runs first: no acceptor goroutine may
			// outlive Serve, and none may read the hook after it is cleared.
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
			for started.Load() != acceptors && time.Now().Before(deadline) {
				time.Sleep(2 * time.Millisecond)
			}
			if got := started.Load(); got != acceptors {
				t.Fatalf("acceptor goroutines started = %d, want %d", got, acceptors)
			}
			if got := tookLock.Load(); got != tc.lock {
				t.Errorf("acceptor goroutines took the thread association = %v, want %v", got, tc.lock)
			}
		})
	}
}
