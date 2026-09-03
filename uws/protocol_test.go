package uws

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/urpc/uio"
	"github.com/urpc/uio/uws/internal/compress"
	"github.com/urpc/uio/uws/internal/frame"
)

func TestFrameParserIsRetainedOnlyAcrossIncompleteInput(t *testing.T) {
	newConn := func(handler Handler) *Conn {
		conn := &Conn{
			raw:     &writeProbeConn{},
			config:  testServerConfig(NewServer(nil)),
			handler: handler,
		}
		conn.opened.Store(true)
		return conn
	}

	completeHandler := &recordingHandler{}
	completeConn := newConn(completeHandler)
	completeWire := frame.Append(nil, frame.Frame{
		Fin: true, Opcode: frame.Binary, Masked: true, Payload: []byte("complete"),
	}, [4]byte{1, 2, 3, 4})
	if consumed, err := completeConn.feedFrames(completeWire, completeConn.acceptFrame); err != nil || consumed != len(completeWire) {
		t.Fatalf("complete feed = %d, %v", consumed, err)
	}
	if completeConn.parser != nil {
		t.Fatal("complete frame retained an incremental parser")
	}

	incrementalHandler := &recordingHandler{}
	incrementalConn := newConn(incrementalHandler)
	incrementalWire := frame.Append(nil, frame.Frame{
		Fin: true, Opcode: frame.Binary, Masked: true, Payload: []byte("incremental"),
	}, [4]byte{5, 6, 7, 8})
	split := 4
	if consumed, err := incrementalConn.feedFrames(incrementalWire[:split], incrementalConn.acceptFrame); err != nil || consumed != split {
		t.Fatalf("partial feed = %d, %v", consumed, err)
	}
	if incrementalConn.parser == nil {
		t.Fatal("incomplete frame did not retain an incremental parser")
	}
	if consumed, err := incrementalConn.feedFrames(incrementalWire[split:], incrementalConn.acceptFrame); err != nil || consumed != len(incrementalWire)-split {
		t.Fatalf("completion feed = %d, %v", consumed, err)
	}
	if incrementalConn.parser != nil {
		t.Fatal("completed incremental frame retained its parser")
	}
	incrementalHandler.mu.Lock()
	defer incrementalHandler.mu.Unlock()
	if got := strings.Join(incrementalHandler.messages, ","); got != "incremental" {
		t.Fatalf("incremental messages = %q", got)
	}
}

func TestAssemblerIsRetainedOnlyForFragmentedMessages(t *testing.T) {
	handler := &assemblerStateHandler{}
	conn := &Conn{
		raw:     &writeProbeConn{},
		config:  testServerConfig(NewServer(nil)),
		handler: handler,
	}
	conn.opened.Store(true)

	if err := conn.acceptFrame(frame.Frame{Fin: true, Opcode: frame.Binary, Payload: []byte("complete")}); err != nil {
		t.Fatal(err)
	}
	if conn.assembler != nil || handler.assemblerVisible {
		t.Fatal("complete frame retained or exposed an assembler")
	}

	if err := conn.acceptFrame(frame.Frame{Opcode: frame.Text, Payload: []byte("frag")}); err != nil {
		t.Fatal(err)
	}
	assembler := conn.assembler
	if assembler == nil {
		t.Fatal("first fragment did not retain an assembler")
	}
	if err := conn.acceptFrame(frame.Frame{Fin: true, Opcode: frame.Ping}); err != nil {
		t.Fatal(err)
	}
	if conn.assembler != assembler {
		t.Fatal("interleaved control frame released fragmented message state")
	}
	if err := conn.acceptFrame(frame.Frame{Fin: true, Opcode: frame.Continuation, Payload: []byte("mented")}); err != nil {
		t.Fatal(err)
	}
	if !handler.assemblerVisible {
		t.Fatal("assembler was released before OnMessage returned")
	}
	if conn.assembler != nil {
		t.Fatal("completed fragmented message retained its assembler")
	}
	if got := strings.Join(handler.messages, ","); got != "complete,fragmented" {
		t.Fatalf("messages = %q, want complete,fragmented", got)
	}
}

func TestAssemblerIsReleasedAfterErrorAndTransportClose(t *testing.T) {
	config := testServerConfig(&Server{EnableCompression: true})
	conn := &Conn{raw: &writeProbeConn{}, config: config}
	conn.opened.Store(true)
	if err := conn.acceptFrame(frame.Frame{RSV1: true, Opcode: frame.Binary, Payload: []byte("compressed")}); err != nil {
		t.Fatal(err)
	}
	if conn.assembler == nil {
		t.Fatal("compressed fragment did not retain an assembler")
	}
	if err := conn.acceptFrame(frame.Frame{Fin: true, Opcode: frame.Continuation}); !errors.Is(err, frame.ErrProtocol) {
		t.Fatalf("missing decoder error = %v, want %v", err, frame.ErrProtocol)
	}
	if conn.assembler != nil {
		t.Fatal("message callback error retained assembler state")
	}

	server := NewServer(nil)
	transport := newScriptedConn()
	conn = &Conn{raw: transport, config: testServerConfig(server)}
	conn.opened.Store(true)
	if err := conn.acceptFrame(frame.Frame{Opcode: frame.Binary, Payload: []byte("partial")}); err != nil {
		t.Fatal(err)
	}
	transport.userdata = conn
	server.onClose(transport, io.EOF)
	if conn.assembler != nil {
		t.Fatal("transport close retained assembler state")
	}
}

func TestReleaseParserDropsIncompleteState(t *testing.T) {
	conn := &Conn{config: testServerConfig(NewServer(nil))}
	wire := frame.Append(nil, frame.Frame{
		Fin: true, Opcode: frame.Binary, Masked: true, Payload: []byte("payload"),
	}, [4]byte{1, 2, 3, 4})
	if _, err := conn.feedFrames(wire[:4], func(frame.Frame) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if conn.parser == nil {
		t.Fatal("partial frame did not retain parser state")
	}
	conn.releaseParser()
	if conn.parser != nil {
		t.Fatal("release retained parser state")
	}
}

func TestIncrementalParserReturnsToPoolAfterCallbackError(t *testing.T) {
	conn := &Conn{config: testDialerConfig(NewDialer())}
	wire := frame.Append(nil, frame.Frame{Fin: true, Opcode: frame.Binary, Payload: []byte("payload")}, [4]byte{})
	if _, err := conn.feedFrames(wire[:1], func(frame.Frame) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if conn.parser == nil {
		t.Fatal("partial frame did not retain parser state")
	}
	wantErr := errors.New("stop after frame")
	if _, err := conn.feedFrames(wire[1:], func(frame.Frame) error { return wantErr }); !errors.Is(err, wantErr) {
		t.Fatalf("callback error = %v, want %v", err, wantErr)
	}
	if conn.parser != nil {
		t.Fatal("frame-boundary callback error retained parser state")
	}
}

func TestIncrementalParserReturnsToPoolAfterProtocolError(t *testing.T) {
	conn := &Conn{config: testServerConfig(NewServer(nil))}
	if _, err := conn.feedFrames([]byte{0x82}, func(frame.Frame) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if conn.parser == nil {
		t.Fatal("partial header did not retain parser state")
	}
	if _, err := conn.feedFrames([]byte{0x00}, func(frame.Frame) error { return nil }); !errors.Is(err, frame.ErrProtocol) {
		t.Fatalf("unmasked frame error = %v, want %v", err, frame.ErrProtocol)
	}
	if conn.parser != nil {
		t.Fatal("protocol error retained incremental parser state")
	}
}

func TestReadAvailableStopsWhenTransportPausesRead(t *testing.T) {
	first := frame.Append(nil, frame.Frame{Fin: true, Opcode: frame.Binary, Masked: true, Payload: []byte("a")}, [4]byte{1, 2, 3, 4})
	second := frame.Append(nil, frame.Frame{Fin: true, Opcode: frame.Binary, Masked: true, Payload: []byte("b")}, [4]byte{5, 6, 7, 8})
	raw := &bufferedProbeConn{}
	raw.inbound = append(first, second...)
	handler := &pauseAfterMessageHandler{}
	conn := &Conn{
		raw:     raw,
		handler: handler,
		config:  testServerConfig(NewServer(nil)),
	}
	conn.opened.Store(true)

	if err := conn.readAvailable(); err != nil {
		t.Fatal(err)
	}
	if handler.messages != 1 {
		t.Fatalf("messages delivered = %d, want 1", handler.messages)
	}
	if got, want := raw.InboundBuffered(), len(second); got != want {
		t.Fatalf("inbound bytes = %d, want %d", got, want)
	}
	if raw.wakes != 0 {
		t.Fatalf("wake calls = %d, want 0 while reads are paused", raw.wakes)
	}
}

func TestReadAvailableParsesFrameAcrossInboundSegments(t *testing.T) {
	wire := frame.Append(nil, frame.Frame{Fin: true, Opcode: frame.Binary, Masked: true, Payload: []byte("payload")}, [4]byte{1, 2, 3, 4})
	raw := newSegmentedProbeConn(wire[:1], wire[1:4], wire[4:])
	handler := &recordingHandler{}
	conn := &Conn{
		raw:     raw,
		handler: handler,
		config:  testServerConfig(NewServer(nil)),
	}
	conn.opened.Store(true)

	if err := conn.readAvailable(); err != nil {
		t.Fatal(err)
	}
	if raw.InboundBuffered() != 0 {
		t.Fatalf("inbound bytes after parse = %d, want 0", raw.InboundBuffered())
	}
	handler.mu.Lock()
	defer handler.mu.Unlock()
	if got, want := strings.Join(handler.messages, ","), "payload"; got != want {
		t.Fatalf("messages = %q, want %q", got, want)
	}
}

func TestReadAvailableParsesHandshakeAcrossInboundSegments(t *testing.T) {
	request := []byte("GET /chat HTTP/1.1\r\n" +
		"Host: example.com\r\n" +
		"Connection: Upgrade\r\n" +
		"Upgrade: websocket\r\n" +
		"Sec-WebSocket-Version: 13\r\n" +
		"Sec-WebSocket-Key: " + testKey + "\r\n\r\n")
	raw := newSegmentedProbeConn(request[:1], request[1:17], request[17:])
	handler := &recordingHandler{}
	server := NewServer(handler)
	conn := &Conn{
		raw:     raw,
		handler: handler,
		config:  testServerConfig(server),
	}
	conn.handshake.Store(&handshakeState{})

	if err := conn.readAvailable(); err != nil {
		t.Fatal(err)
	}
	if !conn.opened.Load() {
		t.Fatal("segmented handshake did not open connection")
	}
	if raw.InboundBuffered() != 0 {
		t.Fatalf("inbound bytes after handshake = %d, want 0", raw.InboundBuffered())
	}
	if raw.writes != 1 {
		t.Fatalf("handshake response writes = %d, want 1", raw.writes)
	}
	if conn.handshake.Load() != nil {
		t.Fatal("completed handshake retained handshake state")
	}
	if conn.dispatch != nil || conn.compression != nil || conn.metadata.Load() != nil {
		t.Fatal("unconfigured connection allocated optional steady-state data")
	}
	tracked := 0
	server.connections.Range(func(_, _ any) bool {
		tracked++
		return true
	})
	if tracked != 0 {
		t.Fatalf("heartbeat-disabled server tracked %d connections", tracked)
	}
	handler.mu.Lock()
	defer handler.mu.Unlock()
	if got, want := strings.Join(handler.events, ","), "open"; got != want {
		t.Fatalf("lifecycle events = %q, want %q", got, want)
	}
}

func TestReadBudgetDoesNotReplayIncrementalFrame(t *testing.T) {
	var wire []byte
	for i := 0; i < maxFramesPerDataEvent+1; i++ {
		wire = frame.Append(wire, frame.Frame{
			Fin:     true,
			Opcode:  frame.Binary,
			Masked:  true,
			Payload: []byte{byte(i)},
		}, [4]byte{1, 2, 3, 4})
	}
	segments := make([][]byte, len(wire))
	for index := range wire {
		segments[index] = wire[index : index+1]
	}
	raw := newSegmentedProbeConn(segments...)
	handler := &recordingHandler{}
	conn := &Conn{
		raw:     raw,
		handler: handler,
		config:  testServerConfig(NewServer(nil)),
	}
	conn.opened.Store(true)
	if err := conn.readAvailable(); err != nil {
		t.Fatal(err)
	}
	handler.mu.Lock()
	firstBatch := len(handler.messages)
	handler.mu.Unlock()
	if firstBatch != maxFramesPerDataEvent {
		t.Fatalf("first batch messages = %d, want %d", firstBatch, maxFramesPerDataEvent)
	}
	if raw.wakes != 1 {
		t.Fatalf("wake calls after first batch = %d, want 1", raw.wakes)
	}
	if err := conn.readAvailable(); err != nil {
		t.Fatal(err)
	}
	handler.mu.Lock()
	defer handler.mu.Unlock()
	if len(handler.messages) != maxFramesPerDataEvent+1 {
		t.Fatalf("total messages = %d, want %d", len(handler.messages), maxFramesPerDataEvent+1)
	}
	if raw.wakes != 1 {
		t.Fatalf("total wake calls = %d, want 1", raw.wakes)
	}
}

func TestReadBudgetWakeFailureIsReturned(t *testing.T) {
	var wire []byte
	for i := 0; i < maxFramesPerDataEvent+1; i++ {
		wire = frame.Append(wire, frame.Frame{
			Fin:     true,
			Opcode:  frame.Binary,
			Masked:  true,
			Payload: []byte{byte(i)},
		}, [4]byte{1, 2, 3, 4})
	}
	wakeErr := errors.New("wake failed")
	raw := newSegmentedProbeConn(wire)
	raw.wakeErr = wakeErr
	conn := &Conn{
		raw:     raw,
		handler: &recordingHandler{},
		config:  testServerConfig(NewServer(nil)),
	}
	conn.opened.Store(true)

	if err := conn.readAvailable(); !errors.Is(err, wakeErr) {
		t.Fatalf("read error = %v, want wrapped %v", err, wakeErr)
	}
	if raw.wakes != 1 {
		t.Fatalf("wake calls = %d, want 1", raw.wakes)
	}
}

type budgetIntegrationHandler struct {
	opened   chan struct{}
	messages chan Message
}

func (h *budgetIntegrationHandler) OnOpen(*Conn) { h.opened <- struct{}{} }

func (h *budgetIntegrationHandler) OnMessage(_ *Conn, message Message) {
	h.messages <- message.Clone()
}

func (*budgetIntegrationHandler) OnClose(*Conn, CloseEvent) {}

func TestReadBudgetSchedulesBufferedFramesAndDoesNotStarvePeer(t *testing.T) {
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := probe.Addr().String()
	_ = probe.Close()

	const primaryFrames = maxFramesPerDataEvent + 1
	handler := &budgetIntegrationHandler{
		opened:   make(chan struct{}, 2),
		messages: make(chan Message, primaryFrames+1),
	}
	server := NewServer(handler)
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

	openClient := func() net.Conn {
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
		t.Cleanup(func() { _ = client.Close() })
		if err = client.SetDeadline(time.Now().Add(testIOTimeout())); err != nil {
			t.Fatal(err)
		}
		request := "GET /chat HTTP/1.1\r\nHost: " + addr + "\r\n" +
			"Connection: Upgrade\r\nUpgrade: websocket\r\n" +
			"Sec-WebSocket-Version: 13\r\nSec-WebSocket-Key: " + testKey + "\r\n\r\n"
		if _, err = io.WriteString(client, request); err != nil {
			t.Fatal(err)
		}
		reader := bufio.NewReader(client)
		status, readErr := reader.ReadString('\n')
		if readErr != nil {
			t.Fatal(readErr)
		}
		if !strings.HasPrefix(status, "HTTP/1.1 101 ") {
			t.Fatalf("response = %q", status)
		}
		for {
			line, readErr := reader.ReadString('\n')
			if readErr != nil {
				t.Fatal(readErr)
			}
			if line == "\r\n" {
				break
			}
		}
		if err = client.SetDeadline(time.Time{}); err != nil {
			t.Fatal(err)
		}
		return client
	}

	primary := openClient()
	peer := openClient()
	for i := 0; i < 2; i++ {
		select {
		case <-handler.opened:
		case <-time.After(testIOTimeout()):
			t.Fatal("OnOpen was not called")
		}
	}

	var primaryWire []byte
	for i := 0; i < primaryFrames; i++ {
		primaryWire = frame.Append(primaryWire, frame.Frame{
			Fin: true, Opcode: frame.Binary, Masked: true, Payload: []byte{0, byte(i)},
		}, [4]byte{1, 2, 3, 4})
	}
	peerWire := frame.Append(nil, frame.Frame{
		Fin: true, Opcode: frame.Binary, Masked: true, Payload: []byte{1, 0},
	}, [4]byte{4, 3, 2, 1})
	if _, err = primary.Write(primaryWire); err != nil {
		t.Fatal(err)
	}
	if _, err = peer.Write(peerWire); err != nil {
		t.Fatal(err)
	}

	nextPrimary := 0
	peerReceived := false
	for nextPrimary < primaryFrames || !peerReceived {
		select {
		case message := <-handler.messages:
			if message.Type != BinaryMessage || len(message.Payload) != 2 {
				t.Fatalf("unexpected message = %+v", message)
			}
			switch message.Payload[0] {
			case 0:
				if got := int(message.Payload[1]); got != nextPrimary {
					t.Fatalf("primary message index = %d, want %d", got, nextPrimary)
				}
				nextPrimary++
			case 1:
				if peerReceived {
					t.Fatal("peer message delivered more than once")
				}
				peerReceived = true
			default:
				t.Fatalf("unexpected connection marker = %d", message.Payload[0])
			}
		case <-time.After(testIOTimeout()):
			t.Fatalf("received %d/%d primary messages, peer received = %v", nextPrimary, primaryFrames, peerReceived)
		}
	}
}

func TestServerHandshakeAndMaskedMessage(t *testing.T) {
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := probe.Addr().String()
	_ = probe.Close()

	handler := &echoHandler{open: make(chan struct{}), closed: make(chan struct{}), message: make(chan Message, 1)}
	server := NewServer(handler)
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
		client, err = net.Dial("tcp", addr)
		if err != nil {
			time.Sleep(time.Millisecond)
		}
	}
	if client == nil {
		t.Fatal(err)
	}
	defer client.Close()

	request := "GET /chat HTTP/1.1\r\n" +
		"Host: " + addr + "\r\n" +
		"Connection: Upgrade\r\nUpgrade: websocket\r\n" +
		"Sec-WebSocket-Version: 13\r\nSec-WebSocket-Key: " + testKey + "\r\n\r\n"
	masked := frame.Append(nil, frame.Frame{Fin: true, Opcode: frame.Text, Masked: true, Payload: []byte("hello")}, [4]byte{1, 2, 3, 4})
	if _, err = client.Write(append([]byte(request), masked...)); err != nil {
		t.Fatal(err)
	}

	reader := bufio.NewReader(client)
	response, err := reader.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(response, "HTTP/1.1 101 ") {
		t.Fatalf("response = %q", response)
	}
	for {
		line, readErr := reader.ReadString('\n')
		if readErr != nil {
			t.Fatal(readErr)
		}
		if line == "\r\n" {
			break
		}
	}

	select {
	case <-handler.open:
	case <-time.After(testIOTimeout()):
		t.Fatal("OnOpen was not called")
	}
	select {
	case message := <-handler.message:
		if message.Type != TextMessage || string(message.Payload) != "hello" {
			t.Fatalf("message = %+v", message)
		}
	case <-time.After(testIOTimeout()):
		t.Fatal("OnMessage was not called")
	}

	var header [2]byte
	if _, err = io.ReadFull(reader, header[:]); err != nil {
		t.Fatal(err)
	}
	if header[0] != 0x81 || header[1] != 5 {
		t.Fatalf("response frame header = %x", header)
	}
	payload := make([]byte, 5)
	if _, err = io.ReadFull(reader, payload); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(payload, []byte("world")) {
		t.Fatalf("response payload = %q", payload)
	}

	ping := frame.Append(nil, frame.Frame{Fin: true, Opcode: frame.Ping, Masked: true, Payload: []byte("?")}, [4]byte{4, 3, 2, 1})
	if _, err = client.Write(ping); err != nil {
		t.Fatal(err)
	}
	if _, err = io.ReadFull(reader, header[:]); err != nil {
		t.Fatal(err)
	}
	if header[0] != 0x8a || header[1] != 1 {
		t.Fatalf("pong frame header = %x", header)
	}
	if _, err = reader.ReadByte(); err != nil {
		t.Fatal(err)
	}

	closePayload := []byte{3, 232}
	closeFrame := frame.Append(nil, frame.Frame{Fin: true, Opcode: frame.Close, Masked: true, Payload: closePayload}, [4]byte{5, 6, 7, 8})
	if _, err = client.Write(closeFrame); err != nil {
		t.Fatal(err)
	}
	if _, err = io.ReadFull(reader, header[:]); err != nil {
		t.Fatal(err)
	}
	if header[0] != 0x88 || header[1] != 2 {
		t.Fatalf("close frame header = %x", header)
	}
	closeReply := make([]byte, 2)
	if _, err = io.ReadFull(reader, closeReply); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(closeReply, closePayload) {
		t.Fatalf("close payload = %x", closeReply)
	}
	select {
	case <-handler.closed:
	case <-time.After(testIOTimeout()):
		t.Fatal("OnClose was not called")
	}
}

func TestCloseWaitsForPeerClose(t *testing.T) {
	raw := &writeProbeConn{}
	server := &Server{MaxFramePayload: 1024, MaxMessageSize: 1024, MaxOutboundBytes: 1 << 20, CloseTimeout: time.Second}
	conn := &Conn{raw: raw, config: testServerConfig(server)}
	conn.opened.Store(true)
	if err := conn.Close(1000, ""); err != nil {
		t.Fatal(err)
	}
	if raw.closes != 0 {
		t.Fatalf("transport closed before peer close: %d", raw.closes)
	}
	if err := conn.acceptControl(frame.Frame{Fin: true, Opcode: frame.Close, Payload: []byte{3, 232}}); err != nil {
		t.Fatal(err)
	}
	completeTestOutbound(conn)
	if raw.closes != 1 {
		t.Fatalf("transport closes after peer close = %d, want 1", raw.closes)
	}
}

func TestCloseTimeoutTerminatesUnresponsivePeer(t *testing.T) {
	raw := &writeProbeConn{closed: make(chan struct{})}
	server := &Server{MaxFramePayload: 1024, MaxMessageSize: 1024, MaxOutboundBytes: 1 << 20, CloseTimeout: 5 * time.Millisecond}
	conn := &Conn{raw: raw, config: testServerConfig(server)}
	conn.opened.Store(true)
	if err := conn.Close(1000, ""); err != nil {
		t.Fatal(err)
	}
	select {
	case <-raw.closed:
	case <-time.After(time.Second):
		t.Fatal("close timeout did not terminate transport")
	}
}

func TestCloseDoesNotRecreateTimerAfterTransportClose(t *testing.T) {
	raw := newScriptedConn()
	server := NewServer(nil)
	conn := &Conn{raw: raw, config: testServerConfig(server)}
	conn.opened.Store(true)
	raw.userdata = conn
	raw.flushHook = func() {
		server.onClose(raw, io.EOF)
	}

	if err := conn.Close(1000, ""); err != nil {
		t.Fatal(err)
	}
	if !conn.closed.Load() {
		t.Fatal("transport close did not mark connection closed")
	}
	if state := conn.closeTimer.Load(); state != nil {
		state.mu.Lock()
		timer := state.timer
		state.mu.Unlock()
		if timer != nil {
			t.Fatal("closed connection recreated its close timer")
		}
	}
}

func TestClosedConnectionDoesNotCreateCloseTimer(t *testing.T) {
	conn := &Conn{raw: &writeProbeConn{}}
	conn.closed.Store(true)
	conn.startCloseTimer()
	conn.ensureCloseTimer()
	if conn.closeTimer.Load() != nil {
		t.Fatal("closed connection allocated close timer state")
	}
}

func TestPingRejectedAfterCloseStarts(t *testing.T) {
	raw := &writeProbeConn{}
	server := &Server{MaxFramePayload: 1024, MaxMessageSize: 1024, MaxOutboundBytes: 1 << 20, CloseTimeout: time.Second}
	conn := &Conn{raw: raw, config: testServerConfig(server)}
	conn.opened.Store(true)
	if err := conn.Close(1000, ""); err != nil {
		t.Fatal(err)
	}
	if err := conn.Ping(nil); err != ErrClosed {
		t.Fatalf("Ping() after Close = %v, want %v", err, ErrClosed)
	}
	_ = conn.closeTransport()
}

func TestSendFrameLockedRejectsDataAfterClosing(t *testing.T) {
	raw := &writeProbeConn{}
	server := &Server{MaxFramePayload: 1024, MaxMessageSize: 1024, MaxOutboundBytes: 1 << 20}
	conn := &Conn{raw: raw, config: testServerConfig(server)}
	conn.opened.Store(true)
	conn.closing.Store(true)

	if err := conn.sendFrameLocked(frame.Frame{Fin: true, Opcode: frame.Text, Payload: []byte("late")}); err != ErrClosed {
		t.Fatalf("data frame after closing = %v, want %v", err, ErrClosed)
	}
	if raw.writes != 0 {
		t.Fatalf("late data frame writes = %d, want 0", raw.writes)
	}
}

func TestClientHandshakeAcceptsSplitResponse(t *testing.T) {
	response := []byte("HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: websocket\r\nConnection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: s3pPLMBiTxaQ9kYGzzhZRbK+xOo=\r\n" +
		"Sec-WebSocket-Protocol: chat\r\n\r\n")
	dialer := &Dialer{MaxHeaderBytes: DefaultMaxHeaderBytes, Subprotocols: []string{"chat"}}
	conn := &Conn{
		raw:    &writeProbeConn{},
		config: testDialerConfig(dialer),
	}
	conn.handshake.Store(&handshakeState{clientKey: testKey})
	split := len(response) / 2
	if err := conn.consumeHandshake(response[:split]); err != nil {
		t.Fatalf("first handshake fragment = %v", err)
	}
	if conn.opened.Load() {
		t.Fatal("connection opened before complete handshake")
	}
	if err := conn.consumeHandshake(response[split:]); err != nil {
		t.Fatalf("second handshake fragment = %v", err)
	}
	if !conn.opened.Load() {
		t.Fatal("connection did not open after complete handshake")
	}
	if protocol := conn.Subprotocol(); protocol != "chat" {
		t.Fatalf("negotiated subprotocol = %q, want chat", protocol)
	}
}

func TestServerPerMessageDeflate(t *testing.T) {
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := probe.Addr().String()
	_ = probe.Close()

	handler := &echoHandler{open: make(chan struct{}), closed: make(chan struct{}), message: make(chan Message, 1)}
	server := NewServer(handler)
	server.EnableCompression = true
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
	request := "GET /chat HTTP/1.1\r\nHost: " + addr + "\r\n" +
		"Connection: Upgrade\r\nUpgrade: websocket\r\n" +
		"Sec-WebSocket-Version: 13\r\nSec-WebSocket-Key: " + testKey + "\r\n" +
		"Sec-WebSocket-Extensions: permessage-deflate; server_max_window_bits=8; client_max_window_bits=8\r\n\r\n"
	compressed, err := compress.Compress([]byte("hello hello hello"), -1)
	if err != nil {
		t.Fatal(err)
	}
	split := len(compressed) / 2
	wire := frame.Append(nil, frame.Frame{RSV1: true, Opcode: frame.Text, Masked: true, Payload: compressed[:split]}, [4]byte{9, 8, 7, 6})
	wire = frame.Append(wire, frame.Frame{Fin: true, Opcode: frame.Continuation, Masked: true, Payload: compressed[split:]}, [4]byte{6, 7, 8, 9})
	if _, err = client.Write(append([]byte(request), wire...)); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(client)
	response, err := reader.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(response, "HTTP/1.1 101 ") {
		t.Fatalf("response = %q", response)
	}
	responseHeaders := response
	for {
		line, readErr := reader.ReadString('\n')
		if readErr != nil {
			t.Fatal(readErr)
		}
		responseHeaders += line
		if line == "\r\n" {
			break
		}
	}
	if !strings.Contains(responseHeaders, "Sec-WebSocket-Extensions: permessage-deflate;") {
		t.Fatalf("response did not negotiate deflate: %s", responseHeaders)
	}
	select {
	case message := <-handler.message:
		if message.Type != TextMessage || string(message.Payload) != "hello hello hello" {
			t.Fatalf("message = %+v", message)
		}
	case <-time.After(testIOTimeout()):
		t.Fatal("compressed OnMessage was not called")
	}
	var header [2]byte
	if _, err = io.ReadFull(reader, header[:]); err != nil {
		t.Fatal(err)
	}
	if header[0] != 0x81 || header[1] != 5 {
		t.Fatalf("response frame header = %x", header)
	}
	payload := make([]byte, 5)
	if _, err = io.ReadFull(reader, payload); err != nil {
		t.Fatal(err)
	}
	if string(payload) != "world" {
		t.Fatalf("response payload = %q", payload)
	}
}

func TestCompressedMessageLimitUsesDecodedSize(t *testing.T) {
	equalPayload := make([]byte, 128)
	for index := range equalPayload {
		equalPayload[index] = byte(index*37 + index*index + 11)
	}
	copy(equalPayload, bytes.Repeat([]byte{'x'}, 11))
	// Fixed permessage-deflate wire data keeps this boundary independent of
	// changes to the encoder's compression strategy.
	equalWire, err := hex.DecodeString("aa4000e9f0a957c5a317be34ae3f2b9b7f58b2f0b47aeb53f7d582958ffdf7eace17ee64acfe5efab5fc7723e764f9b5b6e7937f4ed43e9ecabadcfbe34ce7f7737dff6fce54bc3d3d54f4faec44cdcffb7b63f5996f6eec4e7755657f7d71e7e2fedadcf8604f476b735353736b47cfe0f8dcdafec53b2fbe6657754d070c00")
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name     string
		payload  []byte
		wire     []byte
		relation func(wire, decoded int) bool
	}{
		{
			name:    "wire smaller than decoded",
			payload: bytes.Repeat([]byte{'x'}, 1024),
			relation: func(wire, decoded int) bool {
				return wire < decoded
			},
		},
		{
			name:    "wire equal to decoded",
			payload: equalPayload,
			wire:    equalWire,
			relation: func(wire, decoded int) bool {
				return wire == decoded
			},
		},
		{
			name:    "wire larger than decoded",
			payload: []byte("xx"),
			relation: func(wire, decoded int) bool {
				return wire > decoded
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			encoded := test.wire
			if encoded == nil {
				encoded, err = compress.Compress(test.payload, -1)
				if err != nil {
					t.Fatal(err)
				}
			}
			if !test.relation(len(encoded), len(test.payload)) {
				t.Fatalf("fixture wire size = %d, decoded size = %d", len(encoded), len(test.payload))
			}

			for _, fragmented := range []bool{false, true} {
				mode := "complete"
				frames := []frame.Frame{{Fin: true, RSV1: true, Opcode: frame.Binary, Payload: encoded}}
				if fragmented {
					mode = "fragmented"
					split := len(encoded) / 2
					frames = []frame.Frame{
						{RSV1: true, Opcode: frame.Binary, Payload: encoded[:split]},
						{Fin: true, Opcode: frame.Continuation, Payload: encoded[split:]},
					}
				}
				t.Run(mode, func(t *testing.T) {
					accept := func(limit uint64, handler *recordingHandler) error {
						server := &Server{MaxFramePayload: uint64(len(encoded)), MaxMessageSize: limit}
						conn := &Conn{
							raw:         &writeProbeConn{},
							config:      testServerConfig(server),
							handler:     handler,
							compression: &compressionState{decoder: compress.NewDecoder(true)},
						}
						var acceptErr error
						for _, f := range frames {
							if acceptErr = conn.acceptFrame(f); acceptErr != nil {
								break
							}
						}
						return acceptErr
					}

					handler := &recordingHandler{}
					if err = accept(uint64(len(test.payload)), handler); err != nil {
						t.Fatalf("message at decoded limit: %v", err)
					}
					handler.mu.Lock()
					if len(handler.messages) != 1 || handler.messages[0] != string(test.payload) {
						t.Fatalf("messages = %q, want one decoded payload", handler.messages)
					}
					handler.mu.Unlock()

					rejected := &recordingHandler{}
					if err = accept(uint64(len(test.payload)-1), rejected); !errors.Is(err, compress.ErrTooLarge) {
						t.Fatalf("message above decoded limit error = %v, want %v", err, compress.ErrTooLarge)
					}
					rejected.mu.Lock()
					if len(rejected.messages) != 0 {
						t.Fatalf("messages delivered above limit = %d, want 0", len(rejected.messages))
					}
					rejected.mu.Unlock()
				})
			}
		})
	}
}
