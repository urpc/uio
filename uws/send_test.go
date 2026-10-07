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

	// Text payloads are the sender's contract and are not revalidated on
	// send; what a peer sent has already been checked on read.
	if err := conn.SendText([]byte{0xff}); err != nil {
		t.Fatalf("SendText(unvalidated byte) = %v, want nil", err)
	}
	if err := conn.SendBinary([]byte("12345")); err != frame.ErrMessageTooBig {
		t.Fatalf("SendBinary(oversized) = %v, want %v", err, frame.ErrMessageTooBig)
	}
	if raw.writes != 1 {
		t.Fatalf("sends wrote %d frames, want the one valid text frame", raw.writes)
	}
}

// A fragmented message whose room was checked cannot be refused part-way by a
// real transport; if one does, the partial message on the wire is fatal.
func TestFragmentRefusedPartWayAbortsConnection(t *testing.T) {
	raw := &failNthWriteConn{scriptedConn: newScriptedConn(), failAt: 2, err: uio.ErrOutboundOverflow}
	conn := &Conn{
		raw: raw,
		config: testServerConfig(&Server{
			MaxFramePayload: 4,
			MaxMessageSize:  1024,
		}),
	}
	conn.opened.Store(true)
	if err := conn.SendBinary([]byte("abcdefgh")); !errors.Is(err, ErrBackpressure) {
		t.Fatalf("SendBinary = %v, want ErrBackpressure", err)
	}
	if len(raw.written) != 1 {
		t.Fatalf("transport writes = %d, want only the accepted first fragment", len(raw.written))
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
		t.Fatal("failed fragment retained the write lock")
	}
}

// A message larger than MaxFramePayload is sent in fragments, and only when all
// of them fit the outbound limit: nothing is queued otherwise.
func TestFragmentedMessageAdmittedWholeOrNotAtAll(t *testing.T) {
	raw := &limitedWire{limit: 20}
	server := NewServer(nil)
	server.Events.MaxOutboundBuffered = raw.limit
	server.MaxFramePayload = 4
	conn := &Conn{raw: raw, config: testServerConfig(server)}
	conn.opened.Store(true)
	if err := conn.SendBinary([]byte("12345678")); err != nil {
		t.Fatal(err)
	}
	if err := conn.SendBinary([]byte("abcdefgh")); !errors.Is(err, ErrBackpressure) {
		t.Fatalf("SendBinary beyond the limit = %v, want ErrBackpressure", err)
	}
	if frames := raw.frames(t); len(frames) != 2 || conn.closing.Load() {
		t.Fatalf("frames = %d closing = %v, want the first message only and an open connection", len(frames), conn.closing.Load())
	}
	raw.retire(conn, raw.queuedBytes())
	if err := conn.SendBinary([]byte("abcdefgh")); err != nil {
		t.Fatalf("SendBinary after the queue drained = %v", err)
	}
	frames := raw.frames(t)
	if len(frames) != 4 {
		t.Fatalf("frames = %v, want two messages of two fragments", frames)
	}
	for i, want := range []struct {
		opcode  frame.OpCode
		fin     bool
		payload string
	}{{frame.Binary, false, "1234"}, {frame.Continuation, true, "5678"}, {frame.Binary, false, "abcd"}, {frame.Continuation, true, "efgh"}} {
		if frames[i].Opcode != want.opcode || frames[i].Fin != want.fin || string(frames[i].Payload) != want.payload {
			t.Fatalf("frame %d = %+v, want %+v", i, frames[i], want)
		}
	}
}

func TestCompressedFragmentRefusedPartWayAbortsConnection(t *testing.T) {
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
	payload := bytes.Repeat([]byte("abcdefgh"), 64)
	if err := conn.SendBinary(payload); !errors.Is(err, writeErr) {
		t.Fatalf("SendBinary = %v, want %v", err, writeErr)
	}
	// The script records the failed second fragment too; nothing follows it.
	if len(raw.written) != 2 || raw.written[0][0] != 0x40|byte(frame.Binary) || raw.written[1][0] != byte(frame.Continuation) {
		t.Fatalf("written = %x, want the first compressed fragment and the failed continuation, neither final", raw.written)
	}
	if raw.closes != 1 || !conn.closing.Load() {
		t.Fatalf("transport closes/closing = %d/%v, want 1/true", raw.closes, conn.closing.Load())
	}
	if err := conn.SendBinary(payload); !errors.Is(err, ErrClosed) {
		t.Fatalf("send after the broken stream = %v, want %v", err, ErrClosed)
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
	if err := conn.Close(1000, string([]byte{0xff})); !errors.Is(err, frame.ErrInvalidUTF8) {
		t.Fatalf("invalid close reason = %v, want %v", err, frame.ErrInvalidUTF8)
	}
}

func TestCompressedMessageFragmentsAfterEncoding(t *testing.T) {
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := probe.Addr().String()
	_ = probe.Close()

	handler := &compressedMessageHandler{ready: make(chan error, 1)}
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
		t.Fatal("compressed message was not sent")
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
	want := bytes.Repeat([]byte("compressed-message-"), 32)
	if !bytes.Equal(decoded, want) {
		t.Fatalf("decoded payload length = %d, want %d", len(decoded), len(want))
	}
}

func TestMessageFragmentsByFrameLimit(t *testing.T) {
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := probe.Addr().String()
	_ = probe.Close()

	handler := &fragmentedMessageHandler{ready: make(chan error, 1)}
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
		t.Fatal("fragmented message was not sent")
	}
	var got []byte
	for frames := 0; ; frames++ {
		var header [2]byte
		if _, err = io.ReadFull(reader, header[:]); err != nil {
			t.Fatal(err)
		}
		payload := make([]byte, int(header[1]&0x7f))
		if _, err = io.ReadFull(reader, payload); err != nil {
			t.Fatal(err)
		}
		got = append(got, payload...)
		wantOpcode := byte(frame.Continuation)
		if frames == 0 {
			wantOpcode = byte(frame.Binary)
		}
		if header[0]&0x0f != wantOpcode || len(payload) > 3 {
			t.Fatalf("frame %d header = %x, want opcode %d and at most 3 bytes", frames, header, wantOpcode)
		}
		if header[0]&0x80 != 0 {
			break
		}
	}
	if string(got) != "abcdef" {
		t.Fatalf("fragmented payload = %q", got)
	}
}
