package uws

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/urpc/uio/uws/internal/compress"
	"github.com/urpc/uio/uws/internal/frame"
)

func TestFrameParserIsRetainedOnlyAcrossIncompleteInput(t *testing.T) {
	newConn := func(handler Handler) *Conn {
		conn := &Conn{
			raw:     &writeProbeConn{},
			config:  testServerConfig(NewServer(Options{})),
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
		config:  testServerConfig(NewServer(Options{})),
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
	config := testServerConfig(&Server{Options: Options{EnableCompression: true}})
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

	server := NewServer(Options{})
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
	conn := &Conn{config: testServerConfig(NewServer(Options{}))}
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
	conn := &Conn{config: testDialerConfig(NewDialer(Options{}))}
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
	conn := &Conn{config: testServerConfig(NewServer(Options{}))}
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

func TestReadAvailableParsesFrameAcrossInboundSegments(t *testing.T) {
	wire := frame.Append(nil, frame.Frame{Fin: true, Opcode: frame.Binary, Masked: true, Payload: []byte("payload")}, [4]byte{1, 2, 3, 4})
	raw := newSegmentedProbeConn(wire[:1], wire[1:4], wire[4:])
	handler := &recordingHandler{}
	conn := &Conn{
		raw:     raw,
		handler: handler,
		config:  testServerConfig(NewServer(Options{})),
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
	server := NewServer(Options{Handler: handler})
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
	if conn.compression != nil || conn.metadata.Load() != nil {
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
	config := testServerConfig(NewServer(Options{}))
	var wire []byte
	for i := 0; i < config.maxFramesPerDataEvent+1; i++ {
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
		config:  testServerConfig(NewServer(Options{})),
	}
	conn.opened.Store(true)
	if err := conn.readAvailable(); err != nil {
		t.Fatal(err)
	}
	handler.mu.Lock()
	firstBatch := len(handler.messages)
	handler.mu.Unlock()
	if want := config.maxFramesPerDataEvent; firstBatch != want {
		t.Fatalf("first batch messages = %d, want %d", firstBatch, want)
	}
	if raw.wakes != 1 {
		t.Fatalf("wake calls after first batch = %d, want 1", raw.wakes)
	}
	if err := conn.readAvailable(); err != nil {
		t.Fatal(err)
	}
	handler.mu.Lock()
	defer handler.mu.Unlock()
	if len(handler.messages) != minFramesPerDataEvent+1 {
		t.Fatalf("total messages = %d, want %d", len(handler.messages), minFramesPerDataEvent+1)
	}
	if raw.wakes != 1 {
		t.Fatalf("total wake calls = %d, want 1", raw.wakes)
	}
}

func TestReadBudgetWakeFailureIsReturned(t *testing.T) {
	var wire []byte
	for i := 0; i < minFramesPerDataEvent+1; i++ {
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
		config:  testServerConfig(NewServer(Options{})),
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
	const primaryFrames = minFramesPerDataEvent + 1
	handler := &budgetIntegrationHandler{
		opened:   make(chan struct{}, 2),
		messages: make(chan Message, primaryFrames+1),
	}
	server, addr, serveDone := startTestServer(t, func() *Server {
		s := NewServer(Options{Handler: handler})
		s.Options.Pollers = 1
		s.Options.ReadBufferSize = 4 << 10
		return s
	})
	t.Cleanup(func() { stopTestServer(t, server, serveDone) })

	var err error
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
			Fin: true, Opcode: frame.Binary, Masked: true, Payload: []byte{0, byte(i >> 8), byte(i)},
		}, [4]byte{1, 2, 3, 4})
	}
	peerWire := frame.Append(nil, frame.Frame{
		Fin: true, Opcode: frame.Binary, Masked: true, Payload: []byte{1, 0, 0},
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
			if message.Type != BinaryMessage || len(message.Payload) != 3 {
				t.Fatalf("unexpected message = %+v", message)
			}
			switch message.Payload[0] {
			case 0:
				if got := int(message.Payload[1])<<8 | int(message.Payload[2]); got != nextPrimary {
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
	handler := &echoHandler{open: make(chan struct{}), closed: make(chan struct{}), message: make(chan Message, 1)}
	server, addr, serveDone := startTestServer(t, func() *Server {
		s := NewServer(Options{Handler: handler})
		s.Options.Pollers = 1
		s.Options.ReadBufferSize = 4 << 10
		return s
	})
	t.Cleanup(func() { stopTestServer(t, server, serveDone) })

	var client net.Conn
	var err error
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
	server := &Server{Options: Options{MaxFramePayload: 1024, MaxMessageSize: 1024, CloseTimeout: time.Second}}
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
	server := &Server{Options: Options{MaxFramePayload: 1024, MaxMessageSize: 1024, CloseTimeout: 5 * time.Millisecond}}
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
	server := NewServer(Options{})
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
		shard := state.shard
		state.mu.Unlock()
		if deadlineShardAt(shard).has(conn, deadlineKindClose) {
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
	server := &Server{Options: Options{MaxFramePayload: 1024, MaxMessageSize: 1024, CloseTimeout: time.Second}}
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
	server := &Server{Options: Options{MaxFramePayload: 1024, MaxMessageSize: 1024}}
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
	dialer := &Dialer{Options: Options{MaxHeaderBytes: DefaultMaxHeaderBytes, Subprotocols: []string{"chat"}}}
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
	handler := &echoHandler{open: make(chan struct{}), closed: make(chan struct{}), message: make(chan Message, 1)}
	server, addr, serveDone := startTestServer(t, func() *Server {
		s := NewServer(Options{Handler: handler})
		s.EnableCompression = true
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
						server := &Server{Options: Options{MaxFramePayload: uint64(len(encoded)), MaxMessageSize: limit}}
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

func TestCloseTimerCancelCoversEveryArm(t *testing.T) {
	raw := newScriptedConn()
	raw.closed = make(chan struct{})
	conn := &Conn{raw: raw, config: testServerConfig(&Server{Options: Options{CloseTimeout: 25 * time.Millisecond}})}
	// An arm from either entry point must land in the connection's one shard,
	// so a single cancel reaches every pending entry.
	conn.startCloseTimer()
	conn.ensureCloseTimer()
	conn.stopCloseTimer()
	select {
	case <-raw.closed:
		t.Fatal("a stopped close timeout still aborted the transport")
	case <-time.After(100 * time.Millisecond):
	}
	// A later arm reuses the pinned shard and is still cancellable.
	conn.startCloseTimer()
	conn.stopCloseTimer()
	select {
	case <-raw.closed:
		t.Fatal("a re-armed close timeout was not canceled")
	case <-time.After(100 * time.Millisecond):
	}
}

// A request that arrives in one read is parsed where it lies; only one split
// across reads is copied, and the copy does not alias the read buffer.
func TestHandshakeCopiesOnlyARequestSplitAcrossReads(t *testing.T) {
	request := []byte("GET /chat HTTP/1.1\r\n" +
		"Host: example.com\r\n" +
		"Connection: Upgrade\r\n" +
		"Upgrade: websocket\r\n" +
		"Sec-WebSocket-Version: 13\r\n" +
		"Sec-WebSocket-Key: " + testKey + "\r\n\r\n")
	newConn := func() (*Conn, *handshakeState) {
		handler := &recordingHandler{}
		conn := &Conn{raw: newSegmentedProbeConn(), handler: handler, config: testServerConfig(NewServer(Options{Handler: handler}))}
		state := &handshakeState{}
		conn.handshake.Store(state)
		return conn, state
	}

	whole, state := newConn()
	if err := whole.consumeHandshake(append([]byte(nil), request...)); err != nil {
		t.Fatal(err)
	}
	if !whole.opened.Load() || state.data != nil {
		t.Fatalf("opened %v, copied %d bytes of a request read whole", whole.opened.Load(), len(state.data))
	}

	split, state := newConn()
	first := append([]byte(nil), request[:40]...)
	if err := split.consumeHandshake(first); err != nil {
		t.Fatal(err)
	}
	clear(first) // the read buffer is reused once the callback returns
	if string(state.data) != string(request[:40]) {
		t.Fatalf("the partial request was not kept: %q", state.data)
	}
	if err := split.consumeHandshake(append([]byte(nil), request[40:]...)); err != nil {
		t.Fatal(err)
	}
	if !split.opened.Load() {
		t.Fatal("a request split across reads did not open the connection")
	}
}

type scriptedYieldConn struct{ *scriptedConn }

func (raw *scriptedYieldConn) YieldRead() error { return raw.Wake() }

func TestOnDataPropagatesUnrelatedErrorWhileClosing(t *testing.T) {
	for _, tc := range []struct {
		name   string
		client bool
	}{
		{name: "server"},
		{name: "client", client: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wakeErr := errors.New("yield failed")
			raw := &scriptedYieldConn{newScriptedConn()}
			raw.wakeErr = wakeErr
			conn := &Conn{raw: raw}
			if tc.client {
				conn.config = testDialerConfig(NewDialer(Options{}))
			} else {
				conn.config = testServerConfig(NewServer(Options{}))
			}
			conn.opened.Store(true)
			seen := 0
			conn.handler = handlerFuncs{onMessage: func(_ *Conn, _ Message) {
				seen++
				if seen == minFramesPerDataEvent {
					conn.closing.Store(true)
				}
			}}
			for i := 0; i < minFramesPerDataEvent+1; i++ {
				raw.inbound = frame.Append(raw.inbound, frame.Frame{
					Fin: true, Opcode: frame.Binary, Masked: !tc.client, Payload: []byte{byte(i)},
				}, [4]byte{1, 2, 3, 4})
			}
			raw.userdata = conn
			var err error
			if tc.client {
				err = (&Dialer{}).onData(raw)
			} else {
				err = (&Server{}).onData(raw)
			}
			if !errors.Is(err, wakeErr) {
				t.Fatalf("OnData error = %v, want %v", err, wakeErr)
			}
			if seen != minFramesPerDataEvent {
				t.Fatalf("messages = %d, want %d", seen, minFramesPerDataEvent)
			}
		})
	}
}

func TestOnDataPropagatesFailedProtocolClose(t *testing.T) {
	for _, tc := range []struct {
		name   string
		client bool
	}{
		{name: "server"},
		{name: "client", client: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			writeErr := errors.New("close frame write failed")
			raw := newScriptedConn()
			raw.writeErr = writeErr
			raw.inbound = frame.Append(nil, frame.Frame{
				Fin: true, Opcode: frame.Binary, Masked: tc.client,
			}, [4]byte{1, 2, 3, 4})
			conn := testServerConn(raw)
			if tc.client {
				conn.config = testDialerConfig(NewDialer(Options{}))
			}
			raw.userdata = conn
			var err error
			if tc.client {
				err = (&Dialer{}).onData(raw)
			} else {
				err = (&Server{}).onData(raw)
			}
			if !errors.Is(err, frame.ErrProtocol) || !errors.Is(err, writeErr) {
				t.Fatalf("OnData error = %v, want protocol and write errors", err)
			}
			if raw.closes != 1 {
				t.Fatalf("transport closes = %d, want 1", raw.closes)
			}
		})
	}
}

func TestOnDataLeavesOwnedProtocolCloseToTransport(t *testing.T) {
	for _, tc := range []struct {
		name   string
		client bool
	}{
		{name: "server"},
		{name: "client", client: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := newScriptedConn()
			raw.inbound = frame.Append(nil, frame.Frame{
				Fin: true, Opcode: frame.Binary, Masked: tc.client,
			}, [4]byte{1, 2, 3, 4})
			conn := testServerConn(raw)
			if tc.client {
				conn.config = testDialerConfig(NewDialer(Options{}))
			}
			raw.userdata = conn
			var err error
			if tc.client {
				err = (&Dialer{}).onData(raw)
			} else {
				err = (&Server{}).onData(raw)
			}
			if err != nil {
				t.Fatalf("OnData error = %v, want graceful close ownership", err)
			}
			if !conn.closing.Load() || len(raw.written) != 1 || raw.closes != 0 {
				t.Fatalf("close state = closing:%v writes:%d closes:%d", conn.closing.Load(), len(raw.written), raw.closes)
			}
			if info := conn.closeInfo(); !errors.Is(info.Err, frame.ErrProtocol) {
				t.Fatalf("recorded protocol error = %v, want %v", info.Err, frame.ErrProtocol)
			}
			completeTestOutbound(conn)
			if raw.closes != 1 {
				t.Fatalf("transport closes after outbound drain = %d, want 1", raw.closes)
			}
		})
	}
}

func TestUnmaskShortMatchesBytewiseUnmask(t *testing.T) {
	random := rand.New(rand.NewPCG(1, 2))
	for size := 0; size < 126; size++ {
		for _, ascii := range []bool{true, false} {
			key := [4]byte{byte(random.Uint32()), byte(random.Uint32()), byte(random.Uint32()), byte(random.Uint32())}
			plain := make([]byte, size)
			for i := range plain {
				plain[i] = byte(random.Uint32()) & 0x7f
			}
			if !ascii {
				if size == 0 {
					continue
				}
				plain[random.IntN(size)] |= 0x80
			}
			masked := make([]byte, size)
			for i := range plain {
				masked[i] = plain[i] ^ key[i&3]
			}
			if got := unmaskShort(masked, binary.LittleEndian.Uint32(key[:])); got != ascii {
				t.Fatalf("size %d: ascii = %v, want %v", size, got, ascii)
			}
			if !bytes.Equal(masked, plain) {
				t.Fatalf("size %d: unmasked %x, want %x", size, masked, plain)
			}
		}
	}
}

// textPaths are the ways a text message reaches validation: one frame in one
// read (the short path up to 16-bit lengths, the parser past them), one frame
// over reads of seven bytes (the incremental parser), three fragments (the
// assembler), and permessage-deflate (validated after inflating).
var textPaths = []string{"frame", "split", "fragments", "compressed"}

// deliverText sends payload as one text message along path and reports what
// the handler got and whether the connection refused the message.
func deliverText(t testing.TB, payload []byte, path string) ([]Message, bool) {
	t.Helper()
	var got []Message
	server := NewServer(Options{Handler: messageLog{&got}})
	server.config = testServerConfig(server)
	raw := &echoPathConn{}
	conn := server.newConnection(raw)
	conn.opened.Store(true)
	raw.userdata = conn
	defer server.onClose(raw, io.EOF)
	raw.out = make([]byte, 0, 64) // room for the Close frame a refusal sends

	key := [4]byte{0x9e, 0x01, 0xc3, 0x7f}
	text := func(f frame.Frame) []byte {
		f.Masked = true
		return frame.Append(nil, f, key)
	}
	read := func(wire []byte, size int) bool {
		for len(wire) > 0 && !conn.closing.Load() {
			n := min(size, len(wire))
			raw.inbound = wire[:n]
			if err := server.onData(raw); err != nil {
				return true
			}
			wire = wire[n:]
		}
		return conn.closing.Load()
	}
	switch path {
	case "frame":
		return got, read(text(frame.Frame{Fin: true, Opcode: frame.Text, Payload: payload}), len(payload)+14)
	case "split":
		return got, read(text(frame.Frame{Fin: true, Opcode: frame.Text, Payload: payload}), 7)
	case "fragments":
		a, b := len(payload)/3, 2*len(payload)/3
		wire := text(frame.Frame{Opcode: frame.Text, Payload: payload[:a]})
		wire = append(wire, text(frame.Frame{Opcode: frame.Continuation, Payload: payload[a:b]})...)
		wire = append(wire, text(frame.Frame{Fin: true, Opcode: frame.Continuation, Payload: payload[b:]})...)
		return got, read(wire, len(wire))
	case "compressed":
		encoded, err := compress.Compress(payload, -1)
		if err != nil {
			t.Fatal(err)
		}
		conn.compression = &compressionState{decoder: compress.NewDecoder(true)}
		err = conn.acceptFrame(frame.Frame{Fin: true, RSV1: true, Opcode: frame.Text, Payload: encoded})
		return got, err != nil
	}
	t.Fatalf("unknown path %q", path)
	return nil, false
}

// checkTextDelivery holds every path to utf8.Valid: valid text is delivered
// once, byte for byte, as text; anything else is refused before the handler
// sees it.
func checkTextDelivery(t testing.TB, payload []byte, what string) {
	t.Helper()
	valid := utf8.Valid(payload)
	for _, path := range textPaths {
		got, refused := deliverText(t, append([]byte(nil), payload...), path)
		if valid && (refused || len(got) != 1 || got[0].Type != TextMessage || !bytes.Equal(got[0].Payload, payload)) {
			t.Fatalf("%s, %s path: valid text of %d bytes refused %v, %d messages", what, path, len(payload), refused, len(got))
		}
		if !valid && (!refused || len(got) != 0) {
			t.Fatalf("%s, %s path: invalid text of %d bytes refused %v, %d messages\n% x", what, path, len(payload), refused, len(got), payload)
		}
	}
}

// Multibyte text, valid and broken, is judged the same on every path and at
// every length around the validator's 32-byte blocks and the unmask's vector
// threshold: cut short, with a byte replaced or a bad sequence set at the
// block edges, the start and the end.
func TestTextValidationAgreesOnEveryPath(t *testing.T) {
	sizes := []int{1, 2, 3, 4, 31, 32, 33, 34, 63, 64, 65, 95, 96, 97, 125, 126, 127, 128, 129, 1000, 1024, 4097, 65535, 70000}
	bad := [][]byte{{0xff}, {0x80}, {0xc0, 0xaf}, {0xe0, 0x80, 0xaf}, {0xed, 0xa0, 0x80}, {0xf4, 0x90, 0x80, 0x80}, {0xe4, 0xb8}, {0xf0, 0x9f, 0x98}}
	for _, size := range sizes {
		for _, multibyte := range []bool{true, false} {
			text := textOfLength(size, multibyte)
			checkTextDelivery(t, text, fmt.Sprintf("text of %d bytes, multibyte %v", size, multibyte))
			if multibyte {
				// Cut short, possibly mid-sequence.
				cut := bytes.Repeat([]byte("中文"), size/6+1)[:size]
				checkTextDelivery(t, cut, fmt.Sprintf("Chinese cut at %d bytes", size))
			}
			for _, seq := range bad {
				for _, at := range []int{0, 29, 30, 31, 32, 33, 63, 64, size / 2, size - len(seq)} {
					if at < 0 || at+len(seq) > size {
						continue
					}
					p := append([]byte(nil), text...)
					copy(p[at:], seq)
					checkTextDelivery(t, p, fmt.Sprintf("%d bytes, multibyte %v, % x at %d", size, multibyte, seq, at))
				}
			}
		}
	}
}

func FuzzTextValidation(f *testing.F) {
	f.Add([]byte("中文文本"), uint8(0))
	f.Add([]byte("😀é€\xff"), uint8(40))
	f.Add([]byte("\xed\xa0\x80"), uint8(200))
	f.Add(bytes.Repeat([]byte("文本帧"), 50), uint8(130))
	f.Fuzz(func(t *testing.T, p []byte, lead uint8) {
		// A multibyte lead takes short input past the validator's block
		// size and the unmask's vector threshold.
		prefix := bytes.Repeat([]byte("中"), int(lead%64))
		if lead >= 128 {
			prefix = bytes.Repeat([]byte("a"), int(lead%128))
		}
		checkTextDelivery(t, append(prefix, p...), "fuzzed")
	})
}

// peerCloseBehindHandler closes on its first message, so a peer that answers
// with more frames plus its Close in one segment exercises what a read round
// does after this end started closing.
type peerCloseBehindHandler struct {
	closed chan CloseEvent
}

func (*peerCloseBehindHandler) OnOpen(*Conn) {}
func (h *peerCloseBehindHandler) OnMessage(c *Conn, m Message) {
	if string(m.Payload) == "first" {
		_ = c.Close(1000, "bye")
	}
}
func (h *peerCloseBehindHandler) OnClose(_ *Conn, info CloseEvent) { h.closed <- info }

// peerCloseBehindRun sends first + middle + the peer's Close in one segment
// and checks the server ends the connection once its close frame drains,
// rather than sitting on the buffered frames until the close timer.
func peerCloseBehindRun(t *testing.T, middle frame.Frame, closeTimeout time.Duration) {
	t.Helper()
	h := &peerCloseBehindHandler{closed: make(chan CloseEvent, 1)}
	addr := startRobustServer(t, Options{Pollers: 1, CloseTimeout: closeTimeout}, h)
	conn, reader := robustDial(t, addr)
	_ = conn.SetDeadline(time.Now().Add(4 * testIOTimeout()))
	closePayload := make([]byte, 2)
	binary.BigEndian.PutUint16(closePayload, 1000)
	maskKey := [4]byte{1, 2, 3, 4}
	wire := frame.Append(nil, frame.Frame{Fin: true, Opcode: frame.Text, Masked: true, Payload: []byte("first")}, maskKey)
	wire = frame.Append(wire, middle, maskKey)
	wire = frame.Append(wire, frame.Frame{Fin: true, Opcode: frame.Close, Masked: true, Payload: closePayload}, maskKey)
	start := time.Now()
	if _, err := conn.Write(wire); err != nil {
		t.Fatal(err)
	}
	if code := awaitClose(t, reader); code != 1000 {
		t.Fatalf("server close code %d", code)
	}
	_, err := reader.ReadByte()
	elapsed := time.Since(start)
	var info CloseEvent
	select {
	case info = <-h.closed:
	case <-time.After(testIOTimeout()):
		t.Fatalf("OnClose never ran: TCP err %v after %v", err, elapsed)
	}
	if err != io.EOF || elapsed > time.Second {
		t.Fatalf("peer Close not processed: TCP closed after %v (err %v), OnClose err %v", elapsed, err, info.Err)
	}
	if info.Err != nil && !errors.Is(info.Err, io.EOF) {
		t.Fatalf("OnClose err = %v, want a clean close", info.Err)
	}
}

// A peer's Close in the same segment as data sent after this end closed is
// still answered, and the connection ends once the close frame drains.
func TestPeerCloseBehindDataCompletesHandshake(t *testing.T) {
	peerCloseBehindRun(t, frame.Frame{Fin: true, Opcode: frame.Text, Masked: true, Payload: []byte("second")}, 2*time.Second)
}

// The same with a Ping in between: it gets no Pong after the close began, but
// the Close behind it still completes the handshake.
func TestPeerCloseBehindPingCompletesHandshake(t *testing.T) {
	peerCloseBehindRun(t, frame.Frame{Fin: true, Opcode: frame.Ping, Masked: true, Payload: []byte("p")}, 2*time.Second)
}

// Without a usable close timer the handshake must still complete: the peer's
// buffered Close, not the timer, ends the connection.
func TestPeerCloseBehindDataCompletesWithoutTimer(t *testing.T) {
	peerCloseBehindRun(t, frame.Frame{Fin: true, Opcode: frame.Text, Masked: true, Payload: []byte("second")}, 24*time.Hour)
}

// closeInOpenHandler closes from OnOpen, so a frame that arrived with the
// upgrade request is delivered after this end already started closing.
type closeInOpenHandler struct{ closed chan CloseEvent }

func (*closeInOpenHandler) OnOpen(c *Conn)                     { _ = c.Close(4000, "no") }
func (*closeInOpenHandler) OnMessage(*Conn, Message)           {}
func (h *closeInOpenHandler) OnClose(_ *Conn, info CloseEvent) { h.closed <- info }

// A frame in the upgrade segment behind a close started in OnOpen is a stop,
// not a protocol error: the transport stays up until the peer answers the
// close handshake instead of dropping the connection at once.
func TestEarlyFrameAfterCloseInOnOpenWaitsForPeerClose(t *testing.T) {
	h := &closeInOpenHandler{closed: make(chan CloseEvent, 1)}
	addr := startRobustServer(t, Options{Pollers: 1, CloseTimeout: 3 * time.Second}, h)
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	request := "GET / HTTP/1.1\r\nHost: " + addr + "\r\n" +
		"Connection: Upgrade\r\nUpgrade: websocket\r\n" +
		"Sec-WebSocket-Version: 13\r\nSec-WebSocket-Key: " + testKey + "\r\n\r\n"
	wire := append([]byte(request), frame.Append(nil, frame.Frame{Fin: true, Opcode: frame.Text, Masked: true, Payload: []byte("early")}, [4]byte{1, 2, 3, 4})...)
	if _, err := conn.Write(wire); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(conn)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if line == "\r\n" {
			break
		}
	}
	_ = conn.SetDeadline(time.Now().Add(2 * testIOTimeout()))
	if code := awaitClose(t, reader); code != 4000 {
		t.Fatalf("close code %d", code)
	}
	_ = conn.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	_, readErr := reader.ReadByte()
	var info CloseEvent
	select {
	case info = <-h.closed:
	case <-time.After(testIOTimeout()):
		t.Fatal("OnClose never ran")
	}
	if readErr == io.EOF {
		t.Fatalf("server closed TCP without waiting for the peer's Close reply (OnClose err %v)", info.Err)
	}
}
