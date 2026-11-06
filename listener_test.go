package uio

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

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
				sc, _ := SocketOf(c)
				controlled <- sc.Control(func(uintptr) {
					_ = (*ln.Load()).Close()
				})
			}
			events := withFuncs(&Events{Pollers: 1}, FuncHandler{Open: func(c Conn) {
				select {
				case opened <- c:
				default:
				}
			}, Read: func(c Conn) error {
				_, _ = c.Discard(-1)
				if fromCallback {
					closeInControl(c)
				}
				return nil
			}})
			serveClosing(t, events)
			l, err := events.Listen("udp4://127.0.0.1:0")
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
	ln, err := events.Listen("udp4://127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	local := ln.Addr().(*net.UDPAddr).AddrPort()
	packet := ln.(PacketListener)
	conn, err := packet.DialPeer(remoteAddr, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := remote.WriteToUDPAddrPort([]byte("hold"), local); err != nil {
		t.Fatal(err)
	}
	waitLifecycle(t, holding, "OnData holding the listener's turn")
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	var others []Listener // New sockets, one of them likely on the closed socket's number.

	for range 8 {
		other, err := events.Listen("udp4://127.0.0.1:0")
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
		sc, _ := SocketOf(conn)
		if err := sc.SetReadBuffer(1 << 16); err == nil {
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

func TestUDPListenerDoubleCloseDuringControl(t *testing.T) {
	var ln atomic.Pointer[Listener]
	inControl := make(chan struct{})
	controlDone := make(chan struct{})
	events := withFuncs(&Events{Pollers: 1}, FuncHandler{Read: func(c Conn) error {
		_, _ = c.Discard(-1)
		sc, _ := SocketOf(c)
		_ = sc.Control(func(uintptr) {
			close(inControl)
			time.Sleep(200 * time.Millisecond)
			_ = (*ln.Load()).Close()
			close(controlDone)
		})
		return nil
	}})
	serveClosing(t, events)
	l, err := events.Listen("udp4://127.0.0.1:0")
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
	go func() {
		_ = l.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("Listener.Close did not return while a Control held its socket")
	}
	waitLifecycle(t, controlDone, "the Control")
	waitBindable(t, l.Addr().String(), 2*time.Second)
}

func waitLifecycle(t *testing.T, done <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

func TestListenerCloseLifecycleOrderings(t *testing.T) {
	for _, order := range []string{"PLE", "PEL", "LPE", "LEP", "EPL", "ELP"} {
		t.Run(order, func(t *testing.T) {
			added := make(chan PacketListener, 1)
			leaveStart, leaveOpen := make(chan struct{}), make(chan struct{})
			opened, closed := make(chan struct{}), make(chan struct{})
			var releaseStart, releaseOpen sync.Once
			var inOpen, stopped atomic.Bool
			var opens, closes atomic.Int32
			var other Listener
			var cause error
			want := errors.New("peer requested close")
			ev := withFuncs(&Events{Pollers: 1}, FuncHandler{Start: func(ev *Events) {
				ln, err := ev.Listen("udp4://127.0.0.1:0")
				if err != nil {
					t.Error(err)
					return
				}
				other, err = ev.Listen("tcp4://127.0.0.1:0")
				if err != nil {
					t.Error(err)
					return
				}
				added <- ln.(PacketListener)
				<-leaveStart
			}, Open: func(Conn) {
				opens.Add(1)
				inOpen.Store(true)
				close(opened)
				<-leaveOpen
				inOpen.Store(false)
			}, Close: func(_ Conn, err error) {
				if inOpen.Load() || stopped.Load() {
					t.Error("OnClose overlapped OnOpen or ran after OnStop")
				}
				_ = other.Close()
				cause = err
				if closes.Add(1) == 1 {
					close(closed)
				}
			}, Stop: func(*Events) {
				stopped.Store(true)
			}})
			serveDone := make(chan struct{})
			go func() {
				_ = ev.Serve()
				close(serveDone)
			}()
			t.Cleanup(func() {
				releaseOpen.Do(func() {
					close(leaveOpen)
				})
				releaseStart.Do(func() {
					close(leaveStart)
				})
				_ = ev.Close(nil)
				waitLifecycle(t, serveDone, "Serve cleanup")
			})
			var ln PacketListener
			select {
			case ln = <-added:
			case <-time.After(5 * time.Second):
				t.Fatal("listener startup timed out")
			}
			peer, err := ln.DialPeer(netip.MustParseAddrPort("127.0.0.1:9876"), nil)
			if err != nil {
				t.Fatal(err)
			}
			waitLifecycle(t, opened, "OnOpen")
			for _, action := range order {
				switch action {
				case 'P':
					_ = peer.Abort(want)
				case 'L':
					_ = ln.Close()
				case 'E':
					_ = ev.Close(nil)
				}
			}
			releaseOpen.Do(func() {
				close(leaveOpen)
			})
			releaseStart.Do(func() {
				close(leaveStart)
			})
			waitLifecycle(t, serveDone, "Serve with reentrant OnClose")
			select {
			case <-closed:
			default:
				t.Fatal("Serve returned before OnClose")
			}
			if opens.Load() != 1 || closes.Load() != 1 {
				t.Fatalf("opens=%d closes=%d", opens.Load(), closes.Load())
			}
			if order[0] == 'P' && !errors.Is(cause, want) {
				t.Fatalf("peer's close cause = %v, want %v", cause, want)
			}
			if order[0] == 'L' && !errors.Is(cause, net.ErrClosed) {
				t.Fatalf("listener's close cause = %v, want net.ErrClosed", cause)
			}
		})
	}
}

func TestListenControlShutdownAdmission(t *testing.T) {
	for _, network := range []string{"tcp4", "udp4"} {
		t.Run(network, func(t *testing.T) {
			started, serveDone := make(chan struct{}), make(chan struct{})
			ev := withFuncs(&Events{Pollers: 1}, FuncHandler{Start: func(*Events) {
				close(started)
			}})
			go func() {
				_ = ev.Serve()
				close(serveDone)
			}()
			resume, entered := make(chan struct{}), make(chan struct{})
			var release sync.Once
			t.Cleanup(func() {
				release.Do(func() {
					close(resume)
				})
				go ev.Close(nil)
				waitLifecycle(t, serveDone, "Serve cleanup")
			})
			waitLifecycle(t, started, "OnStart")
			listenDone := make(chan struct{})
			go func() {
				ln, err := ev.Listen(network+"://127.0.0.1:0", WithListenControl(func(string, string, uintptr) error {
					close(entered)
					<-resume
					return nil
				}))
				if ln != nil || !errors.Is(err, net.ErrClosed) {
					t.Errorf("Listen after shutdown = %v, %v; want nil, net.ErrClosed", ln, err)
				}
				close(listenDone)
			}()
			waitLifecycle(t, entered, "socket control")
			go ev.Close(nil)
			waitLifecycle(t, serveDone, "shutdown during socket control")
			release.Do(func() {
				close(resume)
			})
			waitLifecycle(t, listenDone, "rejected listener admission")
		})
	}
}

type peerReporter struct {
	events   chan string
	payloads chan []byte
}

func newPeerReporter() *peerReporter {
	return &peerReporter{events: make(chan string, 16), payloads: make(chan []byte, 16)}
}

func (r *peerReporter) handler() FuncHandler {
	return FuncHandler{Open: func(Conn) {
		signal(r.events, "open")
	}, Read: func(c Conn) error {
		buffer := make([]byte, 2048)
		r.payloads <- append(buffer[:0], c.Peek(buffer)...)
		_, _ = c.Discard(-1)
		return nil
	}, Close: func(_ Conn, err error) {
		signal(r.events, "close:"+fmt.Sprint(err))
	}}
}

func (r *peerReporter) waitEvent(t *testing.T) string {
	t.Helper()
	select {
	case e := <-r.events:
		return e
	case <-time.After(5 * time.Second):
		t.Fatal("no callback ran")
		return ""
	}
}

type packetFixture struct {
	t        *testing.T
	events   *Events
	ln       Listener
	packet   PacketListener
	reporter *peerReporter
	peer     *net.UDPConn
	peerAddr netip.AddrPort
}

func startPacketFixture(t *testing.T) *packetFixture {
	t.Helper()
	reporter := newPeerReporter()
	events := withFuncs(&Events{Pollers: 2}, reporter.handler())
	serveLoopback(t, events)
	ln, err := events.Listen("udp://127.0.0.1:0", WithUserdata("listener-userdata"))
	if err != nil {
		t.Fatal(err)
	}
	udpLn := ln.Addr().(*net.UDPAddr)
	peer, err := net.DialUDP("udp", nil, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: udpLn.Port})
	if err != nil {
		t.Fatal(err)
	}
	local := peer.LocalAddr().(*net.UDPAddr)
	f := &packetFixture{t: t, ln: ln, packet: ln.(PacketListener), reporter: reporter, peer: peer, peerAddr: netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), uint16(local.Port))}
	t.Cleanup(func() {
		_ = peer.Close()
	})
	return f
}

func (f *packetFixture) peerSend(t *testing.T, payload string) {
	t.Helper()
	if _, err := f.peer.Write([]byte(payload)); err != nil {
		t.Fatal(err)
	}
}

func (f *packetFixture) peerRead(t *testing.T) (string, netip.AddrPort) {
	t.Helper()
	buffer := make([]byte, 2048)
	_ = f.peer.SetReadDeadline(time.Now().Add(5 * time.Second))
	n, addr, err := f.peer.ReadFrom(buffer)
	if err != nil {
		t.Fatal(err)
	}
	source, ok := addr.(*net.UDPAddr)
	if !ok {
		t.Fatalf("datagram source %v is not UDP", addr)
	}
	return string(buffer[:n]), source.AddrPort()
}

func TestListenerDialPeerPeerSpokeFirst(t *testing.T) {
	f := startPacketFixture(t)
	f.peerSend(t, "hello")
	if got := <-f.reporter.payloads; string(got) != "hello" {
		t.Fatalf("passive peer received %q, want hello", got)
	}
	if f.reporter.waitEvent(t) != "open" {
		t.Fatal("passive peer did not open")
	}
	conn, err := f.packet.DialPeer(f.peerAddr, "second")
	if !errors.Is(err, ErrPeerExists) {
		t.Fatalf("DialPeer = %v, want ErrPeerExists", err)
	}
	if conn == nil {
		t.Fatal("ErrPeerExists came without the existing connection")
	}
	if ud := conn.Userdata(); ud != "listener-userdata" {
		t.Fatalf("existing peer userdata = %v, want the listener's", ud)
	}
}

func TestListenerDialPeerAfterListenerClose(t *testing.T) {
	f := startPacketFixture(t)
	packet := f.ln.(PacketListener)
	if err := f.ln.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := packet.DialPeer(f.peerAddr, nil); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("DialPeer after Close = %v, want net.ErrClosed", err)
	}
	if _, err := packet.WriteTo([]byte("x"), f.peerAddr); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("WriteTo after Close = %v, want net.ErrClosed", err)
	}
}

func TestListenerDialPeerMappedAddress(t *testing.T) {
	f := startPacketFixture(t)
	mapped := netip.AddrPortFrom(netip.MustParseAddr("::ffff:127.0.0.1").Unmap().WithZone(""), f.peerAddr.Port())
	conn, err := f.packet.DialPeer(mapped, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("mapped")); err != nil {
		t.Fatal(err)
	}
	if got, _ := f.peerRead(t); got != "mapped" {
		t.Fatalf("peer got %q, want mapped", got)
	}
	f.peerSend(t, "back")
	if got := <-f.reporter.payloads; string(got) != "back" {
		t.Fatalf("dialed peer received %q, want back", got)
	}
}

func TestDialPeerRefusesUnusableAddress(t *testing.T) {
	f := startPacketFixture(t)
	cases := []struct {
		name string
		addr netip.AddrPort
	}{{"zero", netip.AddrPort{}}, {"ipv6 on udp4", netip.MustParseAddrPort("[2001:db8::9]:53")}}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			conn, err := f.packet.DialPeer(c.addr, nil)
			if err == nil {
				_ = conn.Close()
				t.Fatalf("DialPeer(%v) = a connection, want an error", c.addr)
			}
			if !errors.Is(err, syscall.EINVAL) && !errors.Is(err, syscall.EAFNOSUPPORT) {
				t.Fatalf("DialPeer(%v) = %v, want EINVAL or EAFNOSUPPORT", c.addr, err)
			}
		})
	}
}

type closeRecorder struct {
	mu     sync.Mutex
	closes []error
}

func (h *closeRecorder) events() *Events {
	return withFuncs(&Events{Pollers: 2}, FuncHandler{Read: func(c Conn) error {
		_, _ = c.WriteTo(c)
		return nil
	}, Close: func(_ Conn, err error) {
		h.mu.Lock()
		h.closes = append(h.closes, err)
		h.mu.Unlock()
	}})
}

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
			if err != nil && !errors.Is(err, net.ErrClosed) && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Fatalf("round %d: close reason = %v", round, err)
			}
		}
	}
}

func TestControlAfterPeerCloseListenerAlive(t *testing.T) {
	f := startPacketFixture(t)
	conn, err := f.packet.DialPeer(f.peerAddr, nil)
	if err != nil {
		t.Fatal(err)
	}
	sc, _ := SocketOf(conn)
	if err := sc.Control(func(uintptr) {
	}); err != nil {
		t.Fatalf("Control before close: %v", err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	if err := sc.Control(func(uintptr) {
	}); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("Control after peer close = %v, want %v", err, net.ErrClosed)
	}
	if _, err := f.packet.WriteTo([]byte("alive"), f.peerAddr); err != nil {
		t.Fatalf("WriteTo after a peer closed: %v", err)
	}
}

func TestReplacedPeerKeepsItsCloseCause(t *testing.T) {
	want := errors.New("custom end")
	const rounds = 200
	got := make(chan error, rounds*2)
	events := withFuncs(&Events{Pollers: 1}, FuncHandler{Read: func(c Conn) error {
		_, _ = c.Discard(-1)
		return nil
	}, Close: func(_ Conn, err error) {
		signal(got, err)
	}})
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
		_ = conn.Abort(want)
		if next, err := packet.DialPeer(target, nil); err == nil {
			created++
			_ = next.Abort(want)
		}
	}
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

type listenEcho struct {
	mu     sync.Mutex
	opened []any
	remote []string
	closed chan error
} // each opened connection's peer address, in order

func newListenEcho() *listenEcho {
	return &listenEcho{closed: make(chan error, 64)}
}

func (h *listenEcho) events() *Events {
	return withFuncs(&Events{Pollers: 2}, FuncHandler{Open: func(c Conn) {
		h.mu.Lock()
		h.opened = append(h.opened, c.Userdata())
		h.remote = append(h.remote, c.RemoteAddr().String())
		h.mu.Unlock()
	}, Read: func(c Conn) error {
		_, err := c.WriteTo(c)
		return err
	}, Close: func(_ Conn, err error) {
		select {
		case h.closed <- err:
		default:
		}
	}})
}

func (h *listenEcho) userdata() []any {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]any(nil), h.opened...)
}

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
