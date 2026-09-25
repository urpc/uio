//go:build (linux || darwin || netbsd || freebsd || openbsd || dragonfly) && !stdio

package uio

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urpc/uio/internal/bytebuf"
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
	loop.loopGoid.Store(owner)
	stream := &fdConn{commonConn: commonConn{loop: loop}}
	if stream.directOwner() {
		t.Fatal("loop owner incorrectly owns a stream connection task")
	}
	datagram := &fdConn{commonConn: commonConn{loop: loop}, udp: &unixUDPState{}}
	if !datagram.directOwner() {
		t.Fatal("loop owner cannot write its datagram")
	}
	stream.ioOwner.Store(owner)
	if !stream.directOwner() {
		t.Fatal("stream task owner cannot write its connection")
	}
	stream.ioOwner.Store(0)
	loop.loopGoid.Store(0)
	if datagram.directOwner() {
		t.Fatal("datagram remained loop-owned after loop exit")
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
	if err := events.initConfig(); err != nil {
		t.Fatal(err)
	}
	loop, err := newEventLoop(events)
	if err != nil {
		t.Fatal(err)
	}
	events.workers = []*eventLoop{loop}
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err = unix.SetNonblock(fds[0], true); err != nil {
		t.Fatal(err)
	}
	if err = unix.SetNonblock(fds[1], true); err != nil {
		t.Fatal(err)
	}
	conn := &fdConn{fd: fds[0]}
	conn.events = events
	conn.loop = loop

	done := make(chan error, 1)
	go func() { done <- loop.Serve(false, nil) }()
	registered := make(chan error, 1)
	go func() { registered <- events.addConn(conn) }()

	var once sync.Once
	stop := func() {
		once.Do(func() {
			if !conn.isClosing() {
				_ = conn.CloseWith(io.EOF)
			}
			loop.beginStop(nil)
			select {
			case <-done:
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
	events.OnOpen = func(Conn) {
		mu.Lock()
		sequence = append(sequence, "open")
		mu.Unlock()
	}
	events.OnData = func(conn Conn) error {
		mu.Lock()
		sequence = append(sequence, "data")
		mu.Unlock()
		_, _ = conn.Discard(-1)
		return conn.CloseWith(cause)
	}
	events.OnClose = func(_ Conn, err error) {
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
	events.OnData = func(conn Conn) error {
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
	events.OnData = func(conn Conn) error {
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
	events.OnOpen = func(Conn) { opened <- struct{}{} }
	events.OnData = func(conn Conn) error {
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
	events.OnOpen = func(Conn) { opened <- struct{}{} }
	events.OnData = func(conn Conn) error {
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
	for testConn.conn.scheduled.Load() {
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
	opened := make(chan Conn, 1)
	closed := make(chan Conn, 1)
	events := &Events{
		OnOpen:  func(conn Conn) { opened <- conn },
		OnClose: func(conn Conn, _ error) { closed <- conn },
	}
	pool := newIOTaskPool(executor)
	loop := &eventLoop{ioPool: pool}
	first := &fdConn{commonConn: commonConn{events: events, loop: loop}}
	first.pendingEvents.Store(ioEventOpen)
	first.scheduled.Store(true)
	if !loop.acquireIO() {
		t.Fatal("failed to reserve first connection task")
	}
	second := &fdConn{commonConn: commonConn{events: events, loop: loop}}
	second.close.phase.Store(closeResourcesReleased)
	second.pendingEvents.Store(ioEventClose)
	second.scheduled.Store(true)
	if !loop.acquireIO() {
		t.Fatal("failed to reserve second connection task")
	}

	if !pool.submitBatch([]IOTask{first, second}) {
		t.Fatal("batch submission was rejected by the I/O pool")
	}
	select {
	case conn := <-opened:
		if conn != first {
			t.Fatalf("opened connection = %p, want %p", conn, first)
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
	events.OnOpen = func(conn Conn) {
		if err := conn.Wake(); err != nil {
			owner <- false
		}
	}
	events.OnData = func(conn Conn) error {
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

func TestConnectionTaskExecutorRejectionClosesConnection(t *testing.T) {
	closed := make(chan error, 1)
	events := &Events{Pollers: 1, Executor: rejectingNativeExecutor{}}
	events.OnClose = func(_ Conn, err error) { closed <- err }
	testConn, registered := startTestConnection(t, events)
	if err := <-registered; err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-closed:
		if !errors.Is(err, net.ErrClosed) {
			t.Fatalf("executor rejection close error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatalf("executor rejection did not close connection: scheduled=%v phase=%d loopState=%d queued=%v stopped=%v",
			testConn.conn.scheduled.Load(), testConn.conn.close.phase.Load(),
			testConn.conn.loop.ioState.Load(), testConn.conn.loop.hasPendingTasks(), testConn.conn.loop.stopping.Load())
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
	conn.scheduled.Store(true)
	conn.handleIOSubmitFailure(net.ErrClosed)
	if conn.scheduled.Load() {
		t.Fatal("rejected task still owned the connection after requesting close")
	}
	batch := loop.tasks.Drain()
	if batch == nil || batch.Value.kind != closeTask || batch.Value.conn != conn {
		t.Fatal("rejected task did not enqueue the connection close")
	}
	releaseTask(batch.Value)
}

func TestRejectedTaskWaitsForPublishedCloseCause(t *testing.T) {
	closed := make(chan error, 1)
	wantErr := errors.New("close cause")
	loop := &eventLoop{ioIdle: make(chan struct{})}
	conn := &fdConn{commonConn: commonConn{
		events: &Events{OnClose: func(_ Conn, err error) { closed <- err }},
		loop:   loop,
	}}
	conn.close.phase.Store(closeResourcesReleased)
	conn.pendingEvents.Store(ioEventOpen)
	conn.scheduled.Store(true)
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
	conn.pendingEvents.Store(ioEventClose)
	conn.scheduled.Store(true)
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
	events.OnData = func(conn Conn) error {
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
	events.OnData = func(conn Conn) error {
		close(started)
		<-release
		_, _ = conn.Discard(-1)
		return nil
	}
	events.OnClose = func(_ Conn, err error) { closed <- err }
	testConn := newTestConnection(t, events)
	if _, err := unix.Write(testConn.peer, []byte("block")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("connection task did not start")
	}
	testConn.conn.loop.beginStop(nil)
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
	events.OnData = func(conn Conn) error {
		close(started)
		<-release
		_, _ = conn.Discard(-1)
		return wantErr
	}
	events.OnClose = func(_ Conn, err error) { closed <- err }
	testConn := newTestConnection(t, events)
	if _, err := unix.Write(testConn.peer, []byte("block")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("connection task did not start")
	}
	testConn.conn.loop.beginStop(nil)
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

func TestShutdownWaitsForTaskBehindQueuedRefresh(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	closed := make(chan struct{}, 1)
	events := &Events{Pollers: 1}
	events.OnData = func(conn Conn) error {
		close(started)
		<-release
		_, _ = conn.Discard(-1)
		return nil
	}
	events.OnClose = func(Conn, error) { closed <- struct{}{} }
	testConn := newTestConnection(t, events)
	if _, err := unix.Write(testConn.peer, []byte("block")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("connection task did not start")
	}
	refresh := acquireTask(refreshTask, testConn.conn)
	if !testConn.conn.loop.submitTask(refresh) {
		releaseTask(refresh)
		close(release)
		t.Fatal("refresh task was rejected before shutdown")
	}
	testConn.conn.loop.beginStop(nil)
	deadline := time.Now().Add(time.Second)
	for !testConn.conn.loop.ioStopped() {
		if time.Now().After(deadline) {
			close(release)
			t.Fatal("loop did not enter shutdown")
		}
		runtime.Gosched()
	}
	if testConn.conn.isClosedOnLoop() {
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
	events.OnData = func(conn Conn) error {
		close(started)
		<-release
		_, _ = conn.Discard(-1)
		return nil
	}
	events.OnClose = func(_ Conn, err error) { closed <- err }
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
	testConn.conn.loop.beginStop(nil)
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
	events.OnData = func(conn Conn) error {
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
	events.OnData = func(conn Conn) error {
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
	events.OnData = func(conn Conn) error {
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
	events.OnOpen = func(conn Conn) {
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
	events.OnOpen = func(conn Conn) { opened <- conn.(*fdConn); <-releaseOpen }
	events.OnOutbound = func(_ Conn, written int) { outbound <- written }
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
		t.Fatalf("OnOutbound bytes = %d, want batched 2", got)
	}
}

func TestCallbackThresholdBatchesWithoutTasks(t *testing.T) {
	outbound := make(chan int, 2)
	events := &Events{Pollers: 1, WriteBufferedThreshold: 16}
	events.OnOpen = func(conn Conn) {
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
	events.OnOutbound = func(_ Conn, written int) { outbound <- written }
	testConn := newTestConnection(t, events)
	if got := string(readPeer(t, testConn.peer, 3)); got != "abc" {
		t.Fatalf("peer received %q", got)
	}
	if written := <-outbound; written != 3 {
		t.Fatalf("OnOutbound bytes = %d", written)
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
			events.OnData = func(conn Conn) error {
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
			events.OnOutbound = func(_ Conn, written int) { outbound <- written }
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
						t.Fatalf("OnOutbound bytes = %d, want %d", got, want)
					}
				case <-time.After(time.Second):
					t.Fatal("timed out waiting for OnOutbound")
				}
			}
			select {
			case extra := <-outbound:
				t.Fatalf("unexpected extra OnOutbound call for %d bytes", extra)
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
			events.workers = []*eventLoop{loop}
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
			source := &fdConn{fd: sourceFDs[0]}
			target := &fdConn{fd: targetFDs[0]}
			for _, conn := range []*fdConn{source, target} {
				conn.events = events
				conn.loop = loop
			}
			callbackErr := make(chan error, 1)
			events.OnData = func(conn Conn) error {
				if conn != source {
					_, _ = conn.Discard(-1)
					return nil
				}
				_, _ = conn.Discard(-1)
				err := test.write(target)
				callbackErr <- err
				return err
			}
			done := make(chan error, 1)
			go func() { done <- loop.Serve(false, nil) }()
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
				loop.beginStop(nil)
				select {
				case <-done:
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
	events.workers = []*eventLoop{loop}
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
	source := &fdConn{fd: sourceFDs[0], commonConn: commonConn{events: events, loop: loop}}
	target := &fdConn{fd: targetFDs[0], commonConn: commonConn{events: events, loop: loop}}
	payload := make([]byte, 1<<20)
	for index := range payload {
		payload[index] = byte(index)
	}
	callbackErr := make(chan error, 1)
	events.OnData = func(conn Conn) error {
		if conn != source {
			_, _ = conn.Discard(-1)
			return nil
		}
		_, _ = conn.Discard(-1)
		_, err := target.Write(payload)
		callbackErr <- err
		return err
	}
	done := make(chan error, 1)
	go func() { done <- loop.Serve(false, nil) }()
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
		loop.beginStop(nil)
		select {
		case <-done:
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
	events.OnOpen = func(conn Conn) { opened <- conn.(*fdConn); <-releaseOpen }
	events.OnClose = func(_ Conn, err error) { closed <- err }
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
	events := &Events{Pollers: 1, OnClose: func(_ Conn, err error) { closed <- err }}
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

func TestBackpressureHysteresis(t *testing.T) {
	events := &Events{MaxOutboundBuffered: 100}
	conn := &fdConn{}
	conn.events = events
	conn.pending.Store(75)
	conn.outbound.AppendOwned(bytebuf.CloneBuffer([]byte("x")))
	if got := conn.desiredInterest(); got&poller.Readable != 0 || got&poller.Writable == 0 {
		t.Fatalf("high-water interest = %v", got)
	}
	conn.pending.Store(60)
	if got := conn.desiredInterest(); got&poller.Readable != 0 {
		t.Fatalf("hysteresis interest = %v", got)
	}
	conn.pending.Store(50)
	conn.outbound.Reset()
	conn.pending.Store(0)
	if got := conn.desiredInterest(); got != poller.Readable {
		t.Fatalf("low-water interest = %v", got)
	}
}

func TestUDPChildReportsUnflushedBytes(t *testing.T) {
	cause := errors.New("closed")
	closed := make(chan error, 1)
	events := &Events{OnClose: func(_ Conn, err error) { closed <- err }}
	server := &fdConn{udp: &unixUDPState{peers: make(map[socket.UDPAddress]*fdConn)}}
	child := &fdConn{udp: &unixUDPState{server: server}}
	child.events = events
	child.remoteAddr = &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1}
	child.udp.key = socket.UDPAddress{Port: 1}
	server.udp.peers[child.udp.key] = child
	child.pending.Store(7)
	child.closeOnLoop(cause)
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
	events.OnStart = func(events *Events) {
		events.acceptor.mux.Lock()
		for _, listener := range events.acceptor.listeners {
			started <- listener.laddr.String()
			break
		}
		events.acceptor.mux.Unlock()
	}
	events.OnOpen = func(conn Conn) {
		fdConn := conn.(*fdConn)
		if fdConn.loop == events.master {
			t.Errorf("TCP connection registered on the listener loop")
		}
		if conn.Userdata() == "dial" {
			dialOpenOnce.Do(func() { close(dialOpened) })
		}
	}
	events.OnData = func(conn Conn) error {
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
	events.OnStop = func(*Events) { close(stopped) }

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

func TestInboundLimitClosesConnection(t *testing.T) {
	closed := make(chan error, 1)
	events := &Events{
		Pollers:            1,
		MaxInboundBuffered: 4,
		OnData:             func(Conn) error { return nil },
		OnClose:            func(_ Conn, err error) { closed <- err },
	}
	testConn := newTestConnection(t, events)
	if _, err := unix.Write(testConn.peer, []byte("12345")); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-closed:
		if !errors.Is(err, ErrInboundOverflow) {
			t.Fatalf("close error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("inbound overflow did not close connection")
	}
}

func TestEventsCloseFromCallbackDoesNotDeadlock(t *testing.T) {
	started := make(chan string, 1)
	closeReturned := make(chan struct{})
	shutdownErr := errors.New("callback shutdown")
	events := &Events{Pollers: 1}
	events.OnStart = func(events *Events) {
		events.acceptor.mux.Lock()
		for _, listener := range events.acceptor.listeners {
			started <- listener.laddr.String()
			break
		}
		events.acceptor.mux.Unlock()
	}
	events.OnData = func(conn Conn) error {
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
	events.OnStart = func(events *Events) {
		events.acceptor.mux.Lock()
		for _, listener := range events.acceptor.listeners {
			started <- listener.laddr.String()
			break
		}
		events.acceptor.mux.Unlock()
	}
	events.OnData = func(conn Conn) error {
		data := make([]byte, conn.InboundBuffered())
		n, _ := conn.Read(data)
		if string(data[:n]) == "close" {
			return conn.Close()
		}
		_, err := conn.Write(data[:n])
		return err
	}
	events.OnClose = func(Conn, error) { childClosed <- struct{}{} }
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
	events.OnData = func(conn Conn) error {
		data := conn.PeekChunk()
		if _, err := conn.Write(data); err != nil {
			return err
		}
		_, err := conn.Discard(-1)
		return err
	}
	events.OnOutbound = func(_ Conn, written int) { outbound <- written }
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
			t.Fatalf("first OnOutbound = %d bytes, want the corked 8", written)
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
