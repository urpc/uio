//go:build windows || stdio

package uio

import (
	"net/netip"
	"testing"
	"time"
)

// A datagram child's periodic wake timer stops with the child: the child's
// close path does not pass through closeDescriptor, so the stop must live in
// the terminal finishClose.
func TestWakeUDPChildIntervalStopsAtClose(t *testing.T) {
	rec := &wakeRecorder{}
	events := &Events{Pollers: 2, Handler: rec}
	started := make(chan struct{})
	events.Hooks = FuncHandler{Start: func(*Events) { close(started) }}
	done := make(chan error, 1)
	go func() { done <- events.Serve() }()
	t.Cleanup(func() { _ = events.Close(nil); <-done })
	<-started

	listener, err := events.Listen("udp://127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	packet := listener.(PacketListener)
	far, err := listenUDPLoopback(t)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = far.Close() })
	raddr, err := netip.ParseAddrPort(far.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	peer, err := packet.DialPeer(raddr, nil)
	if err != nil {
		t.Fatal(err)
	}
	child := peer.(*fdConn)
	if err := peer.SetWakeInterval(5 * time.Millisecond); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 2*time.Second, func() bool { return rec.fires() >= 2 })
	if err := peer.Close(); err != nil {
		t.Fatal(err)
	}
	// The close hands off asynchronously; its terminal cleanup is what stops
	// the timer, so wait for OnClose before judging.
	waitFor(t, 2*time.Second, func() bool { return rec.closed.Load() >= 1 })
	child.wakeMu.Lock()
	armed := !child.wakeAt.IsZero() || child.wakeTimer != nil
	child.wakeMu.Unlock()
	if armed {
		t.Fatal("the udp child's wake timer survived its close")
	}
	fires := rec.fires()
	time.Sleep(150 * time.Millisecond)
	if got := rec.fires(); got > fires {
		t.Fatalf("a closed udp child kept waking: %d then %d", fires, got)
	}
}
