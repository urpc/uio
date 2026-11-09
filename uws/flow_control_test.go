package uws

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"github.com/urpc/uio"
	"github.com/urpc/uio/internal/readcontrol"
	"github.com/urpc/uio/uws/internal/frame"
	"io"
	"net"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func newBackpressureConn(_, _ int) (*limitedWire, *Conn, *Server) {
	server := NewServer(Options{Handler: &batchEchoHandler{}})
	raw := &limitedWire{}
	conn := &Conn{raw: raw, handler: &batchEchoHandler{}, config: testServerConfig(server)}
	conn.setRaw(raw)
	conn.opened.Store(true)
	return raw, conn, server
}

func TestBackpressureBoundsInbound(t *testing.T) {
	opts, err := Options{MaxInboundBuffered: 32 << 10}.resolve()
	if err != nil {
		t.Fatal(err)
	}
	if got := opts.transportEvents().MaxInboundBuffered; got != 32<<10 {
		t.Fatalf("MaxInboundBuffered = %d, want 32 KiB", got)
	}
	opts, err = Options{MaxInboundBuffered: -1}.resolve()
	if err != nil {
		t.Fatal(err)
	}
	if got := opts.transportEvents().MaxInboundBuffered; got != DefaultMaxInboundBuffered {
		t.Fatalf("MaxInboundBuffered = %d, want %d", got, DefaultMaxInboundBuffered)
	}
}

type turnReserveMock struct {
	writeProbeConn
	buf [64]byte
	err error
}

func (m *turnReserveMock) ReserveTurnWrite(n int) ([]byte, bool, error) {
	if m.err != nil {
		return nil, true, m.err
	}
	return m.buf[:n], true, nil
}

func (echoAllHandler) OnOpen(*Conn) {
}

func (echoAllHandler) OnMessage(c *Conn, m Message) {
	_ = c.SendBinary(m.Bytes())
}

func (echoAllHandler) OnClose(*Conn, CloseEvent) {
}

func readEchoFrame(reader *bufio.Reader) ([]byte, error) {
	var header [2]byte
	if _, err := io.ReadFull(reader, header[:]); err != nil {
		return nil, err
	}
	length := int(header[1] & 0x7f)
	switch length {
	case 126:
		var ext [2]byte
		if _, err := io.ReadFull(reader, ext[:]); err != nil {
			return nil, err
		}
		length = int(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err := io.ReadFull(reader, ext[:]); err != nil {
			return nil, err
		}
		length = int(binary.BigEndian.Uint64(ext[:]))
	}
	payload := make([]byte, length)
	_, err := io.ReadFull(reader, payload)
	return payload, err
}

type batchProbeConn struct {
	bufferedProbeConn
	wire     bytes.Buffer
	handoffs int
	outbound int
}

func (c *batchProbeConn) Write(p []byte) (int, error) {
	c.handoffs++
	return c.wire.Write(p)
}

func (c *batchProbeConn) WriteOwned(buffer *uio.Buffer) (int, error) {
	c.handoffs++
	n, _ := c.wire.Write(buffer.Bytes())
	uio.ReleaseBuffer(buffer)
	return n, nil
}

func (c *batchProbeConn) Writev(vec [][]byte) (int, error) {
	c.handoffs++
	total := 0
	for _, segment := range vec {
		n, _ := c.wire.Write(segment)
		total += n
	}
	return total, nil
}

func (c *batchProbeConn) OutboundBuffered() int {
	return c.outbound
}

func (c *batchProbeConn) payloads(t *testing.T) []string {
	t.Helper()
	reader := bufio.NewReader(bytes.NewReader(c.wire.Bytes()))
	var payloads []string
	for reader.Buffered() > 0 || c.wire.Len() > 0 {
		_, payload, _, err := readServerFrame(reader)
		if err != nil {
			break
		}
		payloads = append(payloads, string(payload))
	}
	return payloads
}

type batchEchoHandler struct{ onMessage func(*Conn, Message) }

func (*batchEchoHandler) OnOpen(*Conn) {
}

func (h *batchEchoHandler) OnMessage(conn *Conn, message Message) {
	if h.onMessage != nil {
		h.onMessage(conn, message)
		return
	}
	_ = conn.SendBinary(message.Bytes())
}

func (*batchEchoHandler) OnClose(*Conn, CloseEvent) {
}

func maskedFrames(payloads ...string) []byte {
	var wire []byte
	for _, payload := range payloads {
		wire = frame.Append(wire, frame.Frame{Fin: true, Opcode: frame.Binary, Masked: true, Payload: []byte(payload)}, [4]byte{9, 8, 7, 6})
	}
	return wire
}

func newBatchConn(raw uio.Conn, handler Handler) *Conn {
	conn := &Conn{raw: raw, handler: handler, config: testServerConfig(NewServer(Options{}))}
	conn.opened.Store(true)
	return conn
}

func pingWire(payload string) []byte {
	return frame.Append(nil, frame.Frame{Fin: true, Opcode: frame.Ping, Masked: true, Payload: []byte(payload)}, [4]byte{9, 8, 7, 6})
}

type limitProbeConn struct {
	batchProbeConn
	atLimit bool
}

func (c *limitProbeConn) WriteOwned(buffer *uio.Buffer) (int, error) {
	if c.atLimit {
		return 0, uio.ErrOutboundOverflow
	}
	return c.batchProbeConn.WriteOwned(buffer)
}

func TestRefusedBatchHandoffAbortsInsteadOfDroppingFrames(t *testing.T) {
	raw := &limitProbeConn{atLimit: true}
	conn := newBatchConn(raw, &batchEchoHandler{})
	raw.inbound = maskedFrames("a", "b")
	if err := conn.readAvailable(); err != nil {
		t.Fatal(err)
	}
	if raw.closes != 1 || !conn.closing.Load() {
		t.Fatalf("refused hand-off = closes:%d closing:%v, want 1/true", raw.closes, conn.closing.Load())
	}
	if info := conn.closeInfo(); !errors.Is(info.Err, errBatchRefused) {
		t.Fatalf("close error = %v, want %v", info.Err, errBatchRefused)
	}
	if conn.batch != nil {
		t.Fatal("refused batch left behind")
	}
}

func TestPongAtLimitIsDroppedNotClosed(t *testing.T) {
	raw := &limitProbeConn{atLimit: true}
	conn := newBatchConn(raw, &batchEchoHandler{})
	raw.inbound = pingWire("k")
	if err := conn.readAvailable(); err != nil {
		t.Fatalf("ping at limit closed the read: %v", err)
	}
	if raw.closes != 0 || conn.closing.Load() {
		t.Fatalf("dropped Pong closed the connection: closes:%d closing:%v", raw.closes, conn.closing.Load())
	}
}

const (
	bulkMessages = 8192
	bulkPayload  = 4 << 10
)

func pumpAndDrain(t *testing.T, client *Client, stats func() string) {
	t.Helper()
	go func() {
		payload := make([]byte, bulkPayload)
		for i := range bulkMessages {
			payload[0] = byte(i)
			if client.SendBinary(payload) != nil {
				return
			}
		}
	}()
	for i := range bulkMessages {
		ctx, cancel := context.WithTimeout(context.Background(), testIOTimeout())
		message, err := client.ReadMessage(ctx)
		cancel()
		if err != nil {
			t.Fatalf("echo %d of %d: %v (%s)", i, bulkMessages, err, stats())
		}
		if len(message.Payload) != bulkPayload || message.Payload[0] != byte(i) {
			t.Fatalf("echo %d: %d bytes starting %d", i, len(message.Payload), message.Payload[0])
		}
	}
}

func TestUploadingClientKeepsReadingAnEchoServer(t *testing.T) {
	server, addr, serveDone := startTestServer(t, func() *Server {
		return NewServer(Options{Handler: echoAllHandler{}, HeartbeatInterval: 5 * time.Millisecond, HeartbeatTimeout: time.Minute})
	})
	t.Cleanup(func() {
		stopTestServer(t, server, serveDone)
	})
	dialer := NewDialer(Options{})
	t.Cleanup(func() {
		_ = dialer.Close(nil)
	})
	ctx, cancel := context.WithTimeout(context.Background(), testIOTimeout())
	defer cancel()
	client, err := dialer.DialClient(ctx, "ws://"+addr+"/")
	if err != nil {
		t.Fatal(err)
	}
	pumpAndDrain(t, client, func() string {
		return "transfer"
	})
}

func TestHeartbeatBatchRespectsQuota(t *testing.T) {
	raw := &limitedWire{limit: 64}
	c := newLimitedConn(raw, time.Hour)
	c.heartbeat = &heartbeatState{}
	var sends []error
	c.handler = &batchEchoHandler{onMessage: func(c *Conn, m Message) {
		sends = append(sends, c.SendBinary(m.Bytes()))
	}}
	raw.inbound = maskedFrames(string(bytes.Repeat([]byte("a"), 40)), string(bytes.Repeat([]byte("b"), 40)))
	if err := c.readAvailable(); err != nil {
		t.Fatal(err)
	}
	held := 0
	if c.batch != nil {
		held = c.batch.Len()
	}
	if len(sends) != 2 || sends[0] != nil || !errors.Is(sends[1], ErrBackpressure) || held != 0 || len(raw.frames(t)) != 1 {
		t.Fatalf("quota failed: sends=%v delivered=%d retained=%d transport=%d pending=%d", sends, len(raw.frames(t)), held, raw.queuedBytes(), int64(c.OutboundBuffered()))
	}
}

func startEchoServer(t *testing.T, handler Handler) string {
	t.Helper()
	return startRobustServer(t, Options{Pollers: 1}, handler)
}

func dialTestWS(t *testing.T, addr string) (net.Conn, *bufio.Reader) {
	t.Helper()
	var conn net.Conn
	var err error
	for deadline := time.Now().Add(testIOTimeout()); conn == nil && time.Now().Before(deadline); {
		conn, err = net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err != nil {
			time.Sleep(time.Millisecond)
		}
	}
	if conn == nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() {
		_ = conn.Close()
	})
	request := "GET / HTTP/1.1\r\nHost: " + addr + "\r\n" + "Connection: Upgrade\r\nUpgrade: websocket\r\n" + "Sec-WebSocket-Version: 13\r\nSec-WebSocket-Key: " + testKey + "\r\n\r\n"
	if _, err := conn.Write([]byte(request)); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(conn)
	if line, err := reader.ReadString('\n'); err != nil || len(line) < 12 || line[:12] != "HTTP/1.1 101" {
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
	return conn, reader
}

func readFrameWithOpcode(reader *bufio.Reader) (frame.OpCode, []byte, error) {
	var header [2]byte
	if _, err := io.ReadFull(reader, header[:]); err != nil {
		return 0, nil, err
	}
	length := int(header[1] & 0x7f)
	switch length {
	case 126:
		var ext [2]byte
		if _, err := io.ReadFull(reader, ext[:]); err != nil {
			return 0, nil, err
		}
		length = int(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err := io.ReadFull(reader, ext[:]); err != nil {
			return 0, nil, err
		}
		length = int(binary.BigEndian.Uint64(ext[:]))
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(reader, payload); err != nil {
		return 0, nil, err
	}
	return frame.OpCode(header[0] & 0x0f), payload, nil
}

type limitedWire struct {
	writeProbeConn
	mu         sync.Mutex
	limit      int
	queued     int
	wire       bytes.Buffer
	closeCalls int
	closedCh   chan struct{}
	inbound    []byte
}

func (c *limitedWire) admitLocked(n int) bool {
	return c.limit <= 0 || c.queued+n <= c.limit
}

func (c *limitedWire) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.admitLocked(len(p)) {
		return 0, uio.ErrOutboundOverflow
	}
	c.queued += len(p)
	c.wire.Write(p)
	return len(p), nil
}

func (c *limitedWire) WriteOwned(buffer *uio.Buffer) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := buffer.Len()
	if !c.admitLocked(n) {
		return 0, uio.ErrOutboundOverflow
	}
	c.queued += n
	c.wire.Write(buffer.Bytes())
	uio.ReleaseBuffer(buffer)
	return n, nil
}

func (c *limitedWire) Writev(vec [][]byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	total := 0
	for _, segment := range vec {
		total += len(segment)
	}
	if !c.admitLocked(total) {
		return 0, uio.ErrOutboundOverflow
	}
	c.queued += total
	for _, segment := range vec {
		c.wire.Write(segment)
	}
	return total, nil
}

func (c *limitedWire) OutboundBuffered() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.queued
}

func (c *limitedWire) Flush() error {
	return nil
}

func (c *limitedWire) Close() error {
	return c.Abort(nil)
}

func (c *limitedWire) Abort(error) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closeCalls++
	if c.closeCalls == 1 && c.closedCh != nil {
		close(c.closedCh)
	}
	return nil
}

func (c *limitedWire) closeCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closeCalls
}

func (c *limitedWire) queuedBytes() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.queued
}

func (c *limitedWire) retire(conn *Conn, n int) {
	c.mu.Lock()
	n = min(n, c.queued)
	c.queued -= n
	c.mu.Unlock()
	conn.onTransportProgress(n)
}

func (c *limitedWire) frames(t *testing.T) []frame.Frame {
	t.Helper()
	c.mu.Lock()
	wire := append([]byte(nil), c.wire.Bytes()...)
	c.mu.Unlock()
	reader := bufio.NewReader(bytes.NewReader(wire))
	var frames []frame.Frame
	for reader.Buffered() > 0 || len(wire) > 0 {
		first, payload, fin, err := readServerFrame(reader)
		if err != nil {
			break
		}
		frames = append(frames, frame.Frame{Opcode: frame.OpCode(first & 0x0f), Payload: payload, Fin: fin})
	}
	return frames
}

func (c *limitedWire) PeekChunk() []byte {
	return c.inbound
}

func (c *limitedWire) InboundBuffered() int {
	return len(c.inbound)
}

func (c *limitedWire) Discard(n int) (int, error) {
	n = min(n, len(c.inbound))
	c.inbound = c.inbound[n:]
	return n, nil
}

func newLimitedConn(raw *limitedWire, closeTimeout time.Duration) *Conn {
	server := NewServer(Options{})
	server.Options.MaxOutboundBuffered = raw.limit
	server.CloseTimeout = closeTimeout
	conn := &Conn{raw: raw, config: testServerConfig(server)}
	conn.opened.Store(true)
	return conn
}

func TestRepeatedTransportCloseHasOneOwner(t *testing.T) {
	raw := newScriptedConn()
	conn := testServerConn(raw)
	if err := conn.closeTransport(); err != nil {
		t.Fatal(err)
	}
	if err := conn.closeTransport(); err != nil {
		t.Fatal(err)
	}
	if raw.closes != 1 || conn.writes.close.phase() != transportCloseClaimed {
		t.Fatalf("transport closes = %d, phase = %d", raw.closes, conn.writes.close.phase())
	}
}

func TestPeerCloseReplyWaitsForOutboundRoom(t *testing.T) {
	raw := &limitedWire{limit: 64}
	conn := newLimitedConn(raw, time.Hour)
	if err := conn.SendBinary(bytes.Repeat([]byte("x"), 60)); err != nil {
		t.Fatal(err)
	}
	if err := conn.acceptControl(frame.Frame{Fin: true, Opcode: frame.Close, Payload: []byte{3, 232}}); err != nil {
		t.Fatal(err)
	}
	if !conn.writes.close.closeIsDeferred() || raw.closeCount() != 0 {
		t.Fatalf("deferred:%v closes:%d, want a deferred reply and an open transport", conn.writes.close.closeIsDeferred(), raw.closeCount())
	}
	raw.retire(conn, 62)
	frames := raw.frames(t)
	if len(frames) != 2 || frames[1].Opcode != frame.Close || frame.CloseCode(frames[1].Payload) != 1000 {
		t.Fatalf("wire = %v, want the message then the Close reply", frames)
	}
	if raw.closeCount() != 0 {
		t.Fatal("transport closed before the Close reply retired")
	}
	raw.retire(conn, raw.queuedBytes())
	if raw.closeCount() != 1 {
		t.Fatalf("closes = %d, want 1", raw.closeCount())
	}
}

func TestPeerCloseInARoundWaitsForTheBatchedReplies(t *testing.T) {
	raw := &limitedWire{}
	conn := newBatchConn(raw, &batchEchoHandler{})
	raw.inbound = append(maskedFrames("a", "bb"), frame.Append(nil, frame.Frame{Fin: true, Opcode: frame.Close, Masked: true, Payload: []byte{3, 232}}, [4]byte{1, 2, 3, 4})...)
	raw.retire(conn, 0)
	if err := conn.readAvailable(); err != nil && !errors.Is(err, ErrClosed) {
		t.Fatal(err)
	}
	if raw.closeCount() != 0 {
		t.Fatal("transport closed before the batched replies went out")
	}
	frames := raw.frames(t)
	if len(frames) != 3 || string(frames[0].Payload) != "a" || string(frames[1].Payload) != "bb" || frames[2].Opcode != frame.Close {
		t.Fatalf("wire = %v, want both replies then the Close reply", frames)
	}
	if pending := int64(conn.OutboundBuffered()); pending != int64(raw.queuedBytes()) {
		t.Fatalf("pending = %d, transport queued = %d", pending, raw.queuedBytes())
	}
	raw.retire(conn, 3+4)
	if raw.closeCount() != 0 {
		t.Fatal("transport closed before the Close reply retired")
	}
	raw.retire(conn, raw.queuedBytes())
	if raw.closeCount() != 1 {
		t.Fatalf("closes = %d, want 1", raw.closeCount())
	}
}

type externalReadControlHandler struct {
	open    func(*Conn)
	message func(*Conn, Message)
	closed  func(*Conn, CloseEvent)
	inbound func(*Conn, int)
}

func (h *externalReadControlHandler) OnOpen(c *Conn) {
	if h.open != nil {
		h.open(c)
	}
}

func (h *externalReadControlHandler) OnMessage(c *Conn, m Message) {
	if h.message != nil {
		h.message(c, m)
	}
}

func (h *externalReadControlHandler) OnClose(c *Conn, event CloseEvent) {
	if h.closed != nil {
		h.closed(c, event)
	}
}

func (h *externalReadControlHandler) OnBytesRead(c *Conn, n int) {
	if h.inbound != nil {
		h.inbound(c, n)
	}
}

func (*externalReadControlHandler) OnBytesWritten(*Conn, int) {
}

func TestPausedReadsKeepStandaloneHeartbeatTimersRunning(t *testing.T) {
	for _, side := range []string{"server", "dialer"} {
		t.Run(side, func(t *testing.T) {
			externalReadControlCases(t, func(t *testing.T, serverTLS, clientTLS *tls.Config) {
				paused := make(chan error, 1)
				closed := make(chan CloseEvent, 1)
				handler := &externalReadControlHandler{open: func(c *Conn) {
					paused <- c.PauseRead()
				}, closed: func(_ *Conn, event CloseEvent) {
					closed <- event
				}}
				server, addr, done := startTestServer(t, func() *Server {
					opts := Options{TLSConfig: serverTLS, Pollers: 1}
					if side == "server" {
						opts.Handler = handler
						opts.HeartbeatInterval = 10 * time.Millisecond
						opts.HeartbeatTimeout = 30 * time.Millisecond
					}
					return NewServer(opts)
				})
				t.Cleanup(func() {
					stopTestServer(t, server, done)
				})
				if side == "server" {
					_, _ = dialExternalReadControl(t, addr, clientTLS, nil)
				} else {
					dialer := NewDialer(Options{Handler: handler, TLSConfig: clientTLS, Pollers: 1, HeartbeatInterval: 10 * time.Millisecond, HeartbeatTimeout: 30 * time.Millisecond})
					t.Cleanup(func() {
						_ = dialer.Close(nil)
					})
					scheme := "ws://"
					if clientTLS != nil {
						scheme = "wss://"
					}
					if err := dialer.Dial(context.Background(), scheme+addr+"/", nil); err != nil {
						t.Fatal(err)
					}
				}
				if err := waitExternalReadControl(t, paused, "heartbeat read pause"); err != nil {
					t.Fatal(err)
				}
				if event := waitExternalReadControl(t, closed, "heartbeat timeout while reads are paused"); event.Code != 1001 {
					t.Fatalf("heartbeat close code = %d, want 1001", event.Code)
				}
			})
		})
	}
}

func waitExternalReadControl[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(testIOTimeout()):
		t.Fatalf("timed out waiting for %s", what)
		return *new(T)
	}
}

func dialExternalReadControl(t *testing.T, addr string, tlsConfig *tls.Config, tail []byte) (net.Conn, *bufio.Reader) {
	t.Helper()
	var peer net.Conn
	var err error
	dialer := &net.Dialer{Timeout: testIOTimeout()}
	if tlsConfig == nil {
		peer, err = dialer.Dial("tcp", addr)
	} else {
		peer, err = tls.DialWithDialer(dialer, "tcp", addr, tlsConfig)
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = peer.Close()
	})
	if err := peer.SetDeadline(time.Now().Add(testIOTimeout())); err != nil {
		t.Fatal(err)
	}
	request := "GET / HTTP/1.1\r\nHost: " + addr + "\r\n" + "Connection: Upgrade\r\nUpgrade: websocket\r\n" + "Sec-WebSocket-Version: 13\r\nSec-WebSocket-Key: " + testKey + "\r\n\r\n"
	wire := append([]byte(request), tail...)
	if n, err := peer.Write(wire); err != nil || n != len(wire) {
		t.Fatalf("write upgrade and trailing frames: %d/%d bytes, %v", n, len(wire), err)
	}
	reader := bufio.NewReader(peer)
	if line, err := reader.ReadString('\n'); err != nil || len(line) < 12 || line[:12] != "HTTP/1.1 101" {
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
	return peer, reader
}

func externalReadControlCases(t *testing.T, test func(*testing.T, *tls.Config, *tls.Config)) {
	t.Helper()
	t.Run("ws", func(t *testing.T) {
		test(t, nil, nil)
	})
	t.Run("wss", func(t *testing.T) {
		serverTLS, clientTLS := wssConfigs(t)
		test(t, serverTLS, clientTLS)
	})
}

func TestExternalPauseResumeSocketBufferedFrames(t *testing.T) {
	externalReadControlCases(t, func(t *testing.T, serverTLS, clientTLS *tls.Config) {
		for _, high := range []int{0, -1} {
			t.Run(fmt.Sprintf("automatic_high=%d", high), func(t *testing.T) {
				opened := make(chan *Conn, 1)
				paused := make(chan error, 1)
				messages := make(chan string, 3)
				calls := 0
				handler := &externalReadControlHandler{open: func(c *Conn) {
					opened <- c
				}, message: func(c *Conn, m Message) {
					calls++
					if calls == 1 {
						paused <- c.PauseRead()
					}
					messages <- string(m.Payload)
					if err := c.Send(m.Type, m.Payload); err != nil {
						t.Error(err)
					}
				}}
				server, addr, done := startTestServer(t, func() *Server {
					return NewServer(Options{Handler: handler, TLSConfig: serverTLS, Pollers: 1, MaxInboundBuffered: high})
				})
				t.Cleanup(func() {
					stopTestServer(t, server, done)
				})
				peer, reader := dialExternalReadControl(t, addr, clientTLS, nil)
				conn := waitExternalReadControl(t, opened, "server connection")
				wire := maskedFrames("one", "two", "three")
				if n, err := peer.Write(wire); err != nil || n != len(wire) {
					t.Fatalf("write frames: %d/%d, %v", n, len(wire), err)
				}
				if err := waitExternalReadControl(t, paused, "PauseRead result"); err != nil {
					t.Fatal(err)
				}
				if payload, err := readEchoFrame(reader); err != nil || string(payload) != "one" {
					t.Fatalf("first echo = %q, %v", payload, err)
				}
				if message := waitExternalReadControl(t, messages, "first message"); message != "one" {
					t.Fatalf("first message = %q", message)
				}
				select {
				case message := <-messages:
					t.Fatalf("paused connection delivered %q", message)
				default:
				}
				if err := conn.ResumeRead(); err != nil {
					t.Fatal(err)
				}
				for _, want := range []string{"two", "three"} {
					if payload, err := readEchoFrame(reader); err != nil || string(payload) != want {
						t.Fatalf("resumed echo = %q, %v; want %q", payload, err, want)
					}
					if message := waitExternalReadControl(t, messages, "resumed message"); message != want {
						t.Fatalf("resumed message = %q, want %q", message, want)
					}
				}
			})
		}
	})
}

type externalPauseHandler struct {
	open    func(*Conn)
	message func(*Conn, Message)
}

func (h *externalPauseHandler) OnOpen(c *Conn) {
	if h.open != nil {
		h.open(c)
	}
}

func (h *externalPauseHandler) OnMessage(c *Conn, m Message) {
	if h.message != nil {
		h.message(c, m)
	}
}

func (*externalPauseHandler) OnClose(*Conn, CloseEvent) {
}

func TestPauseAndResumeReadsAreIdempotentAndResumeWakesBufferedInput(t *testing.T) {
	raw, c, _ := newBackpressureConn(64, 16)
	delivered := 0
	c.handler = &externalPauseHandler{message: func(*Conn, Message) {
		delivered++
	}}
	if err := c.ResumeRead(); err != nil {
		t.Fatal(err)
	}
	if raw.wakes != 0 {
		t.Fatal("resuming an active connection scheduled a wake")
	}
	for range 2 {
		if err := c.PauseRead(); err != nil {
			t.Fatal(err)
		}
	}
	raw.inbound = maskedFrames("buffered")
	if err := c.readAvailable(); err != nil {
		t.Fatal(err)
	}
	if delivered != 0 || len(raw.inbound) == 0 {
		t.Fatal("paused reads consumed input")
	}
	for range 2 {
		if err := c.ResumeRead(); err != nil {
			t.Fatal(err)
		}
	}
	if raw.wakes != 1 {
		t.Fatalf("resume wakes = %d, want one", raw.wakes)
	}
	if err := c.readAvailable(); err != nil {
		t.Fatal(err)
	}
	if delivered != 1 {
		t.Fatalf("resume delivered %d messages, want one", delivered)
	}
}

func TestReadPauseAPIsRejectUnavailableConnections(t *testing.T) {
	var nilConn *Conn
	for _, c := range []*Conn{nilConn, {}, {raw: &limitedWire{}}} {
		if err := c.PauseRead(); !errors.Is(err, ErrNotReady) {
			t.Fatalf("PauseRead on unavailable connection = %v", err)
		}
		if err := c.ResumeRead(); !errors.Is(err, ErrNotReady) {
			t.Fatalf("ResumeRead on unavailable connection = %v", err)
		}
	}
	_, c, _ := newBackpressureConn(64, 16)
	c.closed.Store(true)
	if err := c.PauseRead(); !errors.Is(err, ErrClosed) {
		t.Fatalf("PauseRead after close = %v", err)
	}
	if err := c.ResumeRead(); !errors.Is(err, ErrClosed) {
		t.Fatalf("ResumeRead after close = %v", err)
	}
	_, closing, _ := newBackpressureConn(64, 16)
	closing.closing.Store(true)
	if err := closing.PauseRead(); !errors.Is(err, ErrClosed) {
		t.Fatalf("PauseRead while closing = %v", err)
	}
	if err := closing.ResumeRead(); !errors.Is(err, ErrClosed) {
		t.Fatalf("ResumeRead while closing = %v", err)
	}
}

type externalObserveWriteWire struct {
	*limitedWire
	observe func()
}

func (w *externalObserveWriteWire) WriteOwned(buffer *uio.Buffer) (int, error) {
	n, err := w.limitedWire.WriteOwned(buffer)
	if w.observe != nil {
		w.observe()
	}
	return n, err
}

func TestOutboundBufferedAndResumeReadsAreSafeInASynchronousWriteReport(t *testing.T) {
	raw, c, _ := newBackpressureConn(64, 16)
	wire := &externalObserveWriteWire{limitedWire: raw}
	c.raw = wire
	c.round.Store(roundBatching)
	if err := c.PauseRead(); err != nil {
		t.Fatal(err)
	}
	if err := c.SendBinary([]byte("report")); err != nil {
		t.Fatal(err)
	}
	var observed int
	var resumeErr error
	wire.observe = func() {
		observed = c.OutboundBuffered()
		resumeErr = c.ResumeRead()
	}
	done := make(chan struct{})
	go func() {
		c.endRound(c.round.Load())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(testIOTimeout()):
		t.Fatal("queue snapshot or resume deadlocked in a synchronous write report")
	}
	if observed < 8 || resumeErr != nil || raw.wakes != 1 {
		t.Fatalf("report observed=%d, resume=%v, wakes=%d", observed, resumeErr, raw.wakes)
	}
	if got := c.OutboundBuffered(); got != 8 {
		t.Fatalf("settled queue snapshot = %d, want 8", got)
	}
}

type externalQueueObserver struct {
	batchEchoHandler
	observe func(*Conn, int)
}

func (*externalQueueObserver) OnBytesRead(*Conn, int) {
}

func (o *externalQueueObserver) OnBytesWritten(c *Conn, n int) {
	o.observe(c, n)
}

func TestDeferredOutboundReportsKeepTheirTotalAndDoNotOverlap(t *testing.T) {
	_, c, _ := newBackpressureConn(64, 16)
	var active atomic.Int32
	var overlap atomic.Bool
	var total atomic.Int64
	c.handler = &externalQueueObserver{observe: func(c *Conn, n int) {
		if active.Add(1) != 1 {
			overlap.Store(true)
		}
		_ = c.OutboundBuffered()
		runtime.Gosched()
		total.Add(int64(n))
		active.Add(-1)
	}}
	c.config.observeTraffic = true
	var reports sync.WaitGroup
	reports.Add(1)
	go func() {
		defer reports.Done()
		for range 1000 {
			c.observeOutbound(1)
		}
	}()
	for range 1000 {
		c.batchHandoff.Store(-1)
		c.observeOutbound(2)
		c.batchHandoff.Store(0)
		c.flushOutboundObservation()
	}
	reports.Wait()
	c.flushOutboundObservation()
	if total.Load() != 3000 || overlap.Load() {
		t.Fatalf("reported bytes=%d, overlapping callbacks=%v; want 3000 serialized bytes", total.Load(), overlap.Load())
	}
}

type delayingExecutor struct {
	holdUntil atomic.Int64
	submitted atomic.Int64
}

func (e *delayingExecutor) run(task uio.IOTask) {
	e.submitted.Add(1)
	go func() {
		if d := time.Until(time.Unix(0, e.holdUntil.Load())); d > 0 {
			time.Sleep(d)
		}
		task.RunTask()
	}()
}

func (e *delayingExecutor) Submit(task uio.IOTask) bool {
	e.run(task)
	return true
}

func (e *delayingExecutor) SubmitBatch(tasks []uio.IOTask) int {
	for _, task := range tasks {
		e.run(task)
	}
	return len(tasks)
}

func TestHeartbeatSurvivesABudgetYieldBeforeThePong(t *testing.T) {
	exec := &delayingExecutor{}
	handler := &externalReadControlHandler{}
	server, addr, done := startTestServer(t, func() *Server {
		return NewServer(Options{Handler: handler, Pollers: 1, Executor: exec, HeartbeatInterval: 100 * time.Millisecond, HeartbeatTimeout: 200 * time.Millisecond})
	})
	t.Cleanup(func() {
		stopTestServer(t, server, done)
	})
	peer, reader := dialExternalReadControl(t, addr, nil, nil)
	opcode, payload, err := readFrameWithOpcode(reader)
	if err != nil || opcode != frame.Ping {
		t.Fatalf("first frame = %v %q, %v; want Ping", opcode, payload, err)
	}
	exec.holdUntil.Store(time.Now().Add(350 * time.Millisecond).UnixNano())
	parts := make([]string, minFramesPerDataEvent*2+16)
	for i := range parts {
		parts[i] = "."
	}
	pong := frame.Append(nil, frame.Frame{Fin: true, Opcode: frame.Pong, Masked: true, Payload: payload}, [4]byte{9, 8, 7, 6})
	if _, err := peer.Write(append(maskedFrames(parts...), pong...)); err != nil {
		t.Fatal(err)
	}
	_ = peer.SetReadDeadline(time.Now().Add(3 * time.Second))
	opcode, _, err = readFrameWithOpcode(reader)
	if err != nil {
		t.Fatal(err)
	}
	if opcode == frame.Close {
		t.Fatal("the round that yielded expired a Ping its continuation answered")
	}
}

type probeReadControl struct {
	control atomic.Pointer[readcontrol.Control]
}

type probeReadAccess = readcontrol.Access[probeReadControlGetter]

type probeReadControlGetter struct{}

func (probeReadControlGetter) GetReadControl(source any) *readcontrol.Control {
	return source.(interface{ probeControl() *readcontrol.Control }).probeControl()
}

func (p *probeReadControl) get(resume func() error) *readcontrol.Control {
	if c := p.control.Load(); c != nil {
		return c
	}
	c := readcontrol.New(nil, resume)
	if p.control.CompareAndSwap(nil, c) {
		return c
	}
	return p.control.Load()
}

func (p *probeReadControl) ReadPaused() bool {
	c := p.control.Load()
	return c != nil && c.ReadPaused()
}

func (c *writeProbeConn) PauseRead() error {
	return c.probeReadControl.get(c.Wake).PauseRead()
}

func (c *writeProbeConn) ResumeRead() error {
	return c.probeReadControl.get(c.Wake).ResumeRead()
}

func (c *writeProbeConn) ReadPaused() bool {
	return c.probeReadControl.ReadPaused()
}

func (c *writeProbeConn) probeControl() *readcontrol.Control {
	return c.probeReadControl.get(c.Wake)
}

func (c *echoPathConn) PauseRead() error {
	return c.probeReadControl.get(nil).PauseRead()
}

func (c *echoPathConn) ResumeRead() error {
	return c.probeReadControl.get(nil).ResumeRead()
}

func (c *echoPathConn) ReadPaused() bool {
	return c.probeReadControl.ReadPaused()
}

func (c *echoPathConn) probeControl() *readcontrol.Control {
	return c.probeReadControl.get(nil)
}

func TestCloseRacingReadPausesLeavesNoWebSocketPause(t *testing.T) {
	for range 100 {
		raw, c, _ := newBackpressureConn(64, 16)
		start := make(chan struct{})
		var done sync.WaitGroup
		done.Add(2)
		go func() {
			defer done.Done()
			<-start
			_ = c.PauseRead()
		}()
		go func() {
			defer done.Done()
			<-start
			c.markClosing()
		}()
		close(start)
		done.Wait()
		if raw.ReadPaused() || c.externalReadPaused() {
			t.Fatal("a pause raced past local Close and blocked its handshake")
		}
	}
}

func TestReadPausedAndStartupGuardContracts(t *testing.T) {
	var nilConn *Conn
	if nilConn.ReadPaused() {
		t.Fatal("nil Conn reported ReadPaused")
	}
	_, c, _ := newBackpressureConn(64, 16)
	if err := c.PauseRead(); err != nil || !c.ReadPaused() {
		t.Fatalf("PauseRead = %v, ReadPaused = %v", err, c.ReadPaused())
	}
	c.closing.Store(true)
	if c.ReadPaused() {
		t.Fatal("closing Conn reported ReadPaused")
	}
	if err := NewServer(Options{Pollers: -1}).Serve(); !errors.Is(err, ErrInvalidOptions) {
		t.Fatalf("invalid Serve = %v", err)
	}
	s := NewServer(Options{})
	s.started.Store(true)
	if err := s.Serve(); !errors.Is(err, ErrServerStarted) {
		t.Fatalf("second Serve = %v", err)
	}
	d := NewDialer(Options{})
	if err := d.start(nil); err != nil {
		t.Fatal(err)
	}
	_ = d.Close(nil)
	closingPing := &Conn{raw: newScriptedConn(), heartbeat: &heartbeatState{}}
	closingPing.opened.Store(true)
	closingPing.closing.Store(true)
	if err := closingPing.sendHeartbeatPing(time.Now()); !errors.Is(err, ErrClosed) {
		t.Fatalf("closing sendHeartbeatPing = %v", err)
	}
	closingPing.sendHeartbeatClose(1001, "bye")

	var cp connCloseProgress
	if !cp.requestTransportClose() || !cp.requestTransportClose() {
		t.Fatal("requestTransportClose idempotence failed")
	}
	if !cp.claimAbort() || cp.claimAbort() {
		t.Fatal("claimAbort idempotence failed")
	}
	if cp.requestTransportClose() {
		t.Fatal("claimed progress accepted requestTransportClose")
	}

	hc := &Conn{raw: newScriptedConn()}
	hc.watchHandshakeContext(nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hc.watchHandshakeContext(ctx) // nil handshake state
	hs := &handshakeState{}
	hc.handshake.Store(hs)
	hc.watchHandshakeContext(ctx)
	hc.watchHandshakeContext(ctx) // replaces previous stop
	hc.closed.Store(true)
	hc.watchHandshakeContext(ctx)
	hc.expireHandshake(hs, 0, nil)

	nilConn.ensureCloseWatch()
	watchConn := &Conn{raw: newScriptedConn(), config: testServerConfig(nil)}
	watchConn.ensureCloseWatch()
	watchConn.ensureCloseWatch() // idempotent

	trMock := &turnReserveMock{}
	trConn := &Conn{config: testServerConfig(nil)}
	trConn.setRaw(trMock)
	trConn.opened.Store(true)
	if err := trConn.SendBinary([]byte("fast")); err != nil {
		t.Fatalf("TurnReserver SendBinary = %v", err)
	}
	trMock.err = uio.ErrOutboundOverflow
	if err := trConn.SendBinary([]byte("full")); !errors.Is(err, ErrBackpressure) {
		t.Fatalf("TurnReserver overflow = %v", err)
	}
}

type echoAllHandler struct{}
