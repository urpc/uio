package uio

import (
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// OnOpen may Dial on either backend: it never runs on an event loop.
func TestDialFromOnOpen(t *testing.T) {
	target, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	go func() {
		for {
			c, err := target.Accept()
			if err != nil {
				return
			}
			go func() { _, _ = io.Copy(io.Discard, c); _ = c.Close() }()
		}
	}()
	result := make(chan error, 1)
	events := &Events{Pollers: 1}
	events.OnOpen = func(c Conn) {
		if c.Userdata() != nil {
			return // the dialed connection's own OnOpen
		}
		dialed, err := events.Dial("tcp://"+target.Addr().String(), "dialed")
		if dialed != nil {
			_ = dialed.Close()
		}
		result <- err
	}
	serveInboundTest(t, events)
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("Dial from OnOpen: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Dial from OnOpen did not return")
	}
}

// A connection whose OnOpen blocks holds up no other connection of the same
// loop: one accepted after it opens and is served meanwhile.
func TestSlowOnOpenDoesNotStallTheLoop(t *testing.T) {
	release := make(chan struct{})
	var accepted atomic.Int32
	events := &Events{
		Pollers: 1,
		OnOpen: func(c Conn) {
			if accepted.Add(1) == 2 {
				<-release // the second accepted connection's OnOpen blocks
			}
		},
		OnData: func(c Conn) error {
			_, err := c.WriteTo(c)
			return err
		},
	}
	first, _ := serveInboundTest(t, events)
	defer close(release)
	addr := first.RemoteAddr().String()
	slow, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer slow.Close()
	for accepted.Load() < 2 {
		time.Sleep(time.Millisecond)
	}
	fast, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer fast.Close()
	if _, err := fast.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	_ = fast.SetReadDeadline(time.Now().Add(3 * time.Second))
	got := make([]byte, 4)
	if _, err := io.ReadFull(fast, got); err != nil || string(got) != "ping" {
		t.Fatalf("a connection echoed %q, %v while another's OnOpen blocked", got, err)
	}
}

// A Wake made before OnOpen has run, as on a connection Dial has just
// returned, runs its OnData after OnOpen, never before it.
func TestWakeBeforeOnOpenRunsAfterIt(t *testing.T) {
	var mu sync.Mutex
	var seq []string
	note := func(s string) {
		mu.Lock()
		seq = append(seq, s)
		mu.Unlock()
	}
	woke := make(chan struct{}, 1)
	events := &Events{
		Pollers: 1,
		OnOpen: func(c Conn) {
			if c.Userdata() == "dialed" {
				time.Sleep(50 * time.Millisecond)
				note("open")
			}
		},
		OnData: func(c Conn) error {
			if c.Userdata() == "dialed" {
				note("data")
				select {
				case woke <- struct{}{}:
				default:
				}
			}
			return nil
		},
	}
	serveInboundTest(t, events)
	target, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	go func() {
		c, err := target.Accept()
		if err == nil {
			defer c.Close()
			time.Sleep(2 * time.Second)
		}
	}()
	dialed, err := events.Dial("tcp://"+target.Addr().String(), "dialed")
	if err != nil {
		t.Fatal(err)
	}
	if err := dialed.Wake(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-woke:
	case <-time.After(3 * time.Second):
		t.Fatal("the Wake never ran OnData")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seq) < 2 || seq[0] != "open" || seq[1] != "data" {
		t.Fatalf("callbacks ran as %v, want OnOpen before the Wake's OnData", seq)
	}
}

// A connection closed right after Dial returns, possibly before its OnOpen,
// never gets OnClose without OnOpen.
func TestCloseBeforeOnOpenSkipsOnClose(t *testing.T) {
	target, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	go func() {
		for {
			c, err := target.Accept()
			if err != nil {
				return
			}
			go func() { _, _ = io.Copy(io.Discard, c); _ = c.Close() }()
		}
	}()
	var mu sync.Mutex
	opened := map[Conn]bool{}
	var orphans, closes atomic.Int32
	events := &Events{
		Pollers: 1,
		OnOpen: func(c Conn) {
			mu.Lock()
			opened[c] = true
			mu.Unlock()
		},
		OnClose: func(c Conn, _ error) {
			closes.Add(1)
			mu.Lock()
			if !opened[c] {
				orphans.Add(1)
			}
			mu.Unlock()
		},
	}
	serveInboundTest(t, events)
	for range 200 {
		c, err := events.Dial("tcp://"+target.Addr().String(), "dialed")
		if err != nil {
			t.Fatal(err)
		}
		_ = c.Close()
	}
	time.Sleep(200 * time.Millisecond)
	if n := orphans.Load(); n != 0 {
		t.Fatalf("%d of %d closes reached a connection that never got OnOpen", n, closes.Load())
	}
}

// A UDP server's child connection opens when its first datagram arrives and,
// having had OnOpen, gets OnClose when it closes.
func TestUDPChildGetsOnCloseAfterOnOpen(t *testing.T) {
	probe, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := probe.LocalAddr().String()
	_ = probe.Close()
	opened := make(chan struct{}, 1)
	closed := make(chan struct{}, 1)
	started := make(chan struct{})
	events := &Events{
		Pollers: 1,
		OnStart: func(*Events) { close(started) },
		OnOpen:  func(Conn) { opened <- struct{}{} },
		OnData: func(c Conn) error {
			_, _ = c.Discard(-1)
			return c.Close()
		},
		OnClose: func(Conn, error) { closed <- struct{}{} },
	}
	done := make(chan error, 1)
	go func() { done <- events.Serve("udp://" + addr) }()
	<-started
	t.Cleanup(func() {
		_ = events.Close(nil)
		<-done
	})
	client, err := net.Dial("udp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if _, err := client.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	for _, step := range []struct {
		name string
		ch   chan struct{}
	}{{"OnOpen", opened}, {"OnClose", closed}} {
		select {
		case <-step.ch:
		case <-time.After(2 * time.Second):
			t.Fatalf("the UDP child got no %s", step.name)
		}
	}
}
