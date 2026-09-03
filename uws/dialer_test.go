package uws

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urpc/uio"
	"github.com/urpc/uio/uws/internal/compress"
	"github.com/urpc/uio/uws/internal/frame"
)

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

func (*dialLifecycleHandler) OnMessage(*Conn, Message) {}

func (h *dialLifecycleHandler) OnClose(_ *Conn, info CloseEvent) {
	h.closed <- info
}

func TestDialerNegotiatesAndUsesSmallDeflateWindow(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	serverPayload := bytes.Repeat([]byte("server-window-"), 32)
	clientPayload := bytes.Repeat([]byte("client-window-"), 32)
	serverMessage := make(chan frame.Frame, 64)
	serverErr := make(chan error, 1)
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			serverErr <- acceptErr
			return
		}
		defer conn.Close()
		reader := bufio.NewReader(conn)
		request, readErr := http.ReadRequest(reader)
		if readErr != nil {
			serverErr <- readErr
			return
		}
		key := request.Header.Get("Sec-WebSocket-Key")
		accept := sha1.Sum([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
		response := "HTTP/1.1 101 Switching Protocols\r\n" +
			"Upgrade: websocket\r\nConnection: Upgrade\r\n" +
			"Sec-WebSocket-Accept: " + base64.StdEncoding.EncodeToString(accept[:]) + "\r\n" +
			"Sec-WebSocket-Extensions: permessage-deflate; server_max_window_bits=8; client_max_window_bits=8\r\n\r\n"
		if _, writeErr := conn.Write([]byte(response)); writeErr != nil {
			serverErr <- writeErr
			return
		}
		encoder := compress.NewEncoderWithWindow(-1, true, 8)
		encoded, encodeErr := encoder.Encode(serverPayload)
		if encodeErr != nil {
			serverErr <- encodeErr
			return
		}
		if _, writeErr := conn.Write(frame.Append(nil, frame.Frame{Fin: true, RSV1: true, Opcode: frame.Binary, Payload: encoded}, [4]byte{})); writeErr != nil {
			serverErr <- writeErr
			return
		}
		parser := frame.NewParser(frame.ParserConfig{ExpectMask: true, AllowRSV1: true, MaxFramePayload: 1 << 20})
		buffer := make([]byte, 4096)
		for {
			n, readErr := conn.Read(buffer)
			if n > 0 {
				_, parseErr := parser.Feed(buffer[:n], func(f frame.Frame) error {
					f.Payload = append([]byte(nil), f.Payload...)
					serverMessage <- f
					return nil
				})
				if parseErr != nil {
					serverErr <- parseErr
					return
				}
			}
			if readErr != nil {
				if !errors.Is(readErr, io.EOF) {
					serverErr <- readErr
				}
				return
			}
		}
	}()

	handler := &clientHandler{open: make(chan struct{}), closed: make(chan struct{}), message: make(chan Message, 1)}
	dialer := NewDialer()
	dialer.EnableCompression = true
	dialer.Events = &uio.Events{Pollers: 1, MaxBufferSize: 4 << 10}
	client, err := dialer.Dial(context.Background(), "ws://"+listener.Addr().String()+"/", handler)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = client.Close(1000, "")
		_ = dialer.Close(nil)
	})
	select {
	case <-handler.open:
	case err = <-serverErr:
		t.Fatal(err)
	case <-time.After(testIOTimeout()):
		t.Fatal("small-window handshake did not call OnOpen")
	}
	select {
	case message := <-handler.message:
		if !bytes.Equal(message.Payload, serverPayload) {
			t.Fatalf("server compressed payload = %d bytes, want %d", len(message.Payload), len(serverPayload))
		}
	case err = <-serverErr:
		t.Fatal(err)
	case <-time.After(testIOTimeout()):
		t.Fatal("client did not decode small-window message")
	}
	writer, err := client.BeginMessage(BinaryMessage)
	if err != nil {
		t.Fatal(err)
	}
	split := len(clientPayload) / 2
	if _, err = writer.Write(clientPayload[:split]); err != nil {
		t.Fatal(err)
	}
	if _, err = writer.Write(clientPayload[split:]); err != nil {
		t.Fatal(err)
	}
	if err = writer.Close(); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(testIOTimeout())
	select {
	case first := <-serverMessage:
		if !first.RSV1 || first.Opcode != frame.Binary || !first.Masked || first.Fin {
			t.Fatalf("client first frame = %+v", first)
		}
		encoded := append([]byte(nil), first.Payload...)
		for {
			var message frame.Frame
			select {
			case message = <-serverMessage:
			case <-deadline:
				t.Fatal("server did not receive final client continuation")
			}
			if message.RSV1 || message.Opcode != frame.Continuation || !message.Masked {
				t.Fatalf("client continuation frame = %+v", message)
			}
			encoded = append(encoded, message.Payload...)
			if message.Fin {
				break
			}
		}
		decoder := compress.NewDecoderWithWindow(false, 8)
		decoded, decodeErr := decoder.Decode(encoded, len(clientPayload))
		if decodeErr != nil || !bytes.Equal(decoded, clientPayload) {
			t.Fatalf("client compressed payload = %d bytes, %v; want %d", len(decoded), decodeErr, len(clientPayload))
		}
	case err = <-serverErr:
		t.Fatal(err)
	case <-time.After(testIOTimeout()):
		t.Fatal("server did not receive client compressed message")
	}
}

func TestDialerUsesUIOEventLoop(t *testing.T) {
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := probe.Addr().String()
	_ = probe.Close()

	serverHandler := &echoHandler{open: make(chan struct{}), closed: make(chan struct{}), message: make(chan Message, 1)}
	server := NewServer(serverHandler)
	server.EnableCompression = true
	server.Events = &uio.Events{Pollers: 1, MaxBufferSize: 4 << 10}
	serverDone := make(chan error, 1)
	go func() { serverDone <- server.Serve(addr) }()
	for deadline := time.Now().Add(testIOTimeout()); time.Now().Before(deadline); {
		probe, probeErr := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if probeErr == nil {
			_ = probe.Close()
			break
		}
		time.Sleep(time.Millisecond)
	}

	clientHandler := &clientHandler{open: make(chan struct{}), closed: make(chan struct{}), message: make(chan Message, 1)}
	dialer := NewDialer()
	dialer.EnableCompression = true
	dialer.Events = &uio.Events{Pollers: 1, MaxBufferSize: 4 << 10}
	client, err := dialer.Dial(context.Background(), "ws://"+addr+"/chat", clientHandler)
	if err != nil {
		_ = server.Close(nil)
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = client.Close(1000, "")
		_ = dialer.Close(nil)
		_ = server.Close(nil)
		select {
		case <-serverDone:
		case <-time.After(testIOTimeout()):
			t.Error("server did not stop")
		}
	})

	select {
	case <-clientHandler.open:
	case <-time.After(testIOTimeout()):
		t.Fatal("client OnOpen was not called")
	}
	if client.handshake.Load() != nil {
		t.Fatal("completed client handshake retained handshake state")
	}
	if err = client.SendText([]byte("hello")); err != nil {
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
	case <-time.After(time.Second):
		t.Fatal("Dialer.Close did not call OnClose")
	}
}

func TestDialerValidationAndConnectionFailure(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := new(Dialer).Dial(canceled, "ws://127.0.0.1:1", nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled Dial() = %v", err)
	}

	dialer := NewDialer()
	t.Cleanup(func() { _ = dialer.Close(nil) })
	if _, err := dialer.Dial(nil, "http://example.test", nil); !errors.Is(err, ErrProtocol) {
		t.Fatalf("invalid scheme Dial() = %v", err)
	}
	if dialer.started != nil {
		t.Fatal("invalid target started the Dialer event loop")
	}
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := probe.Addr().String()
	_ = probe.Close()
	if _, err := dialer.Dial(context.Background(), "ws://"+address, nil); err == nil {
		t.Fatal("connection failure Dial() returned nil")
	}
	if err := (&Dialer{}).Close(nil); err != nil {
		t.Fatalf("Dialer.Close() with nil Events = %v", err)
	}

	closedDialer := NewDialer()
	closeErr := errors.New("closed before dial")
	if err := closedDialer.Close(closeErr); err != nil {
		t.Fatal(err)
	}
	if _, err := closedDialer.Dial(context.Background(), "ws://127.0.0.1:1", nil); !errors.Is(err, closeErr) {
		t.Fatalf("Dial() after Close error = %v, want %v", err, closeErr)
	}
}

func TestDialerHandshakeContextDeadline(t *testing.T) {
	dialCtx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	handler := &dialLifecycleHandler{opened: make(chan struct{}, 1), closed: make(chan CloseEvent, 1)}
	dialer := NewDialer()
	raw := newScriptedConn()
	raw.closeCause = make(chan error, 1)
	raw.userdata = &dialSetup{Context: dialCtx, handler: handler, key: testKey}
	dialer.onOpen(raw)

	var cause error
	select {
	case cause = <-raw.closeCause:
	case <-time.After(time.Second):
		t.Fatal("dial context deadline did not close pending handshake")
	}
	if !errors.Is(cause, context.DeadlineExceeded) {
		t.Fatalf("transport close cause = %v, want %v", cause, context.DeadlineExceeded)
	}
	dialer.onClose(raw, cause)
	select {
	case info := <-handler.closed:
		if !errors.Is(info.Err, context.DeadlineExceeded) {
			t.Fatalf("OnClose error = %v", info.Err)
		}
	case <-time.After(time.Second):
		t.Fatal("deadline did not call OnClose")
	}
}

func TestEstablishedConnectionOutlivesDialContext(t *testing.T) {
	dialCtx, cancel := context.WithCancel(context.Background())
	dialer := NewDialer()
	raw := newScriptedConn()
	raw.closeCause = make(chan error, 1)
	raw.userdata = &dialSetup{Context: dialCtx, key: testKey}
	dialer.onOpen(raw)
	conn := raw.userdata.(*Conn)
	if !conn.markOpened() {
		t.Fatal("connection did not complete handshake")
	}

	cancel()
	select {
	case <-dialCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("dial context was not canceled")
	}
	select {
	case cause := <-raw.closeCause:
		t.Fatalf("dial cancellation closed established connection: %v", cause)
	case <-time.After(20 * time.Millisecond):
	}

	closeErr := errors.New("transport closed")
	dialer.onClose(raw, closeErr)
	if !conn.IsClosed() {
		t.Fatal("transport close did not close established connection")
	}
}

func TestDialerCloseCancelsInFlightAttempt(t *testing.T) {
	type contextKey struct{}
	dialer := NewDialer()
	attemptCtx, cleanup, err := dialer.newAttemptContext(
		context.WithValue(context.Background(), contextKey{}, "attempt"),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	closeErr := errors.New("dialer closed during dial")
	if err = dialer.Close(closeErr); err != nil {
		t.Fatal(err)
	}
	select {
	case <-attemptCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("Dialer.Close did not cancel in-flight attempt")
	}
	if !errors.Is(context.Cause(attemptCtx), closeErr) {
		t.Fatalf("attempt context cause = %v, want %v", context.Cause(attemptCtx), closeErr)
	}
	if got := attemptCtx.Value(contextKey{}); got != "attempt" {
		t.Fatalf("attempt context value = %v", got)
	}
}

func TestDialContextCancelsDuringDialerStartup(t *testing.T) {
	dialer := NewDialer()
	entered := make(chan struct{})
	release := make(chan struct{})
	dialer.Events.OnStart = func(*uio.Events) {
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
		_, err := dialer.Dial(ctx, "ws://127.0.0.1:1/", nil)
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

func TestDialerCustomLimitsAndExtensions(t *testing.T) {
	dialer := &Dialer{
		MaxFramePayload:  11,
		MaxMessageSize:   12,
		HandshakeTimeout: 13 * time.Millisecond,
		CompressionLevel: 1,
	}
	if dialer.maxFramePayload() != 11 || dialer.maxMessageSize() != 12 {
		t.Fatal("custom frame/message limits were not preserved")
	}
	if dialer.handshakeTimeout() != 13*time.Millisecond {
		t.Fatal("custom timeouts were not preserved")
	}
	if dialer.compressionLevel() != 1 {
		t.Fatalf("compression level = %d", dialer.compressionLevel())
	}
	if extension := dialer.clientExtensions(); extension != "" {
		t.Fatalf("disabled compression extension = %q", extension)
	}
	dialer.EnableCompression = true
	if extension := dialer.clientExtensions(); !strings.Contains(extension, "permessage-deflate") {
		t.Fatalf("compression extension = %q", extension)
	}

	defaults := new(Dialer)
	if defaults.maxFramePayload() != DefaultMaxFramePayload || defaults.maxMessageSize() != DefaultMaxMessageSize ||
		defaults.handshakeTimeout() != DefaultHandshakeTimeout ||
		defaults.compressionLevel() != -1 {
		t.Fatal("dialer defaults changed")
	}
	if got := (&Conn{config: testDialerConfig(defaults)}).maxOutboundBytes(); got != DefaultMaxOutboundBytes {
		t.Fatalf("default outbound limit = %d, want %d", got, DefaultMaxOutboundBytes)
	}
	dialer.MaxOutboundBytes = -1
	if got := (&Conn{config: testDialerConfig(dialer)}).maxOutboundBytes(); got != -1 {
		t.Fatalf("disabled outbound limit = %d, want -1", got)
	}
	dialer.MaxOutboundBytes = 123
	if got := (&Conn{config: testDialerConfig(dialer)}).maxOutboundBytes(); got != 123 {
		t.Fatalf("custom outbound limit = %d, want 123", got)
	}
	if err := defaults.Close(nil); err != nil {
		t.Fatalf("Dialer.Close with nil Events = %v", err)
	}
}

func TestDialerDisableUTF8CheckConfiguresConnection(t *testing.T) {
	handler := &recordingHandler{}
	dialer := &Dialer{DisableUTF8Check: true}
	setup := &dialSetup{Context: context.Background(), handler: handler}
	conn := dialer.newClientConn(&writeProbeConn{}, setup)
	conn.opened.Store(true)

	if err := conn.acceptFrame(frame.Frame{Fin: true, Opcode: frame.Text, Payload: []byte{0xff}}); err != nil {
		t.Fatalf("incoming text with dialer validation disabled = %v", err)
	}
	handler.mu.Lock()
	defer handler.mu.Unlock()
	if len(handler.messages) != 1 || handler.messages[0] != string([]byte{0xff}) {
		t.Fatalf("messages = %q, want invalid UTF-8 payload", handler.messages)
	}
}

func TestDialerCallbacksHandleLifecycleEdges(t *testing.T) {
	dialer := NewDialer()
	dialer.dispatchBudget.configure(defaultMaxPendingTotalMessages, defaultMaxPendingTotalBytes)

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

	failureHandler := &dialLifecycleHandler{opened: make(chan struct{}, 1), closed: make(chan CloseEvent, 2)}
	raw := newScriptedConn()
	raw.userdata = &dialSetup{handler: failureHandler, key: testKey}
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
	opened.pendingBytes.Store(8)
	dialer.onOutbound(openedRaw, 8)
	if opened.pendingBytes.Load() != 0 {
		t.Fatal("onOutbound did not release pending bytes")
	}
	dialer.onClose(openedRaw, io.EOF)
	dialer.onClose(openedRaw, io.EOF)
	handler.mu.Lock()
	defer handler.mu.Unlock()
	if got := strings.Join(handler.events, ","); got != "close" {
		t.Fatalf("close events = %q", got)
	}
}

func TestDialerCloseBeforeAsyncOnOpenAttachesPendingConnection(t *testing.T) {
	dialer := NewDialer()
	handler := &recordingHandler{}
	cleaned := atomic.Int64{}
	setup := &dialSetup{
		Context: context.Background(),
		cleanup: func() { cleaned.Add(1) },
		handler: handler,
		key:     testKey,
	}
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

func TestDialerHandshakeFailureCallsOnClose(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	serverErr := make(chan error, 1)
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			serverErr <- acceptErr
			return
		}
		defer conn.Close()
		if _, readErr := http.ReadRequest(bufio.NewReader(conn)); readErr != nil {
			serverErr <- readErr
			return
		}
		_, writeErr := io.WriteString(conn, "HTTP/1.1 403 Forbidden\r\nContent-Length: 0\r\n\r\n")
		serverErr <- writeErr
	}()

	handler := &dialLifecycleHandler{opened: make(chan struct{}, 1), closed: make(chan CloseEvent, 1)}
	dialer := NewDialer()
	_, err = dialer.Dial(context.Background(), "ws://"+listener.Addr().String()+"/", handler)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dialer.Close(nil) })

	select {
	case info := <-handler.closed:
		if info.Err == nil {
			t.Fatal("handshake failure OnClose had nil error")
		}
	case err = <-serverErr:
		if err != nil {
			t.Fatal(err)
		}
		select {
		case info := <-handler.closed:
			if info.Err == nil {
				t.Fatal("handshake failure OnClose had nil error")
			}
		case <-time.After(testIOTimeout()):
			t.Fatal("handshake failure did not call OnClose")
		}
	case <-time.After(testIOTimeout()):
		t.Fatal("handshake failure did not call OnClose")
	}
	select {
	case <-handler.opened:
		t.Fatal("failed handshake called OnOpen")
	default:
	}
}

func TestDialerDeadlineClosesRealPendingHandshake(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	serverErr := make(chan error, 1)
	requestRead := make(chan struct{})
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			serverErr <- acceptErr
			return
		}
		defer conn.Close()
		if _, readErr := http.ReadRequest(bufio.NewReader(conn)); readErr != nil {
			serverErr <- readErr
			return
		}
		close(requestRead)
		var buffer [1]byte
		_, readErr := conn.Read(buffer[:])
		if errors.Is(readErr, io.EOF) {
			readErr = nil
		}
		serverErr <- readErr
	}()

	dialCtx, cancel := context.WithCancelCause(context.Background())
	defer cancel(ErrClosed)
	handler := &dialLifecycleHandler{opened: make(chan struct{}, 1), closed: make(chan CloseEvent, 1)}
	dialer := NewDialer()
	_, err = dialer.Dial(dialCtx, "ws://"+listener.Addr().String()+"/", handler)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dialer.Close(nil) })
	select {
	case <-requestRead:
	case err = <-serverErr:
		t.Fatal(err)
	case <-time.After(testIOTimeout()):
		t.Fatal("server did not receive the pending handshake")
	}
	cancel(context.DeadlineExceeded)

	select {
	case info := <-handler.closed:
		if !errors.Is(info.Err, context.DeadlineExceeded) {
			t.Fatalf("OnClose error = %v, want %v", info.Err, context.DeadlineExceeded)
		}
	case err = <-serverErr:
		if err != nil {
			t.Fatal(err)
		}
		select {
		case info := <-handler.closed:
			if !errors.Is(info.Err, context.DeadlineExceeded) {
				t.Fatalf("OnClose error = %v, want %v", info.Err, context.DeadlineExceeded)
			}
		case <-time.After(testIOTimeout()):
			t.Fatal("dial deadline did not call OnClose")
		}
	case <-time.After(testIOTimeout()):
		t.Fatal("dial deadline did not close pending handshake")
	}
	select {
	case <-handler.opened:
		t.Fatal("timed-out handshake called OnOpen")
	default:
	}
}

func TestDialerHandshakeFailureUsesExecutor(t *testing.T) {
	executor := &queuedExecutor{}
	handler := &dialLifecycleHandler{opened: make(chan struct{}, 1), closed: make(chan CloseEvent, 1)}
	dialer := NewDialer()
	dialer.Executor = executor
	raw := newScriptedConn()
	raw.userdata = &dialSetup{handler: handler, key: testKey}
	dialer.onOpen(raw)
	conn := raw.userdata.(*Conn)
	conn.stopHandshakeTimer()
	dialer.onClose(raw, nil)

	if !conn.IsClosed() {
		t.Fatal("handshake failure did not close connection before OnClose dispatch")
	}
	if got := executor.pending(); got != 1 {
		t.Fatalf("executor tasks = %d, want 1", got)
	}
	select {
	case info := <-handler.closed:
		t.Fatalf("OnClose ran before executor: %+v", info)
	default:
	}
	if !executor.runNext() {
		t.Fatal("executor did not run OnClose")
	}
	select {
	case info := <-handler.closed:
		if !errors.Is(info.Err, ErrClosed) {
			t.Fatalf("OnClose error = %v, want %v", info.Err, ErrClosed)
		}
	case <-time.After(time.Second):
		t.Fatal("executor did not deliver OnClose")
	}
}
