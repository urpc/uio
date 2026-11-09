//go:build (linux || darwin || netbsd || freebsd || openbsd || dragonfly) && !stdio

package uio

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"github.com/urpc/uio/internal/bytebuf"
	"github.com/urpc/uio/internal/fdmap"
	"github.com/urpc/uio/internal/poller"
	"github.com/urpc/uio/internal/socket"
	"golang.org/x/sys/unix"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

func TestWaitersHonorLockOSThread(t *testing.T) {
	cases := []struct {
		name string
		lock bool
	}{{name: "locked", lock: true}, {name: "unlocked", lock: false}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			loops, perLoop := int32(min(2, runtime.GOMAXPROCS(0))), int32(2)
			loopWaitersOverride = int(perLoop)
			t.Cleanup(func() {
				loopWaitersOverride = 0
			})
			var started atomic.Int32
			var tookLock, skippedLock atomic.Bool
			testHookWaiterStarted = func(locked bool) {
				started.Add(1)
				if locked {
					tookLock.Store(true)
				} else {
					skippedLock.Store(true)
				}
			}
			t.Cleanup(func() {
				testHookWaiterStarted = nil
			})
			events := &Events{Pollers: int(loops), LockOSThread: tc.lock}
			done := make(chan error, 1)
			go func() {
				done <- events.Serve("tcp://127.0.0.1:0")
			}()
			t.Cleanup(func() {
				_ = events.Close(nil)
				select {
				case err := <-done:
					if err != nil {
						t.Error(err)
					}
				case <-time.After(3 * time.Second):
					t.Error("Serve did not stop")
				}
			})
			deadline := time.Now().Add(3 * time.Second)
			for started.Load() != loops*perLoop && time.Now().Before(deadline) {
				time.Sleep(2 * time.Millisecond)
			}
			if got := started.Load(); got != loops*perLoop {
				t.Fatalf("waiters started = %d, want %d", got, loops*perLoop)
			}
			if tookLock.Load() != tc.lock || skippedLock.Load() == tc.lock {
				t.Errorf("waiters took the thread association = %v/%v, want all %v", tookLock.Load(), skippedLock.Load(), tc.lock)
			}
		})
	}
}

const stdioBackend = false

func TestAddConnContextRejectsAlreadyCanceledContext(t *testing.T) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = unix.Close(fds[1])
	})
	cause := errors.New("registration canceled")
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(cause)
	conn := &fdConn{fd: int32(fds[0]), commonConn: commonConn{loop: &eventLoop{}}}
	conn.entry = (&Events{}).plainEntry()
	if err = (&Events{}).addConnContext(ctx, conn); !errors.Is(err, cause) {
		t.Fatalf("addConnContext() error = %v, want %v", err, cause)
	}
	if !conn.isClosing() || !conn.isReleased() {
		t.Fatalf("canceled connection state = closing:%v phase:%v", conn.isClosing(), conn.close.phase.Load())
	}
}

const backpressureLimit = 1 << 20

type backpressureServer struct {
	server *fdConn
	client net.Conn
	armed  atomic.Bool
	calls  atomic.Int64
	got    atomic.Int64
	onData func(conn Conn, n int) error
} // input before this is the test's own setup
// input bytes once armed

func startBackpressureServer(t *testing.T, onData func(conn Conn, n int) error) *backpressureServer {
	t.Helper()
	bp := &backpressureServer{onData: onData}
	opened := make(chan *fdConn, 1)
	started := make(chan string, 1)
	events := &Events{Pollers: 1, MaxOutboundBuffered: backpressureLimit}
	funcs(events).Start = func(ev *Events) {
		for _, listener := range ev.acceptor.listeners {
			started <- listener.pair.local.String()
			return
		}
	}
	funcs(events).Open = func(conn Conn) {
		_ = unix.SetsockoptInt(int(conn.(*fdConn).fd), unix.SOL_SOCKET, unix.SO_SNDBUF, pinnedSocketBuffer)
		opened <- conn.(*fdConn)
	}
	funcs(events).Read = func(conn Conn) error {
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
	go func() {
		serveDone <- events.Serve("tcp://127.0.0.1:0")
	}()
	t.Cleanup(func() {
		_ = events.Close(nil)
		<-serveDone
	})
	client, err := dialWithReceiveBuffer(<-started, pinnedSocketBuffer)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = client.Close()
	})
	bp.client = client
	bp.server = <-opened
	return bp
}

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

func (bp *backpressureServer) expectDelivered(t *testing.T, want int64, when string) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); bp.got.Load() < want; {
		if time.Now().After(deadline) {
			t.Fatalf("%s: OnData got %d of %d bytes (backlog %d of %d)", when, bp.got.Load(), want, bp.server.pending.Load(), backpressureLimit)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestReadsContinueWhileOutboundBackedUp(t *testing.T) {
	bp := startBackpressureServer(t, nil)
	bp.fillTo(t, backpressureLimit-backpressureLimit/8, backpressureLimit-backpressureLimit/4)
	bp.armed.Store(true)
	bp.sendInput(t, 10)
	bp.expectDelivered(t, 10, "with the backlog past the pause mark")
}

func TestReadsContinueAtOutboundLimit(t *testing.T) {
	bp := startBackpressureServer(t, fillOutboundLimitOnce())
	bp.fillTo(t, backpressureLimit/2, 1)
	bp.sendInput(t, 1)
	if pending := bp.server.pending.Load(); pending < backpressureLimit-backpressureLimit/8 {
		t.Fatalf("backlog = %d, want near the limit %d", pending, backpressureLimit)
	}
	bp.armed.Store(true)
	bp.sendInput(t, 10)
	bp.expectDelivered(t, 10, "with the backlog near the limit")
	if _, err := bp.server.Write(make([]byte, backpressureLimit)); !errors.Is(err, ErrOutboundOverflow) {
		t.Fatalf("write past the limit: err = %v, want ErrOutboundOverflow", err)
	}
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

type shortWriter struct{ PassThrough }

func (shortWriter) OnOpen(ctx LayerContext) {
	ctx.Open()
}

func (shortWriter) OnWrite(_ LayerContext, p []byte) (int, error) {
	return 1, nil
}

func TestShortOnWriteIsErrShortWrite(t *testing.T) {
	addr := overrideListener(t, 1)
	wrote := make(chan error, 1)
	events := dialLayered(t, addr, []Layer{LayerFunc(func(LayerContext) LayerHandler {
		return shortWriter{}
	})}, func(ev *Events) {
		ev.Handler = FuncHandler{Open: func(c Conn) {
			_, err := c.Write([]byte("hello"))
			wrote <- err
		}, Close: func(Conn, error) {
		}}
	})
	ctx, cancel := context.WithTimeout(context.Background(), layerWait)
	defer cancel()
	conn, err := events.DialContext(ctx, "tcp://"+addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := layerWaitFor(t, wrote, "the write"); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("Write = %v, want io.ErrShortWrite", err)
	}
}

func TestNativeConnectionUnavailableAndClosedAPIBoundaries(t *testing.T) {
	events := &Events{}
	conn := &fdConn{fd: -1}
	conn.entry = events.plainEntry()
	for _, operation := range []struct {
		name string
		call func() error
	}{{"Flush", conn.Flush}, {"CloseWrite", conn.CloseWrite}, {"Close", conn.Close}, {"Abort", func() error {
		return conn.Abort(nil)
	}}, {"Control", func() error {
		return conn.Control(func(uintptr) {
			t.Error("unavailable descriptor reached callback")
		})
	}}} {
		if err := operation.call(); !errors.Is(err, net.ErrClosed) {
			t.Errorf("unavailable %s = %v", operation.name, err)
		}
	}
	if conn.RemoteAddr() != nil {
		t.Fatal("unavailable connection invented a remote address")
	}
	if n, err := conn.WriteOwned(nil); n != 0 || err != nil {
		t.Fatalf("nil owned write=%d,%v", n, err)
	}
	if n, err := conn.WriteOwned(AcquireBuffer(0)); n != 0 || err != nil {
		t.Fatalf("empty owned write=%d,%v", n, err)
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
	owned := AcquireBuffer(7)
	_, _ = owned.WriteString("payload")
	if n, err := conn.WriteOwned(owned); n != 0 || !errors.Is(err, net.ErrClosed) {
		t.Fatalf("closed owned write=%d,%v", n, err)
	}
}

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
		if err := child.Control(func(uintptr) {
			called = true
		}); !errors.Is(err, net.ErrClosed) || called {
			t.Fatalf("closed-owner Control=%v, invoked=%v (direct=%v)", err, called, direct)
		}
		if child.loop.ioTurnCount() != 0 {
			t.Fatal("closed-owner Control retained its lifetime reservation")
		}
	}
}

func TestAbortCutsADrainShort(t *testing.T) {
	saved := lingerTimeout
	lingerTimeout = 10 * time.Second
	t.Cleanup(func() {
		lingerTimeout = saved
	})
	closed := make(chan error, 1)
	events := &Events{Pollers: 1}
	funcs(events).Read = func(Conn) error {
		return nil
	}
	funcs(events).Close = func(_ Conn, err error) {
		closed <- err
	}
	testConn, registered := startTestConnection(t, events)
	if err := <-registered; err != nil {
		t.Fatal(err)
	}
	if err := unix.SetsockoptInt(testConn.peer, unix.SOL_SOCKET, unix.SO_RCVBUF, 4<<10); err != nil {
		t.Fatal(err)
	}
	payload := make([]byte, 64<<10)
	for i := 0; i < 4; i++ {
		if _, err := testConn.conn.Write(payload); err != nil {
			t.Fatal(err)
		}
	}
	if err := testConn.conn.Close(); err != nil {
		t.Fatal(err)
	}
	if err := testConn.conn.Abort(io.EOF); err != nil {
		t.Fatal(err)
	}
	select {
	case cause := <-closed:
		if !errors.Is(cause, io.EOF) {
			t.Fatalf("OnClose reported %v; want io.EOF", cause)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the abort did not end the drain")
	}
}

func TestCloseWithEmptyQueueClosesAtOnce(t *testing.T) {
	saved := lingerTimeout
	lingerTimeout = 10 * time.Second
	t.Cleanup(func() {
		lingerTimeout = saved
	})
	closed := make(chan error, 1)
	events := &Events{Pollers: 1}
	funcs(events).Read = func(Conn) error {
		return nil
	}
	funcs(events).Close = func(_ Conn, err error) {
		closed <- err
	}
	testConn, registered := startTestConnection(t, events)
	if err := <-registered; err != nil {
		t.Fatal(err)
	}
	if err := testConn.conn.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case cause := <-closed:
		if cause != nil {
			t.Fatalf("OnClose reported %v; want nil", cause)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("an empty drain did not release at once")
	}
}

type markLayer struct {
	PassThrough
	opened atomic.Int32
}

func (m *markLayer) OnOpen(ctx LayerContext) {
	m.opened.Add(1)
	ctx.Open()
}

func overrideListener(t *testing.T, count int) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = listener.Close()
	})
	go func() {
		var held []net.Conn
		defer func() {
			for _, peer := range held {
				_ = peer.Close()
			}
		}()
		for range count {
			peer, err := listener.Accept()
			if err != nil {
				return
			}
			held = append(held, peer)
		}
		time.Sleep(200 * time.Millisecond)
	}()
	return listener.Addr().String()
}

func dialOverride(t *testing.T, events *layeredEvents, addr string, opts ...DialOption) Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), layerWait)
	defer cancel()
	conn, err := events.DialContext(ctx, "tcp://"+addr, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = conn.Close()
	})
	return conn
}

func TestDialWithLayersEmptyIsPassThrough(t *testing.T) {
	def := &markLayer{}
	addr := overrideListener(t, 1)
	events := dialLayered(t, addr, []Layer{LayerFunc(func(LayerContext) LayerHandler {
		return def
	})}, func(ev *Events) {
	})
	conn := dialOverride(t, events, addr, WithLayers())
	if _, ok := conn.(*layerNode); !ok {
		t.Fatal("a layered dial did not return the top layer's connection")
	}
	if got := def.opened.Load(); got != 0 {
		t.Fatalf("the default chain ran under an empty override %d times", got)
	}
}

func TestWithLayersLayerCap(t *testing.T) {
	addr := overrideListener(t, 0)
	events := dialLayered(t, addr, []Layer{PassThroughLayer}, func(ev *Events) {
	})
	chain := make([]Layer, maxLayers+1)
	for i := range chain {
		chain[i] = PassThroughLayer
	}
	ctx, cancel := context.WithTimeout(context.Background(), layerWait)
	defer cancel()
	_, err := events.DialContext(ctx, "tcp://"+addr, WithLayers(chain...))
	if err == nil || !strings.Contains(err.Error(), "at most") {
		t.Fatalf("DialContext = %v, want the layer cap refused", err)
	}
}

func acceptorLayoutName(reusePort bool) string {
	if reusePort {
		return "reuseport-listeners"
	}
	return "single-listener"
}

func TestAcceptShutdownRaceClosesEveryOpenedConn(t *testing.T) {
	for _, reusePort := range []bool{false, true} {
		t.Run(acceptorLayoutName(reusePort), func(t *testing.T) {
			testAcceptShutdownRace(t, reusePort)
		})
	}
}

func testAcceptShutdownRace(t *testing.T, reusePort bool) {
	for round := 0; round < 15; round++ {
		events := &Events{Pollers: 2, ReusePort: reusePort}
		var opened, closed atomic.Int64
		funcs(events).Open = func(Conn) {
			opened.Add(1)
		}
		funcs(events).Close = func(Conn, error) {
			closed.Add(1)
		}
		addrCh := make(chan string, 1)
		funcs(events).Start = func(ev *Events) {
			for _, l := range ev.acceptor.listeners {
				addrCh <- l.ln.Addr().String()
				return
			}
		}
		done := make(chan error, 1)
		go func() {
			done <- events.Serve("tcp://127.0.0.1:0")
		}()
		var addr string
		select {
		case addr = <-addrCh:
		case <-time.After(3 * time.Second):
			t.Fatalf("round %d: Serve did not start", round)
		}
		var wg sync.WaitGroup
		for i := 0; i < 16; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				c, err := net.Dial("tcp", addr)
				if err != nil {
					return
				}
				time.Sleep(time.Duration(i%4) * time.Millisecond)
				_ = c.Close()
			}(i)
		}
		time.Sleep(time.Duration(round%4) * time.Millisecond)
		_ = events.Close(nil)
		wg.Wait()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("round %d: Serve returned %v", round, err)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("round %d: Serve did not stop", round)
		}
		if o, c := opened.Load(), closed.Load(); o != c {
			t.Fatalf("round %d: %d connections opened but %d closed", round, o, c)
		}
	}
}

func TestShutdownJoinsPendingWake(t *testing.T) {
	ev := &Events{Pollers: 1}
	started := make(chan struct{})
	opened := make(chan *fdConn, 1)
	closed := make(chan struct{})
	funcs(ev).Start = func(*Events) {
		close(started)
	}
	funcs(ev).Open = func(conn Conn) {
		opened <- conn.(*fdConn)
	}
	funcs(ev).Close = func(Conn, error) {
		close(closed)
	}
	serveDone := make(chan error, 1)
	go func() {
		serveDone <- ev.Serve()
	}()
	<-started
	client, server := tcpConnectionPair(t)
	defer client.Close()
	if _, err := ev.Adopt(server); err != nil {
		t.Fatal(err)
	}
	conn := <-opened
	for deadline := time.Now().Add(time.Second); conn.taskState.Load()&taskScheduledBit != 0; {
		if time.Now().After(deadline) {
			t.Fatal("OnOpen did not finish")
		}
		time.Sleep(time.Millisecond)
	}
	paused, resume := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	release := func() {
		releaseOnce.Do(func() {
			close(resume)
		})
	}
	wakeDone := make(chan error, 1)
	testHookTaskClaimed = func(c *fdConn) {
		if c == conn {
			close(paused)
			<-resume
		}
	}
	defer func() {
		release()
		<-wakeDone
		testHookTaskClaimed = nil
	}()
	go func() {
		wakeDone <- conn.Wake()
	}()
	select {
	case <-paused:
	case <-time.After(time.Second):
		t.Fatal("wake admission did not pause")
	}
	if conn.loop.ioTurnCount() == 0 {
		t.Fatal("claim visible without its turn reservation behind it")
	}
	ev.Close(nil)
	select {
	case err := <-serveDone:
		if err != nil && err != net.ErrClosed {
			t.Error(err)
		}
		select {
		case <-closed:
		default:
			t.Error("Serve returned before OnClose: pending Wake claim was not joined")
		}
		if !conn.close.isReleased() {
			t.Error("Serve returned with the connection fd still owned")
		}
		if _, err := unix.FcntlInt(uintptr(conn.Fd()), unix.F_GETFD, 0); err != unix.EBADF {
			t.Errorf("Serve returned with socket fd still open: F_GETFD error = %v", err)
		}
	case <-time.After(100 * time.Millisecond):
		release()
		select {
		case <-serveDone:
		case <-time.After(3 * time.Second):
			t.Fatal("Serve did not finish after Wake admission resumed")
		}
		select {
		case <-closed:
		case <-time.After(3 * time.Second):
			t.Fatal("OnClose was never delivered")
		}
	}
}

func TestUnixConnectionColdStateIsLazy(t *testing.T) {
	if unsafe.Sizeof(uintptr(0)) == 8 {
		maximum := uintptr(256) + (unsafe.Sizeof(inboundScope{})+7)&^7
		if size := unsafe.Sizeof(fdConn{}); size > maximum {
			t.Fatalf("fdConn size = %d bytes, want at most %d", size, maximum)
		}
	}
	conn := &fdConn{}
	conn.entry = (&Events{}).plainEntry()
	if conn.udp != nil || conn.deadlines != nil || conn.isDatagram() {
		t.Fatal("new TCP connection allocated cold state")
	}
	var deadlineErr error
	allocations := testing.AllocsPerRun(1000, func() {
		deadlineErr = conn.applyDeadline(deadlineBoth, time.Time{})
	})
	if deadlineErr != nil {
		t.Fatal(deadlineErr)
	}
	if allocations != 0 || conn.deadlines != nil {
		t.Fatalf("clearing unused deadlines allocated %.2f objects", allocations)
	}
	if err := conn.applyDeadline(deadlineRead, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if conn.deadlines == nil || conn.deadlines.readTimer == nil {
		t.Fatal("nonzero deadline did not initialize deadline state")
	}
	conn.stopDeadlines()
	conn.udp = &unixUDPState{}
	if !conn.isDatagram() {
		t.Fatal("UDP state did not mark connection as datagram")
	}
}

func TestServeReturnsWithUDPAddressFree(t *testing.T) {
	previous := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(previous)
	for round := range 100 {
		var addr string
		events := &Events{Pollers: 1}
		events.Hooks = FuncHandler{Start: func(ev *Events) {
			listener, err := ev.Listen("udp4://127.0.0.1:0")
			if err != nil {
				t.Error(err)
			} else {
				addr = listener.Addr().String()
			}
			_ = ev.Close(nil)
		}}
		if err := events.Serve(); err != nil {
			t.Fatal(err)
		}
		if addr == "" {
			t.FailNow()
		}
		probe, err := net.ListenPacket("udp4", addr)
		if err != nil {
			t.Fatalf("round %d: Serve returned with %s still bound: %v", round, addr, err)
		}
		_ = probe.Close()
	}
}

func TestServeWaitsForAListenerCloseUnderWay(t *testing.T) {
	previous := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(previous)
	started := make(chan struct{})
	events := &Events{Pollers: 1, Hooks: FuncHandler{Start: func(*Events) {
		close(started)
	}}}
	served := make(chan error, 1)
	go func() {
		served <- events.Serve()
	}()
	<-started
	listener, err := events.Listen("udp4://127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	published, release := make(chan struct{}), make(chan struct{})
	hold := func(*fdConn) {
		close(published)
		<-release
	}
	testHookSocketClosePublished.Store(&hold)
	defer testHookSocketClosePublished.Store(nil)
	closed := make(chan struct{})
	go func() {
		_ = listener.Close()
		close(closed)
	}()
	select {
	case <-published:
	case <-time.After(5 * time.Second):
		t.Fatal("the Listener.Close never published its close")
	}
	testHookSocketClosePublished.Store(nil)
	_ = events.Close(nil)
	select {
	case <-served:
		close(release)
		t.Fatal("Serve returned while the socket's close was still to run")
	case <-time.After(200 * time.Millisecond):
	}
	close(release)
	<-closed
	if err := <-served; err != nil {
		t.Fatal(err)
	}
	probe, err := net.ListenPacket("udp4", listener.Addr().String())
	if err != nil {
		t.Fatalf("Serve returned with the address still bound: %v", err)
	}
	_ = probe.Close()
}

type pausedBottomInput struct {
	PassThrough
	down chan Lower
	err  chan error
}

func (h *pausedBottomInput) OnOpen(ctx LayerContext) {
	h.err <- ctx.Lower().PauseRead()
	h.down <- ctx.Lower()
	ctx.Open()
}

func TestPausedNativeReadsReportResetWithoutResume(t *testing.T) {
	for _, mode := range []string{"pause-in-open", "pause-in-read", "capacity-hold", "bottom-layer"} {
		t.Run(mode, func(t *testing.T) {
			const limit = 64
			ready := make(chan error, 1)
			closed := make(chan error, 1)
			var once sync.Once
			releaseRead := make(chan struct{})
			var release sync.Once
			defer release.Do(func() {
				close(releaseRead)
			})
			handler := FuncHandler{Open: func(c Conn) {
				if mode == "pause-in-open" {
					ready <- c.PauseRead()
				}
			}, Read: func(c Conn) error {
				if mode == "pause-in-read" && c.InboundBuffered() != 0 {
					once.Do(func() {
						ready <- c.PauseRead()
						<-releaseRead
					})
				}
				if mode == "capacity-hold" && c.InboundBuffered() >= limit {
					once.Do(func() {
						ready <- nil
					})
				}
				return nil
			}, Close: func(_ Conn, err error) {
				closed <- err
			}}
			var layers []Layer
			if mode == "bottom-layer" {
				bottom := &pausedBottomInput{down: make(chan Lower, 1), err: ready}
				layers = []Layer{LayerFunc(func(LayerContext) LayerHandler {
					return bottom
				})}
			}
			peer, server := readControlServe(t, &Events{Pollers: 1, ReadBufferSize: limit, MaxInboundBuffered: limit, Handler: handler}, layers...)
			if mode == "pause-in-read" || mode == "capacity-hold" {
				count := limit
				if mode == "pause-in-read" {
					count = 8
				}
				if _, err := peer.Write(bytes.Repeat([]byte("x"), count)); err != nil {
					t.Fatal(err)
				}
			}
			if err := readControlAwait(t, ready, "the read pause before reset"); err != nil {
				t.Fatal(err)
			}
			if mode == "capacity-hold" {
				conn := server.(*fdConn)
				waitFor(t, time.Second, conn.readHeld)
				if !conn.readHeld() {
					t.Fatal("the producer did not enter its capacity hold before reset")
				}
			}
			tcp := peer.(*net.TCPConn)
			if err := tcp.SetLinger(0); err != nil {
				t.Fatal(err)
			}
			if err := tcp.Close(); err != nil {
				t.Fatal(err)
			}
			if mode == "pause-in-read" {
				conn := server.(*fdConn)
				waitFor(t, time.Second, func() bool {
					return conn.taskState.Load()&ioEventHangup != 0
				})
				if conn.taskState.Load()&ioEventHangup == 0 {
					t.Fatal("reset was not queued while the read callback was paused")
				}
				release.Do(func() {
					close(releaseRead)
				})
			}
			select {
			case err := <-closed:
				if !errors.Is(err, syscall.ECONNRESET) {
					t.Fatalf("OnClose = %v, want connection reset", err)
				}
			case <-time.After(time.Second):
				t.Fatal("reset remained hidden behind the read pause without ResumeRead")
			}
		})
	}
}

const pinnedSocketBuffer = 16 << 10

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

func waitWriteBlockedAndSettled(t *testing.T, conn *fdConn) {
	t.Helper()
	const settled = 20
	last, same := int64(-1), 0
	for deadline := time.Now().Add(5 * time.Second); same < settled; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("output did not settle behind a blocked write: writeBlocked=%v pending=%d", conn.writeBlocked(), conn.pending.Load())
		}
		pending := conn.pending.Load()
		if conn.writeBlocked() && pending == last {
			same++
		} else {
			same = 0
		}
		last = pending
	}
} // consecutive samples, 10ms apart

func stateOnTurn(t *testing.T, conn Conn, seen *atomic.Uint32, want ConnState) ConnState {
	t.Helper()
	deadline := time.Now().Add(layerWait)
	for time.Now().Before(deadline) {
		_ = conn.Wake()
		if state := ConnState(seen.Load()); state&want == want {
			return state
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("State() = %b, want bits %b", ConnState(seen.Load()), want)
	return 0
}

func TestConnStateBits(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = listener.Close()
	})
	peerDone := make(chan struct{})
	peerRelease := make(chan struct{})
	release := sync.OnceFunc(func() {
		close(peerRelease)
	})
	t.Cleanup(release)
	go func() {
		defer close(peerDone)
		peer, err := listener.Accept()
		if err != nil {
			return
		}
		defer peer.Close()
		if _, err := peer.Write([]byte("hi")); err != nil {
			return
		}
		<-peerRelease
		_ = peer.(*net.TCPConn).CloseWrite()
		_, _ = io.Copy(io.Discard, peer)
	}()
	opened := make(chan Conn, 1)
	var seen, closeSeen atomic.Uint32
	events := dialLayered(t, listener.Addr().String(), nil, func(ev *Events) {
		ev.AllowHalfOpen = true
		ev.Handler = FuncHandler{Open: func(c Conn) {
			signal(opened, c)
		}, Read: func(c Conn) error {
			seen.Store(uint32(c.State()))
			_, _ = c.Discard(c.InboundBuffered())
			return nil
		}, Close: func(c Conn, _ error) {
			closeSeen.Store(uint32(c.State()))
		}}
	})
	ctx, cancel := context.WithTimeout(context.Background(), layerWait)
	defer cancel()
	if _, err := events.DialContext(ctx, "tcp://"+listener.Addr().String()); err != nil {
		t.Fatal(err)
	}
	server := <-opened
	if err := server.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	if state := stateOnTurn(t, server, &seen, StateWriteEnded); state&StateReadEnded != 0 {
		t.Fatalf("State() = %b, the peer has not ended yet", state)
	}
	release()
	stateOnTurn(t, server, &seen, StateReadEnded|StateWriteEnded)
	deadline := time.Now().Add(layerWait)
	for ConnState(closeSeen.Load())&StateClosed == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	both := StateReadEnded | StateWriteEnded | StateClosing | StateClosed
	if state := ConnState(closeSeen.Load()); state&both != both {
		t.Fatalf("OnClose saw State() = %b, want both ends ended, closing and closed", state)
	}
	<-peerDone
}

type testConnection struct {
	conn *fdConn
	peer int
	stop func()
}

type nativeTaskExecutor struct{}

func (nativeTaskExecutor) Submit(task IOTask) bool {
	go task.RunTask()
	return true
}

func (executor nativeTaskExecutor) SubmitBatch(tasks []IOTask) int {
	for _, task := range tasks {
		executor.Submit(task)
	}
	return len(tasks)
}

type rejectingNativeExecutor struct{}

func (rejectingNativeExecutor) Submit(IOTask) bool {
	return false
}

func (rejectingNativeExecutor) SubmitBatch([]IOTask) int {
	return 0
}

type batchNativeExecutor struct {
	taskCalls   atomic.Int32
	batchCalls  atomic.Int32
	rejectAfter int
}

func (executor *batchNativeExecutor) Submit(task IOTask) bool {
	executor.taskCalls.Add(1)
	go task.RunTask()
	return true
}

func (executor *batchNativeExecutor) SubmitBatch(tasks []IOTask) int {
	executor.batchCalls.Add(1)
	accepted := len(tasks)
	if executor.rejectAfter >= 0 {
		accepted = min(accepted, executor.rejectAfter)
	}
	for _, task := range tasks[:accepted] {
		go task.RunTask()
	}
	return accepted
}

func newTestConnection(t *testing.T, events *Events) testConnection {
	t.Helper()
	testConn, registered := startTestConnection(t, events)
	if err := <-registered; err != nil {
		t.Fatal(err)
	}
	return testConn
}

func startTestConnection(t *testing.T, events *Events) (testConnection, <-chan error) {
	t.Helper()
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	return startTestConnectionOn(t, events, fds)
}

func startTestConnectionOn(t *testing.T, events *Events, fds [2]int) (testConnection, <-chan error) {
	t.Helper()
	if events.readBufSize == 0 {
		if err := events.initConfig(); err != nil {
			t.Fatal(err)
		}
	}
	loop, err := newEventLoop(events)
	if err != nil {
		t.Fatal(err)
	}
	events.loops = []*eventLoop{loop}
	if err = unix.SetNonblock(fds[0], true); err != nil {
		t.Fatal(err)
	}
	if err = unix.SetNonblock(fds[1], true); err != nil {
		t.Fatal(err)
	}
	conn := &fdConn{fd: int32(fds[0])}
	conn.loop = loop
	conn.entry = events.plainEntry()
	startTestLoop(events, loop)
	registered := make(chan error, 1)
	go func() {
		registered <- events.addConn(conn)
	}()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			if !conn.isClosing() {
				_ = conn.Abort(io.EOF)
			}
			select {
			case <-stopTestLoop(loop, nil):
			case <-time.After(2 * time.Second):
				t.Error("event loop did not stop")
			}
			_ = unix.Close(fds[1])
		})
	}
	t.Cleanup(stop)
	return testConnection{conn: conn, peer: fds[1], stop: stop}, registered
}

func readPeer(t *testing.T, fd int, size int) []byte {
	t.Helper()
	result := make([]byte, size)
	offset := 0
	deadline := time.Now().Add(2 * time.Second)
	for offset < size && time.Now().Before(deadline) {
		n, err := unix.Read(fd, result[offset:])
		if n > 0 {
			offset += n
		}
		if err == nil {
			continue
		}
		if errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EWOULDBLOCK) {
			time.Sleep(time.Millisecond)
			continue
		}
		t.Fatal(err)
	}
	if offset != size {
		t.Fatalf("read %d bytes, want %d", offset, size)
	}
	return result
}

func TestBatchExecutorPartialRejection(t *testing.T) {
	executor := &batchNativeExecutor{rejectAfter: 1}
	woken := make(chan Conn, 1)
	closed := make(chan Conn, 1)
	events := withFuncs(&Events{}, FuncHandler{Read: func(conn Conn) error {
		woken <- conn
		return nil
	}, Close: func(conn Conn, _ error) {
		signal(closed, conn)
	}})
	pool := newIOTaskPool(executor)
	loop := &eventLoop{ioPool: pool}
	first := &fdConn{commonConn: commonConn{loop: loop}}
	first.entry = events.plainEntry()
	first.taskState.Store(ioEventWake)
	first.taskState.Or(taskScheduledBit)
	if !loop.acquireIO() {
		t.Fatal("failed to reserve first connection task")
	}
	second := &fdConn{commonConn: commonConn{loop: loop}}
	second.entry = events.plainEntry()
	second.close.phase.Store(closeResourcesReleased)
	second.taskState.Store(ioEventClose)
	second.taskState.Or(taskScheduledBit)
	if !loop.acquireIO() {
		t.Fatal("failed to reserve second connection task")
	}
	if !pool.submitBatch([]IOTask{first, second}) {
		t.Fatal("batch submission was rejected by the I/O pool")
	}
	select {
	case conn := <-woken:
		if conn != first {
			t.Fatalf("woken connection = %p, want %p", conn, first)
		}
	case <-time.After(time.Second):
		t.Fatal("accepted task did not run")
	}
	select {
	case conn := <-closed:
		if conn != second {
			t.Fatalf("closed connection = %p, want %p", conn, second)
		}
	case <-time.After(time.Second):
		t.Fatal("rejected batch suffix was not closed")
	}
	pool.stop()
	if calls := executor.batchCalls.Load(); calls != 1 {
		t.Fatalf("SubmitBatch calls = %d, want 1", calls)
	}
	if calls := executor.taskCalls.Load(); calls != 0 {
		t.Fatalf("single Submit calls = %d, want 0", calls)
	}
}

func TestConnectionTaskExecutorRejectionClosesConnection(t *testing.T) {
	var opened, closed atomic.Int32
	events := &Events{Pollers: 1, Executor: rejectingNativeExecutor{}}
	funcs(events).Open = func(Conn) {
		opened.Add(1)
	}
	funcs(events).Close = func(Conn, error) {
		closed.Add(1)
	}
	testConn, registered := startTestConnection(t, events)
	if err := <-registered; !errors.Is(err, ErrFirstTurnRefused) {
		t.Fatalf("registration error = %v, want %v", err, ErrFirstTurnRefused)
	}
	conn := testConn.conn
	for deadline := time.Now().Add(time.Second); conn.close.phase.Load() != closeCallbackDelivered || conn.taskState.Load()&taskScheduledBit != 0; {
		if time.Now().After(deadline) {
			t.Fatalf("executor rejection did not close connection: scheduled=%v phase=%d turns=%d", conn.taskState.Load()&taskScheduledBit != 0, conn.close.phase.Load(), conn.loop.ioTurnCount())
		}
		time.Sleep(time.Millisecond)
	}
	if got := opened.Load(); got != 0 {
		t.Fatalf("OnOpen ran %d times for a refused open turn", got)
	}
	if got := closed.Load(); got != 0 {
		t.Fatalf("OnClose ran %d times for a connection that never opened", got)
	}
	if _, err := conn.Write([]byte("x")); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("Write after the refused open turn = %v, want net.ErrClosed", err)
	}
}

func TestShutdownRetainsTaskErrorAfterNilCloseRequest(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	closed := make(chan error, 1)
	wantErr := errors.New("callback failed during shutdown")
	events := &Events{Pollers: 1}
	funcs(events).Read = func(conn Conn) error {
		close(started)
		<-release
		_, _ = conn.Discard(-1)
		return wantErr
	}
	funcs(events).Close = func(_ Conn, err error) {
		closed <- err
	}
	testConn := newTestConnection(t, events)
	if _, err := unix.Write(testConn.peer, []byte("block")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("connection task did not start")
	}
	stopTestLoop(testConn.conn.loop, nil)
	deadline := time.Now().Add(time.Second)
	for !testConn.conn.isClosing() {
		if time.Now().After(deadline) {
			close(release)
			t.Fatal("shutdown did not mark the connection closing")
		}
		runtime.Gosched()
	}
	close(release)
	select {
	case err := <-closed:
		if !errors.Is(err, wantErr) {
			t.Fatalf("OnClose error = %v, want %v", err, wantErr)
		}
	case <-time.After(time.Second):
		t.Fatal("callback error was lost during shutdown")
	}
}

func TestExternalWriteCopiesAndWakePreservesOrder(t *testing.T) {
	woke := make(chan struct{}, 1)
	events := &Events{Pollers: 1}
	funcs(events).Read = func(conn Conn) error {
		_, err := conn.WriteString("second")
		woke <- struct{}{}
		return err
	}
	testConn := newTestConnection(t, events)
	first := []byte("first-")
	if n, err := testConn.conn.Write(first); err != nil || n != len(first) {
		t.Fatalf("Write = %d, %v", n, err)
	}
	copy(first, "xxxxxx")
	if err := testConn.conn.Wake(); err != nil {
		t.Fatal(err)
	}
	if got := string(readPeer(t, testConn.peer, len("first-second"))); got != "first-second" {
		t.Fatalf("peer received %q", got)
	}
	select {
	case <-woke:
	case <-time.After(time.Second):
		t.Fatal("Wake did not invoke OnData")
	}
}

func TestOutboundLimitDoesNotRejectImmediateLoopWrite(t *testing.T) {
	result := make(chan error, 1)
	events := &Events{Pollers: 1, MaxOutboundBuffered: 4}
	funcs(events).Open = func(conn Conn) {
		n, err := conn.Write([]byte("12345678"))
		if err == nil && n != 8 {
			err = fmt.Errorf("Write returned %d bytes", n)
		}
		result <- err
	}
	testConn := newTestConnection(t, events)
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	if got := string(readPeer(t, testConn.peer, 8)); got != "12345678" {
		t.Fatalf("peer received %q", got)
	}
}

func TestCallbackThresholdBatchesWithoutTasks(t *testing.T) {
	outbound := make(chan int, 2)
	events := &Events{Pollers: 1, WriteBufferedThreshold: 16}
	funcs(events).Open = func(conn Conn) {
		if err := conn.WriteByte('a'); err != nil {
			t.Error(err)
		}
		if err := conn.WriteByte('b'); err != nil {
			t.Error(err)
		}
		if err := conn.WriteByte('c'); err != nil {
			t.Error(err)
		}
	}
	funcs(events).BytesWritten = func(_ Conn, written int) {
		outbound <- written
	}
	testConn := newTestConnection(t, events)
	if got := string(readPeer(t, testConn.peer, 3)); got != "abc" {
		t.Fatalf("peer received %q", got)
	}
	if written := <-outbound; written != 3 {
		t.Fatalf("OnWrite bytes = %d", written)
	}
	select {
	case extra := <-outbound:
		t.Fatalf("threshold write used an extra flush of %d bytes", extra)
	default:
	}
}

func TestExistingUDPPeerPacketDoesNotAllocate(t *testing.T) {
	var opened, packets int
	events := withFuncs(&Events{}, FuncHandler{Open: func(Conn) {
		opened++
	}, Read: func(Conn) error {
		packets++
		return nil
	}})
	loop := &eventLoop{}
	server := &fdConn{fd: -1, udp: &unixUDPState{peers: make(map[socket.UDPAddress]*fdConn)}}
	server.loop = loop
	server.entry = events.plainEntry()
	server.ioOwner.Store(currentGoroutineID())
	source := socket.UDPAddress{Family: 4, Port: 1234}
	copy(source.Addr[:4], []byte{127, 0, 0, 1})
	server.handleUDPPacket([]byte("first"), source)
	if opened != 1 || packets != 1 || len(server.udp.peers) != 1 {
		t.Fatalf("first packet created %d peers and delivered %d packets", opened, packets)
	}
	next := []byte("next")
	allocs := testing.AllocsPerRun(1000, func() {
		server.handleUDPPacket(next, source)
	})
	if allocs != 0 {
		t.Fatalf("existing UDP peer packet allocated %.2f objects/op", allocs)
	}
	if opened != 1 || packets != 1002 || len(server.udp.peers) != 1 {
		t.Fatalf("steady packets created %d peers and delivered %d packets", opened, packets)
	}
}

func newUDP4TestPair(t *testing.T) (receiver, sender int, target *unix.SockaddrInet4) {
	t.Helper()
	receiver, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err = unix.Bind(receiver, &unix.SockaddrInet4{Addr: [4]byte{127, 0, 0, 1}}); err != nil {
		_ = unix.Close(receiver)
		t.Fatal(err)
	}
	addr, err := unix.Getsockname(receiver)
	if err != nil {
		_ = unix.Close(receiver)
		t.Fatal(err)
	}
	target = addr.(*unix.SockaddrInet4)
	sender, err = unix.Socket(unix.AF_INET, unix.SOCK_DGRAM, 0)
	if err != nil {
		_ = unix.Close(receiver)
		t.Fatal(err)
	}
	if err = unix.SetNonblock(receiver, true); err != nil {
		_ = unix.Close(receiver)
		_ = unix.Close(sender)
		t.Fatal(err)
	}
	return receiver, sender, target
}

func TestExistingUDPPeerReceiveDoesNotAllocate(t *testing.T) {
	receiver, sender, target := newUDP4TestPair(t)
	defer unix.Close(receiver)
	defer unix.Close(sender)
	packets := 0
	events := withFuncs(&Events{ReadBufferSize: 64}, FuncHandler{Read: func(Conn) error {
		packets++
		return nil
	}})
	if err := events.initConfig(); err != nil {
		t.Fatal(err)
	}
	loop := &eventLoop{}
	server := &fdConn{fd: int32(receiver), udp: &unixUDPState{peers: make(map[socket.UDPAddress]*fdConn)}}
	server.loop = loop
	server.entry = events.plainEntry()
	server.ioOwner.Store(currentGoroutineID())
	payload := []byte{'x'}
	sendAndReceive := func() {
		before := packets
		if err := unix.Sendto(sender, payload, 0, target); err != nil {
			panic(err)
		}
		deadline := time.Now().Add(5 * time.Second)
		for polls := 0; packets == before; polls++ {
			if polls%1024 == 1023 && time.Now().After(deadline) {
				panic("sent UDP packet was not consumed")
			}
			if err := server.onRecvUDP(); err != nil {
				panic(err)
			}
			if packets == before {
				runtime.Gosched()
			}
		}
		if packets != before+1 {
			panic("UDP receive consumed more than one packet")
		}
	}
	sendAndReceive()
	const measuredPackets = 1000
	allocs := testing.AllocsPerRun(measuredPackets, sendAndReceive)
	if allocs != 0 {
		t.Fatalf("full existing-peer receive path allocated %.2f objects/packet", allocs)
	}
	if packets != measuredPackets+2 || len(server.udp.peers) != 1 {
		t.Fatalf("received %d packets across %d peers", packets, len(server.udp.peers))
	}
}

func TestUDPReadEventDoesNotExceedPacketBudget(t *testing.T) {
	receiver, sender, target := newUDP4TestPair(t)
	defer unix.Close(receiver)
	defer unix.Close(sender)
	packets := 0
	events := withFuncs(&Events{ReadBufferSize: 64}, FuncHandler{Read: func(Conn) error {
		packets++
		return nil
	}})
	if err := events.initConfig(); err != nil {
		t.Fatal(err)
	}
	loop := &eventLoop{}
	conn := &fdConn{fd: int32(receiver), udp: &unixUDPState{}}
	conn.loop = loop
	conn.entry = events.plainEntry()
	conn.ioOwner.Store(currentGoroutineID())
	_ = hasPeerInBatch([]socket.UDPReceive{{Addr: socket.UDPAddress{Port: 1}}}, socket.UDPAddress{Port: 1})
	_ = hasPeerInBatch([]socket.UDPReceive{{Addr: socket.UDPAddress{Port: 1}}}, socket.UDPAddress{Port: 2})
	for range 272 {
		if err := unix.Sendto(sender, []byte{'x'}, 0, target); err != nil {
			t.Fatal(err)
		}
	}
	before := packets
	if err := conn.onRecvUDP(); err != nil {
		t.Fatal(err)
	}
	if handled := packets - before; handled > 256 {
		t.Fatalf("first read event handled %d packets, limit 256", handled)
	}
	before = packets
	if err := conn.onRecvUDP(); err != nil {
		t.Fatal(err)
	}
	if handled := packets - before; handled > 256 {
		t.Fatalf("second read event handled %d packets, limit 256", handled)
	}
}

func fillDatagramSendBuffer(t *testing.T, fd int) []byte {
	t.Helper()
	if err := unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_SNDBUF, 4096); err != nil {
		t.Fatal(err)
	}
	if err := unix.SetNonblock(fd, true); err != nil {
		t.Fatal(err)
	}
	payload := make([]byte, 1024)
	for range 1 << 16 {
		n, err := unix.Write(fd, payload)
		if isUDPSendBlocked(err) {
			return payload
		}
		if err != nil || n != len(payload) {
			t.Fatalf("fill datagram socket = %d, %v", n, err)
		}
	}
	t.Fatal("datagram socket did not reach EAGAIN")
	return nil
}

func TestUDPWouldBlockDoesNotCloseConnection(t *testing.T) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_DGRAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fds[0])
	defer unix.Close(fds[1])
	payload := fillDatagramSendBuffer(t, fds[0])
	direct := &fdConn{fd: int32(fds[0]), udp: &unixUDPState{}}
	direct.entry = (&Events{}).plainEntry()
	direct.loop = &eventLoop{}
	direct.ioOwner.Store(currentGoroutineID())
	directBuffer := AcquireBuffer(len(payload))
	_, _ = directBuffer.Write(payload)
	if n, writeErr := direct.WriteOwned(directBuffer); n != 0 || !isUDPSendBlocked(writeErr) {
		t.Fatalf("direct UDP WriteOwned = %d, %v", n, writeErr)
	}
	if direct.isClosing() {
		t.Fatal("direct EAGAIN closed a datagram that was never accepted")
	}
}

func TestUnixSocketOptions(t *testing.T) {
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	loop := &eventLoop{}
	conn := &fdConn{fd: int32(fd)}
	conn.entry = (&Events{}).plainEntry()
	conn.loop = loop
	for name, set := range map[string]func() error{"linger": func() error {
		return conn.SetLinger(0)
	}, "no delay": func() error {
		return conn.SetNoDelay(false)
	}, "read buffer": func() error {
		return conn.SetReadBuffer(4096)
	}, "write buffer": func() error {
		return conn.SetWriteBuffer(4096)
	}, "keep alive": func() error {
		return conn.SetKeepAlive(true)
	}, "keep alive period": func() error {
		return conn.SetKeepAlivePeriod(1)
	}} {
		t.Run(name, func(t *testing.T) {
			if err := set(); err != nil {
				t.Fatalf("socket option failed: %v", err)
			}
		})
	}
	if boolInt(true) != 1 || boolInt(false) != 0 {
		t.Fatal("boolInt returned an invalid value")
	}
	conn.close.phase.Store(closeRequested)
	if err := conn.SetNoDelay(true); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("closed SetNoDelay error = %v", err)
	}
	conn.close.phase.Store(closeResourcesReleased)
	if err := conn.applySocketOption(optionLinger, 0); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("closed applySocketOption error = %v", err)
	}
}

func TestCloseUnregisteredDescriptorOwnership(t *testing.T) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fds[1])
	conn := &fdConn{fd: int32(fds[0])}
	conn.entry = (&Events{}).plainEntry()
	conn.closeUnregistered()
	conn.closeUnregistered()
	if _, err := unix.Write(fds[0], []byte("x")); !errors.Is(err, unix.EBADF) {
		t.Fatalf("write after close error = %v", err)
	}
	requested, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(requested[1])
	conn = &fdConn{fd: int32(requested[0])}
	conn.entry = (&Events{}).plainEntry()
	if !conn.close.request() {
		t.Fatal("unregistered connection did not accept close request")
	}
	conn.closeUnregistered()
	if phase := conn.close.phase.Load(); phase != closeCallbackDelivered {
		t.Fatalf("unregistered close phase = %d, want callback delivered", phase)
	}
	if _, err := unix.Write(requested[0], []byte("x")); !errors.Is(err, unix.EBADF) {
		t.Fatalf("write after requested close error = %v", err)
	}
}

func TestLoopDirectWritevAndFlush(t *testing.T) {
	type writeResult struct {
		n   int
		err error
	}
	written := make(chan writeResult, 1)
	events := &Events{Pollers: 1}
	funcs(events).Open = func(conn Conn) {
		if n, err := conn.Write(nil); n != 0 || err != nil {
			t.Errorf("empty Write = %d, %v", n, err)
		}
		if n, err := conn.Writev(nil); n != 0 || err != nil {
			t.Errorf("empty Writev = %d, %v", n, err)
		}
		n, err := conn.Writev([][]byte{[]byte("ab"), []byte("cd")})
		if flushErr := conn.Flush(); err == nil {
			err = flushErr
		}
		written <- writeResult{n: n, err: err}
	}
	testConn := newTestConnection(t, events)
	got := <-written
	if got.err != nil || got.n != 4 {
		t.Fatalf("Writev = %d, %v", got.n, got.err)
	}
	if got := string(readPeer(t, testConn.peer, 4)); got != "abcd" {
		t.Fatalf("peer received %q", got)
	}
}

func TestLoopBufferedWritevAndOverflow(t *testing.T) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fds[0])
	defer unix.Close(fds[1])
	loop := &eventLoop{}
	conn := &fdConn{fd: int32(fds[0])}
	conn.entry = (&Events{WriteBufferedThreshold: 16, MaxOutboundBuffered: 8}).plainEntry()
	conn.ioOwner.Store(currentGoroutineID())
	conn.loop = loop
	if n, err := conn.Writev([][]byte{[]byte("a"), []byte("b")}); err != nil || n != 2 {
		t.Fatalf("buffered Writev = %d, %v", n, err)
	}
	if conn.OutboundBuffered() != 2 {
		t.Fatalf("buffered bytes = %d", conn.OutboundBuffered())
	}
	conn.setWriteBlocked(true)
	if n, err := conn.flushOnLoop(); err != nil || n != 0 {
		t.Fatalf("blocked flush = %d, %v", n, err)
	}
	conn.setWriteBlocked(false)
	if err := conn.Flush(); err != nil {
		t.Fatal(err)
	}
	if got := string(readPeer(t, fds[1], 2)); got != "ab" {
		t.Fatalf("peer received %q", got)
	}
	conn.events().MaxOutboundBuffered = 1
	if n, err := conn.Writev([][]byte{[]byte("a"), []byte("b")}); n != 0 || !errors.Is(err, ErrOutboundOverflow) {
		t.Fatalf("overflow Writev = %d, %v", n, err)
	}
	if n, err := conn.Write([]byte("ab")); n != 0 || !errors.Is(err, ErrOutboundOverflow) {
		t.Fatalf("overflow Write = %d, %v", n, err)
	}
	conn.events().MaxOutboundBuffered = 8
	conn.setWriteBlocked(true)
	if err := conn.fireWriteEvent(); err != nil || conn.writeBlocked() {
		t.Fatalf("fireWriteEvent = %v, blocked %v", err, conn.writeBlocked())
	}
	conn.udp = &unixUDPState{}
	if n, err := conn.Writev([][]byte{[]byte("x")}); n != 1 || err != nil {
		t.Fatalf("UDP Writev = %d, %v", n, err)
	}
	if got := string(readPeer(t, fds[1], 1)); got != "x" {
		t.Fatalf("peer received %q", got)
	}
	if err := conn.fireWriteEvent(); err != nil {
		t.Fatalf("UDP fireWriteEvent = %v", err)
	}
}

func TestCorkedOwnedWritesCoalesceInRound(t *testing.T) {
	loop := &eventLoop{}
	events := &Events{MaxOutboundBuffered: 64 << 10, readBufSize: 16 << 10}
	conn := &fdConn{fd: -1, turn: turnCorked}
	conn.entry = events.plainEntry()
	conn.ioOwner.Store(currentGoroutineID())
	conn.loop = loop
	const payloadSize = 1028
	writeOwned := func(value byte) {
		buffer := AcquireBuffer(payloadSize)
		_, _ = buffer.Write(bytes.Repeat([]byte{value}, payloadSize))
		if n, err := conn.WriteOwned(buffer); err != nil || n != payloadSize {
			t.Fatalf("WriteOwned = %d, %v", n, err)
		}
	}
	writeOwned('a')
	vec, _ := conn.outbound.PeekVecN(nil, 8)
	if len(vec) != 1 {
		t.Fatalf("first write segment count = %d, want 1", len(vec))
	}
	first := &vec[0][0]
	writeOwned('b')
	writeOwned('c')
	vec, length := conn.outbound.PeekVecN(nil, 8)
	if len(vec) != 2 {
		t.Fatalf("coalesced segment count = %d, want 2", len(vec))
	}
	if &vec[0][0] != first {
		t.Fatal("first corked write lost ownership transfer")
	}
	if length != 3*payloadSize || len(vec[1]) != 2*payloadSize {
		t.Fatalf("coalesced lengths = %d, %d", length, len(vec[1]))
	}
	conn.outbound.Reset()
	conn.pending.Store(0)
}

func TestUnixWriteFailureAndRejectedQueues(t *testing.T) {
	loop := &eventLoop{}
	conn := &fdConn{fd: -1}
	conn.entry = (&Events{}).plainEntry()
	conn.ioOwner.Store(currentGoroutineID())
	conn.loop = loop
	if n, err := conn.Write([]byte("x")); n != 0 || err == nil {
		t.Fatalf("invalid-fd Write = %d, %v", n, err)
	}
	if !conn.isClosing() {
		t.Fatal("write failure did not request close")
	}
	conn = &fdConn{fd: -1}
	conn.entry = (&Events{}).plainEntry()
	conn.ioOwner.Store(currentGoroutineID())
	conn.loop = loop
	if n, err := conn.Writev([][]byte{[]byte("x")}); n != 0 || err == nil {
		t.Fatalf("invalid-fd Writev = %d, %v", n, err)
	}
	if !conn.isClosing() {
		t.Fatal("writev failure did not request close")
	}
	conn = &fdConn{fd: -1}
	conn.entry = (&Events{}).plainEntry()
	_, _ = conn.outbound.WriteString("x")
	conn.pending.Store(1)
	if n, err := conn.flushOnLoop(); n != 0 || err == nil {
		t.Fatalf("invalid-fd flush = %d, %v", n, err)
	}
	conn = &fdConn{}
	conn.entry = (&Events{MaxOutboundBuffered: 1}).plainEntry()
	if n, err := conn.queueOwnedWrite(bytebuf.CloneBuffer([]byte("xx")), 2); n != 0 || !errors.Is(err, ErrOutboundOverflow) {
		t.Fatalf("oversize owned write = %d, %v", n, err)
	}
}

func TestRejectedExternalWriteDoesNotAllocate(t *testing.T) {
	payload := make([]byte, 65537)
	vec := [][]byte{payload}
	for _, test := range []struct {
		name  string
		setup func(*fdConn)
		write func(*fdConn) (int, error)
		want  error
	}{{name: "Write/payload_limit", setup: func(conn *fdConn) {
		conn.pending.Store(1)
	}, write: func(conn *fdConn) (int, error) {
		return conn.Write(payload)
	}, want: ErrOutboundOverflow}, {name: "Writev/payload_limit", setup: func(conn *fdConn) {
		conn.pending.Store(1)
	}, write: func(conn *fdConn) (int, error) {
		return conn.Writev(vec)
	}, want: ErrOutboundOverflow}} {
		t.Run(test.name, func(t *testing.T) {
			events := &Events{MaxOutboundBuffered: len(payload)}
			conn := &fdConn{}
			conn.entry = events.plainEntry()
			conn.loop = &eventLoop{}
			test.setup(conn)
			pending := conn.pending.Load()
			var n int
			var err error
			allocs := testing.AllocsPerRun(100, func() {
				n, err = test.write(conn)
			})
			if allocs != 0 {
				t.Fatalf("rejected write allocated %.2f objects/op", allocs)
			}
			if n != 0 || !errors.Is(err, test.want) {
				t.Fatalf("rejected write = %d, %v", n, err)
			}
			if conn.pending.Load() != pending {
				t.Fatalf("rejected write changed pending counter to %d", conn.pending.Load())
			}
		})
	}
}

func TestUnixDeadlineAndClosedOperations(t *testing.T) {
	closed := make(chan error, 1)
	events := withFuncs(&Events{Pollers: 1}, FuncHandler{Close: func(_ Conn, err error) {
		signal(closed, err)
	}})
	testConn := newTestConnection(t, events)
	if err := testConn.conn.SetReadBuffer(4096); err != nil {
		t.Fatal(err)
	}
	if err := testConn.conn.SetDeadline(time.Time{}); err != nil {
		t.Fatal(err)
	}
	if err := testConn.conn.SetWriteDeadline(time.Now().Add(20 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-closed:
		if !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("write deadline error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("write deadline did not close connection")
	}
	if err := testConn.conn.SetDeadline(time.Time{}); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("closed deadline error = %v", err)
	}
	if err := testConn.conn.Flush(); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("closed Flush error = %v", err)
	}
	if err := testConn.conn.Wake(); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("closed Wake error = %v", err)
	}
}

func TestEventAndListenerHelperBranches(t *testing.T) {
	events := &Events{Pollers: 1, ReadBufferSize: 64}
	if err := events.initConfig(); err != nil {
		t.Fatal(err)
	}
	loop, err := newEventLoop(events)
	if err != nil {
		t.Fatal(err)
	}
	defer loop.poller.Close(nil)
	events.loops = []*eventLoop{loop}
	if events.selectLoop() != loop {
		t.Fatal("the only loop was not selected")
	}
	if (&Events{}).selectLoop() != nil {
		t.Fatal("selectLoop returned a loop from an empty set")
	}
	conn := &fdConn{}
	conn.entry = events.plainEntry()
	conn.inboundTail = []byte("discard")
	if err := conn.fireOnRead(); err != nil || conn.InboundBuffered() != 0 {
		t.Fatalf("default OnData = %v, buffered %d", err, conn.InboundBuffered())
	}
	wantErr := errors.New("handler")
	funcs(events).Read = func(Conn) error {
		return wantErr
	}
	conn.entry, _ = events.newEntry(nil)
	if err := conn.fireOnRead(); !errors.Is(err, wantErr) {
		t.Fatalf("OnData error = %v", err)
	}
	var inbound, outbound int
	funcs(events).BytesRead = func(Conn, int) {
		inbound++
	}
	funcs(events).BytesWritten = func(Conn, int) {
		outbound++
	}
	conn.entry, _ = events.newEntry(nil)
	events.onSocketBytesRead(conn, 0)
	events.onSocketBytesRead(conn, 1)
	events.onSocketBytesWrite(conn, 0)
	events.onSocketBytesWrite(conn, 1)
	if inbound != 1 || outbound != 1 {
		t.Fatalf("byte callbacks = %d, %d", inbound, outbound)
	}
	missing := &fdConn{fd: -1}
	missing.entry = events.plainEntry()
	if err := events.addConn(missing); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("addConn without loop error = %v", err)
	}
	rejected := &fdConn{fd: -1}
	rejected.entry = events.plainEntry()
	if rejected.admitAccepted(false) {
		t.Fatal("admitAccepted accepted a connection without a loop")
	}
	badEvents := &Events{}
	bad := &eventLoop{events: badEvents, fdMap: newFdMap()}
	bad.listeners.Store(&[]*listener{{fd: -1}})
	bad.accepts = newAcceptRunner(bad)
	bad.dispatch(&loopWaiter{}, []poller.Event{{FD: -1, Events: poller.ReadEvents}})
	for deadline := time.Now().Add(5 * time.Second); !badEvents.closing.Load() && time.Now().Before(deadline); {
		time.Sleep(time.Millisecond)
	}
	bad.accepts.stop()
	if !badEvents.closing.Load() {
		t.Fatal("accept error did not close Events")
	}
}

func TestListenerAddressVariants(t *testing.T) {
	ld := &acceptor{}
	for _, address := range []string{"127.0.0.1:0", "udp://127.0.0.1:0"} {
		listener, err := ld.listen(address, false, nil)
		if err != nil {
			t.Fatalf("listen %q: %v", address, err)
		}
		ld.closeListener(listener)
	}
	path := filepath.Join(t.TempDir(), "uio.sock")
	listener, err := ld.listen("unix://"+path, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	ld.closeListener(listener)
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unix socket still exists: %v", err)
	}
	for _, address := range []string{"unsupported://address", "tcp://%", "tcp://bad-address"} {
		if listener, err := ld.listen(address, false, nil); err == nil {
			ld.closeListener(listener)
			t.Fatalf("listen %q unexpectedly succeeded", address)
		}
	}
	listener, err = ld.listen("tcp://127.0.0.1:0", true, nil)
	if err == nil {
		ld.closeListener(listener)
	}
}

func TestEventLoopInterestAndRegistrationErrors(t *testing.T) {
	events := &Events{Pollers: 1, ReadBufferSize: 64}
	if err := events.initConfig(); err != nil {
		t.Fatal(err)
	}
	loop, err := newEventLoop(events)
	if err != nil {
		t.Fatal(err)
	}
	defer loop.poller.Close(nil)
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fds[0])
	defer unix.Close(fds[1])
	conn := &fdConn{fd: int32(fds[0])}
	conn.entry = events.plainEntry()
	conn.loop = loop
	if err := loop.poller.Add(int(conn.fd), poller.Readable); err != nil {
		t.Fatal(err)
	}
	loop.dispatch(loop.newWaiter(), []poller.Event{{FD: 12345, Events: poller.ReadEvents, Tag: 1}})
	failedLoop, err := newEventLoop(events)
	if err != nil {
		t.Fatal(err)
	}
	defer failedLoop.poller.Close(nil)
	closedFD, err := unix.Socket(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.Close(closedFD); err != nil {
		t.Fatal(err)
	}
	failedConn := &fdConn{fd: int32(closedFD)}
	failedConn.entry = events.plainEntry()
	failedConn.loop = failedLoop
	if err := failedLoop.registerConn(failedConn); err == nil {
		t.Fatal("registerConn accepted a closed descriptor")
	}
}

func TestUnixDialFailureAndResourceCleanup(t *testing.T) {
	events := &Events{}
	if _, err := events.Dial("tcp://127.0.0.1:1"); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("unready Dial error = %v", err)
	}
	events.ready.Store(true)
	dialCtx, cancelDial := context.WithCancel(context.Background())
	cancelDial()
	if _, err := events.DialContext(dialCtx, "tcp://127.0.0.1:1"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled DialContext error = %v", err)
	}
	for _, address := range []string{"%", "unsupported://address", "tcp://127.0.0.1:0"} {
		if conn, err := events.Dial(address); err == nil {
			_ = conn.Close()
			t.Fatalf("Dial %q unexpectedly succeeded", address)
		}
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	accepted := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err == nil {
			err = conn.Close()
		}
		accepted <- err
	}()
	if conn, err := events.Dial("tcp://"+listener.Addr().String(), WithUserdata("context")); conn != nil || !errors.Is(err, net.ErrClosed) {
		t.Fatalf("Dial without a worker = %v, %v", conn, err)
	}
	if err := <-accepted; err != nil {
		t.Fatal(err)
	}
	events.closing.Store(true)
	if _, err := events.Dial("tcp://127.0.0.1:1"); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("closing Dial error = %v", err)
	}
}

func TestStoppedLoopRejectsConnections(t *testing.T) {
	loop := &eventLoop{fdMap: newFdMap()}
	loop.stopping.Store(true)
	events := &Events{}
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fds[1])
	conn := &fdConn{fd: int32(fds[0])}
	conn.entry = events.plainEntry()
	conn.loop = loop
	if err := events.addConn(conn); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("stopped addConn error = %v", err)
	}
	fds, err = unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fds[1])
	conn = &fdConn{fd: int32(fds[0])}
	conn.entry = events.plainEntry()
	conn.loop = loop
	if conn.admitAccepted(false) {
		t.Fatal("stopped loop accepted a connection")
	}
}

func TestRegisterConnClosesOutOfRangeFD(t *testing.T) {
	oldMax := fdmap.MaxOpenFiles
	fdmap.MaxOpenFiles = 1
	mapping := fdmap.NewMap[fdConn]()
	fdmap.MaxOpenFiles = oldMax
	events := &Events{ReadBufferSize: 64}
	loop, err := newEventLoop(events)
	if err != nil {
		t.Fatal(err)
	}
	defer loop.poller.Close(nil)
	loop.fdMap = mapping
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fds[1])
	conn := &fdConn{fd: int32(fds[0])}
	conn.loop = loop
	conn.entry = events.plainEntry()
	if err = loop.registerConn(conn); !errors.Is(err, fdmap.ErrOutOfRange) {
		t.Fatalf("registerConn error = %v", err)
	}
	if _, err = unix.FcntlInt(uintptr(fds[0]), unix.F_GETFD, 0); !errors.Is(err, unix.EBADF) {
		t.Fatalf("rejected descriptor was not closed: %v", err)
	}
}

func TestUnixClosedAndCallbackStateBranches(t *testing.T) {
	loop := &eventLoop{}
	events := &Events{}
	conn := &fdConn{fd: -1}
	conn.entry = events.plainEntry()
	conn.loop = loop
	conn.close.phase.Store(closeRequested)
	conn.inboundTail = []byte("discard")
	err := conn.fireOnRead()
	started := conn.beginInboundCallback()
	buffered := conn.InboundBuffered()
	conn.endInboundCallback(started)
	if err != nil || buffered != 0 {
		t.Fatalf("default fireOnRead = %v, buffered %d", err, buffered)
	}
	wantErr := errors.New("callback")
	funcs(events).Read = func(Conn) error {
		return wantErr
	}
	conn.entry, _ = events.newEntry(nil)
	if err := conn.fireOnRead(); !errors.Is(err, wantErr) {
		t.Fatalf("callback fireOnRead error = %v", err)
	}
	if err := conn.Wake(); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("closed Wake error = %v", err)
	}
	if n, err := conn.Write([]byte("x")); n != 0 || !errors.Is(err, net.ErrClosed) {
		t.Fatalf("closed Write = %d, %v", n, err)
	}
	if n, err := conn.Writev([][]byte{[]byte("x")}); n != 0 || !errors.Is(err, net.ErrClosed) {
		t.Fatalf("closed Writev = %d, %v", n, err)
	}
	if n, err := conn.queueOwnedWrite(bytebuf.CloneBuffer([]byte("x")), 1); n != 0 || !errors.Is(err, net.ErrClosed) {
		t.Fatalf("closing queueOwnedWrite = %d, %v", n, err)
	}
	conn.expireDeadline(deadlineRead)
	conn.requestClose(wantErr)
	conn.close.phase.Store(closeResourcesReleased)
	if _, ok := conn.teardown(nil); ok {
		t.Fatal("a released connection was released again")
	}
	if err := conn.SetDeadline(time.Time{}); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("closed in-loop SetDeadline error = %v", err)
	}
	stopped := &eventLoop{}
	stopped.stopping.Store(true)
	rejectedClose := &fdConn{fd: -1}
	rejectedClose.entry = events.plainEntry()
	rejectedClose.loop = stopped
	rejectedClose.requestClose(wantErr)
	if rejectedClose.close.deferred == nil || !errors.Is(rejectedClose.close.deferred.cause, wantErr) {
		t.Fatalf("deferred close error = %v", rejectedClose.close.deferred)
	}
	rejectedTimeout := &fdConn{fd: -1}
	rejectedTimeout.entry = events.plainEntry()
	rejectedTimeout.loop = stopped
	rejectedTimeout.deadlines = &deadlineState{}
	rejectedTimeout.deadlines.readTimerGen.Store(3)
	rejectedTimeout.expireDeadline(deadlineRead)
	rejectedTimeout.deadlines.readGeneration = 3
	rejectedTimeout.deadlines.readDeadline = time.Now().Add(time.Hour)
	rejectedTimeout.handleTimeout(deadlineRead, 2)
	if rejectedTimeout.isClosing() {
		t.Fatal("stale timeout closed the connection")
	}
}

func TestWriteOwnedOverflowReturnsOwnership(t *testing.T) {
	conn := &fdConn{}
	conn.entry = (&Events{MaxOutboundBuffered: 8}).plainEntry()
	conn.loop = &eventLoop{}
	conn.pending.Store(8)
	buffer := AcquireBuffer(8)
	buffer.CommitWrite(copy(buffer.AvailableBuffer()[:7], "payload"))
	n, err := conn.WriteOwned(buffer)
	if n != 0 || !errors.Is(err, ErrOutboundOverflow) {
		t.Fatalf("WriteOwned = %d, %v", n, err)
	}
	if buffer.Len() != 7 || string(buffer.Bytes()) != "payload" {
		t.Fatal("overflowed buffer was consumed")
	}
	ReleaseBuffer(buffer)
	loop := &eventLoop{}
	owner := &fdConn{fd: -1}
	owner.entry = (&Events{WriteBufferedThreshold: 16, MaxOutboundBuffered: 8}).plainEntry()
	owner.ioOwner.Store(currentGoroutineID())
	owner.loop = loop
	owner.pending.Store(8)
	owned := AcquireBuffer(8)
	owned.CommitWrite(copy(owned.AvailableBuffer()[:7], "payload"))
	n, err = owner.WriteOwned(owned)
	if n != 0 || !errors.Is(err, ErrOutboundOverflow) {
		t.Fatalf("task-owner WriteOwned = %d, %v", n, err)
	}
	if owned.Len() != 7 || string(owned.Bytes()) != "payload" {
		t.Fatal("overflowed task-owner buffer was consumed")
	}
	ReleaseBuffer(owned)
}

func TestUnixShutWriteAndRegistrationGuardBranches(t *testing.T) {
	ev := &Events{}
	_ = ev.initConfig()
	loop, err := newEventLoop(ev)
	if err != nil {
		t.Fatal(err)
	}
	defer loop.closePoller(nil)

	stoppingLoop := &eventLoop{events: ev}
	stoppingLoop.stopping.Store(true)
	stoppedConn := &fdConn{commonConn: commonConn{entry: ev.plainEntry(), loop: stoppingLoop}, fd: -1}
	if err := stoppedConn.CloseWrite(); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("stopping CloseWrite = %v", err)
	}

	claimed := &fdConn{commonConn: commonConn{entry: ev.plainEntry(), loop: loop}, fd: -1}
	_ = claimed.tryClaimWrite(0)
	claimed.writeState.Or(writeShutFlag)
	claimed.shutWrite()
	claimed.releaseWrite()

	badShutdown := &fdConn{commonConn: commonConn{entry: ev.plainEntry(), loop: loop}, fd: -1}
	badShutdown.writeState.Store(writeShutFlag)
	badShutdown.shutWrite()
	if !badShutdown.isClosing() {
		t.Fatal("failed Shutdown did not request close")
	}

	negFD := &fdConn{commonConn: commonConn{entry: ev.plainEntry(), loop: loop}, fd: -1}
	if negFD.admitAccepted(false) {
		t.Fatal("negative descriptor passed admitAccepted")
	}

	loop.closePoller(nil)
	unwatchable := &fdConn{commonConn: commonConn{entry: ev.plainEntry(), loop: loop}, fd: 0}
	if unwatchable.admitAccepted(false) {
		t.Fatal("closed poller passed admitAccepted")
	}
}
