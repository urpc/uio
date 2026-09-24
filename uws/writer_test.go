package uws

import (
	"bufio"
	"bytes"
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

func TestTransportBackpressureMapsToUWSError(t *testing.T) {
	raw := newScriptedConn()
	raw.writeErr = uio.ErrOutboundOverflow
	server := &Server{MaxFramePayload: 1024, MaxMessageSize: 1024}
	conn := &Conn{raw: raw, config: testServerConfig(server)}
	conn.opened.Store(true)

	payload := bytes.Repeat([]byte("x"), 50)
	if err := conn.SendBinary(payload); !errors.Is(err, ErrBackpressure) {
		t.Fatalf("SendBinary() error = %v, want %v", err, ErrBackpressure)
	}
	if pending := conn.writes.close.pendingBytes.Load(); pending != 0 {
		t.Fatalf("pending bytes = %d, want 0", pending)
	}
	raw.writeErr = nil
	if err := conn.SendBinary(payload); err != nil {
		t.Fatalf("SendBinary() after recovery = %v", err)
	}
}

func TestServerFrameCoalescesOnlyBelowWriteBufferThreshold(t *testing.T) {
	raw := &writeProbeConn{}
	server := &Server{
		Events:          &uio.Events{WriteBufferedThreshold: 64},
		MaxFramePayload: 1024,
		MaxMessageSize:  1024,
	}
	conn := &Conn{raw: raw, config: testServerConfig(server)}
	conn.opened.Store(true)

	if err := conn.SendBinary(make([]byte, 32)); err != nil {
		t.Fatal(err)
	}
	if raw.writes != 1 || raw.writevs != 0 {
		t.Fatalf("small frame transport calls = Write:%d Writev:%d, want 1/0", raw.writes, raw.writevs)
	}
	if err := conn.SendBinary(make([]byte, 64)); err != nil {
		t.Fatal(err)
	}
	if raw.writes != 2 || raw.writevs != 1 {
		t.Fatalf("large frame transport calls = Write:%d Writev:%d, want 2/1", raw.writes, raw.writevs)
	}

	disabledRaw := &writeProbeConn{}
	disabled := &Conn{
		raw: disabledRaw,
		config: testServerConfig(&Server{
			Events:          &uio.Events{WriteBufferedThreshold: -1},
			MaxFramePayload: 1024,
			MaxMessageSize:  1024,
		}),
	}
	disabled.opened.Store(true)
	if err := disabled.SendBinary(make([]byte, 32)); err != nil {
		t.Fatal(err)
	}
	if disabledRaw.writes != 1 || disabledRaw.writevs != 1 {
		t.Fatalf("disabled coalescing transport calls = Write:%d Writev:%d, want 1/1", disabledRaw.writes, disabledRaw.writevs)
	}
}

func TestFrameWireSizesAndServerWrites(t *testing.T) {
	tests := []struct {
		payload int
		masked  bool
		want    int
	}{
		{payload: 0, want: 2},
		{payload: 125, masked: true, want: 131},
		{payload: 126, want: 130},
		{payload: 0xffff, masked: true, want: 0xffff + 8},
		{payload: 0x10000, want: 0x10000 + 10},
	}
	for _, test := range tests {
		if got := frameWireSize(test.payload, test.masked); got != test.want {
			t.Fatalf("frameWireSize(%d, %v) = %d, want %d", test.payload, test.masked, got, test.want)
		}
	}

	conn := &Conn{
		raw: &writeProbeConn{},
		config: testServerConfig(&Server{
			MaxFramePayload: DefaultMaxFramePayload,
			MaxMessageSize:  DefaultMaxMessageSize,
		}),
	}
	conn.opened.Store(true)
	if err := conn.SendBinary(make([]byte, 32)); err != nil {
		t.Fatal(err)
	}
	if err := conn.SendBinary(make([]byte, 1<<16)); err != nil {
		t.Fatal(err)
	}
}

func TestSendValidatesTextAndMessageLimit(t *testing.T) {
	raw := &writeProbeConn{}
	server := &Server{MaxFramePayload: 1024, MaxMessageSize: 4}
	conn := &Conn{raw: raw, config: testServerConfig(server)}
	conn.opened.Store(true)

	if err := conn.SendText([]byte{0xff}); err != frame.ErrInvalidUTF8 {
		t.Fatalf("SendText(invalid) = %v, want %v", err, frame.ErrInvalidUTF8)
	}
	if err := conn.SendBinary([]byte("12345")); err != frame.ErrMessageTooBig {
		t.Fatalf("SendBinary(oversized) = %v, want %v", err, frame.ErrMessageTooBig)
	}
	if raw.writes != 0 {
		t.Fatalf("invalid sends wrote %d frames", raw.writes)
	}
}

func TestStreamingWriterMakesOtherSendsFailFast(t *testing.T) {
	conn := testServerConn(newScriptedConn())
	writer, err := conn.BeginMessage(BinaryMessage)
	if err != nil {
		t.Fatal(err)
	}
	checks := []struct {
		name string
		call func() error
	}{
		{name: "send", call: func() error { return conn.SendBinary([]byte("x")) }},
		{name: "ping", call: func() error { return conn.Ping(nil) }},
		{name: "close", call: func() error { return conn.Close(1000, "") }},
		{name: "writer", call: func() error {
			_, err := conn.BeginMessage(BinaryMessage)
			return err
		}},
	}
	for _, check := range checks {
		t.Run(check.name, func(t *testing.T) {
			started := time.Now()
			if err := check.call(); !errors.Is(err, ErrWriteBusy) {
				t.Fatalf("error = %v, want %v", err, ErrWriteBusy)
			}
			if elapsed := time.Since(started); elapsed > 100*time.Millisecond {
				t.Fatalf("operation blocked for %v", elapsed)
			}
		})
	}
	_ = writer.fail(ErrClosed)
}

func TestStreamingWriterAllowsInboundPing(t *testing.T) {
	raw := newScriptedConn()
	conn := testServerConn(raw)
	writer, err := conn.BeginMessage(BinaryMessage)
	if err != nil {
		t.Fatal(err)
	}
	if err = conn.acceptControl(frame.Frame{Fin: true, Opcode: frame.Ping, Payload: []byte("ping")}); err != nil {
		t.Fatal(err)
	}
	if conn.closing.Load() {
		t.Fatal("ping closed connection while streaming writer was active")
	}
	if len(raw.written) != 1 || raw.written[0][0]&0x0f != byte(frame.Pong) {
		t.Fatalf("written frames = %x, want pong", raw.written)
	}
	_ = writer.fail(ErrClosed)
}

func TestStreamingWriterHandsInboundCloseToWriterRelease(t *testing.T) {
	raw := newScriptedConn()
	conn := testServerConn(raw)
	writer, err := conn.BeginMessage(BinaryMessage)
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte{0x03, 0xe8}
	if err = conn.acceptControl(frame.Frame{Fin: true, Opcode: frame.Close, Payload: payload}); err != nil {
		t.Fatal(err)
	}
	if len(raw.written) != 0 {
		t.Fatalf("close frame bypassed active writer: %x", raw.written)
	}
	if err = writer.Close(); !errors.Is(err, ErrClosed) {
		t.Fatalf("Writer.Close error = %v, want %v", err, ErrClosed)
	}
	if len(raw.written) != 1 || raw.written[0][0]&0x0f != byte(frame.Close) {
		t.Fatalf("written frames = %x, want close", raw.written)
	}
}

func TestUncompressedWriterValidatesText(t *testing.T) {
	raw := &writeProbeConn{closed: make(chan struct{})}
	server := &Server{MaxFramePayload: 1024, MaxMessageSize: 1024}
	conn := &Conn{raw: raw, config: testServerConfig(server)}
	conn.opened.Store(true)

	writer, err := conn.BeginMessage(TextMessage)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := writer.Write([]byte{0xff}); n != 0 || err != frame.ErrInvalidUTF8 {
		t.Fatalf("invalid text Write() = (%d, %v)", n, err)
	}
	if err = writer.Close(); err != frame.ErrInvalidUTF8 {
		t.Fatalf("invalid text Close() = %v, want %v", err, frame.ErrInvalidUTF8)
	}
	completeTestOutbound(conn)
	select {
	case <-raw.closed:
	case <-time.After(time.Second):
		t.Fatal("invalid text did not close transport")
	}
}

func TestWriterBackpressureDoesNotFinalizePartialMessage(t *testing.T) {
	raw := &failNthWriteConn{scriptedConn: newScriptedConn(), failAt: 2, err: uio.ErrOutboundOverflow}
	conn := &Conn{
		raw: raw,
		config: testServerConfig(&Server{
			MaxFramePayload: 4,
			MaxMessageSize:  1024,
		}),
	}
	conn.opened.Store(true)
	writer, err := conn.BeginMessage(BinaryMessage)
	if err != nil {
		t.Fatal(err)
	}
	if n, writeErr := writer.Write([]byte("abcdefgh")); n != 4 || !errors.Is(writeErr, ErrBackpressure) {
		t.Fatalf("Writer.Write = %d, %v; want 4, ErrBackpressure", n, writeErr)
	}
	if closeErr := writer.Close(); !errors.Is(closeErr, ErrBackpressure) {
		t.Fatalf("Writer.Close error = %v, want ErrBackpressure", closeErr)
	}
	if len(raw.written) != 2 {
		t.Fatalf("transport writes = %d, want one successful and one rejected fragment", len(raw.written))
	}
	if raw.written[0][0]&0x80 != 0 {
		t.Fatalf("first fragment unexpectedly has FIN set: %#x", raw.written[0][0])
	}
	if raw.closes != 1 || !conn.closing.Load() {
		t.Fatalf("transport closes/closing = %d/%v, want 1/true", raw.closes, conn.closing.Load())
	}
	lockAvailable := make(chan struct{})
	go func() {
		conn.writes.mu.Lock()
		conn.unlockWrite()
		close(lockAvailable)
	}()
	select {
	case <-lockAvailable:
	case <-time.After(time.Second):
		t.Fatal("Writer.Write failure retained connection write ownership")
	}
}

func TestCompressedWriterFailureDoesNotEmitMoreFrames(t *testing.T) {
	writeErr := errors.New("compressed write failed")
	raw := &failNthWriteConn{scriptedConn: newScriptedConn(), failAt: 2, err: writeErr}
	conn := &Conn{
		raw: raw,
		config: testServerConfig(&Server{
			MaxFramePayload: 4,
			MaxMessageSize:  1024,
		}),
		compression: &compressionState{encoder: compress.NewEncoder(-1, true)},
	}
	conn.opened.Store(true)
	writer, err := conn.BeginMessage(BinaryMessage)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = writer.Write([]byte("abcdefghijklmnopqrstuvwxyz")); !errors.Is(err, writeErr) {
		t.Fatalf("Writer.Write error = %v, want %v", err, writeErr)
	}
	writesAfterFailure := len(raw.written)
	if closeErr := writer.Close(); !errors.Is(closeErr, writeErr) {
		t.Fatalf("Writer.Close error = %v, want %v", closeErr, writeErr)
	}
	if len(raw.written) != writesAfterFailure {
		t.Fatalf("Writer.Close emitted %d frames after failure", len(raw.written)-writesAfterFailure)
	}
	for _, wire := range raw.written {
		if len(wire) > 0 && wire[0]&0x80 != 0 {
			t.Fatalf("failed compressed writer emitted FIN frame: %#x", wire[0])
		}
	}
	if raw.closes != 1 || !conn.closing.Load() {
		t.Fatalf("transport closes/closing = %d/%v, want 1/true", raw.closes, conn.closing.Load())
	}
}

type failNthWriteConn struct {
	*scriptedConn
	failAt int
	err    error
}

func (conn *failNthWriteConn) Writev(buffers [][]byte) (int, error) {
	if conn.writes+1 != conn.failAt {
		return conn.scriptedConn.Writev(buffers)
	}
	previous := conn.writeErr
	conn.writeErr = conn.err
	n, err := conn.scriptedConn.Writev(buffers)
	conn.writeErr = previous
	return n, err
}

func (conn *failNthWriteConn) WriteOwned(buffer *uio.Buffer) (int, error) {
	if conn.writes+1 != conn.failAt {
		return conn.scriptedConn.WriteOwned(buffer)
	}
	previous := conn.writeErr
	conn.writeErr = conn.err
	n, err := conn.scriptedConn.WriteOwned(buffer)
	conn.writeErr = previous
	return n, err
}

func TestWriterValidatesSplitUTF8(t *testing.T) {
	validChunks := [][][]byte{
		{[]byte{0xc2}, []byte{0xa2}},
		{[]byte{0xe2}, []byte{0x82}, []byte{0xac}},
		{[]byte{0xf0}, []byte{0x9f, 0x98}, []byte{0x80}},
		{[]byte("prefix\xe2"), []byte{0x82, 0xac}, []byte("suffix")},
	}
	for _, chunks := range validChunks {
		writer := &Writer{opcode: frame.Text}
		for _, chunk := range chunks {
			if !writer.validateText(chunk) {
				t.Fatalf("valid split UTF-8 rejected: %x", chunks)
			}
		}
		if writer.textTailLen != 0 {
			t.Fatalf("valid split retained %d tail bytes", writer.textTailLen)
		}
	}
	invalidChunks := [][][]byte{
		{[]byte{0xff}},
		{[]byte{0xe2}, []byte{0x28}},
		{[]byte{0xf0, 0x9f}, []byte{0xff}},
	}
	for _, chunks := range invalidChunks {
		writer := &Writer{opcode: frame.Text}
		valid := true
		for _, chunk := range chunks {
			if !writer.validateText(chunk) {
				valid = false
				break
			}
		}
		if valid {
			t.Fatalf("invalid split UTF-8 accepted: %x", chunks)
		}
	}
}

func TestDisableUTF8CheckAllowsTextMessages(t *testing.T) {
	raw := &writeProbeConn{}
	server := &Server{
		MaxFramePayload:  1024,
		MaxMessageSize:   1024,
		DisableUTF8Check: true,
	}
	conn := &Conn{raw: raw, config: testServerConfig(server)}
	conn.opened.Store(true)
	if err := conn.SendText([]byte{0xff}); err != nil {
		t.Fatalf("SendText with validation disabled = %v", err)
	}
	writer, err := conn.BeginMessage(TextMessage)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := writer.Write([]byte{0xe2}); err != nil || n != 1 {
		t.Fatalf("Writer.Write with validation disabled = %d, %v", n, err)
	}
	if err = writer.Close(); err != nil {
		t.Fatalf("Writer.Close with validation disabled = %v", err)
	}
	if err = conn.Close(1000, string([]byte{0xff})); !errors.Is(err, frame.ErrInvalidUTF8) {
		t.Fatalf("invalid close reason = %v, want %v", err, frame.ErrInvalidUTF8)
	}
}

func TestCompressedWriterAbortsIncompleteText(t *testing.T) {
	raw := &writeProbeConn{}
	server := &Server{MaxFramePayload: 1024, MaxMessageSize: 1024}
	conn := &Conn{
		raw:    raw,
		config: testServerConfig(server),
		compression: &compressionState{
			encoder: compress.NewEncoder(-1, true),
		},
	}
	conn.opened.Store(true)
	writer, err := conn.BeginMessage(TextMessage)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = writer.Write([]byte{0xe2}); err != nil {
		t.Fatalf("incomplete text Write() = %v", err)
	}
	if err = writer.Close(); err != frame.ErrInvalidUTF8 {
		t.Fatalf("Close() error = %v, want %v", err, frame.ErrInvalidUTF8)
	}
	completeTestOutbound(conn)
	if raw.closes != 1 {
		t.Fatalf("transport closes = %d, want 1", raw.closes)
	}
}

func TestCompressedMessageWriterFragmentsAfterEncoding(t *testing.T) {
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := probe.Addr().String()
	_ = probe.Close()

	handler := &compressedStreamHandler{ready: make(chan error, 1)}
	server := NewServer(handler)
	server.EnableCompression = true
	server.MaxFramePayload = 8
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
		"Sec-WebSocket-Version: 13\r\nSec-WebSocket-Key: " + testKey + "\r\n" +
		"Sec-WebSocket-Extensions: permessage-deflate; server_max_window_bits=8; client_max_window_bits=8\r\n\r\n"
	if _, err = client.Write([]byte(request)); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(client)
	line, err := reader.ReadString('\n')
	if err != nil || !strings.HasPrefix(line, "HTTP/1.1 101 ") {
		t.Fatalf("handshake response = %q, %v", line, err)
	}
	responseHeaders := line
	for {
		line, err = reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		responseHeaders += line
		if line == "\r\n" {
			break
		}
	}
	if !strings.Contains(responseHeaders, "server_max_window_bits=8") ||
		!strings.Contains(responseHeaders, "client_max_window_bits=8") {
		t.Fatalf("window bits were not negotiated: %s", responseHeaders)
	}
	select {
	case err = <-handler.ready:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(testIOTimeout()):
		t.Fatal("compressed stream writer did not finish")
	}

	var compressed []byte
	frames := 0
	for {
		first, payload, fin, err := readServerFrame(reader)
		if err != nil {
			t.Fatal(err)
		}
		if frames == 0 {
			if first&0x40 == 0 || first&0x0f != byte(frame.Binary) {
				t.Fatalf("first compressed frame = %#x", first)
			}
		} else if first&0x40 != 0 || first&0x0f != byte(frame.Continuation) {
			t.Fatalf("continuation frame = %#x", first)
		}
		compressed = append(compressed, payload...)
		frames++
		if fin {
			break
		}
	}
	if frames < 2 {
		t.Fatalf("frame count = %d, want fragmented output", frames)
	}
	decoded, err := compress.Decompress(compressed, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	want := bytes.Repeat([]byte("compressed-stream-"), 32)
	if !bytes.Equal(decoded, want) {
		t.Fatalf("decoded payload length = %d, want %d", len(decoded), len(want))
	}
}

func TestMessageWriterFragmentsByFrameLimit(t *testing.T) {
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := probe.Addr().String()
	_ = probe.Close()

	handler := &streamHandler{ready: make(chan error, 1)}
	server := NewServer(handler)
	server.MaxFramePayload = 3
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
	case err := <-handler.ready:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(testIOTimeout()):
		t.Fatal("stream writer did not finish")
	}
	var got []byte
	for {
		var header [2]byte
		if _, err = io.ReadFull(reader, header[:]); err != nil {
			t.Fatal(err)
		}
		payload := make([]byte, int(header[1]&0x7f))
		if _, err = io.ReadFull(reader, payload); err != nil {
			t.Fatal(err)
		}
		got = append(got, payload...)
		if header[0]&0x80 != 0 {
			if header[0] != 0x80 || header[1] != 0 {
				t.Fatalf("final frame = %x, want 8000", header)
			}
			break
		}
	}
	if string(got) != "abcdef" {
		t.Fatalf("fragmented payload = %q", got)
	}
}
