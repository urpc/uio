//go:build (linux || darwin || netbsd || freebsd || openbsd || dragonfly) && !stdio

package uio

import (
	"net"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// runBidirectionalLimitTransfer pins that outbound backlog never stops reads:
// both peers fill their own backlog past the limit's old pause mark while the
// sockets hold small buffers, exactly the shape that used to leave each side
// waiting for the other to read. Every byte must reach the far OnData.
func runBidirectionalLimitTransfer(t *testing.T, raw [2]net.Conn, limit int) {
	t.Helper()
	total := int64(limit) * 7 / 8 // past the old pause mark, below the limit
	releaseOpen := make(chan struct{})
	var received [2]atomic.Int64
	var conns [2]Conn
	var events [2]*Events
	var done [2]chan error
	for i := range 2 {
		started := make(chan struct{})
		opened := make(chan Conn, 1)
		ev := &Events{Pollers: 1, MaxOutboundBuffered: limit, MaxBufferSize: 16 << 10}
		ev.OnStart = func(*Events) { close(started) }
		ev.OnOpen = func(conn Conn) {
			opened <- conn
			// Hold the open until both sides have their output queued, so
			// neither reads while the other is still filling.
			<-releaseOpen
		}
		ev.OnData = func(conn Conn) error {
			n, _ := conn.Discard(conn.InboundBuffered())
			received[i].Add(int64(n))
			return nil
		}
		events[i] = ev
		done[i] = make(chan error, 1)
		go func() { done[i] <- ev.Serve() }()
		<-started
		conn, err := ev.Adopt(raw[i], nil)
		if err != nil {
			t.Fatal(err)
		}
		conns[i] = conn
		<-opened
	}
	t.Cleanup(func() {
		for _, ev := range events {
			_ = ev.Close(nil)
		}
		for _, ch := range done {
			select {
			case <-ch:
			case <-time.After(3 * time.Second):
				t.Error("Serve did not stop")
			}
		}
	})

	payload := make([]byte, total)
	for _, conn := range conns {
		if _, err := conn.Write(payload); err != nil {
			t.Fatal(err)
		}
	}
	close(releaseOpen)

	// The stall detector reports a mutual wait within a second; the deadline
	// only bounds a slow-but-moving transfer, which a pinned-window pair under
	// -race can be.
	last, lastMoved := int64(-1), time.Now()
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); {
		a, b := received[0].Load(), received[1].Load()
		if a == total && b == total {
			return
		}
		if a+b != last {
			last, lastMoved = a+b, time.Now()
		}
		if time.Since(lastMoved) > time.Second {
			first, second := conns[0].(*fdConn), conns[1].(*fdConn)
			t.Fatalf("both directions stopped: received %d/%d and %d/%d; pending %d,%d; readStalled %v,%v; writeBlocked %v,%v",
				a, total, b, total, first.pending.Load(), second.pending.Load(),
				first.readStalled(), second.readStalled(), first.writeBlocked(), second.writeBlocked())
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("bidirectional transfer did not complete")
}

func TestBidirectionalOutboundLimitUnix(t *testing.T) {
	sockets, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	var raw [2]net.Conn
	for i, fd := range sockets {
		_ = unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_RCVBUF, pinnedSocketBuffer)
		_ = unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_SNDBUF, pinnedSocketBuffer)
		file := os.NewFile(uintptr(fd), "bidirectional-peer")
		raw[i], err = net.FileConn(file)
		_ = file.Close()
		if err != nil {
			t.Fatal(err)
		}
		defer raw[i].Close()
	}
	runBidirectionalLimitTransfer(t, raw, 1<<20)
}

func TestBidirectionalOutboundLimitTCP(t *testing.T) {
	first, second := tcpConnectionPair(t)
	defer first.Close()
	defer second.Close()
	for _, conn := range []net.Conn{first, second} {
		_ = conn.(*net.TCPConn).SetReadBuffer(pinnedSocketBuffer)
		_ = conn.(*net.TCPConn).SetWriteBuffer(pinnedSocketBuffer)
	}
	// A smaller budget than the Unix case keeps the pinned-window transfer
	// bounded under -race on a loaded host; the mechanism is the same: the
	// backlog sits past the old pause mark for the whole transfer.
	runBidirectionalLimitTransfer(t, [2]net.Conn{first, second}, 2<<20)
}
