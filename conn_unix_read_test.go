//go:build (linux || darwin || netbsd || freebsd || openbsd || dragonfly) && !stdio

package uio

import (
	"bytes"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// A peer's FIN or reset that is already queued behind its last bytes raises
// no edge of its own: the edge that announced the bytes announced it too. A
// read round that stops at a short read would leave it unread, and the
// connection would sit half-closed with no callback to come. The peer here
// has written and closed before uio sees the socket, so every round over
// these bytes runs with the hangup already pending.
func TestReadReachesHangupQueuedBehindData(t *testing.T) {
	for _, test := range []struct {
		name    string
		buffer  int  // Events.MaxBufferSize, one socket read
		limit   int  // Events.MaxOutboundBuffered
		payload int  // bytes queued ahead of the hangup
		yield   bool // first callback calls YieldRead
		reset   bool // peer resets instead of closing
	}{
		// One read comes up short and the hangup is behind it.
		{name: "short read", payload: 100},
		// The round that reads the short tail is a redelivered one, which
		// carries no readiness of its own: the hangup reported to the first
		// round has to be remembered. Without an outbound limit the turn
		// queues the next round itself; with one, the loop's refresh does.
		{name: "after a yielded round", buffer: 1024, payload: 2500, yield: true},
		{name: "after the round budget", buffer: 2, payload: 2*256 + 1},
		{name: "after a yielded round under a limit", buffer: 1024, limit: 1 << 20, payload: 2500, yield: true},
		{name: "after the round budget under a limit", buffer: 2, limit: 1 << 20, payload: 2*256 + 1},
		{name: "reset", payload: 100, reset: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			events := &Events{Pollers: 1, MaxBufferSize: test.buffer, MaxOutboundBuffered: test.limit}
			started := make(chan struct{})
			closed := make(chan error, 1)
			var mu sync.Mutex
			var received bytes.Buffer
			var yielded atomic.Bool
			funcs(events).Start = func(*Events) { close(started) }
			funcs(events).Read = func(conn Conn) error {
				mu.Lock()
				_, err := conn.WriteTo(&received)
				mu.Unlock()
				if err != nil {
					return err
				}
				if test.yield && yielded.CompareAndSwap(false, true) {
					return conn.YieldRead()
				}
				return nil
			}
			funcs(events).Close = func(_ Conn, err error) { closed <- err }
			serveDone := make(chan error, 1)
			go func() { serveDone <- events.Serve() }()
			t.Cleanup(func() {
				_ = events.Close(nil)
				<-serveDone
			})
			<-started

			server, client := tcpPair(t)
			payload := bytes.Repeat([]byte("0123456789"), test.payload/10+1)[:test.payload]
			if _, err := client.Write(payload); err != nil {
				t.Fatal(err)
			}
			if test.reset {
				if err := client.(*net.TCPConn).SetLinger(0); err != nil {
					t.Fatal(err)
				}
			}
			if err := client.Close(); err != nil {
				t.Fatal(err)
			}
			waitReadHangupQueued(t, server)
			if _, err := events.Adopt(server); err != nil {
				t.Fatal(err)
			}

			select {
			case <-closed:
			case <-time.After(3 * time.Second):
				mu.Lock()
				defer mu.Unlock()
				t.Fatalf("peer hung up but the connection stayed open after reading %d of %d bytes",
					received.Len(), len(payload))
			}
			if test.reset {
				return // a reset may discard bytes the peer sent
			}
			mu.Lock()
			defer mu.Unlock()
			if !bytes.Equal(received.Bytes(), payload) {
				t.Fatalf("received %d bytes, want the %d the peer sent before closing", received.Len(), len(payload))
			}
		})
	}
}

// uio ends a connection when it reads the end of stream, after one attempt to
// send what its callbacks queued. A peer that sends a request and shuts down
// its side, as one-shot clients do, gets the reply the request produced and
// then the end of stream, also when its FIN is queued behind the request.
func TestHalfClosedPeerGetsReplyThenEndOfStream(t *testing.T) {
	request := []byte("ping")
	reply := bytes.Repeat([]byte("pong"), 1024) // well within any socket buffer
	events := &Events{Pollers: 1}
	started := make(chan struct{})
	funcs(events).Start = func(*Events) { close(started) }
	funcs(events).Read = func(conn Conn) error {
		if conn.InboundBuffered() < len(request) {
			return nil
		}
		if _, err := conn.Discard(len(request)); err != nil {
			return err
		}
		_, err := conn.Write(reply)
		return err
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- events.Serve() }()
	t.Cleanup(func() {
		_ = events.Close(nil)
		<-serveDone
	})
	<-started

	server, client := tcpPair(t)
	defer client.Close()
	if _, err := client.Write(request); err != nil {
		t.Fatal(err)
	}
	if err := client.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	waitReadHangupQueued(t, server)
	if _, err := events.Adopt(server); err != nil {
		t.Fatal(err)
	}
	if err := client.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(client)
	if err != nil {
		t.Fatalf("read %d reply bytes, then %v; want the reply and the end of stream", len(got), err)
	}
	if !bytes.Equal(got, reply) {
		t.Fatalf("read %d bytes, want the %d-byte reply", len(got), len(reply))
	}
}

// Peers that write a request and close at once are common, and their FIN can
// reach the kernel before the accepted connection's first read event is taken.
// Accepted connections run keepalive, so a missed FIN costs a probe interval
// rather than the connection; the test allows far less than that.
func TestAcceptedPeersThatWriteAndCloseAreClosed(t *testing.T) {
	const peers = 300
	events := &Events{Pollers: 1}
	started := make(chan string, 1)
	var closed atomic.Int64
	funcs(events).Start = func(ev *Events) {
		for _, listener := range ev.acceptor.listeners {
			started <- listener.pair.local.String()
			return
		}
	}
	funcs(events).Read = func(conn Conn) error {
		_, err := conn.Discard(conn.InboundBuffered())
		return err
	}
	funcs(events).Close = func(Conn, error) { closed.Add(1) }
	serveDone := make(chan error, 1)
	go func() { serveDone <- events.Serve("tcp://127.0.0.1:0") }()
	t.Cleanup(func() {
		_ = events.Close(nil)
		<-serveDone
	})
	addr := <-started

	request := bytes.Repeat([]byte("x"), 64)
	for range peers {
		client, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = client.Write(request); err != nil {
			t.Fatal(err)
		}
		if err = client.Close(); err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for closed.Load() < peers && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := closed.Load(); got < peers {
		t.Fatalf("%d of %d connections whose peer closed are still open", peers-got, peers)
	}
}

// A server that answers and closes at once can do both before the dialing
// side registers its socket. Dialed connections run no keepalive, so a missed
// FIN would hold such a connection until its next write fails.
func TestDialedConnectionsToAServerThatAnswersAndClosesAreClosed(t *testing.T) {
	const dials = 200
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		answer := bytes.Repeat([]byte("y"), 64)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			_, _ = conn.Write(answer)
			_ = conn.Close()
		}
	}()

	events := &Events{Pollers: 1}
	started := make(chan struct{})
	var closed atomic.Int64
	funcs(events).Start = func(*Events) { close(started) }
	funcs(events).Read = func(conn Conn) error {
		_, err := conn.Discard(conn.InboundBuffered())
		return err
	}
	funcs(events).Close = func(Conn, error) { closed.Add(1) }
	serveDone := make(chan error, 1)
	go func() { serveDone <- events.Serve() }()
	t.Cleanup(func() {
		_ = events.Close(nil)
		<-serveDone
	})
	<-started

	for range dials {
		if _, err = events.Dial("tcp://" + listener.Addr().String()); err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for closed.Load() < dials && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := closed.Load(); got < dials {
		t.Fatalf("%d of %d dialed connections whose server closed are still open", dials-got, dials)
	}
}

// tcpPair returns both ends of a loopback TCP connection.
func tcpPair(t *testing.T) (server, client net.Conn) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, _ := listener.Accept()
		accepted <- conn
	}()
	client, err = net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	server = <-accepted
	if server == nil {
		_ = client.Close()
		t.Fatal("accept failed")
	}
	return server, client
}

// waitReadHangupQueued waits until conn's socket reports the peer's hangup,
// which arrives after the bytes sent ahead of it.
func waitReadHangupQueued(t *testing.T, conn net.Conn) {
	t.Helper()
	raw, err := conn.(syscall.Conn).SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		var queued bool
		var checkErr error
		if err = raw.Control(func(fd uintptr) { queued, checkErr = readHangupQueued(int(fd)) }); err != nil {
			t.Fatal(err)
		}
		if checkErr != nil {
			t.Fatal(checkErr)
		}
		if queued {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the peer's hangup never reached the socket")
		}
		time.Sleep(time.Millisecond)
	}
}

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
			funcs(events).Start = func(ev *Events) {
				for _, listener := range ev.acceptor.listeners {
					started <- listener.pair.local.String()
					return
				}
			}
			funcs(events).Open = func(conn Conn) { opened <- conn.(*fdConn) }
			funcs(events).Read = func(conn Conn) error {
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
			if err = unix.SetsockoptInt(int(server.fd), unix.SOL_SOCKET, unix.SO_SNDBUF, pinnedSocketBuffer); err != nil {
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
