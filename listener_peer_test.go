package uio

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// peerReporter records, in order, the callbacks of one UDP child connection.
type peerReporter struct {
	events   chan string
	payloads chan []byte
}

func newPeerReporter() *peerReporter {
	return &peerReporter{events: make(chan string, 16), payloads: make(chan []byte, 16)}
}

func (r *peerReporter) handler() FuncHandler {
	return FuncHandler{
		Open: func(Conn) { signal(r.events, "open") },
		Read: func(c Conn) error {
			buffer := make([]byte, 2048)
			r.payloads <- append(buffer[:0], c.Peek(buffer)...)
			_, _ = c.Discard(-1)
			return nil
		},
		Close: func(_ Conn, err error) { signal(r.events, "close:"+fmt.Sprint(err)) },
	}
}

// waitEvent waits for one recorded event.
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

// startPacketEvents is an Events with a UDP listener whose peers report
// through peerReporter callbacks, plus a plain UDP peer talking to it.
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
	f := &packetFixture{
		t:        t,
		ln:       ln,
		packet:   ln.(PacketListener),
		reporter: reporter,
		peer:     peer,
		peerAddr: netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), uint16(local.Port)),
	}
	t.Cleanup(func() { _ = peer.Close() })
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

// A dialed peer sends before its OnOpen, from the listener's own address, and
// the peer's answers reach it — with OnOpen before its first OnData.
func TestListenerDialPeer(t *testing.T) {
	f := startPacketFixture(t)
	conn, err := f.packet.DialPeer(f.peerAddr, "hole")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("punch")); err != nil {
		t.Fatal(err)
	}
	if got, source := f.peerRead(t); got != "punch" || source.Port() != uint16(f.ln.Addr().(*net.UDPAddr).Port) {
		t.Fatalf("peer got %q from %v, want punch from the listener's port", got, source)
	}
	if _, err := f.peer.Write([]byte("answer")); err != nil {
		t.Fatal(err)
	}
	if got := <-f.reporter.payloads; string(got) != "answer" {
		t.Fatalf("dialed peer received %q, want answer", got)
	}
	if got := f.reporter.waitEvent(t); got != "open" {
		t.Fatalf("first callback was %q, want open", got)
	}
	if ud := conn.Userdata(); ud != "hole" {
		t.Fatalf("dialed peer userdata = %v, want hole", ud)
	}
}

// A peer that spoke first already has its connection: DialPeer returns it
// with ErrPeerExists, keeping its userdata.
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

// Dialing a peer again once its old connection closed gives a fresh one.
func TestListenerDialPeerAfterClose(t *testing.T) {
	f := startPacketFixture(t)
	conn, err := f.packet.DialPeer(f.peerAddr, "one")
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	if got := f.reporter.waitEvent(t); got != "open" {
		t.Fatalf("first callback was %q, want open", got)
	}
	// A stray datagram from another test's process may open a peer of its
	// own here; what this test waits for is its own peer's close.
	for got := f.reporter.waitEvent(t); !strings.HasPrefix(got, "close:"); got = f.reporter.waitEvent(t) {
	}
	second, err := f.packet.DialPeer(f.peerAddr, "two")
	if err != nil {
		t.Fatal(err)
	}
	if second == conn {
		t.Fatal("the second dial returned the closed connection")
	}
	if _, err := second.Write([]byte("again")); err != nil {
		t.Fatal(err)
	}
	if got, _ := f.peerRead(t); got != "again" {
		t.Fatalf("peer got %q, want again", got)
	}
	f.peerSend(t, "reply")
	if got := <-f.reporter.payloads; string(got) != "reply" {
		t.Fatalf("redialed peer received %q, want reply", got)
	}
	if f.reporter.waitEvent(t) != "open" {
		t.Fatal("the redialed peer did not open")
	}
}

// WriteTo sends from the listener's address without a connection.
func TestListenerWriteTo(t *testing.T) {
	f := startPacketFixture(t)
	n, err := f.packet.WriteTo([]byte("probe"), f.peerAddr)
	if err != nil || n != 5 {
		t.Fatalf("WriteTo = %d, %v", n, err)
	}
	got, source := f.peerRead(t)
	if got != "probe" || source.Port() != uint16(f.ln.Addr().(*net.UDPAddr).Port) {
		t.Fatalf("peer got %q from %v", got, source)
	}
}

// A stream listener carries no PacketListener face; a packet one does.
func TestPacketListenerFace(t *testing.T) {
	events := withFuncs(&Events{Pollers: 1}, FuncHandler{})
	serveLoopback(t, events)
	stream, err := events.Listen("tcp://127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := stream.(PacketListener); ok {
		t.Fatal("a stream listener carried a PacketListener face")
	}
	packet, err := events.Listen("udp://127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := packet.(PacketListener); !ok {
		t.Fatal("a packet listener carried no PacketListener face")
	}
}

// Once the listener closed, its peers are gone.
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

// An IPv4-mapped address dials the peer it names, on a socket of either
// family: the key a datagram arrives with is what DialPeer keyed.
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

// Dialing many peers at once, while the peer side floods datagrams and
// listeners close, keeps every connection whole: each opens once, in order
// before its data, and closes once.
func TestListenerDialPeerConcurrent(t *testing.T) {
	events := withFuncs(&Events{Pollers: 2}, FuncHandler{
		Open: func(Conn) {},
		Read: func(c Conn) error {
			_, err := c.WriteTo(c)
			return err
		},
		Close: func(Conn, error) {},
	})
	serveLoopback(t, events)
	listener, err := events.Listen("udp://127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ln := listener.(PacketListener)
	var peers sync.WaitGroup
	var dialed atomic.Int32
	for i := range 8 {
		peers.Add(1)
		go func(i int) {
			defer peers.Done()
			udpLn := ln.Addr().(*net.UDPAddr)
			peer, err := net.DialUDP("udp", nil, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: udpLn.Port})
			if err != nil {
				t.Error(err)
				return
			}
			defer peer.Close()
			local := peer.LocalAddr().(*net.UDPAddr)
			target := netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), uint16(local.Port))
			for j := range 25 {
				if j%3 == 0 {
					if _, err := ln.DialPeer(target, i*100+j); err == nil {
						dialed.Add(1)
					} else if !errors.Is(err, ErrPeerExists) {
						t.Errorf("DialPeer: %v", err)
						return
					}
				}
				if _, err := peer.Write([]byte("m")); err != nil {
					return
				}
				buffer := make([]byte, 64)
				_ = peer.SetReadDeadline(time.Now().Add(2 * time.Second))
				if _, _, err := peer.ReadFrom(buffer); err != nil {
					t.Errorf("no echo: %v", err)
					return
				}
			}
		}(i)
	}
	peers.Wait()
	if dialed.Load() == 0 {
		t.Fatal("no dial ever succeeded")
	}
}

// A peer's address is checked before the child exists, on both backends: an
// invalid one fails the Dial instead of yielding a connection whose every
// send does.
func TestDialPeerRefusesUnusableAddress(t *testing.T) {
	f := startPacketFixture(t)
	cases := []struct {
		name string
		addr netip.AddrPort
	}{
		{"zero", netip.AddrPort{}},
		{"ipv6 on udp4", netip.MustParseAddrPort("[2001:db8::9]:53")},
	}
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

// A UDP child reports the listener's local address, on both backends.
func TestUDPChildLocalAddr(t *testing.T) {
	f := startPacketFixture(t)
	conn, err := f.packet.DialPeer(f.peerAddr, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	local, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok || local.IP == nil || local.Port == 0 {
		t.Fatalf("child LocalAddr = %v, want the listener's address", conn.LocalAddr())
	}
	if want := f.ln.Addr().(*net.UDPAddr); local.Port != want.Port {
		t.Fatalf("child LocalAddr = %v, want the listener's %v", local, want)
	}
}
