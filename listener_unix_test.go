//go:build (linux || darwin || netbsd || freebsd || openbsd || dragonfly) && !stdio

package uio

import (
	"net"
	"testing"
	"time"

	"github.com/urpc/uio/internal/socket"
	"golang.org/x/sys/unix"
)

// retire waits for an accept in progress and refuses new ones, so a closing
// listener's descriptor outlives every accept that could still use it.
func TestListenerRetireWaitsForAccepts(t *testing.T) {
	l := &listener{}
	if !l.enter() {
		t.Fatal("an open listener refused an accept")
	}
	retired := make(chan struct{})
	go func() {
		l.retire()
		close(retired)
	}()
	select {
	case <-retired:
		t.Fatal("retire returned while an accept was in progress")
	case <-time.After(20 * time.Millisecond):
	}
	if l.enter() {
		t.Fatal("a retiring listener let a new accept in")
	}
	l.leave()
	select {
	case <-retired:
	case <-time.After(5 * time.Second):
		t.Fatal("retire did not return once the accept left")
	}
}

// With ReusePort, Listen gives the address the group of listeners Serve
// would, and Close takes all of them.
func TestListenReusePortGroup(t *testing.T) {
	listenersOverride = 3
	t.Cleanup(func() { listenersOverride = 0 })
	h := newListenEcho()
	events := h.events()
	events.ReusePort = true
	serveLoopback(t, events)
	ln, err := events.Listen("tcp://127.0.0.1:0", WithUserdata("group"))
	if err != nil {
		t.Fatal(err)
	}
	if members := len(ln.(*listenerGroup).members); members != 3 {
		t.Fatalf("ReusePort listener has %d members, want 3", members)
	}
	addr := ln.Addr().String()
	for range 6 {
		c, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		echoOnce(t, c, "group")
		_ = c.Close()
	}
	for _, ud := range h.userdata() {
		if ud != "group" {
			t.Fatalf("a connection of the group opened with %v", ud)
		}
	}
	_ = ln.Close()
	waitRefused(t, addr)
}

// A listener closed while its accepting is paused stays out of the poller:
// the pause's re-arm finds it closing and leaves it, and a listener that
// takes over its descriptor number accepts as usual.
func TestPausedListenerCloseBeforeResume(t *testing.T) {
	opened := make(chan Conn, 4)
	events := withFuncs(&Events{Pollers: 1}, FuncHandler{Open: func(c Conn) { signal(opened, c) }})
	serveClosing(t, events)
	for round := range 20 {
		ln, err := events.Listen("tcp4://127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		group := ln.(*listenerGroup)
		l := group.members[0]
		l.loop.pauseAccept(l)
		if err := ln.Close(); err != nil {
			t.Fatal(err)
		}
		next, err := events.Listen("tcp4://127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		time.Sleep(2 * minAcceptPause) // the closed listener's re-arm has run
		c, err := net.Dial("tcp4", next.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		select {
		case <-opened:
		case <-time.After(5 * time.Second):
			t.Fatalf("round %d: the listener after a paused one did not accept", round)
		}
		_ = c.Close()
		_ = next.Close()
	}
}

// A ReusePort group binds one socket family: what the address asked for, not
// the dual-stack sockets a scheme-less rebind of the resolved address would
// produce, so a tcp4 listener refuses IPv6 peers and a tcp6 one IPv4.
func TestReusePortGroupKeepsAddressFamily(t *testing.T) {
	hold := listenersOverride
	listenersOverride = 3
	defer func() { listenersOverride = hold }()
	for _, tc := range []struct {
		addr   string
		family uint8
		other  string
	}{
		{"tcp4://0.0.0.0:0", 4, "tcp6"},
		{"tcp6://[::]:0", 6, "tcp4"},
	} {
		t.Run(tc.addr, func(t *testing.T) {
			events := withFuncs(&Events{Pollers: 1, ReusePort: true}, FuncHandler{})
			serveClosing(t, events)
			ln, err := events.Listen(tc.addr)
			if err != nil {
				t.Fatal(err)
			}
			group := ln.(*listenerGroup)
			if len(group.members) < 2 {
				t.Fatalf("ReusePort group has %d members, want several", len(group.members))
			}
			for i, m := range group.members {
				family, err := socket.SocketFamily(int(m.fd))
				if err != nil {
					t.Fatal(err)
				}
				if family != tc.family {
					t.Fatalf("member %d family = %d, want %d", i, family, tc.family)
				}
			}
			if tc.family == 6 {
				// A live dial from the other family would test the same thing,
				// but on a shared host the random port may be bound by another
				// process's listener meanwhile: assert the property on the
				// socket itself. V6ONLY is what refuses IPv4 peers, and the
				// family check above pins the rest.
				for i, m := range group.members {
					v6only, err := unix.GetsockoptInt(int(m.fd), unix.IPPROTO_IPV6, unix.IPV6_V6ONLY)
					if err != nil {
						t.Fatalf("member %d IPV6_V6ONLY: %v", i, err)
					}
					if v6only != 1 {
						t.Fatalf("member %d IPV6_V6ONLY = %d, a tcp4 peer could be accepted", i, v6only)
					}
				}
			}
		})
	}
}
