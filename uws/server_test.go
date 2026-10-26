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
	"github.com/urpc/uio/uws/internal/frame"
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

func TestResolveWriteCoalesce(t *testing.T) {
	opts, err := (Options{}).resolve()
	if err != nil {
		t.Fatal(err)
	}
	if got := opts.WriteCoalesce; got != defaultWriteBufferedThreshold {
		t.Fatalf("default WriteCoalesce = %d, want %d", got, defaultWriteBufferedThreshold)
	}

	const configured = 32 << 10
	opts, err = Options{WriteCoalesce: configured}.resolve()
	if err != nil {
		t.Fatal(err)
	}
	if got := opts.WriteCoalesce; got != configured {
		t.Fatalf("configured WriteCoalesce = %d, want %d", got, configured)
	}

	opts, err = Options{WriteCoalesce: -1}.resolve()
	if err != nil {
		t.Fatal(err)
	}
	if got := opts.WriteCoalesce; got != -1 {
		t.Fatalf("disabled WriteCoalesce = %d, want -1", got)
	}
}

func TestServerServeAcceptsMultipleAddresses(t *testing.T) {
	server := NewServer(Options{})
	server.Options.Pollers = 1
	server.startHook = func() { _ = server.Close(nil) }
	if err := server.Serve("127.0.0.1:0", "127.0.0.1:0"); err != nil {
		t.Fatalf("Serve with multiple addresses: %v", err)
	}
	if !server.started.Load() || server.ready.Load() {
		t.Fatal("Serve did not start and stop the Server")
	}
}

func TestHeartbeatTimeoutClosesAndPingsOthers(t *testing.T) {
	server := NewServer(Options{})
	server.HeartbeatInterval = time.Millisecond
	server.HeartbeatTimeout = 5 * time.Millisecond
	server.CloseTimeout = 5 * time.Millisecond
	config := testServerConfig(server)
	expiredRaw := &writeProbeConn{closed: make(chan struct{})}
	expired := &Conn{raw: expiredRaw, config: config, heartbeat: &heartbeatState{}}
	expired.opened.Store(true)
	timedOutAt := time.Now().Add(-time.Second)
	expired.heartbeat.beginPing(timedOutAt, 1)
	expired.heartbeat.mu.Lock()
	expired.heartbeat.pingSentAt = timedOutAt.UnixNano()
	expired.heartbeat.mu.Unlock()
	responsiveRaw := newHeartbeatWriteProbe()
	responsive := &Conn{raw: responsiveRaw, config: config, heartbeat: &heartbeatState{}}
	responsive.opened.Store(true)
	responsive.beginHeartbeat()
	now := time.Now().Add(time.Millisecond) // one interval in: the first ping is due
	expired.heartbeatTick(now)
	responsive.heartbeatTick(now)
	select {
	case <-responsiveRaw.written:
	case <-time.After(testIOTimeout()):
		t.Fatal("heartbeat timeout prevented later connections from being processed")
	}
	select {
	case <-expiredRaw.closed:
	case <-time.After(testIOTimeout()):
		t.Fatal("timed-out connection did not close within CloseTimeout")
	}
	if info := expired.closeInfo(); info.Code != 1001 || info.Reason != "heartbeat timeout" {
		t.Fatalf("heartbeat close info = %+v", info)
	}
}

func TestHeartbeatQueuedPingHasFiniteTimeout(t *testing.T) {
	server := NewServer(Options{})
	server.HeartbeatInterval = time.Millisecond
	server.HeartbeatTimeout = 20 * time.Millisecond
	server.CloseTimeout = 5 * time.Millisecond
	config := testServerConfig(server)
	raw := newHeartbeatWriteProbe()
	raw.closed = make(chan struct{})
	conn := &Conn{raw: raw, config: config, heartbeat: &heartbeatState{}}
	conn.opened.Store(true)
	queuedAt := time.Now()
	if err := conn.sendHeartbeatPing(queuedAt); err != nil || !conn.heartbeat.pingOutstanding.Load() {
		t.Fatalf("queue heartbeat ping: outstanding=%v err=%v", conn.heartbeat.pingOutstanding.Load(), err)
	}
	conn.heartbeat.mu.Lock()
	pingTarget := conn.heartbeat.pingTarget
	conn.heartbeat.mu.Unlock()
	if conn.writes.close.pendingBytes.Load() == 0 || pingTarget == 0 {
		t.Fatal("heartbeat ping was not tracked in the outbound queue")
	}
	conn.heartbeatTick(queuedAt.Add(2 * server.HeartbeatTimeout))
	select {
	case <-raw.closed:
	case <-time.After(testIOTimeout()):
		t.Fatal("connection with a permanently queued ping did not close")
	}
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
		if accepted, retired := conn.heartbeat.outboundAccepted.Load(), conn.heartbeat.outboundRetired.Load(); accepted != 0 || retired != 0 {
			t.Fatalf("attempt %d: rejected Ping counted as sent: accepted=%d retired=%d", i, accepted, retired)
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

func TestHeartbeatRetryKeepsOriginalSendDeadline(t *testing.T) {
	server := NewServer(Options{})
	server.HeartbeatTimeout = 20 * time.Millisecond
	raw := newScriptedConn()
	raw.writeErr = uio.ErrOutboundOverflow
	conn := &Conn{raw: raw, config: testServerConfig(server), heartbeat: &heartbeatState{}}
	conn.opened.Store(true)
	conn.beginHeartbeat()
	first := time.Now()
	conn.heartbeatTick(first)
	raw.writeErr = nil
	conn.heartbeatTick(first.Add(server.HeartbeatTimeout / 2))
	conn.heartbeat.mu.Lock()
	queuedAt := conn.heartbeat.pingQueuedAt
	conn.heartbeat.mu.Unlock()
	if queuedAt != first.UnixNano() || !conn.heartbeat.pingOutstanding.Load() {
		t.Fatalf("successful retry queued at %d, want first stalled time %d", queuedAt, first.UnixNano())
	}
	pending := conn.writes.close.pendingBytes.Load()
	if pending == 0 {
		t.Fatal("successful retry did not queue a Ping")
	}
	conn.releaseOutbound(int(pending))
	conn.heartbeat.mu.Lock()
	sentAt, stalledAt := conn.heartbeat.pingSentAt, conn.heartbeat.sendStalledAt
	conn.heartbeat.mu.Unlock()
	if sentAt == 0 || stalledAt != 0 {
		t.Fatalf("sent Ping did not switch deadlines: sent=%d stalled=%d", sentAt, stalledAt)
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

func TestHeartbeatPongTimeoutStartsWhenPingIsWritten(t *testing.T) {
	server := NewServer(Options{})
	server.HeartbeatInterval = time.Millisecond
	server.HeartbeatTimeout = 20 * time.Millisecond
	server.CloseTimeout = 5 * time.Millisecond
	config := testServerConfig(server)
	raw := newHeartbeatWriteProbe()
	raw.closed = make(chan struct{})
	conn := &Conn{raw: raw, config: config, heartbeat: &heartbeatState{}}
	conn.opened.Store(true)
	queuedAt := time.Now()
	if err := conn.sendHeartbeatPing(queuedAt); err != nil || !conn.heartbeat.pingOutstanding.Load() {
		t.Fatalf("queue heartbeat ping: outstanding=%v err=%v", conn.heartbeat.pingOutstanding.Load(), err)
	}
	pending := conn.writes.close.pendingBytes.Load()
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
	if conn.writes.close.pendingBytes.Load() == 0 {
		t.Fatal("later business write did not remain queued")
	}
	sent := time.Unix(0, sentAt)
	conn.heartbeatTick(sent.Add(server.HeartbeatTimeout / 2))
	if conn.closing.Load() {
		t.Fatal("connection timed out before the Pong deadline")
	}
	conn.heartbeatTick(sent.Add(2 * server.HeartbeatTimeout))
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

func TestHeartbeatSendPaths(t *testing.T) {
	if err := (&Conn{}).sendHeartbeatPing(time.Now()); !errors.Is(err, ErrClosed) {
		t.Fatalf("unavailable heartbeat ping = %v", err)
	}

	raw := newScriptedConn()
	conn := &Conn{
		raw:       raw,
		config:    testServerConfig(NewServer(Options{})),
		heartbeat: &heartbeatState{},
	}
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
	if err := conn.consumeHandshake([]byte("late")); !errors.Is(err, ErrClosed) {
		t.Fatalf("timed-out handshake accepted more data: %v", err)
	}
}

func TestCompletedHandshakeCancelsTimeout(t *testing.T) {
	raw := &writeProbeConn{closed: make(chan struct{})}
	conn := &Conn{raw: raw, config: testServerConfig(&Server{Options: Options{HandshakeTimeout: 5 * time.Millisecond}})}
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

func TestServerServeAfterCloseReportsClosed(t *testing.T) {
	server := &Server{Options: Options{HeartbeatInterval: time.Hour}}
	if err := server.Close(nil); err != nil {
		t.Fatal(err)
	}
	if err := server.Serve("127.0.0.1:0"); !errors.Is(err, ErrClosed) {
		t.Fatalf("Serve after Close error = %v, want %v", err, ErrClosed)
	}
}

func TestServerServeRunsEventsOnCallerGoroutine(t *testing.T) {
	server := NewServer(Options{})
	server.Options.Pollers = 1
	caller := make(chan int64, 1)
	onStart := make(chan int64, 1)
	server.startHook = func() {
		onStart <- goid.Get()
		_ = server.Close(nil)
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
	handler := &heartbeatHandler{closed: make(chan CloseEvent, 1)}
	server, addr, serveDone := startTestServer(t, func() *Server {
		s := NewServer(Options{Handler: handler})
		s.HeartbeatInterval = 10 * time.Millisecond
		s.HeartbeatTimeout = 25 * time.Millisecond
		s.Options.Pollers = 1
		s.Options.ReadBufferSize = 4 << 10
		return s
	})
	t.Cleanup(func() { stopTestServer(t, server, serveDone) })

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
	serverHandler := &echoHandler{open: make(chan struct{}), closed: make(chan struct{}), message: make(chan Message, 1)}
	server, addr, serveDone := startTestServer(t, func() *Server {
		s := NewServer(Options{Handler: serverHandler})
		s.HeartbeatInterval = 10 * time.Millisecond
		// The timeout must survive a starved CI runner: with 250ms a pong delayed
		// by scheduling could close a client that responded promptly in program
		// order, which is exactly what this test asserts cannot happen.
		s.HeartbeatTimeout = 2 * time.Second
		s.Options.Pollers = 1
		s.Options.ReadBufferSize = 4 << 10
		return s
	})

	clientHandler := &clientHandler{open: make(chan struct{}), closed: make(chan struct{}), message: make(chan Message, 1)}
	dialer := NewDialer(Options{Handler: clientHandler})
	dialer.Options.Pollers = 1
	dialer.Options.ReadBufferSize = 4 << 10
	err := dialer.Dial(context.Background(), "ws://"+addr+"/", nil)
	if err != nil {
		_ = server.Close(nil)
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = dialer.Close(nil)
		stopTestServer(t, server, serveDone)
	})
	select {
	case <-clientHandler.open:
	case <-time.After(testIOTimeout()):
		t.Fatal("client OnOpen was not called")
	}
	client := clientHandler.conn

	time.Sleep(600 * time.Millisecond)
	select {
	case <-serverHandler.closed:
		t.Fatal("responsive heartbeat client was closed")
	default:
	}
	if err = client.SendText("alive"); err != nil {
		t.Fatal(err)
	}
	select {
	case message := <-clientHandler.message:
		if string(message.Payload) != "world" {
			t.Fatalf("heartbeat echo = %q", message.Payload)
		}
	case <-clientHandler.closed:
		t.Fatal("responsive client was closed while waiting for its echo")
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
	opened.writes.close.pendingBytes.Store(8)
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

// A Pong accepted before a heartbeat Ping keeps its place ahead of it: only
// the Ping's own retirement starts the Pong timeout.
func TestHeartbeatPingPositionFollowsEarlierPong(t *testing.T) {
	raw := newScriptedConn()
	conn := &Conn{raw: raw, config: testServerConfig(NewServer(Options{})), heartbeat: &heartbeatState{}}
	conn.opened.Store(true)
	if err := conn.acceptControl(frame.Frame{Fin: true, Opcode: frame.Ping, Payload: []byte{1}}); err != nil {
		t.Fatal(err)
	}
	beforePing := conn.writes.close.pendingBytes.Load()
	if beforePing == 0 {
		t.Fatal("Pong was not queued")
	}
	if err := conn.sendHeartbeatPing(time.Now()); err != nil {
		t.Fatal(err)
	}
	pending := conn.writes.close.pendingBytes.Load()
	conn.heartbeat.mu.Lock()
	target, sentAt := conn.heartbeat.pingTarget, conn.heartbeat.pingSentAt
	conn.heartbeat.mu.Unlock()
	if pending <= beforePing || target != uint64(pending) || sentAt != 0 {
		t.Fatalf("queued Ping = pending:%d prior:%d target:%d sent:%d", pending, beforePing, target, sentAt)
	}
	conn.releaseOutbound(int(beforePing))
	conn.heartbeat.mu.Lock()
	sentAt = conn.heartbeat.pingSentAt
	conn.heartbeat.mu.Unlock()
	if sentAt != 0 {
		t.Fatal("Pong retirement marked Ping as sent")
	}
	conn.releaseOutbound(int(pending - beforePing))
	conn.heartbeat.mu.Lock()
	sentAt = conn.heartbeat.pingSentAt
	conn.heartbeat.mu.Unlock()
	if sentAt == 0 {
		t.Fatal("Ping retirement did not start Pong timeout")
	}
}

// A Ping the outbound limit refuses must leave a timer aimed at the stall
// deadline: the one-shot that brought the turn has fired, so re-arming the
// deadline it carried would never come again and the connection would never
// time out. This is the heartbeat's "peer silent and our own outbound full"
// case.
func TestHeartbeatRearmsAfterBackpressure(t *testing.T) {
	server := NewServer(Options{})
	server.HeartbeatInterval = 10 * time.Millisecond
	server.HeartbeatTimeout = 50 * time.Millisecond
	raw := newScriptedConn()
	raw.writeErr = uio.ErrOutboundOverflow
	conn := &Conn{raw: raw, config: testServerConfig(server), heartbeat: &heartbeatState{}}
	conn.opened.Store(true)
	conn.beginHeartbeat()
	first, ok := raw.lastWakeArm()
	if !ok {
		t.Fatal("beginHeartbeat armed no wake deadline")
	}

	// At the interval the Ping is refused for lack of outbound room.
	conn.heartbeatTick(first)
	if conn.heartbeat.pingOutstanding.Load() {
		t.Fatal("a refused Ping stayed outstanding")
	}
	heartbeat := conn.heartbeat
	heartbeat.mu.Lock()
	stalledAt := heartbeat.sendStalledAt
	heartbeat.mu.Unlock()
	if stalledAt != first.UnixNano() {
		t.Fatalf("stall noted at %d, want the refused attempt's %d", stalledAt, first.UnixNano())
	}
	// The wake is re-aimed at the earlier of the next interval and the
	// stall deadline — never left on the deadline that just fired.
	next, ok := raw.lastWakeArm()
	if !ok || next.Equal(first) {
		t.Fatalf("re-armed at %v; the fired deadline %v left no timer for the stall", next, first)
	}
	stallDeadline := first.Add(server.HeartbeatTimeout)
	if want := first.Add(server.HeartbeatInterval); !next.Equal(want) {
		t.Fatalf("re-armed at %v, want the next interval %v (before the stall deadline %v)", next, want, stallDeadline)
	}

	// Each retry is refused again; the stall keeps its first moment, so the
	// chain of wakes ends at the stall deadline, where the connection times
	// out.
	for at := next; at.Before(stallDeadline); {
		conn.heartbeatTick(at)
		if conn.closing.Load() {
			t.Fatalf("closed at %v, before the stall deadline %v", at, stallDeadline)
		}
		armed, ok := raw.lastWakeArm()
		if !ok || !armed.After(at) {
			t.Fatalf("retry at %v left the timer at %v", at, armed)
		}
		at = armed
	}
	conn.heartbeatTick(stallDeadline)
	if !conn.closing.Load() || raw.closes != 1 {
		t.Fatalf("closing=%v closes=%d, want the stall timeout to close", conn.closing.Load(), raw.closes)
	}
	if info := conn.closeInfo(); info.Code != 1001 || info.Reason != "heartbeat timeout" {
		t.Fatalf("heartbeat close info = %+v", info)
	}
}

// A wake deadline the transport refuses is not recorded as armed: the steady
// fast path would otherwise trust a timer that does not exist, and the
// connection's heartbeat would stop. The next turn retries the arm.
func TestHeartbeatRefusedArmIsRetried(t *testing.T) {
	server := NewServer(Options{})
	server.HeartbeatInterval = time.Hour
	raw := newScriptedConn()
	raw.wakeArmErr = errors.New("transport refused the timer")
	conn := &Conn{raw: raw, config: testServerConfig(server), heartbeat: &heartbeatState{}}
	conn.opened.Store(true)
	conn.beginHeartbeat()
	conn.heartbeat.mu.Lock()
	armed := conn.heartbeat.armedAt
	conn.heartbeat.mu.Unlock()
	if armed != 0 {
		t.Fatal("a refused arm was recorded as armed")
	}

	// A data turn long before the interval: with no timer, it must re-arm
	// rather than take the steady fast path.
	raw.wakeMu.Lock()
	raw.wakeArmErr = nil
	raw.wakeMu.Unlock()
	before := len(raw.wakeArms())
	conn.heartbeatTick(time.Now())
	if len(raw.wakeArms()) != before+1 {
		t.Fatal("a turn after a refused arm took the fast path and left no timer")
	}
	conn.heartbeat.mu.Lock()
	armed, next := conn.heartbeat.armedAt, conn.heartbeat.nextPingAt
	conn.heartbeat.mu.Unlock()
	if armed != next {
		t.Fatalf("armedAt %d, want the next ping %d once the transport took it", armed, next)
	}
	// Now the steady path holds: another turn arms nothing.
	conn.heartbeatTick(time.Now())
	if len(raw.wakeArms()) != before+1 {
		t.Fatal("a steady turn re-armed an already armed timer")
	}
}
