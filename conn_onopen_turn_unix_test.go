//go:build (linux || darwin || netbsd || freebsd || openbsd || dragonfly) && !stdio

package uio

import (
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// acceptorLayoutName labels the two accept layouts the contract tests run
// under.
func acceptorLayoutName(reusePort bool) string {
	if reusePort {
		return "reuseport-listeners"
	}
	return "single-listener"
}

// TestOnOpenRunsInThePoolTurn pins the callback contract on the accept path:
// OnOpen is delivered by the connection's io-pool turn — never on the
// accepting loop — even after accepted-stream registration moved in place on
// platforms whose sockets inherit the listener's options.
func TestOnOpenRunsInThePoolTurn(t *testing.T) {
	for _, reusePort := range []bool{false, true} {
		t.Run(acceptorLayoutName(reusePort), func(t *testing.T) {
			testOnOpenRunsInThePoolTurn(t, reusePort)
		})
	}
}

func testOnOpenRunsInThePoolTurn(t *testing.T, reusePort bool) {
	events := &Events{Pollers: 2, ReusePort: reusePort}
	const dials = 8

	var opens atomic.Int64
	var notOwner atomic.Bool
	events.OnOpen = func(conn Conn) {
		fdc := conn.(*fdConn)
		// runIOTask stamps the running worker; the open callback must see its
		// own goroutine there, which is what "inside a worker" means.
		if fdc.ioOwner.Load() != currentGoroutineID() {
			notOwner.Store(true)
		}
		opens.Add(1)
	}

	addrCh := make(chan string, 1)
	events.OnStart = func(ev *Events) {
		for _, l := range ev.acceptor.listeners {
			addrCh <- l.ln.Addr().String()
			return
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
	})

	var addr string
	select {
	case addr = <-addrCh:
	case <-time.After(3 * time.Second):
		t.Fatal("Serve did not start")
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
	for opens.Load() < dials && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if got := opens.Load(); got != dials {
		t.Fatalf("OnOpen ran %d times, want %d", got, dials)
	}
	if notOwner.Load() {
		t.Fatal("OnOpen ran outside the connection's io-pool turn")
	}
}

// TestAcceptShutdownRaceClosesEveryOpenedConn closes the server while dials
// are landing: whichever side of the in-place registration a connection is
// on, every connection that reached OnOpen must also reach OnClose before
// Serve returns, and Serve must return.
func TestAcceptShutdownRaceClosesEveryOpenedConn(t *testing.T) {
	for _, reusePort := range []bool{false, true} {
		t.Run(acceptorLayoutName(reusePort), func(t *testing.T) {
			testAcceptShutdownRace(t, reusePort)
		})
	}
}

func testAcceptShutdownRace(t *testing.T, reusePort bool) {
	for round := 0; round < 15; round++ {
		events := &Events{Pollers: 2, ReusePort: reusePort}
		var opened, closed atomic.Int64
		events.OnOpen = func(Conn) { opened.Add(1) }
		events.OnClose = func(Conn, error) { closed.Add(1) }

		addrCh := make(chan string, 1)
		events.OnStart = func(ev *Events) {
			for _, l := range ev.acceptor.listeners {
				addrCh <- l.ln.Addr().String()
				return
			}
		}
		done := make(chan error, 1)
		go func() { done <- events.Serve("tcp://127.0.0.1:0") }()

		var addr string
		select {
		case addr = <-addrCh:
		case <-time.After(3 * time.Second):
			t.Fatalf("round %d: Serve did not start", round)
		}

		var wg sync.WaitGroup
		for i := 0; i < 16; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				c, err := net.Dial("tcp", addr)
				if err != nil {
					return
				}
				// Hold the connection so some of them are open when the
				// server starts closing.
				time.Sleep(time.Duration(i%4) * time.Millisecond)
				_ = c.Close()
			}(i)
		}
		// Vary the moment the shutdown lands inside the accept stream.
		time.Sleep(time.Duration(round%4) * time.Millisecond)
		_ = events.Close(nil)
		wg.Wait()

		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("round %d: Serve returned %v", round, err)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("round %d: Serve did not stop", round)
		}
		if o, c := opened.Load(), closed.Load(); o != c {
			t.Fatalf("round %d: %d connections opened but %d closed", round, o, c)
		}
	}
}
