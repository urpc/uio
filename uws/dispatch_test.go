package uws

import (
	"bufio"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urpc/uio"
)

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

	if !executor.runNext() || !executor.runNext() {
		t.Fatal("executor did not run both queued messages")
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
	if !executor.runNext() || !executor.runNext() {
		t.Fatal("executor did not run globally budgeted messages")
	}
	if got := budget.messages.Load(); got != 0 {
		t.Fatalf("global pending messages = %d, want 0", got)
	}
	if got := budget.bytes.Load(); got != 0 {
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
	if got := budget.messages.Load(); got != 0 {
		t.Fatalf("global pending messages after close = %d, want 0", got)
	}
	if got := budget.bytes.Load(); got != 0 {
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
	if !executor.runNext() || !executor.runNext() {
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
	if budget.messages.Load() != 0 || budget.bytes.Load() != 0 {
		t.Fatalf("pending budget after rejection = %d/%d, want 0/0", budget.messages.Load(), budget.bytes.Load())
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

	conn.dispatch = &dispatchState{executor: &queuedExecutor{}, closed: true}
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
	if !nilBudget.reserve(1024) {
		t.Fatal("nil budget rejected a message")
	}
	nilBudget.release(1024)

	budget := &pendingBudget{}
	budget.configure(1, 2)
	if budget.reserve(3) {
		t.Fatal("byte budget accepted oversized message")
	}
	if budget.messages.Load() != 0 || budget.bytes.Load() != 0 {
		t.Fatalf("budget after rollback = %d messages, %d bytes", budget.messages.Load(), budget.bytes.Load())
	}
	if !budget.reserve(2) || budget.reserve(1) {
		t.Fatal("message budget limit was not enforced")
	}
	budget.release(2)
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
	for index := 0; index < burstMessages; index++ {
		if !budget.reserve(1024) {
			t.Fatalf("default total mailbox rejected a normal benchmark burst at message %d", index)
		}
	}
	for index := 0; index < burstMessages; index++ {
		budget.release(1024)
	}
	if gotMessages, gotBytes := budget.messages.Load(), budget.bytes.Load(); gotMessages != 0 || gotBytes != 0 {
		t.Fatalf("budget after burst release = %d messages, %d bytes", gotMessages, gotBytes)
	}
}

func TestFailDispatchReleasesQueueAndDropsClose(t *testing.T) {
	raw := newScriptedConn()
	handler := &recordingHandler{}
	budget := &pendingBudget{}
	budget.configure(4, 64)
	if !budget.reserve(7) {
		t.Fatal("failed to reserve test budget")
	}
	conn := &Conn{
		raw:     raw,
		handler: handler,
		dispatch: &dispatchState{
			budget: budget,
			queue: []dispatchEvent{
				{kind: dispatchMessage, bytes: 7},
				{kind: dispatchClose, close: CloseEvent{Code: 1001}},
			},
			messages: 1,
			bytes:    7,
		},
	}
	conn.failDispatch()
	conn.failDispatch()
	if raw.closes != 1 || budget.messages.Load() != 0 || budget.bytes.Load() != 0 {
		t.Fatalf("failed dispatch cleanup: closes=%d budget=%d/%d", raw.closes, budget.messages.Load(), budget.bytes.Load())
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
	if empty.dispatch != nil && empty.dispatch.running {
		t.Fatal("empty dispatch remained running")
	}
}

func TestDispatchExecutorRejectsOpenAndClose(t *testing.T) {
	openRaw := newScriptedConn()
	openConn := &Conn{
		raw:      openRaw,
		handler:  &recordingHandler{},
		dispatch: newDispatchState(rejectingExecutor{}, 0, 0, nil),
	}
	if err := openConn.dispatchOpen(); !errors.Is(err, ErrExecutorRejected) {
		t.Fatalf("rejected open error = %v", err)
	}
	if openRaw.closes != 1 {
		t.Fatalf("rejected open closes = %d, want 1", openRaw.closes)
	}

	failedHandler := &recordingHandler{}
	failed := &Conn{
		handler:  failedHandler,
		dispatch: &dispatchState{executor: &queuedExecutor{}, failed: true},
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
