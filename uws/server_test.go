package uws

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	crand "crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"github.com/urpc/uio"
	"github.com/urpc/uio/uws/internal/frame"
	"io"
	"math/big"
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
)

func TestServerHTTPHandlerEcho(t *testing.T) {
	baseHandler := &echoHandler{open: make(chan struct{}), closed: make(chan struct{}), message: make(chan Message, 1), conn: make(chan *Conn, 1)}
	serverHandler := &httpEchoHandler{echoHandler: baseHandler, request: make(chan *http.Request, 1)}
	server := NewServer(Options{Handler: serverHandler})
	server.Options.Pollers = 1
	server.Subprotocols = []string{"chat"}
	server.EnableCompression = true
	serveWithoutListener(t, server)
	httpServer := httptest.NewServer(server)
	clientHandler := &clientHandler{open: make(chan struct{}), closed: make(chan struct{}), message: make(chan Message, 1)}
	dialer := NewDialer(Options{Handler: clientHandler})
	dialer.Options.Pollers = 1
	dialer.Subprotocols = []string{"chat"}
	dialer.EnableCompression = true
	err := dialer.Dial(context.Background(), "ws"+strings.TrimPrefix(httpServer.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = dialer.Close(nil)
		httpServer.Close()
	})
	waitForSignal(t, serverHandler.open, "server OnOpen")
	waitForSignal(t, clientHandler.open, "client OnOpen")
	client := clientHandler.conn
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
	payload := strings.Repeat("compressible-message-", 64)
	if err = client.SendText(payload); err != nil {
		t.Fatal(err)
	}
	select {
	case message := <-serverHandler.message:
		if string(message.Payload) != payload {
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
	server := NewServer(Options{Handler: handlerFuncs{onOpen: func(*Conn) {
		signal(opened, struct{}{})
	}}})
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
	earlyFrame := frame.Append(nil, frame.Frame{Fin: true, Opcode: frame.Text, Masked: true, Payload: []byte("early")}, [4]byte{1, 2, 3, 4})
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
	go func() {
		done <- server.Serve()
	}()
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
	return "GET /ws HTTP/1.1\r\n" + "Host: " + host + "\r\n" + "Connection: Upgrade\r\nUpgrade: websocket\r\n" + "Sec-WebSocket-Version: 13\r\nSec-WebSocket-Key: " + testKey + "\r\n\r\n"
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
	t.Cleanup(func() {
		stopTestServer(t, server, serveDone)
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
	t.Cleanup(func() {
		_ = client.Close()
	})
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

type reuseRequestHandler struct {
	request chan *http.Request
	conn    chan *Conn
}

func (h *reuseRequestHandler) OnOpen(conn *Conn) {
	h.request <- conn.Request()
	h.conn <- conn
}

func (h *reuseRequestHandler) OnMessage(*Conn, Message) {
}

func (h *reuseRequestHandler) OnClose(*Conn, CloseEvent) {
}

func TestServerNativeRequestSurvivesHandshakeReuse(t *testing.T) {
	handler := &reuseRequestHandler{request: make(chan *http.Request, 16), conn: make(chan *Conn, 16)}
	addr := startNativeTestServer(t, handler, nil)
	var kept []*http.Request
	for i := range 8 {
		client, reader := dialNativeTestServer(t, addr)
		token := fmt.Sprintf("token-%d", i)
		request := "GET /n/" + token + " HTTP/1.1\r\nHost: " + addr + "\r\n" + "X-Business-Token: " + token + "\r\n" + "Connection: Upgrade\r\nUpgrade: websocket\r\n" + "Sec-WebSocket-Version: 13\r\nSec-WebSocket-Key: " + testKey + "\r\n\r\n"
		if _, err := client.Write([]byte(request)); err != nil {
			t.Fatal(err)
		}
		if line := readStatusLine(t, reader); !strings.HasPrefix(line, "HTTP/1.1 101 ") {
			t.Fatalf("handshake response = %q", line)
		}
		seen := <-handler.request
		if seen == nil || seen.Header.Get("X-Business-Token") != token || seen.RequestURI != "/n/"+token {
			t.Fatalf("connection %d saw request %+v", i, seen)
		}
		kept = append(kept, seen)
		conn := <-handler.conn
		deadline := time.Now().Add(testIOTimeout())
		for conn.Request() != nil {
			if time.Now().After(deadline) {
				t.Fatal("request retained after OnOpen")
			}
			time.Sleep(time.Millisecond)
		}
		_ = client.Close()
	}
	for i, request := range kept {
		if token := fmt.Sprintf("token-%d", i); request.Header.Get("X-Business-Token") != token {
			t.Fatalf("kept request %d now carries %q", i, request.Header.Get("X-Business-Token"))
		}
	}
}

func TestHandshakeResourcesDetachExactlyOnce(t *testing.T) {
	for _, action := range []string{"open", "stop", "expire"} {
		t.Run(action, func(t *testing.T) {
			var contextStops, cleanups atomic.Int32
			state := &handshakeState{epoch: 1, contextStop: func() bool {
				contextStops.Add(1)
				return true
			}, cleanup: func() {
				cleanups.Add(1)
			}}
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
		state := &handshakeState{epoch: 1, cleanup: func() {
			cleanups.Add(1)
		}}
		raw := newScriptedConn()
		conn := &Conn{raw: raw}
		conn.handshake.Store(state)
		var wg sync.WaitGroup
		wg.Add(3)
		go func() {
			defer wg.Done()
			conn.markOpened()
		}()
		go func() {
			defer wg.Done()
			conn.stopHandshakeTimer()
		}()
		go func() {
			defer wg.Done()
			conn.expireHandshake(state, 1, context.DeadlineExceeded)
		}()
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

func TestOptionsValidation(t *testing.T) {
	for _, test := range []struct {
		name string
		opts Options
	}{{"negative header limit", Options{MaxHeaderBytes: -1}}, {"negative timeout", Options{HandshakeTimeout: -time.Second}}, {"negative close timeout", Options{CloseTimeout: -time.Second}}, {"negative heartbeat", Options{HeartbeatInterval: -time.Second}}, {"negative pollers", Options{Pollers: -1}}, {"negative read buffer", Options{ReadBufferSize: -1}}, {"negative outbound limit", Options{MaxOutboundBuffered: -1}}, {"compression level range", Options{EnableCompression: true, CompressionLevel: 10}}, {"level without compression", Options{CompressionLevel: 1}}} {
		if _, err := test.opts.resolve(); !errors.Is(err, ErrInvalidOptions) {
			t.Fatalf("%s: error = %v, want %v", test.name, err, ErrInvalidOptions)
		}
	}
}

func TestDialerConnectionsShareFrozenConfig(t *testing.T) {
	dialer := &Dialer{Options: Options{Subprotocols: []string{"v1"}, MaxHeaderBytes: 101, MaxFramePayload: 102, MaxMessageSize: 103, CloseTimeout: 105 * time.Millisecond, HandshakeTimeout: 106 * time.Millisecond, EnableCompression: true, CompressionLevel: 1}}
	dialer.config = testDialerConfig(dialer)
	dialer.Subprotocols[0] = "mutated"
	dialer.MaxFramePayload = 1
	dialer.MaxMessageSize = 1
	first := dialer.newClientConn(nil, &dialSetup{})
	second := dialer.newClientConn(nil, &dialSetup{})
	if first.config != dialer.config || second.config != dialer.config || first.config != second.config {
		t.Fatal("dialer connections did not share the startup config")
	}
	if first.config.subprotocols[0] != "v1" || !first.isClient() {
		t.Fatal("dialer connection observed configuration mutated after startup")
	}
	if first.maxFramePayload() != 102 || first.maxMessageSize() != 103 {
		t.Fatal("dialer connection limits changed after startup")
	}
}

type heartbeatWriteProbe struct {
	*scriptedConn
	written chan struct{}
}

func (c *heartbeatWriteProbe) signalWrite() {
	select {
	case c.written <- struct{}{}:
	default:
	}
}

func (c *heartbeatWriteProbe) Writev(buffers [][]byte) (int, error) {
	n, err := c.scriptedConn.Writev(buffers)
	c.signalWrite()
	return n, err
}

func (c *heartbeatWriteProbe) WriteOwned(buffer *uio.Buffer) (int, error) {
	n, err := c.scriptedConn.WriteOwned(buffer)
	c.signalWrite()
	return n, err
}

func TestHeartbeatRepeatedBackpressureHasFiniteTimeout(t *testing.T) {
	server := NewServer(Options{})
	server.HeartbeatTimeout = 20 * time.Millisecond
	server.CloseTimeout = 5 * time.Millisecond
	raw := newScriptedConn()
	raw.writeErr = uio.ErrOutboundOverflow
	conn := &Conn{raw: raw, config: testServerConfig(server), heartbeat: &heartbeatState{}}
	conn.opened.Store(true)
	conn.beginHeartbeat()
	first := time.Now()
	for i := 0; i < 4; i++ {
		at := first.Add(time.Duration(i) * server.HeartbeatTimeout / 4)
		conn.heartbeatTick(at)
		if conn.closing.Load() || conn.heartbeat.pingOutstanding.Load() || raw.closes != 0 {
			t.Fatalf("attempt %d: heartbeat closed early or retained an unsent Ping", i)
		}
		conn.heartbeat.mu.Lock()
		stalledAt := conn.heartbeat.sendStalledAt
		conn.heartbeat.mu.Unlock()
		if stalledAt != first.UnixNano() {
			t.Fatalf("attempt %d: stalled at %d, want %d", i, stalledAt, first.UnixNano())
		}
	}
	if raw.writes != 4 {
		t.Fatalf("failed Ping writes = %d, want 4", raw.writes)
	}
	conn.heartbeatTick(first.Add(server.HeartbeatTimeout))
	if raw.closes != 1 || !conn.closing.Load() {
		t.Fatalf("stalled connection = closes:%d closing:%v, want 1/true", raw.closes, conn.closing.Load())
	}
	if info := conn.closeInfo(); info.Code != 1001 || info.Reason != "heartbeat timeout" {
		t.Fatalf("heartbeat close info = %+v", info)
	}
}

func TestHeartbeatFatalSendErrorAbortsImmediately(t *testing.T) {
	server := NewServer(Options{})
	failure := errors.New("heartbeat write failed")
	raw := newScriptedConn()
	raw.writeErr = failure
	conn := &Conn{raw: raw, config: testServerConfig(server), heartbeat: &heartbeatState{}}
	conn.opened.Store(true)
	conn.beginHeartbeat()
	conn.heartbeatTick(time.Now())
	if raw.closes != 1 || !conn.closing.Load() {
		t.Fatalf("failed heartbeat = closes:%d closing:%v", raw.closes, conn.closing.Load())
	}
	if info := conn.closeInfo(); !errors.Is(info.Err, failure) {
		t.Fatalf("close error = %v, want %v", info.Err, failure)
	}
}

func TestHeartbeatSendPaths(t *testing.T) {
	if err := (&Conn{}).sendHeartbeatPing(time.Now()); !errors.Is(err, ErrClosed) {
		t.Fatalf("unavailable heartbeat ping = %v", err)
	}
	raw := newScriptedConn()
	conn := &Conn{raw: raw, config: testServerConfig(NewServer(Options{})), heartbeat: &heartbeatState{}}
	conn.opened.Store(true)
	if !conn.heartbeat.beginPing(time.Now(), 1) {
		t.Fatal("heartbeat ping did not start")
	}
	if err := conn.sendHeartbeatPing(time.Now()); err != nil || raw.writes != 0 {
		t.Fatalf("outstanding heartbeat ping = %v, writes = %d", err, raw.writes)
	}
	writeErr := errors.New("heartbeat close write failed")
	failedRaw := newScriptedConn()
	failedRaw.writeErr = writeErr
	failed := &Conn{raw: failedRaw, config: testServerConfig(NewServer(Options{}))}
	failed.opened.Store(true)
	failed.sendHeartbeatClose(1001, "timeout")
	if !failed.closing.Load() || failedRaw.closes != 1 {
		t.Fatalf("failed heartbeat close state = closing:%v closes:%d", failed.closing.Load(), failedRaw.closes)
	}
	if info := failed.closeInfo(); !errors.Is(info.Err, writeErr) {
		t.Fatalf("heartbeat close error = %v, want %v", info.Err, writeErr)
	}
}

func TestHandshakeTimeoutClosesUnresponsiveConnection(t *testing.T) {
	raw := &writeProbeConn{closed: make(chan struct{})}
	conn := &Conn{raw: raw, config: testServerConfig(&Server{Options: Options{HandshakeTimeout: 5 * time.Millisecond}})}
	conn.startHandshakeTimer(conn.config.handshakeTimeout)
	select {
	case <-raw.closed:
	case <-time.After(time.Second):
		t.Fatal("handshake timeout did not close transport")
	}
	if conn.opened.Load() {
		t.Fatal("timed out handshake was marked open")
	}
	if _, err := conn.consumeHandshake([]byte("late")); !errors.Is(err, ErrClosed) {
		t.Fatalf("timed-out handshake accepted more data: %v", err)
	}
}

func TestServerServeAfterCloseReportsClosed(t *testing.T) {
	server := &Server{Options: Options{HeartbeatInterval: time.Hour}}
	if err := server.Close(nil); err != nil {
		t.Fatal(err)
	}
	if err := server.Serve("127.0.0.1:0"); !errors.Is(err, ErrClosed) {
		t.Fatalf("Serve after Close error = %v, want %v", err, ErrClosed)
	}
}

func TestServerLifecycleEdges(t *testing.T) {
	server := &Server{}
	if err := server.Close(nil); err != nil {
		t.Fatalf("Server.Close with nil Events = %v", err)
	}
	invalid := newScriptedConn()
	invalid.userdata = "invalid"
	if err := server.onData(invalid); !errors.Is(err, ErrClosed) {
		t.Fatalf("invalid onData error = %v", err)
	}
	server.onClose(invalid, io.EOF)
	unopenedRaw := newScriptedConn()
	unopened := &Conn{raw: unopenedRaw, config: testServerConfig(server)}
	unopenedRaw.userdata = unopened
	unopenedErr := errors.New("handshake failed")
	server.onClose(unopenedRaw, unopenedErr)
	if !unopened.closed.Load() {
		t.Fatal("unopened server connection was not marked closed")
	}
	openedRaw := newScriptedConn()
	handler := &recordingHandler{}
	opened := &Conn{raw: openedRaw, config: testServerConfig(server), handler: handler}
	opened.opened.Store(true)
	openedRaw.userdata = opened
	server.onOutbound(openedRaw, 8)
	server.onClose(openedRaw, io.EOF)
	server.onClose(openedRaw, io.EOF)
	handler.mu.Lock()
	defer handler.mu.Unlock()
	if got := strings.Join(handler.events, ","); got != "close" {
		t.Fatalf("close events = %q", got)
	}
}

func TestServerReadySignalsServeStart(t *testing.T) {
	server := NewServer(Options{})
	select {
	case <-server.Ready():
		t.Fatal("Ready closed before Serve")
	case <-time.After(20 * time.Millisecond):
	}
	done := make(chan error, 1)
	go func() {
		done <- server.Serve("tcp://127.0.0.1:0")
	}()
	select {
	case <-server.Ready():
	case <-time.After(5 * time.Second):
		t.Fatal("Ready never closed though Serve started the transport")
	}
	if err := server.Close(nil); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, ErrClosed) {
			t.Fatalf("Serve = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return")
	}
}

func wssConfigs(t testing.TB) (serverTLS *tls.Config, clientTLS *tls.Config) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), crand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "uws wss test"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(crand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	serverTLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}}}
	clientTLS = &tls.Config{ServerName: "localhost", RootCAs: pool}
	return serverTLS, clientTLS
}

type wssRecorder struct {
	opened chan *Conn
	got    chan string
	closed chan error
}

func newWSSRecorder() *wssRecorder {
	return &wssRecorder{opened: make(chan *Conn, 4), got: make(chan string, 16), closed: make(chan error, 4)}
}

func (h *wssRecorder) OnOpen(c *Conn) {
	h.opened <- c
}

func (h *wssRecorder) OnMessage(c *Conn, m Message) {
	h.got <- string(m.Payload)
	_ = c.SendText(m.Text())
}

func (h *wssRecorder) OnClose(_ *Conn, event CloseEvent) {
	h.closed <- event.Err
}

func startWSS(t *testing.T, serverTLS *tls.Config) string {
	t.Helper()
	server, addr, serveDone := startTestServer(t, func() *Server {
		return NewServer(Options{Handler: newWSSRecorder(), TLSConfig: serverTLS})
	})
	t.Cleanup(func() {
		_ = server.Close(nil)
		<-serveDone
	})
	return addr
}

func wssWait[T any](t *testing.T, c chan T, what string) T {
	t.Helper()
	select {
	case v := <-c:
		return v
	case <-time.After(testIOTimeout()):
		t.Fatalf("no %s within %v", what, testIOTimeout())
		return *new(T)
	}
}

func TestWSSNeedsATLSConfig(t *testing.T) {
	dialer := &Dialer{}
	t.Cleanup(func() {
		_ = dialer.Close(nil)
	})
	err := dialer.Dial(context.Background(), "wss://127.0.0.1:1/", nil)
	if !errors.Is(err, ErrInvalidOptions) {
		t.Fatalf("Dial wss without a TLSConfig = %v, want ErrInvalidOptions", err)
	}
}

func TestWSSHeartbeatClosesBackpressuredPeer(t *testing.T) {
	serverTLS, clientTLS := wssConfigs(t)
	listener, err := tls.Listen("tcp", "127.0.0.1:0", serverTLS)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	hold := make(chan struct{})
	defer close(hold)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		reader := bufio.NewReader(conn)
		key := ""
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				return
			}
			trimmed := strings.TrimSpace(line)
			if len(trimmed) > len("sec-websocket-key:") && strings.EqualFold(trimmed[:len("sec-websocket-key:")], "sec-websocket-key:") {
				key = strings.TrimSpace(trimmed[len("sec-websocket-key:"):])
			}
			if line == "\r\n" {
				break
			}
		}
		_, _ = fmt.Fprintf(conn, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n", testAcceptKey(key))
		<-hold
		_, _ = io.Copy(io.Discard, reader)
	}()
	watcher := &connCloseWatcher{closeWatcher: newCloseWatcher(), conns: make(chan *Conn, 1)}
	dialer := NewDialer(Options{Handler: watcher, Pollers: 1, HeartbeatInterval: 10 * time.Millisecond, HeartbeatTimeout: 60 * time.Millisecond, CloseTimeout: 100 * time.Millisecond, MaxOutboundBuffered: 64 << 10, TLSConfig: clientTLS})
	t.Cleanup(func() {
		_ = dialer.Close(nil)
	})
	if err := dialer.Dial(context.Background(), "wss://"+listener.Addr().String()+"/", nil); err != nil {
		t.Fatal(err)
	}
	conn := wssWait(t, watcher.conns, "OnOpen")
	big := make([]byte, 16<<10)
	for i := 0; i < 64; i++ {
		if err := conn.SendBinary(big); err != nil {
			break
		}
	}
	if event := wssWait(t, watcher.closed, "the heartbeat close"); event.Code != 1001 || event.Reason != "heartbeat timeout" {
		t.Fatalf("closed with %d %q, want the heartbeat timeout", event.Code, event.Reason)
	}
}

type connCloseWatcher struct {
	*closeWatcher
	conns chan *Conn
}

func (w *connCloseWatcher) OnOpen(c *Conn) {
	w.closeWatcher.OnOpen(c)
	w.conns <- c
}

func TestWSSDialVerifiesTheURLHost(t *testing.T) {
	serverTLS, clientTLS := wssConfigs(t)
	addr := startWSS(t, serverTLS)
	clientTLS.ServerName = ""
	client := newWSSRecorder()
	dialer := &Dialer{Options: Options{Handler: client, TLSConfig: clientTLS}}
	t.Cleanup(func() {
		_ = dialer.Close(nil)
	})
	ctx, cancel := context.WithTimeout(context.Background(), testIOTimeout())
	defer cancel()
	if err := dialer.Dial(ctx, "wss://"+addr+"/", nil); err != nil {
		t.Fatal(err)
	}
	conn := wssWait(t, client.opened, "client open over wss")
	state, ok := uio.LayerOf(conn.raw).(interface{ State() tls.ConnectionState })
	if !ok {
		t.Fatal("the connection runs no TLS layer")
	}
	if chains := state.State().VerifiedChains; len(chains) == 0 {
		t.Fatal("the server's certificate was not verified")
	}
}

func TestWSSServerAdoptsHTTPUpgradesWithoutTLS(t *testing.T) {
	serverTLS, _ := wssConfigs(t)
	server := NewServer(Options{Handler: newWSSRecorder(), TLSConfig: serverTLS, Pollers: 1})
	serveWithoutListener(t, server)
	httpServer := httptest.NewServer(server)
	t.Cleanup(httpServer.Close)
	client := quietRecorder{newWSSRecorder()}
	dialer := &Dialer{Options: Options{Handler: client}}
	t.Cleanup(func() {
		_ = dialer.Close(nil)
	})
	ctx, cancel := context.WithTimeout(context.Background(), testIOTimeout())
	defer cancel()
	if err := dialer.Dial(ctx, "ws"+strings.TrimPrefix(httpServer.URL, "http"), nil); err != nil {
		t.Fatal(err)
	}
	conn := wssWait(t, client.opened, "client open through net/http")
	if err := conn.SendText("adopted"); err != nil {
		t.Fatal(err)
	}
	if got := wssWait(t, client.got, "the adopted connection's echo"); got != "adopted" {
		t.Fatalf("echo = %q", got)
	}
}

type quietRecorder struct{ *wssRecorder }

func (h quietRecorder) OnMessage(_ *Conn, m Message) {
	h.got <- string(m.Payload)
}

func TestWSSAuthorizeSeesTLS(t *testing.T) {
	serverTLS, clientTLS := wssConfigs(t)
	seen := make(chan string, 1)
	server := newWSSRecorder()
	srv, addr, done := startTestServer(t, func() *Server {
		s := NewServer(Options{Handler: server, TLSConfig: serverTLS})
		s.Authorize = func(r *http.Request) (any, error) {
			if r.TLS == nil {
				seen <- "no TLS"
			} else {
				seen <- r.TLS.ServerName
			}
			return nil, nil
		}
		return s
	})
	t.Cleanup(func() {
		stopTestServer(t, srv, done)
	})
	dialer := &Dialer{Options: Options{Handler: quietRecorder{newWSSRecorder()}, TLSConfig: clientTLS}}
	t.Cleanup(func() {
		_ = dialer.Close(nil)
	})
	ctx, cancel := context.WithTimeout(context.Background(), testIOTimeout())
	defer cancel()
	if err := dialer.Dial(ctx, "wss://"+addr+"/", nil); err != nil {
		t.Fatal(err)
	}
	if got := wssWait(t, seen, "Authorize"); got != "localhost" {
		t.Fatalf("Authorize saw server name %q, want localhost", got)
	}
	conn := wssWait(t, server.opened, "the server's open")
	if state, ok := conn.TLSState(); !ok || !state.HandshakeComplete {
		t.Fatalf("TLSState = %v, %v", state.HandshakeComplete, ok)
	}
}

func signal[T any](ch chan<- T, v T) {
	select {
	case ch <- v:
	default:
	}
}
