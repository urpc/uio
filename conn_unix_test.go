//go:build (linux || darwin || netbsd || freebsd || openbsd || dragonfly) && !stdio

package uio

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urpc/uio/internal/bytebuf"
	"github.com/urpc/uio/internal/fdmap"
	"github.com/urpc/uio/internal/poller"
	"github.com/urpc/uio/internal/socket"
	"golang.org/x/sys/unix"
)

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

func (rejectingNativeExecutor) Submit(IOTask) bool       { return false }
func (rejectingNativeExecutor) SubmitBatch([]IOTask) int { return 0 }

func TestDirectOwnerIsConnectionScoped(t *testing.T) {
	loop := &eventLoop{}
	owner := currentGoroutineID()
	stream := &fdConn{commonConn: commonConn{loop: loop}}
	if stream.directOwner() {
		t.Fatal("a goroutine owns a connection whose turn it is not running")
	}
	stream.ioOwner.Store(owner)
	if !stream.directOwner() {
		t.Fatal("stream task owner cannot write its connection")
	}
	stream.ioOwner.Store(0)
	// A UDP child's turn is its server's: the server's turn runs its callbacks.
	server := &fdConn{commonConn: commonConn{loop: loop}, udp: &unixUDPState{}}
	child := &fdConn{commonConn: commonConn{loop: loop}, udp: &unixUDPState{server: server}}
	if child.directOwner() {
		t.Fatal("UDP child owned outside its server's turn")
	}
	server.ioOwner.Store(owner)
	if !child.directOwner() {
		t.Fatal("UDP child not owned by its server's turn")
	}
}

type handoffNativeExecutor struct {
	submitted atomic.Int32
	firstDone chan struct{}
}

func (executor *handoffNativeExecutor) Submit(task IOTask) bool {
	if executor.submitted.Add(1) == 1 {
		go func() {
			task.RunTask()
			close(executor.firstDone)
		}()
		return true
	}
	go task.RunTask()
	return true
}

func (executor *handoffNativeExecutor) SubmitBatch(tasks []IOTask) int {
	for _, task := range tasks {
		executor.Submit(task)
	}
	return len(tasks)
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

// newTCPTestConnection is newTestConnection over loopback TCP. A Unix stream
// socket charges a whole buffer for every send, so on Linux its default limit
// holds only a few hundred small writes; TCP coalesces them.
func newTCPTestConnection(t *testing.T, events *Events) testConnection {
	t.Helper()
	testConn, registered := startTestConnectionOn(t, events, tcpLoopbackPair(t))
	if err := <-registered; err != nil {
		t.Fatal(err)
	}
	return testConn
}

func tcpLoopbackPair(t *testing.T) [2]int {
	t.Helper()
	listener, err := unix.Socket(unix.AF_INET, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(listener)
	if err = unix.Bind(listener, &unix.SockaddrInet4{Addr: [4]byte{127, 0, 0, 1}}); err != nil {
		t.Fatal(err)
	}
	if err = unix.Listen(listener, 1); err != nil {
		t.Fatal(err)
	}
	addr, err := unix.Getsockname(listener)
	if err != nil {
		t.Fatal(err)
	}
	peer, err := unix.Socket(unix.AF_INET, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err = unix.Connect(peer, addr); err != nil {
		t.Fatal(err)
	}
	local, _, err := unix.Accept(listener)
	if err != nil {
		t.Fatal(err)
	}
	for _, fd := range []int{local, peer} {
		if err = unix.SetsockoptInt(fd, unix.IPPROTO_TCP, unix.TCP_NODELAY, 1); err != nil {
			t.Fatal(err)
		}
	}
	return [2]int{local, peer}
}

// startTestConnectionOn registers fds[0] as a connection on a fresh loop and
// leaves fds[1] to the test as its peer.
func startTestConnectionOn(t *testing.T, events *Events, fds [2]int) (testConnection, <-chan error) {
	t.Helper()
	// Configure an Events once, as Serve does: a second connection on it must
	// not resolve the callbacks again under the first one's running turns.
	if events.readBufferSize == 0 {
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
	conn.events = events
	conn.loop = loop

	startTestLoop(events, loop)
	registered := make(chan error, 1)
	go func() { registered <- events.addConn(conn) }()

	var once sync.Once
	stop := func() {
		once.Do(func() {
			if !conn.isClosing() {
				_ = conn.CloseWith(io.EOF)
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

func TestCallbacksStaySerializedAndCallbackCloseIsOrdered(t *testing.T) {
	var mu sync.Mutex
	var sequence []string
	closed := make(chan error, 1)
	cause := errors.New("callback close")
	events := &Events{Pollers: 1}
	funcs(events).Open = func(Conn) {
		mu.Lock()
		sequence = append(sequence, "open")
		mu.Unlock()
	}
	funcs(events).Read = func(conn Conn) error {
		mu.Lock()
		sequence = append(sequence, "data")
		mu.Unlock()
		_, _ = conn.Discard(-1)
		return conn.CloseWith(cause)
	}
	funcs(events).Close = func(_ Conn, err error) {
		mu.Lock()
		sequence = append(sequence, "close")
		mu.Unlock()
		closed <- err
	}
	testConn := newTestConnection(t, events)
	if _, err := unix.Write(testConn.peer, []byte("request")); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-closed:
		if !errors.Is(err, cause) {
			t.Fatalf("close error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("OnClose was not called")
	}
	mu.Lock()
	defer mu.Unlock()
	want := []string{"open", "data", "close"}
	if len(sequence) != len(want) {
		t.Fatalf("callback sequence = %v", sequence)
	}
	for i := range want {
		if sequence[i] != want[i] {
			t.Fatalf("callback sequence = %v", sequence)
		}
	}
}

func TestDialAllowedFromConnectionCallback(t *testing.T) {
	dialResult := make(chan error, 1)
	events := &Events{Pollers: 1}
	funcs(events).Read = func(conn Conn) error {
		dialed, err := events.Dial("tcp://127.0.0.1:1", nil)
		if dialed != nil {
			_ = dialed.Close()
		}
		if errors.Is(err, ErrDialOnEventLoop) {
			dialResult <- fmt.Errorf("callback Dial was rejected as event-loop work")
		} else {
			dialResult <- nil
		}
		_, _ = conn.Discard(-1)
		_, err = conn.Write([]byte("ok"))
		return err
	}
	testConn := newTestConnection(t, events)
	events.ready.Store(true)
	if _, err := unix.Write(testConn.peer, []byte("dial")); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-dialResult:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("callback Dial did not return")
	}
	if got := string(readPeer(t, testConn.peer, 2)); got != "ok" {
		t.Fatalf("same-loop response = %q", got)
	}
}

func TestConnectionTaskSlowCallbackDoesNotBlockOtherConnection(t *testing.T) {
	started := make(chan *fdConn, 1)
	release := make(chan struct{})
	fastDone := make(chan struct{}, 1)
	var slow atomic.Pointer[fdConn]
	events := &Events{Pollers: 1}
	funcs(events).Read = func(conn Conn) error {
		fdc := conn.(*fdConn)
		if slow.Load() == fdc {
			select {
			case started <- fdc:
			default:
			}
			<-release
		}
		payload := conn.PeekChunk()
		if len(payload) > 0 {
			_, _ = conn.Write(payload)
			_, _ = conn.Discard(-1)
		}
		if slow.Load() != fdc {
			select {
			case fastDone <- struct{}{}:
			default:
			}
		}
		return nil
	}

	first, firstRegistered := startTestConnection(t, events)
	second, secondRegistered := startTestConnection(t, events)
	if err := <-firstRegistered; err != nil {
		t.Fatal(err)
	}
	if err := <-secondRegistered; err != nil {
		t.Fatal(err)
	}
	slow.Store(first.conn)
	if _, err := unix.Write(first.peer, []byte("slow")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("slow callback did not start")
	}
	if _, err := unix.Write(second.peer, []byte("fast")); err != nil {
		t.Fatal(err)
	}
	if got := string(readPeer(t, second.peer, len("fast"))); got != "fast" {
		t.Fatalf("fast connection received %q", got)
	}
	select {
	case <-fastDone:
	case <-time.After(time.Second):
		t.Fatal("fast callback was blocked by slow callback")
	}
	close(release)
	if got := string(readPeer(t, first.peer, len("slow"))); got != "slow" {
		t.Fatalf("slow connection received %q", got)
	}
}

func TestConnectionTaskUsesExternalExecutor(t *testing.T) {
	events := &Events{Pollers: 1, Executor: nativeTaskExecutor{}}
	opened := make(chan struct{}, 1)
	funcs(events).Open = func(Conn) { opened <- struct{}{} }
	funcs(events).Read = func(conn Conn) error {
		data := conn.PeekChunk()
		_, err := conn.Write(data)
		_, _ = conn.Discard(-1)
		return err
	}
	testConn := newTestConnection(t, events)
	select {
	case <-opened:
	case <-time.After(time.Second):
		t.Fatal("external executor did not run connection task")
	}
	if _, err := unix.Write(testConn.peer, []byte("external")); err != nil {
		t.Fatal(err)
	}
	if got := string(readPeer(t, testConn.peer, len("external"))); got != "external" {
		t.Fatalf("external executor echo = %q", got)
	}
}

func TestConnectionTaskUsesBatchExecutorFastPath(t *testing.T) {
	executor := &batchNativeExecutor{rejectAfter: -1}
	opened := make(chan struct{}, 1)
	processed := make(chan struct{}, 1)
	events := &Events{Pollers: 1, Executor: executor}
	funcs(events).Open = func(Conn) { opened <- struct{}{} }
	funcs(events).Read = func(conn Conn) error {
		_, _ = conn.Discard(-1)
		processed <- struct{}{}
		return nil
	}
	testConn := newTestConnection(t, events)
	select {
	case <-opened:
	case <-time.After(time.Second):
		t.Fatal("batch executor did not run OnOpen")
	}
	deadline := time.Now().Add(time.Second)
	for testConn.conn.taskState.Load()&taskScheduledBit != 0 {
		if time.Now().After(deadline) {
			t.Fatal("OnOpen connection task did not finish")
		}
		runtime.Gosched()
	}
	if _, err := unix.Write(testConn.peer, []byte("batch")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-processed:
	case <-time.After(time.Second):
		t.Fatal("batch executor did not run connection task")
	}
	if calls := executor.batchCalls.Load(); calls == 0 {
		t.Fatal("readiness event did not use SubmitBatch")
	}
}

func TestBatchExecutorPartialRejection(t *testing.T) {
	executor := &batchNativeExecutor{rejectAfter: 1}
	woken := make(chan Conn, 1)
	closed := make(chan Conn, 1)
	events := withFuncs(&Events{}, FuncHandler{
		Read:  func(conn Conn) error { woken <- conn; return nil },
		Close: func(conn Conn, _ error) { closed <- conn },
	})
	pool := newIOTaskPool(executor)
	loop := &eventLoop{ioPool: pool}
	first := &fdConn{commonConn: commonConn{events: events, loop: loop}}
	// A wake turn runs a callback without touching a socket, which this
	// connection does not have.
	first.taskState.Store(ioEventWake)
	first.taskState.Or(taskScheduledBit)
	if !loop.acquireIO() {
		t.Fatal("failed to reserve first connection task")
	}
	second := &fdConn{commonConn: commonConn{events: events, loop: loop}}
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

func TestConnectionTaskHandoffPublishesNewOwnerAfterPreviousTask(t *testing.T) {
	executor := &handoffNativeExecutor{firstDone: make(chan struct{})}
	owner := make(chan bool, 1)
	events := &Events{Pollers: 1, Executor: executor}
	funcs(events).Open = func(conn Conn) {
		if err := conn.Wake(); err != nil {
			owner <- false
		}
	}
	funcs(events).Read = func(conn Conn) error {
		<-executor.firstDone
		fdc := conn.(*fdConn)
		owner <- fdc.ioOwner.Load() == currentGoroutineID()
		return nil
	}
	testConn := newTestConnection(t, events)
	select {
	case correct := <-owner:
		if !correct {
			t.Fatal("previous connection task cleared the next task owner")
		}
	case <-time.After(time.Second):
		t.Fatal("connection task handoff did not complete")
	}
	_ = testConn.conn.Close()
}

// A refused open turn closes the connection before OnOpen ever ran, so it
// gets no OnClose either — the two always come as a pair — and registration
// reports the refusal.
func TestConnectionTaskExecutorRejectionClosesConnection(t *testing.T) {
	var opened, closed atomic.Int32
	events := &Events{Pollers: 1, Executor: rejectingNativeExecutor{}}
	funcs(events).Open = func(Conn) { opened.Add(1) }
	funcs(events).Close = func(Conn, error) { closed.Add(1) }
	testConn, registered := startTestConnection(t, events)
	if err := <-registered; !errors.Is(err, ErrFirstTurnRefused) {
		t.Fatalf("registration error = %v, want %v", err, ErrFirstTurnRefused)
	}
	conn := testConn.conn
	for deadline := time.Now().Add(time.Second); conn.close.phase.Load() != closeCallbackDelivered ||
		conn.taskState.Load()&taskScheduledBit != 0; {
		if time.Now().After(deadline) {
			t.Fatalf("executor rejection did not close connection: scheduled=%v phase=%d turns=%d",
				conn.taskState.Load()&taskScheduledBit != 0, conn.close.phase.Load(), conn.loop.ioTurnCount())
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

func TestRejectedIOTaskReleasesScheduleBeforeClose(t *testing.T) {
	events := &Events{Pollers: 1}
	if err := events.initConfig(); err != nil {
		t.Fatal(err)
	}
	loop, err := newEventLoop(events)
	if err != nil {
		t.Fatal(err)
	}
	defer loop.poller.Close(nil)
	defer loop.ioPool.stop()
	conn := &fdConn{fd: -1, commonConn: commonConn{events: events, loop: loop}}
	if !loop.acquireIO() {
		t.Fatal("loop did not reserve the rejected task")
	}
	conn.taskState.Or(taskScheduledBit)
	conn.handleIOSubmitFailure(net.ErrClosed)
	if conn.taskState.Load()&taskScheduledBit != 0 {
		t.Fatal("rejected task still owned the connection after requesting close")
	}
	// The refused turn will never run, so its handler releases the
	// connection as that turn would have.
	if !conn.close.isReleased() {
		t.Fatal("rejected task did not release the connection")
	}
}

func TestRejectedTaskWaitsForPublishedCloseCause(t *testing.T) {
	closed := make(chan error, 1)
	wantErr := errors.New("close cause")
	loop := &eventLoop{ioIdle: make(chan struct{})}
	conn := &fdConn{commonConn: commonConn{
		events: withFuncs(&Events{}, FuncHandler{Close: func(_ Conn, err error) { closed <- err }}),
		loop:   loop,
	}}
	conn.close.phase.Store(closeResourcesReleased)
	conn.taskState.Store(ioEventOpen)
	conn.taskState.Or(taskScheduledBit)
	if !loop.acquireIO() {
		t.Fatal("failed to reserve earlier task")
	}
	conn.handleIOSubmitFailure(net.ErrClosed)
	select {
	case err := <-closed:
		t.Fatalf("earlier rejected task delivered OnClose before its cause: %v", err)
	default:
	}
	if phase := conn.close.phase.Load(); phase != closeResourcesReleased {
		t.Fatalf("close phase = %d before final callback", phase)
	}

	conn.submitMu.Lock()
	conn.setDeferredCloseLocked(wantErr)
	conn.submitMu.Unlock()
	conn.taskState.Store(ioEventClose)
	conn.taskState.Or(taskScheduledBit)
	loop.acquireCloseIO()
	conn.handleIOSubmitFailure(net.ErrClosed)
	select {
	case err := <-closed:
		if !errors.Is(err, wantErr) {
			t.Fatalf("OnClose error = %v, want %v", err, wantErr)
		}
	default:
		t.Fatal("final rejected task did not deliver OnClose")
	}
}

func TestExternalExecutorShutdownWaitsForConnectionTask(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	events := &Events{Pollers: 1, Executor: nativeTaskExecutor{}}
	funcs(events).Read = func(conn Conn) error {
		close(started)
		<-release
		_, _ = conn.Discard(-1)
		return nil
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
	stopped := make(chan struct{})
	go func() {
		testConn.stop()
		close(stopped)
	}()
	select {
	case <-stopped:
		t.Fatal("shutdown returned while external executor task was active")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not finish after external executor task returned")
	}
}

func TestNilShutdownCauseClosesBlockedConnectionTask(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	closed := make(chan error, 1)
	events := &Events{Pollers: 1}
	funcs(events).Read = func(conn Conn) error {
		close(started)
		<-release
		_, _ = conn.Discard(-1)
		return nil
	}
	funcs(events).Close = func(_ Conn, err error) { closed <- err }
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
	close(release)
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("OnClose error = %v, want nil", err)
		}
	case <-time.After(time.Second):
		t.Fatal("nil shutdown cause lost the deferred close")
	}
	if phase := testConn.conn.close.phase.Load(); phase != closeCallbackDelivered {
		t.Fatalf("close phase = %d, want callback delivered", phase)
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
	funcs(events).Close = func(_ Conn, err error) { closed <- err }
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

func TestShutdownWaitsForRunningTask(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	closed := make(chan struct{}, 1)
	events := &Events{Pollers: 1}
	funcs(events).Read = func(conn Conn) error {
		close(started)
		<-release
		_, _ = conn.Discard(-1)
		return nil
	}
	funcs(events).Close = func(Conn, error) { closed <- struct{}{} }
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
	for !testConn.conn.loop.ioStopped() {
		if time.Now().After(deadline) {
			close(release)
			t.Fatal("loop did not enter shutdown")
		}
		runtime.Gosched()
	}
	if testConn.conn.isReleased() {
		close(release)
		t.Fatal("loop released the fd while its connection task was active")
	}
	select {
	case <-closed:
		close(release)
		t.Fatal("OnClose overlapped the active callback")
	default:
	}
	close(release)
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("OnClose was not delivered after task completion")
	}
}

func TestDeferredTransportCauseSurvivesShutdownHandoff(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	closed := make(chan error, 1)
	wantErr := errors.New("transport failed")
	events := &Events{Pollers: 1}
	funcs(events).Read = func(conn Conn) error {
		close(started)
		<-release
		_, _ = conn.Discard(-1)
		return nil
	}
	funcs(events).Close = func(_ Conn, err error) { closed <- err }
	testConn := newTestConnection(t, events)
	if _, err := unix.Write(testConn.peer, []byte("block")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("connection task did not start")
	}
	testConn.conn.requestClose(wantErr)
	deadline := time.Now().Add(time.Second)
	for {
		testConn.conn.submitMu.Lock()
		deferred := testConn.conn.close.deferred != nil
		testConn.conn.submitMu.Unlock()
		if deferred {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("transport cause was not deferred")
		}
		time.Sleep(time.Millisecond)
	}
	stopTestLoop(testConn.conn.loop, nil)
	close(release)
	select {
	case err := <-closed:
		if !errors.Is(err, wantErr) {
			t.Fatalf("OnClose error = %v, want %v", err, wantErr)
		}
	case <-time.After(time.Second):
		t.Fatal("deferred transport cause was not delivered")
	}
}

func TestConnectionTaskStopsReadingWhileCallbackIsBlocked(t *testing.T) {
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	var calls atomic.Int32
	events := &Events{Pollers: 1, MaxBufferSize: 4}
	funcs(events).Read = func(conn Conn) error {
		if calls.Add(1) == 1 {
			close(started)
			<-release
		}
		_, _ = conn.Discard(-1)
		return nil
	}
	testConn := newTestConnection(t, events)
	if _, err := unix.Write(testConn.peer, []byte("four")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("first callback did not start")
	}
	if _, err := unix.Write(testConn.peer, []byte("second")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if got := calls.Load(); got != 1 {
		t.Fatalf("callbacks while blocked = %d, want 1", got)
	}
	close(release)
	deadline := time.Now().Add(time.Second)
	for calls.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := calls.Load(); got < 2 {
		t.Fatalf("callbacks after release = %d, want at least 2", got)
	}
}

func TestConnectionTaskRedeliversReadAfterRoundBudget(t *testing.T) {
	const total = (2 << 20) + 123
	var received atomic.Int64
	done := make(chan struct{}, 1)
	events := &Events{Pollers: 1, MaxBufferSize: 4096}
	funcs(events).Read = func(conn Conn) error {
		n := conn.InboundBuffered()
		if _, err := conn.Discard(-1); err != nil {
			return err
		}
		if received.Add(int64(n)) >= total {
			select {
			case done <- struct{}{}:
			default:
			}
		}
		return nil
	}
	testConn := newTestConnection(t, events)
	payload := make([]byte, total)
	go func() {
		for len(payload) > 0 {
			n, err := unix.Write(testConn.peer, payload)
			if n > 0 {
				payload = payload[n:]
			}
			if err != nil && !isWouldBlock(err) {
				return
			}
			if n == 0 {
				time.Sleep(time.Millisecond)
			}
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("redelivery received %d/%d bytes", received.Load(), total)
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

func TestExternalWritevCopiesOnceWithoutMutatingCallerVector(t *testing.T) {
	events := &Events{Pollers: 1}
	testConn := newTestConnection(t, events)
	first := []byte("first-")
	second := []byte("second")
	vec := [][]byte{first, second}
	if n, err := testConn.conn.Writev(vec); err != nil || n != len("first-second") {
		t.Fatalf("Writev = %d, %v", n, err)
	}
	if len(vec) != 2 || len(vec[0]) != len("first-") || len(vec[1]) != len("second") {
		t.Fatalf("Writev mutated vector: %q", vec)
	}
	copy(first, "xxxxxx")
	copy(second, "yyyyyy")
	vec[0] = nil
	if got := string(readPeer(t, testConn.peer, len("first-second"))); got != "first-second" {
		t.Fatalf("peer received %q", got)
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

func TestFlushTaskIsAWriteBarrier(t *testing.T) {
	opened := make(chan *fdConn, 1)
	releaseOpen := make(chan struct{})
	outbound := make(chan int, 4)
	events := &Events{Pollers: 1}
	funcs(events).Open = func(conn Conn) { opened <- conn.(*fdConn); <-releaseOpen }
	funcs(events).BytesWritten = func(_ Conn, written int) { outbound <- written }
	testConn, registered := startTestConnection(t, events)
	conn := <-opened
	if _, err := conn.Write([]byte("A")); err != nil {
		t.Fatal(err)
	}
	if err := conn.Flush(); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write([]byte("B")); err != nil {
		t.Fatal(err)
	}
	close(releaseOpen)
	if err := <-registered; err != nil {
		t.Fatal(err)
	}
	if got := string(readPeer(t, testConn.peer, 2)); got != "AB" {
		t.Fatalf("peer received %q", got)
	}
	if got := <-outbound; got != 2 {
		t.Fatalf("OnWrite bytes = %d, want batched 2", got)
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
	funcs(events).BytesWritten = func(_ Conn, written int) { outbound <- written }
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

func TestReadEventFlushesCallbacksAsOneBatch(t *testing.T) {
	for _, test := range []struct {
		name                string
		explicitFlush       bool
		maxOutboundBuffered int
		wantWrites          []int
	}{
		{name: "implicit", wantWrites: []int{2}},
		{name: "implicit constrained", maxOutboundBuffered: 1, wantWrites: []int{1, 1}},
		{name: "explicit", explicitFlush: true, wantWrites: []int{1, 1}},
	} {
		t.Run(test.name, func(t *testing.T) {
			outbound := make(chan int, 3)
			events := &Events{
				Pollers:                1,
				MaxBufferSize:          4,
				MaxOutboundBuffered:    test.maxOutboundBuffered,
				WriteBufferedThreshold: 16,
			}
			funcs(events).Read = func(conn Conn) error {
				if _, err := conn.Discard(-1); err != nil {
					return err
				}
				if err := conn.WriteByte('x'); err != nil {
					return err
				}
				if test.explicitFlush {
					return conn.Flush()
				}
				return nil
			}
			funcs(events).BytesWritten = func(_ Conn, written int) { outbound <- written }
			testConn := newTestConnection(t, events)
			if _, err := unix.Write(testConn.peer, []byte("12345678")); err != nil {
				t.Fatal(err)
			}
			if got := string(readPeer(t, testConn.peer, 2)); got != "xx" {
				t.Fatalf("peer received %q", got)
			}
			for _, want := range test.wantWrites {
				select {
				case got := <-outbound:
					if got != want {
						t.Fatalf("OnWrite bytes = %d, want %d", got, want)
					}
				case <-time.After(time.Second):
					t.Fatal("timed out waiting for OnWrite")
				}
			}
			select {
			case extra := <-outbound:
				t.Fatalf("unexpected extra OnWrite call for %d bytes", extra)
			default:
			}
		})
	}
}

func TestSameLoopCrossConnectionBufferedWritesFlushTarget(t *testing.T) {
	for _, test := range []struct {
		name  string
		write func(*fdConn) error
	}{
		{name: "write", write: func(conn *fdConn) error {
			_, err := conn.Write([]byte{'x'})
			return err
		}},
		{name: "writev", write: func(conn *fdConn) error {
			_, err := conn.Writev([][]byte{{'x'}})
			return err
		}},
		{name: "write-owned", write: func(conn *fdConn) error {
			buffer := AcquireBuffer(1)
			_, _ = buffer.Write([]byte{'x'})
			_, err := conn.WriteOwned(buffer)
			return err
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			events := &Events{Pollers: 1, WriteBufferedThreshold: 16}
			if err := events.initConfig(); err != nil {
				t.Fatal(err)
			}
			loop, err := newEventLoop(events)
			if err != nil {
				t.Fatal(err)
			}
			events.loops = []*eventLoop{loop}
			sourceFDs, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
			if err != nil {
				t.Fatal(err)
			}
			targetFDs, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
			if err != nil {
				_ = unix.Close(sourceFDs[0])
				_ = unix.Close(sourceFDs[1])
				t.Fatal(err)
			}
			for _, fd := range []int{sourceFDs[0], sourceFDs[1], targetFDs[0], targetFDs[1]} {
				if err := unix.SetNonblock(fd, true); err != nil {
					t.Fatal(err)
				}
			}
			source := &fdConn{fd: int32(sourceFDs[0])}
			target := &fdConn{fd: int32(targetFDs[0])}
			for _, conn := range []*fdConn{source, target} {
				conn.events = events
				conn.loop = loop
			}
			callbackErr := make(chan error, 1)
			funcs(events).Read = func(conn Conn) error {
				if conn != source {
					_, _ = conn.Discard(-1)
					return nil
				}
				_, _ = conn.Discard(-1)
				err := test.write(target)
				callbackErr <- err
				return err
			}
			startTestLoop(events, loop)
			for _, conn := range []*fdConn{source, target} {
				registered := make(chan error, 1)
				go func(conn *fdConn) { registered <- events.addConn(conn) }(conn)
				if err := <-registered; err != nil {
					t.Fatal(err)
				}
			}
			t.Cleanup(func() {
				for _, conn := range []*fdConn{source, target} {
					if !conn.isClosing() {
						_ = conn.CloseWith(io.EOF)
					}
				}
				select {
				case <-stopTestLoop(loop, nil):
				case <-time.After(2 * time.Second):
					t.Error("event loop did not stop")
				}
				_ = unix.Close(sourceFDs[1])
				_ = unix.Close(targetFDs[1])
			})
			if _, err := unix.Write(sourceFDs[1], []byte{'a'}); err != nil {
				t.Fatal(err)
			}
			if got := string(readPeer(t, targetFDs[1], 1)); got != "x" {
				t.Fatalf("target received %q, want x", got)
			}
			if err := <-callbackErr; err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestSameLoopCrossConnectionPartialWriteFlushesSuffix(t *testing.T) {
	events := &Events{Pollers: 1, MaxOutboundBuffered: 2 << 20, WriteBufferedThreshold: -1}
	if err := events.initConfig(); err != nil {
		t.Fatal(err)
	}
	loop, err := newEventLoop(events)
	if err != nil {
		t.Fatal(err)
	}
	events.loops = []*eventLoop{loop}
	sourceFDs, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	targetFDs, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, fd := range []int{sourceFDs[0], sourceFDs[1], targetFDs[0], targetFDs[1]} {
		if err := unix.SetNonblock(fd, true); err != nil {
			t.Fatal(err)
		}
	}
	if err := unix.SetsockoptInt(targetFDs[0], unix.SOL_SOCKET, unix.SO_SNDBUF, 4096); err != nil {
		t.Fatal(err)
	}
	source := &fdConn{fd: int32(sourceFDs[0]), commonConn: commonConn{events: events, loop: loop}}
	target := &fdConn{fd: int32(targetFDs[0]), commonConn: commonConn{events: events, loop: loop}}
	payload := make([]byte, 1<<20)
	for index := range payload {
		payload[index] = byte(index)
	}
	callbackErr := make(chan error, 1)
	funcs(events).Read = func(conn Conn) error {
		if conn != source {
			_, _ = conn.Discard(-1)
			return nil
		}
		_, _ = conn.Discard(-1)
		_, err := target.Write(payload)
		callbackErr <- err
		return err
	}
	startTestLoop(events, loop)
	for _, conn := range []*fdConn{source, target} {
		registered := make(chan error, 1)
		go func(conn *fdConn) { registered <- events.addConn(conn) }(conn)
		if err := <-registered; err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		for _, conn := range []*fdConn{source, target} {
			if !conn.isClosing() {
				_ = conn.CloseWith(io.EOF)
			}
		}
		select {
		case <-stopTestLoop(loop, nil):
		case <-time.After(2 * time.Second):
			t.Error("event loop did not stop")
		}
		_ = unix.Close(sourceFDs[1])
		_ = unix.Close(targetFDs[1])
	})
	if _, err := unix.Write(sourceFDs[1], []byte{'a'}); err != nil {
		t.Fatal(err)
	}
	got := readPeer(t, targetFDs[1], len(payload))
	if !bytes.Equal(got, payload) {
		t.Fatal("target did not receive the complete partial-write suffix")
	}
	if err := <-callbackErr; err != nil {
		t.Fatal(err)
	}
}

func TestConcurrentWriteAndCloseDoesNotLoseAcceptedData(t *testing.T) {
	opened := make(chan *fdConn, 1)
	releaseOpen := make(chan struct{})
	closed := make(chan error, 1)
	events := &Events{Pollers: 1, MaxOutboundBuffered: 1 << 20}
	funcs(events).Open = func(conn Conn) { opened <- conn.(*fdConn); <-releaseOpen }
	funcs(events).Close = func(_ Conn, err error) { closed <- err }
	testConn, registered := startTestConnection(t, events)
	conn := <-opened

	accepted := make(map[string]bool)
	for index := 0; index < 16; index++ {
		message := fmt.Sprintf("%08d", index)
		if _, err := conn.Write([]byte(message)); err != nil {
			t.Fatal(err)
		}
		accepted[message] = true
	}

	type writeResult struct {
		message string
		err     error
	}
	results := make(chan writeResult, 32)
	start := make(chan struct{})
	var writers sync.WaitGroup
	for index := 16; index < 48; index++ {
		writers.Add(1)
		go func(index int) {
			defer writers.Done()
			<-start
			message := fmt.Sprintf("%08d", index)
			_, err := conn.Write([]byte(message))
			results <- writeResult{message: message, err: err}
		}(index)
	}
	closeResult := make(chan error, 1)
	go func() { <-start; closeResult <- conn.CloseWith(errors.New("concurrent close")) }()
	close(start)
	writers.Wait()
	close(results)
	for result := range results {
		if result.err == nil {
			accepted[result.message] = true
		} else if !errors.Is(result.err, net.ErrClosed) {
			t.Fatalf("Write error = %v", result.err)
		}
	}
	if err := <-closeResult; err != nil && !errors.Is(err, net.ErrClosed) {
		t.Fatalf("CloseWith error = %v", err)
	}
	close(releaseOpen)
	if err := <-registered; err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-closed:
		if errors.Is(err, ErrUnflushedData) {
			t.Fatalf("small accepted writes were not flushed: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("connection did not close")
	}

	payload := readPeer(t, testConn.peer, len(accepted)*8)
	seen := make(map[string]bool, len(accepted))
	for offset := 0; offset < len(payload); offset += 8 {
		seen[string(payload[offset:offset+8])] = true
	}
	if len(seen) != len(accepted) {
		t.Fatalf("received %d unique messages, accepted %d", len(seen), len(accepted))
	}
	for message := range accepted {
		if !seen[message] {
			t.Fatalf("accepted message %q was not sent", message)
		}
	}
	if pending := conn.pending.Load(); pending != 0 {
		t.Fatalf("pending bytes after Close = %d", pending)
	}
}

func TestDeadlineClearAndExpire(t *testing.T) {
	closed := make(chan error, 1)
	events := withFuncs(&Events{Pollers: 1}, FuncHandler{Close: func(_ Conn, err error) { closed <- err }})
	testConn := newTestConnection(t, events)
	if err := testConn.conn.SetReadDeadline(time.Now().Add(20 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if err := testConn.conn.SetReadDeadline(time.Time{}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-closed:
		t.Fatalf("cleared deadline closed connection: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	if err := testConn.conn.SetReadDeadline(time.Now().Add(20 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-closed:
		if !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("deadline error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("deadline did not close connection")
	}
}

func TestUDPChildReportsUnflushedBytes(t *testing.T) {
	cause := errors.New("closed")
	closed := make(chan error, 1)
	events := withFuncs(&Events{}, FuncHandler{Close: func(_ Conn, err error) { closed <- err }})
	server := &fdConn{udp: &unixUDPState{peers: make(map[socket.UDPAddress]*fdConn)}}
	child := &fdConn{udp: &unixUDPState{server: server}}
	child.events = events
	child.remoteAddr = netip.MustParseAddrPort("127.0.0.1:1")
	child.udp.key = socket.UDPAddress{Port: 1}
	server.udp.peers[child.udp.key] = child
	child.pending.Store(7)
	finalErr, ok := child.teardown(cause)
	if !ok {
		t.Fatal("UDP child was not released")
	}
	child.deliverClose(finalErr)
	err := <-closed
	if !errors.Is(err, cause) || !errors.Is(err, ErrUnflushedData) {
		t.Fatalf("close error = %v", err)
	}
	var unflushed UnflushedError
	if !errors.As(err, &unflushed) || unflushed.Remaining != 7 {
		t.Fatalf("unflushed error = %#v", unflushed)
	}
	if len(server.udp.peers) != 0 {
		t.Fatalf("closed UDP child left %d peer entries", len(server.udp.peers))
	}
}

func TestServeDialAndShutdownLifecycle(t *testing.T) {
	started := make(chan string, 1)
	dialOpened := make(chan struct{})
	received := make(chan string, 1)
	stopped := make(chan struct{})
	var dialOpenOnce sync.Once

	events := &Events{Pollers: 2}
	funcs(events).Start = func(events *Events) {
		events.acceptor.mux.Lock()
		for _, listener := range events.acceptor.listeners {
			started <- listener.pair.local.String()
			break
		}
		events.acceptor.mux.Unlock()
	}
	funcs(events).Open = func(conn Conn) {
		if conn.Userdata() == "dial" {
			dialOpenOnce.Do(func() { close(dialOpened) })
		}
	}
	funcs(events).Read = func(conn Conn) error {
		if conn.Userdata() != "dial" {
			_, err := conn.WriteTo(conn)
			return err
		}
		data := make([]byte, conn.InboundBuffered())
		n, err := conn.Read(data)
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		received <- string(data[:n])
		return nil
	}
	funcs(events).Stop = func(*Events) { close(stopped) }

	serveDone := make(chan error, 1)
	go func() { serveDone <- events.Serve("tcp://127.0.0.1:0") }()
	var address string
	select {
	case address = <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("server did not start")
	}

	dialed, err := events.DialContext(context.Background(), "tcp://"+address, "dial")
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-dialOpened:
	case <-time.After(time.Second):
		t.Fatal("OnOpen was not delivered")
	}
	if n, err := dialed.Write([]byte("ping")); err != nil || n != 4 {
		t.Fatalf("Dial connection Write = %d, %v", n, err)
	}
	select {
	case got := <-received:
		if got != "ping" {
			t.Fatalf("received %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("echo did not arrive")
	}

	shutdownErr := errors.New("test shutdown")
	closeDone := make(chan error, 1)
	go func() { closeDone <- events.Close(shutdownErr) }()
	select {
	case err = <-closeDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Events.Close did not return")
	}
	select {
	case err = <-serveDone:
		if !errors.Is(err, shutdownErr) {
			t.Fatalf("Serve error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not return")
	}
	select {
	case <-stopped:
	default:
		t.Fatal("OnStop was not called")
	}
}

// A read round that leaves the limit's worth unconsumed holds further reads:
// the round stops, and the turn leaves read edges alone until a callback
// consumes below the limit.
func TestInboundLimitHoldsTheReadRound(t *testing.T) {
	var calls atomic.Int32
	events := withFuncs(&Events{
		Pollers:            1,
		MaxBufferSize:      4,
		MaxInboundBuffered: 8,
	}, FuncHandler{
		Read: func(Conn) error { calls.Add(1); return nil },
	})
	testConn := newTestConnection(t, events)
	if _, err := unix.Write(testConn.peer, []byte("0123456789abcdef")); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for !testConn.conn.readHeld() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !testConn.conn.readHeld() {
		t.Fatal("reads were not held at the limit")
	}
	time.Sleep(50 * time.Millisecond)
	if got := calls.Load(); got != 2 {
		t.Fatalf("OnData ran %d times, want 2: the read held at 8 buffered bytes", got)
	}
	if testConn.conn.isClosing() {
		t.Fatal("the connection closed at the limit")
	}
}

// A held read stays held whatever event reaches the turn: a writable edge
// that carries read readiness, as epoll reports one, or a hangup, must not
// restart it.
func TestInboundLimitHeldReadIgnoresEdges(t *testing.T) {
	var calls atomic.Int32
	events := withFuncs(&Events{
		Pollers:            1,
		MaxBufferSize:      4,
		MaxInboundBuffered: 8,
	}, FuncHandler{
		Read: func(Conn) error { calls.Add(1); return nil },
	})
	testConn := newTestConnection(t, events)
	if _, err := unix.Write(testConn.peer, []byte("0123456789abcdef")); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for !testConn.conn.readHeld() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !testConn.conn.readHeld() {
		t.Fatal("reads were not held at the limit")
	}
	held := calls.Load()
	for _, events := range []uint32{ioEventRead | ioEventWrite, ioEventRead | ioEventHangup, ioEventRead} {
		testConn.conn.scheduleIO(events)
		time.Sleep(20 * time.Millisecond)
		if got := calls.Load(); got != held {
			t.Fatalf("events %#x restarted a held read: OnData ran %d more times", events, got-held)
		}
	}
}

func TestEventsCloseFromCallbackDoesNotDeadlock(t *testing.T) {
	started := make(chan string, 1)
	closeReturned := make(chan struct{})
	shutdownErr := errors.New("callback shutdown")
	events := &Events{Pollers: 1}
	funcs(events).Start = func(events *Events) {
		events.acceptor.mux.Lock()
		for _, listener := range events.acceptor.listeners {
			started <- listener.pair.local.String()
			break
		}
		events.acceptor.mux.Unlock()
	}
	funcs(events).Read = func(conn Conn) error {
		_, _ = conn.Discard(-1)
		if err := events.Close(shutdownErr); err != nil {
			return err
		}
		close(closeReturned)
		return nil
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- events.Serve("tcp://127.0.0.1:0") }()
	address := <-started
	client, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if _, err = client.Write([]byte("stop")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-closeReturned:
	case <-time.After(time.Second):
		t.Fatal("Events.Close blocked inside OnData")
	}
	select {
	case err = <-serveDone:
		if !errors.Is(err, shutdownErr) {
			t.Fatalf("Serve error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not stop")
	}
}

func TestListenerInitializationFailureRollsBackLoops(t *testing.T) {
	events := &Events{Pollers: 1}
	done := make(chan error, 1)
	go func() { done <- events.Serve("unsupported://address") }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Serve succeeded with unsupported protocol")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("initialization rollback did not finish")
	}
}

func TestUDPChildCloseDoesNotCloseSharedServer(t *testing.T) {
	started := make(chan string, 1)
	childClosed := make(chan struct{}, 2)
	events := &Events{Pollers: 1}
	funcs(events).Start = func(events *Events) {
		events.acceptor.mux.Lock()
		for _, listener := range events.acceptor.listeners {
			started <- listener.pair.local.String()
			break
		}
		events.acceptor.mux.Unlock()
	}
	funcs(events).Read = func(conn Conn) error {
		data := make([]byte, conn.InboundBuffered())
		n, _ := conn.Read(data)
		if string(data[:n]) == "close" {
			return conn.Close()
		}
		_, err := conn.Write(data[:n])
		return err
	}
	funcs(events).Close = func(Conn, error) { childClosed <- struct{}{} }
	serveDone := make(chan error, 1)
	go func() { serveDone <- events.Serve("udp://127.0.0.1:0") }()
	address := <-started
	serverAddr, err := net.ResolveUDPAddr("udp", address)
	if err != nil {
		t.Fatal(err)
	}
	first, err := net.DialUDP("udp", nil, serverAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := net.DialUDP("udp", nil, serverAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if _, err = first.Write([]byte("close")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-childClosed:
	case <-time.After(time.Second):
		t.Fatal("first UDP child did not close")
	}
	if _, err = second.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	if err = second.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	buffer := make([]byte, 16)
	n, err := second.Read(buffer)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(buffer[:n]); got != "ping" {
		t.Fatalf("UDP echo = %q", got)
	}

	shutdownErr := errors.New("udp shutdown")
	if err = events.Close(shutdownErr); err != nil {
		t.Fatal(err)
	}
	if err = <-serveDone; !errors.Is(err, shutdownErr) {
		t.Fatalf("Serve error = %v", err)
	}
	select {
	case <-childClosed:
	case <-time.After(time.Second):
		t.Fatal("remaining UDP child did not close on shutdown")
	}
}

func TestReadRoundCorksRepliesToOneFlush(t *testing.T) {
	events := &Events{Pollers: 1, MaxBufferSize: 4}
	outbound := make(chan int, 8)
	funcs(events).Read = func(conn Conn) error {
		data := conn.PeekChunk()
		if _, err := conn.Write(data); err != nil {
			return err
		}
		_, err := conn.Discard(-1)
		return err
	}
	funcs(events).BytesWritten = func(_ Conn, written int) { outbound <- written }
	testConn := newTestConnection(t, events)
	if _, err := unix.Write(testConn.peer, []byte("12345678")); err != nil {
		t.Fatal(err)
	}
	if got := string(readPeer(t, testConn.peer, 8)); got != "12345678" {
		t.Fatalf("peer received %q", got)
	}
	// Eight bytes arrive as two 4-byte reads; both replies must leave the
	// socket in one writev, not one syscall per message.
	select {
	case written := <-outbound:
		if written != 8 {
			t.Fatalf("first OnWrite = %d bytes, want the corked 8", written)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for the corked flush")
	}
	select {
	case extra := <-outbound:
		t.Fatalf("corked round produced an extra %d-byte write", extra)
	default:
	}
}

func newPartialStreamWriter(t *testing.T) (writer, reader int) {
	t.Helper()
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err = unix.SetNonblock(fds[0], true); err != nil {
		_ = unix.Close(fds[0])
		_ = unix.Close(fds[1])
		t.Fatal(err)
	}
	if err = unix.SetNonblock(fds[1], true); err != nil {
		_ = unix.Close(fds[0])
		_ = unix.Close(fds[1])
		t.Fatal(err)
	}
	if err = unix.SetsockoptInt(fds[0], unix.SOL_SOCKET, unix.SO_SNDBUF, 4096); err != nil {
		_ = unix.Close(fds[0])
		_ = unix.Close(fds[1])
		t.Fatal(err)
	}
	return fds[0], fds[1]
}

func readStreamToEOF(t *testing.T, fd int) []byte {
	t.Helper()
	var result []byte
	buffer := make([]byte, 64*1024)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		n, err := unix.Read(fd, buffer)
		if n > 0 {
			result = append(result, buffer[:n]...)
		}
		if n == 0 && err == nil {
			return result
		}
		if err == nil {
			continue
		}
		if isWouldBlock(err) {
			time.Sleep(time.Millisecond)
			continue
		}
		t.Fatal(err)
	}
	t.Fatal("stream did not reach EOF")
	return nil
}

func TestUDPPeerKey(t *testing.T) {
	key4 := socket.UDPAddress{Family: 4, Port: 1234}
	copy(key4.Addr[:4], []byte{127, 0, 0, 1})
	if duplicate := key4; duplicate != key4 {
		t.Fatalf("duplicate IPv4 key = %#v", duplicate)
	}
	key6 := socket.UDPAddress{Family: 6, Port: 1234, Zone: 2}
	key6.Addr[15] = 1
	if key6 == key4 {
		t.Fatalf("IPv6 key = %#v matched IPv4", key6)
	}
	otherZone := key6
	otherZone.Zone++
	if otherZone == key6 {
		t.Fatal("IPv6 keys ignored zone")
	}
}

func TestExistingUDPPeerPacketDoesNotAllocate(t *testing.T) {
	var opened, packets int
	events := withFuncs(&Events{}, FuncHandler{
		Open: func(Conn) { opened++ },
		Read: func(Conn) error {
			packets++
			return nil
		},
	})
	loop := &eventLoop{}
	server := &fdConn{fd: -1, udp: &unixUDPState{peers: make(map[socket.UDPAddress]*fdConn)}}
	server.events, server.loop = events, loop
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
	events := withFuncs(&Events{
		MaxBufferSize: 64,
	}, FuncHandler{
		Read: func(Conn) error {
			packets++
			return nil
		},
	})
	if err := events.initConfig(); err != nil {
		t.Fatal(err)
	}
	loop := &eventLoop{}
	server := &fdConn{
		fd: int32(receiver), udp: &unixUDPState{peers: make(map[socket.UDPAddress]*fdConn)},
	}
	server.events, server.loop = events, loop
	server.ioOwner.Store(currentGoroutineID())
	payload := []byte{'x'}
	sendAndReceive := func() {
		before := packets
		if err := unix.Sendto(sender, payload, 0, target); err != nil {
			panic(err)
		}
		// Bounded by time, not by polls: on a loaded host a loopback packet
		// can take longer than any fixed number of yields.
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
	sendAndReceive() // create the peer before measuring the steady path
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
	events := withFuncs(&Events{
		MaxBufferSize: 64,
	}, FuncHandler{
		Read: func(Conn) error {
			packets++
			return nil
		},
	})
	if err := events.initConfig(); err != nil {
		t.Fatal(err)
	}
	loop := &eventLoop{}
	conn := &fdConn{fd: int32(receiver), udp: &unixUDPState{}}
	conn.events, conn.loop = events, loop
	conn.ioOwner.Store(currentGoroutineID())
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
	direct.events = &Events{}
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
	conn.events = &Events{}
	conn.loop = loop
	for name, set := range map[string]func() error{
		"linger":            func() error { return conn.SetLinger(0) },
		"no delay":          func() error { return conn.SetNoDelay(false) },
		"read buffer":       func() error { return conn.SetReadBuffer(4096) },
		"write buffer":      func() error { return conn.SetWriteBuffer(4096) },
		"keep alive":        func() error { return conn.SetKeepAlive(true) },
		"keep alive period": func() error { return conn.SetKeepAlivePeriod(1) },
	} {
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

	readFile, writeFile, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer writeFile.Close()
	conn = &fdConn{udp: &unixUDPState{file: readFile}}
	conn.closeUnregistered()
	if _, err := readFile.Stat(); err == nil {
		t.Fatal("owned file remained open")
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
	conn.ioOwner.Store(currentGoroutineID())
	conn.events = &Events{WriteBufferedThreshold: 16, MaxOutboundBuffered: 8}
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
	conn.events.MaxOutboundBuffered = 1
	if n, err := conn.Writev([][]byte{[]byte("a"), []byte("b")}); n != 0 || !errors.Is(err, ErrOutboundOverflow) {
		t.Fatalf("overflow Writev = %d, %v", n, err)
	}
	if n, err := conn.Write([]byte("ab")); n != 0 || !errors.Is(err, ErrOutboundOverflow) {
		t.Fatalf("overflow Write = %d, %v", n, err)
	}
	conn.events.MaxOutboundBuffered = 8
	conn.setWriteBlocked(true)
	if err := conn.fireWriteEvent(); err != nil || conn.writeBlocked() {
		t.Fatalf("fireWriteEvent = %v, blocked %v", err, conn.writeBlocked())
	}
	conn.udp = &unixUDPState{}
	if n, err := conn.Writev([][]byte{[]byte("x")}); n != 0 || !errors.Is(err, errUnsupported) {
		t.Fatalf("UDP Writev = %d, %v", n, err)
	}
	if err := conn.fireWriteEvent(); err != nil {
		t.Fatalf("UDP fireWriteEvent = %v", err)
	}
}

func TestLoopWriteOwnedTransfersBufferWithoutCopy(t *testing.T) {
	loop := &eventLoop{}
	conn := &fdConn{fd: -1}
	conn.ioOwner.Store(currentGoroutineID())
	conn.events = &Events{WriteBufferedThreshold: 16, MaxOutboundBuffered: 16}
	conn.loop = loop

	buffer := AcquireBuffer(8)
	dst := buffer.AvailableBuffer()[:8]
	encoded := &dst[0]
	buffer.CommitWrite(copy(dst, "payload"))
	n, err := conn.WriteOwned(buffer)
	if err != nil || n != 7 {
		t.Fatalf("WriteOwned = %d, %v", n, err)
	}
	peeked := conn.outbound.Peek(make([]byte, 7))
	if string(peeked) != "payload" || &peeked[0] != encoded {
		t.Fatal("owned write was copied before entering outbound")
	}
	if conn.OutboundBuffered() != 7 {
		t.Fatalf("buffered bytes = %d, want 7", conn.OutboundBuffered())
	}
	conn.outbound.Reset()
	conn.pending.Store(0)
}

func TestCorkedOwnedWritesCoalesceInRound(t *testing.T) {
	loop := &eventLoop{}
	events := &Events{MaxOutboundBuffered: 64 << 10, readBufferSize: 16 << 10}
	// The round's first write opens the queue on its own; the writes that
	// follow coalesce into one more block, so the round's final flush hands
	// the peer two segments in one call.
	conn := &fdConn{fd: -1, turn: turnCorked}
	conn.ioOwner.Store(currentGoroutineID())
	conn.events = events
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
	conn.ioOwner.Store(currentGoroutineID())
	conn.events = &Events{}
	conn.loop = loop
	if n, err := conn.Write([]byte("x")); n != 0 || err == nil {
		t.Fatalf("invalid-fd Write = %d, %v", n, err)
	}
	if !conn.isClosing() {
		t.Fatal("write failure did not request close")
	}

	conn = &fdConn{fd: -1}
	conn.ioOwner.Store(currentGoroutineID())
	conn.events = &Events{}
	conn.loop = loop
	if n, err := conn.Writev([][]byte{[]byte("x")}); n != 0 || err == nil {
		t.Fatalf("invalid-fd Writev = %d, %v", n, err)
	}
	if !conn.isClosing() {
		t.Fatal("writev failure did not request close")
	}

	conn = &fdConn{fd: -1}
	conn.events = &Events{}
	_, _ = conn.outbound.WriteString("x")
	conn.pending.Store(1)
	if n, err := conn.flushOnLoop(); n != 0 || err == nil {
		t.Fatalf("invalid-fd flush = %d, %v", n, err)
	}

	conn = &fdConn{}
	conn.events = &Events{MaxOutboundBuffered: 1}
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
	}{
		{
			name:  "Write/payload_limit",
			setup: func(conn *fdConn) { conn.pending.Store(1) },
			write: func(conn *fdConn) (int, error) { return conn.Write(payload) },
			want:  ErrOutboundOverflow,
		},
		{
			name:  "Writev/payload_limit",
			setup: func(conn *fdConn) { conn.pending.Store(1) },
			write: func(conn *fdConn) (int, error) { return conn.Writev(vec) },
			want:  ErrOutboundOverflow,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			events := &Events{MaxOutboundBuffered: len(payload)}
			conn := &fdConn{}
			conn.events = events
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
	events := withFuncs(&Events{Pollers: 1}, FuncHandler{Close: func(_ Conn, err error) { closed <- err }})
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
	events := &Events{Pollers: 1, MaxBufferSize: 64}
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
	conn.events = events
	conn.inboundTail = []byte("discard")
	if err := events.onRead(conn); err != nil || conn.InboundBuffered() != 0 {
		t.Fatalf("default OnData = %v, buffered %d", err, conn.InboundBuffered())
	}
	wantErr := errors.New("handler")
	funcs(events).Read = func(Conn) error { return wantErr }
	if err := events.onRead(conn); !errors.Is(err, wantErr) {
		t.Fatalf("OnData error = %v", err)
	}
	var inbound, outbound int
	funcs(events).BytesRead = func(Conn, int) { inbound++ }
	funcs(events).BytesWritten = func(Conn, int) { outbound++ }
	events.onSocketBytesRead(conn, 0)
	events.onSocketBytesRead(conn, 1)
	events.onSocketBytesWrite(conn, 0)
	events.onSocketBytesWrite(conn, 1)
	if inbound != 1 || outbound != 1 {
		t.Fatalf("byte callbacks = %d, %d", inbound, outbound)
	}

	missing := &fdConn{fd: -1}
	missing.events = events
	if err := events.addConn(missing); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("addConn without loop error = %v", err)
	}
	rejected := &fdConn{fd: -1}
	rejected.events = events
	if rejected.admitAccepted(false) {
		t.Fatal("admitAccepted accepted a connection without a loop")
	}

	// A listener's accept error closes the Events.
	badEvents := &Events{}
	bad := &eventLoop{events: badEvents, fdMap: newFdMap()}
	bad.listeners.Store(&[]*listener{{fd: -1}})
	bad.dispatch(&loopWaiter{}, []poller.Event{{FD: -1, Events: poller.ReadEvents}})
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
	events := &Events{Pollers: 1, MaxBufferSize: 64}
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
	conn.events = events
	conn.loop = loop
	if err := loop.poller.Add(int(conn.fd), poller.Readable); err != nil {
		t.Fatal(err)
	}
	// An event for a descriptor no table holds is dropped.
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
	failedConn.events = events
	failedConn.loop = failedLoop
	if err := failedLoop.registerConn(failedConn); err == nil {
		t.Fatal("registerConn accepted a closed descriptor")
	}
}

func TestUnixDialFailureAndResourceCleanup(t *testing.T) {
	events := &Events{}
	if _, err := events.Dial("tcp://127.0.0.1:1", nil); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("unready Dial error = %v", err)
	}
	events.ready.Store(true)
	dialCtx, cancelDial := context.WithCancel(context.Background())
	cancelDial()
	if _, err := events.DialContext(dialCtx, "tcp://127.0.0.1:1", nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled DialContext error = %v", err)
	}
	for _, address := range []string{"%", "unsupported://address", "tcp://127.0.0.1:0"} {
		if conn, err := events.Dial(address, nil); err == nil {
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
	if conn, err := events.Dial("tcp://"+listener.Addr().String(), "context"); conn != nil || !errors.Is(err, net.ErrClosed) {
		t.Fatalf("Dial without a worker = %v, %v", conn, err)
	}
	if err := <-accepted; err != nil {
		t.Fatal(err)
	}

	events.closing.Store(true)
	if _, err := events.Dial("tcp://127.0.0.1:1", nil); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("closing Dial error = %v", err)
	}
}

func TestDialBeforeReadyDoesNotReadLoopState(t *testing.T) {
	events := &Events{}
	firstLoops := []*eventLoop{{}}
	secondLoops := []*eventLoop{{}}
	started := make(chan struct{})
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		close(started)
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
				events.loops = firstLoops
				events.loops = secondLoops
				runtime.Gosched()
			}
		}
	}()
	<-started
	for range 10000 {
		if _, err := events.Dial("tcp://127.0.0.1:1", nil); !errors.Is(err, net.ErrClosed) {
			close(stop)
			<-done
			t.Fatalf("Dial before ready error = %v", err)
		}
		runtime.Gosched()
	}
	close(stop)
	<-done
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
	conn.events = events
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
	conn.events = events
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

	events := &Events{MaxBufferSize: 64}
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
	conn.events, conn.loop = events, loop
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
	conn.events = events
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
	funcs(events).Read = func(Conn) error { return wantErr }
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
	rejectedClose.events = events
	rejectedClose.loop = stopped
	rejectedClose.requestClose(wantErr)
	if rejectedClose.close.deferred == nil || !errors.Is(rejectedClose.close.deferred.cause, wantErr) {
		t.Fatalf("deferred close error = %v", rejectedClose.close.deferred)
	}

	rejectedTimeout := &fdConn{fd: -1}
	rejectedTimeout.events = events
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
	// External submission path: the outbound limit is full, so nothing is
	// accepted and the buffer must remain the caller's.
	conn := &fdConn{}
	conn.events = &Events{MaxOutboundBuffered: 8}
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

	// Task-owner path with batching: the same contract.
	loop := &eventLoop{}
	owner := &fdConn{fd: -1}
	owner.ioOwner.Store(currentGoroutineID())
	owner.events = &Events{WriteBufferedThreshold: 16, MaxOutboundBuffered: 8}
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

// fillStreamSendBuffer leaves the stream socket refusing further writes.
func fillStreamSendBuffer(t *testing.T, fd int) {
	t.Helper()
	if err := unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_SNDBUF, 4096); err != nil {
		t.Fatal(err)
	}
	if err := unix.SetNonblock(fd, true); err != nil {
		t.Fatal(err)
	}
	payload := make([]byte, 4096)
	for range 1 << 16 {
		n, err := unix.Write(fd, payload)
		if err == unix.EAGAIN {
			return
		}
		if err != nil {
			t.Fatalf("fill stream socket = %d, %v", n, err)
		}
	}
	t.Fatal("stream socket did not reach EAGAIN")
}

func TestWriteOwnedBlockedSocketOverflowReturnsOwnership(t *testing.T) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fds[0])
	defer unix.Close(fds[1])
	fillStreamSendBuffer(t, fds[0])

	loop := &eventLoop{}
	conn := &fdConn{fd: int32(fds[0])}
	conn.ioOwner.Store(currentGoroutineID())
	conn.events = &Events{WriteBufferedThreshold: 0, MaxOutboundBuffered: 4}
	conn.loop = loop

	buffer := AcquireBuffer(8)
	buffer.CommitWrite(copy(buffer.AvailableBuffer()[:7], "payload"))
	n, err := conn.WriteOwned(buffer)
	if n != 0 || !errors.Is(err, ErrOutboundOverflow) {
		t.Fatalf("WriteOwned = %d, %v", n, err)
	}
	// Nothing reached the socket and nothing entered the queue: the caller
	// keeps ownership, exactly like every other ErrOutboundOverflow.
	if buffer.Len() != 7 || string(buffer.Bytes()) != "payload" {
		t.Fatal("overflowed buffer was consumed")
	}
	if conn.isClosing() {
		t.Fatal("pure overflow closed the stream")
	}
	ReleaseBuffer(buffer)
}

func TestWriteOwnedPartialWriteDespiteLimitReportsShortWrite(t *testing.T) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fds[0])
	defer unix.Close(fds[1])
	// AF_UNIX flow control charges the sender until the peer reads, and it
	// only becomes predictable when both queues are small.
	if err := unix.SetsockoptInt(fds[1], unix.SOL_SOCKET, unix.SO_RCVBUF, 8192); err != nil {
		t.Fatal(err)
	}
	fillStreamSendBuffer(t, fds[0])
	// Drain one whole fill chunk: Linux frees queue slots per skb, so a
	// partial read would not admit new bytes.
	readPeer(t, fds[1], 4096)

	// The close request must not need a running poller: the turn that made
	// the write releases the connection as it ends.
	loop := &eventLoop{}
	conn := &fdConn{fd: int32(fds[0])}
	conn.ioOwner.Store(currentGoroutineID())
	conn.events = &Events{WriteBufferedThreshold: 0, MaxOutboundBuffered: 4}
	conn.loop = loop

	buffer := AcquireBuffer(8192)
	buffer.CommitWrite(copy(buffer.AvailableBuffer()[:8192], make([]byte, 8192)))
	n, err := conn.WriteOwned(buffer)
	if n <= 0 || n >= 8192 {
		t.Fatalf("WriteOwned wrote %d of 8192, want a partial write", n)
	}
	if errors.Is(err, ErrOutboundOverflow) || !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("partial write error = %v, want io.ErrShortWrite", err)
	}
	if !conn.isClosing() {
		t.Fatal("broken partial frame did not close the stream")
	}
}

// The []IOTask readiness scratch is only worth its allocation for an
// injected Executor: the owned typed queue takes the connections directly.
func TestExecutorTaskScratchIsAllocatedOnlyForAnInjectedExecutor(t *testing.T) {
	owned := &Events{Pollers: 1}
	if err := owned.initConfig(); err != nil {
		t.Fatal(err)
	}
	loop, err := newEventLoop(owned)
	if err != nil {
		t.Fatal(err)
	}
	defer loop.poller.Close(nil)
	if loop.newWaiter().args != nil {
		t.Fatal("owned scheduler allocated the []IOTask scratch")
	}

	injected := &Events{Pollers: 1, Executor: &recoveringExecutor{}}
	if err := injected.initConfig(); err != nil {
		t.Fatal(err)
	}
	loop, err = newEventLoop(injected)
	if err != nil {
		t.Fatal(err)
	}
	defer loop.poller.Close(nil)
	if args := loop.newWaiter().args; len(args) != 0 || cap(args) != eventBatch {
		t.Fatalf("injected executor scratch = len %d cap %d, want 0/%d",
			len(args), cap(args), eventBatch)
	}
}
