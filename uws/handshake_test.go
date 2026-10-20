package uws

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urpc/uio/uws/internal/frame"
)

func TestServerHTTPHandlerEcho(t *testing.T) {
	baseHandler := &echoHandler{
		open:    make(chan struct{}),
		closed:  make(chan struct{}),
		message: make(chan Message, 1),
		conn:    make(chan *Conn, 1),
	}
	serverHandler := &httpEchoHandler{
		echoHandler: baseHandler,
		request:     make(chan *http.Request, 1),
	}
	server := NewServer(Options{Handler: serverHandler})
	server.Options.Pollers = 1
	server.Subprotocols = []string{"chat"}
	server.EnableCompression = true
	serveWithoutListener(t, server)
	httpServer := httptest.NewServer(server)

	clientHandler := &clientHandler{
		open:    make(chan struct{}),
		closed:  make(chan struct{}),
		message: make(chan Message, 1),
	}
	dialer := NewDialer(Options{Handler: clientHandler})
	dialer.Options.Pollers = 1
	dialer.Subprotocols = []string{"chat"}
	dialer.EnableCompression = true
	client, err := dialer.Dial(context.Background(), "ws"+strings.TrimPrefix(httpServer.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = client.Close(1000, "")
		_ = dialer.Close(nil)
		httpServer.Close()
	})

	waitForSignal(t, serverHandler.open, "server OnOpen")
	waitForSignal(t, clientHandler.open, "client OnOpen")
	serverConn := <-serverHandler.conn
	if serverConn.raw.Userdata() != serverConn {
		t.Fatal("transport userdata changed meaning during HTTP adoption")
	}
	if request := <-serverHandler.request; request == nil || request.RequestURI != "/" {
		t.Fatalf("HTTP upgrade request = %#v, want request URI /", request)
	}
	deadline := time.Now().Add(testIOTimeout())
	for serverConn.Request() != nil && time.Now().Before(deadline) {
		runtime.Gosched()
	}
	if request := serverConn.Request(); request != nil {
		t.Fatalf("HTTP upgrade request retained after OnOpen: %#v", request)
	}
	if serverConn.handshake.Load() != nil {
		t.Fatal("HTTP adoption retained handshake state after OnOpen")
	}
	serverConn.SetUserdata("business")
	if serverConn.Userdata() != "business" {
		t.Fatalf("business userdata = %v", serverConn.Userdata())
	}
	if protocol := client.Subprotocol(); protocol != "chat" {
		t.Fatalf("client subprotocol = %q, want chat", protocol)
	}
	payload := bytes.Repeat([]byte("compressible-message-"), 64)
	if err = client.SendText(payload); err != nil {
		t.Fatal(err)
	}
	select {
	case message := <-serverHandler.message:
		if !bytes.Equal(message.Payload, payload) {
			t.Fatalf("server message length = %d, want %d", len(message.Payload), len(payload))
		}
	case <-time.After(testIOTimeout()):
		t.Fatal("server did not receive message")
	}
	select {
	case message := <-clientHandler.message:
		if string(message.Payload) != "world" {
			t.Fatalf("client message = %q", message.Payload)
		}
	case <-time.After(testIOTimeout()):
		t.Fatal("client did not receive echo")
	}
}

type httpEchoHandler struct {
	*echoHandler
	request chan *http.Request
}

func (h *httpEchoHandler) OnOpen(conn *Conn) {
	h.request <- conn.Request()
	h.echoHandler.OnOpen(conn)
}

func TestServerHTTPRejectsBufferedDataBeforeSwitchingProtocols(t *testing.T) {
	opened := make(chan struct{}, 1)
	server := NewServer(Options{Handler: handlerFuncs{onOpen: func(*Conn) { opened <- struct{}{} }}})
	server.Options.Pollers = 1
	serveWithoutListener(t, server)
	httpServer := httptest.NewServer(server)
	t.Cleanup(httpServer.Close)

	target, err := url.Parse(httpServer.URL)
	if err != nil {
		t.Fatal(err)
	}
	client, err := net.Dial("tcp", target.Host)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	request := websocketUpgradeRequest(target.Host)
	earlyFrame := frame.Append(nil, frame.Frame{
		Fin: true, Opcode: frame.Text, Masked: true, Payload: []byte("early"),
	}, [4]byte{1, 2, 3, 4})
	if _, err = client.Write(append([]byte(request), earlyFrame...)); err != nil {
		t.Fatal(err)
	}
	if err = client.SetReadDeadline(time.Now().Add(testIOTimeout())); err != nil {
		t.Fatal(err)
	}
	response, err := http.ReadResponse(bufio.NewReader(client), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("response status = %q", response.Status)
	}
	if got := strings.TrimSpace(string(body)); got != httpErrorEarlyData {
		t.Fatalf("response body = %q, want %q", got, httpErrorEarlyData)
	}
	select {
	case <-opened:
		t.Fatal("buffered early frame reached OnOpen")
	case <-time.After(50 * time.Millisecond):
	}
}

func TestServerHTTPRequiresServe(t *testing.T) {
	server := NewServer(Options{})
	server.Options.Pollers = 1
	request := newHTTPUpgradeRequest()
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, request)
	assertHTTPError(t, recorder, http.StatusInternalServerError, httpErrorServerNotServing)
	if server.started.Load() || server.ready.Load() {
		t.Fatal("ServeHTTP started the UIO transport")
	}
}

func TestServerHTTPRequiresHijacker(t *testing.T) {
	server := NewServer(Options{})
	server.Options.Pollers = 1
	serveWithoutListener(t, server)
	request := newHTTPUpgradeRequest()
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, request)
	assertHTTPError(t, recorder, http.StatusInternalServerError, httpErrorCannotHijack)
}

func TestServerHTTPAfterCloseDoesNotHijack(t *testing.T) {
	server := NewServer(Options{})
	server.Options.Pollers = 1
	serveWithoutListener(t, server)
	if err := server.Close(nil); err != nil {
		t.Fatal(err)
	}
	request := newHTTPUpgradeRequest()
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, request)
	assertHTTPError(t, recorder, http.StatusInternalServerError, httpErrorServerNotServing)
}

func assertHTTPError(t *testing.T, recorder *httptest.ResponseRecorder, status int, message string) {
	t.Helper()
	if recorder.Code != status {
		t.Fatalf("status = %d, want %d", recorder.Code, status)
	}
	if got := strings.TrimSpace(recorder.Body.String()); got != message {
		t.Fatalf("response body = %q, want %q", got, message)
	}
}

func serveWithoutListener(t *testing.T, server *Server) {
	t.Helper()
	started := make(chan struct{})
	oldHook := server.startHook
	server.startHook = func() {
		if oldHook != nil {
			oldHook()
		}
		close(started)
	}
	done := make(chan error, 1)
	go func() { done <- server.Serve() }()
	t.Cleanup(func() {
		_ = server.Close(nil)
		select {
		case <-done:
		case <-time.After(testIOTimeout()):
			t.Error("Server.Serve did not stop")
		}
	})
	select {
	case <-started:
	case err := <-done:
		t.Fatalf("Server.Serve exited before OnStart: %v", err)
	case <-time.After(testIOTimeout()):
		t.Fatal("Server.Serve did not start")
	}
}

func newHTTPUpgradeRequest() *http.Request {
	request := httptest.NewRequest(http.MethodGet, "http://example.test/ws", nil)
	request.Header.Set("Connection", "Upgrade")
	request.Header.Set("Upgrade", "websocket")
	request.Header.Set("Sec-WebSocket-Version", "13")
	request.Header.Set("Sec-WebSocket-Key", testKey)
	return request
}

func websocketUpgradeRequest(host string) string {
	return "GET /ws HTTP/1.1\r\n" +
		"Host: " + host + "\r\n" +
		"Connection: Upgrade\r\nUpgrade: websocket\r\n" +
		"Sec-WebSocket-Version: 13\r\nSec-WebSocket-Key: " + testKey + "\r\n\r\n"
}

func waitForSignal(t *testing.T, signal <-chan struct{}, name string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(testIOTimeout()):
		t.Fatalf("%s was not called", name)
	}
}

type handlerFuncs struct {
	onOpen    func(*Conn)
	onMessage func(*Conn, Message)
	onClose   func(*Conn, CloseEvent)
}

func (handler handlerFuncs) OnOpen(conn *Conn) {
	if handler.onOpen != nil {
		handler.onOpen(conn)
	}
}

func (handler handlerFuncs) OnMessage(conn *Conn, message Message) {
	if handler.onMessage != nil {
		handler.onMessage(conn, message)
	}
}

func (handler handlerFuncs) OnClose(conn *Conn, event CloseEvent) {
	if handler.onClose != nil {
		handler.onClose(conn, event)
	}
}

// nativeRequestHandler records what OnOpen observes through Conn.Request.
type nativeRequestHandler struct {
	open    chan struct{}
	request chan *http.Request
	conn    chan *Conn
	opens   atomic.Int32
}

func (h *nativeRequestHandler) OnOpen(conn *Conn) {
	h.opens.Add(1)
	h.request <- conn.Request()
	h.conn <- conn
	close(h.open)
}

func (h *nativeRequestHandler) OnMessage(*Conn, Message) {}

func (h *nativeRequestHandler) OnClose(*Conn, CloseEvent) {}

// startNativeTestServer starts a native Serve server on a loopback address.
func startNativeTestServer(t *testing.T, handler Handler, configure func(*Server)) string {
	t.Helper()
	server, addr, serveDone := startTestServer(t, func() *Server {
		s := NewServer(Options{Handler: handler})
		if configure != nil {
			configure(s)
		}
		s.Options.Pollers = 1
		s.Options.ReadBufferSize = 4 << 10
		return s
	})
	t.Cleanup(func() { stopTestServer(t, server, serveDone) })
	return addr
}

func dialNativeTestServer(t *testing.T, addr string) (net.Conn, *bufio.Reader) {
	t.Helper()
	var client net.Conn
	var err error
	for deadline := time.Now().Add(testIOTimeout()); client == nil && time.Now().Before(deadline); {
		client, err = net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err != nil {
			time.Sleep(time.Millisecond)
		}
	}
	if client == nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client, bufio.NewReader(client)
}

func readStatusLine(t *testing.T, reader *bufio.Reader) string {
	t.Helper()
	line, err := reader.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	return line
}

func TestServerNativeOnOpenRequestVisible(t *testing.T) {
	handler := &nativeRequestHandler{
		open:    make(chan struct{}),
		request: make(chan *http.Request, 1),
		conn:    make(chan *Conn, 1),
	}
	addr := startNativeTestServer(t, handler, nil)
	client, reader := dialNativeTestServer(t, addr)
	request := "GET /custom?x=1 HTTP/1.1\r\nHost: " + addr + "\r\n" +
		"X-Business-Token: token-42\r\n" +
		"Connection: Upgrade\r\nUpgrade: websocket\r\n" +
		"Sec-WebSocket-Version: 13\r\nSec-WebSocket-Key: " + testKey + "\r\n\r\n"
	if _, err := client.Write([]byte(request)); err != nil {
		t.Fatal(err)
	}
	if line := readStatusLine(t, reader); !strings.HasPrefix(line, "HTTP/1.1 101 ") {
		t.Fatalf("handshake response = %q", line)
	}

	select {
	case <-handler.open:
	case <-time.After(testIOTimeout()):
		t.Fatal("server did not open")
	}
	seen := <-handler.request
	if seen == nil {
		t.Fatal("Request() returned nil during OnOpen for a native Serve connection")
	}
	if seen.Method != http.MethodGet || seen.RequestURI != "/custom?x=1" {
		t.Fatalf("request = %s %s", seen.Method, seen.RequestURI)
	}
	if token := seen.Header.Get("X-Business-Token"); token != "token-42" {
		t.Fatalf("custom header = %q, want token-42", token)
	}

	// notifyOpen releases the handshake state once OnOpen returns; the request
	// is then gone, and handlers that need it keep it themselves.
	conn := <-handler.conn
	deadline := time.Now().Add(testIOTimeout())
	for conn.Request() != nil || conn.handshake.Load() != nil {
		if time.Now().After(deadline) {
			t.Fatal("handshake state or request retained after OnOpen")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestServerNativeRequestWithCheckOrigin(t *testing.T) {
	handler := &nativeRequestHandler{
		open:    make(chan struct{}),
		request: make(chan *http.Request, 1),
		conn:    make(chan *Conn, 1),
	}
	configure := func(server *Server) {
		server.CheckOrigin = func(request *http.Request) bool {
			return request.Header.Get("Origin") == "http://allowed.test"
		}
	}
	addr := startNativeTestServer(t, handler, configure)

	allowed, reader := dialNativeTestServer(t, addr)
	if _, err := allowed.Write([]byte("GET / HTTP/1.1\r\nHost: " + addr + "\r\nOrigin: http://allowed.test\r\n" +
		"Connection: Upgrade\r\nUpgrade: websocket\r\n" +
		"Sec-WebSocket-Version: 13\r\nSec-WebSocket-Key: " + testKey + "\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	if line := readStatusLine(t, reader); !strings.HasPrefix(line, "HTTP/1.1 101 ") {
		t.Fatalf("allowed handshake response = %q", line)
	}
	select {
	case <-handler.open:
	case <-time.After(testIOTimeout()):
		t.Fatal("allowed origin did not open")
	}
	seen := <-handler.request
	if seen == nil || seen.Header.Get("Origin") != "http://allowed.test" {
		t.Fatalf("request during OnOpen = %#v", seen)
	}

	denied, deniedReader := dialNativeTestServer(t, addr)
	if _, err := denied.Write([]byte("GET / HTTP/1.1\r\nHost: " + addr + "\r\nOrigin: http://denied.test\r\n" +
		"Connection: Upgrade\r\nUpgrade: websocket\r\n" +
		"Sec-WebSocket-Version: 13\r\nSec-WebSocket-Key: " + testKey + "\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	// A rejected handshake closes without upgrading. The 400 and the close
	// race on the stdio backend, whose queued rejection may lose to the
	// blocking writer being shut down, so the client sees the response or just
	// EOF; either is a rejection, but an upgrade is not.
	line, readErr := deniedReader.ReadString('\n')
	if readErr == nil && !strings.HasPrefix(line, "HTTP/1.1 400 ") {
		t.Fatalf("denied handshake response = %q", line)
	}
	time.Sleep(100 * time.Millisecond)
	if got := handler.opens.Load(); got != 1 {
		t.Fatalf("OnOpen ran %d times, want only the allowed connection", got)
	}
}

func TestHandshakeResourcesDetachExactlyOnce(t *testing.T) {
	for _, action := range []string{"open", "stop", "expire"} {
		t.Run(action, func(t *testing.T) {
			var contextStops, cleanups atomic.Int32
			state := &handshakeState{
				epoch:       1,
				contextStop: func() bool { contextStops.Add(1); return true },
				cleanup:     func() { cleanups.Add(1) },
			}
			conn := &Conn{raw: newScriptedConn()}
			state.shard = deadlineShardIndex()
			deadlineShardAt(state.shard).arm(conn, deadlineKindHandshake, time.Now().Add(time.Hour).UnixNano(), 1, true)
			conn.handshake.Store(state)
			switch action {
			case "open":
				if !conn.markOpened() {
					t.Fatal("markOpened rejected live handshake")
				}
				if conn.handshake.Load() != state {
					t.Fatal("OnOpen lost handshake state before callback")
				}
				conn.notifyOpen()
			case "stop":
				conn.stopHandshakeTimer()
			case "expire":
				conn.expireHandshake(state, 1, context.DeadlineExceeded)
			}
			conn.stopHandshakeTimer()
			conn.expireHandshake(state, 1, context.DeadlineExceeded)
			if got := contextStops.Load(); got != 1 {
				t.Fatalf("context stops = %d, want 1", got)
			}
			if got := cleanups.Load(); got != 1 {
				t.Fatalf("cleanups = %d, want 1", got)
			}
			if conn.handshake.Load() != nil {
				t.Fatal("handshake state retained after completion")
			}
			state.mu.Lock()
			defer state.mu.Unlock()
			if deadlineShardAt(state.shard).has(conn, deadlineKindHandshake) {
				t.Fatal("handshake deadline retained after completion")
			}
			if state.contextStop != nil || state.cleanup != nil {
				t.Fatal("handshake resources retained after completion")
			}
		})
	}
}

func TestHandshakeOpenStopAndExpiryRace(t *testing.T) {
	for i := 0; i < 100; i++ {
		var cleanups atomic.Int32
		state := &handshakeState{epoch: 1, cleanup: func() { cleanups.Add(1) }}
		raw := newScriptedConn()
		conn := &Conn{raw: raw}
		conn.handshake.Store(state)
		var wg sync.WaitGroup
		wg.Add(3)
		go func() { defer wg.Done(); conn.markOpened() }()
		go func() { defer wg.Done(); conn.stopHandshakeTimer() }()
		go func() { defer wg.Done(); conn.expireHandshake(state, 1, context.DeadlineExceeded) }()
		wg.Wait()
		if got := cleanups.Load(); got != 1 {
			t.Fatalf("iteration %d: cleanups = %d, want 1", i, got)
		}
		if conn.handshake.Load() != nil {
			t.Fatalf("iteration %d: handshake state retained", i)
		}
		if conn.opened.Load() && raw.closes != 0 {
			t.Fatalf("iteration %d: opened connection expired", i)
		}
	}
}
