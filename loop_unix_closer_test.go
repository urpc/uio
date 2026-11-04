//go:build (linux || darwin || netbsd || freebsd || openbsd || dragonfly) && !stdio

package uio

import (
	"errors"
	"io"
	"net"
	"os"
	"runtime"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// openDescriptors counts the process's open descriptors.
func openDescriptors(t *testing.T) int {
	t.Helper()
	for _, dir := range []string{"/proc/self/fd", "/dev/fd"} {
		if entries, err := os.ReadDir(dir); err == nil {
			return len(entries)
		}
	}
	t.Skip("no descriptor directory")
	return 0
}

// blockingLoops makes the next Events' loops block in the kernel, as a wide
// host's do, which gives each loop a closer.
func blockingLoops(t *testing.T) {
	t.Helper()
	loopWaitersOverride = parkedWaiters + 1
	t.Cleanup(func() { loopWaitersOverride = 0 })
}

// A server-side close on a loop with a closer still reaches the peer
// promptly, and the descriptor is closed for good.
func TestLoopCloserClosesReleasedDescriptors(t *testing.T) {
	blockingLoops(t)
	opened := make(chan Conn, 1)
	events := &Events{Pollers: 1, Handler: FuncHandler{Open: func(c Conn) { opened <- c }}}
	addr, _ := serveLoopback(t, events)
	if events.loops[0].closer == nil {
		t.Fatal("a loop whose waiters block has no closer")
	}
	client, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	var server Conn
	select {
	case server = <-opened:
	case <-time.After(5 * time.Second):
		t.Fatal("no OnOpen")
	}
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
	// The peer sees the end only once the descriptor is closed: uio does not
	// shut the socket down before closing it.
	_ = client.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := client.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Fatalf("peer read after the server closed: %v, want EOF", err)
	}
}

// Connections churned through a loop with a closer leave no descriptor
// behind, neither while the server runs nor once it has closed.
func TestLoopCloserLeaksNothingUnderChurn(t *testing.T) {
	blockingLoops(t)
	var closed atomic.Int32
	events := &Events{
		Pollers: 2,
		Handler: FuncHandler{
			Read: func(c Conn) error {
				_, err := c.Discard(-1)
				if err != nil {
					return err
				}
				return c.Close()
			},
			Close: func(Conn, error) { closed.Add(1) },
		},
	}
	before := openDescriptors(t)
	addr, done := serveLoopback(t, events)
	const n = 200
	for range n {
		c, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = c.Write([]byte("x"))
		_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
		if _, err := c.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
			t.Fatalf("read: %v, want EOF", err)
		}
		_ = c.Close()
	}
	deadline := time.Now().Add(5 * time.Second)
	for closed.Load() < n && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := closed.Load(); got != n {
		t.Fatalf("OnClose ran %d times, want %d", got, n)
	}
	// Serving holds the listener, the pollers and their wake descriptors:
	// a few, not one per connection.
	for open := openDescriptors(t); open > before+32; open = openDescriptors(t) {
		if time.Now().After(deadline) {
			t.Fatalf("%d descriptors open while serving, %d before", open, before)
		}
		time.Sleep(time.Millisecond)
	}
	if err := events.Close(nil); err != nil {
		t.Fatal(err)
	}
	<-done
	if open := openDescriptors(t); open > before+2 {
		t.Fatalf("%d descriptors open after Close, %d before", open, before)
	}
}

// A closer that stopped closes what it held and turns down the rest, which
// the caller then closes itself.
func TestLoopCloserStopDrainsAndRefuses(t *testing.T) {
	loop := &eventLoop{closer: newLoopCloser()}
	var fds [2][2]int
	for i := range fds {
		if err := syscall.Pipe(fds[i][:]); err != nil {
			t.Fatal(err)
		}
		defer syscall.Close(fds[i][1])
	}
	if !loop.deferClose(fds[0][0]) {
		t.Fatal("a running closer refused a descriptor")
	}
	loop.closer.stop()
	var st syscall.Stat_t
	if err := syscall.Fstat(fds[0][0], &st); err == nil {
		t.Fatal("a queued descriptor is still open after stop")
	}
	if loop.deferClose(fds[1][0]) {
		t.Fatal("a stopped closer took a descriptor")
	}
	_ = syscall.Close(fds[1][0])
	loop.closer.stop() // stopping twice is harmless
}

// A close that lingers runs in its connection's own turn: on Linux it blocks
// until the peer takes the unsent data or the linger time runs out, and on
// the loop's closer it would hold up every descriptor queued behind it.
func TestLoopCloserLeavesLingeringClosesToTheirTurn(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("only Linux blocks a non-blocking socket's close for its linger time")
	}
	blockingLoops(t)
	opened := make(chan Conn, 2)
	events := &Events{Pollers: 1, Handler: FuncHandler{Open: func(c Conn) { opened <- c }}}
	addr, _ := serveLoopback(t, events)
	// The first peer never reads, with a small window, so the server's data
	// stays unsent and its lingering close blocks.
	dialer := net.Dialer{Control: func(_, _ string, raw syscall.RawConn) error {
		return raw.Control(func(fd uintptr) {
			_ = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF, 4096)
		})
	}}
	stalled, err := dialer.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer stalled.Close()
	lingering := <-opened
	if err := lingering.SetLinger(3); err != nil {
		t.Fatal(err)
	}
	_, _ = lingering.Write(make([]byte, 4<<20))
	_ = lingering.Close()
	time.Sleep(100 * time.Millisecond) // its close has begun lingering
	quick, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer quick.Close()
	_ = (<-opened).Close()
	start := time.Now()
	_ = quick.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := quick.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Fatalf("peer read after the server closed: %v, want EOF", err)
	}
	if waited := time.Since(start); waited > time.Second {
		t.Fatalf("a plain close waited %v behind a lingering one", waited)
	}
}
