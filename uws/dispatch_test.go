package uws

import (
	"bufio"
	"errors"
	"io"
	"net"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	"github.com/urpc/uio"
	"github.com/urpc/uio/uws/internal/frame"
	"github.com/urpc/uio/uws/internal/handshake"
)

type dispatchTestSnapshot struct {
	phase            dispatchPhase
	runnerActive     bool
	queueLen         int
	queueCap         int
	firstBudgetShard uint8
	writeOwner       int64
	finishRequested  bool
}

func snapshotDispatchForTest(conn *Conn) dispatchTestSnapshot {
	state := conn.dispatch
	if state == nil {
		return dispatchTestSnapshot{}
	}
	mailbox := &state.mailbox
	mailbox.mu.Lock()
	snapshot := dispatchTestSnapshot{
		phase:        mailbox.phase,
		runnerActive: mailbox.runnerActive,
		queueLen:     len(mailbox.queue),
		queueCap:     cap(mailbox.queue),
	}
	if mailbox.head < len(mailbox.queue) {
		snapshot.firstBudgetShard = mailbox.queue[mailbox.head].budgetShard
	}
	mailbox.mu.Unlock()

	snapshot.writeOwner = state.writeBatch.ownerID()
	snapshot.finishRequested = state.writeBatch.finishIsRequested()
	return snapshot
}

func dispatchBufferCapacityForTest(conn *Conn) int {
	conn.writes.mu.Lock()
	defer conn.writes.mu.Unlock()
	if conn.dispatch == nil || conn.dispatch.writeBatch.buffer == nil {
		return 0
	}
	return conn.dispatch.writeBatch.buffer.Cap()
}

func configureEmptyDispatchQueueForTest(state *dispatchState, capacity int) {
	state.mailbox.mu.Lock()
	state.mailbox.queue = make([]dispatchEvent, 0, capacity)
	state.mailbox.runnerActive = true
	state.mailbox.mu.Unlock()
}

func installDispatchBatchForTest(conn *Conn, buffer *uio.Buffer, owner int64, finish bool) {
	conn.writes.mu.Lock()
	conn.dispatch.writeBatch.buffer = buffer
	conn.dispatch.writeBatch.begin(owner)
	if finish {
		conn.dispatch.writeBatch.requestFinish()
	}
	conn.writes.mu.Unlock()
}

func dispatchStateInPhaseForTest(executor Executor, phase dispatchPhase) *dispatchState {
	state := &dispatchState{executor: executor}
	state.mailbox.phase = phase
	return state
}

func seedDispatchMailboxForTest(
	state *dispatchState,
	closeEvent CloseEvent,
	events []dispatchEvent,
	pendingMessages, pendingBytes int,
) {
	state.mailbox.mu.Lock()
	state.mailbox.closeEvent = closeEvent
	state.mailbox.queue = events
	state.mailbox.pendingMessages = pendingMessages
	state.mailbox.pendingBytes = pendingBytes
	state.mailbox.mu.Unlock()
}

func setDispatchBudgetStartForTest(state *dispatchState, shard uint8) {
	state.mailbox.mu.Lock()
	state.mailbox.budgetStart = shard
	state.mailbox.mu.Unlock()
}

func TestExecutorOpenRequestIsReleasedAfterCallback(t *testing.T) {
	executor := &queuedExecutor{}
	request := &http.Request{RequestURI: "/rpc"}
	seen := make(chan *http.Request, 1)
	conn := &Conn{
		handler: handlerFuncs{onOpen: func(conn *Conn) { seen <- conn.Request() }},
	}
	conn.dispatch = newDispatchState(executor, 1, 1, nil)
	state := &handshakeState{upgrade: &httpUpgrade{request: handshake.Request{HTTP: request}}}
	conn.handshake.Store(state)

	if err := conn.dispatchOpen(); err != nil {
		t.Fatal(err)
	}
	if got := conn.handshake.Load(); got != state {
		t.Fatalf("executor queue handshake state = %p, want %p", got, state)
	}
	if !executor.runNext() {
		t.Fatal("executor did not run OnOpen")
	}
	if got := <-seen; got != request {
		t.Fatalf("OnOpen request = %p, want %p", got, request)
	}
	if got := conn.Request(); got != nil {
		t.Fatalf("request retained after OnOpen: %#v", got)
	}
	if got := conn.handshake.Load(); got != nil {
		t.Fatalf("handshake state retained after OnOpen: %#v", got)
	}
}

type blockingCloseHandler struct {
	called  chan struct{}
	release chan struct{}
	once    sync.Once
}

type dispatchWriteHandler struct{ err error }

func (*dispatchWriteHandler) OnOpen(*Conn) {}

func (h *dispatchWriteHandler) OnMessage(conn *Conn, _ Message) {
	h.err = conn.SendBinary([]byte("echo"))
}

func (*dispatchWriteHandler) OnClose(*Conn, CloseEvent) {}

func (*blockingCloseHandler) OnOpen(*Conn) {}

func (*blockingCloseHandler) OnMessage(*Conn, Message) {}

func (h *blockingCloseHandler) OnClose(*Conn, CloseEvent) {
	h.once.Do(func() { close(h.called) })
	<-h.release
}

func TestExecutorSerializesAndBoundsApplicationMessages(t *testing.T) {
	executor := &queuedExecutor{}
	handler := &recordingHandler{}
	conn := &Conn{
		handler:  handler,
		dispatch: newDispatchState(executor, 2, 8, nil),
	}
	conn.opened.Store(true)

	for _, payload := range [][]byte{[]byte("one"), []byte("two")} {
		if err := conn.enqueueMessage(Message{Type: BinaryMessage, Payload: payload}); err != nil {
			t.Fatalf("enqueueMessage(%q): %v", payload, err)
		}
	}
	payload := []byte("three")
	if err := conn.enqueueMessage(Message{Type: BinaryMessage, Payload: payload}); !errors.Is(err, ErrApplicationBackpressure) {
		t.Fatalf("third enqueue error = %v, want ErrApplicationBackpressure", err)
	}
	payload[0] = 'X'
	if got := executor.pending(); got != 1 {
		t.Fatalf("executor tasks = %d, want 1", got)
	}

	if !executor.runNext() {
		t.Fatal("executor did not run queued messages")
	}
	handler.mu.Lock()
	defer handler.mu.Unlock()
	if got, want := strings.Join(handler.messages, ","), "one,two"; got != want {
		t.Fatalf("message order = %q, want %q", got, want)
	}
}

func TestExecutorHonorsGlobalPendingBudget(t *testing.T) {
	executor := &queuedExecutor{}
	handler := &recordingHandler{}
	budget := &pendingBudget{}
	budget.configure(2, 6)
	conn := &Conn{
		handler:  handler,
		dispatch: newDispatchState(executor, 8, 32, budget),
	}
	conn.opened.Store(true)

	for _, payload := range [][]byte{[]byte("one"), []byte("two")} {
		if err := conn.enqueueMessage(Message{Type: BinaryMessage, Payload: payload}); err != nil {
			t.Fatalf("enqueueMessage(%q): %v", payload, err)
		}
	}
	if err := conn.enqueueMessage(Message{Type: BinaryMessage, Payload: []byte("x")}); !errors.Is(err, ErrApplicationBackpressure) {
		t.Fatalf("global budget error = %v, want ErrApplicationBackpressure", err)
	}
	if !executor.runNext() {
		t.Fatal("executor did not run globally budgeted messages")
	}
	gotMessages, gotBytes := budget.totals()
	if got := gotMessages; got != 0 {
		t.Fatalf("global pending messages = %d, want 0", got)
	}
	if got := gotBytes; got != 0 {
		t.Fatalf("global pending bytes = %d, want 0", got)
	}
}

func TestExecutorReleasesPendingBudgetWhenConnectionCloses(t *testing.T) {
	executor := &queuedExecutor{}
	handler := &recordingHandler{}
	budget := &pendingBudget{}
	budget.configure(4, 16)
	conn := &Conn{
		handler:  handler,
		dispatch: newDispatchState(executor, 4, 16, budget),
	}
	conn.opened.Store(true)
	if err := conn.enqueueMessage(Message{Type: BinaryMessage, Payload: []byte("pending")}); err != nil {
		t.Fatal(err)
	}
	conn.dispatchClose(CloseEvent{Code: 1000})
	gotMessages, gotBytes := budget.totals()
	if got := gotMessages; got != 0 {
		t.Fatalf("global pending messages after close = %d, want 0", got)
	}
	if got := gotBytes; got != 0 {
		t.Fatalf("global pending bytes after close = %d, want 0", got)
	}
	if !executor.runNext() {
		t.Fatal("close callback was not scheduled")
	}
}

func TestExecutorPreservesLifecycleOrder(t *testing.T) {
	executor := &queuedExecutor{}
	handler := &recordingHandler{}
	conn := &Conn{handler: handler, dispatch: newDispatchState(executor, 0, 0, nil)}
	conn.opened.Store(true)

	if err := conn.dispatchOpen(); err != nil {
		t.Fatal(err)
	}
	conn.dispatchClose(CloseEvent{Code: 1000})
	if !executor.runNext() {
		t.Fatal("executor did not run lifecycle callbacks")
	}
	handler.mu.Lock()
	defer handler.mu.Unlock()
	if got, want := strings.Join(handler.events, ","), "open,close"; got != want {
		t.Fatalf("lifecycle order = %q, want %q", got, want)
	}
}

func TestExecutorRejectionClosesConnection(t *testing.T) {
	raw := newScriptedConn()
	budget := &pendingBudget{}
	budget.configure(1, 16)
	handler := &blockingCloseHandler{called: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(func() { close(handler.release) })
	server := &Server{}
	conn := &Conn{
		raw:      raw,
		config:   testServerConfig(server),
		handler:  handler,
		dispatch: newDispatchState(rejectingExecutor{}, 1, 16, budget),
	}
	conn.opened.Store(true)
	raw.userdata = conn
	if err := conn.enqueueMessage(Message{Type: BinaryMessage, Payload: []byte("x")}); !errors.Is(err, ErrExecutorRejected) {
		t.Fatalf("enqueue error = %v, want ErrExecutorRejected", err)
	}
	if raw.closes != 1 {
		t.Fatalf("raw closes = %d, want 1", raw.closes)
	}
	done := make(chan struct{})
	go func() {
		server.onClose(raw, ErrExecutorRejected)
		close(done)
	}()
	select {
	case <-done:
	case <-handler.called:
		t.Fatal("OnClose fell back to the caller after executor rejection")
	case <-time.After(testIOTimeout()):
		t.Fatal("dispatchClose blocked after executor rejection")
	}
	select {
	case <-handler.called:
		t.Fatal("OnClose was delivered after executor rejection")
	default:
	}
	if messages, bytes := budget.totals(); messages != 0 || bytes != 0 {
		t.Fatalf("pending budget after rejection = %d/%d, want 0/0", messages, bytes)
	}
	if !conn.IsClosed() {
		t.Fatal("connection remained open after executor rejection")
	}
}

func TestExecutorRejectionDoesNotBlockEventLoopOrServerClose(t *testing.T) {
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := probe.Addr().String()
	_ = probe.Close()

	handler := &blockingCloseHandler{called: make(chan struct{}), release: make(chan struct{})}
	server := NewServer(handler)
	server.Executor = rejectingExecutor{}
	server.Events = &uio.Events{Pollers: 1, MaxBufferSize: 4 << 10}
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(addr) }()
	t.Cleanup(func() {
		close(handler.release)
		_ = server.Close(nil)
		select {
		case <-serveDone:
		case <-time.After(testIOTimeout()):
			t.Error("server did not stop")
		}
	})

	connect := func() {
		var client net.Conn
		for deadline := time.Now().Add(testIOTimeout()); client == nil && time.Now().Before(deadline); {
			client, err = net.DialTimeout("tcp", addr, 100*time.Millisecond)
			if err != nil {
				time.Sleep(time.Millisecond)
			}
		}
		if client == nil {
			t.Fatal(err)
		}
		defer client.Close()
		if err = client.SetDeadline(time.Now().Add(testIOTimeout())); err != nil {
			t.Fatal(err)
		}
		request := "GET /chat HTTP/1.1\r\nHost: " + addr + "\r\n" +
			"Connection: Upgrade\r\nUpgrade: websocket\r\n" +
			"Sec-WebSocket-Version: 13\r\nSec-WebSocket-Key: " + testKey + "\r\n\r\n"
		if _, err = io.WriteString(client, request); err != nil {
			t.Fatal(err)
		}
		reader := bufio.NewReader(client)
		status, readErr := reader.ReadString('\n')
		if readErr != nil {
			t.Fatal(readErr)
		}
		if !strings.HasPrefix(status, "HTTP/1.1 101 ") {
			t.Fatalf("response = %q", status)
		}
		for {
			line, readErr := reader.ReadString('\n')
			if readErr != nil {
				t.Fatal(readErr)
			}
			if line == "\r\n" {
				break
			}
		}
		if _, err = reader.ReadByte(); err == nil {
			t.Fatal("executor-rejected connection remained open")
		}
	}

	connect()
	connect()
	closeDone := make(chan error, 1)
	go func() { closeDone <- server.Close(nil) }()
	select {
	case err = <-closeDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(testIOTimeout()):
		t.Fatal("Server.Close blocked after executor rejection")
	}
	select {
	case <-handler.called:
		t.Fatal("OnClose ran outside the rejecting executor")
	default:
	}
}

func TestExecutorDoesNotFlushIdleCallback(t *testing.T) {
	raw := &writeProbeConn{}
	executor := &queuedExecutor{}
	conn := &Conn{
		raw:      raw,
		handler:  &recordingHandler{},
		dispatch: newDispatchState(executor, 1, 16, nil),
	}
	conn.opened.Store(true)
	if err := conn.enqueueMessage(Message{Type: BinaryMessage, Payload: []byte("x")}); err != nil {
		t.Fatal(err)
	}
	if !executor.runNext() {
		t.Fatal("executor did not run callback")
	}
	if raw.flushes != 0 {
		t.Fatalf("idle callback flushes = %d, want 0", raw.flushes)
	}
}

func TestExecutorCallbackWriteDoesNotAddFlushBarrier(t *testing.T) {
	raw := &writeProbeConn{}
	executor := &queuedExecutor{}
	handler := &dispatchWriteHandler{}
	conn := &Conn{
		raw:      raw,
		config:   testServerConfig(&Server{MaxFramePayload: 1024, MaxMessageSize: 1024, MaxOutboundBytes: 1 << 20}),
		handler:  handler,
		dispatch: newDispatchState(executor, 1, 16, nil),
	}
	conn.opened.Store(true)
	if err := conn.enqueueMessage(Message{Type: BinaryMessage, Payload: []byte("request")}); err != nil {
		t.Fatal(err)
	}
	if !executor.runNext() {
		t.Fatal("executor did not run callback")
	}
	if handler.err != nil {
		t.Fatal(handler.err)
	}
	if raw.writes != 1 || raw.flushes != 0 {
		t.Fatalf("callback transport calls = Write:%d Flush:%d, want 1/0", raw.writes, raw.flushes)
	}
}

func TestExecutorBatchesCallbackWritesPerRun(t *testing.T) {
	raw := &writeProbeConn{}
	executor := &queuedExecutor{}
	handler := &dispatchWriteHandler{}
	conn := &Conn{
		raw:      raw,
		config:   testServerConfig(&Server{MaxFramePayload: 1024, MaxMessageSize: 1024, MaxOutboundBytes: 1 << 20}),
		handler:  handler,
		dispatch: newDispatchState(executor, 4, 64, nil),
	}
	conn.opened.Store(true)
	for range 2 {
		if err := conn.enqueueMessage(Message{Type: BinaryMessage, Payload: []byte("request")}); err != nil {
			t.Fatal(err)
		}
	}
	if !executor.runNext() {
		t.Fatal("executor did not run callbacks")
	}
	if handler.err != nil {
		t.Fatal(handler.err)
	}
	if raw.writes != 1 || raw.flushes != 0 {
		t.Fatalf("batched transport calls = Write:%d Flush:%d, want 1/0", raw.writes, raw.flushes)
	}
}

func TestDispatchEventRemainsCompact(t *testing.T) {
	if unsafe.Sizeof(uintptr(0)) != 8 {
		return
	}
	if size := unsafe.Sizeof(dispatchEvent{}); size > 32 {
		t.Fatalf("dispatchEvent size = %d bytes, want at most 32", size)
	}
	if size := unsafe.Sizeof(dispatchState{}); size > 192 {
		t.Fatalf("dispatchState size = %d bytes (mailbox=%d batch=%d limits=%d), want at most 192",
			size, unsafe.Sizeof(dispatchMailbox{}), unsafe.Sizeof(dispatchWriteBatch{}), unsafe.Sizeof(dispatchLimits{}))
	}
}

func TestDispatchRetainsOnlyBoundedEmptyQueue(t *testing.T) {
	for _, test := range []struct {
		name     string
		capacity int
		want     int
	}{
		{name: "retain", capacity: maxRetainedDispatchEvents, want: maxRetainedDispatchEvents},
		{name: "release", capacity: maxRetainedDispatchEvents + 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			state := newDispatchState(&queuedExecutor{}, 1, 1, nil)
			configureEmptyDispatchQueueForTest(state, test.capacity)
			conn := &Conn{dispatch: state}
			conn.runDispatch()
			snapshot := snapshotDispatchForTest(conn)
			if snapshot.queueLen != 0 || snapshot.queueCap != test.want {
				t.Fatalf("empty queue len/cap = %d/%d, want 0/%d", snapshot.queueLen, snapshot.queueCap, test.want)
			}
		})
	}
}

func TestDispatchMailboxCompactsConsumedPrefix(t *testing.T) {
	mailbox := &dispatchMailbox{
		queue: make([]dispatchEvent, 128),
		head:  64,
	}
	mailbox.queue[64] = dispatchEvent{kind: dispatchOpen}
	mailbox.appendEvent(dispatchEvent{kind: dispatchClose})
	if mailbox.head != 0 || len(mailbox.queue) != 65 {
		t.Fatalf("compacted mailbox head/len = %d/%d, want 0/65", mailbox.head, len(mailbox.queue))
	}
	if mailbox.queue[0].kind != dispatchOpen || mailbox.queue[64].kind != dispatchClose {
		t.Fatalf("compacted event order = %v ... %v", mailbox.queue[0].kind, mailbox.queue[64].kind)
	}
}

func TestDispatchBatchCapacityAdaptsToFrameSize(t *testing.T) {
	for _, test := range []struct {
		name        string
		payloadSize int
		want        int
	}{
		{name: "small", payloadSize: 1024, want: dispatchWriteBatchSmallBytes},
		{name: "large", payloadSize: 9 << 10, want: dispatchWriteBatchMaxBytes},
	} {
		t.Run(test.name, func(t *testing.T) {
			conn := &Conn{
				raw: &writeProbeConn{},
				config: testServerConfig(&Server{
					MaxFramePayload:  dispatchWriteBatchMaxBytes,
					MaxMessageSize:   dispatchWriteBatchMaxBytes,
					MaxOutboundBytes: 1 << 20,
				}),
				dispatch: newDispatchState(&queuedExecutor{}, 1, 1, nil),
			}
			conn.opened.Store(true)
			started, err := conn.beginDispatchWrites()
			if err != nil {
				t.Fatal(err)
			}
			if !started {
				t.Fatal("failed to begin dispatch writes")
			}
			if err := conn.SendBinary(make([]byte, test.payloadSize)); err != nil {
				t.Fatal(err)
			}
			capacity := dispatchBufferCapacityForTest(conn)
			if capacity == 0 {
				t.Fatal("dispatch write was not batched")
			}
			if capacity != test.want {
				t.Fatalf("batch capacity = %d, want %d", capacity, test.want)
			}
			if err := conn.endDispatchWrites(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestExecutorCallbackDoesNotWaitForDispatchBatchLock(t *testing.T) {
	executor := &queuedExecutor{}
	called := make(chan struct{})
	conn := &Conn{
		handler:  handlerFuncs{onMessage: func(*Conn, Message) { close(called) }},
		dispatch: newDispatchState(executor, 1, 1, nil),
	}
	conn.opened.Store(true)
	if err := conn.enqueueMessage(Message{Type: BinaryMessage, Payload: []byte{'x'}}); err != nil {
		t.Fatal(err)
	}
	conn.lockWrite()
	runDone := make(chan struct{})
	go func() {
		executor.runNext()
		close(runDone)
	}()
	select {
	case <-called:
	case <-time.After(testIOTimeout()):
		conn.unlockWrite()
		t.Fatal("executor callback waited for a concurrent streaming writer")
	}
	conn.unlockWrite()
	<-runDone
	if snapshotDispatchForTest(conn).writeOwner != 0 {
		t.Fatal("dispatch batching was enabled without acquiring the write lock")
	}
}

func TestExecutorWriterMayOutliveCallback(t *testing.T) {
	raw := newScriptedConn()
	executor := &queuedExecutor{}
	writers := make(chan *Writer, 1)
	errs := make(chan error, 1)
	conn := &Conn{
		raw:    raw,
		config: testServerConfig(&Server{MaxFramePayload: 1024, MaxMessageSize: 1024, MaxOutboundBytes: 1 << 20}),
		handler: handlerFuncs{onMessage: func(conn *Conn, _ Message) {
			writer, err := conn.BeginMessage(BinaryMessage)
			if err != nil {
				errs <- err
				return
			}
			writers <- writer
		}},
	}
	conn.dispatch = newDispatchState(executor, 1, 1, nil)
	conn.opened.Store(true)
	if err := conn.enqueueMessage(Message{Type: BinaryMessage, Payload: []byte{'x'}}); err != nil {
		t.Fatal(err)
	}
	runDone := make(chan struct{})
	go func() {
		executor.runNext()
		close(runDone)
	}()
	var writer *Writer
	select {
	case writer = <-writers:
	case err := <-errs:
		t.Fatal(err)
	case <-time.After(testIOTimeout()):
		t.Fatal("callback did not create Writer")
	}
	select {
	case <-runDone:
	case <-time.After(testIOTimeout()):
		t.Fatal("executor runner waited for callback Writer.Close")
	}
	if owner := snapshotDispatchForTest(conn).writeOwner; owner != 0 {
		t.Fatalf("dispatch write owner = %d after callback, want 0", owner)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestExecutorLaterCloseCallbackCanCloseWriter(t *testing.T) {
	raw := newScriptedConn()
	executor := &queuedExecutor{}
	done := make(chan error, 1)
	var writer *Writer
	conn := &Conn{
		raw:    raw,
		config: testServerConfig(&Server{MaxFramePayload: 1024, MaxMessageSize: 1024, MaxOutboundBytes: 1 << 20}),
		handler: handlerFuncs{
			onMessage: func(conn *Conn, _ Message) {
				var err error
				writer, err = conn.BeginMessage(BinaryMessage)
				if err != nil {
					done <- err
					return
				}
				conn.dispatchClose(CloseEvent{Code: 1000})
			},
			onClose: func(*Conn, CloseEvent) { done <- writer.Close() },
		},
	}
	conn.dispatch = newDispatchState(executor, 1, 1, nil)
	conn.opened.Store(true)
	if err := conn.enqueueMessage(Message{Type: BinaryMessage, Payload: []byte{'x'}}); err != nil {
		t.Fatal(err)
	}
	runDone := make(chan struct{})
	go func() {
		executor.runNext()
		close(runDone)
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(testIOTimeout()):
		t.Fatal("later OnClose callback could not close Writer")
	}
	select {
	case <-runDone:
	case <-time.After(testIOTimeout()):
		t.Fatal("executor runner did not finish after OnClose closed Writer")
	}
}

func TestExecutorEndDoesNotWaitForExternalWriter(t *testing.T) {
	raw := newScriptedConn()
	executor := &queuedExecutor{}
	firstSent := make(chan struct{})
	resume := make(chan struct{})
	conn := &Conn{
		raw:    raw,
		config: testServerConfig(&Server{MaxFramePayload: 1024, MaxMessageSize: 1024, MaxOutboundBytes: 1 << 20}),
		handler: handlerFuncs{onMessage: func(conn *Conn, _ Message) {
			if err := conn.SendBinary([]byte("batched")); err != nil {
				t.Errorf("SendBinary: %v", err)
			}
			close(firstSent)
			<-resume
		}},
		dispatch: newDispatchState(executor, 1, 1, nil),
	}
	conn.opened.Store(true)
	if err := conn.enqueueMessage(Message{Type: BinaryMessage, Payload: []byte{'x'}}); err != nil {
		t.Fatal(err)
	}
	runDone := make(chan struct{})
	go func() {
		executor.runNext()
		close(runDone)
	}()
	<-firstSent
	externalWriter, err := conn.BeginMessage(BinaryMessage)
	if err != nil {
		t.Fatal(err)
	}
	close(resume)
	select {
	case <-runDone:
	case <-time.After(testIOTimeout()):
		t.Fatal("executor runner waited for external Writer.Close")
	}
	if len(raw.written) != 1 {
		t.Fatalf("writes before external Writer.Close = %d, want the dispatch batch", len(raw.written))
	}
	if err := externalWriter.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestCloseTransportHandsBatchFlushToActiveWrite(t *testing.T) {
	raw := newScriptedConn()
	conn := &Conn{
		raw:      raw,
		config:   testServerConfig(&Server{MaxFramePayload: 1024, MaxMessageSize: 1024, MaxOutboundBytes: 1 << 20}),
		dispatch: newDispatchState(&queuedExecutor{}, 1, 1, nil),
	}
	conn.opened.Store(true)
	started, err := conn.beginDispatchWrites()
	if err != nil {
		t.Fatal(err)
	}
	if !started {
		t.Fatal("failed to begin dispatch writes")
	}
	conn.lockWrite()
	closeDone := make(chan error, 1)
	go func() { closeDone <- conn.closeTransport() }()
	deadline := time.After(testIOTimeout())
	for !conn.dispatch.writeBatch.finishIsRequested() {
		select {
		case <-deadline:
			conn.unlockWrite()
			t.Fatal("closeTransport did not request batch completion")
		default:
			runtime.Gosched()
		}
	}
	if err := <-closeDone; err != nil {
		conn.unlockWrite()
		t.Fatal(err)
	}
	if raw.closes != 0 {
		conn.unlockWrite()
		t.Fatal("transport closed while a write transaction was active")
	}
	wireSize := frameWireSize(1, false)
	if !conn.reserveOutbound(wireSize) {
		conn.unlockWrite()
		t.Fatal("failed to reserve outbound bytes")
	}
	if err := conn.appendDispatchFrameLocked(frame.Frame{Fin: true, Opcode: frame.Binary, Payload: []byte{'x'}}, wireSize, [4]byte{}); err != nil {
		conn.unlockWrite()
		t.Fatal(err)
	}
	conn.unlockWrite()
	snapshot := snapshotDispatchForTest(conn)
	if dispatchBufferCapacityForTest(conn) != 0 || snapshot.writeOwner != 0 || snapshot.finishRequested {
		t.Fatal("batch completion responsibility was retained after active write released")
	}
	if raw.writes != 1 || len(raw.written) != 1 {
		t.Fatalf("flushed batch writes = %d/%d, want 1/1", raw.writes, len(raw.written))
	}
	if conn.pendingBytes.Load() != int64(wireSize) {
		t.Fatalf("pending bytes = %d, want %d until OnOutbound", conn.pendingBytes.Load(), wireSize)
	}
	completeTestOutbound(conn)
	if raw.closes != 1 {
		t.Fatalf("transport closes = %d, want 1 after outbound completion", raw.closes)
	}
}

func TestExecutorSlowCallbackYieldsBeforeNextMessage(t *testing.T) {
	executor := &queuedExecutor{}
	var calls atomic.Int32
	handler := handlerFuncs{onMessage: func(*Conn, Message) {
		if calls.Add(1) == 1 {
			time.Sleep(2 * maxDispatchRunDuration)
		}
	}}
	conn := &Conn{handler: handler, dispatch: newDispatchState(executor, 4, 64, nil)}
	conn.opened.Store(true)
	for range 2 {
		if err := conn.enqueueMessage(Message{Type: BinaryMessage, Payload: []byte("x")}); err != nil {
			t.Fatal(err)
		}
	}
	if !executor.runNext() {
		t.Fatal("executor did not run first callback")
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("callbacks in slow time slice = %d, want 1", got)
	}
	if got := executor.pending(); got != 1 {
		t.Fatalf("resubmitted runners = %d, want 1", got)
	}
	if !executor.runNext() || calls.Load() != 2 {
		t.Fatal("executor did not run the remaining callback")
	}
}

func TestExecutorYieldsAtEventBudget(t *testing.T) {
	executor := &queuedExecutor{}
	var calls atomic.Int32
	handler := handlerFuncs{onMessage: func(*Conn, Message) { calls.Add(1) }}
	conn := &Conn{
		handler:  handler,
		dispatch: newDispatchState(executor, maxDispatchEventsPerRun+1, maxDispatchEventsPerRun+1, nil),
	}
	conn.opened.Store(true)
	for range maxDispatchEventsPerRun + 1 {
		if err := conn.enqueueMessage(Message{Type: BinaryMessage, Payload: []byte{'x'}}); err != nil {
			t.Fatal(err)
		}
	}
	if !executor.runNext() {
		t.Fatal("executor did not run first time slice")
	}
	if got := calls.Load(); got != maxDispatchEventsPerRun {
		t.Fatalf("callbacks in first time slice = %d, want %d", got, maxDispatchEventsPerRun)
	}
	if got := executor.pending(); got != 1 {
		t.Fatalf("resubmitted runners = %d, want 1", got)
	}
	if !executor.runNext() || calls.Load() != maxDispatchEventsPerRun+1 {
		t.Fatal("executor did not run callback after event-budget yield")
	}
}

func TestExecutorControlFrameFlushesDataBatchFirst(t *testing.T) {
	raw := &writeProbeConn{}
	conn := &Conn{
		raw:      raw,
		config:   testServerConfig(&Server{MaxFramePayload: 1024, MaxMessageSize: 1024, MaxOutboundBytes: 1 << 20}),
		dispatch: newDispatchState(&queuedExecutor{}, 4, 64, nil),
	}
	conn.opened.Store(true)
	started, err := conn.beginDispatchWrites()
	if err != nil {
		t.Fatal(err)
	}
	if !started {
		t.Fatal("failed to begin dispatch write batch")
	}
	if err := conn.SendBinary([]byte("data")); err != nil {
		t.Fatal(err)
	}
	if err := conn.Ping([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	if err := conn.endDispatchWrites(); err != nil {
		t.Fatalf("ending dispatch writes: %v", err)
	}
	if raw.writes != 2 || raw.flushes != 1 {
		t.Fatalf("control transport calls = Write:%d Flush:%d, want 2/1", raw.writes, raw.flushes)
	}
}

func TestExecutorBatchDoesNotCaptureExternalSend(t *testing.T) {
	raw := newScriptedConn()
	executor := &queuedExecutor{}
	firstSent := make(chan struct{})
	resume := make(chan struct{})
	handler := handlerFuncs{onMessage: func(conn *Conn, _ Message) {
		if err := conn.SendBinary([]byte("first")); err != nil {
			t.Errorf("first SendBinary: %v", err)
		}
		close(firstSent)
		<-resume
		if err := conn.SendBinary([]byte("second")); err != nil {
			t.Errorf("second SendBinary: %v", err)
		}
	}}
	conn := &Conn{
		raw:      raw,
		config:   testServerConfig(&Server{MaxFramePayload: 1024, MaxMessageSize: 1024, MaxOutboundBytes: 1 << 20}),
		handler:  handler,
		dispatch: newDispatchState(executor, 4, 64, nil),
	}
	conn.opened.Store(true)
	if err := conn.enqueueMessage(Message{Type: BinaryMessage, Payload: []byte("request")}); err != nil {
		t.Fatal(err)
	}
	runnerDone := make(chan struct{})
	go func() {
		executor.runNext()
		close(runnerDone)
	}()
	<-firstSent
	if err := conn.SendBinary([]byte("external")); err != nil {
		t.Fatal(err)
	}
	close(resume)
	<-runnerDone
	if got := len(raw.written); got != 3 {
		t.Fatalf("transport writes = %d, want 3 independently ordered writes", got)
	}
	for index, want := range []string{"first", "external", "second"} {
		wire := raw.written[index]
		if len(wire) < len(want) || string(wire[len(wire)-len(want):]) != want {
			t.Fatalf("write %d = %x, want payload %q", index, wire, want)
		}
	}
}

func TestExecutorFlushesMessageWritesBeforeCloseCallback(t *testing.T) {
	raw := newScriptedConn()
	executor := &queuedExecutor{}
	messageStarted := make(chan struct{})
	resume := make(chan struct{})
	closeWrites := make(chan int, 1)
	handler := handlerFuncs{
		onMessage: func(conn *Conn, _ Message) {
			if err := conn.SendBinary([]byte("data")); err != nil {
				t.Errorf("SendBinary: %v", err)
			}
			close(messageStarted)
			<-resume
		},
		onClose: func(*Conn, CloseEvent) { closeWrites <- len(raw.written) },
	}
	conn := &Conn{
		raw:      raw,
		config:   testServerConfig(&Server{MaxFramePayload: 1024, MaxMessageSize: 1024, MaxOutboundBytes: 1 << 20}),
		handler:  handler,
		dispatch: newDispatchState(executor, 4, 64, nil),
	}
	conn.opened.Store(true)
	if err := conn.enqueueMessage(Message{Type: BinaryMessage, Payload: []byte("request")}); err != nil {
		t.Fatal(err)
	}
	runnerDone := make(chan struct{})
	go func() {
		executor.runNext()
		close(runnerDone)
	}()
	<-messageStarted
	conn.dispatchClose(CloseEvent{Code: 1000})
	close(resume)
	<-runnerDone
	select {
	case writes := <-closeWrites:
		if writes != 1 {
			t.Fatalf("writes visible to OnClose = %d, want 1", writes)
		}
	case <-time.After(testIOTimeout()):
		t.Fatal("OnClose was not called")
	}
}

func TestExecutorBatchWriteFailureClosesConnection(t *testing.T) {
	writeErr := errors.New("batch write failed")
	raw := &failNthWriteConn{scriptedConn: newScriptedConn(), failAt: 1, err: writeErr}
	executor := &queuedExecutor{}
	handler := &dispatchWriteHandler{}
	conn := &Conn{
		raw:      raw,
		config:   testServerConfig(&Server{MaxFramePayload: 1024, MaxMessageSize: 1024, MaxOutboundBytes: 1 << 20}),
		handler:  handler,
		dispatch: newDispatchState(executor, 4, 64, nil),
	}
	conn.opened.Store(true)
	if err := conn.enqueueMessage(Message{Type: BinaryMessage, Payload: []byte("request")}); err != nil {
		t.Fatal(err)
	}
	if !executor.runNext() {
		t.Fatal("executor did not run callback")
	}
	if handler.err != nil {
		t.Fatalf("callback SendBinary = %v, want deferred batch result", handler.err)
	}
	if !conn.closing.Load() || raw.closes != 1 {
		t.Fatalf("closing/raw closes = %v/%d, want true/1", conn.closing.Load(), raw.closes)
	}
	if snapshot := snapshotDispatchForTest(conn); conn.dispatch == nil || snapshot.phase == dispatchRejected || snapshot.runnerActive {
		t.Fatalf("dispatch after batch failure = %#v", conn.dispatch)
	}
	if err := conn.enqueueMessage(Message{Type: BinaryMessage, Payload: []byte("after failure")}); !errors.Is(err, ErrClosed) {
		t.Fatalf("message after asynchronous write failure = %v, want ErrClosed", err)
	}
	if pending := executor.pending(); pending != 0 {
		t.Fatalf("executor tasks after asynchronous write failure = %d, want 0", pending)
	}
}

func TestDispatchWriteFailureWaitsForCurrentCallback(t *testing.T) {
	writeErr := errors.New("batch write failed")
	raw := &failNthWriteConn{scriptedConn: newScriptedConn(), failAt: 1, err: writeErr}
	executor := &queuedExecutor{}
	callbackBlocked := make(chan struct{})
	releaseCallback := make(chan struct{})
	callbackErr := make(chan error, 1)
	closeEvent := make(chan CloseEvent, 1)
	var callbackActive atomic.Bool
	var callbacksOverlapped atomic.Bool
	conn := &Conn{
		raw:    raw,
		config: testServerConfig(&Server{MaxFramePayload: 1024, MaxMessageSize: 1024, MaxOutboundBytes: 1 << 20}),
		handler: handlerFuncs{
			onMessage: func(conn *Conn, _ Message) {
				callbackActive.Store(true)
				defer callbackActive.Store(false)
				conn.lockWrite()
				wireSize := frameWireSize(1, false)
				if !conn.reserveOutbound(wireSize) {
					conn.unlockWrite()
					callbackErr <- ErrBackpressure
					return
				}
				if err := conn.appendDispatchFrameLocked(frame.Frame{
					Fin: true, Opcode: frame.Binary, Payload: []byte{'x'},
				}, wireSize, [4]byte{}); err != nil {
					conn.unlockWrite()
					callbackErr <- err
					return
				}
				conn.dispatch.writeBatch.requestFinish()
				conn.unlockWrite()
				conn.dispatchClose(CloseEvent{Code: 1000})
				close(callbackBlocked)
				<-releaseCallback
			},
			onClose: func(_ *Conn, event CloseEvent) {
				if callbackActive.Load() {
					callbacksOverlapped.Store(true)
				}
				closeEvent <- event
			},
		},
		dispatch: newDispatchState(executor, 1, 1, nil),
	}
	conn.opened.Store(true)
	if err := conn.enqueueMessage(Message{Type: BinaryMessage, Payload: []byte{'x'}}); err != nil {
		t.Fatal(err)
	}
	runnerDone := make(chan struct{})
	go func() {
		executor.runNext()
		close(runnerDone)
	}()
	select {
	case <-callbackBlocked:
	case err := <-callbackErr:
		close(releaseCallback)
		t.Fatal(err)
	case <-time.After(testIOTimeout()):
		close(releaseCallback)
		t.Fatal("OnMessage did not reach the post-failure block")
	}
	if pending := executor.pending(); pending != 0 {
		close(releaseCallback)
		<-runnerDone
		t.Fatalf("executor queued %d runner before OnMessage returned", pending)
	}
	close(releaseCallback)
	<-runnerDone
	if pending := executor.pending(); pending != 1 {
		t.Fatalf("executor runners after OnMessage returned = %d, want 1", pending)
	}
	if !executor.runNext() {
		t.Fatal("executor did not deliver OnClose")
	}
	select {
	case event := <-closeEvent:
		if !errors.Is(event.Err, writeErr) {
			t.Fatalf("OnClose error = %v, want %v", event.Err, writeErr)
		}
	default:
		t.Fatal("OnClose was not delivered")
	}
	if callbacksOverlapped.Load() {
		t.Fatal("OnClose overlapped the active OnMessage callback")
	}
}

func TestDispatchBeginWriteFailureSkipsCallback(t *testing.T) {
	writeErr := errors.New("stale batch write failed")
	raw := &failNthWriteConn{scriptedConn: newScriptedConn(), failAt: 1, err: writeErr}
	executor := &queuedExecutor{}
	var calls atomic.Int32
	conn := &Conn{
		raw:      raw,
		config:   testServerConfig(&Server{MaxFramePayload: 1024, MaxMessageSize: 1024, MaxOutboundBytes: 1 << 20}),
		handler:  handlerFuncs{onMessage: func(*Conn, Message) { calls.Add(1) }},
		dispatch: newDispatchState(executor, 1, 1, nil),
	}
	conn.opened.Store(true)
	buffer := uio.AcquireBuffer(1)
	_, _ = buffer.Write([]byte{'x'})
	installDispatchBatchForTest(conn, buffer, 1, true)
	conn.pendingBytes.Store(1)
	if err := conn.enqueueMessage(Message{Type: BinaryMessage, Payload: []byte{'x'}}); err != nil {
		t.Fatal(err)
	}
	if !executor.runNext() {
		t.Fatal("executor did not run")
	}
	if calls.Load() != 0 {
		t.Fatal("business callback ran after the runner-start batch flush failed")
	}
	if !conn.closing.Load() || raw.closes != 1 {
		t.Fatalf("closing/raw closes = %v/%d, want true/1", conn.closing.Load(), raw.closes)
	}
	if snapshotDispatchForTest(conn).runnerActive {
		t.Fatal("dispatch remained running after runner-start batch failure")
	}
	conn.failDispatch(writeErr)
}

func TestDispatchWriteFailureCanRepeatAfterRunnerConsumesIt(t *testing.T) {
	state := newDispatchState(&queuedExecutor{}, 1, 1, nil)
	conn := &Conn{dispatch: state}
	conn.opened.Store(true)
	first := errors.New("first write failure")
	second := errors.New("second write failure")
	if !state.recordWriteFailure(first) {
		t.Fatal("first write failure was not recorded")
	}
	if state.recordWriteFailure(second) {
		t.Fatal("write failure replaced an unconsumed failure")
	}
	if _, restart, consumed := state.consumeWriteFailure(); !consumed || restart {
		t.Fatalf("first failure consume = consumed:%v restart:%v", consumed, restart)
	}
	if err := state.preflightMessage(conn, 1, 1, 1); !errors.Is(err, ErrClosed) {
		t.Fatalf("message after consumed write failure = %v, want ErrClosed", err)
	}
	if !state.recordWriteFailure(second) {
		t.Fatal("second write failure was not recorded after the first was consumed")
	}
	if _, restart, consumed := state.consumeWriteFailure(); !consumed || restart {
		t.Fatalf("second failure consume = consumed:%v restart:%v", consumed, restart)
	}
}

func TestDispatchDirectAndClosedPaths(t *testing.T) {
	conn := &Conn{}
	if err := conn.dispatchOpen(); err != nil {
		t.Fatal(err)
	}
	if err := conn.enqueueMessage(Message{Type: BinaryMessage}); err != nil {
		t.Fatal(err)
	}
	conn.dispatchClose(CloseEvent{Code: 1000})

	handler := &recordingHandler{}
	conn.handler = handler
	if err := conn.dispatchOpen(); err != nil {
		t.Fatal(err)
	}
	if err := conn.enqueueMessage(Message{Type: TextMessage, Payload: []byte("message")}); err != nil {
		t.Fatal(err)
	}
	conn.dispatchClose(CloseEvent{Code: 1000})
	handler.mu.Lock()
	if got := strings.Join(handler.events, ","); got != "open,message,close" {
		handler.mu.Unlock()
		t.Fatalf("direct events = %q", got)
	}
	handler.mu.Unlock()

	conn.dispatch = dispatchStateInPhaseForTest(&queuedExecutor{}, dispatchClosing)
	if err := conn.dispatchOpen(); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed dispatchOpen error = %v", err)
	}
	if err := conn.enqueueMessage(Message{Type: BinaryMessage}); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed enqueueMessage error = %v", err)
	}
	conn.dispatchClose(CloseEvent{Code: 1000})
}

func TestPendingBudgetRollbackAndNilPaths(t *testing.T) {
	var nilBudget *pendingBudget
	if _, ok := nilBudget.reserve(0, 1024); !ok {
		t.Fatal("nil budget rejected a message")
	}
	nilBudget.release(0, 1024)

	budget := &pendingBudget{}
	budget.configure(1, 2)
	if _, ok := budget.reserve(0, 3); ok {
		t.Fatal("byte budget accepted oversized message")
	}
	if messages, bytes := budget.totals(); messages != 0 || bytes != 0 {
		t.Fatalf("budget after rollback = %d messages, %d bytes", messages, bytes)
	}
	shard, ok := budget.reserve(0, 2)
	if !ok {
		t.Fatal("message budget rejected an in-budget message")
	}
	if _, ok := budget.reserve(0, 1); ok {
		t.Fatal("message budget limit was not enforced")
	}
	budget.release(shard, 2)
}

func TestDefaultExecutorMailboxLimitsLeaveBurstHeadroom(t *testing.T) {
	const (
		minimumPerConnMessages = 16 << 10
		minimumPerConnBytes    = 64 << 20
		minimumTotalMessages   = 1 << 20
		minimumTotalBytes      = 4 << 30
	)
	if defaultMaxPendingMessages < minimumPerConnMessages ||
		defaultMaxPendingBytes < minimumPerConnBytes ||
		defaultMaxPendingTotalMessages < minimumTotalMessages ||
		defaultMaxPendingTotalBytes < minimumTotalBytes {
		t.Fatalf("default executor mailbox limits are too restrictive: per-conn=%d/%d total=%d/%d",
			defaultMaxPendingMessages, defaultMaxPendingBytes,
			defaultMaxPendingTotalMessages, defaultMaxPendingTotalBytes)
	}

	budget := &pendingBudget{}
	budget.configure(defaultMaxPendingTotalMessages, defaultMaxPendingTotalBytes)
	const burstMessages = 1 << 16
	shards := make([]uint8, burstMessages)
	for index := 0; index < burstMessages; index++ {
		shard, ok := budget.reserve(uint8(index%pendingBudgetShardCount), 1024)
		if !ok {
			t.Fatalf("default total mailbox rejected a normal benchmark burst at message %d", index)
		}
		shards[index] = shard
	}
	for index := 0; index < burstMessages; index++ {
		budget.release(shards[index], 1024)
	}
	if gotMessages, gotBytes := budget.totals(); gotMessages != 0 || gotBytes != 0 {
		t.Fatalf("budget after burst release = %d messages, %d bytes", gotMessages, gotBytes)
	}
}

func TestPendingBudgetBorrowsCapacityFromAnotherShard(t *testing.T) {
	budget := &pendingBudget{shardCount: 2}
	for index := 0; index < 2; index++ {
		budget.shards[index].maxMessages = 1
		budget.shards[index].maxBytes = 8
	}
	executor := &queuedExecutor{}
	newConn := func() *Conn {
		conn := &Conn{handler: &recordingHandler{}, dispatch: newDispatchState(executor, 4, 32, budget)}
		setDispatchBudgetStartForTest(conn.dispatch, 0)
		conn.opened.Store(true)
		return conn
	}
	first := newConn()
	second := newConn()
	third := newConn()
	if err := first.enqueueMessage(Message{Type: BinaryMessage, Payload: []byte("a")}); err != nil {
		t.Fatal(err)
	}
	if err := second.enqueueMessage(Message{Type: BinaryMessage, Payload: []byte("b")}); err != nil {
		t.Fatalf("second connection could not borrow an idle shard: %v", err)
	}
	if got := snapshotDispatchForTest(second).firstBudgetShard; got != 1 {
		t.Fatalf("borrowed shard = %d, want 1", got)
	}
	if err := third.enqueueMessage(Message{Type: BinaryMessage, Payload: []byte("c")}); !errors.Is(err, ErrApplicationBackpressure) {
		t.Fatalf("fully reserved budget error = %v, want ErrApplicationBackpressure", err)
	}
	for executor.runNext() {
	}
	if messages, bytes := budget.totals(); messages != 0 || bytes != 0 {
		t.Fatalf("budget after borrowed events = %d/%d, want 0/0", messages, bytes)
	}
}

func TestPendingBudgetReleasesBorrowedShardOnCloseAndFailure(t *testing.T) {
	for _, test := range []struct {
		name    string
		cleanup func(*Conn)
	}{
		{name: "close", cleanup: func(conn *Conn) { conn.dispatchClose(CloseEvent{Code: 1000}) }},
		{name: "failure", cleanup: func(conn *Conn) { conn.failDispatch(ErrExecutorRejected) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			budget := &pendingBudget{shardCount: 2}
			for index := 0; index < 2; index++ {
				budget.shards[index].maxMessages = 1
				budget.shards[index].maxBytes = 8
			}
			executor := &queuedExecutor{}
			newConn := func() *Conn {
				conn := &Conn{handler: &recordingHandler{}, dispatch: newDispatchState(executor, 4, 32, budget)}
				setDispatchBudgetStartForTest(conn.dispatch, 0)
				conn.opened.Store(true)
				return conn
			}
			first := newConn()
			borrowed := newConn()
			if err := first.enqueueMessage(Message{Type: BinaryMessage, Payload: []byte("a")}); err != nil {
				t.Fatal(err)
			}
			if err := borrowed.enqueueMessage(Message{Type: BinaryMessage, Payload: []byte("b")}); err != nil {
				t.Fatal(err)
			}
			if got := snapshotDispatchForTest(borrowed).firstBudgetShard; got != 1 {
				t.Fatalf("borrowed shard = %d, want 1", got)
			}
			test.cleanup(borrowed)
			test.cleanup(first)
			if messages, bytes := budget.totals(); messages != 0 || bytes != 0 {
				t.Fatalf("budget after %s cleanup = %d/%d, want 0/0", test.name, messages, bytes)
			}
		})
	}
}

func TestPendingBudgetShardsPreserveConfiguredTotals(t *testing.T) {
	budget := &pendingBudget{}
	budget.configure(defaultMaxPendingTotalMessages, defaultMaxPendingTotalBytes)
	if got := budget.shardCount; got != pendingBudgetShardCount {
		t.Fatalf("budget shards = %d, want %d", got, pendingBudgetShardCount)
	}
	var maxMessages, maxBytes int64
	for index := range int(budget.shardCount) {
		shard := &budget.shards[index]
		if shard.maxMessages < defaultMaxPendingMessages {
			t.Fatalf("shard %d message budget = %d, want at least %d", index, shard.maxMessages, defaultMaxPendingMessages)
		}
		if shard.maxBytes < defaultMaxPendingBytes {
			t.Fatalf("shard %d byte budget = %d, want at least %d", index, shard.maxBytes, defaultMaxPendingBytes)
		}
		maxMessages += shard.maxMessages
		maxBytes += shard.maxBytes
	}
	if maxMessages != defaultMaxPendingTotalMessages || maxBytes != defaultMaxPendingTotalBytes {
		t.Fatalf("shard totals = %d/%d, want %d/%d", maxMessages, maxBytes,
			defaultMaxPendingTotalMessages, defaultMaxPendingTotalBytes)
	}
}

func TestFailDispatchReleasesQueueAndDropsClose(t *testing.T) {
	raw := newScriptedConn()
	handler := &recordingHandler{}
	budget := &pendingBudget{}
	budget.configure(4, 64)
	budgetShard, ok := budget.reserve(0, 7)
	if !ok {
		t.Fatal("failed to reserve test budget")
	}
	state := &dispatchState{budget: budget}
	seedDispatchMailboxForTest(state, CloseEvent{Code: 1001}, []dispatchEvent{
		{kind: dispatchMessage, bytes: 7, budgetShard: budgetShard},
		{kind: dispatchClose},
	}, 1, 7)
	conn := &Conn{raw: raw, handler: handler, dispatch: state}
	conn.failDispatch(ErrExecutorRejected)
	conn.failDispatch(ErrExecutorRejected)
	if messages, bytes := budget.totals(); raw.closes != 1 || messages != 0 || bytes != 0 {
		t.Fatalf("failed dispatch cleanup: closes=%d budget=%d/%d", raw.closes, messages, bytes)
	}
	handler.mu.Lock()
	defer handler.mu.Unlock()
	if got := strings.Join(handler.events, ","); got != "" {
		t.Fatalf("failed dispatch events = %q", got)
	}
}

func TestRunDispatchEmptyPath(t *testing.T) {
	empty := &Conn{}
	empty.runDispatch()
	if empty.dispatch != nil && snapshotDispatchForTest(empty).runnerActive {
		t.Fatal("empty dispatch remained running")
	}
}

func TestDispatchExecutorRejectsOpenAndClose(t *testing.T) {
	openRaw := newScriptedConn()
	handshake := &handshakeState{}
	openConn := &Conn{
		raw:      openRaw,
		handler:  &recordingHandler{},
		dispatch: newDispatchState(rejectingExecutor{}, 0, 0, nil),
	}
	openConn.handshake.Store(handshake)
	if err := openConn.dispatchOpen(); !errors.Is(err, ErrExecutorRejected) {
		t.Fatalf("rejected open error = %v", err)
	}
	if got := openConn.handshake.Load(); got != nil {
		t.Fatalf("rejected open retained handshake state: %#v", got)
	}
	if openRaw.closes != 1 {
		t.Fatalf("rejected open closes = %d, want 1", openRaw.closes)
	}

	failedHandler := &recordingHandler{}
	failed := &Conn{
		handler:  failedHandler,
		dispatch: dispatchStateInPhaseForTest(&queuedExecutor{}, dispatchRejected),
	}
	failed.dispatchClose(CloseEvent{Code: 1001})
	failed.dispatchClose(CloseEvent{Code: 1001})
	failedHandler.mu.Lock()
	if got := strings.Join(failedHandler.events, ","); got != "" {
		failedHandler.mu.Unlock()
		t.Fatalf("failed close events = %q", got)
	}
	failedHandler.mu.Unlock()

	closeRaw := newScriptedConn()
	closeConn := &Conn{
		raw:      closeRaw,
		handler:  &recordingHandler{},
		dispatch: newDispatchState(rejectingExecutor{}, 0, 0, nil),
	}
	closeConn.dispatchClose(CloseEvent{Code: 1001})
	if closeRaw.closes != 1 {
		t.Fatalf("rejected close submits = %d closes, want 1", closeRaw.closes)
	}
}
