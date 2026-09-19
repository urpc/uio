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

type heartbeatWriteProbe struct {
	*scriptedConn
	written chan struct{}
}

func newHeartbeatWriteProbe() *heartbeatWriteProbe {
	return &heartbeatWriteProbe{scriptedConn: newScriptedConn(), written: make(chan struct{}, 1)}
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

func TestServerServeAcceptsMultipleAddresses(t *testing.T) {
	server := NewServer(nil)
	server.Events = &uio.Events{Pollers: 1}
	server.Events.OnStart = func(*uio.Events) { _ = server.Close(nil) }
	if err := server.Serve("127.0.0.1:0", "127.0.0.1:0"); err != nil {
		t.Fatalf("Serve with multiple addresses: %v", err)
	}
	if !server.started.Load() || server.ready.Load() {
		t.Fatal("Serve did not start and stop the Server")
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

func TestHeartbeatScanSkipsBusyWriterAndProcessesOthers(t *testing.T) {
	server := NewServer(nil)
	server.HeartbeatInterval = time.Millisecond
	server.HeartbeatTimeout = 20 * time.Millisecond
	config := testServerConfig(server)
	newConn := func(raw uio.Conn) *Conn {
		conn := &Conn{raw: raw, config: config, heartbeat: &heartbeatState{}}
		conn.opened.Store(true)
		return conn
	}
	busy := newConn(newScriptedConn())
	responsiveRaw := newHeartbeatWriteProbe()
	responsive := newConn(responsiveRaw)
	writer, err := busy.BeginMessage(BinaryMessage)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = writer.fail(ErrClosed) })
	server.connections.Store(busy, busy)
	server.connections.Store(responsive, responsive)
	done := make(chan bool, 1)
	go func() {
		done <- scanHeartbeat(&server.connections, time.Now(), server.HeartbeatTimeout, make(chan struct{}))
	}()
	select {
	case completed := <-done:
		if !completed {
			t.Fatal("heartbeat scan stopped unexpectedly")
		}
	case <-time.After(testIOTimeout()):
		t.Fatal("heartbeat scan waited for a busy Writer")
	}
	select {
	case <-responsiveRaw.written:
	case <-time.After(testIOTimeout()):
		t.Fatal("responsive connection did not receive heartbeat")
	}
	if busy.heartbeat.pingOutstanding.Load() {
		t.Fatal("busy connection recorded a ping that was not sent")
	}
}

func TestHeartbeatTimeoutDoesNotWaitForBusyWriter(t *testing.T) {
	server := NewServer(nil)
	server.HeartbeatInterval = time.Millisecond
	server.HeartbeatTimeout = 5 * time.Millisecond
	server.CloseTimeout = 5 * time.Millisecond
	config := testServerConfig(server)
	busyRaw := &writeProbeConn{closed: make(chan struct{})}
	busy := &Conn{raw: busyRaw, config: config, heartbeat: &heartbeatState{}}
	busy.opened.Store(true)
	timedOutAt := time.Now().Add(-time.Second)
	busy.heartbeat.beginPing(timedOutAt, 1)
	busy.heartbeat.mu.Lock()
	busy.heartbeat.pingSentAt = timedOutAt.UnixNano()
	busy.heartbeat.mu.Unlock()
	responsiveRaw := newHeartbeatWriteProbe()
	responsive := &Conn{raw: responsiveRaw, config: config, heartbeat: &heartbeatState{}}
	responsive.opened.Store(true)
	writer, err := busy.BeginMessage(BinaryMessage)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = writer.fail(ErrClosed) })
	server.connections.Store(busy, busy)
	server.connections.Store(responsive, responsive)
	done := make(chan bool, 1)
	go func() {
		done <- scanHeartbeat(&server.connections, time.Now(), server.HeartbeatTimeout, make(chan struct{}))
	}()
	select {
	case <-done:
	case <-time.After(testIOTimeout()):
		t.Fatal("heartbeat timeout waited for a busy Writer")
	}
	select {
	case <-responsiveRaw.written:
	case <-time.After(testIOTimeout()):
		t.Fatal("heartbeat timeout prevented later connections from being processed")
	}
	select {
	case <-busyRaw.closed:
	case <-time.After(testIOTimeout()):
		t.Fatal("busy timed-out connection did not close within CloseTimeout")
	}
	if info := busy.closeInfo(); info.Code != 1001 || info.Reason != "heartbeat timeout" {
		t.Fatalf("heartbeat close info = %+v", info)
	}
}

func TestHeartbeatQueuedPingHasFiniteTimeout(t *testing.T) {
	server := NewServer(nil)
	server.HeartbeatInterval = time.Millisecond
	server.HeartbeatTimeout = 20 * time.Millisecond
	server.CloseTimeout = 5 * time.Millisecond
	config := testServerConfig(server)
	raw := newHeartbeatWriteProbe()
	raw.closed = make(chan struct{})
	conn := &Conn{raw: raw, config: config, heartbeat: &heartbeatState{}}
	conn.opened.Store(true)
	queuedAt := time.Now()
	attempted, err := conn.tryHeartbeatPing(queuedAt)
	if err != nil || !attempted {
		t.Fatalf("queue heartbeat ping: attempted=%v err=%v", attempted, err)
	}
	conn.heartbeat.mu.Lock()
	pingTarget := conn.heartbeat.pingTarget
	conn.heartbeat.mu.Unlock()
	if conn.pendingBytes.Load() == 0 || pingTarget == 0 {
		t.Fatal("heartbeat ping was not tracked in the outbound queue")
	}
	server.connections.Store(conn, conn)
	if !scanHeartbeat(&server.connections, queuedAt.Add(2*server.HeartbeatTimeout), server.HeartbeatTimeout, make(chan struct{})) {
		t.Fatal("heartbeat scan stopped unexpectedly")
	}
	select {
	case <-raw.closed:
	case <-time.After(testIOTimeout()):
		t.Fatal("connection with a permanently queued ping did not close")
	}
}

func TestHeartbeatPongTimeoutStartsWhenPingIsWritten(t *testing.T) {
	server := NewServer(nil)
	server.HeartbeatInterval = time.Millisecond
	server.HeartbeatTimeout = 20 * time.Millisecond
	server.CloseTimeout = 5 * time.Millisecond
	config := testServerConfig(server)
	raw := newHeartbeatWriteProbe()
	raw.closed = make(chan struct{})
	conn := &Conn{raw: raw, config: config, heartbeat: &heartbeatState{}}
	conn.opened.Store(true)
	queuedAt := time.Now()
	attempted, err := conn.tryHeartbeatPing(queuedAt)
	if err != nil || !attempted {
		t.Fatalf("queue heartbeat ping: attempted=%v err=%v", attempted, err)
	}
	pending := conn.pendingBytes.Load()
	conn.releaseOutbound(int(pending))
	conn.heartbeat.mu.Lock()
	sentAt := conn.heartbeat.pingSentAt
	conn.heartbeat.mu.Unlock()
	if sentAt == 0 {
		t.Fatal("heartbeat ping was not marked written when its queue position retired")
	}
	// Later business writes must not move the fixed Pong deadline.
	if err := conn.SendBinary([]byte("later")); err != nil {
		t.Fatal(err)
	}
	if conn.pendingBytes.Load() == 0 {
		t.Fatal("later business write did not remain queued")
	}
	server.connections.Store(conn, conn)
	sent := time.Unix(0, sentAt)
	scanHeartbeat(&server.connections, sent.Add(server.HeartbeatTimeout/2), server.HeartbeatTimeout, make(chan struct{}))
	if conn.closing.Load() {
		t.Fatal("connection timed out before the Pong deadline")
	}
	scanHeartbeat(&server.connections, sent.Add(2*server.HeartbeatTimeout), server.HeartbeatTimeout, make(chan struct{}))
	select {
	case <-raw.closed:
	case <-time.After(testIOTimeout()):
		t.Fatal("later outbound traffic postponed the Pong timeout")
	}
}

func TestHeartbeatCancelRequiresMatchingNonce(t *testing.T) {
	heartbeat := &heartbeatState{}
	const nonce = uint64(42)
	if !heartbeat.beginPing(time.Now(), nonce) {
		t.Fatal("heartbeat ping did not start")
	}
	heartbeat.cancelPing(nonce + 1)
	if !heartbeat.pingOutstanding.Load() {
		t.Fatal("mismatched nonce canceled the heartbeat ping")
	}
	heartbeat.cancelPing(nonce)
	if heartbeat.pingOutstanding.Load() {
		t.Fatal("matching nonce did not cancel the heartbeat ping")
	}
	heartbeat.mu.Lock()
	defer heartbeat.mu.Unlock()
	if heartbeat.pingQueuedAt != 0 || heartbeat.pingSentAt != 0 || heartbeat.pingNonce != 0 || heartbeat.pingTarget != 0 {
		t.Fatal("canceled heartbeat retained ping state")
	}
}

func TestHeartbeatTryPaths(t *testing.T) {
	if attempted, err := (&Conn{}).tryHeartbeatPing(time.Now()); attempted || !errors.Is(err, ErrClosed) {
		t.Fatalf("unavailable heartbeat ping = attempted:%v err:%v", attempted, err)
	}

	conn := &Conn{
		raw:       newScriptedConn(),
		config:    testServerConfig(NewServer(nil)),
		heartbeat: &heartbeatState{},
	}
	conn.opened.Store(true)
	if !conn.heartbeat.beginPing(time.Now(), 1) {
		t.Fatal("heartbeat ping did not start")
	}
	if attempted, err := conn.tryHeartbeatPing(time.Now()); attempted || err != nil {
		t.Fatalf("outstanding heartbeat ping = attempted:%v err:%v", attempted, err)
	}

	writeErr := errors.New("heartbeat close write failed")
	raw := newScriptedConn()
	raw.writeErr = writeErr
	failed := &Conn{raw: raw, config: testServerConfig(NewServer(nil))}
	failed.opened.Store(true)
	if !failed.tryHeartbeatClose(1001, "timeout") {
		t.Fatal("heartbeat close did not acquire write ownership")
	}
	if !failed.closing.Load() || raw.closes != 1 {
		t.Fatalf("failed heartbeat close state = closing:%v closes:%d", failed.closing.Load(), raw.closes)
	}
	if info := failed.closeInfo(); !errors.Is(info.Err, writeErr) {
		t.Fatalf("heartbeat close error = %v, want %v", info.Err, writeErr)
	}
}

func TestHeartbeatWorkerStopsWithBusyWriter(t *testing.T) {
	server := NewServer(nil)
	server.HeartbeatInterval = time.Millisecond
	server.HeartbeatTimeout = time.Second
	config := testServerConfig(server)
	busy := &Conn{raw: newScriptedConn(), config: config, heartbeat: &heartbeatState{}}
	busy.opened.Store(true)
	writer, err := busy.BeginMessage(BinaryMessage)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = writer.fail(ErrClosed) })
	server.connections.Store(busy, busy)
	server.startHeartbeat(config)
	time.Sleep(2 * server.HeartbeatInterval)
	done := make(chan struct{})
	go func() {
		server.stopHeartbeat()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(testIOTimeout()):
		t.Fatal("heartbeat worker did not stop while Writer held the write lock")
	}
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
