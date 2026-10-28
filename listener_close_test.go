package uio

import (
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// serveClosing serves events until the test ends, then closes it and waits
// for Serve to return.
func serveClosing(t *testing.T, events *Events) {
	t.Helper()
	_, done := serveLoopback(t, events)
	t.Cleanup(func() {
		_ = events.Close(nil)
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("Serve did not return")
		}
	})
}

// waitBindable waits until addr can be bound again with a plain UDP socket.
func waitBindable(t *testing.T, addr string, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		probe, err := net.ListenPacket("udp4", addr)
		if err == nil {
			_ = probe.Close()
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s still bound %v after Close: %v", addr, within, err)
		}
		time.Sleep(time.Millisecond)
	}
}

// Listener.Close of a UDP listener frees its address before returning, even
// while its socket's turns are busy reading a peer's traffic.
func TestUDPListenerCloseFreesAddress(t *testing.T) {
	events := withFuncs(&Events{Pollers: 2}, FuncHandler{Read: func(c Conn) error {
		_, _ = c.Discard(-1)
		return nil
	}})
	serveClosing(t, events)
	ln, err := events.Listen("udp4://127.0.0.1:0", nil)
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	peer, err := net.DialUDP("udp4", nil, ln.Addr().(*net.UDPAddr))
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	stop := make(chan struct{})
	var sender sync.WaitGroup
	sender.Add(1)
	go func() {
		defer sender.Done()
		for {
			select {
			case <-stop:
				return
			default:
				_, _ = peer.Write([]byte("traffic"))
			}
		}
	}()
	defer func() { close(stop); sender.Wait() }()

	for i := range 200 {
		if err := ln.Close(); err != nil {
			t.Fatalf("round %d: Close: %v", i, err)
		}
		if ln, err = events.Listen("udp4://"+addr, nil); err != nil {
			t.Fatalf("round %d: Listen right after Close: %v", i, err)
		}
	}
	_ = ln.Close()
	waitBindable(t, addr, 0)
}

// A peer's callback may close its own listener: nothing deadlocks, the peer
// closes with net.ErrClosed, and the address is free once the callback's
// turn is over.
func TestUDPListenerCloseFromPeerCallback(t *testing.T) {
	var ln atomic.Pointer[Listener]
	closed := make(chan error, 1)
	returned := make(chan struct{})
	events := withFuncs(&Events{Pollers: 1}, FuncHandler{
		Read: func(c Conn) error {
			_, _ = c.Discard(-1)
			_ = (*ln.Load()).Close()
			close(returned)
			return nil
		},
		Close: func(_ Conn, err error) { closed <- err },
	})
	serveClosing(t, events)
	l, err := events.Listen("udp4://127.0.0.1:0", nil)
	if err != nil {
		t.Fatal(err)
	}
	ln.Store(&l)
	peer, err := net.DialUDP("udp4", nil, l.Addr().(*net.UDPAddr))
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	if _, err := peer.Write([]byte("close it")); err != nil {
		t.Fatal(err)
	}
	waitLifecycle(t, returned, "Close inside the peer's OnData")
	select {
	case err := <-closed:
		if !errors.Is(err, net.ErrClosed) {
			t.Fatalf("peer closed with %v, want net.ErrClosed", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("peer got no OnClose")
	}
	waitBindable(t, l.Addr().String(), 2*time.Second)
}

// A Control on a peer may close the listener whose socket it holds, from an
// application goroutine and from the peer's own callback alike; the address
// is free once Control has returned.
func TestUDPListenerCloseInsideControl(t *testing.T) {
	for _, fromCallback := range []bool{false, true} {
		name := "goroutine"
		if fromCallback {
			name = "callback"
		}
		t.Run(name, func(t *testing.T) {
			var ln atomic.Pointer[Listener]
			controlled := make(chan error, 1)
			opened := make(chan Conn, 1)
			closeInControl := func(c Conn) {
				controlled <- c.Control(func(uintptr) { _ = (*ln.Load()).Close() })
			}
			events := withFuncs(&Events{Pollers: 1}, FuncHandler{
				Open: func(c Conn) { opened <- c },
				Read: func(c Conn) error {
					_, _ = c.Discard(-1)
					if fromCallback {
						closeInControl(c)
					}
					return nil
				},
			})
			serveClosing(t, events)
			l, err := events.Listen("udp4://127.0.0.1:0", nil)
			if err != nil {
				t.Fatal(err)
			}
			ln.Store(&l)
			peer, err := net.DialUDP("udp4", nil, l.Addr().(*net.UDPAddr))
			if err != nil {
				t.Fatal(err)
			}
			defer peer.Close()
			if _, err := peer.Write([]byte("open")); err != nil {
				t.Fatal(err)
			}
			var conn Conn
			select {
			case conn = <-opened:
			case <-time.After(5 * time.Second):
				t.Fatal("peer did not open")
			}
			if !fromCallback {
				go closeInControl(conn)
			}
			select {
			case err := <-controlled:
				if err != nil {
					t.Fatalf("Control: %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("Control closing its listener did not return")
			}
			waitBindable(t, l.Addr().String(), 2*time.Second)
		})
	}
}

// Once Listener.Close has returned, its peers' sends never leave through the
// socket that takes over the descriptor number, even while the listener's own
// turn is held up in a callback and has not yet closed its peers: every
// datagram the remote end gets comes from the listener's address.
func TestUDPListenerCloseKeepsPeerSendsOffReusedDescriptor(t *testing.T) {
	release := make(chan struct{})
	holding := make(chan struct{}, 1)
	events := withFuncs(&Events{Pollers: 1}, FuncHandler{Read: func(c Conn) error {
		_, _ = c.Discard(-1)
		select {
		case holding <- struct{}{}:
			<-release
		default:
		}
		return nil
	}})
	serveClosing(t, events)
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	remote, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer remote.Close()
	remoteAddr := remote.LocalAddr().(*net.UDPAddr).AddrPort()

	ln, err := events.Listen("udp4://127.0.0.1:0", nil)
	if err != nil {
		t.Fatal(err)
	}
	local := ln.Addr().(*net.UDPAddr).AddrPort()
	packet := ln.(PacketListener)
	conn, err := packet.DialPeer(remoteAddr, nil)
	if err != nil {
		t.Fatal(err)
	}
	// The listener's turn takes the remote's datagram and stays in OnData.
	if _, err := remote.WriteToUDPAddrPort([]byte("hold"), local); err != nil {
		t.Fatal(err)
	}
	waitLifecycle(t, holding, "OnData holding the listener's turn")
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	// New sockets, one of them likely on the closed socket's number.
	var others []Listener
	for range 8 {
		other, err := events.Listen("udp4://127.0.0.1:0", nil)
		if err != nil {
			t.Fatal(err)
		}
		others = append(others, other)
	}
	defer func() {
		for _, other := range others {
			_ = other.Close()
		}
	}()
	for range 200 {
		if _, err := conn.Write([]byte("x")); err == nil {
			t.Fatal("a peer of a closed listener still sends")
		}
		if _, err := packet.WriteTo([]byte("y"), remoteAddr); !errors.Is(err, net.ErrClosed) {
			t.Fatalf("WriteTo on a closed listener = %v, want net.ErrClosed", err)
		}
		if err := conn.SetReadBuffer(1 << 16); err == nil {
			t.Fatal("a peer of a closed listener still sets socket options")
		}
	}
	_ = remote.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
	buffer := make([]byte, 64)
	for {
		_, source, err := remote.ReadFromUDPAddrPort(buffer)
		if err != nil {
			break
		}
		t.Fatalf("remote got a datagram from %v after the listener at %v closed", source, local)
	}
}

// Two Listener.Closes while a Control on a peer holds the socket — one from
// another goroutine, one from the Control itself — all return: the close
// waits only so long for the Control, and the address frees once it ends.
func TestUDPListenerDoubleCloseDuringControl(t *testing.T) {
	var ln atomic.Pointer[Listener]
	inControl := make(chan struct{})
	controlDone := make(chan struct{})
	events := withFuncs(&Events{Pollers: 1}, FuncHandler{
		Read: func(c Conn) error {
			_, _ = c.Discard(-1)
			_ = c.Control(func(uintptr) {
				close(inControl)
				time.Sleep(200 * time.Millisecond)
				// A second Close while the first one waits for this fn.
				_ = (*ln.Load()).Close()
				close(controlDone)
			})
			return nil
		},
	})
	serveClosing(t, events)
	l, err := events.Listen("udp4://127.0.0.1:0", nil)
	if err != nil {
		t.Fatal(err)
	}
	ln.Store(&l)
	peer, err := net.DialUDP("udp4", nil, l.Addr().(*net.UDPAddr))
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	if _, err := peer.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	waitLifecycle(t, inControl, "the peer's Control")
	closed := make(chan struct{})
	go func() { _ = l.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("Listener.Close did not return while a Control held its socket")
	}
	waitLifecycle(t, controlDone, "the Control")
	waitBindable(t, l.Addr().String(), 2*time.Second)
}
