//go:build (linux || darwin || netbsd || freebsd || openbsd || dragonfly) && !stdio

package uio

import (
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// eachWaitMode runs test with the loops' waiters parked or adapting, as this
// host sizes them, and blocking in the kernel, as a wide host's do.
func eachWaitMode(t *testing.T, test func(t *testing.T)) {
	t.Run("sized", test)
	t.Run("blocking", func(t *testing.T) {
		blockingLoops(t)
		test(t)
	})
}

// A loop hands its listeners to its accept runner, which takes every
// connection of a burst: its edge-triggered listener reports no backlog
// twice.
func TestAcceptRunnerTakesEveryConnectionOfABurst(t *testing.T) {
	eachWaitMode(t, testAcceptRunnerTakesEveryConnectionOfABurst)
}

func testAcceptRunnerTakesEveryConnectionOfABurst(t *testing.T) {
	var opened atomic.Int32
	events := &Events{Pollers: 1, Handler: FuncHandler{Open: func(Conn) { opened.Add(1) }}}
	addr, _ := serveLoopback(t, events)
	if events.loops[0].accepts == nil {
		t.Fatal("a loop has no accept runner")
	}
	const n = 256
	clients := make([]net.Conn, n)
	var wg sync.WaitGroup
	for i := range clients {
		wg.Go(func() {
			c, err := net.Dial("tcp", addr)
			if err != nil {
				t.Error(err)
				return
			}
			clients[i] = c
		})
	}
	wg.Wait()
	defer func() {
		for _, c := range clients {
			if c != nil {
				_ = c.Close()
			}
		}
	}()
	deadline := time.Now().Add(5 * time.Second)
	for opened.Load() < n && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := opened.Load(); got != n {
		t.Fatalf("%d of %d connections opened", got, n)
	}
}

// A connection whose edge a waiter collected while the runner was finishing
// a drain is accepted: the runner finds the edge recorded and drains again,
// although the socket had just reported it would block.
func TestAcceptRunnerDrainsAgainForALateEdge(t *testing.T) {
	eachWaitMode(t, testAcceptRunnerDrainsAgainForALateEdge)
}

func testAcceptRunnerDrainsAgainForALateEdge(t *testing.T) {
	var (
		once     atomic.Bool
		target   atomic.Pointer[string]
		late     net.Conn
		lateEdge = make(chan bool, 1)
	)
	// The hook is in place before the runner starts, and cleared after the
	// server it runs in has closed.
	testHookAcceptDrained = func(l *listener) {
		if !once.CompareAndSwap(false, true) {
			return
		}
		// The second connection queues after the would-block; its edge
		// reaches the waiter while this drain still runs.
		c, err := net.Dial("tcp", *target.Load())
		if err != nil {
			lateEdge <- false
			return
		}
		late = c
		deadline := time.Now().Add(5 * time.Second)
		for l.acceptState.Load() != acceptAgain && time.Now().Before(deadline) {
			time.Sleep(100 * time.Microsecond)
		}
		lateEdge <- l.acceptState.Load() == acceptAgain
	}
	t.Cleanup(func() { testHookAcceptDrained = nil })
	opened := make(chan Conn, 4)
	events := &Events{Pollers: 1, Handler: FuncHandler{Open: func(c Conn) { signal(opened, c) }}}
	addr, _ := serveLoopback(t, events)
	target.Store(&addr)
	first, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	for i := range 2 {
		select {
		case <-opened:
		case <-time.After(5 * time.Second):
			t.Fatalf("connection %d of 2 never opened", i+1)
		}
	}
	if !<-lateEdge {
		t.Fatal("the waiter did not record the late edge")
	}
	_ = late.Close()
}

// A listener paused for want of descriptors is watched again after the
// pause, and the connections that queued meanwhile are accepted by the
// runner: re-registering the edge-triggered listener reports its backlog.
func TestAcceptRunnerResumesAPausedListener(t *testing.T) {
	eachWaitMode(t, testAcceptRunnerResumesAPausedListener)
}

func testAcceptRunnerResumesAPausedListener(t *testing.T) {
	opened := make(chan Conn, 4)
	events := &Events{Pollers: 1, Handler: FuncHandler{Open: func(c Conn) { signal(opened, c) }}}
	addr, _ := serveLoopback(t, events)
	var l *listener
	for _, member := range events.acceptor.listeners {
		l = member
	}
	l.loop.pauseAccept(l)
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	select {
	case <-opened:
	case <-time.After(5 * time.Second):
		t.Fatal("the connection queued during the pause never opened")
	}
}

// handOff wakes the runner for an idle listener and only records an edge
// for one being drained.
func TestAcceptRunnerHandOffRecordsEdgesWhileDraining(t *testing.T) {
	r := &acceptRunner{kick: make(chan struct{}, 1)}
	l := &listener{}
	if !r.handOff(l) {
		t.Fatal("handing off an idle listener did not wake the runner")
	}
	if len(r.kick) != 1 {
		t.Fatal("no kick queued")
	}
	for range 2 {
		if r.handOff(l) {
			t.Fatal("handing off a listener being drained woke the runner again")
		}
		if state := l.acceptState.Load(); state != acceptAgain {
			t.Fatalf("state %d after an edge while draining, want acceptAgain", state)
		}
	}
}
