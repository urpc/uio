//go:build (linux || darwin || netbsd || freebsd || openbsd || dragonfly) && !stdio

package uio

import (
	"bytes"
	"errors"
	"net"
	"testing"

	"github.com/urpc/uio/internal/socket"
	"golang.org/x/sys/unix"
)

func nativeDatagramFixture(t *testing.T, events *Events) (*fdConn, [2]int) {
	t.Helper()
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_DGRAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, fd := range fds {
		if err := unix.SetNonblock(fd, true); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		_ = unix.Close(fds[0])
		_ = unix.Close(fds[1])
	})
	conn := &fdConn{fd: int32(fds[0]), udp: &unixUDPState{}}
	conn.loop, conn.entry = &eventLoop{}, events.plainEntry()
	return conn, fds
}

// A datagram vector consists of separate packets. A refusal never creates a
// stream suffix, and a per-packet budget failure must occur before any send.
func TestNativeDatagramVectorAtomicityAndRefusal(t *testing.T) {
	for _, mode := range []string{"complete", "per-packet-limit", "blocked", "peer-closed"} {
		t.Run(mode, func(t *testing.T) {
			written := 0
			events := withFuncs(&Events{}, FuncHandler{BytesWritten: func(_ Conn, n int) { written += n }})
			if mode == "per-packet-limit" {
				events.MaxOutboundBuffered = 3
			}
			conn, fds := nativeDatagramFixture(t, events)
			vec := [][]byte{nil, []byte("ab"), nil, []byte("cd")}
			switch mode {
			case "per-packet-limit":
				vec = [][]byte{[]byte("ab"), []byte("four")}
			case "blocked":
				vec = [][]byte{fillDatagramSendBuffer(t, fds[0])}
			case "peer-closed":
				replacement, err := unix.Open("/dev/null", unix.O_RDONLY, 0)
				if err != nil {
					t.Fatal(err)
				}
				defer unix.Close(replacement)
				if err := unix.Dup2(replacement, fds[1]); err != nil {
					t.Fatal(err)
				}
			}
			n, err := conn.Writev(vec)
			switch mode {
			case "complete":
				if n != 4 || err != nil || written != n {
					t.Fatalf("vector write = %d,%v; reported=%d", n, err, written)
				}
				for _, want := range []string{"ab", "cd"} {
					if got := readPeer(t, fds[1], len(want)); string(got) != want {
						t.Fatalf("datagram=%q, want %q", got, want)
					}
				}
			case "per-packet-limit":
				if n != 0 || !errors.Is(err, ErrOutboundOverflow) || written != 0 {
					t.Fatalf("oversized vector=%d,%v; reported=%d", n, err, written)
				}
				var packet [16]byte
				if _, err := unix.Read(fds[1], packet[:]); !isWouldBlock(err) {
					t.Fatalf("budget rejection emitted a prefix packet: %v", err)
				}
			case "blocked":
				if n != 0 || !isUDPSendBlocked(err) || conn.isClosing() || written != 0 {
					t.Fatalf("blocked vector=%d,%v; closing=%v reported=%d", n, err, conn.isClosing(), written)
				}
			case "peer-closed":
				if n != 0 || err == nil || isUDPSendBlocked(err) || !conn.isClosing() || written != 0 {
					t.Fatalf("failed vector=%d,%v; closing=%v reported=%d", n, err, conn.isClosing(), written)
				}
				if cause := conn.takeDeferredCloseCause(); !errors.Is(cause, err) {
					t.Fatalf("send failure %v lost as close cause %v", err, cause)
				}
			}
			if conn.OutboundBuffered() != 0 {
				t.Fatal("datagram send retained a stream suffix")
			}
		})
	}
}

// Listener retirement is published before every child has been released.
// Both caller and callback paths must reject use of that shared descriptor.
func TestNativeDatagramRetirementStopsEveryDescriptorEntry(t *testing.T) {
	for _, direct := range []bool{false, true} {
		conn, _ := nativeDatagramFixture(t, &Events{})
		server := &fdConn{fd: conn.fd, internal: true, udp: &unixUDPState{peers: make(map[socket.UDPAddress]*fdConn)}}
		server.loop, server.entry = conn.loop, conn.entry
		conn.udp.server = server
		if direct {
			server.ioOwner.Store(currentGoroutineID())
		}
		server.udp.gate.retire()
		for _, write := range []func() (int, error){
			func() (int, error) { return conn.Write([]byte("packet")) },
			func() (int, error) { return conn.Writev([][]byte{[]byte("packet")}) },
			func() (int, error) {
				owned := AcquireBuffer(6)
				_, _ = owned.WriteString("packet")
				return conn.WriteOwned(owned)
			},
		} {
			if n, err := write(); n != 0 || !errors.Is(err, net.ErrClosed) {
				t.Fatalf("retired datagram write=%d,%v (direct=%v)", n, err, direct)
			}
		}
		called := false
		if err := conn.Control(func(uintptr) { called = true }); !errors.Is(err, net.ErrClosed) || called {
			t.Fatalf("retired Control=%v, invoked=%v (direct=%v)", err, called, direct)
		}
		if n := conn.loop.ioTurnCount(); n != 0 {
			t.Fatalf("retired descriptor entry retained %d turns", n)
		}
		if err := conn.ReserveInbound(10); err != nil {
			t.Fatal(err)
		}
		if _, err := conn.ReserveOutbound(10); !errors.Is(err, ErrReserveUnsupported) {
			t.Fatalf("datagram reservation=%v", err)
		}
		if err := conn.YieldRead(); direct {
			if err != nil {
				t.Fatalf("callback datagram yield=%v", err)
			}
			conn.settleInterest()
			if conn.taskState.Load()&ioEventRead == 0 {
				t.Fatal("callback yield lost the next datagram read turn")
			}
		} else if !errors.Is(err, ErrUnsupported) {
			t.Fatalf("external datagram yield=%v", err)
		}
		if err := conn.Flush(); err != nil {
			t.Fatalf("datagram flush=%v", err)
		}
	}
}

func TestNativeDatagramShutdownRejectsVectorsBeforeSending(t *testing.T) {
	for _, reason := range []string{"loop-stopped", "events-closing", "owner-released"} {
		t.Run(reason, func(t *testing.T) {
			events := &Events{}
			conn, fds := nativeDatagramFixture(t, events)
			switch reason {
			case "loop-stopped":
				conn.loop.stopping.Store(true)
			case "events-closing":
				events.closing.Store(true)
			case "owner-released":
				server := &fdConn{fd: conn.fd, udp: &unixUDPState{}}
				server.loop, server.entry = conn.loop, conn.entry
				server.close.phase.Store(closeResourcesReleased)
				conn.udp.server = server
			}
			if n, err := conn.Writev([][]byte{[]byte("packet")}); n != 0 || !errors.Is(err, net.ErrClosed) {
				t.Fatalf("shutdown vector=%d,%v", n, err)
			}
			var packet [16]byte
			if n, err := unix.Read(fds[1], packet[:]); n > 0 || !isWouldBlock(err) {
				t.Fatalf("shutdown emitted a packet: %q,%v", bytes.TrimRight(packet[:], "\x00"), err)
			}
		})
	}
}

func TestNativeDatagramScalarFailureRetainsTransportCause(t *testing.T) {
	for _, owned := range []bool{false, true} {
		conn, fds := nativeDatagramFixture(t, &Events{})
		replacement, err := unix.Open("/dev/null", unix.O_RDONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		if err := unix.Dup2(replacement, fds[1]); err != nil {
			t.Fatal(err)
		}
		_ = unix.Close(replacement)
		var n int
		if owned {
			buffer := AcquireBuffer(6)
			_, _ = buffer.WriteString("packet")
			n, err = conn.WriteOwned(buffer)
		} else {
			n, err = conn.Write([]byte("packet"))
		}
		if n != 0 || err == nil || isUDPSendBlocked(err) || !conn.isClosing() || conn.OutboundBuffered() != 0 {
			t.Fatalf("failed scalar datagram=%d,%v; closing=%v pending=%d", n, err, conn.isClosing(), conn.OutboundBuffered())
		}
		if cause := conn.takeDeferredCloseCause(); !errors.Is(cause, err) {
			t.Fatalf("scalar send error %v lost as close cause %v", err, cause)
		}
	}
}
