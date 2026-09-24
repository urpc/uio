package uws

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urpc/uio"
)

func lifecycleTestTimeout() time.Duration {
	if testing.CoverMode() == "atomic" {
		return 90 * time.Second
	}
	return 30 * time.Second
}

type closeFromMessageHandler struct {
	open        chan struct{}
	closed      chan struct{}
	closeResult chan error
	closeMu     sync.RWMutex
	closeFn     func() error
	messageOnce sync.Once
	closeOnce   sync.Once
}

type lifecycleEchoHandler struct{}

type slowConnectionHandler struct {
	started chan struct{}
	release chan struct{}
}

type blockingCloseConnectionHandler struct {
	opens   atomic.Int32
	started chan struct{}
	release chan struct{}
}

type goroutineExecutor struct{}

func (goroutineExecutor) Submit(task uio.IOTask) bool {
	go task.RunTask()
	return true
}

func (executor goroutineExecutor) SubmitBatch(tasks []uio.IOTask) int {
	for _, task := range tasks {
		executor.Submit(task)
	}
	return len(tasks)
}

type switchableExecutor struct{ reject atomic.Bool }

func (executor *switchableExecutor) Submit(task uio.IOTask) bool {
	if executor.reject.Load() {
		return false
	}
	go task.RunTask()
	return true
}

func (executor *switchableExecutor) SubmitBatch(tasks []uio.IOTask) int {
	for index, task := range tasks {
		if !executor.Submit(task) {
			return index
		}
	}
	return len(tasks)
}

func (lifecycleEchoHandler) OnOpen(*Conn) {}
func (lifecycleEchoHandler) OnMessage(conn *Conn, message Message) {
	_ = conn.SendBinary(message.Payload)
}
func (lifecycleEchoHandler) OnClose(*Conn, CloseEvent) {}

func (h *slowConnectionHandler) OnOpen(*Conn) {}
func (h *slowConnectionHandler) OnMessage(conn *Conn, message Message) {
	if string(message.Payload) == "slow" {
		close(h.started)
		<-h.release
	}
	_ = conn.SendBinary(message.Payload)
}
func (*slowConnectionHandler) OnClose(*Conn, CloseEvent) {}

func (h *blockingCloseConnectionHandler) OnOpen(conn *Conn) {
	if h.opens.Add(1) == 1 {
		conn.SetUserdata("slow")
	}
}
func (*blockingCloseConnectionHandler) OnMessage(conn *Conn, message Message) {
	_ = conn.SendBinary(message.Payload)
}
func (h *blockingCloseConnectionHandler) OnClose(conn *Conn, _ CloseEvent) {
	if conn.Userdata() != "slow" {
		return
	}
	close(h.started)
	<-h.release
}

func TestUIOExecutorWritesFlushAutomatically(t *testing.T) {
	_, addr, _ := startConfiguredLifecycleTestServer(t, lifecycleEchoHandler{}, func(server *Server) {
		server.Events.Executor = goroutineExecutor{}
	})
	dialer := NewDialer()
	handler := &clientHandler{
		open:    make(chan struct{}),
		closed:  make(chan struct{}),
		message: make(chan Message, 1),
	}
	client, err := dialer.Dial(context.Background(), "ws://"+addr+"/", handler)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dialer.Close(nil) })
	select {
	case <-handler.open:
	case <-time.After(lifecycleTestTimeout()):
		t.Fatal("client did not open")
	}

	payload := []byte("uio executor auto flush")
	if err = client.SendBinary(payload); err != nil {
		t.Fatal(err)
	}
	select {
	case message := <-handler.message:
		if message.Type != BinaryMessage || string(message.Payload) != string(payload) {
			t.Fatalf("echo = %d/%q, want %d/%q", message.Type, message.Payload, BinaryMessage, payload)
		}
	case <-time.After(lifecycleTestTimeout()):
		t.Fatal("UIO executor write was not flushed automatically")
	}
}

func TestSlowConnectionDoesNotBlockOtherWebSocket(t *testing.T) {
	handler := &slowConnectionHandler{started: make(chan struct{}), release: make(chan struct{})}
	_, addr, _ := startConfiguredLifecycleTestServer(t, handler, func(server *Server) {
	})
	dialer := NewDialer()
	t.Cleanup(func() { _ = dialer.Close(nil) })
	newClient := func() (*Conn, *clientHandler) {
		clientHandler := &clientHandler{
			open: make(chan struct{}), closed: make(chan struct{}), message: make(chan Message, 1),
		}
		client, err := dialer.Dial(context.Background(), "ws://"+addr+"/", clientHandler)
		if err != nil {
			t.Fatal(err)
		}
		select {
		case <-clientHandler.open:
		case <-time.After(lifecycleTestTimeout()):
			t.Fatal("client did not open")
		}
		return client, clientHandler
	}
	slow, slowClientHandler := newClient()
	fast, fastClientHandler := newClient()
	if err := slow.SendBinary([]byte("slow")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-handler.started:
	case <-time.After(lifecycleTestTimeout()):
		t.Fatal("slow callback did not start")
	}
	if err := fast.SendBinary([]byte("fast")); err != nil {
		t.Fatal(err)
	}
	select {
	case message := <-fastClientHandler.message:
		if string(message.Payload) != "fast" {
			t.Fatalf("fast echo = %q", message.Payload)
		}
	case <-time.After(lifecycleTestTimeout()):
		t.Fatal("slow connection blocked fast connection")
	}
	select {
	case <-slowClientHandler.message:
		t.Fatal("slow connection completed before release")
	default:
	}
	close(handler.release)
	select {
	case message := <-slowClientHandler.message:
		if string(message.Payload) != "slow" {
			t.Fatalf("slow echo = %q", message.Payload)
		}
	case <-time.After(lifecycleTestTimeout()):
		t.Fatal("slow connection did not resume")
	}
}

func TestBlockingOnCloseDoesNotBlockOtherWebSocket(t *testing.T) {
	handler := &blockingCloseConnectionHandler{started: make(chan struct{}), release: make(chan struct{})}
	_, addr, _ := startConfiguredLifecycleTestServer(t, handler, func(server *Server) {
	})
	dialer := NewDialer()
	t.Cleanup(func() { _ = dialer.Close(nil) })
	newClient := func() (*Conn, *clientHandler) {
		clientHandler := &clientHandler{
			open: make(chan struct{}), closed: make(chan struct{}), message: make(chan Message, 1),
		}
		client, err := dialer.Dial(context.Background(), "ws://"+addr+"/", clientHandler)
		if err != nil {
			t.Fatal(err)
		}
		select {
		case <-clientHandler.open:
		case <-time.After(lifecycleTestTimeout()):
			t.Fatal("client did not open")
		}
		return client, clientHandler
	}
	slow, _ := newClient()
	fast, fastHandler := newClient()
	if err := slow.raw.CloseWith(io.EOF); err != nil {
		t.Fatal(err)
	}
	select {
	case <-handler.started:
	case <-time.After(lifecycleTestTimeout()):
		t.Fatal("slow OnClose did not start")
	}
	if err := fast.SendBinary([]byte("fast")); err != nil {
		t.Fatal(err)
	}
	select {
	case message := <-fastHandler.message:
		if string(message.Payload) != "fast" {
			t.Fatalf("fast echo = %q", message.Payload)
		}
	case <-time.After(lifecycleTestTimeout()):
		t.Fatal("blocking OnClose stopped the other connection")
	}
	close(handler.release)
}

func (h *closeFromMessageHandler) OnOpen(*Conn) { close(h.open) }

func (h *closeFromMessageHandler) OnMessage(*Conn, Message) {
	h.messageOnce.Do(func() {
		h.closeMu.RLock()
		closeFn := h.closeFn
		h.closeMu.RUnlock()
		h.closeResult <- closeFn()
	})
}

func (h *closeFromMessageHandler) OnClose(*Conn, CloseEvent) {
	h.closeOnce.Do(func() { close(h.closed) })
}

func (h *closeFromMessageHandler) setCloseFunc(closeFn func() error) {
	h.closeMu.Lock()
	h.closeFn = closeFn
	h.closeMu.Unlock()
}

func startLifecycleTestServer(t *testing.T, handler Handler) (*Server, string, <-chan struct{}) {
	return startConfiguredLifecycleTestServer(t, handler, nil)
}

func startConfiguredLifecycleTestServer(t *testing.T, handler Handler, configure func(*Server)) (*Server, string, <-chan struct{}) {
	t.Helper()
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := probe.Addr().String()
	_ = probe.Close()

	server := NewServer(handler)
	server.Events = &uio.Events{Pollers: 1, MaxBufferSize: 4 << 10}
	if configure != nil {
		configure(server)
	}
	started := make(chan struct{})
	server.Events.OnStart = func(*uio.Events) { close(started) }
	done := make(chan struct{})
	go func() {
		_ = server.Serve(addr)
		close(done)
	}()
	select {
	case <-started:
	case <-time.After(lifecycleTestTimeout()):
		t.Fatal("server did not start")
	}
	t.Cleanup(func() {
		_ = server.Close(nil)
		select {
		case <-done:
		case <-time.After(lifecycleTestTimeout()):
			t.Error("server did not stop")
		}
	})
	return server, addr, done
}

func TestDialerCloseIsReentrantFromOnClose(t *testing.T) {
	_, addr, _ := startLifecycleTestServer(t, nil)
	dialer := NewDialer()
	handler := &reentrantCloseHandler{
		open:   make(chan struct{}),
		closed: make(chan struct{}),
	}
	handler.setCloseFunc(func() { _ = dialer.Close(nil) })
	if _, err := dialer.Dial(context.Background(), "ws://"+addr+"/", handler); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dialer.Close(nil) })
	select {
	case <-handler.open:
	case <-time.After(lifecycleTestTimeout()):
		t.Fatal("client did not open")
	}

	closeErr := errors.New("dialer reentrant close")
	closeDone := make(chan error, 1)
	go func() { closeDone <- dialer.Close(closeErr) }()
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(lifecycleTestTimeout()):
		t.Fatal("Dialer.Close deadlocked with reentrant OnClose")
	}
	select {
	case <-handler.closed:
	case <-time.After(lifecycleTestTimeout()):
		t.Fatal("client OnClose was not called")
	}
}

func TestServerCloseIsReentrantFromOnClose(t *testing.T) {
	handler := &reentrantCloseHandler{
		open:   make(chan struct{}),
		closed: make(chan struct{}),
	}
	server, addr, _ := startLifecycleTestServer(t, handler)
	handler.setCloseFunc(func() { _ = server.Close(nil) })

	clientHandler := &clientHandler{
		open:    make(chan struct{}),
		closed:  make(chan struct{}),
		message: make(chan Message, 1),
	}
	dialer := NewDialer()
	if _, err := dialer.Dial(context.Background(), "ws://"+addr+"/", clientHandler); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dialer.Close(nil) })
	select {
	case <-handler.open:
	case <-time.After(lifecycleTestTimeout()):
		t.Fatal("server connection did not open")
	}

	closeErr := errors.New("server reentrant close")
	closeDone := make(chan error, 1)
	go func() { closeDone <- server.Close(closeErr) }()
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(lifecycleTestTimeout()):
		t.Fatal("Server.Close deadlocked with reentrant OnClose")
	}
	select {
	case <-handler.closed:
	case <-time.After(lifecycleTestTimeout()):
		t.Fatal("server OnClose was not called")
	}
}

func TestRejectedFinalCallbackKeepsOnCloseReentrant(t *testing.T) {
	executor := &switchableExecutor{}
	handler := &reentrantCloseHandler{open: make(chan struct{}), closed: make(chan struct{})}
	server, addr, serveDone := startConfiguredLifecycleTestServer(t, handler, func(server *Server) {
		server.Events.Executor = executor
	})
	handler.setCloseFunc(func() { _ = server.Close(nil) })
	dialer := NewDialer()
	clientHandler := &clientHandler{
		open: make(chan struct{}), closed: make(chan struct{}), message: make(chan Message, 1),
	}
	client, err := dialer.Dial(context.Background(), "ws://"+addr+"/", clientHandler)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dialer.Close(nil) })
	select {
	case <-handler.open:
	case <-time.After(lifecycleTestTimeout()):
		t.Fatal("server connection did not open")
	}
	executor.reject.Store(true)
	if err := client.raw.CloseWith(io.EOF); err != nil {
		t.Fatal(err)
	}
	select {
	case <-handler.closed:
	case <-time.After(lifecycleTestTimeout()):
		t.Fatal("rejected final task deadlocked reentrant OnClose")
	}
	select {
	case <-serveDone:
	case <-time.After(lifecycleTestTimeout()):
		t.Fatal("reentrant OnClose did not stop server")
	}
}

func TestServerCloseReturnsFromSynchronousOnMessage(t *testing.T) {
	handler := &closeFromMessageHandler{
		open:        make(chan struct{}),
		closed:      make(chan struct{}),
		closeResult: make(chan error, 1),
	}
	server, addr, serveDone := startLifecycleTestServer(t, handler)
	handler.setCloseFunc(func() error { return server.Close(errors.New("close from server callback")) })

	dialer := NewDialer()
	clientHandler := &clientHandler{
		open:    make(chan struct{}),
		closed:  make(chan struct{}),
		message: make(chan Message, 1),
	}
	client, err := dialer.Dial(context.Background(), "ws://"+addr+"/", clientHandler)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dialer.Close(nil) })
	select {
	case <-handler.open:
	case <-time.After(lifecycleTestTimeout()):
		t.Fatal("server connection did not open")
	}
	select {
	case <-clientHandler.open:
	case <-time.After(lifecycleTestTimeout()):
		t.Fatal("client connection did not open")
	}
	if err = client.SendBinary([]byte("close")); err != nil {
		t.Fatal(err)
	}
	select {
	case err = <-handler.closeResult:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(lifecycleTestTimeout()):
		t.Fatal("Server.Close blocked in synchronous OnMessage")
	}
	select {
	case <-handler.closed:
	case <-time.After(lifecycleTestTimeout()):
		t.Fatal("server callback shutdown did not complete")
	}
	select {
	case <-serveDone:
	case <-time.After(lifecycleTestTimeout()):
		t.Fatal("Serve did not complete callback shutdown")
	}
}

func TestDialerCloseReturnsFromSynchronousOnMessage(t *testing.T) {
	_, addr, _ := startLifecycleTestServer(t, lifecycleEchoHandler{})
	dialer := NewDialer()
	handler := &closeFromMessageHandler{
		open:        make(chan struct{}),
		closed:      make(chan struct{}),
		closeResult: make(chan error, 1),
	}
	handler.setCloseFunc(func() error { return dialer.Close(errors.New("close from dialer callback")) })
	client, err := dialer.Dial(context.Background(), "ws://"+addr+"/", handler)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dialer.Close(nil) })
	select {
	case <-handler.open:
	case <-time.After(lifecycleTestTimeout()):
		t.Fatal("client did not open")
	}
	if err = client.SendBinary([]byte("echo")); err != nil {
		t.Fatal(err)
	}
	select {
	case err = <-handler.closeResult:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(lifecycleTestTimeout()):
		t.Fatal("Dialer.Close blocked in synchronous OnMessage")
	}
	select {
	case <-handler.closed:
	case <-time.After(lifecycleTestTimeout()):
		t.Fatal("dialer callback shutdown did not complete")
	}
}

func TestServerCloseReturnsFromCheckOrigin(t *testing.T) {
	closeResult := make(chan error, 1)
	_, addr, serveDone := startConfiguredLifecycleTestServer(t, nil, func(server *Server) {
		server.CheckOrigin = func(*http.Request) bool {
			closeResult <- server.Close(errors.New("close from CheckOrigin"))
			return true
		}
	})
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	request := "GET / HTTP/1.1\r\n" +
		"Host: " + addr + "\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n" +
		"Sec-WebSocket-Version: 13\r\n\r\n"
	if _, err = conn.Write([]byte(request)); err != nil {
		t.Fatal(err)
	}
	select {
	case err = <-closeResult:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(lifecycleTestTimeout()):
		t.Fatal("Server.Close blocked in CheckOrigin")
	}
	select {
	case <-serveDone:
	case <-time.After(lifecycleTestTimeout()):
		t.Fatal("Serve did not stop after CheckOrigin closed the server")
	}
}
