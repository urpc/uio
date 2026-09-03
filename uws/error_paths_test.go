package uws

import (
	"encoding/binary"
	"errors"
	"io"
	"math"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/urpc/uio"
	"github.com/urpc/uio/uws/internal/compress"
	"github.com/urpc/uio/uws/internal/frame"
	"github.com/urpc/uio/uws/internal/handshake"
)

type testAddr string

func (a testAddr) Network() string { return "test" }
func (a testAddr) String() string  { return string(a) }

type scriptedConn struct {
	writeProbeConn
	local     net.Addr
	remote    net.Addr
	written   [][]byte
	writeErr  error
	flushErr  error
	flushHook func()
	short     bool
	userdata  any
	inbound   []byte
}

func newScriptedConn() *scriptedConn {
	return &scriptedConn{local: testAddr("local"), remote: testAddr("remote")}
}

func (c *scriptedConn) LocalAddr() net.Addr   { return c.local }
func (c *scriptedConn) RemoteAddr() net.Addr  { return c.remote }
func (c *scriptedConn) Userdata() any         { return c.userdata }
func (c *scriptedConn) SetUserdata(value any) { c.userdata = value }

func (c *scriptedConn) InboundBuffered() int { return len(c.inbound) }

func (c *scriptedConn) Peek(dst []byte) []byte {
	n := copy(dst, c.inbound)
	return dst[:n]
}

func (c *scriptedConn) PeekChunk() []byte { return c.inbound }

func (c *scriptedConn) Discard(n int) (int, error) {
	if n < 0 || n > len(c.inbound) {
		n = len(c.inbound)
	}
	c.inbound = c.inbound[n:]
	return n, nil
}

func (c *scriptedConn) Write(payload []byte) (int, error) {
	c.writes++
	c.written = append(c.written, append([]byte(nil), payload...))
	if c.writeErr != nil {
		return 0, c.writeErr
	}
	if c.short && len(payload) > 0 {
		return len(payload) - 1, nil
	}
	return len(payload), nil
}

func (c *scriptedConn) Writev(buffers [][]byte) (int, error) {
	c.writes++
	c.writevs++
	total := 0
	for _, buffer := range buffers {
		total += len(buffer)
	}
	payload := make([]byte, 0, total)
	for _, buffer := range buffers {
		payload = append(payload, buffer...)
	}
	c.written = append(c.written, payload)
	if c.writeErr != nil {
		return 0, c.writeErr
	}
	if c.short && total > 0 {
		return total - 1, nil
	}
	return total, nil
}

func (c *scriptedConn) WriteOwned(buffer *uio.Buffer) (int, error) {
	payload := append([]byte(nil), buffer.Bytes()...)
	uio.ReleaseBuffer(buffer)
	c.writes++
	c.written = append(c.written, payload)
	if c.writeErr != nil {
		return 0, c.writeErr
	}
	if c.short && len(payload) > 0 {
		return len(payload) - 1, nil
	}
	return len(payload), nil
}

func (c *scriptedConn) Flush() error {
	c.flushes++
	hook := c.flushHook
	c.flushHook = nil
	if hook != nil {
		hook()
	}
	return c.flushErr
}

func (c *scriptedConn) Close() error { return c.CloseWith(io.EOF) }

func testServerConn(raw *scriptedConn) *Conn {
	conn := &Conn{
		raw: raw,
		config: testServerConfig(&Server{
			MaxFramePayload:  1024,
			MaxMessageSize:   1024,
			MaxOutboundBytes: 1 << 20,
		}),
	}
	conn.opened.Store(true)
	return conn
}

func TestProtocolCloseMapsErrorsToCloseCodes(t *testing.T) {
	tests := []struct {
		name string
		err  error
		code uint16
	}{
		{name: "protocol", err: frame.ErrProtocol, code: 1002},
		{name: "invalid UTF-8", err: frame.ErrInvalidUTF8, code: 1007},
		{name: "frame too large", err: frame.ErrMessageTooBig, code: 1009},
		{name: "inflate too large", err: compress.ErrTooLarge, code: 1009},
		{name: "application backpressure", err: ErrApplicationBackpressure, code: 1013},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			raw := newScriptedConn()
			conn := testServerConn(raw)
			if err := conn.protocolClose(test.err); !errors.Is(err, test.err) {
				t.Fatalf("protocolClose() = %v, want %v", err, test.err)
			}
			if !conn.closing.Load() {
				t.Fatal("protocol close did not mark the connection closing")
			}
			if len(raw.written) != 1 {
				t.Fatalf("close writes = %d, want 1", len(raw.written))
			}
			wire := raw.written[0]
			if len(wire) < 4 || wire[0] != 0x88 || binary.BigEndian.Uint16(wire[len(wire)-2:]) != test.code {
				t.Fatalf("close frame = %x, want code %d", wire, test.code)
			}
			if info := conn.closeInfo(); !errors.Is(info.Err, test.err) {
				t.Fatalf("close info error = %v, want %v", info.Err, test.err)
			}
			completeTestOutbound(conn)
		})
	}
}

func TestIncomingInvalidUTF8SendsClose1007(t *testing.T) {
	raw := newScriptedConn()
	raw.inbound = frame.Append(nil, frame.Frame{
		Fin: true, Opcode: frame.Text, Masked: true, Payload: []byte{0xff},
	}, [4]byte{1, 2, 3, 4})
	handler := &recordingHandler{}
	conn := testServerConn(raw)
	conn.handler = handler
	raw.userdata = conn

	if err := (&Server{}).onData(raw); err != nil {
		t.Fatalf("server OnData error = %v", err)
	}
	if len(raw.written) != 1 {
		t.Fatalf("close writes = %d, want 1", len(raw.written))
	}
	wire := raw.written[0]
	if len(wire) < 4 || wire[0] != 0x88 || binary.BigEndian.Uint16(wire[len(wire)-2:]) != 1007 {
		t.Fatalf("close frame = %x, want code 1007", wire)
	}
	if !conn.closing.Load() {
		t.Fatal("invalid UTF-8 did not start protocol close")
	}
	if raw.closes != 0 {
		t.Fatal("transport closed before the 1007 frame was written")
	}
	if info := conn.closeInfo(); !errors.Is(info.Err, frame.ErrInvalidUTF8) {
		t.Fatalf("close error = %v, want %v", info.Err, frame.ErrInvalidUTF8)
	}
	completeTestOutbound(conn)
	if raw.closes != 1 {
		t.Fatalf("transport closes after 1007 = %d, want 1", raw.closes)
	}
	handler.mu.Lock()
	defer handler.mu.Unlock()
	if len(handler.messages) != 0 {
		t.Fatalf("invalid UTF-8 reached OnMessage: %q", handler.messages)
	}
}

func TestOversizedIncomingMessagesCloseWith1009BeforeCallback(t *testing.T) {
	const (
		maxMessage  = 1024
		payloadSize = maxMessage + 1
	)
	payload := make([]byte, payloadSize)
	for _, test := range []struct {
		name   string
		frames []frame.Frame
	}{
		{
			name: "complete",
			frames: []frame.Frame{{
				Fin: true, Opcode: frame.Binary, Masked: true, Payload: payload,
			}},
		},
		{
			name: "fragmented",
			frames: []frame.Frame{
				{Opcode: frame.Binary, Masked: true, Payload: payload[:maxMessage/2]},
				{Fin: true, Opcode: frame.Continuation, Masked: true, Payload: payload[maxMessage/2:]},
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			raw := newScriptedConn()
			var wire []byte
			for index, f := range test.frames {
				wire = frame.Append(wire, f, [4]byte{byte(index + 1), 2, 3, 4})
			}
			raw.inbound = wire
			handler := &recordingHandler{}
			conn := &Conn{
				raw:     raw,
				config:  testServerConfig(&Server{MaxFramePayload: 16 << 10, MaxMessageSize: maxMessage, MaxOutboundBytes: 1 << 20}),
				handler: handler,
			}
			conn.opened.Store(true)

			if err := conn.readAvailable(); !errors.Is(err, frame.ErrMessageTooBig) {
				t.Fatalf("readAvailable() error = %v, want %v", err, frame.ErrMessageTooBig)
			}
			handler.mu.Lock()
			delivered := len(handler.messages)
			handler.mu.Unlock()
			if delivered != 0 {
				t.Fatalf("delivered messages = %d, want 0", delivered)
			}
			if len(raw.written) != 1 || len(raw.written[0]) != 4 || raw.written[0][0] != 0x88 || frame.CloseCode(raw.written[0][2:]) != 1009 {
				t.Fatalf("close frame = %x, want close code 1009", raw.written)
			}
			completeTestOutbound(conn)
		})
	}
}

func TestOversizedCompressedFragmentsCloseWith1009BeforeFIN(t *testing.T) {
	const maxCompressed = 4
	frames := []frame.Frame{
		{RSV1: true, Opcode: frame.Binary, Masked: true, Payload: []byte("ab")},
		{Opcode: frame.Continuation, Masked: true, Payload: []byte("cd")},
		{Opcode: frame.Continuation, Masked: true, Payload: []byte("e")},
	}
	var wire []byte
	for index, f := range frames {
		wire = frame.Append(wire, f, [4]byte{byte(index + 1), 2, 3, 4})
	}
	raw := newScriptedConn()
	raw.inbound = wire
	handler := &recordingHandler{}
	conn := &Conn{
		raw:     raw,
		config:  testServerConfig(&Server{MaxFramePayload: maxCompressed, MaxMessageSize: 64, MaxOutboundBytes: 1 << 20, EnableCompression: true}),
		handler: handler,
	}
	conn.opened.Store(true)

	if err := conn.readAvailable(); !errors.Is(err, frame.ErrMessageTooBig) {
		t.Fatalf("readAvailable() error = %v, want %v", err, frame.ErrMessageTooBig)
	}
	handler.mu.Lock()
	delivered := len(handler.messages)
	handler.mu.Unlock()
	if delivered != 0 {
		t.Fatalf("delivered messages = %d, want 0", delivered)
	}
	if len(raw.written) != 1 || len(raw.written[0]) != 4 || raw.written[0][0] != 0x88 || frame.CloseCode(raw.written[0][2:]) != 1009 {
		t.Fatalf("close frame = %x, want close code 1009", raw.written)
	}
	completeTestOutbound(conn)
}

func TestRejectHandshakeServerAndClientPaths(t *testing.T) {
	serverRaw := newScriptedConn()
	serverConn := &Conn{raw: serverRaw}
	serverConn.rejectHandshake()
	if len(serverRaw.written) != 1 || !strings.HasPrefix(string(serverRaw.written[0]), "HTTP/1.1 400 Bad Request") {
		t.Fatalf("rejection response = %q", serverRaw.written)
	}

	clientRaw := newScriptedConn()
	clientConn := &Conn{raw: clientRaw, config: testDialerConfig(new(Dialer))}
	clientConn.rejectHandshake()
	if clientRaw.closes != 1 {
		t.Fatalf("client transport closes = %d, want 1", clientRaw.closes)
	}

	openedRaw := newScriptedConn()
	openedConn := &Conn{raw: openedRaw}
	openedConn.opened.Store(true)
	openedConn.rejectHandshake()
	if openedRaw.writes != 0 || openedRaw.closes != 0 {
		t.Fatal("opened connection was rejected")
	}
}

func TestConnectionAccessorsAndUserdata(t *testing.T) {
	raw := newScriptedConn()
	conn := testServerConn(raw)
	conn.setSubprotocol("rpc")
	conn.SetUserdata("userdata")

	if conn.LocalAddr().String() != "local" || conn.RemoteAddr().String() != "remote" {
		t.Fatalf("addresses = %v -> %v", conn.LocalAddr(), conn.RemoteAddr())
	}
	if conn.IsClosed() {
		t.Fatal("new connection is closed")
	}
	if conn.Userdata() != "userdata" || conn.Subprotocol() != "rpc" {
		t.Fatalf("userdata/subprotocol = %v/%q", conn.Userdata(), conn.Subprotocol())
	}
	conn.SetUserdata(nil)
	if conn.Userdata() != nil {
		t.Fatalf("cleared userdata = %v, want nil", conn.Userdata())
	}
	conn.closed.Store(true)
	if !conn.IsClosed() {
		t.Fatal("closed connection reported open")
	}
}

func TestControlAndMessageValidationErrors(t *testing.T) {
	raw := newScriptedConn()
	conn := testServerConn(raw)
	conn.heartbeat = &heartbeatState{}
	conn.heartbeat.pingOutstanding.Store(true)
	beforePong := time.Now().Add(-time.Second).UnixNano()
	conn.heartbeat.lastPong.Store(beforePong)
	if err := conn.acceptControl(frame.Frame{Fin: true, Opcode: frame.Ping, Payload: []byte("ping")}); err != nil {
		t.Fatal(err)
	}
	if len(raw.written) != 1 || raw.written[0][0]&0x0f != byte(frame.Pong) {
		t.Fatalf("pong frame = %x", raw.written)
	}
	if err := conn.acceptControl(frame.Frame{Fin: true, Opcode: frame.Pong}); err != nil {
		t.Fatal(err)
	}
	if conn.heartbeat.pingOutstanding.Load() || conn.heartbeat.lastPong.Load() <= beforePong {
		t.Fatal("pong did not acknowledge the heartbeat")
	}
	if err := conn.acceptControl(frame.Frame{Fin: true, Opcode: frame.Text}); !errors.Is(err, frame.ErrProtocol) {
		t.Fatalf("invalid control error = %v", err)
	}

	if err := conn.acceptFrame(frame.Frame{Fin: true, Opcode: frame.Text, Payload: []byte{0xff}}); !errors.Is(err, frame.ErrInvalidUTF8) {
		t.Fatalf("invalid text error = %v", err)
	}
	if err := conn.acceptFrame(frame.Frame{Fin: true, RSV1: true, Opcode: frame.Binary, Payload: []byte("compressed")}); !errors.Is(err, frame.ErrProtocol) {
		t.Fatalf("unexpected compressed frame error = %v", err)
	}
}

func TestServerDisableUTF8CheckAllowsIncomingText(t *testing.T) {
	handler := &recordingHandler{}
	server := NewServer(handler)
	server.DisableUTF8Check = true
	raw := newScriptedConn()
	server.onOpen(raw)
	conn, ok := raw.userdata.(*Conn)
	if !ok || conn == nil {
		t.Fatal("server did not attach connection")
	}
	conn.stopHandshakeTimer()
	conn.opened.Store(true)

	if err := conn.acceptFrame(frame.Frame{Fin: true, Opcode: frame.Text, Payload: []byte{0xff}}); err != nil {
		t.Fatalf("incoming text with validation disabled = %v", err)
	}
	compressed, err := compress.Compress([]byte{0xff}, -1)
	if err != nil {
		t.Fatal(err)
	}
	conn.compression = &compressionState{decoder: compress.NewDecoder(true)}
	if err = conn.acceptFrame(frame.Frame{Fin: true, RSV1: true, Opcode: frame.Text, Payload: compressed}); err != nil {
		t.Fatalf("incoming compressed text with validation disabled = %v", err)
	}
	handler.mu.Lock()
	defer handler.mu.Unlock()
	if len(handler.messages) != 2 || handler.messages[0] != string([]byte{0xff}) || handler.messages[1] != string([]byte{0xff}) {
		t.Fatalf("messages = %q, want two invalid UTF-8 payloads", handler.messages)
	}
}

func TestSendAndTransportErrorPaths(t *testing.T) {
	if err := (&Conn{}).send(MessageType(99), nil); !errors.Is(err, frame.ErrProtocol) {
		t.Fatalf("invalid message type error = %v", err)
	}
	if err := testServerConn(newScriptedConn()).Ping(make([]byte, 126)); !errors.Is(err, frame.ErrProtocol) {
		t.Fatalf("oversized ping error = %v", err)
	}

	notReady := &Conn{}
	if err := notReady.sendFrame(frame.Frame{Fin: true, Opcode: frame.Binary}); !errors.Is(err, ErrNotReady) {
		t.Fatalf("not-ready send error = %v", err)
	}

	tooLarge := testServerConn(newScriptedConn())
	tooLarge.config = testServerConfig(&Server{MaxFramePayload: 1})
	if err := tooLarge.sendFrameLocked(frame.Frame{Fin: true, Opcode: frame.Binary, Payload: []byte("xx")}); !errors.Is(err, frame.ErrMessageTooBig) {
		t.Fatalf("oversized frame error = %v", err)
	}

	writeFailure := errors.New("write failure")
	failedRaw := newScriptedConn()
	failedRaw.writeErr = writeFailure
	if err := testServerConn(failedRaw).SendBinary([]byte("data")); !errors.Is(err, writeFailure) {
		t.Fatalf("write failure = %v", err)
	}
	closedRaw := newScriptedConn()
	closedRaw.writeErr = net.ErrClosed
	if err := testServerConn(closedRaw).SendBinary([]byte("data")); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed transport write = %v", err)
	}

	shortRaw := newScriptedConn()
	shortRaw.short = true
	if err := testServerConn(shortRaw).SendBinary([]byte("data")); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("short write error = %v", err)
	}

	flushFailure := errors.New("flush failure")
	flushRaw := newScriptedConn()
	flushRaw.flushErr = flushFailure
	if err := testServerConn(flushRaw).Ping(nil); !errors.Is(err, flushFailure) {
		t.Fatalf("flush failure = %v", err)
	}
}

func TestCloseAndWriterStateErrors(t *testing.T) {
	conn := testServerConn(newScriptedConn())
	if err := conn.Close(1005, ""); !errors.Is(err, frame.ErrProtocol) {
		t.Fatalf("reserved close code error = %v", err)
	}
	if err := conn.Close(1000, string([]byte{0xff})); !errors.Is(err, frame.ErrInvalidUTF8) {
		t.Fatalf("invalid close reason error = %v", err)
	}
	if err := conn.Close(1000, strings.Repeat("x", 124)); !errors.Is(err, frame.ErrInvalidUTF8) {
		t.Fatalf("long close reason error = %v", err)
	}

	if _, err := conn.BeginMessage(MessageType(99)); !errors.Is(err, frame.ErrProtocol) {
		t.Fatalf("invalid writer type error = %v", err)
	}
	closedConn := testServerConn(newScriptedConn())
	closedConn.closing.Store(true)
	if _, err := closedConn.BeginMessage(BinaryMessage); !errors.Is(err, ErrClosed) {
		t.Fatalf("closing writer error = %v", err)
	}
	if _, err := (&Conn{}).BeginMessage(BinaryMessage); !errors.Is(err, ErrNotReady) {
		t.Fatalf("not-ready writer error = %v", err)
	}

	var nilWriter *Writer
	if _, err := nilWriter.Write(nil); !errors.Is(err, ErrWriterClosed) {
		t.Fatalf("nil Writer.Write error = %v", err)
	}
	if err := nilWriter.Close(); !errors.Is(err, ErrWriterClosed) {
		t.Fatalf("nil Writer.Close error = %v", err)
	}

	limitedRaw := newScriptedConn()
	limitedConn := testServerConn(limitedRaw)
	limitedConn.config = testServerConfig(&Server{MaxMessageSize: 1})
	writer, err := limitedConn.BeginMessage(BinaryMessage)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := writer.Write([]byte("xx")); n != 0 || !errors.Is(err, frame.ErrMessageTooBig) {
		t.Fatalf("oversized Writer.Write = %d, %v", n, err)
	}
	if n, err := writer.Write(nil); n != 0 || !errors.Is(err, frame.ErrMessageTooBig) {
		t.Fatalf("failed Writer.Write = %d, %v", n, err)
	}
	if err := writer.Close(); !errors.Is(err, frame.ErrMessageTooBig) {
		t.Fatalf("failed Writer.Close = %v", err)
	}
	if limitedRaw.closes != 1 || !limitedConn.closing.Load() {
		t.Fatalf("oversized writer closes/closing = %d/%v", limitedRaw.closes, limitedConn.closing.Load())
	}

	writer = &Writer{conn: limitedConn, closed: true}
	if _, err := writer.Write(nil); !errors.Is(err, ErrWriterClosed) {
		t.Fatalf("closed Writer.Write error = %v", err)
	}
}

func TestReadAvailableHandshakeAndParserErrors(t *testing.T) {
	badHandshake := &bufferedProbeConn{inbound: []byte("not HTTP\r\n\r\n")}
	serverConn := &Conn{
		raw:    badHandshake,
		config: testServerConfig(NewServer(nil)),
	}
	if err := serverConn.readAvailable(); err == nil {
		t.Fatal("malformed handshake was accepted")
	}
	if badHandshake.writes != 1 {
		t.Fatalf("handshake rejection writes = %d, want 1", badHandshake.writes)
	}

	incomplete := &bufferedProbeConn{inbound: []byte{0x82}}
	incompleteConn := testServerConn(newScriptedConn())
	incompleteConn.raw = incomplete
	if err := incompleteConn.readAvailable(); err != nil {
		t.Fatalf("incomplete frame error = %v", err)
	}
	if incomplete.InboundBuffered() != 0 {
		t.Fatal("incremental parser did not consume partial header")
	}

	unmasked := frame.Append(nil, frame.Frame{Fin: true, Opcode: frame.Binary, Payload: []byte("bad")}, [4]byte{})
	protocolRaw := &bufferedProbeConn{inbound: unmasked}
	protocolConn := testServerConn(newScriptedConn())
	protocolConn.raw = protocolRaw
	if err := protocolConn.readAvailable(); !errors.Is(err, frame.ErrProtocol) {
		t.Fatalf("parser protocol error = %v", err)
	}
	if protocolRaw.writes != 1 {
		t.Fatalf("protocol close writes = %d, want 1", protocolRaw.writes)
	}
	completeTestOutbound(protocolConn)
}

func TestHandshakeAndCloseLifecycleErrors(t *testing.T) {
	expired := &Conn{}
	expired.handshake.Store(&handshakeState{expired: true})
	if expired.markOpened() {
		t.Fatal("expired handshake was opened")
	}

	client := &Conn{config: testDialerConfig(&Dialer{MaxHeaderBytes: 1})}
	client.handshake.Store(&handshakeState{data: []byte("xx")})
	if err := client.consumeClientHandshake(); !errors.Is(err, handshake.ErrBadRequest) {
		t.Fatalf("oversized client handshake error = %v", err)
	}

	closedRaw := newScriptedConn()
	closedRaw.flushErr = net.ErrClosed
	closedConn := testServerConn(closedRaw)
	if err := closedConn.closeTransport(); err != nil {
		t.Fatalf("closeTransport(net.ErrClosed) = %v", err)
	}
	if closedRaw.closes != 1 {
		t.Fatalf("closeTransport closes = %d, want 1", closedRaw.closes)
	}

	flushErr := errors.New("flush failed")
	failedRaw := newScriptedConn()
	failedRaw.flushErr = flushErr
	if err := testServerConn(failedRaw).closeTransport(); !errors.Is(err, flushErr) {
		t.Fatalf("closeTransport flush error = %v", err)
	}
}

func TestSendCloseAndLimitStateBranches(t *testing.T) {
	notReadyRaw := newScriptedConn()
	notReady := &Conn{raw: notReadyRaw, config: testServerConfig(&Server{MaxMessageSize: 1024})}
	if err := notReady.SendBinary(nil); !errors.Is(err, ErrNotReady) {
		t.Fatalf("not-ready SendBinary error = %v", err)
	}
	if err := notReady.Close(1000, ""); err != nil || notReadyRaw.closes != 1 {
		t.Fatalf("not-ready Close = %v, closes %d", err, notReadyRaw.closes)
	}

	closed := testServerConn(newScriptedConn())
	closed.closed.Store(true)
	if err := closed.SendBinary(nil); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed SendBinary error = %v", err)
	}
	if err := closed.Close(1000, ""); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed Close error = %v", err)
	}

	closing := testServerConn(newScriptedConn())
	closing.closing.Store(true)
	if err := closing.Close(1000, ""); err != nil {
		t.Fatalf("repeated Close error = %v", err)
	}

	duplicateRaw := newScriptedConn()
	duplicate := testServerConn(duplicateRaw)
	if err := duplicate.sendFrameLocked(frame.Frame{Fin: true, Opcode: frame.Close, Payload: []byte{3, 232}}); err != nil {
		t.Fatal(err)
	}
	if err := duplicate.sendFrameLocked(frame.Frame{Fin: true, Opcode: frame.Close, Payload: []byte{3, 232}}); err != nil {
		t.Fatal(err)
	}
	if duplicateRaw.writes != 1 {
		t.Fatalf("duplicate close writes = %d, want 1", duplicateRaw.writes)
	}

	unbounded := &Conn{config: testServerConfig(&Server{MaxOutboundBytes: -1})}
	if !unbounded.reserveOutbound(math.MaxInt) {
		t.Fatal("unbounded outbound reservation failed")
	}
	unbounded.releaseOutbound(0)
	unbounded.releaseOutbound(1)

	large := &Conn{config: testServerConfig(&Server{MaxMessageSize: math.MaxUint64})}
	if large.maxMessageSizeInt() != math.MaxInt {
		t.Fatalf("max message int = %d, want %d", large.maxMessageSizeInt(), math.MaxInt)
	}

	dialerLimits := &Conn{config: testDialerConfig(&Dialer{MaxOutboundBytes: 123})}
	if dialerLimits.maxOutboundBytes() != 123 {
		t.Fatalf("dialer outbound limit = %d", dialerLimits.maxOutboundBytes())
	}
}

func TestSendQueuesMessagesWithoutFlushBarrier(t *testing.T) {
	raw := newScriptedConn()
	conn := testServerConn(raw)
	for i := 0; i < 17; i++ {
		if err := conn.SendBinary([]byte{byte(i)}); err != nil {
			t.Fatal(err)
		}
	}
	if raw.writes != 17 || raw.flushes != 0 {
		t.Fatalf("transport calls = Write:%d Flush:%d, want 17/0", raw.writes, raw.flushes)
	}
}

func TestCloseRetriesAfterBackpressure(t *testing.T) {
	raw := newScriptedConn()
	conn := testServerConn(raw)
	conn.config = testServerConfig(&Server{MaxOutboundBytes: 4})
	conn.pendingBytes.Store(4)
	if err := conn.Close(1000, ""); !errors.Is(err, ErrBackpressure) {
		t.Fatalf("backpressured Close() = %v", err)
	}
	if conn.closeSent || conn.closing.Load() {
		t.Fatal("failed close poisoned connection state")
	}
	conn.releaseOutbound(4)
	if err := conn.Close(1000, ""); err != nil {
		t.Fatalf("retried Close() = %v", err)
	}
	if !conn.closeSent || !conn.closing.Load() || raw.writes != 1 {
		t.Fatalf("retried close state: sent=%v closing=%v writes=%d", conn.closeSent, conn.closing.Load(), raw.writes)
	}
}

func TestTimerReplacementAndDialerCloseTimeout(t *testing.T) {
	handshakeConn := &Conn{raw: newScriptedConn(), config: testServerConfig(&Server{})}
	handshakeConn.startHandshakeTimer(0)
	handshakeConn.startHandshakeTimer(time.Hour)
	handshakeConn.stopHandshakeTimer()

	closeConn := &Conn{raw: newScriptedConn(), config: testServerConfig(&Server{})}
	closeConn.startCloseTimer()
	closeConn.startCloseTimer()
	closeConn.stopCloseTimer()

	raw := newScriptedConn()
	raw.closed = make(chan struct{})
	dialerConn := &Conn{raw: raw, config: testDialerConfig(&Dialer{CloseTimeout: 5 * time.Millisecond})}
	dialerConn.startCloseTimer()
	select {
	case <-raw.closed:
	case <-time.After(time.Second):
		t.Fatal("dialer close timeout did not close transport")
	}
}

func TestClientHandshakeRejectsInvalidResponses(t *testing.T) {
	invalid := &Conn{config: testDialerConfig(&Dialer{MaxHeaderBytes: DefaultMaxHeaderBytes})}
	invalid.handshake.Store(&handshakeState{
		clientKey: testKey,
		data:      []byte("HTTP/1.1 500 Error\r\n\r\n"),
	})
	if err := invalid.consumeClientHandshake(); err == nil {
		t.Fatal("invalid client handshake was accepted")
	}

	response := []byte("HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: websocket\r\nConnection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: s3pPLMBiTxaQ9kYGzzhZRbK+xOo=\r\n" +
		"Sec-WebSocket-Extensions: permessage-deflate\r\n\r\n")
	unexpectedExtension := &Conn{
		raw:    newScriptedConn(),
		config: testDialerConfig(&Dialer{MaxHeaderBytes: DefaultMaxHeaderBytes}),
	}
	unexpectedExtension.handshake.Store(&handshakeState{clientKey: testKey, data: response})
	if err := unexpectedExtension.consumeClientHandshake(); err == nil {
		t.Fatal("unexpected extension was accepted")
	}
}
