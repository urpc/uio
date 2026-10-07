package uws

import (
	"bufio"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/urpc/uio"
)

// nativeRequestHandler records what OnOpen observes through Conn.Request.
type nativeRequestHandler struct {
	open    chan struct{}
	request chan *http.Request
	conn    chan *Conn
}

func (h *nativeRequestHandler) OnOpen(conn *Conn) {
	h.request <- conn.Request()
	h.conn <- conn
	close(h.open)
}

func (h *nativeRequestHandler) OnMessage(*Conn, Message) {}

func (h *nativeRequestHandler) OnClose(*Conn, CloseEvent) {}

// startNativeTestServer starts a native Serve server on a loopback address.
func startNativeTestServer(t *testing.T, handler Handler, configure func(*Server)) string {
	t.Helper()
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := probe.Addr().String()
	_ = probe.Close()

	server := NewServer(handler)
	if configure != nil {
		configure(server)
	}
	server.Events = &uio.Events{Pollers: 1, MaxBufferSize: 4 << 10}
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(addr) }()
	t.Cleanup(func() {
		_ = server.Close(nil)
		select {
		case <-serveDone:
		case <-time.After(testIOTimeout()):
			t.Error("server did not stop")
		}
	})
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
	if line := readStatusLine(t, deniedReader); !strings.HasPrefix(line, "HTTP/1.1 400 ") {
		t.Fatalf("denied handshake response = %q", line)
	}
}
