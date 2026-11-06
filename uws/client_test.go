package uws

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"errors"
	"fmt"
	"github.com/urpc/uio"
	"io"
	"net"
	"net/url"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type openSendHandler struct{ send func(*Conn) }

func (h openSendHandler) OnOpen(conn *Conn) {
	if h.send != nil {
		h.send(conn)
	}
}

func (openSendHandler) OnMessage(*Conn, Message) {
}

func (openSendHandler) OnClose(*Conn, CloseEvent) {
}

func startClientTestServer(t *testing.T, send func(*Conn)) string {
	t.Helper()
	server, addr, serveDone := startTestServer(t, func() *Server {
		return NewServer(Options{Handler: openSendHandler{send: send}})
	})
	t.Cleanup(func() {
		stopTestServer(t, server, serveDone)
	})
	return addr
}

func TestClientReceivesMessagesSentRightAfterOpen(t *testing.T) {
	const messages = 3
	addr := startClientTestServer(t, func(conn *Conn) {
		for i := range messages {
			_ = conn.SendText(fmt.Sprintf("m%d", i))
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), testIOTimeout())
	defer cancel()
	client, err := Dial(ctx, "ws://"+addr+"/")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = client.Close(1000, "")
	}()
	for i := range messages {
		message, err := client.ReadMessage(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if want := fmt.Sprintf("m%d", i); string(message.Payload) != want {
			t.Fatalf("message %d = %q, want %q", i, message.Payload, want)
		}
	}
}

func TestClientReadMessageContextTimesOutIndependently(t *testing.T) {
	trigger := make(chan struct{})
	addr := startClientTestServer(t, func(conn *Conn) {
		go func() {
			<-trigger
			_ = conn.SendText("late")
		}()
	})
	ctx, cancel := context.WithTimeout(context.Background(), testIOTimeout())
	defer cancel()
	client, err := Dial(ctx, "ws://"+addr+"/")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = client.Close(1000, "")
	}()
	quick, quickCancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	if _, err := client.ReadMessage(quick); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("ReadMessage error = %v, want %v", err, context.DeadlineExceeded)
	}
	quickCancel()
	close(trigger)
	message, err := client.ReadMessage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if string(message.Payload) != "late" {
		t.Fatalf("message = %q", message.Payload)
	}
}

func TestClientReportsPeerCloseAfterDrain(t *testing.T) {
	addr := startClientTestServer(t, func(conn *Conn) {
		_ = conn.SendText("one")
		_ = conn.SendText("two")
		_ = conn.Close(1000, "bye")
	})
	ctx, cancel := context.WithTimeout(context.Background(), testIOTimeout())
	defer cancel()
	client, err := Dial(ctx, "ws://"+addr+"/")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"one", "two"} {
		message, err := client.ReadMessage(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if string(message.Payload) != want {
			t.Fatalf("message = %q, want %q", message.Payload, want)
		}
	}
	_, err = client.ReadMessage(ctx)
	var closeErr *CloseError
	if !errors.As(err, &closeErr) {
		t.Fatalf("ReadMessage error = %v, want a CloseError", err)
	}
	if closeErr.Code != 1000 || closeErr.Reason != "bye" {
		t.Fatalf("close event = %d/%q, want 1000/bye", closeErr.Code, closeErr.Reason)
	}
	if info := client.CloseEventInfo(); info.Code != 1000 {
		t.Fatalf("CloseEventInfo = %+v", info)
	}
}

func TestClientCloseEndsFullQueue(t *testing.T) {
	const messages = 200
	addr := startClientTestServer(t, func(conn *Conn) {
		go func() {
			for i := range // well past the queue bound
			messages {
				if err := conn.SendText(fmt.Sprintf("m%d", i)); err != nil {
					return
				}
			}
		}()
	})
	ctx, cancel := context.WithTimeout(context.Background(), testIOTimeout())
	defer cancel()
	client, err := Dial(ctx, "ws://"+addr+"/")
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	done := make(chan error, 1)
	go func() {
		done <- client.Close(1000, "")
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(testIOTimeout()):
		t.Fatal("Close never returned from under a full queue")
	}
	deadline := time.Now().Add(testIOTimeout())
	for {
		_, err := client.ReadMessage(context.Background())
		if err == nil {
			continue
		}
		var closeErr *CloseError
		if !errors.As(err, &closeErr) {
			t.Fatalf("ReadMessage error = %v, want a CloseError", err)
		}
		break
	}
	if time.Now().After(deadline) {
		t.Fatal("the queue never drained to the close")
	}
}

func TestDialClientRejectsConfiguredHandler(t *testing.T) {
	dialer := NewDialer(Options{Handler: lifecycleEchoHandler{}})
	if _, err := dialer.DialClient(context.Background(), "ws://127.0.0.1:1/"); !errors.Is(err, ErrInvalidOptions) {
		t.Fatalf("DialClient error = %v, want %v", err, ErrInvalidOptions)
	}
	if dialer.started != nil {
		t.Fatal("a refused DialClient started the dialer event loop")
	}
}

func TestClientCloseInvalidReasonKeepsReading(t *testing.T) {
	server, addr, serveDone := startTestServer(t, func() *Server {
		return NewServer(Options{Handler: lifecycleEchoHandler{}})
	})
	t.Cleanup(func() {
		stopTestServer(t, server, serveDone)
	})
	ctx, cancel := context.WithTimeout(context.Background(), testIOTimeout())
	defer cancel()
	client, err := Dial(ctx, "ws://"+addr+"/")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = client.Close(1000, "")
	}()
	if err := client.Close(1000, "\xff"); err == nil {
		t.Fatal("an invalid close reason was accepted")
	}
	if client.IsClosed() {
		t.Fatal("an invalid Close closed the connection")
	}
	if err := client.SendText("still here"); err != nil {
		t.Fatal(err)
	}
	message, err := client.ReadMessage(ctx)
	if err != nil {
		t.Fatalf("reading stopped after a rejected Close: %v", err)
	}
	if string(message.Payload) != "still here" {
		t.Fatalf("message = %q", message.Payload)
	}
}

func TestClientCloseErrorCarriesCloseEvent(t *testing.T) {
	server, addr, serveDone := startTestServer(t, func() *Server {
		return NewServer(Options{Handler: lifecycleEchoHandler{}})
	})
	t.Cleanup(func() {
		stopTestServer(t, server, serveDone)
	})
	ctx, cancel := context.WithTimeout(context.Background(), testIOTimeout())
	defer cancel()
	client, err := Dial(ctx, "ws://"+addr+"/")
	if err != nil {
		t.Fatal(err)
	}
	if err := client.SendText("last"); err != nil {
		t.Fatal(err)
	}
	message, err := client.ReadMessage(ctx)
	if err != nil || string(message.Payload) != "last" {
		t.Fatalf("echo = %q, %v", message.Payload, err)
	}
	if err := client.Close(1000, "done"); err != nil {
		t.Fatal(err)
	}
	_, err = client.ReadMessage(ctx)
	var closeErr *CloseError
	if !errors.As(err, &closeErr) {
		t.Fatalf("ReadMessage error = %v, want a CloseError", err)
	}
	if closeErr.Code != 1000 {
		t.Fatalf("close event = %d/%q, want the peer's 1000 close answer", closeErr.Code, closeErr.Reason)
	}
}

type refusingExecutor struct{}

func (refusingExecutor) Submit(uio.IOTask) bool {
	return false
}

func (refusingExecutor) SubmitBatch([]uio.IOTask) int {
	return 0
}

func TestDialClientFirstTurnRefusalFailsDial(t *testing.T) {
	if !executorRunsTurns {
		t.Skip("the std backend ignores the Executor")
	}
	addr := startClientTestServer(t, nil)
	dialer := NewDialer(Options{Executor: refusingExecutor{}})
	ctx, cancel := context.WithTimeout(context.Background(), testIOTimeout())
	defer cancel()
	if _, err := dialer.DialClient(ctx, "ws://"+addr+"/"); !errors.Is(err, uio.ErrFirstTurnRefused) {
		t.Fatalf("DialClient error = %v, want %v", err, uio.ErrFirstTurnRefused)
	}
	if err := dialer.Dial(ctx, "ws://"+addr+"/", nil); !errors.Is(err, uio.ErrFirstTurnRefused) {
		t.Fatalf("Dial error = %v, want %v", err, uio.ErrFirstTurnRefused)
	}
}

func floodClient(t *testing.T, dialer *Dialer) *Client {
	t.Helper()
	addr := startClientTestServer(t, func(conn *Conn) {
		go func() {
			for range 10 * clientQueueMessages {
				if err := conn.SendText("flood"); err != nil {
					return
				}
			}
		}()
	})
	t.Cleanup(func() {
		_ = dialer.Close(nil)
	})
	ctx, cancel := context.WithTimeout(context.Background(), testIOTimeout())
	defer cancel()
	client, err := dialer.DialClient(ctx, "ws://"+addr+"/")
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(testIOTimeout())
	for client.InboundMessages() < clientQueueMessages {
		if time.Now().After(deadline) {
			t.Fatalf("queue holds %d messages, want it full", client.InboundMessages())
		}
		time.Sleep(time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)
	return client
}

func drainToClose(t *testing.T, client *Client) *CloseError {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testIOTimeout())
	defer cancel()
	for {
		_, err := client.ReadMessage(ctx)
		if err == nil {
			continue
		}
		var closeErr *CloseError
		if !errors.As(err, &closeErr) {
			t.Fatalf("ReadMessage error = %v, want a CloseError", err)
		}
		return closeErr
	}
}

func waitClientClosed(t *testing.T, client *Client, what string) {
	t.Helper()
	select {
	case <-client.ended:
	case <-time.After(testIOTimeout()):
		t.Fatalf("%s did not close a Client whose full queue nobody reads", what)
	}
}

func TestDialerCloseEndsUnreadClient(t *testing.T) {
	dialer := &Dialer{}
	client := floodClient(t, dialer)
	if err := dialer.Close(nil); err != nil {
		t.Fatal(err)
	}
	waitClientClosed(t, client, "Dialer.Close")
	stopped := make(chan struct{})
	go func() {
		_ = dialer.events.Wait()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(testIOTimeout()):
		t.Fatal("the dialer's transport did not stop")
	}
	queued := client.InboundMessages()
	if queued < clientQueueMessages {
		t.Fatalf("%d queued messages left, want the full queue", queued)
	}
	drainToClose(t, client)
}

func TestSelfInitiatedClosesEndUnreadClient(t *testing.T) {
	t.Run("read deadline", func(t *testing.T) {
		client := floodClient(t, &Dialer{})
		if err := client.SetReadDeadline(time.Now().Add(20 * time.Millisecond)); err != nil {
			t.Fatal(err)
		}
		waitClientClosed(t, client, "an expired read deadline")
		if closeErr := drainToClose(t, client); !errors.Is(closeErr, os.ErrDeadlineExceeded) {
			t.Fatalf("closed with %v, want the deadline", closeErr.Err)
		}
	})
	t.Run("deadline cleared in time", func(t *testing.T) {
		client := floodClient(t, &Dialer{})
		if err := client.SetDeadline(time.Now().Add(30 * time.Millisecond)); err != nil {
			t.Fatal(err)
		}
		if err := client.SetDeadline(time.Time{}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(60 * time.Millisecond)
		select {
		case <-client.ended:
			t.Fatal("a cleared deadline closed the connection")
		default:
		}
		ctx, cancel := context.WithTimeout(context.Background(), testIOTimeout())
		defer cancel()
		for range 10 * clientQueueMessages {
			if _, err := client.ReadMessage(ctx); err != nil {
				t.Fatalf("ReadMessage: %v", err)
			}
		}
	})
	t.Run("embedded Conn.Close", func(t *testing.T) {
		client := floodClient(t, &Dialer{})
		done := make(chan error, 1)
		go func() {
			done <- client.Conn.Close(1000, "bye")
		}()
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(testIOTimeout()):
			t.Fatal("Conn.Close did not return")
		}
		waitClientClosed(t, client, "Conn.Close")
		drainToClose(t, client)
	})
	t.Run("heartbeat timeout", func(t *testing.T) {
		client := floodClient(t, &Dialer{Options: Options{HeartbeatInterval: 10 * time.Millisecond, HeartbeatTimeout: 50 * time.Millisecond}})
		waitClientClosed(t, client, "a heartbeat timeout")
		drainToClose(t, client)
	})
}

type dialLifecycleHandler struct {
	opened chan struct{}
	closed chan CloseEvent
}

func (h *dialLifecycleHandler) OnOpen(*Conn) {
	select {
	case h.opened <- struct{}{}:
	default:
	}
}

func (*dialLifecycleHandler) OnMessage(*Conn, Message) {
}

func (h *dialLifecycleHandler) OnClose(_ *Conn, info CloseEvent) {
	h.closed <- info
}

func TestDialerUsesUIOEventLoop(t *testing.T) {
	serverHandler := &echoHandler{open: make(chan struct{}), closed: make(chan struct{}), message: make(chan Message, 1)}
	server, addr, serverDone := startTestServer(t, func() *Server {
		s := NewServer(Options{Handler: serverHandler})
		s.EnableCompression = true
		s.Options.Pollers = 1
		s.Options.ReadBufferSize = 4 << 10
		return s
	})
	clientHandler := &clientHandler{open: make(chan struct{}), closed: make(chan struct{}), message: make(chan Message, 1)}
	dialer := NewDialer(Options{Handler: clientHandler})
	dialer.EnableCompression = true
	dialer.Options.Pollers = 1
	dialer.Options.ReadBufferSize = 4 << 10
	err := dialer.Dial(context.Background(), "ws://"+addr+"/chat", nil)
	if err != nil {
		_ = server.Close(nil)
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = dialer.Close(nil)
		stopTestServer(t, server, serverDone)
	})
	select {
	case <-clientHandler.open:
	case <-time.After(testIOTimeout()):
		t.Fatal("client OnOpen was not called")
	}
	client := clientHandler.conn
	deadline := time.Now().Add(testIOTimeout())
	for client.handshake.Load() != nil && time.Now().Before(deadline) {
		runtime.Gosched()
	}
	if client.handshake.Load() != nil {
		t.Fatal("completed client handshake retained handshake state")
	}
	if err = client.SendText("hello"); err != nil {
		t.Fatal(err)
	}
	select {
	case message := <-serverHandler.message:
		if message.Type != TextMessage || string(message.Payload) != "hello" {
			t.Fatalf("server message = %+v", message)
		}
	case <-time.After(testIOTimeout()):
		t.Fatal("server did not receive client message")
	}
	select {
	case message := <-clientHandler.message:
		if message.Type != TextMessage || string(message.Payload) != "world" {
			t.Fatalf("client message = %+v", message)
		}
	case <-time.After(testIOTimeout()):
		t.Fatal("client did not receive server message")
	}
	shutdownErr := errors.New("dialer shutdown")
	if err = dialer.Close(shutdownErr); err != nil {
		t.Fatal(err)
	}
	select {
	case <-clientHandler.closed:
	case <-time.After(testIOTimeout()):
		t.Fatal("Dialer.Close did not call OnClose")
	}
}

func TestDialerValidationAndConnectionFailure(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := new(Dialer).Dial(canceled, "ws://127.0.0.1:1", nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled Dial() = %v", err)
	}
	dialer := NewDialer(Options{})
	t.Cleanup(func() {
		_ = dialer.Close(nil)
	})
	if err := dialer.Dial(nil, "http://example.test", nil); !errors.Is(err, ErrProtocol) {
		t.Fatalf("invalid scheme Dial() = %v", err)
	}
	if dialer.started != nil {
		t.Fatal("invalid target started the Dialer event loop")
	}
	if err := dialer.Dial(context.Background(), "ws://127.0.0.1:1", nil); err == nil {
		t.Fatal("connection failure Dial() returned nil")
	}
	if err := (&Dialer{}).Close(nil); err != nil {
		t.Fatalf("Dialer.Close() with nil Events = %v", err)
	}
	closedDialer := NewDialer(Options{})
	closeErr := errors.New("closed before dial")
	if err := closedDialer.Close(closeErr); err != nil {
		t.Fatal(err)
	}
	if err := closedDialer.Dial(context.Background(), "ws://127.0.0.1:1", nil); !errors.Is(err, closeErr) {
		t.Fatalf("Dial() after Close error = %v, want %v", err, closeErr)
	}
}

func TestDialContextCancelsDuringDialerStartup(t *testing.T) {
	dialer := NewDialer(Options{})
	entered := make(chan struct{})
	release := make(chan struct{})
	dialer.startHook = func() {
		close(entered)
		<-release
	}
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
		_ = dialer.Close(nil)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	result := make(chan error, 1)
	go func() {
		err := dialer.Dial(ctx, "ws://127.0.0.1:1/", nil)
		result <- err
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("Dialer OnStart was not entered")
	}
	select {
	case err := <-result:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Dial() error = %v, want %v", err, context.DeadlineExceeded)
		}
	case <-time.After(time.Second):
		t.Fatal("Dial context did not interrupt startup")
	}
	close(release)
}

func TestDialerCallbacksHandleLifecycleEdges(t *testing.T) {
	failureHandler := &dialLifecycleHandler{opened: make(chan struct{}, 1), closed: make(chan CloseEvent, 2)}
	dialer := NewDialer(Options{Handler: failureHandler})
	invalid := newScriptedConn()
	dialer.onOpen(invalid)
	if invalid.closes != 1 {
		t.Fatalf("invalid setup closes = %d, want 1", invalid.closes)
	}
	invalid.userdata = "invalid"
	if err := dialer.onData(invalid); !errors.Is(err, ErrClosed) {
		t.Fatalf("invalid onData error = %v", err)
	}
	dialer.onClose(invalid, io.EOF)
	raw := newScriptedConn()
	raw.userdata = &dialSetup{key: testKey}
	dialer.onOpen(raw)
	conn, ok := raw.userdata.(*Conn)
	if !ok || !conn.isClient() {
		t.Fatalf("dialer onOpen userdata = %#v", raw.userdata)
	}
	conn.stopHandshakeTimer()
	unopenedErr := errors.New("handshake failed")
	dialer.onClose(raw, unopenedErr)
	dialer.onClose(raw, unopenedErr)
	select {
	case info := <-failureHandler.closed:
		if !errors.Is(info.Err, unopenedErr) {
			t.Fatalf("OnClose error = %v, want %v", info.Err, unopenedErr)
		}
	case <-time.After(time.Second):
		t.Fatal("failed handshake did not call OnClose")
	}
	select {
	case <-failureHandler.opened:
		t.Fatal("failed handshake called OnOpen")
	default:
	}
	select {
	case info := <-failureHandler.closed:
		t.Fatalf("duplicate OnClose callback: %+v", info)
	default:
	}
	openedRaw := newScriptedConn()
	handler := &recordingHandler{}
	opened := &Conn{raw: openedRaw, config: testDialerConfig(dialer), handler: handler}
	opened.opened.Store(true)
	openedRaw.userdata = opened
	dialer.onOutbound(openedRaw, 8)
	dialer.onClose(openedRaw, io.EOF)
	dialer.onClose(openedRaw, io.EOF)
	handler.mu.Lock()
	defer handler.mu.Unlock()
	if got := strings.Join(handler.events, ","); got != "close" {
		t.Fatalf("close events = %q", got)
	}
}

func TestDialerCloseBeforeAsyncOnOpenAttachesPendingConnection(t *testing.T) {
	handler := &recordingHandler{}
	cleaned := atomic.Int64{}
	dialer := NewDialer(Options{Handler: handler})
	setup := &dialSetup{Context: context.Background(), cleanup: func() {
		cleaned.Add(1)
	}, key: testKey}
	setup.conn = dialer.newClientConn(nil, setup)
	raw := newScriptedConn()
	raw.userdata = setup
	closeErr := errors.New("closed before async OnOpen")
	dialer.onClose(raw, closeErr)
	conn, ok := raw.userdata.(*Conn)
	if !ok || conn != setup.conn || !conn.closed.Load() {
		t.Fatalf("pending connection was not attached and closed: %#v", raw.userdata)
	}
	if cleaned.Load() != 1 {
		t.Fatalf("attempt cleanup calls = %d, want 1", cleaned.Load())
	}
	handler.mu.Lock()
	defer handler.mu.Unlock()
	if got := strings.Join(handler.events, ","); got != "close" {
		t.Fatalf("lifecycle events = %q, want close", got)
	}
}

type closeWatcher struct {
	open   chan struct{}
	closed chan CloseEvent
}

func newCloseWatcher() *closeWatcher {
	return &closeWatcher{open: make(chan struct{}, 1), closed: make(chan CloseEvent, 1)}
}

func (h *closeWatcher) OnOpen(*Conn) {
	select {
	case h.open <- struct{}{}:
	default:
	}
}

func (h *closeWatcher) OnMessage(*Conn, Message) {
}

func (h *closeWatcher) OnClose(_ *Conn, event CloseEvent) {
	select {
	case h.closed <- event:
	default:
	}
}

func testAcceptKey(key string) string {
	sum := sha1.Sum([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	return base64.StdEncoding.EncodeToString(sum[:])
}

type closeCountingHandler struct {
	mu     sync.Mutex
	closed map[*int]int
}

func (*closeCountingHandler) OnOpen(*Conn) {
}

func (*closeCountingHandler) OnMessage(*Conn, Message) {
}

func (h *closeCountingHandler) OnClose(c *Conn, _ CloseEvent) {
	h.mu.Lock()
	h.closed[c.Userdata().(*int)]++
	h.mu.Unlock()
}

func TestDialRacingDialerCloseReportsEveryConnectionClosed(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				_, _ = io.Copy(io.Discard, c)
				_ = c.Close()
			}()
		}
	}()
	addr := "ws://" + ln.Addr().String() + "/"
	rounds := 100
	if testing.Short() {
		rounds = 20
	}
	for round := range rounds {
		handler := &closeCountingHandler{closed: map[*int]int{}}
		dialer := NewDialer(Options{Handler: handler})
		var mu sync.Mutex
		var dialed, refused []*int
		dial := func() {
			token := new(int)
			err := dialer.Dial(context.Background(), addr, token)
			mu.Lock()
			if err == nil {
				dialed = append(dialed, token)
			} else {
				refused = append(refused, token)
			}
			mu.Unlock()
		}
		dial()
		if len(dialed) != 1 {
			t.Fatal("the first Dial failed")
		}
		var wg sync.WaitGroup
		for range 8 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				dial()
			}()
		}
		time.Sleep(time.Duration(round%7) * 50 * time.Microsecond)
		_ = dialer.Close(nil)
		wg.Wait()
		for deadline := time.Now().Add(testIOTimeout()); ; time.Sleep(time.Millisecond) {
			handler.mu.Lock()
			missing, twice, stray := 0, 0, 0
			for _, token := range dialed {
				switch handler.closed[token] {
				case 0:
					missing++
				case 1:
				default:
					twice++
				}
			}
			for _, token := range refused {
				stray += handler.closed[token]
			}
			handler.mu.Unlock()
			if twice > 0 || stray > 0 {
				t.Fatalf("round %d: %d connections got OnClose more than once, %d failed dials got one", round, twice, stray)
			}
			if missing == 0 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("round %d: %d of %d dialed connections never got OnClose", round, missing, len(dialed))
			}
		}
	}
}

func TestDialHostDefaultsThePort(t *testing.T) {
	for _, tc := range []struct{ url, host string }{{"ws://example.test/chat", "example.test:80"}, {"wss://example.test/chat", "example.test:443"}, {"ws://example.test:8080/chat", "example.test:8080"}, {"wss://[::1]/chat", "[::1]:443"}, {"ws://[::1]:9/chat", "[::1]:9"}} {
		target, err := url.Parse(tc.url)
		if err != nil {
			t.Fatal(err)
		}
		if got := dialHost(target); got != tc.host {
			t.Fatalf("dialHost(%s) = %q, want %q", tc.url, got, tc.host)
		}
	}
}

type serverBlockingUpgradeHandler struct {
	ready       chan *BlockingConn
	closeCalled atomic.Int32
	turnErr     chan error
}

func (h *serverBlockingUpgradeHandler) OnOpen(conn *Conn) {
	bc, err := NewBlockingConn(conn, WithReadQueueBytes(4<<10), WithReadQueueMessages(8), WithWriteBuffer(4<<10))
	if err != nil {
		h.turnErr <- err
		return
	}
	// Idempotent second call returns the same instance.
	bc2, err := NewBlockingConn(conn)
	if err != nil || bc2 != bc {
		h.turnErr <- fmt.Errorf("idempotence failed: %v, %v", bc2, err)
		return
	}
	// Calling blocking methods or DialClient on a turn fails fast with ErrBlockingOnTurn!
	if _, rerr := bc.ReadMessage(context.Background()); !errors.Is(rerr, ErrBlockingOnTurn) {
		h.turnErr <- fmt.Errorf("ReadMessage on turn = %v", rerr)
		return
	}
	if werr := bc.WriteText(context.Background(), "x"); !errors.Is(werr, ErrBlockingOnTurn) {
		h.turnErr <- fmt.Errorf("WriteText on turn = %v", werr)
		return
	}
	if ferr := bc.Flush(context.Background()); !errors.Is(ferr, ErrBlockingOnTurn) {
		h.turnErr <- fmt.Errorf("Flush on turn = %v", ferr)
		return
	}
	if _, derr := DefaultDialer.DialClient(context.Background(), "ws://127.0.0.1:1/"); !errors.Is(derr, ErrBlockingOnTurn) {
		h.turnErr <- fmt.Errorf("DialClient on turn = %v", derr)
		return
	}
	h.turnErr <- nil
	h.ready <- bc
}

func (*serverBlockingUpgradeHandler) OnMessage(*Conn, Message) {}

func (h *serverBlockingUpgradeHandler) OnClose(*Conn, CloseEvent) {
	h.closeCalled.Add(1)
}

func TestServerAndClientSymmetricBlockingConnAndBackpressure(t *testing.T) {
	upg := &serverBlockingUpgradeHandler{
		ready:   make(chan *BlockingConn, 1),
		turnErr: make(chan error, 1),
	}
	server, addr, serveDone := startTestServer(t, func() *Server {
		return NewServer(Options{
			Handler:             upg,
			MaxInboundBuffered:  8 << 10,
			MaxOutboundBuffered: 8 << 10,
		})
	})
	t.Cleanup(func() {
		stopTestServer(t, server, serveDone)
	})

	dialer := NewDialer(Options{
		MaxInboundBuffered:  8 << 10,
		MaxOutboundBuffered: 8 << 10,
	})
	t.Cleanup(func() { _ = dialer.Close(nil) })

	ctx, cancel := context.WithTimeout(context.Background(), testIOTimeout())
	defer cancel()

	cliBC, err := dialer.DialClient(ctx, "ws://"+addr+"/", WithReadQueueBytes(4<<10), WithWriteBuffer(4<<10))
	if err != nil {
		t.Fatal(err)
	}
	if err := <-upg.turnErr; err != nil {
		t.Fatal(err)
	}
	srvBC := <-upg.ready

	// Server echoes every message using ReadMessageInto (zero-alloc reuse) + WriteMessage/WriteText + Flush!
	srvDone := make(chan error, 1)
	go func() {
		var scratch [4 << 10]byte
		for {
			typ, payload, err := srvBC.ReadMessageInto(ctx, scratch[:0])
			if err != nil {
				var ce *CloseError
				if errors.As(err, &ce) {
					srvDone <- nil
					return
				}
				srvDone <- err
				return
			}
			if typ == TextMessage {
				err = srvBC.WriteText(ctx, string(payload))
			} else {
				err = srvBC.WriteBinary(ctx, payload)
			}
			if err != nil {
				srvDone <- err
				return
			}
		}
	}()

	// Oversized single message (including near-boundary [limit-13, limit-2] masked frame!) fails fast with ErrBackpressure!
	if err := cliBC.WriteBinary(ctx, make([]byte, (8<<10)-4)); !errors.Is(err, ErrBackpressure) {
		t.Fatalf("near-boundary WriteBinary = %v, want ErrBackpressure", err)
	}

	// Stream 48 messages of 1.5 KiB (72 KiB >> 8 KiB outbound/inbound caps) + empty text message!
	const count = 48
	chunk := bytes.Repeat([]byte(" symmetric-uws-block "), 70)
	sendDone := make(chan error, 1)
	go func() {
		if err := cliBC.WriteText(ctx, ""); err != nil {
			sendDone <- err
			return
		}
		for i := range count {
			var err error
			if i%2 == 0 {
				err = cliBC.WriteBinary(ctx, chunk)
			} else {
				err = cliBC.WriteText(ctx, string(chunk))
			}
			if err != nil {
				sendDone <- err
				return
			}
		}
		sendDone <- cliBC.Flush(ctx)
	}()

	var recvScratch [2 << 10]byte
	// First empty text frame
	typ0, out0, err := cliBC.ReadMessageInto(ctx, recvScratch[:0])
	if err != nil || typ0 != TextMessage || len(out0) != 0 {
		t.Fatalf("empty text echo = %v, len=%d, err=%v", typ0, len(out0), err)
	}
	for i := range count {
		if i%2 == 0 {
			m, err := cliBC.ReadMessage(ctx)
			if err != nil || m.Type != BinaryMessage || !bytes.Equal(m.Payload, chunk) {
				t.Fatalf("msg %d ReadMessage err=%v len=%d", i, err, len(m.Payload))
			}
		} else {
			typ, out, err := cliBC.ReadMessageInto(ctx, recvScratch[:0])
			if err != nil || typ != TextMessage || !bytes.Equal(out, chunk) {
				t.Fatalf("msg %d ReadMessageInto err=%v len=%d", i, err, len(out))
			}
		}
	}
	if err := <-sendDone; err != nil {
		t.Fatalf("sendDone = %v", err)
	}

	if err := cliBC.Close(1000, "bye"); err != nil {
		t.Fatal(err)
	}
	if err := <-srvDone; err != nil {
		t.Fatalf("srvDone = %v", err)
	}
	for deadline := time.Now().Add(time.Second); upg.closeCalled.Load() != 1; {
		if time.Now().After(deadline) {
			t.Fatal("upgraded server prevHandler.OnClose was not called")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestBlockingConnErrorAndCancellationGuards(t *testing.T) {
	// 1. NewBlockingConn error guards (nil, closed, off-turn active conn).
	if _, err := NewBlockingConn(nil); !errors.Is(err, ErrNotReady) {
		t.Fatalf("nil NewBlockingConn = %v", err)
	}
	closedConn := testServerConn(newScriptedConn())
	closedConn.closed.Store(true)
	if _, err := NewBlockingConn(closedConn); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed NewBlockingConn = %v", err)
	}
	activeConn := testServerConn(newScriptedConn())
	if _, err := NewBlockingConn(activeConn); !errors.Is(err, ErrInvalidOptions) {
		t.Fatalf("off-turn NewBlockingConn = %v", err)
	}

	// 2. Unattached and canceled ctx guards on WriteMessage / Flush / ReadMessage + queued empty message.
	unattached := newBlockingConn(nil, resolveBlockingOpts(nil, []BlockingOption{
		WithReadQueueBytes(-1),
		WithReadQueueMessages(-1),
		WithWriteBuffer(-1),
	}))
	unattached.enqueue(nil, Message{Type: TextMessage, Payload: nil})
	var smallDst [4]byte
	if typ, out, err := unattached.ReadMessageInto(nil, smallDst[:0]); err != nil || typ != TextMessage || len(out) != 0 {
		t.Fatalf("queued empty ReadMessageInto = %v, %q, %v", typ, out, err)
	}
	unattached.enqueue(nil, Message{Type: BinaryMessage, Payload: nil})
	if m, err := unattached.ReadMessage(nil); err != nil || m.Type != BinaryMessage || m.Payload != nil {
		t.Fatalf("queued empty ReadMessage = %+v, %v", m, err)
	}
	if err := unattached.WriteBinary(nil, []byte("x")); !errors.Is(err, ErrNotReady) {
		t.Fatalf("unattached WriteBinary = %v", err)
	}
	if err := unattached.Close(1000, ""); !errors.Is(err, ErrNotReady) {
		t.Fatalf("unattached Close = %v", err)
	}
	if err := unattached.Flush(nil); !errors.Is(err, ErrNotReady) {
		t.Fatalf("unattached Flush = %v", err)
	}
	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := unattached.ReadMessage(canceledCtx); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled ReadMessage = %v", err)
	}
	if err := unattached.Flush(canceledCtx); !errors.Is(err, ErrNotReady) {
		t.Fatalf("unattached canceled Flush = %v", err)
	}

	// 3. Blocked WriteBinary & Flush interrupted by short ctx timeout, non-backpressure errors, and close!
	if (&CloseError{}).Error() != "uws: connection closed" {
		t.Fatal("empty CloseError string mismatch")
	}
	raw := &limitedWire{limit: 32}
	conn := newLimitedConn(raw, time.Hour)
	bc := newBlockingConn(conn, blockingOptions{readBytes: 1024, readMsgs: 8, writeBytes: 32})
	if err := bc.Flush(context.Background()); err != nil { // OutboundBuffered == 0 fast return
		t.Fatalf("empty Flush = %v", err)
	}
	if err := bc.WriteText(canceledCtx, "x"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled WriteText = %v", err)
	}
	if err := bc.Flush(canceledCtx); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled Flush = %v", err)
	}
	if err := bc.WriteMessage(context.Background(), MessageType(99), []byte("bad")); err == nil {
		t.Fatal("invalid MessageType accepted")
	}
	// Parked ReadMessage receiving empty nil-payload message directly!
	emptyDirect := make(chan Message, 1)
	go func() {
		m, _ := bc.ReadMessage(context.Background())
		emptyDirect <- m
	}()
	time.Sleep(10 * time.Millisecond)
	bc.enqueue(conn, Message{Type: BinaryMessage, Payload: nil})
	if gotM := <-emptyDirect; gotM.Type != BinaryMessage || gotM.Payload != nil {
		t.Fatalf("direct empty message = %+v", gotM)
	}

	if err := bc.WriteBinary(context.Background(), make([]byte, 28)); err != nil {
		t.Fatal(err)
	}
	shortCtx, shortCancel := context.WithTimeout(context.Background(), 15*time.Millisecond)
	defer shortCancel()
	if err := bc.WriteBinary(shortCtx, make([]byte, 20)); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("blocked WriteBinary timeout = %v", err)
	}
	shortFlush, shortFlushCancel := context.WithTimeout(context.Background(), 15*time.Millisecond)
	defer shortFlushCancel()
	if err := bc.Flush(shortFlush); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("blocked Flush timeout = %v", err)
	}

	// Closed before event stored -> ErrClosed!
	conn.closed.Store(true)
	if err := bc.WriteBinary(context.Background(), []byte("x")); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed pre-event WriteBinary = %v", err)
	}
	conn.closed.Store(false)

	// Closing wakes blocked Flush & WriteBinary with CloseError!
	flushWait := make(chan error, 1)
	go func() { flushWait <- bc.Flush(context.Background()) }()
	time.Sleep(10 * time.Millisecond)
	conn.closed.Store(true)
	bc.onConnClose(CloseEvent{Code: 1001, Err: ErrClosed})
	if err := <-flushWait; err == nil {
		t.Fatal("closed Flush returned nil")
	}
	if err := bc.WriteBinary(context.Background(), []byte("x")); err == nil {
		t.Fatal("closed WriteBinary returned nil")
	}
	if err := bc.Flush(context.Background()); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed Flush = %v", err)
	}
	bc.enqueue(conn, Message{Type: TextMessage, Payload: []byte("late")}) // intakeDone drops late frame
	if err := bc.WriteMessage(context.Background(), MessageType(99), []byte("bad")); !errors.Is(err, ErrClosed) {
		var ce *CloseError
		if !errors.As(err, &ce) {
			t.Fatalf("closed invalid WriteMessage = %v", err)
		}
	}
}
