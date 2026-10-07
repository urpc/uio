//go:build (linux || darwin || netbsd || freebsd || openbsd || dragonfly) && !stdio

package uio

import (
	"net"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// A read paused by its round's budget or by YieldRead is redelivered while the
// socket refuses the connection's output, with or without an outbound limit
// below its throttle mark. The peer here never reads, as a uio peer whose own
// read is paused the same way would not: a read that waited for writability
// would leave the rest of the peer's bytes in the socket with no edge to come.
func TestPausedReadRedeliveredWhileWriteBlocked(t *testing.T) {
	const (
		input  = 512     // with one-byte reads, two rounds of the budget
		output = 4 << 20 // far more than the pinned socket buffers hold
	)
	for _, test := range []struct {
		name  string
		yield bool
		limit int
	}{
		{name: "budget"},
		{name: "budget under limit", limit: 128 << 20},
		{name: "YieldRead", yield: true},
		{name: "YieldRead under limit", yield: true, limit: 128 << 20},
	} {
		t.Run(test.name, func(t *testing.T) {
			opened := make(chan *fdConn, 1)
			started := make(chan string, 1)
			var received atomic.Int64
			events := &Events{Pollers: 1, MaxBufferSize: 1, MaxOutboundBuffered: test.limit}
			events.OnStart = func(ev *Events) {
				for _, listener := range ev.acceptor.listeners {
					started <- listener.laddr.String()
					return
				}
			}
			events.OnOpen = func(conn Conn) { opened <- conn.(*fdConn) }
			events.OnData = func(conn Conn) error {
				n, _ := conn.Discard(conn.InboundBuffered())
				if n == 0 {
					return nil
				}
				if received.Add(int64(n)) == int64(n) {
					// The first byte's reply fills the socket: the peer reads nothing.
					if _, err := conn.Write(make([]byte, output)); err != nil {
						return err
					}
				}
				if test.yield {
					return conn.YieldRead()
				}
				return nil
			}
			serveDone := make(chan error, 1)
			go func() { serveDone <- events.Serve("tcp://127.0.0.1:0") }()
			t.Cleanup(func() {
				_ = events.Close(nil)
				<-serveDone
			})
			client, err := dialWithReceiveBuffer(<-started, pinnedSocketBuffer)
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			server := <-opened
			// Kernels that grow buffers on their own would make room for more
			// output later and raise a writable edge the test must not get.
			if err = unix.SetsockoptInt(server.fd, unix.SOL_SOCKET, unix.SO_SNDBUF, pinnedSocketBuffer); err != nil {
				t.Fatal(err)
			}
			if _, err = client.Write([]byte{0}); err != nil {
				t.Fatal(err)
			}
			waitWriteBlockedAndSettled(t, server)
			// Every read of this input happens while the socket refuses output,
			// and no writable edge can come to restart it.
			if _, err = client.Write(make([]byte, input)); err != nil {
				t.Fatal(err)
			}
			for deadline := time.Now().Add(5 * time.Second); received.Load() < 1+input; time.Sleep(time.Millisecond) {
				if time.Now().After(deadline) {
					t.Fatalf("server read %d of %d bytes: readStalled=%v writeBlocked=%v pending=%d",
						received.Load(), 1+input, server.readStalled(), server.writeBlocked(), server.pending.Load())
				}
			}
			if !server.writeBlocked() {
				t.Fatal("the socket took more output while the input was read")
			}
		})
	}
}

// pinnedSocketBuffer is small enough that output fills both ends in a moment.
const pinnedSocketBuffer = 16 << 10

// dialWithReceiveBuffer connects with SO_RCVBUF set before the handshake,
// which fixes the receive window the peer may fill.
func dialWithReceiveBuffer(addr string, size int) (net.Conn, error) {
	dialer := net.Dialer{Control: func(_, _ string, raw syscall.RawConn) error {
		var err error
		if controlErr := raw.Control(func(fd uintptr) {
			err = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_RCVBUF, size)
		}); controlErr != nil {
			return controlErr
		}
		return err
	}}
	return dialer.Dial("tcp", addr)
}

// waitWriteBlockedAndSettled waits until conn's socket refuses output and the
// kernel has stopped taking more of it: the peer's receive window is full, so
// no writable edge will come while the peer does not read.
func waitWriteBlockedAndSettled(t *testing.T, conn *fdConn) {
	t.Helper()
	const settled = 20 // consecutive samples, 10ms apart
	last, same := int64(-1), 0
	for deadline := time.Now().Add(5 * time.Second); same < settled; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("output did not settle behind a blocked write: writeBlocked=%v pending=%d",
				conn.writeBlocked(), conn.pending.Load())
		}
		pending := conn.pending.Load()
		if conn.writeBlocked() && pending == last {
			same++
		} else {
			same = 0
		}
		last = pending
	}
}
