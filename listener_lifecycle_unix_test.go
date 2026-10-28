//go:build (linux || darwin || netbsd || freebsd || openbsd || dragonfly) && !stdio

package uio

import (
	"errors"
	"net"
	"net/netip"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"weak"

	"github.com/urpc/uio/internal/poller"
	"github.com/urpc/uio/internal/socket"
)

func TestUnstartedUDPListenerOwnedWithoutReadiness(t *testing.T) {
	added := make(chan PacketListener, 1)
	leaveStart := make(chan struct{})
	var releaseStart sync.Once
	var opens atomic.Int32
	ev := withFuncs(&Events{Pollers: 1}, FuncHandler{
		Start: func(ev *Events) {
			ln, err := ev.Listen("udp4://127.0.0.1:0", nil)
			if err != nil {
				t.Error(err)
				return
			}
			added <- ln.(PacketListener)
			<-leaveStart
		},
		Open: func(Conn) { opens.Add(1) },
	})
	serveDone := make(chan struct{})
	go func() { _ = ev.Serve(); close(serveDone) }()
	t.Cleanup(func() {
		_ = ev.Close(nil)
		releaseStart.Do(func() { close(leaveStart) })
		waitLifecycle(t, serveDone, "Serve cleanup")
	})
	var ln PacketListener
	select {
	case ln = <-added:
	case <-time.After(5 * time.Second):
		t.Fatal("listener startup timed out")
	}
	server := ln.(packetListener).group.members[0].udpSvr
	loop := server.loop
	loop.udpMu.Lock()
	_, owned := loop.udpServers[server]
	loop.udpMu.Unlock()
	if !owned {
		t.Fatal("unstarted listener has no lifetime owner")
	}
	if loop.fdMap.Get(server.Fd()) == server {
		t.Fatal("unstarted listener is visible to stale poller events")
	}
	peer, err := net.Dial("udp4", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	if _, err := peer.Write([]byte("early")); err != nil {
		t.Fatal(err)
	}
	// An event already collected for the descriptor's previous owner must
	// not start this listener before OnStart returns.
	waiter := &loopWaiter{}
	loop.dispatch(waiter, []poller.Event{{FD: server.Fd(), Events: poller.ReadEvents}})
	loop.submit(waiter)
	loop.waitIODrained()
	if opens.Load() != 0 {
		t.Fatal("stale readiness started a callback during OnStart")
	}
	_ = ev.Close(nil)
	_ = ln.Close()
	releaseStart.Do(func() { close(leaveStart) })
	waitLifecycle(t, serveDone, "unstarted listener shutdown")
	if _, err := server.udp.file.Stat(); err == nil {
		t.Fatal("unstarted listener leaked its descriptor")
	}
	loop.udpMu.Lock()
	remaining := len(loop.udpServers)
	loop.udpMu.Unlock()
	if remaining != 0 {
		t.Fatal("released listeners remain in the lifetime registry")
	}
}

// Closing a child's address entry must transfer its ownership before that
// entry is replaced, even if its closer has not queued the server turn yet.
func TestUDPServerReleaseClosesReplacedPeers(t *testing.T) {
	closed := make(chan Conn, 2)
	ev := withFuncs(&Events{}, FuncHandler{Close: func(c Conn, _ error) { closed <- c }})
	if err := ev.initConfig(); err != nil {
		t.Fatal(err)
	}
	loop, err := newEventLoop(ev)
	if err != nil {
		t.Fatal(err)
	}
	defer loop.poller.Close(nil)
	defer loop.ioPool.stop()
	// Leave synthetic peer turns unrun, as when shutdown wins the hand-off.
	loop.stopping.Store(true)
	server := &fdConn{fd: -1, internal: true, udp: &unixUDPState{
		family: 4,
		peers:  make(map[socket.UDPAddress]*fdConn),
	}}
	server.loop = loop
	server.entry = ev.plainEntry()
	addr := netip.MustParseAddrPort("127.0.0.1:9876")
	old, err := server.dialPeer(addr, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := errors.New("old peer close")
	old.(*fdConn).beginClose(want)
	next, err := server.dialPeer(addr, nil)
	if err != nil {
		t.Fatal(err)
	}
	server.teardown(net.ErrClosed)
	loop.waitIODrained()
	if !old.(*fdConn).isReleased() || !next.(*fdConn).isReleased() || len(closed) != 2 {
		t.Fatalf("replaced peer released=%v, replacement released=%v, OnClose=%d",
			old.(*fdConn).isReleased(), next.(*fdConn).isReleased(), len(closed))
	}
	if len(server.udp.closing)+len(server.udp.opening)+len(server.udp.waking) != 0 {
		t.Fatal("released server retains peer hand-off queues")
	}
}

// A live Events must not retain closed listeners, their server connections,
// or application data just to make a later shutdown safe.
func TestClosedUDPListenersReleaseUserdata(t *testing.T) {
	started := make(chan struct{})
	ev := withFuncs(&Events{Pollers: 1}, FuncHandler{Start: func(*Events) { close(started) }})
	serveDone := make(chan struct{})
	go func() { _ = ev.Serve(); close(serveDone) }()
	t.Cleanup(func() { _ = ev.Close(nil); waitLifecycle(t, serveDone, "Serve cleanup") })
	waitLifecycle(t, started, "OnStart")
	refs := make([]weak.Pointer[[256 << 10]byte], 0, 32)
	for range cap(refs) {
		ref := func() weak.Pointer[[256 << 10]byte] {
			data := new([256 << 10]byte)
			ln, err := ev.Listen("udp4://127.0.0.1:0", data)
			if err != nil {
				t.Fatal(err)
			}
			if err := ln.Close(); err != nil {
				t.Fatal(err)
			}
			return weak.Make(data)
		}()
		refs = append(refs, ref)
	}
	ev.loops[0].waitIODrained()
	runtime.GC()
	for i, ref := range refs {
		if ref.Value() != nil {
			t.Errorf("closed listener %d still retains its userdata", i)
		}
	}
	runtime.KeepAlive(ev)
}

// An external Control holds a socket alias, rather than the submission lock.
// A close may release the original fd, but cannot invalidate that alias or
// let Events finish before the control returns and releases it.
func TestControlDescriptorLifetime(t *testing.T) {
	started, serveDone := make(chan struct{}), make(chan struct{})
	closed := make(chan struct{})
	ev := withFuncs(&Events{Pollers: 1}, FuncHandler{
		Start: func(*Events) { close(started) },
		Close: func(c Conn, _ error) {
			if c.Userdata() == "control" {
				close(closed)
			}
		},
	})
	go func() { _ = ev.Serve(); close(serveDone) }()
	resume := make(chan struct{})
	var release sync.Once
	t.Cleanup(func() {
		release.Do(func() { close(resume) })
		_ = ev.Close(nil)
		waitLifecycle(t, serveDone, "Serve cleanup")
	})
	waitLifecycle(t, started, "OnStart")
	ln, err := ev.Listen("tcp4://127.0.0.1:0", nil)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := ev.Dial("tcp4://"+ln.Addr().String(), "control")
	if err != nil {
		t.Fatal(err)
	}
	usingAlias, controlDone := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(controlDone)
		if err := conn.Control(func(fd uintptr) {
			if err := conn.Close(); err != nil {
				t.Error(err)
			}
			waitLifecycle(t, closed, "OnClose with active Control")
			if err := socket.SetNoDelay(int(fd), true); err != nil {
				t.Errorf("Control descriptor after original close: %v", err)
			}
			close(usingAlias)
			<-resume
		}); err != nil {
			t.Error(err)
		}
	}()
	waitLifecycle(t, usingAlias, "socket alias after close")
	_ = ev.Close(nil)
	select {
	case <-serveDone:
		t.Error("Serve returned before Control released its socket alias")
	case <-time.After(30 * time.Millisecond):
	}
	release.Do(func() { close(resume) })
	waitLifecycle(t, controlDone, "Control return")
	waitLifecycle(t, serveDone, "Serve after Control")
}
