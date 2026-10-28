package uio

import (
	"errors"
	"io"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// closeRecorder counts the callbacks of every connection, and the reasons
// their closes reported.
type closeRecorder struct {
	mu     sync.Mutex
	closes []error
}

func (h *closeRecorder) events() *Events {
	return withFuncs(&Events{Pollers: 2}, FuncHandler{
		Read:  func(c Conn) error { _, _ = c.WriteTo(c); return nil },
		Close: func(_ Conn, err error) { h.mu.Lock(); h.closes = append(h.closes, err); h.mu.Unlock() },
	})
}

// Peer close, Listener.Close and Events.Close all racing on one UDP listener
// close every connection exactly once, with a reason that never changes.
func TestConcurrentCloseOfPeerListenerAndEvents(t *testing.T) {
	var wg sync.WaitGroup
	for round := range 20 {
		recorder := closeRecorder{}
		events := recorder.events()
		_, done := serveLoopback(t, events)
		ln, err := events.Listen("udp://127.0.0.1:0", WithUserdata("p"))
		if err != nil {
			t.Fatal(err)
		}
		packet := ln.(PacketListener)
		addr := ln.Addr().String()
		wg.Add(3)
		go func() {
			defer wg.Done()
			peer, err := net.Dial("udp", addr)
			if err != nil {
				return
			}
			_, _ = peer.Write([]byte("x"))
			time.Sleep(time.Duration(round) * time.Millisecond)
			_ = events.Close(nil)
		}()
		go func() {
			defer wg.Done()
			time.Sleep(time.Duration(round) * time.Millisecond)
			_ = ln.Close()
		}()
		go func() {
			defer wg.Done()
			deadline := time.Now().Add(2 * time.Second)
			for time.Now().Before(deadline) {
				if conn, err := packet.DialPeer(netip.MustParseAddrPort("127.0.0.1:1"), nil); err == nil {
					_ = conn.Close()
					return
				}
				time.Sleep(time.Millisecond)
			}
		}()
		wg.Wait()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatalf("round %d: Serve did not return", round)
		}
		recorder.mu.Lock()
		defer recorder.mu.Unlock()
		for _, err := range recorder.closes {
			// Whichever closer wins the race — the peer's own Close, the
			// listener's, the Events' — its reason is what the callback
			// sees: one of these, or nothing for a clean close.
			if err != nil && !errors.Is(err, net.ErrClosed) && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Fatalf("round %d: close reason = %v", round, err)
			}
		}
	}
}

// The two openers of a dialed UDP peer — its dial goroutine and the listener
// reading the peer's first datagram — run its OnOpen exactly once, whichever
// wins, and always before its OnData.
func TestDialedPeerOpensOnceBothSides(t *testing.T) {
	for _, first := range []string{"datagram", "dialer"} {
		t.Run(first, func(t *testing.T) {
			var mu sync.Mutex
			var order []string
			events := withFuncs(&Events{Pollers: 2}, FuncHandler{
				Open: func(Conn) { mu.Lock(); order = append(order, "open"); mu.Unlock() },
				Read: func(c Conn) error {
					mu.Lock()
					order = append(order, "data")
					mu.Unlock()
					_, _ = c.Discard(-1)
					return nil
				},
				Close: func(Conn, error) { mu.Lock(); order = append(order, "close"); mu.Unlock() },
			})
			serveLoopback(t, events)
			ln, err := events.Listen("udp://127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			peer, err := net.Dial("udp", ln.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			defer peer.Close()
			local := peer.LocalAddr().(*net.UDPAddr)
			target := netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), uint16(local.Port))
			if first == "datagram" {
				if _, err := peer.Write([]byte("early")); err != nil {
					t.Fatal(err)
				}
				// Give the reader a head start; the peer already spoke, so the
				// dial finds its connection.
				time.Sleep(20 * time.Millisecond)
			}
			if _, err := ln.(PacketListener).DialPeer(target, nil); err != nil && !errors.Is(err, ErrPeerExists) {
				t.Fatal(err)
			}
			if first == "dialer" {
				if _, err := peer.Write([]byte("late")); err != nil {
					t.Fatal(err)
				}
			}
			deadline := time.Now().Add(5 * time.Second)
			for time.Now().Before(deadline) {
				mu.Lock()
				done := len(order) >= 2 && order[0] == "open" && order[1] == "data"
				opens := 0
				for _, e := range order {
					if e == "open" {
						opens++
					}
				}
				mu.Unlock()
				if done {
					if opens != 1 {
						t.Fatalf("OnOpen ran %d times, order %v", opens, order)
					}
					return
				}
				time.Sleep(time.Millisecond)
			}
			mu.Lock()
			defer mu.Unlock()
			t.Fatalf("order = %v, want open before data", order)
		})
	}
}

// A mapped dial and the plain address of the same peer are one child on every
// backend: the datagram reaches the dialed one, and a plain dial finds it.
func TestMappedAndPlainPeerAreOneChild(t *testing.T) {
	var opens atomic.Int32
	var mu sync.Mutex
	var remotes []string
	events := withFuncs(&Events{Pollers: 1}, FuncHandler{
		Open: func(c Conn) { opens.Add(1); mu.Lock(); remotes = append(remotes, c.RemoteAddr().String()); mu.Unlock() },
		Read: func(c Conn) error { _, _ = c.Discard(-1); return nil },
	})
	serveLoopback(t, events)
	ln, err := events.Listen("udp://127.0.0.1:0", WithUserdata("mapped"))
	if err != nil {
		t.Fatal(err)
	}
	packet := ln.(PacketListener)
	peer, err := net.DialUDP("udp4", nil, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: ln.Addr().(*net.UDPAddr).Port})
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	local := peer.LocalAddr().(*net.UDPAddr).Port
	mapped := netip.MustParseAddrPort("[::ffff:127.0.0.1]:" + strconvItoa(local))
	if _, err := packet.DialPeer(mapped, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := peer.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	// Wait for the datagram: it must reach the child the mapped dial made,
	// not open one of the plain address.
	deadline := time.Now().Add(5 * time.Second)
	for opens.Load() < 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	conn, err := packet.DialPeer(netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), uint16(local)), nil)
	if !errors.Is(err, ErrPeerExists) {
		t.Fatalf("a plain dial of a mapped peer = %v, want ErrPeerExists", err)
	}
	_ = conn.Close()
	if got := opens.Load(); got != 1 {
		mu.Lock()
		defer mu.Unlock()
		t.Fatalf("the peer opened %d connections %v, want 1", got, remotes)
	}
}

func strconvItoa(v int) string {
	if v == 0 {
		return "0"
	}
	var b [8]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	return string(b[i:])
}

// No connection runs a callback while OnStart has not returned, on either
// backend: a listener Listen adds inside OnStart accepts only after it.
func TestNoCallbacksWhileOnStartRuns(t *testing.T) {
	inStart := make(chan struct{})
	leaveStart := make(chan struct{})
	var callbacks atomic.Int32
	events := withFuncs(&Events{Pollers: 2}, FuncHandler{
		Open:  func(Conn) { callbacks.Add(1) },
		Read:  func(c Conn) error { callbacks.Add(1); _, _ = c.Discard(-1); return nil },
		Close: func(Conn, error) { callbacks.Add(1) },
	})
	lnOnce := make(chan Listener, 1)
	funcs(events).Start = func(ev *Events) {
		ln, err := ev.Listen("tcp://127.0.0.1:0")
		if err != nil {
			t.Error(err)
		} else {
			lnOnce <- ln
		}
		close(inStart)
		<-leaveStart
	}
	done := make(chan error, 1)
	go func() { done <- events.Serve() }()
	t.Cleanup(func() {
		_ = events.Close(nil)
		<-done
	})
	select {
	case <-inStart:
	case err := <-done:
		t.Fatalf("Serve: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("OnStart did not run")
	}
	var ln Listener
	select {
	case ln = <-lnOnce:
	case <-time.After(5 * time.Second):
		t.Fatal("Listen in OnStart returned nothing")
	}
	client, err := net.DialTimeout("tcp", ln.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if _, err := client.Write([]byte("early")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if n := callbacks.Load(); n != 0 {
		t.Fatalf("%d callbacks ran while OnStart had not returned", n)
	}
	close(leaveStart)
	deadline := time.Now().Add(5 * time.Second)
	for callbacks.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if callbacks.Load() == 0 {
		t.Fatal("no callback ran after OnStart returned")
	}
}

// A peer closed while the Events is closing keeps its OnClose: DialPeer may
// not replace it once the close tasks stopped running.
func TestDialPeerRefusedWhileEventsCloses(t *testing.T) {
	var mu sync.Mutex
	var opens, closes int
	events := withFuncs(&Events{Pollers: 1}, FuncHandler{
		Open: func(Conn) { mu.Lock(); opens++; mu.Unlock() },
		Read: func(c Conn) error { _, _ = c.Discard(-1); return nil },
		Close: func(_ Conn, err error) {
			// The peer's own Close reports a clean end; the teardown one of
			// the shutdown causes.
			if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, net.ErrClosed) {
				t.Errorf("close reason = %v", err)
			}
			mu.Lock()
			closes++
			mu.Unlock()
		},
	})
	_, done := serveLoopback(t, events)
	ln, err := events.Listen("udp://127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	packet := ln.(PacketListener)
	conn, err := packet.DialPeer(netip.MustParseAddrPort("127.0.0.1:1"), "one")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = events.Close(nil) }()
	// Close the peer and dial its address over and over while the Events
	// closes: whatever order the two race in, the peer's OnClose arrives.
	for range 100 {
		_ = conn.Close()
		if next, err := packet.DialPeer(netip.MustParseAddrPort("127.0.0.1:1"), "two"); err == nil {
			conn = next
		}
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return")
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		settled := opens > 0 && opens == closes
		o, c := opens, closes
		mu.Unlock()
		if settled {
			return
		}
		if o > c {
			t.Fatalf("%d connections opened, %d closed", o, c)
		}
		time.Sleep(time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	t.Fatalf("%d connections opened, %d closed", opens, closes)
}

// A listener closed before it ever accepted — one added in OnStart and closed
// there — reports net.ErrClosed to its UDP peers, not an unexpected end.
func TestUnstartedListenerCloseCause(t *testing.T) {
	inStart := make(chan struct{})
	leaveStart := make(chan struct{})
	closed := make(chan error, 4)
	var ln Listener
	events := withFuncs(&Events{Pollers: 1}, FuncHandler{
		Open:  func(Conn) {},
		Read:  func(c Conn) error { _, _ = c.Discard(-1); return nil },
		Close: func(_ Conn, err error) { signal(closed, err) },
	})
	funcs(events).Start = func(ev *Events) {
		listener, err := ev.Listen("udp://127.0.0.1:0")
		if err != nil {
			t.Error(err)
		} else {
			ln = listener
			if _, err := listener.(PacketListener).DialPeer(netip.MustParseAddrPort("127.0.0.1:1"), nil); err != nil {
				t.Error(err)
			}
			_ = listener.Close()
		}
		close(inStart)
		<-leaveStart
	}
	done := make(chan error, 1)
	go func() { done <- events.Serve() }()
	t.Cleanup(func() {
		_ = events.Close(nil)
		<-done
	})
	select {
	case <-inStart:
	case err := <-done:
		t.Fatalf("Serve: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("OnStart did not run")
	}
	close(leaveStart)
	if ln == nil {
		t.FailNow()
	}
	select {
	case err := <-closed:
		if !errors.Is(err, net.ErrClosed) {
			t.Fatalf("peer close reason = %v, want %v", err, net.ErrClosed)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the peer of an unstarted listener never closed")
	}
}

// A peer dialed on a listener that never started accepting still gets its
// OnOpen before its OnClose when that listener closes: the release waits in
// line behind the opens DialPeer queued.
func TestUnstartedListenerCloseOrdersCallbacks(t *testing.T) {
	var mu sync.Mutex
	var order []string
	cause := make(chan error, 1)
	events := withFuncs(&Events{Pollers: 1}, FuncHandler{
		Open: func(Conn) { mu.Lock(); order = append(order, "open"); mu.Unlock() },
		Read: func(c Conn) error { _, _ = c.Discard(-1); return nil },
		Close: func(_ Conn, err error) {
			mu.Lock()
			order = append(order, "close")
			mu.Unlock()
			select {
			case cause <- err:
			default:
			}
		},
	})
	inStart := make(chan Listener, 1)
	started := make(chan struct{})
	funcs(events).Start = func(ev *Events) {
		ln, err := ev.Listen("udp://127.0.0.1:0")
		if err != nil {
			t.Error(err)
			startedOnceClose(started)
			return
		}
		inStart <- ln
		<-started // hold OnStart: the listener is not accepting yet
	}
	done := make(chan error, 1)
	go func() { done <- events.Serve() }()
	t.Cleanup(func() {
		_ = events.Close(nil)
		<-done
	})
	var ln Listener
	select {
	case ln = <-inStart:
	case err := <-done:
		t.Fatalf("Serve: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("OnStart did not run")
	}
	if _, err := ln.(PacketListener).DialPeer(netip.MustParseAddrPort("127.0.0.1:1"), nil); err != nil {
		t.Fatal(err)
	}
	_ = ln.Close()
	close(started)
	select {
	case err := <-cause:
		mu.Lock()
		defer mu.Unlock()
		if len(order) < 2 || order[0] != "open" || order[1] != "close" {
			t.Fatalf("order = %v, want open before close", order)
		}
		if !errors.Is(err, net.ErrClosed) {
			t.Fatalf("close reason = %v, want %v", err, net.ErrClosed)
		}
	case <-time.After(5 * time.Second):
		mu.Lock()
		defer mu.Unlock()
		t.Fatalf("no close; order = %v", order)
	}
}

func startedOnceClose(started chan struct{}) {
	select {
	case <-started:
	default:
		close(started)
	}
}

// A closed UDP peer's Control is refused while its listener lives on, and no
// callback of a dialed peer runs after Serve returned.
func TestControlAfterPeerCloseListenerAlive(t *testing.T) {
	f := startPacketFixture(t)
	conn, err := f.packet.DialPeer(f.peerAddr, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.Control(func(uintptr) {}); err != nil {
		t.Fatalf("Control before close: %v", err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	if err := conn.Control(func(uintptr) {}); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("Control after peer close = %v, want %v", err, net.ErrClosed)
	}
	// The listener's socket still answers.
	if _, err := f.packet.WriteTo([]byte("alive"), f.peerAddr); err != nil {
		t.Fatalf("WriteTo after a peer closed: %v", err)
	}
}

// DialPeer racing Events.Close never runs a callback after Serve returned.
func TestDialPeerRacingShutdownKeepsCallbacksInside(t *testing.T) {
	var mu sync.Mutex
	var callbacks int
	events := withFuncs(&Events{Pollers: 1}, FuncHandler{
		Open:  func(Conn) { mu.Lock(); callbacks++; mu.Unlock() },
		Read:  func(c Conn) error { _, _ = c.Discard(-1); return nil },
		Close: func(Conn, error) { mu.Lock(); callbacks++; mu.Unlock() },
	})
	_, done := serveLoopback(t, events)
	ln, err := events.Listen("udp://127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	packet := ln.(PacketListener)
	var dialers sync.WaitGroup
	for range 4 {
		dialers.Add(1)
		go func() {
			defer dialers.Done()
			for range 50 {
				if _, err := packet.DialPeer(netip.MustParseAddrPort("127.0.0.1:1"), nil); err != nil {
					return
				}
			}
		}()
	}
	time.Sleep(5 * time.Millisecond)
	_ = events.Close(nil)
	dialers.Wait()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return")
	}
	mu.Lock()
	atReturn := callbacks
	mu.Unlock()
	time.Sleep(100 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if callbacks != atReturn {
		t.Fatalf("%d callbacks ran after Serve returned", callbacks-atReturn)
	}
}

// The reviewer's shutdown-ownership repro: inside OnStart, Listen, DialPeer,
// Events.Close, then Listener.Close. Serve returns with the peer's OnClose
// delivered and its server released, not stranded between registries.
func TestUnstartedListenerReleasedWhenCloseRacesShutdown(t *testing.T) {
	var mu sync.Mutex
	var order []string
	var cause error
	events := withFuncs(&Events{Pollers: 1}, FuncHandler{
		Open: func(Conn) { mu.Lock(); order = append(order, "open"); mu.Unlock() },
		Read: func(c Conn) error { _, _ = c.Discard(-1); return nil },
		Close: func(_ Conn, err error) {
			mu.Lock()
			order = append(order, "close")
			cause = err
			mu.Unlock()
		},
	})
	lnOnce := make(chan Listener, 1)
	release := make(chan struct{})
	funcs(events).Start = func(ev *Events) {
		ln, err := ev.Listen("udp://127.0.0.1:0")
		if err != nil {
			t.Error(err)
			close(release)
			return
		}
		if _, err := ln.(PacketListener).DialPeer(netip.MustParseAddrPort("127.0.0.1:1"), nil); err != nil {
			t.Error(err)
		}
		lnOnce <- ln
		<-release // let the test drive the closing order
	}
	done := make(chan error, 1)
	go func() { done <- events.Serve() }()
	var ln Listener
	select {
	case ln = <-lnOnce:
	case err := <-done:
		t.Fatalf("Serve: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("OnStart did not run")
	}
	go func() { _ = events.Close(nil) }()
	time.Sleep(5 * time.Millisecond)
	_ = ln.Close()
	close(release)
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, net.ErrClosed) {
			t.Fatalf("Serve returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return")
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		opened, closed := false, false
		for _, e := range order {
			opened = opened || e == "open"
			closed = closed || e == "close"
		}
		gotCause := cause
		mu.Unlock()
		if opened && closed {
			if !errors.Is(gotCause, net.ErrClosed) && !errors.Is(gotCause, io.ErrUnexpectedEOF) {
				t.Fatalf("close reason = %v", gotCause)
			}
			return
		}
		time.Sleep(time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	t.Fatalf("order = %v: the peer was stranded without its OnClose", order)
}

// A peer closed with a custom cause keeps it even when DialPeer replaces it
// at the worst moment: the queue just took the close task.
func TestReplacedPeerKeepsItsCloseCause(t *testing.T) {
	want := errors.New("custom end")
	const rounds = 200
	got := make(chan error, rounds*2)
	events := withFuncs(&Events{Pollers: 1}, FuncHandler{
		Read:  func(c Conn) error { _, _ = c.Discard(-1); return nil },
		Close: func(_ Conn, err error) { signal(got, err) },
	})
	_, done := serveLoopback(t, events)
	t.Cleanup(func() {
		_ = events.Close(nil)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("Serve did not stop")
		}
	})
	ln, err := events.Listen("udp://127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	packet := ln.(PacketListener)
	target := netip.MustParseAddrPort("127.0.0.1:1")
	created := 0
	for range rounds {
		conn, err := packet.DialPeer(target, nil)
		if err != nil {
			t.Fatal(err)
		}
		created++
		_ = conn.CloseWith(want)
		if next, err := packet.DialPeer(target, nil); err == nil {
			created++
			_ = next.CloseWith(want)
		}
	}
	// Closing the listener leaves Events serving, so wait for every close,
	// rather than allowing a shutdown cause to hide a missing peer release.
	_ = ln.Close()
	for range created {
		select {
		case err := <-got:
			if !errors.Is(err, want) {
				t.Fatalf("close reason = %v, want %v", err, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("a replaced peer never reported its close")
		}
	}
}
