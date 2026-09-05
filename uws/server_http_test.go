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
	"testing"
	"time"

	"github.com/urpc/uio"
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
	server := NewServer(serverHandler)
	server.Events = &uio.Events{Pollers: 1}
	server.Subprotocols = []string{"chat"}
	server.EnableCompression = true
	serveWithoutListener(t, server)
	httpServer := httptest.NewServer(server)

	clientHandler := &clientHandler{
		open:    make(chan struct{}),
		closed:  make(chan struct{}),
		message: make(chan Message, 1),
	}
	dialer := NewDialer()
	dialer.Events = &uio.Events{Pollers: 1}
	dialer.Subprotocols = []string{"chat"}
	dialer.EnableCompression = true
	client, err := dialer.Dial(context.Background(), "ws"+strings.TrimPrefix(httpServer.URL, "http"), clientHandler)
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
	server := NewServer(handlerFuncs{onOpen: func(*Conn) { opened <- struct{}{} }})
	server.Events = &uio.Events{Pollers: 1}
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
	server := NewServer(nil)
	server.Events = &uio.Events{Pollers: 1}
	request := newHTTPUpgradeRequest()
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, request)
	assertHTTPError(t, recorder, http.StatusInternalServerError, httpErrorServerNotServing)
	if server.started.Load() || server.ready.Load() {
		t.Fatal("ServeHTTP started the UIO transport")
	}
}

func TestServerHTTPRequiresHijacker(t *testing.T) {
	server := NewServer(nil)
	server.Events = &uio.Events{Pollers: 1}
	serveWithoutListener(t, server)
	request := newHTTPUpgradeRequest()
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, request)
	assertHTTPError(t, recorder, http.StatusInternalServerError, httpErrorCannotHijack)
}

func TestServerHTTPAfterCloseDoesNotHijack(t *testing.T) {
	server := NewServer(nil)
	server.Events = &uio.Events{Pollers: 1}
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
	oldOnStart := server.Events.OnStart
	server.Events.OnStart = func(events *uio.Events) {
		if oldOnStart != nil {
			oldOnStart(events)
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
