//go:build (linux || darwin || netbsd || freebsd || openbsd || dragonfly) && !stdio

package uio

import (
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urpc/uio/internal/poller"
	"golang.org/x/sys/unix"
)

// backpressureLimit is the MaxOutboundBuffered of the outbound-budget tests.
// The pinned socket buffers hold a small fraction of it, so the backlog the
// tests build stays in user space where the limit counts it.
const backpressureLimit = 1 << 20

// backpressureServer is one connection under MaxOutboundBuffered whose
// OnData counts the input it is given after the test has set up its backlog.
type backpressureServer struct {
	server *fdConn
	client net.Conn
	armed  atomic.Bool  // input before this is the test's own setup
	calls  atomic.Int64 // OnData calls with input once armed
	got    atomic.Int64 // input bytes once armed
	onData func(conn Conn, n int) error
}

func startBackpressureServer(t *testing.T, onData func(conn Conn, n int) error) *backpressureServer {
	t.Helper()
	bp := &backpressureServer{onData: onData}
	opened := make(chan *fdConn, 1)
	started := make(chan string, 1)
	events := &Events{Pollers: 1, MaxOutboundBuffered: backpressureLimit}
	events.OnStart = func(ev *Events) {
		for _, listener := range ev.acceptor.listeners {
			started <- listener.pair.local.String()
			return
		}
	}
	events.OnOpen = func(conn Conn) {
		// Kernels that grow buffers on their own would take the backlog the
		// test means to keep in user space.
		_ = unix.SetsockoptInt(conn.(*fdConn).fd, unix.SOL_SOCKET, unix.SO_SNDBUF, pinnedSocketBuffer)
		opened <- conn.(*fdConn)
	}
	events.OnData = func(conn Conn) error {
		n, _ := conn.Discard(conn.InboundBuffered())
		if n == 0 {
			return nil
		}
		if bp.armed.Load() {
			bp.calls.Add(1)
			bp.got.Add(int64(n))
		}
		if bp.onData != nil {
			return bp.onData(conn, n)
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
	t.Cleanup(func() { _ = client.Close() })
	bp.client = client
	bp.server = <-opened
	return bp
}

// fillTo queues output from outside the connection until the backlog behind
// the full socket settles at no less than low and below the limit.
func (bp *backpressureServer) fillTo(t *testing.T, target, low int64) {
	t.Helper()
	for round := 0; round < 8; round++ {
		if gap := target - bp.server.pending.Load(); gap > 0 {
			if _, err := bp.server.Write(make([]byte, gap)); err != nil {
				t.Fatal(err)
			}
		}
		waitWriteBlockedAndSettled(t, bp.server)
		if bp.server.pending.Load() >= low {
			return
		}
	}
	t.Fatalf("backlog settled at %d, want at least %d", bp.server.pending.Load(), low)
}

// sendInput writes count one-byte messages, spaced so each raises its own
// read edge, and waits for them to have reached the server's socket.
func (bp *backpressureServer) sendInput(t *testing.T, count int) {
	t.Helper()
	for range count {
		if _, err := bp.client.Write([]byte{'x'}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
}

// expectDelivered waits until every armed input byte has reached OnData.
func (bp *backpressureServer) expectDelivered(t *testing.T, want int64, when string) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); bp.got.Load() < want; {
		if time.Now().After(deadline) {
			t.Fatalf("%s: OnData got %d of %d bytes (backlog %d of %d, readStalled=%v)",
				when, bp.got.Load(), want, bp.server.pending.Load(), backpressureLimit, bp.server.readStalled())
		}
		time.Sleep(time.Millisecond)
	}
}

// MaxOutboundBuffered is a write budget only: input keeps being read and
// delivered while the backlog sits past the old pause mark, so a full-duplex
// peer can always drain the backlog that would otherwise wait on its reads.
func TestReadsContinueWhileOutboundBackedUp(t *testing.T) {
	bp := startBackpressureServer(t, nil)
	bp.fillTo(t, backpressureLimit-backpressureLimit/8, backpressureLimit-backpressureLimit/4)
	bp.armed.Store(true)
	bp.sendInput(t, 10)
	bp.expectDelivered(t, 10, "with the backlog past the pause mark")
}

// The same holds with the backlog at the limit itself: reads neither pause
// there, nor does a writable edge need to restart them.
func TestReadsContinueAtOutboundLimit(t *testing.T) {
	bp := startBackpressureServer(t, fillOutboundLimitOnce())
	bp.fillTo(t, backpressureLimit/2, 1)
	bp.sendInput(t, 1)
	// The fill writes until the budget refuses; the socket may take a little
	// more afterwards, so the backlog settles near, not at, the limit.
	if pending := bp.server.pending.Load(); pending < backpressureLimit-backpressureLimit/8 {
		t.Fatalf("backlog = %d, want near the limit %d", pending, backpressureLimit)
	}
	bp.armed.Store(true)
	bp.sendInput(t, 10)
	bp.expectDelivered(t, 10, "with the backlog near the limit")

	// The budget still refuses writes that would exceed it...
	if _, err := bp.server.Write(make([]byte, backpressureLimit)); !errors.Is(err, ErrOutboundOverflow) {
		t.Fatalf("write past the limit: err = %v, want ErrOutboundOverflow", err)
	}
	// ...and accepts them again once the peer drains.
	done := make(chan struct{})
	go func() {
		defer close(done)
		buffer := make([]byte, 64<<10)
		for taken := 0; taken < backpressureLimit/2; {
			_ = bp.client.SetReadDeadline(time.Now().Add(time.Second))
			n, err := bp.client.Read(buffer)
			if err != nil {
				return
			}
			taken += n
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("peer did not drain half the budget")
	}
	for deadline := time.Now().Add(5 * time.Second); bp.server.pending.Load() > backpressureLimit-backpressureLimit/4; {
		if time.Now().After(deadline) {
			t.Fatalf("backlog stayed at %d after the drain", bp.server.pending.Load())
		}
		time.Sleep(time.Millisecond)
	}
	if _, err := bp.server.Write([]byte{'z'}); err != nil {
		t.Fatalf("write after the drain: %v", err)
	}
}

// fillOutboundLimitOnce returns an OnData that, the first time it runs,
// queues output up to exactly the limit behind an already full socket, so
// the round's flush cannot bring the backlog back under it.
func fillOutboundLimitOnce() func(Conn, int) error {
	var filled atomic.Bool
	return func(conn Conn, _ int) error {
		if !filled.CompareAndSwap(false, true) {
			return nil
		}
		if rest := backpressureLimit - conn.(*fdConn).pending.Load() - 4096; rest > 0 {
			if _, err := conn.Write(make([]byte, rest)); err != nil {
				return err
			}
		}
		for {
			if _, err := conn.Write([]byte{0}); err != nil {
				if errors.Is(err, ErrOutboundOverflow) {
					return nil
				}
				return err
			}
		}
	}
}

func TestSkipsEdge(t *testing.T) {
	conn := &fdConn{}
	if conn.skipsEdge(poller.ReadEvents) {
		t.Fatal("a read edge was dropped while no read was owed")
	}
	if !conn.skipsEdge(poller.WriteEvents) {
		t.Fatal("a write-only edge with nothing to send scheduled a turn")
	}
	conn.pending.Store(1)
	if conn.skipsEdge(poller.WriteEvents) {
		t.Fatal("a write-only edge was dropped while output was queued")
	}
	conn.pending.Store(0)
	conn.setWriteBlocked(true)
	if conn.skipsEdge(poller.WriteEvents) {
		t.Fatal("a write-only edge was dropped from a write-blocked socket")
	}
	conn.setReadStalled(true)
	if !conn.skipsEdge(poller.ReadEvents) {
		t.Fatal("a read edge reached a read that was already owed")
	}
	if conn.skipsEdge(poller.ReadEvents | poller.HangupEvents) {
		t.Fatal("a hangup was dropped while a read was owed")
	}
	if conn.skipsEdge(poller.ReadEvents | poller.WriteEvents) {
		t.Fatal("a writable edge was dropped while a read was owed")
	}
}

// countingExecutor runs every connection turn on its own goroutine and counts
// them.
type countingExecutor struct{ turns atomic.Int64 }

func (executor *countingExecutor) Submit(task IOTask) bool {
	executor.turns.Add(1)
	go task.RunTask()
	return true
}

func (executor *countingExecutor) SubmitBatch(tasks []IOTask) int {
	executor.turns.Add(int64(len(tasks)))
	for _, task := range tasks {
		go task.RunTask()
	}
	return len(tasks)
}

// A request answered from its own turn costs that one turn. Write interest
// stays armed for a stream's life, and kqueue reports a write-only edge each
// time an acknowledgement frees send space; with nothing queued it must not
// schedule a turn.
func TestRequestReplyRunsOneTurnPerRequest(t *testing.T) {
	const rounds = 200
	executor := &countingExecutor{}
	events := &Events{Pollers: 1, Executor: executor}
	events.OnData = func(conn Conn) error {
		for conn.InboundBuffered() > 0 {
			chunk := conn.PeekChunk()
			if _, err := conn.Write(chunk); err != nil {
				return err
			}
			_, _ = conn.Discard(len(chunk))
		}
		return nil
	}
	testConn := newTCPTestConnection(t, events)
	message := make([]byte, 64)
	reply := make([]byte, 64)
	time.Sleep(20 * time.Millisecond)
	before := executor.turns.Load()
	for range rounds {
		if _, err := unix.Write(testConn.peer, message); err != nil {
			t.Fatal(err)
		}
		for got := 0; got < len(reply); {
			n, err := unix.Read(testConn.peer, reply[got:])
			if err != nil {
				if errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EINTR) {
					time.Sleep(10 * time.Microsecond)
					continue
				}
				t.Fatal(err)
			}
			got += n
		}
		// Give the acknowledgement of the reply time to raise its edge.
		time.Sleep(200 * time.Microsecond)
	}
	time.Sleep(20 * time.Millisecond)
	if turns := executor.turns.Load() - before; turns > rounds+rounds/10 {
		t.Fatalf("%d request/reply rounds ran %d connection turns", rounds, turns)
	}
}
