//go:build (linux || darwin || netbsd || freebsd || openbsd || dragonfly) && !stdio

package uio

import (
	"bytes"
	"errors"
	"io"
	"net"
	"testing"

	"golang.org/x/sys/unix"
)

func TestNativeConnectionUnavailableAndClosedAPIBoundaries(t *testing.T) {
	events := &Events{}
	conn := &fdConn{fd: -1}
	conn.entry = events.plainEntry()
	for _, operation := range []struct {
		name string
		call func() error
	}{
		{"Flush", conn.Flush},
		{"CloseWrite", conn.CloseWrite},
		{"CloseGracefully", func() error { return conn.CloseGracefully(nil) }},
		{"CloseWith", func() error { return conn.CloseWith(nil) }},
		{"Control", func() error {
			return conn.Control(func(uintptr) { t.Error("unavailable descriptor reached callback") })
		}},
	} {
		if err := operation.call(); !errors.Is(err, net.ErrClosed) {
			t.Errorf("unavailable %s = %v", operation.name, err)
		}
	}
	if conn.RemoteAddr() != nil {
		t.Fatal("unavailable connection invented a remote address")
	}
	if err := conn.YieldRead(); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("external yield = %v", err)
	}
	if n, err := conn.WriteOwned(nil); n != 0 || err != nil {
		t.Fatalf("nil owned write=%d,%v", n, err)
	}
	if n, err := conn.WriteOwned(AcquireBuffer(0)); n != 0 || err != nil {
		t.Fatalf("empty owned write=%d,%v", n, err)
	}
	if reserved, err := conn.ReserveOutbound(0); reserved != nil || err != nil {
		t.Fatalf("empty reservation=%v,%v", reserved, err)
	}
	if err := conn.ReserveInbound(0); err != nil {
		t.Fatal(err)
	}
	conn.loop = &eventLoop{}
	conn.loop.stopping.Store(true)
	if err := conn.Flush(); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("stopped Flush=%v", err)
	}
	conn.loop.stopping.Store(false)
	conn.close.phase.Store(closeRequested)
	if err := conn.YieldRead(); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("closed yield=%v", err)
	}
	owned := AcquireBuffer(7)
	_, _ = owned.WriteString("payload")
	if n, err := conn.WriteOwned(owned); n != 0 || !errors.Is(err, net.ErrClosed) {
		t.Fatalf("closed owned write=%d,%v", n, err)
	}
}

// A producer may enqueue before OnOpen returns. Reject oversized input before
// copying it, and reject a full queue without modifying the earlier payload.
func TestNativeProducerBudgetPreflightPreservesAcceptedPayload(t *testing.T) {
	for _, api := range []string{"Write", "Writev"} {
		t.Run(api, func(t *testing.T) {
			conn := &fdConn{fd: -1}
			conn.loop, conn.entry = &eventLoop{}, (&Events{MaxOutboundBuffered: 8}).plainEntry()
			defer conn.outbound.Reset()
			write := func(data []byte) (int, error) {
				if api == "Writev" {
					return conn.Writev([][]byte{data[:1], data[1:]})
				}
				return conn.Write(data)
			}
			if n, err := write([]byte("oversized")); n != 0 || !errors.Is(err, ErrOutboundOverflow) || conn.OutboundBuffered() != 0 {
				t.Fatalf("oversized producer=%d,%v; queued=%d", n, err, conn.OutboundBuffered())
			}
			accepted := []byte("accepted")
			if n, err := write(accepted); n != len(accepted) || err != nil {
				t.Fatalf("initial producer=%d,%v", n, err)
			}
			clear(accepted)
			if n, err := write([]byte("x")); n != 0 || !errors.Is(err, ErrOutboundOverflow) {
				t.Fatalf("full producer=%d,%v", n, err)
			}
			if conn.OutboundBuffered() != 8 || !bytes.Equal(conn.outbound.Peek(make([]byte, 8)), []byte("accepted")) {
				t.Fatal("rejected producer changed earlier accepted bytes")
			}
		})
	}
}

func TestNativeControlRejectsClosedDescriptorOwner(t *testing.T) {
	for _, direct := range []bool{false, true} {
		child, _ := nativeDatagramFixture(t, &Events{})
		owner := &fdConn{fd: child.fd, udp: &unixUDPState{}}
		owner.loop, owner.entry = child.loop, child.entry
		child.udp.server = owner
		if direct {
			owner.ioOwner.Store(currentGoroutineID())
			owner.close.phase.Store(closeResourcesReleased)
		} else {
			owner.close.phase.Store(closeRequested)
		}
		called := false
		if err := child.Control(func(uintptr) { called = true }); !errors.Is(err, net.ErrClosed) || called {
			t.Fatalf("closed-owner Control=%v, invoked=%v (direct=%v)", err, called, direct)
		}
		if child.loop.ioTurnCount() != 0 {
			t.Fatal("closed-owner Control retained its lifetime reservation")
		}
	}
}

func TestNativeHalfOpenEOFCallbackFailureClosesWithConsumerCause(t *testing.T) {
	want := errors.New("consumer rejected end of stream")
	closed := make(chan error, 1)
	events := withFuncs(&Events{Pollers: 1, AllowHalfOpen: true}, FuncHandler{
		Read: func(c Conn) error {
			if _, err := c.Read(nil); errors.Is(err, io.EOF) {
				return want
			}
			return nil
		},
		Close: func(_ Conn, err error) { closed <- err },
	})
	conn := newTestConnection(t, events)
	if err := unix.Shutdown(conn.peer, unix.SHUT_WR); err != nil {
		t.Fatal(err)
	}
	if err := readControlAwait(t, closed, "failed EOF consumer close"); !errors.Is(err, want) {
		t.Fatalf("EOF consumer close=%v", err)
	}
}
