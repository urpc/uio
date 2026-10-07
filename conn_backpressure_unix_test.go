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

// backpressureLimit is the MaxOutboundBuffered of the read-pause tests. The
// pinned socket buffers hold a small fraction of it, so the backlog the tests
// build stays in user space where the limit counts it.
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
			started <- listener.laddr.String()
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

func (bp *backpressureServer) expectPaused(t *testing.T, when string) {
	t.Helper()
	if calls := bp.calls.Load(); calls != 0 {
		t.Fatalf("%s: OnData ran %d times with backlog %d of %d", when, calls, bp.server.pending.Load(), backpressureLimit)
	}
}

// drainAndExpectInput reads everything the server sends until the input the
// test queued has reached OnData: the backlog falls to the resume mark, and
// the paused read must be delivered then.
func (bp *backpressureServer) drainAndExpectInput(t *testing.T, want int64) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		buffer := make([]byte, 64<<10)
		for bp.got.Load() < want {
			_ = bp.client.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
			if _, err := bp.client.Read(buffer); err != nil {
				var netErr net.Error
				if !errors.As(err, &netErr) || !netErr.Timeout() {
					return
				}
			}
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("paused read was never resumed: got %d of %d bytes, backlog %d, readStalled=%v throttled=%v",
			bp.got.Load(), want, bp.server.pending.Load(), bp.server.readStalled(), bp.server.throttled())
	}
	if got := bp.got.Load(); got != want {
		t.Fatalf("got %d input bytes after the drain, want %d", got, want)
	}
}

// MaxOutboundBuffered pauses a connection's reads once its backlog reaches
// 75% of the limit, not only once a read round fills it, and the read resumes
// when the backlog drains to 50%.
func TestReadPausesAtOutboundThrottleMark(t *testing.T) {
	bp := startBackpressureServer(t, nil)
	bp.fillTo(t, backpressureLimit-backpressureLimit/8, backpressureLimit-backpressureLimit/4)
	bp.armed.Store(true)
	bp.sendInput(t, 10)
	bp.expectPaused(t, "between the throttle mark and the limit")
	bp.drainAndExpectInput(t, 10)
}

// A read round that fills the limit pauses the read, and further input must
// not restart it. kqueue raises a read edge for every arrival, which a turn
// must not take as leave to read.
func TestReadPausedAtOutboundLimitIgnoresNewInput(t *testing.T) {
	bp := startBackpressureServer(t, fillOutboundLimitOnce())
	bp.fillTo(t, backpressureLimit/2, 1)
	bp.sendInput(t, 1)
	waitReadStalled(t, bp.server)
	bp.armed.Store(true)
	bp.sendInput(t, 10)
	bp.expectPaused(t, "with the backlog at the limit")
	bp.drainAndExpectInput(t, 10)
}

// A writable edge on a paused read must not restart it. Under always-armed
// write interest epoll reports that edge with read readiness too whenever
// input is queued.
func TestReadPausedAtOutboundLimitIgnoresWritableEdge(t *testing.T) {
	bp := startBackpressureServer(t, fillOutboundLimitOnce())
	bp.fillTo(t, backpressureLimit/2, 1)
	bp.sendInput(t, 1)
	waitReadStalled(t, bp.server)
	bp.armed.Store(true)
	bp.sendInput(t, 1)
	// The peer takes a little output: the socket turns writable while the
	// backlog stays far above the resume mark.
	buffer := make([]byte, 64<<10)
	for taken := 0; taken < len(buffer); {
		_ = bp.client.SetReadDeadline(time.Now().Add(time.Second))
		n, err := bp.client.Read(buffer[taken:])
		if err != nil {
			t.Fatal(err)
		}
		taken += n
	}
	time.Sleep(200 * time.Millisecond)
	if bp.server.pending.Load() <= backpressureLimit/2 {
		t.Skipf("backlog drained to %d; the edge resumed reads legitimately", bp.server.pending.Load())
	}
	bp.expectPaused(t, "after a writable edge")
	bp.drainAndExpectInput(t, 1)
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

func waitReadStalled(t *testing.T, conn *fdConn) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !conn.readStalled(); time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("read never paused: backlog %d of %d", conn.pending.Load(), backpressureLimit)
		}
	}
}

func TestSkipsEdge(t *testing.T) {
	conn := &fdConn{}
	if conn.skipsEdge(poller.ReadEvents) {
		t.Fatal("a read edge was dropped while the read was not paused")
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
		t.Fatal("a read edge reached a paused read")
	}
	if conn.skipsEdge(poller.ReadEvents | poller.HangupEvents) {
		t.Fatal("a hangup was dropped while the read was paused")
	}
	if conn.skipsEdge(poller.ReadEvents | poller.WriteEvents) {
		t.Fatal("a writable edge was dropped while the read was paused")
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
