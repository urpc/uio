package uws

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"github.com/urpc/uio"
	"github.com/urpc/uio/uws/internal/compress"
	"github.com/urpc/uio/uws/internal/frame"
	"github.com/urpc/uio/uws/internal/handshake"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

type authRecorder struct {
	mu        sync.Mutex
	userdata  []any
	callbacks int
	opened    chan struct{}
}

func newAuthRecorder() *authRecorder {
	return &authRecorder{opened: make(chan struct{}, 16)}
}

func (h *authRecorder) OnOpen(c *Conn) {
	h.mu.Lock()
	h.userdata = append(h.userdata, c.Userdata())
	h.callbacks++
	h.mu.Unlock()
	h.opened <- struct{}{}
}

func (h *authRecorder) OnMessage(c *Conn, m Message) {
	_ = c.SendBinary(m.Bytes())
}

func (h *authRecorder) OnClose(*Conn, CloseEvent) {
	h.mu.Lock()
	h.callbacks++
	h.mu.Unlock()
}

func (h *authRecorder) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.callbacks
}

func tokenAuthorize(r *http.Request) (any, error) {
	user, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || user == "" {
		return nil, &RejectError{Status: http.StatusUnauthorized, Header: http.Header{"Www-Authenticate": {"Bearer"}}, Reason: "missing or bad token"}
	}
	return user, nil
}

func startAuthServer(t *testing.T, handler Handler, authorize func(*http.Request) (any, error)) string {
	t.Helper()
	server, addr, serveDone := startTestServer(t, func() *Server {
		s := NewServer(Options{Handler: handler})
		s.Authorize = authorize
		return s
	})
	t.Cleanup(func() {
		stopTestServer(t, server, serveDone)
	})
	return addr
}

func dialAuth(t *testing.T, dialer *Dialer, addr string) (*Client, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testIOTimeout())
	defer cancel()
	return dialer.DialClient(ctx, "ws://"+addr+"/")
}

func TestAuthorizeRejectionReachesClient(t *testing.T) {
	for _, backendPath := range []string{"native", "net/http"} {
		t.Run(backendPath, func(t *testing.T) {
			recorder := newAuthRecorder()
			var addr string
			if backendPath == "native" {
				addr = startAuthServer(t, recorder, tokenAuthorize)
			} else {
				server := NewServer(Options{Handler: recorder})
				server.Authorize = tokenAuthorize
				serveWithoutListener(t, server)
				httpServer := httptest.NewServer(server)
				t.Cleanup(httpServer.Close)
				addr = strings.TrimPrefix(httpServer.URL, "http://")
			}
			dialer := &Dialer{Header: http.Header{"Authorization": {"Basic nope"}}}
			t.Cleanup(func() {
				_ = dialer.Close(nil)
			})
			_, err := dialAuth(t, dialer, addr)
			var refused *HandshakeError
			if !errors.As(err, &refused) {
				t.Fatalf("DialClient error = %v, want a HandshakeError", err)
			}
			if refused.StatusCode != http.StatusUnauthorized || refused.Reason != "missing or bad token" || refused.Header.Get("Www-Authenticate") != "Bearer" {
				t.Fatalf("refusal = %d %q %v", refused.StatusCode, refused.Reason, refused.Header)
			}
			time.Sleep(20 * time.Millisecond)
			if n := recorder.count(); n != 0 {
				t.Fatalf("a refused connection made %d server callbacks", n)
			}
		})
	}
}

func TestAuthorizePlainErrorRefusesWith403(t *testing.T) {
	addr := startAuthServer(t, newAuthRecorder(), func(*http.Request) (any, error) {
		return nil, errors.New("no")
	})
	dialer := &Dialer{}
	t.Cleanup(func() {
		_ = dialer.Close(nil)
	})
	_, err := dialAuth(t, dialer, addr)
	var refused *HandshakeError
	if !errors.As(err, &refused) || refused.StatusCode != http.StatusForbidden {
		t.Fatalf("DialClient error = %v, want a 403 HandshakeError", err)
	}
}

func TestRequestHeaderPerDial(t *testing.T) {
	recorder := newAuthRecorder()
	addr := startAuthServer(t, recorder, tokenAuthorize)
	dialer := NewDialer(Options{Handler: clientUserdataHandler{}})
	dialer.Header = http.Header{"Authorization": {"Bearer static"}}
	dialer.RequestHeader = func(userdata any) http.Header {
		return http.Header{"authorization": {"Bearer " + userdata.(*clientHandler).name}}
	}
	t.Cleanup(func() {
		_ = dialer.Close(nil)
	})
	for _, name := range []string{"bob", "carol"} {
		handler := &clientHandler{name: name, open: make(chan struct{}), closed: make(chan struct{}), message: make(chan Message, 1)}
		if err := dialer.Dial(context.Background(), "ws://"+addr+"/", handler); err != nil {
			t.Fatal(err)
		}
		select {
		case <-handler.open:
		case <-handler.closed:
			t.Fatalf("dial %s was refused", name)
		case <-time.After(testIOTimeout()):
			t.Fatalf("dial %s did not open", name)
		}
		select {
		case <-recorder.opened:
		case <-time.After(testIOTimeout()):
			t.Fatalf("the server connection of %s did not open", name)
		}
	}
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	if len(recorder.userdata) != 2 || recorder.userdata[0] != "bob" || recorder.userdata[1] != "carol" {
		t.Fatalf("server Userdata = %v, want [bob carol]", recorder.userdata)
	}
}

func TestDialerRefusesReservedHeaders(t *testing.T) {
	static := &Dialer{Header: http.Header{"Sec-WebSocket-Key": {"x"}}}
	t.Cleanup(func() {
		_ = static.Close(nil)
	})
	if err := static.Dial(context.Background(), "ws://127.0.0.1:1/", nil); !errors.Is(err, ErrInvalidOptions) {
		t.Fatalf("Dial with a reserved Header = %v, want %v", err, ErrInvalidOptions)
	}
	perDial := &Dialer{RequestHeader: func(any) http.Header {
		return http.Header{"Host": {"evil"}}
	}}
	t.Cleanup(func() {
		_ = perDial.Close(nil)
	})
	if err := perDial.Dial(context.Background(), "ws://127.0.0.1:1/", nil); !errors.Is(err, ErrInvalidOptions) {
		t.Fatalf("Dial with a reserved RequestHeader = %v, want %v", err, ErrInvalidOptions)
	}
	broken := &Dialer{Header: http.Header{"X-Token": {"a\r\nInjected: 1"}}}
	t.Cleanup(func() {
		_ = broken.Close(nil)
	})
	if err := broken.Dial(context.Background(), "ws://127.0.0.1:1/", nil); !errors.Is(err, ErrInvalidOptions) {
		t.Fatalf("Dial with a header value carrying CRLF = %v, want %v", err, ErrInvalidOptions)
	}
}

func TestAuthorizeSeesRemoteAddr(t *testing.T) {
	remote := make(chan string, 4)
	addr := startAuthServer(t, newAuthRecorder(), func(r *http.Request) (any, error) {
		remote <- r.RemoteAddr
		return nil, nil
	})
	dialer := &Dialer{}
	t.Cleanup(func() {
		_ = dialer.Close(nil)
	})
	client, err := dialAuth(t, dialer, addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = client.Close(1000, "")
	}()
	local := client.LocalAddr().String()
	select {
	case got := <-remote:
		if got != local {
			t.Fatalf("Authorize RemoteAddr = %q, want the peer's %q", got, local)
		}
	case <-time.After(testIOTimeout()):
		t.Fatal("Authorize did not run")
	}
}

func TestConnForwardsDeadlines(t *testing.T) {
	raw := &writeProbeConn{}
	conn := &Conn{raw: raw}
	deadline := time.Unix(1, 2)
	readDeadline := time.Unix(3, 4)
	writeDeadline := time.Unix(5, 6)
	if err := conn.SetDeadline(deadline); err != nil {
		t.Fatal(err)
	}
	if err := conn.SetReadDeadline(readDeadline); err != nil {
		t.Fatal(err)
	}
	if err := conn.SetWriteDeadline(writeDeadline); err != nil {
		t.Fatal(err)
	}
	if err := conn.SetNoDelay(true); err != nil {
		t.Fatal(err)
	}
	if !raw.deadline.Equal(deadline) || !raw.readDeadline.Equal(readDeadline) || !raw.writeDeadline.Equal(writeDeadline) {
		t.Fatalf("forwarded deadlines = %v, %v, %v", raw.deadline, raw.readDeadline, raw.writeDeadline)
	}
	if !raw.noDelay {
		t.Fatal("SetNoDelay was not forwarded")
	}
	sentinel := errors.New("deadline failed")
	raw.deadlineErr = sentinel
	if err := conn.SetWriteDeadline(time.Time{}); !errors.Is(err, sentinel) {
		t.Fatalf("SetWriteDeadline() error = %v, want %v", err, sentinel)
	}
	raw.noDelayErr = sentinel
	if err := conn.SetNoDelay(false); !errors.Is(err, sentinel) {
		t.Fatalf("SetNoDelay() error = %v, want %v", err, sentinel)
	}
	var nilConn *Conn
	if err := nilConn.SetDeadline(time.Time{}); !errors.Is(err, ErrNotReady) {
		t.Fatalf("nil SetDeadline() error = %v, want %v", err, ErrNotReady)
	}
	if err := (&Conn{}).SetReadDeadline(time.Time{}); !errors.Is(err, ErrNotReady) {
		t.Fatalf("unattached SetReadDeadline() error = %v, want %v", err, ErrNotReady)
	}
	if err := (&Conn{}).SetWriteDeadline(time.Time{}); !errors.Is(err, ErrNotReady) {
		t.Fatalf("unattached SetWriteDeadline() error = %v, want %v", err, ErrNotReady)
	}
	if err := nilConn.SetNoDelay(true); !errors.Is(err, ErrNotReady) {
		t.Fatalf("nil SetNoDelay() error = %v, want %v", err, ErrNotReady)
	}
	virtualConn := &Conn{raw: struct{ uio.Lower }{}}
	if err := virtualConn.SetNoDelay(true); !errors.Is(err, uio.ErrUnsupported) {
		t.Fatalf("virtual SetNoDelay() error = %v, want %v", err, uio.ErrUnsupported)
	}
}

type testAddr string

func (a testAddr) Network() string {
	return "test"
}

func (a testAddr) String() string {
	return string(a)
}

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

func (c *scriptedConn) LocalAddr() net.Addr {
	return c.local
}

func (c *scriptedConn) RemoteAddr() net.Addr {
	return c.remote
}

func (c *scriptedConn) Userdata() any {
	return c.userdata
}

func (c *scriptedConn) SetUserdata(value any) {
	c.userdata = value
}

func (c *scriptedConn) InboundBuffered() int {
	return len(c.inbound)
}

func (c *scriptedConn) Peek(dst []byte) []byte {
	n := copy(dst, c.inbound)
	return dst[:n]
}

func (c *scriptedConn) PeekChunk() []byte {
	return c.inbound
}

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
	n := len(payload)
	if c.short && n > 0 {
		n--
	}
	c.outbound += n
	return n, nil
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
		total--
	}
	c.outbound += total
	return total, nil
}

func (c *scriptedConn) WriteOwned(buffer *uio.Buffer) (int, error) {
	if c.writeErr != nil && errors.Is(c.writeErr, uio.ErrOutboundOverflow) {
		c.writes++
		return 0, c.writeErr
	}
	payload := append([]byte(nil), buffer.Bytes()...)
	uio.ReleaseBuffer(buffer)
	c.writes++
	c.written = append(c.written, payload)
	if c.writeErr != nil {
		return 0, c.writeErr
	}
	n := len(payload)
	if c.short && n > 0 {
		n--
	}
	c.outbound += n
	return n, nil
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

func (c *scriptedConn) Close() error {
	return c.Abort(io.EOF)
}

func testServerConn(raw *scriptedConn) *Conn {
	conn := &Conn{raw: raw, config: testServerConfig(&Server{Options: Options{MaxFramePayload: 1024, MaxMessageSize: 1024}})}
	conn.opened.Store(true)
	return conn
}

func TestRejectHandshakeServerAndClientPaths(t *testing.T) {
	cause := errors.New("bad upgrade")
	serverRaw := newScriptedConn()
	serverConn := &Conn{raw: serverRaw, config: testServerConfig(&Server{})}
	err := serverConn.rejectHandshake(cause)
	if !protocolCloseOwnsTransport(err) || !errors.Is(err, cause) {
		t.Fatalf("server rejection = %v, want the cause with the close owned", err)
	}
	if len(serverRaw.written) != 1 || !strings.HasPrefix(string(serverRaw.written[0]), "HTTP/1.1 400 Bad Request") {
		t.Fatalf("rejection response = %q", serverRaw.written)
	}
	if serverRaw.closes != 0 {
		t.Fatal("the transport closed before the rejection was sent")
	}
	completeTestOutbound(serverConn)
	if serverRaw.closes != 1 {
		t.Fatalf("server transport closes = %d after the rejection left, want 1", serverRaw.closes)
	}
	clientRaw := newScriptedConn()
	clientConn := &Conn{raw: clientRaw, config: testDialerConfig(new(Dialer))}
	if err := clientConn.rejectHandshake(cause); err != cause {
		t.Fatalf("client rejection = %v, want %v", err, cause)
	}
	if clientRaw.closes != 1 {
		t.Fatalf("client transport closes = %d, want 1", clientRaw.closes)
	}
	openedRaw := newScriptedConn()
	openedConn := &Conn{raw: openedRaw}
	openedConn.opened.Store(true)
	if err := openedConn.rejectHandshake(cause); err != cause {
		t.Fatalf("opened rejection = %v, want %v", err, cause)
	}
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
	const heartbeatNonce = uint64(0x0102030405060708)
	conn.heartbeat.beginPing(time.Now().Add(-time.Second), heartbeatNonce)
	if err := conn.acceptControl(frame.Frame{Fin: true, Opcode: frame.Ping, Payload: []byte("ping")}); err != nil {
		t.Fatal(err)
	}
	if len(raw.written) != 1 || raw.written[0][0]&0x0f != byte(frame.Pong) {
		t.Fatalf("pong frame = %x", raw.written)
	}
	if err := conn.acceptControl(frame.Frame{Fin: true, Opcode: frame.Pong, Payload: []byte("mismatch")}); err != nil {
		t.Fatal(err)
	}
	if !conn.heartbeat.pingOutstanding.Load() {
		t.Fatal("unrelated pong acknowledged the heartbeat")
	}
	var pongPayload [8]byte
	binary.BigEndian.PutUint64(pongPayload[:], heartbeatNonce)
	if err := conn.acceptControl(frame.Frame{Fin: true, Opcode: frame.Pong, Payload: pongPayload[:]}); err != nil {
		t.Fatal(err)
	}
	conn.heartbeat.mu.Lock()
	queuedAt := conn.heartbeat.pingQueuedAt
	sentAt := conn.heartbeat.pingSentAt
	nonce := conn.heartbeat.pingNonce
	wireWait := conn.heartbeat.pingWireWait
	conn.heartbeat.mu.Unlock()
	if conn.heartbeat.pingOutstanding.Load() || queuedAt != 0 || sentAt != 0 || nonce != 0 || wireWait != 0 {
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
	server := NewServer(Options{Handler: handler})
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
	tooLarge.config = testServerConfig(&Server{Options: Options{MaxFramePayload: 1}})
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

func TestCloseValidatesCodeAndReason(t *testing.T) {
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
}

func TestHandshakeAndCloseLifecycleErrors(t *testing.T) {
	expired := &Conn{}
	expired.handshake.Store(&handshakeState{expired: true})
	if expired.markOpened() {
		t.Fatal("expired handshake was opened")
	}
	client := &Conn{config: testDialerConfig(&Dialer{Options: Options{MaxHeaderBytes: 1}})}
	client.handshake.Store(&handshakeState{data: []byte("xx")})
	if _, err := client.consumeClientHandshake(); !errors.Is(err, handshake.ErrBadRequest) {
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
	notReady := &Conn{raw: notReadyRaw, config: testServerConfig(&Server{Options: Options{MaxMessageSize: 1024}})}
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
	large := &Conn{config: testServerConfig(&Server{Options: Options{MaxMessageSize: math.MaxUint64}})}
	if large.maxMessageSizeInt() != math.MaxInt {
		t.Fatalf("max message int = %d, want %d", large.maxMessageSizeInt(), math.MaxInt)
	}
}

func TestTransportOwnedWriteErrors(t *testing.T) {
	writeErr := errors.New("owned write failed")
	failedRaw := newScriptedConn()
	failedRaw.writeErr = writeErr
	failed := testServerConn(failedRaw)
	failedBuffer := uio.AcquireBuffer(4)
	_, _ = failedBuffer.WriteString("data")
	if err := failed.writeTransportOwned(failedBuffer); !errors.Is(err, writeErr) {
		t.Fatalf("owned write error = %v, want %v", err, writeErr)
	}
	shortRaw := newScriptedConn()
	shortRaw.short = true
	short := testServerConn(shortRaw)
	shortBuffer := uio.AcquireBuffer(4)
	_, _ = shortBuffer.WriteString("data")
	if err := short.writeTransportOwned(shortBuffer); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("short owned write error = %v, want io.ErrShortWrite", err)
	}
}

func TestCloseUnderBackpressureIsDeferred(t *testing.T) {
	raw := newScriptedConn()
	raw.writeErr = uio.ErrOutboundOverflow
	conn := testServerConn(raw)
	defer conn.stopCloseTimer()
	if err := conn.Close(1000, ""); err != nil {
		t.Fatalf("backpressured Close() = %v, want the frame deferred", err)
	}
	if conn.writes.close.closeFrameWasSent() || !conn.writes.close.closeIsDeferred() || !conn.closing.Load() {
		t.Fatalf("close state: sent=%v deferred=%v closing=%v", conn.writes.close.closeFrameWasSent(), conn.writes.close.closeIsDeferred(), conn.closing.Load())
	}
	if err := conn.Close(1000, ""); err != nil {
		t.Fatalf("repeated Close() = %v", err)
	}
	raw.writeErr = nil
	conn.onTransportProgress(0)
	if !conn.writes.close.closeFrameWasSent() || conn.writes.close.closeIsDeferred() || len(raw.written) != 1 {
		t.Fatalf("drained close state: sent=%v deferred=%v written=%d", conn.writes.close.closeFrameWasSent(), conn.writes.close.closeIsDeferred(), len(raw.written))
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
	dialerConn := &Conn{raw: raw, config: testDialerConfig(&Dialer{Options: Options{CloseTimeout: 5 * time.Millisecond}})}
	dialerConn.startCloseTimer()
	select {
	case <-raw.closed:
	case <-time.After(time.Second):
		t.Fatal("dialer close timeout did not close transport")
	}
}

func TestClientHandshakeRejectsInvalidResponses(t *testing.T) {
	invalid := &Conn{config: testDialerConfig(&Dialer{Options: Options{MaxHeaderBytes: DefaultMaxHeaderBytes}})}
	invalid.handshake.Store(&handshakeState{clientKey: testKey, data: []byte("HTTP/1.1 500 Error\r\n\r\n")})
	if _, err := invalid.consumeClientHandshake(); err == nil {
		t.Fatal("invalid client handshake was accepted")
	}
	response := []byte("HTTP/1.1 101 Switching Protocols\r\n" + "Upgrade: websocket\r\nConnection: Upgrade\r\n" + "Sec-WebSocket-Accept: s3pPLMBiTxaQ9kYGzzhZRbK+xOo=\r\n" + "Sec-WebSocket-Extensions: permessage-deflate\r\n\r\n")
	unexpectedExtension := &Conn{raw: newScriptedConn(), config: testDialerConfig(&Dialer{Options: Options{MaxHeaderBytes: DefaultMaxHeaderBytes}})}
	unexpectedExtension.handshake.Store(&handshakeState{clientKey: testKey, data: response})
	if _, err := unexpectedExtension.consumeClientHandshake(); err == nil {
		t.Fatal("unexpected extension was accepted")
	}
}

func TestHTTPUpgradeRejectsInvalidRequestsBeforeHijack(t *testing.T) {
	for _, tc := range []struct {
		name    string
		request func() *http.Request
		want    int
	}{{"nil", func() *http.Request {
		return nil
	}, http.StatusBadRequest}, {"tls", func() *http.Request {
		r := newHTTPUpgradeRequest()
		r.TLS = new(tls.ConnectionState)
		return r
	}, http.StatusNotImplemented}, {"http2", func() *http.Request {
		r := newHTTPUpgradeRequest()
		r.ProtoMajor = 2
		return r
	}, http.StatusNotImplemented}, {"invalid-upgrade", func() *http.Request {
		r := newHTTPUpgradeRequest()
		r.Header.Del("Upgrade")
		return r
	}, http.StatusBadRequest}, {"invalid-extension", func() *http.Request {
		r := newHTTPUpgradeRequest()
		r.Header.Set("Sec-WebSocket-Extensions", "permessage-deflate; server_max_window_bits=7")
		return r
	}, http.StatusBadRequest}} {
		t.Run(tc.name, func(t *testing.T) {
			s := NewServer(Options{EnableCompression: true})
			s.config = testServerConfig(s)
			s.ready.Store(true)
			w := httptest.NewRecorder()
			s.ServeHTTP(w, tc.request())
			if w.Code != tc.want || w.Header().Get("Connection") != "close" {
				t.Fatalf("status=%d headers=%v", w.Code, w.Header())
			}
		})
	}
}

func TestHTTPUpgradeRechecksServerAfterAuthorization(t *testing.T) {
	s := NewServer(Options{})
	s.Authorize = func(*http.Request) (any, error) {
		s.ready.Store(false)
		return nil, nil
	}
	s.config = testServerConfig(s)
	s.ready.Store(true)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, newHTTPUpgradeRequest())
	if w.Code != http.StatusInternalServerError {
		t.Fatal(w.Code)
	}
}

type contractHijacker struct {
	*httptest.ResponseRecorder
	conn   net.Conn
	buffer *bufio.ReadWriter
}

func (w contractHijacker) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return w.conn, w.buffer, nil
}

func TestHTTPUpgradeRejectsNonTCPHijack(t *testing.T) {
	a, b := net.Pipe()
	defer b.Close()
	body := new(bytes.Buffer)
	w := contractHijacker{httptest.NewRecorder(), a, bufio.NewReadWriter(bufio.NewReader(bytes.NewReader(nil)), bufio.NewWriter(body))}
	s := NewServer(Options{})
	s.config = testServerConfig(s)
	s.ready.Store(true)
	s.ServeHTTP(w, newHTTPUpgradeRequest())
	if _, err := b.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
	if !bytes.Contains(body.Bytes(), []byte("400 Bad Request")) {
		t.Fatal(body.String())
	}
	rejectHijacked(nil, nil, "ignored")
}

func TestAdoptedUpgradeWriteFailuresNeverPublishOpen(t *testing.T) {
	for _, tc := range []struct {
		name               string
		writeErr, flushErr error
		expired            bool
	}{{"write", io.ErrClosedPipe, nil, false}, {"flush", nil, io.ErrClosedPipe, false}, {"expired", nil, nil, true}} {
		t.Run(tc.name, func(t *testing.T) {
			raw := newScriptedConn()
			raw.writeErr = tc.writeErr
			raw.flushErr = tc.flushErr
			raw.closeCause = make(chan error, 1)
			h := new(recordingHandler)
			c := &Conn{raw: raw, handler: h, config: testServerConfig(nil)}
			state := &handshakeState{expired: tc.expired, upgrade: &httpUpgrade{request: handshake.Request{Key: testKey}}}
			c.handshake.Store(state)
			new(Server).openHTTPConnection(c, state)
			if c.opened.Load() || len(h.events) != 0 || raw.closes != 1 {
				t.Fatalf("opened=%v events=%v closes=%d", c.opened.Load(), h.events, raw.closes)
			}
			c.stopHandshakeTimer()
		})
	}
}

func lifecycleTestTimeout() time.Duration {
	if testing.CoverMode() == "atomic" {
		return 90 * time.Second
	}
	return 30 * time.Second
}

type lifecycleEchoHandler struct{}

func (lifecycleEchoHandler) OnOpen(*Conn) {
}

func (lifecycleEchoHandler) OnMessage(conn *Conn, message Message) {
	_ = conn.SendBinary(message.Bytes())
}

func (lifecycleEchoHandler) OnClose(*Conn, CloseEvent) {
}

type clientUserdataHandler struct{}

func (clientUserdataHandler) OnOpen(c *Conn) {
	c.Userdata().(*clientHandler).OnOpen(c)
}

func (clientUserdataHandler) OnMessage(c *Conn, m Message) {
	c.Userdata().(*clientHandler).OnMessage(c, m)
}

func (clientUserdataHandler) OnClose(c *Conn, e CloseEvent) {
	c.Userdata().(*clientHandler).OnClose(c, e)
}

func startConfiguredLifecycleTestServer(t *testing.T, handler Handler, configure func(*Server)) (*Server, string, <-chan struct{}) {
	t.Helper()
	server, addr, serveDone := startTestServer(t, func() *Server {
		s := NewServer(Options{Handler: handler})
		s.Options.Pollers = 1
		s.Options.ReadBufferSize = 4 << 10
		if configure != nil {
			configure(s)
		}
		return s
	})
	done := make(chan struct{})
	go func() {
		_ = <-serveDone
		close(done)
	}()
	t.Cleanup(func() {
		_ = server.Close(nil)
		select {
		case <-done:
		case <-time.After(lifecycleTestTimeout()):
			t.Error("server did not stop")
		}
	})
	return server, addr, done
}

func TestServerCloseReturnsFromCheckOrigin(t *testing.T) {
	closeResult := make(chan error, 1)
	_, addr, serveDone := startConfiguredLifecycleTestServer(t, nil, func(server *Server) {
		server.CheckOrigin = func(*http.Request) bool {
			closeResult <- server.Close(errors.New("close from CheckOrigin"))
			return true
		}
	})
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	request := "GET / HTTP/1.1\r\n" + "Host: " + addr + "\r\n" + "Upgrade: websocket\r\n" + "Connection: Upgrade\r\n" + "Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n" + "Sec-WebSocket-Version: 13\r\n\r\n"
	if _, err = conn.Write([]byte(request)); err != nil {
		t.Fatal(err)
	}
	select {
	case err = <-closeResult:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(lifecycleTestTimeout()):
		t.Fatal("Server.Close blocked in CheckOrigin")
	}
	select {
	case <-serveDone:
	case <-time.After(lifecycleTestTimeout()):
		t.Fatal("Serve did not stop after CheckOrigin closed the server")
	}
}

func TestIncrementalParserReturnsToPoolAfterProtocolError(t *testing.T) {
	conn := &Conn{config: testServerConfig(NewServer(Options{}))}
	if _, err := conn.feedFrames([]byte{0x82}, func(frame.Frame) error {
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if conn.parser == nil {
		t.Fatal("partial header did not retain parser state")
	}
	if _, err := conn.feedFrames([]byte{0x00}, func(frame.Frame) error {
		return nil
	}); !errors.Is(err, frame.ErrProtocol) {
		t.Fatalf("unmasked frame error = %v, want %v", err, frame.ErrProtocol)
	}
	if conn.parser != nil {
		t.Fatal("protocol error retained incremental parser state")
	}
}

func TestReadAvailableParsesHandshakeAcrossInboundSegments(t *testing.T) {
	request := []byte("GET /chat HTTP/1.1\r\n" + "Host: example.com\r\n" + "Connection: Upgrade\r\n" + "Upgrade: websocket\r\n" + "Sec-WebSocket-Version: 13\r\n" + "Sec-WebSocket-Key: " + testKey + "\r\n\r\n")
	raw := newSegmentedProbeConn(request[:1], request[1:17], request[17:])
	handler := &recordingHandler{}
	server := NewServer(Options{Handler: handler})
	conn := &Conn{raw: raw, handler: handler, config: testServerConfig(server)}
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
		wire = frame.Append(wire, frame.Frame{Fin: true, Opcode: frame.Binary, Masked: true, Payload: []byte{byte(i)}}, [4]byte{1, 2, 3, 4})
	}
	segments := make([][]byte, len(wire))
	for index := range wire {
		segments[index] = wire[index : index+1]
	}
	raw := newSegmentedProbeConn(segments...)
	handler := &recordingHandler{}
	conn := &Conn{raw: raw, handler: handler, config: testServerConfig(NewServer(Options{}))}
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
		wire = frame.Append(wire, frame.Frame{Fin: true, Opcode: frame.Binary, Masked: true, Payload: []byte{byte(i)}}, [4]byte{1, 2, 3, 4})
	}
	wakeErr := errors.New("wake failed")
	raw := newSegmentedProbeConn(wire)
	raw.wakeErr = wakeErr
	conn := &Conn{raw: raw, handler: &recordingHandler{}, config: testServerConfig(NewServer(Options{}))}
	conn.opened.Store(true)
	if err := conn.readAvailable(); !errors.Is(err, wakeErr) {
		t.Fatalf("read error = %v, want wrapped %v", err, wakeErr)
	}
	if raw.wakes != 1 {
		t.Fatalf("wake calls = %d, want 1", raw.wakes)
	}
}

func TestClosedConnectionDoesNotCreateCloseTimer(t *testing.T) {
	conn := &Conn{raw: &writeProbeConn{}}
	conn.closed.Store(true)
	conn.startCloseTimer()
	conn.ensureCloseTimer()
	if metadata := conn.metadata.Load(); metadata != nil && metadata.closeTimer != nil {
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

func TestClientHandshakeAcceptsSplitResponse(t *testing.T) {
	response := []byte("HTTP/1.1 101 Switching Protocols\r\n" + "Upgrade: websocket\r\nConnection: Upgrade\r\n" + "Sec-WebSocket-Accept: s3pPLMBiTxaQ9kYGzzhZRbK+xOo=\r\n" + "Sec-WebSocket-Protocol: chat\r\n\r\n")
	dialer := &Dialer{Options: Options{MaxHeaderBytes: DefaultMaxHeaderBytes, Subprotocols: []string{"chat"}}}
	conn := &Conn{raw: &writeProbeConn{}, config: testDialerConfig(dialer)}
	conn.handshake.Store(&handshakeState{clientKey: testKey})
	split := len(response) / 2
	if _, err := conn.consumeHandshake(response[:split]); err != nil {
		t.Fatalf("first handshake fragment = %v", err)
	}
	if conn.opened.Load() {
		t.Fatal("connection opened before complete handshake")
	}
	if _, err := conn.consumeHandshake(response[split:]); err != nil {
		t.Fatalf("second handshake fragment = %v", err)
	}
	if !conn.opened.Load() {
		t.Fatal("connection did not open after complete handshake")
	}
	if protocol := conn.Subprotocol(); protocol != "chat" {
		t.Fatalf("negotiated subprotocol = %q, want chat", protocol)
	}
}

var textPaths = []string{"frame", "split", "fragments", "compressed"}

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
	raw.out = make([]byte, 0, 64)
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

func TestTextValidationAgreesOnEveryPath(t *testing.T) {
	sizes := []int{1, 2, 3, 4, 31, 32, 33, 34, 63, 64, 65, 95, 96, 97, 125, 126, 127, 128, 129, 1000, 1024, 4097, 65535, 70000}
	bad := [][]byte{{0xff}, {0x80}, {0xc0, 0xaf}, {0xe0, 0x80, 0xaf}, {0xed, 0xa0, 0x80}, {0xf4, 0x90, 0x80, 0x80}, {0xe4, 0xb8}, {0xf0, 0x9f, 0x98}}
	for _, size := range sizes {
		for _, multibyte := range []bool{true, false} {
			text := textOfLength(size, multibyte)
			checkTextDelivery(t, text, fmt.Sprintf("text of %d bytes, multibyte %v", size, multibyte))
			if multibyte {
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
		prefix := bytes.Repeat([]byte("中"), int(lead%64))
		if lead >= 128 {
			prefix = bytes.Repeat([]byte("a"), int(lead%128))
		}
		checkTextDelivery(t, append(prefix, p...), "fuzzed")
	})
}

type peerCloseBehindHandler struct{ closed chan CloseEvent }

func (*peerCloseBehindHandler) OnOpen(*Conn) {
}

func (h *peerCloseBehindHandler) OnMessage(c *Conn, m Message) {
	if string(m.Payload) == "first" {
		_ = c.Close(1000, "bye")
	}
}

func (h *peerCloseBehindHandler) OnClose(_ *Conn, info CloseEvent) {
	h.closed <- info
}

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

func TestPeerCloseBehindDataCompletesHandshake(t *testing.T) {
	peerCloseBehindRun(t, frame.Frame{Fin: true, Opcode: frame.Text, Masked: true, Payload: []byte("second")}, 2*time.Second)
}

func TestPeerCloseBehindPingCompletesHandshake(t *testing.T) {
	peerCloseBehindRun(t, frame.Frame{Fin: true, Opcode: frame.Ping, Masked: true, Payload: []byte("p")}, 2*time.Second)
}

const wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

func startRawWSServer(t *testing.T, handle func(conn net.Conn, br *bufio.Reader)) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = ln.Close()
	})
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				br := bufio.NewReader(conn)
				key := ""
				for {
					line, err := br.ReadString('\n')
					if err != nil {
						return
					}
					lower := strings.ToLower(line)
					if strings.HasPrefix(lower, "sec-websocket-key:") {
						key = strings.TrimSpace(line[len("sec-websocket-key:"):])
					}
					if line == "\r\n" {
						break
					}
				}
				sum := sha1.Sum([]byte(key + wsGUID))
				accept := base64.StdEncoding.EncodeToString(sum[:])
				if _, err := fmt.Fprintf(conn, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n", accept); err != nil {
					return
				}
				handle(conn, br)
			}()
		}
	}()
	return ln.Addr().String()
}

func writeServerFrame(t *testing.T, conn net.Conn, opcode frame.OpCode, fin bool, payload []byte) {
	t.Helper()
	wire := frame.Append(nil, frame.Frame{Fin: fin, Opcode: opcode, Payload: payload}, [4]byte{})
	if _, err := conn.Write(wire); err != nil {
		t.Fatalf("server frame: %v", err)
	}
}

func readClientFrame(br *bufio.Reader) (frame.OpCode, []byte, error) {
	var header [2]byte
	if _, err := io.ReadFull(br, header[:]); err != nil {
		return 0, nil, err
	}
	if header[1]&0x80 == 0 {
		return 0, nil, errors.New("client frame is not masked")
	}
	length := int(header[1] & 0x7f)
	switch length {
	case 126:
		var ext [2]byte
		if _, err := io.ReadFull(br, ext[:]); err != nil {
			return 0, nil, err
		}
		length = int(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err := io.ReadFull(br, ext[:]); err != nil {
			return 0, nil, err
		}
		length = int(binary.BigEndian.Uint64(ext[:]))
	}
	var key [4]byte
	if _, err := io.ReadFull(br, key[:]); err != nil {
		return 0, nil, err
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(br, payload); err != nil {
		return 0, nil, err
	}
	for i := range payload {
		payload[i] ^= key[i&3]
	}
	return frame.OpCode(header[0] & 0x0f), payload, nil
}

type clientEvents struct {
	opened   chan struct{}
	closed   chan struct{}
	onceOpen sync.Once
	onceShut sync.Once
	mu       sync.Mutex
	msgs     []string
	types    []MessageType
	closeErr error
	conn     *Conn
}

func newClientEvents() *clientEvents {
	return &clientEvents{opened: make(chan struct{}), closed: make(chan struct{})}
}

func (e *clientEvents) OnOpen(c *Conn) {
	e.mu.Lock()
	e.conn = c
	e.mu.Unlock()
	e.onceOpen.Do(func() {
		close(e.opened)
	})
}

func (e *clientEvents) OnMessage(_ *Conn, m Message) {
	e.mu.Lock()
	e.msgs = append(e.msgs, string(m.Payload))
	e.types = append(e.types, m.Type)
	e.mu.Unlock()
}

func (e *clientEvents) OnClose(_ *Conn, ev CloseEvent) {
	e.mu.Lock()
	e.closeErr = ev.Err
	e.mu.Unlock()
	e.onceShut.Do(func() {
		close(e.closed)
	})
}

func (e *clientEvents) wait(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(testIOTimeout()):
		t.Fatalf("timed out waiting for %s", what)
	}
}

func TestRobustClientRejectsOversizedMessage(t *testing.T) {
	gotClose := make(chan int, 1)
	addr := startRawWSServer(t, func(conn net.Conn, br *bufio.Reader) {
		writeServerFrame(t, conn, frame.Text, true, bytes.Repeat([]byte("x"), 64))
		_ = conn.SetReadDeadline(time.Now().Add(testIOTimeout()))
		for {
			opcode, payload, err := readClientFrame(br)
			if err != nil {
				gotClose <- -1
				return
			}
			if opcode == frame.Close {
				code := 0
				if len(payload) >= 2 {
					code = int(binary.BigEndian.Uint16(payload))
				}
				gotClose <- code
				return
			}
		}
	})
	events := newClientEvents()
	dialer := NewDialer(Options{Handler: events, MaxMessageSize: 32})
	ctx, cancel := context.WithTimeout(context.Background(), testIOTimeout())
	defer cancel()
	if err := dialer.Dial(ctx, "ws://"+addr+"/", nil); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = dialer.Close(nil)
	}()
	select {
	case code := <-gotClose:
		if code != 1009 {
			t.Fatalf("client close code = %d, want 1009", code)
		}
	case <-time.After(testIOTimeout()):
		t.Fatal("client never closed the oversized connection")
	}
}

func TestRobustConcurrentClose(t *testing.T) {
	raw := &limitedWire{limit: 1 << 20}
	conn := newLimitedConn(raw, time.Hour)
	t.Cleanup(conn.stopCloseTimer)
	const closers = 8
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < closers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_ = conn.Close(1000, "many")
		}()
	}
	close(start)
	wg.Wait()
	frames := raw.frames(t)
	if len(frames) != 1 || frames[0].Opcode != frame.Close {
		t.Fatalf("wire = %v, want one Close frame", frames)
	}
}

type robustEchoHandler struct{}

func (robustEchoHandler) OnOpen(*Conn) {
}

func (robustEchoHandler) OnMessage(c *Conn, m Message) {
	if m.Type == TextMessage {
		_ = c.SendText(m.Text())
	} else {
		_ = c.SendBinary(m.Bytes())
	}
}

func (robustEchoHandler) OnClose(*Conn, CloseEvent) {
}

func startRobustServer(t *testing.T, opts Options, handler Handler) string {
	t.Helper()
	server, addr, serveDone := startTestServer(t, func() *Server {
		opts.Handler = handler
		return NewServer(opts)
	})
	t.Cleanup(func() {
		stopTestServer(t, server, serveDone)
	})
	return addr
}

func robustDial(t *testing.T, addr string) (net.Conn, *bufio.Reader) {
	t.Helper()
	return dialTestWS(t, addr)
}

func writeDataFrame(t *testing.T, conn net.Conn, opcode frame.OpCode, fin bool, payload []byte) {
	t.Helper()
	wire := frame.Append(nil, frame.Frame{Fin: fin, Opcode: opcode, Masked: true, Payload: payload}, [4]byte{9, 8, 7, 6})
	if _, err := conn.Write(wire); err != nil {
		t.Fatalf("write frame: %v", err)
	}
}

func awaitClose(t *testing.T, reader *bufio.Reader) int {
	t.Helper()
	for {
		opcode, payload, err := readFrameWithOpcode(reader)
		if err != nil {
			if err == io.EOF {
				return -1
			}
			return -1
		}
		if opcode == frame.Close {
			if len(payload) < 2 {
				return 0
			}
			return int(binary.BigEndian.Uint16(payload))
		}
	}
}

func TestRobustEmptyMessages(t *testing.T) {
	addr := startRobustServer(t, Options{}, robustEchoHandler{})
	conn, reader := robustDial(t, addr)
	writeDataFrame(t, conn, frame.Text, true, nil)
	if opcode, payload, err := readFrameWithOpcode(reader); err != nil || opcode != frame.Text || len(payload) != 0 {
		t.Fatalf("empty text echo = opcode %d len %d err %v", opcode, len(payload), err)
	}
	writeDataFrame(t, conn, frame.Binary, true, []byte{})
	if opcode, payload, err := readFrameWithOpcode(reader); err != nil || opcode != frame.Binary || len(payload) != 0 {
		t.Fatalf("empty binary echo = opcode %d len %d err %v", opcode, len(payload), err)
	}
}

func TestRobustFrameLengthBoundaries(t *testing.T) {
	addr := startRobustServer(t, Options{}, robustEchoHandler{})
	conn, reader := robustDial(t, addr)
	if err := conn.SetReadDeadline(time.Now().Add(testIOTimeout())); err != nil {
		t.Fatal(err)
	}
	for _, size := range []int{1, 125, 126, 127, 65535, 65536} {
		payload := bytes.Repeat([]byte{byte(size)}, size)
		writeDataFrame(t, conn, frame.Binary, true, payload)
		opcode, got, err := readFrameWithOpcode(reader)
		if err != nil {
			t.Fatalf("size %d: %v", size, err)
		}
		if opcode != frame.Binary || !bytes.Equal(got, payload) {
			t.Fatalf("size %d echoed as opcode %d len %d", size, opcode, len(got))
		}
	}
}

func TestRobustContinuationWithoutStartCloses(t *testing.T) {
	addr := startRobustServer(t, Options{}, robustEchoHandler{})
	conn, reader := robustDial(t, addr)
	if err := conn.SetReadDeadline(time.Now().Add(testIOTimeout())); err != nil {
		t.Fatal(err)
	}
	writeDataFrame(t, conn, frame.Continuation, true, []byte("orphan"))
	if code := awaitClose(t, reader); code != 1002 {
		t.Fatalf("orphan continuation close code = %d, want 1002", code)
	}
}

func TestRobustUnsolicitedPongIgnored(t *testing.T) {
	addr := startRobustServer(t, Options{}, robustEchoHandler{})
	conn, reader := robustDial(t, addr)
	if err := conn.SetReadDeadline(time.Now().Add(testIOTimeout())); err != nil {
		t.Fatal(err)
	}
	writeDataFrame(t, conn, frame.Pong, true, []byte("nobody asked"))
	writeDataFrame(t, conn, frame.Text, true, []byte("after"))
	opcode, payload, err := readFrameWithOpcode(reader)
	if err != nil || opcode != frame.Text || string(payload) != "after" {
		t.Fatalf("echo after unsolicited pong = opcode %d payload %q err %v", opcode, payload, err)
	}
}

func robustTCP(t *testing.T, addr string) net.Conn {
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
	return conn
}

func rawHandshake(t *testing.T, addr, request string) (net.Conn, *bufio.Reader, string, []string) {
	t.Helper()
	conn := robustTCP(t, addr)
	if _, err := conn.Write([]byte(request)); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(conn)
	if err := conn.SetReadDeadline(time.Now().Add(testIOTimeout())); err != nil {
		t.Fatal(err)
	}
	status, err := reader.ReadString('\n')
	if err != nil {
		return conn, reader, "", nil
	}
	var headers []string
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return conn, reader, status, headers
		}
		if line == "\r\n" {
			return conn, reader, status, headers
		}
		headers = append(headers, line)
	}
}

func upgradeRequest(addr string, extra ...string) string {
	lines := []string{"GET / HTTP/1.1", "Host: " + addr, "Connection: Upgrade", "Upgrade: websocket", "Sec-WebSocket-Version: 13", "Sec-WebSocket-Key: " + testKey}
	lines = append(lines, extra...)
	return strings.Join(lines, "\r\n") + "\r\n\r\n"
}

func TestRobustHandshakeOversizedHeaders(t *testing.T) {
	addr := startRobustServer(t, Options{MaxHeaderBytes: 256}, robustEchoHandler{})
	big := upgradeRequest(addr, "X-Padding: "+strings.Repeat("p", 1024))
	_, _, status, _ := rawHandshake(t, addr, big)
	if strings.Contains(status, "101") {
		t.Fatalf("oversized headers upgraded: %q", status)
	}
	_, _, status, _ = rawHandshake(t, addr, upgradeRequest(addr))
	if !strings.Contains(status, "101") {
		t.Fatalf("server stopped serving after oversized headers: %q", status)
	}
}

func TestTransportBackpressureMapsToUWSError(t *testing.T) {
	raw := newScriptedConn()
	raw.writeErr = uio.ErrOutboundOverflow
	server := &Server{Options: Options{MaxFramePayload: 1024, MaxMessageSize: 1024}}
	conn := &Conn{raw: raw, config: testServerConfig(server)}
	conn.opened.Store(true)
	payload := bytes.Repeat([]byte("x"), 50)
	if err := conn.SendBinary(payload); !errors.Is(err, ErrBackpressure) {
		t.Fatalf("SendBinary() error = %v, want %v", err, ErrBackpressure)
	}
	if pending := int64(conn.OutboundBuffered()); pending != 0 {
		t.Fatalf("pending bytes = %d, want 0", pending)
	}
	raw.writeErr = nil
	if err := conn.SendBinary(payload); err != nil {
		t.Fatalf("SendBinary() after recovery = %v", err)
	}
}

func TestServerFrameCoalescesOnlyBelowWriteBufferThreshold(t *testing.T) {
	raw := &writeProbeConn{}
	server := &Server{Options: Options{WriteCoalesce: 64, MaxFramePayload: 1024, MaxMessageSize: 1024}}
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
	disabled := &Conn{raw: disabledRaw, config: testServerConfig(&Server{Options: Options{WriteCoalesce: -1, MaxFramePayload: 1024, MaxMessageSize: 1024}})}
	disabled.opened.Store(true)
	if err := disabled.SendBinary(make([]byte, 32)); err != nil {
		t.Fatal(err)
	}
	if disabledRaw.writes != 1 || disabledRaw.writevs != 1 {
		t.Fatalf("disabled coalescing transport calls = Write:%d Writev:%d, want 1/1", disabledRaw.writes, disabledRaw.writevs)
	}
}

func TestSendValidatesTextAndMessageLimit(t *testing.T) {
	raw := &writeProbeConn{}
	server := &Server{Options: Options{MaxFramePayload: 1024, MaxMessageSize: 4}}
	conn := &Conn{raw: raw, config: testServerConfig(server)}
	conn.opened.Store(true)
	if err := conn.SendText(string([]byte{0xff})); err != nil {
		t.Fatalf("SendText(unvalidated byte) = %v, want nil", err)
	}
	if err := conn.SendBinary([]byte("12345")); err != frame.ErrMessageTooBig {
		t.Fatalf("SendBinary(oversized) = %v, want %v", err, frame.ErrMessageTooBig)
	}
	if raw.writes != 1 {
		t.Fatalf("sends wrote %d frames, want the one valid text frame", raw.writes)
	}
}

func TestFragmentedMessageAdmittedWholeOrNotAtAll(t *testing.T) {
	raw := &limitedWire{limit: 20}
	server := NewServer(Options{})
	server.Options.MaxOutboundBuffered = raw.limit
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
	conn := &Conn{raw: raw, config: testServerConfig(&Server{Options: Options{MaxFramePayload: 4, MaxMessageSize: 1024}}), compression: &compressionState{encoder: compress.NewEncoder(-1, true)}}
	conn.opened.Store(true)
	payload := bytes.Repeat([]byte("abcdefgh"), 64)
	if err := conn.SendBinary(payload); !errors.Is(err, writeErr) {
		t.Fatalf("SendBinary = %v, want %v", err, writeErr)
	}
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

func TestShortPathSixteenBitLengthHonorsFrameLimit(t *testing.T) {
	server := NewServer(Options{Handler: echoPathHandler{}})
	server.config = testServerConfig(server)
	server.config.parser.MaxFramePayload = 2048
	raw := &echoPathConn{}
	conn := server.newConnection(raw)
	conn.opened.Store(true)
	raw.userdata = conn
	t.Cleanup(func() {
		server.onClose(raw, io.EOF)
	})
	payload := bytes.Repeat([]byte{'q'}, 4096)
	raw.inbound = frame.Append(nil, frame.Frame{Fin: true, Opcode: frame.Binary, Masked: true, Payload: payload}, [4]byte{1, 2, 3, 4})
	raw.out = make([]byte, 0, 4096)
	if err := server.onData(raw); err != nil {
		return
	}
	if !conn.closing.Load() {
		t.Fatal("a frame over MaxFramePayload was accepted")
	}
	if len(raw.out) != 4 {
		t.Fatalf("a refused frame produced %d reply bytes, want one close frame", len(raw.out))
	}
	if len(raw.inbound) == 0 {
		t.Fatal("the refused frame was consumed")
	}
}

func (c *echoPathConn) Writev(buffers [][]byte) (int, error) {
	n := 0
	for _, b := range buffers {
		c.out = append(c.out, b...)
		n += len(b)
	}
	return n, nil
}

type messageLog struct{ messages *[]Message }

func (messageLog) OnOpen(*Conn) {
}

func (h messageLog) OnMessage(_ *Conn, message Message) {
	*h.messages = append(*h.messages, Message{Type: message.Type, Payload: append([]byte(nil), message.Payload...)})
}

func (messageLog) OnClose(*Conn, CloseEvent) {
}

func TestBatchedRepliesEncodeEveryLengthAsTheGenericEncoder(t *testing.T) {
	sizes := []int{0, 1, 125, 126, 127, 4096, 65535, 65536, 70000}
	server := NewServer(Options{Handler: echoPathHandler{}})
	server.config = testServerConfig(server)
	server.config.writeBufferedThreshold = 1 << 20
	server.config.maxOutbound = 0
	raw := &echoPathConn{}
	conn := server.newConnection(raw)
	conn.opened.Store(true)
	raw.userdata = conn
	var wire, want []byte
	for i, size := range sizes {
		payload := bytes.Repeat([]byte{byte(i + 1)}, size)
		wire = frame.Append(wire, frame.Frame{Fin: true, Opcode: frame.Binary, Masked: true, Payload: payload}, [4]byte{3, 1, 4, 1})
		want = frame.Append(want, frame.Frame{Fin: true, Opcode: frame.Binary, Payload: payload}, [4]byte{})
	}
	raw.inbound = wire
	raw.out = make([]byte, 0, 2*len(want))
	captured := &batchCaptureConn{echoPathConn: raw}
	conn.raw = captured
	if err := server.onData(captured); err != nil {
		t.Fatalf("read round failed: %v", err)
	}
	if !bytes.Equal(captured.written, want) {
		t.Fatalf("batched replies: %d bytes, want %d", len(captured.written), len(want))
	}
}

type batchCaptureConn struct {
	*echoPathConn
	written []byte
}

func (c *batchCaptureConn) WriteOwned(buffer *uio.Buffer) (int, error) {
	n := buffer.Len()
	c.written = append(c.written, buffer.Bytes()...)
	uio.ReleaseBuffer(buffer)
	return n, nil
}

func textOfLength(size int, multibyte bool) []byte {
	if !multibyte {
		return bytes.Repeat([]byte("{\"k\":1}"), size/7+1)[:size]
	}
	out := make([]byte, 0, size)
	out = append(out, "abc"[:min(3, size)]...)
	runes := []string{"é", "€", "😀"}
	for i := 0; len(out) < size; i++ {
		r := runes[i%len(runes)]
		if len(out)+len(r) > size {
			out = append(out, bytes.Repeat([]byte{'x'}, size-len(out))...)
			break
		}
		out = append(out, r...)
	}
	return out
}
