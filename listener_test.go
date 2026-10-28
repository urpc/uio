package uio

import (
	"errors"
	"io"
	"net"
	"runtime"
	"sync"
	"testing"
	"time"
)

// listenEcho is an echo Events that records the Userdata each connection
// opened with.
type listenEcho struct {
	mu     sync.Mutex
	opened []any
	remote []string // each opened connection's peer address, in order
	closed chan error
}

func newListenEcho() *listenEcho { return &listenEcho{closed: make(chan error, 64)} }

func (h *listenEcho) events() *Events {
	return withFuncs(&Events{Pollers: 2}, FuncHandler{
		Open: func(c Conn) {
			h.mu.Lock()
			h.opened = append(h.opened, c.Userdata())
			h.remote = append(h.remote, c.RemoteAddr().String())
			h.mu.Unlock()
		},
		Read: func(c Conn) error {
			_, err := c.WriteTo(c)
			return err
		},
		Close: func(_ Conn, err error) {
			select {
			case h.closed <- err:
			default: // a test not counting closes leaves the rest here
			}
		},
	})
}

func (h *listenEcho) userdata() []any {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]any(nil), h.opened...)
}

// openedFrom reports the userdata of the connections whose peer is at addr.
// A connection nobody here dialed — a stray of another test on a host
// reusing freed ports — cannot pass for the one under test.
func (h *listenEcho) openedFrom(addr string) []any {
	h.mu.Lock()
	defer h.mu.Unlock()
	var mine []any
	for i, remote := range h.remote {
		if remote == addr {
			mine = append(mine, h.opened[i])
		}
	}
	return mine
}

func echoOnce(t *testing.T, c net.Conn, payload string) {
	t.Helper()
	if _, err := io.WriteString(c, payload); err != nil {
		t.Fatal(err)
	}
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	got := make([]byte, len(payload))
	if _, err := io.ReadFull(c, got); err != nil || string(got) != payload {
		t.Fatalf("echo of %q = %q, %v", payload, got, err)
	}
}

// waitRefused waits until nothing listens at addr any more. A listener Close
// freed is free to bind, so a bind that wins proves it gone — and takes the
// address back for its instant, where a dial loop would instead spew
// connections at whatever process reused the freed port meanwhile.
func waitRefused(t *testing.T, addr string) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(time.Millisecond) {
		ln, err := net.Listen("tcp", addr)
		if err == nil {
			_ = ln.Close()
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s still held after its listener closed: %v", addr, err)
		}
	}
}

// A listener added while serving accepts at once, starts its connections with
// its userdata, and once closed accepts no more while those connections stay.
func TestListenAddsAndClosesAnAddress(t *testing.T) {
	h := newListenEcho()
	events := h.events()
	_, done := serveLoopback(t, events)
	_ = done
	ln, err := events.Listen("tcp://127.0.0.1:0", WithUserdata("proxy-a"))
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	client, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	echoOnce(t, client, "before close")
	if got := h.openedFrom(client.LocalAddr().String()); len(got) != 1 || got[0] != "proxy-a" {
		t.Fatalf("the client's connection opened with %v, want [proxy-a]", got)
	}
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	if err := ln.Close(); err != nil {
		t.Fatalf("a second Close = %v", err)
	}
	waitRefused(t, addr)
	echoOnce(t, client, "after close")
}

// Listen needs a serving Events: before Serve and after Close it refuses, and
// Close takes the listeners Listen added with it.
func TestListenOutsideServe(t *testing.T) {
	h := newListenEcho()
	events := h.events()
	if _, err := events.Listen("tcp://127.0.0.1:0"); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("Listen before Serve = %v, want %v", err, net.ErrClosed)
	}
	_, done := serveLoopback(t, events)
	ln, err := events.Listen("tcp://127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_ = events.Close(nil)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return")
	}
	waitRefused(t, ln.Addr().String())
	if err := ln.Close(); err != nil {
		t.Fatalf("Close after the Events closed = %v", err)
	}
	if _, err := events.Listen("tcp://127.0.0.1:0"); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("Listen after Close = %v, want %v", err, net.ErrClosed)
	}
}

// A listener added in OnStart starts with the others once OnStart returns,
// and Serve keeps running.
func TestListenDuringOnStart(t *testing.T) {
	h := newListenEcho()
	events := h.events()
	added := make(chan Listener, 1)
	funcs(events).Start = func(ev *Events) {
		ln, err := ev.Listen("tcp://127.0.0.1:0", WithUserdata("early"))
		if err != nil {
			t.Error(err)
		}
		added <- ln
	}
	done := make(chan error, 1)
	go func() { done <- events.Serve() }()
	t.Cleanup(func() {
		_ = events.Close(nil)
		<-done
	})
	var ln Listener
	select {
	case ln = <-added:
	case err := <-done:
		t.Fatalf("Serve: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("OnStart did not run")
	}
	if ln == nil {
		t.FailNow()
	}
	client, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	echoOnce(t, client, "early")
	select {
	case err := <-done:
		t.Fatalf("Serve returned: %v", err)
	default:
	}
}

// Listeners are independent: closing one leaves the others accepting, each
// with its own userdata.
func TestListenersAreIndependent(t *testing.T) {
	h := newListenEcho()
	events := h.events()
	serveLoopback(t, events)
	first, err := events.Listen("tcp://127.0.0.1:0", WithUserdata(1))
	if err != nil {
		t.Fatal(err)
	}
	second, err := events.Listen("tcp://127.0.0.1:0", WithUserdata(2))
	if err != nil {
		t.Fatal(err)
	}
	_ = first.Close()
	waitRefused(t, first.Addr().String())
	client, err := net.Dial("tcp", second.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	echoOnce(t, client, "second")
	if got := h.userdata(); len(got) == 0 || got[len(got)-1] != 2 {
		t.Fatalf("the second listener's connection opened with %v, want 2", got)
	}
	_ = second.Close()
}

// Listeners opened and closed over and over, with clients dialing them all
// the while, never disturb the Events: a closing listener's descriptor goes
// only once nobody accepts on it.
func TestListenCloseChurnWhileDialing(t *testing.T) {
	h := newListenEcho()
	events := h.events()
	_, done := serveLoopback(t, events)
	rounds := 200
	if testing.Short() {
		rounds = 40
	}
	for round := range rounds {
		ln, err := events.Listen("tcp://127.0.0.1:0", WithUserdata(round))
		if err != nil {
			t.Fatal(err)
		}
		addr := ln.Addr().String()
		var wg sync.WaitGroup
		for range 4 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if c, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
					_ = c.Close()
				}
			}()
		}
		runtime.Gosched()
		_ = ln.Close()
		wg.Wait()
		select {
		case err := <-done:
			t.Fatalf("round %d: Serve returned %v", round, err)
		default:
		}
	}
}

// A UDP listener's peers start with its userdata and close with it.
func TestListenUDPPeersCloseWithTheirListener(t *testing.T) {
	h := newListenEcho()
	events := h.events()
	serveLoopback(t, events)
	ln, err := events.Listen("udp://127.0.0.1:0", WithUserdata("udp-proxy"))
	if err != nil {
		t.Fatal(err)
	}
	client, err := net.Dial("udp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	echoOnce(t, client, "datagram")
	if got := h.userdata(); len(got) != 1 || got[0] != "udp-proxy" {
		t.Fatalf("UDP peer opened with %v, want [udp-proxy]", got)
	}
	_ = ln.Close()
	select {
	case err := <-h.closed:
		if !errors.Is(err, net.ErrClosed) {
			t.Fatalf("UDP peer closed with %v, want %v", err, net.ErrClosed)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the UDP peer did not close with its listener")
	}
}
