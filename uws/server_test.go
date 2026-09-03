package uws

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/petermattis/goid"
	"github.com/urpc/uio"
)

func TestConfigureWriteBuffer(t *testing.T) {
	events := &uio.Events{}
	configureWriteBuffer(events)
	if got := events.WriteBufferedThreshold; got != defaultWriteBufferedThreshold {
		t.Fatalf("default WriteBufferedThreshold = %d, want %d", got, defaultWriteBufferedThreshold)
	}

	const configured = 32 << 10
	events.WriteBufferedThreshold = configured
	configureWriteBuffer(events)
	if got := events.WriteBufferedThreshold; got != configured {
		t.Fatalf("configured WriteBufferedThreshold = %d, want %d", got, configured)
	}

	const disabled = -1
	events.WriteBufferedThreshold = disabled
	configureWriteBuffer(events)
	if got := events.WriteBufferedThreshold; got != disabled {
		t.Fatalf("disabled WriteBufferedThreshold = %d, want %d", got, disabled)
	}
}

func TestServerServeRejectsMultipleAddressesBeforeStart(t *testing.T) {
	server := NewServer(nil)
	err := server.Serve("127.0.0.1:0", "127.0.0.1:0")
	if !errors.Is(err, uio.ErrTooManyListenAddresses) {
		t.Fatalf("Serve error = %v, want %v", err, uio.ErrTooManyListenAddresses)
	}
	if server.started.Load() || server.ready.Load() || server.config != nil {
		t.Fatal("rejected Serve initialized the Server")
	}
}

func TestHeartbeatCanBeRestarted(t *testing.T) {
	server := &Server{HeartbeatInterval: time.Millisecond}
	server.startHeartbeat(testServerConfig(server))
	server.startHeartbeat(testServerConfig(server))
	server.stopHeartbeat()
	server.startHeartbeat(testServerConfig(server))
	server.stopHeartbeat()

	// Give both ticker goroutines a chance to observe their stop channels.
	time.Sleep(2 * time.Millisecond)
}

func TestHandshakeTimeoutClosesUnresponsiveConnection(t *testing.T) {
	raw := &writeProbeConn{closed: make(chan struct{})}
	conn := &Conn{raw: raw, config: testServerConfig(&Server{HandshakeTimeout: 5 * time.Millisecond})}
	conn.startHandshakeTimer(conn.config.handshakeTimeout)
	select {
	case <-raw.closed:
	case <-time.After(time.Second):
		t.Fatal("handshake timeout did not close transport")
	}
	if conn.opened.Load() {
		t.Fatal("timed out handshake was marked open")
	}
	if err := conn.consumeHandshake([]byte("late")); !errors.Is(err, ErrClosed) {
		t.Fatalf("timed-out handshake accepted more data: %v", err)
	}
}

func TestCompletedHandshakeCancelsTimeout(t *testing.T) {
	raw := &writeProbeConn{closed: make(chan struct{})}
	conn := &Conn{raw: raw, config: testServerConfig(&Server{HandshakeTimeout: 5 * time.Millisecond})}
	conn.startHandshakeTimer(conn.config.handshakeTimeout)
	if !conn.markOpened() {
		t.Fatal("markOpened returned false")
	}
	select {
	case <-raw.closed:
		t.Fatal("completed handshake was closed by stale timeout")
	case <-time.After(30 * time.Millisecond):
	}
	conn.stopHandshakeTimer()
}

func TestServerCloseStopsHeartbeatBeforeEvents(t *testing.T) {
	server := &Server{Events: &uio.Events{}, HeartbeatInterval: time.Hour}
	server.startHeartbeat(testServerConfig(server))
	if err := server.Close(nil); err != nil {
		t.Fatal(err)
	}
	server.stopHeartbeat()
	if err := server.Serve("127.0.0.1:0"); !errors.Is(err, ErrClosed) {
		t.Fatalf("Serve after Close error = %v, want %v", err, ErrClosed)
	}
}

func TestServerServeRunsEventsOnCallerGoroutine(t *testing.T) {
	server := NewServer(nil)
	server.Events = &uio.Events{Pollers: 1}
	caller := make(chan int64, 1)
	onStart := make(chan int64, 1)
	server.Events.OnStart = func(events *uio.Events) {
		onStart <- goid.Get()
		_ = events.Close(nil)
	}
	done := make(chan error, 1)
	go func() {
		caller <- goid.Get()
		done <- server.Serve()
	}()
	var callerID, startID int64
	select {
	case callerID = <-caller:
	case <-time.After(time.Second):
		t.Fatal("Serve caller did not start")
	}
	select {
	case startID = <-onStart:
	case <-time.After(time.Second):
		t.Fatal("Events.OnStart was not called")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Serve did not return")
	}
	if startID != callerID {
		t.Fatalf("OnStart goroutine = %d, Serve caller = %d", startID, callerID)
	}
}

func TestServerHeartbeatClosesSilentPeer(t *testing.T) {
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := probe.Addr().String()
	_ = probe.Close()

	handler := &heartbeatHandler{closed: make(chan CloseEvent, 1)}
	server := NewServer(handler)
	server.HeartbeatInterval = 10 * time.Millisecond
	server.HeartbeatTimeout = 25 * time.Millisecond
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
	request := "GET / HTTP/1.1\r\nHost: " + addr + "\r\n" +
		"Connection: Upgrade\r\nUpgrade: websocket\r\n" +
		"Sec-WebSocket-Version: 13\r\nSec-WebSocket-Key: " + testKey + "\r\n\r\n"
	if _, err = client.Write([]byte(request)); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(client)
	if line, err := reader.ReadString('\n'); err != nil || !strings.HasPrefix(line, "HTTP/1.1 101 ") {
		t.Fatalf("handshake response = %q, %v", line, err)
	}
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if line == "\r\n" {
			break
		}
	}
	select {
	case info := <-handler.closed:
		if info.Code != 1001 {
			t.Fatalf("heartbeat close code = %d, want 1001", info.Code)
		}
	case <-time.After(testIOTimeout()):
		t.Fatal("heartbeat did not close silent peer")
	}
}

func TestServerHeartbeatKeepsResponsiveClient(t *testing.T) {
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := probe.Addr().String()
	_ = probe.Close()

	serverHandler := &echoHandler{open: make(chan struct{}), closed: make(chan struct{}), message: make(chan Message, 1)}
	server := NewServer(serverHandler)
	server.HeartbeatInterval = 10 * time.Millisecond
	server.HeartbeatTimeout = 250 * time.Millisecond
	server.Events = &uio.Events{Pollers: 1, MaxBufferSize: 4 << 10}
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(addr) }()

	clientHandler := &clientHandler{open: make(chan struct{}), closed: make(chan struct{}), message: make(chan Message, 1)}
	dialer := NewDialer()
	dialer.Events = &uio.Events{Pollers: 1, MaxBufferSize: 4 << 10}
	var client *Conn
	for deadline := time.Now().Add(testIOTimeout()); client == nil && time.Now().Before(deadline); {
		client, err = dialer.Dial(context.Background(), "ws://"+addr+"/", clientHandler)
		if err != nil {
			time.Sleep(time.Millisecond)
		}
	}
	if client == nil {
		_ = server.Close(nil)
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = client.Close(1000, "")
		_ = dialer.Close(nil)
		_ = server.Close(nil)
		select {
		case <-serveDone:
		case <-time.After(testIOTimeout()):
			t.Error("server did not stop")
		}
	})
	select {
	case <-clientHandler.open:
	case <-time.After(testIOTimeout()):
		t.Fatal("client OnOpen was not called")
	}

	time.Sleep(600 * time.Millisecond)
	select {
	case <-serverHandler.closed:
		t.Fatal("responsive heartbeat client was closed")
	default:
	}
	if err = client.SendText([]byte("alive")); err != nil {
		t.Fatal(err)
	}
	select {
	case message := <-clientHandler.message:
		if string(message.Payload) != "world" {
			t.Fatalf("heartbeat echo = %q", message.Payload)
		}
	case <-time.After(testIOTimeout()):
		t.Fatal("responsive client stopped receiving messages")
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
	opened.pendingBytes.Store(8)
	openedRaw.userdata = opened
	server.connections.Store(opened, opened)
	server.onOutbound(openedRaw, 8)
	server.onClose(openedRaw, io.EOF)
	server.onClose(openedRaw, io.EOF)
	if _, exists := server.connections.Load(opened); exists {
		t.Fatal("closed connection remained in server map")
	}
	handler.mu.Lock()
	defer handler.mu.Unlock()
	if got := strings.Join(handler.events, ","); got != "close" {
		t.Fatalf("close events = %q", got)
	}
}

func TestServerPreservesDisabledOutboundLimitDuringServe(t *testing.T) {
	events := &uio.Events{}
	if err := events.Close(nil); err != nil {
		t.Fatal(err)
	}
	server := NewServer(nil)
	server.Events = events
	server.MaxOutboundBytes = -1
	if err := server.Serve(); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("Serve() error = %v, want %v", err, net.ErrClosed)
	}
	if server.MaxOutboundBytes != -1 {
		t.Fatalf("MaxOutboundBytes = %d, want -1", server.MaxOutboundBytes)
	}
	if got := (&Conn{config: testServerConfig(server)}).maxOutboundBytes(); got != -1 {
		t.Fatalf("connection outbound limit = %d, want -1", got)
	}
}
