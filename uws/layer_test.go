package uws

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"github.com/urpc/uio"
	"github.com/urpc/uio/utls"
	"github.com/urpc/uio/uws/internal/frame"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

type streamPeer struct {
	echo   bool
	opened chan uio.Conn
	closed chan error
	mu     sync.Mutex
	read   bytes.Buffer
	more   chan struct{}
}

func newStreamPeer(echo bool) *streamPeer {
	return &streamPeer{echo: echo, opened: make(chan uio.Conn, 4), closed: make(chan error, 4), more: make(chan struct{}, 1)}
}

func (p *streamPeer) OnOpen(c uio.Conn) {
	p.opened <- c
}

func (p *streamPeer) OnRead(c uio.Conn) error {
	chunk := c.PeekChunk()
	if len(chunk) == 0 {
		return nil
	}
	if p.echo {
		if _, err := c.Write(chunk); err != nil {
			return err
		}
	}
	p.mu.Lock()
	p.read.Write(chunk)
	p.mu.Unlock()
	_, _ = c.Discard(len(chunk))
	select {
	case p.more <- struct{}{}:
	default:
	}
	return nil
}

func (p *streamPeer) OnTimer(uio.Conn) error {
	return nil
}

func (p *streamPeer) OnClose(_ uio.Conn, err error) {
	p.closed <- err
}

func (p *streamPeer) waitRead(t *testing.T, n int) []byte {
	t.Helper()
	deadline := time.After(testIOTimeout())
	for {
		p.mu.Lock()
		if p.read.Len() >= n {
			got := append([]byte(nil), p.read.Bytes()...)
			p.mu.Unlock()
			return got
		}
		have := p.read.Len()
		p.mu.Unlock()
		select {
		case <-p.more:
		case <-deadline:
			t.Fatalf("read %d of %d bytes", have, n)
		}
	}
}

type layeredEvents struct {
	*uio.Events
	layers []uio.Layer
}

func (e *layeredEvents) Listen(addr string, opts ...uio.ListenOption) (uio.Listener, error) {
	if len(e.layers) > 0 {
		opts = append([]uio.ListenOption{uio.WithLayers(e.layers...)}, opts...)
	}
	return e.Events.Listen(addr, opts...)
}

func (e *layeredEvents) Dial(addr string, opts ...uio.DialOption) (uio.Conn, error) {
	return e.DialContext(context.Background(), addr, opts...)
}

func (e *layeredEvents) DialContext(ctx context.Context, addr string, opts ...uio.DialOption) (uio.Conn, error) {
	if len(e.layers) > 0 {
		opts = append([]uio.DialOption{uio.WithLayers(e.layers...)}, opts...)
	}
	return e.Events.DialContext(ctx, addr, opts...)
}

func targetOf(opts LayerOptions, target string) LayerOptions {
	opts.Target = target
	return opts
}

func layerEvents(t *testing.T, handler uio.Handler, listen bool, layers ...uio.Layer) (*layeredEvents, string) {
	t.Helper()
	started := make(chan struct{})
	events := &uio.Events{Pollers: 1, Handler: handler, Hooks: uio.FuncHandler{Start: func(*uio.Events) {
		close(started)
	}}}
	layered := &layeredEvents{Events: events, layers: layers}
	done := make(chan error, 1)
	go func() {
		done <- events.Serve()
	}()
	t.Cleanup(func() {
		_ = events.Close(nil)
		<-done
	})
	select {
	case <-started:
	case <-time.After(testIOTimeout()):
		t.Fatal("Serve did not start")
	}
	if !listen {
		return layered, ""
	}
	var listener uio.Listener
	var err error
	if len(layers) > 0 {
		listener, err = events.Listen("tcp://127.0.0.1:0", uio.WithLayers(layers...))
	} else {
		listener, err = events.Listen("tcp://127.0.0.1:0")
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = listener.Close()
	})
	return layered, listener.Addr().String()
}

func dialLayer(t *testing.T, events *layeredEvents, addr string) uio.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testIOTimeout())
	defer cancel()
	conn, err := events.DialContext(ctx, "tcp://"+addr)
	if err != nil {
		t.Fatal(err)
	}
	return conn
}

func TestClientLayerTalksToAServer(t *testing.T) {
	addr := startEchoServer(t, robustEchoHandler{})
	peer := newStreamPeer(false)
	client, _ := layerEvents(t, peer, false, (ClientLayer(LayerOptions{Target: "ws://" + addr + "/"})))
	conn := dialLayer(t, client, addr)
	if _, err := conn.Write([]byte("through the layer")); err != nil {
		t.Fatal(err)
	}
	if got := string(peer.waitRead(t, len("through the layer"))); got != "through the layer" {
		t.Fatalf("echo = %q", got)
	}
}

type xorLayer struct{ uio.PassThrough }

func (xorLayer) OnRead(ctx uio.LayerContext) error {
	lower := ctx.Lower()
	chunk := lower.PeekChunk()
	if len(chunk) == 0 {
		return nil
	}
	out := ctx.Upper().Reserve(len(chunk))[:len(chunk)]
	for i, b := range chunk {
		out[i] = b ^ 0x5a
	}
	ctx.Upper().Commit(len(chunk))
	_, err := lower.Discard(len(chunk))
	return err
}

func (xorLayer) OnWrite(ctx uio.LayerContext, p []byte) (int, error) {
	out := make([]byte, len(p))
	for i, b := range p {
		out[i] = b ^ 0x5a
	}
	if _, err := ctx.Lower().Write(out); err != nil {
		return 0, err
	}
	return len(p), nil
}

func xorLayerOf() uio.Layer {
	return uio.LayerFunc(func(uio.LayerContext) uio.LayerHandler {
		return xorLayer{}
	})
}

func TestWebSocketLayerInAStack(t *testing.T) {
	serverTLS, clientTLS := wssConfigs(t)
	echo := newStreamPeer(true)
	_, addr := layerEvents(t, echo, true, utls.ServerLayer(utls.LayerOptions{TLSConfig: serverTLS}), (ServerLayer(LayerOptions{})), xorLayerOf())
	peer := newStreamPeer(false)
	client, _ := layerEvents(t, peer, false, utls.ClientLayer(utls.LayerOptions{TLSConfig: clientTLS}), (ClientLayer(LayerOptions{Target: "wss://" + addr + "/"})), xorLayerOf())
	conn := dialLayer(t, client, addr)
	payload := make([]byte, 1<<20)
	for i := range payload {
		payload[i] = byte(i * 7)
	}
	for sent := 0; sent < len(payload); sent += 32 << 10 {
		if _, err := conn.Write(payload[sent : sent+32<<10]); err != nil {
			t.Fatal(err)
		}
	}
	if got := peer.waitRead(t, len(payload)); !bytes.Equal(got, payload) {
		t.Fatal("the echo came back changed")
	}
}

func TestLayerHandshakeRefusal(t *testing.T) {
	server := newStreamPeer(false)
	_, addr := layerEvents(t, server, true, (ServerLayer(LayerOptions{Authorize: func(*http.Request) (any, error) {
		return nil, &RejectError{Status: http.StatusUnauthorized, Reason: "no token"}
	}})))
	peer := newStreamPeer(false)
	client, _ := layerEvents(t, peer, false, (ClientLayer(LayerOptions{Target: "ws://" + addr + "/"})))
	ctx, cancel := context.WithTimeout(context.Background(), testIOTimeout())
	defer cancel()
	_, err := client.DialContext(ctx, "tcp://"+addr)
	var refused *HandshakeError
	if !errors.As(err, &refused) || refused.StatusCode != http.StatusUnauthorized {
		t.Fatalf("DialContext = %v, want a 401 HandshakeError", err)
	}
	select {
	case <-server.opened:
		t.Fatal("the server's Handler opened a refused connection")
	case <-peer.opened:
		t.Fatal("the client's Handler opened a refused connection")
	case <-time.After(50 * time.Millisecond):
	}
}

func TestServerLayerUserdataAndSubprotocol(t *testing.T) {
	server := newStreamPeer(false)
	_, addr := layerEvents(t, server, true, (ServerLayer(LayerOptions{Options: Options{Subprotocols: []string{"chat"}}, Authorize: func(r *http.Request) (any, error) {
		return "user:" + r.URL.Path, nil
	}})))
	peer := newStreamPeer(false)
	client, _ := layerEvents(t, peer, false, (ClientLayer(LayerOptions{Target: "ws://" + addr + "/room", Options: Options{Subprotocols: []string{"chat"}}})))
	conn := dialLayer(t, client, addr)
	accepted := wssWait(t, server.opened, "the server's open")
	if got, ok := uio.FindLayer[interface{ Userdata() any }](accepted); !ok || got.Userdata() != "user:/room" {
		t.Fatalf("Authorize's userdata through the layer = %v, want %q", got, "user:/room")
	}
	if got := accepted.Userdata(); got != nil {
		t.Fatalf("the connection's UIO userdata = %v, want the application's own (nil)", got)
	}
	for _, c := range []uio.Conn{conn, accepted} {
		ws, ok := uio.LayerOf(c).(interface{ Subprotocol() string })
		if !ok || ws.Subprotocol() != "chat" {
			t.Fatalf("subprotocol through LayerOf = %v", ws)
		}
	}
}

func TestLayerHeartbeat(t *testing.T) {
	heartbeat := LayerOptions{Options: Options{HeartbeatInterval: 20 * time.Millisecond, HeartbeatTimeout: 400 * time.Millisecond}}
	t.Run("silent peer", func(t *testing.T) {
		addr := silentWebSocketPeer(t)
		peer := newStreamPeer(false)
		client, _ := layerEvents(t, peer, false, (ClientLayer(targetOf(heartbeat, "ws://"+addr+"/"))))
		dialLayer(t, client, addr)
		wssWait(t, peer.closed, "the heartbeat close")
	})
	t.Run("answering peer", func(t *testing.T) {
		addr := startEchoServer(t, robustEchoHandler{})
		peer := newStreamPeer(false)
		client, _ := layerEvents(t, peer, false, (ClientLayer(targetOf(heartbeat, "ws://"+addr+"/"))))
		dialLayer(t, client, addr)
		select {
		case err := <-peer.closed:
			t.Fatalf("a peer answering Pings was closed: %v", err)
		case <-time.After(3 * heartbeat.HeartbeatTimeout):
		}
	})
}

func silentWebSocketPeer(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = listener.Close()
	})
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		reader := bufio.NewReader(conn)
		key := ""
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				return
			}
			trimmed := strings.TrimSpace(line)
			if len(trimmed) > len("sec-websocket-key:") && strings.EqualFold(trimmed[:len("sec-websocket-key:")], "sec-websocket-key:") {
				key = strings.TrimSpace(trimmed[len("sec-websocket-key:"):])
			}
			if line == "\r\n" {
				break
			}
		}
		_, _ = fmt.Fprintf(conn, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n", testAcceptKey(key))
		_, _ = io.Copy(io.Discard, reader)
	}()
	return listener.Addr().String()
}

func TestLayerOptionsRefuseTransportSettings(t *testing.T) {
	for name, opts := range map[string]LayerOptions{"Handler": {Options: Options{Handler: robustEchoHandler{}}}, "Pollers": {Options: Options{Pollers: 2}}, "TLSConfig": {Options: Options{TLSConfig: &tls.Config{}}}, "MaxOutboundBuffered": {Options: Options{MaxOutboundBuffered: 1 << 20}}} {
		if err := opts.Validate(); !errors.Is(err, ErrInvalidOptions) {
			t.Fatalf("Validate with %s = %v, want ErrInvalidOptions", name, err)
		}
	}
	if err := (LayerOptions{Target: "http://example.org/"}).Validate(); !errors.Is(err, ErrInvalidOptions) {
		t.Fatalf("Validate of an http URL = %v, want ErrInvalidOptions", err)
	}
	if _, err := (LayerOptions{Target: "ws://example.org/"}).resolveServer(); !errors.Is(err, ErrInvalidOptions) {
		t.Fatalf("ServerLayer with a Target = %v, want ErrInvalidOptions", err)
	}
	if _, _, _, err := (LayerOptions{}).resolveClient(); !errors.Is(err, ErrInvalidOptions) {
		t.Fatalf("ClientLayer without Target = %v, want ErrInvalidOptions", err)
	}
	addr := startEchoServer(t, robustEchoHandler{})
	peer := newStreamPeer(false)
	client, _ := layerEvents(t, peer, false, ClientLayer(LayerOptions{Target: "ws://" + addr + "/", Options: Options{Pollers: 2}}))
	ctx, cancel := context.WithTimeout(context.Background(), testIOTimeout())
	defer cancel()
	if _, err := client.DialContext(ctx, "tcp://"+addr); !errors.Is(err, ErrInvalidOptions) {
		t.Fatalf("a dial through a misconfigured layer = %v, want ErrInvalidOptions", err)
	}
}

func TestLayerSplitsWritesAtTheMessageLimit(t *testing.T) {
	sizes := make(chan int, 16)
	addr := startEchoServer(t, sizeRecorder{sizes})
	peer := newStreamPeer(false)
	client, _ := layerEvents(t, peer, false, ClientLayer(LayerOptions{Target: "ws://" + addr + "/", Options: Options{MaxMessageSize: 1000}}))
	conn := dialLayer(t, client, addr)
	if n, err := conn.Write(nil); n != 0 || err != nil {
		t.Fatalf("an empty write = %d, %v", n, err)
	}
	if n, err := conn.Write(make([]byte, 2500)); n != 2500 || err != nil {
		t.Fatalf("Write = %d, %v, want it all taken", n, err)
	}
	var got []int
	for range 3 {
		got = append(got, wssWait(t, sizes, "a message"))
	}
	if got[0] != 1000 || got[1] != 1000 || got[2] != 500 {
		t.Fatalf("message sizes %v, want 1000, 1000, 500", got)
	}
	select {
	case n := <-sizes:
		t.Fatalf("an extra message of %d bytes", n)
	case <-time.After(50 * time.Millisecond):
	}
}

type sizeRecorder struct{ sizes chan int }

func (sizeRecorder) OnOpen(*Conn) {
}

func (r sizeRecorder) OnMessage(_ *Conn, m Message) {
	r.sizes <- len(m.Payload)
}

func (sizeRecorder) OnClose(*Conn, CloseEvent) {
}

type doubling struct{ uio.PassThrough }

func (doubling) OnRead(ctx uio.LayerContext) error {
	lower := ctx.Lower()
	chunk := lower.PeekChunk()
	pairs := len(chunk) / 2
	if pairs == 0 {
		return nil
	}
	out := ctx.Upper().Reserve(pairs)[:pairs]
	for i := range out {
		out[i] = chunk[2*i]
	}
	ctx.Upper().Commit(pairs)
	_, err := lower.Discard(2 * pairs)
	return err
}

func (doubling) OnWrite(ctx uio.LayerContext, p []byte) (int, error) {
	out := make([]byte, 2*len(p))
	for i, b := range p {
		out[2*i], out[2*i+1] = b, b
	}
	if _, err := ctx.Lower().Write(out); err != nil {
		return 0, err
	}
	return len(p), nil
}

func TestLayerCloseAnswerKeepsTheBacklog(t *testing.T) {
	const messages, size = 64, 64 << 10
	writer := uio.FuncHandler{Open: func(c uio.Conn) {
		for range messages {
			_, _ = c.Write(make([]byte, size))
		}
	}, Read: func(c uio.Conn) error {
		_, _ = c.Discard(c.InboundBuffered())
		return nil
	}, Close: func(uio.Conn, error) {
	}}
	_, addr := layerEvents(t, writer, true, uio.LayerFunc(func(uio.LayerContext) uio.LayerHandler {
		return doubling{}
	}), ServerLayer(LayerOptions{}))
	peer, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	double := func(p []byte) []byte {
		out := make([]byte, 2*len(p))
		for i, b := range p {
			out[2*i], out[2*i+1] = b, b
		}
		return out
	}
	request := "GET / HTTP/1.1\r\nHost: x\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n" + "Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\n\r\n"
	if _, err := peer.Write(double([]byte(request))); err != nil {
		t.Fatal(err)
	}
	if _, err := peer.Write(double([]byte{0x88, 0x82, 0, 0, 0, 0, 0x03, 0xe8})); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	_ = peer.SetReadDeadline(time.Now().Add(testIOTimeout()))
	wire, err := io.ReadAll(peer)
	if err != nil {
		t.Fatal(err)
	}
	stream := make([]byte, len(wire)/2)
	for i := range stream {
		stream[i] = wire[2*i]
	}
	header := bytes.Index(stream, []byte("\r\n\r\n"))
	if header < 0 {
		t.Fatal("no upgrade response")
	}
	frames := stream[header+4:]
	want := messages*(size+10) + 4
	if len(frames) != want || !bytes.HasSuffix(frames, []byte{0x88, 0x02, 0x03, 0xe8}) {
		t.Fatalf("read %d bytes of frames, want %d ending in the Close answer", len(frames), want)
	}
}

func TestClientLayerOverAHoldingLayer(t *testing.T) {
	addr := startEchoServer(t, robustEchoHandler{})
	peer := newStreamPeer(false)
	client, _ := layerEvents(t, peer, false, uio.LayerFunc(func(uio.LayerContext) uio.LayerHandler {
		return &holdUntilFlush{}
	}), ClientLayer(LayerOptions{Target: "ws://" + addr + "/"}))
	conn := dialLayer(t, client, addr)
	if _, err := conn.Write([]byte("held, then flushed")); err != nil {
		t.Fatal(err)
	}
	_ = conn.Flush()
	if got := string(peer.waitRead(t, len("held, then flushed"))); got != "held, then flushed" {
		t.Fatalf("echo = %q", got)
	}
}

type holdUntilFlush struct {
	uio.PassThrough
	mu      sync.Mutex
	held    []byte
	writeMu sync.Mutex
}

func (h *holdUntilFlush) OnWrite(ctx uio.LayerContext, p []byte) (int, error) {
	h.mu.Lock()
	h.held = append(h.held, p...)
	h.mu.Unlock()
	_ = ctx.Wake()
	return len(p), nil
}

func (h *holdUntilFlush) OutboundHeld(uio.LayerContext) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.held)
}

func (h *holdUntilFlush) OnRead(ctx uio.LayerContext) error {
	if err := h.PassThrough.OnRead(ctx); err != nil {
		return err
	}
	return h.drain(ctx)
}

func (h *holdUntilFlush) OnClosing(ctx uio.LayerContext, _ error) {
	_ = h.drain(ctx)
}

func (h *holdUntilFlush) drain(ctx uio.LayerContext) error {
	h.writeMu.Lock()
	defer h.writeMu.Unlock()
	h.mu.Lock()
	held := h.held
	h.held = nil
	h.mu.Unlock()
	if len(held) == 0 {
		return nil
	}
	_, err := ctx.Lower().Write(held)
	return err
}

func TestLayerHeartbeatWhileTheLayerAboveHolds(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = listener.Close()
	})
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		reader := bufio.NewReader(conn)
		key := ""
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				return
			}
			trimmed := strings.TrimSpace(line)
			if len(trimmed) > len("sec-websocket-key:") && strings.EqualFold(trimmed[:len("sec-websocket-key:")], "sec-websocket-key:") {
				key = strings.TrimSpace(trimmed[len("sec-websocket-key:"):])
			}
			if line == "\r\n" {
				break
			}
		}
		_, _ = fmt.Fprintf(conn, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n", testAcceptKey(key))
		go func() {
			_, _ = io.Copy(io.Discard, reader)
		}()
		frame := append([]byte{0x82, 126, 0x40, 0x00}, make([]byte, 16<<10)...)
		for {
			if _, err := conn.Write(frame); err != nil {
				return
			}
		}
	}()
	closed := make(chan error, 1)
	idle := uio.FuncHandler{Read: func(uio.Conn) error {
		return nil
	}, Close: func(_ uio.Conn, err error) {
		signal(closed, err)
	}}
	heartbeat := LayerOptions{Options: Options{HeartbeatInterval: 20 * time.Millisecond, HeartbeatTimeout: 200 * time.Millisecond}}
	addr := listener.Addr().String()
	client, _ := layerEvents(t, idle, false, ClientLayer(targetOf(heartbeat, "ws://"+addr+"/")))
	dialLayer(t, client, addr)
	select {
	case <-closed:
	case <-time.After(10 * heartbeat.HeartbeatTimeout):
		t.Fatal("a peer answering no Ping stayed open while the layer above held")
	}
}

func TestPublicErrorsPreserveCauseAndDescribeRefusal(t *testing.T) {
	cause := errors.New("permission denied")
	for _, tc := range []struct {
		err  error
		text string
	}{{(*RejectError)(nil), "403"}, {&RejectError{Status: 429}, "429"}, {&RejectError{Status: 503, Reason: "busy"}, "busy"}, {&HandshakeError{StatusCode: 401}, "401"}, {&HandshakeError{StatusCode: 401, Reason: "credentials expired"}, "credentials expired"}, {&CloseError{}, "connection closed"}, {&CloseError{CloseEvent: CloseEvent{Err: cause}}, "permission denied"}, {&authorizeError{cause}, "permission denied"}, {&ownedProtocolClose{cause}, "permission denied"}} {
		if got := tc.err.Error(); !strings.Contains(got, tc.text) {
			t.Errorf("%T: %q does not contain %q", tc.err, got, tc.text)
		}
	}
	for _, err := range []error{&CloseError{CloseEvent: CloseEvent{Err: cause}}, &authorizeError{cause}, &ownedProtocolClose{cause}} {
		if !errors.Is(err, cause) {
			t.Fatalf("%T lost cause", err)
		}
	}
}

func TestUnopenedConnectionObservationsAndLateDeadlines(t *testing.T) {
	var absent *Conn
	if absent.Request() != nil {
		t.Fatal("nil connection has request")
	}
	c := new(Conn)
	if c.Request() != nil || c.Subprotocol() != "" || c.frameParserConfig() != nil || !c.utf8ValidationEnabled() || c.maxFramePayload() != DefaultMaxFramePayload || c.maxMessageSize() != DefaultMaxMessageSize || c.maxOutbound() != 0 || c.writeBufferedThreshold() != defaultWriteBufferedThreshold {
		t.Fatal("unopened connection defaults differ")
	}
	if _, ok := c.TLSState(); ok {
		t.Fatal("unopened connection has TLS")
	}
	c.raw = newScriptedConn()
	if _, ok := c.TLSState(); ok {
		t.Fatal("plain transport has TLS")
	}
	c.fireDeadline(deadlineKindHandshake, 1)
	c.closed.Store(true)
	c.fireDeadline(deadlineKindClose, 0)
	if c.raw.(*scriptedConn).closes != 0 {
		t.Fatal("late deadline closed an already closed connection")
	}
	state := &handshakeState{}
	c.handshake.Store(&handshakeState{})
	if c.handshakeHTTPRequest(state) != nil {
		t.Fatal("retired handshake returned a request")
	}
}

func TestDialRequestFailuresRollbackAndPreserveCloseCause(t *testing.T) {
	cause := errors.New("transport failure")
	for _, tc := range []struct {
		name               string
		writeErr, flushErr error
		short              bool
		closes             int
		want               error
	}{{"write", cause, nil, false, 1, cause}, {"short", nil, nil, true, 1, io.ErrShortWrite}, {"already-closed", net.ErrClosed, nil, false, 0, nil}, {"flush", nil, cause, false, 1, cause}, {"closed-flush", nil, net.ErrClosed, false, 0, nil}} {
		t.Run(tc.name, func(t *testing.T) {
			raw := newScriptedConn()
			raw.writeErr = tc.writeErr
			raw.flushErr = tc.flushErr
			raw.short = tc.short
			raw.closeCause = make(chan error, 1)
			c := testServerConn(raw)
			new(Dialer).sendRequest(raw, &dialSetup{conn: c, request: []byte("upgrade")})
			if raw.closes != tc.closes {
				t.Fatalf("closes=%d", raw.closes)
			}
			if tc.want != nil {
				select {
				case got := <-raw.closeCause:
					if !errors.Is(got, tc.want) {
						t.Fatal(got)
					}
				default:
					t.Fatal("missing close cause")
				}
			}
		})
	}
	new(Dialer).sendRequest(nil, &dialSetup{})
}

func TestPullClientCloseAndDeadlineLifecycle(t *testing.T) {
	bc := newBlockingConn(nil, resolveBlockingOpts(nil, []BlockingOption{
		WithReadQueueBytes(4096),
		WithReadQueueMessages(8),
		WithWriteBuffer(4096),
	}))
	if bc.CloseEventInfo() != (CloseEvent{}) || bc.InboundMessages() != 0 || bc.InboundBytes() != 0 {
		t.Fatal("new blocking conn had unexpected initial state")
	}
	bc.enqueue(nil, Message{Type: BinaryMessage, Payload: []byte("queued")})
	if bc.InboundMessages() != 1 || bc.InboundBytes() != 6 {
		t.Fatalf("queued stats = %d msgs / %d bytes", bc.InboundMessages(), bc.InboundBytes())
	}
	var dst [16]byte
	typ, out, err := bc.ReadMessageInto(nil, dst[:0])
	if err != nil || typ != BinaryMessage || string(out) != "queued" {
		t.Fatalf("ReadMessageInto = %v, %q, %v", typ, out, err)
	}
	bc.onConnClose(CloseEvent{Code: 1000, Reason: "bye"})
	if bc.CloseEventInfo().Code != 1000 {
		t.Fatalf("CloseEventInfo = %+v", bc.CloseEventInfo())
	}
}

type contractLower struct {
	*scriptedConn
	state uio.ConnState
}

func (d *contractLower) State() uio.ConnState {
	return d.state
}

type contractUpper struct {
	uio.Upper
	bytes.Buffer
	opened int
}

func (u *contractUpper) Write(p []byte) (int, error) {
	return u.Buffer.Write(p)
}

type contractLayerContext struct {
	uio.LayerContext
	down    *contractLower
	up      *contractUpper
	aborted error
	wakes   int
}

func (c *contractLayerContext) Lower() uio.Lower {
	return c.down
}

func (c *contractLayerContext) Upper() uio.Upper {
	return c.up
}

func (c *contractLayerContext) Open() {
	c.up.opened++
}

func (c *contractLayerContext) Close() error {
	return nil
}

func (c *contractLayerContext) Abort(err error) error {
	c.aborted = err
	return nil
}

func (c *contractLayerContext) Wake() error {
	c.wakes++
	return nil
}

func TestWebSocketLayerStagedFramesAndTransportEnd(t *testing.T) {
	for _, tc := range []struct {
		name    string
		wire    []byte
		closing bool
		want    error
	}{{"staged-message", frame.Append(nil, frame.Frame{Fin: true, Opcode: frame.Binary, Masked: true, Payload: []byte("staged")}, [4]byte{1, 2, 3, 4}), false, nil}, {"staged-invalid", []byte{0x83, 0}, false, nil}, {"peer-ended", nil, false, io.ErrUnexpectedEOF}, {"peer-ended-after-close", nil, true, io.EOF}} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := &contractLayerContext{down: &contractLower{scriptedConn: newScriptedConn()}, up: new(contractUpper)}
			ctx.down.inbound = tc.wire
			w := &wsLayerHandler{ctx: ctx}
			c := testServerConn(ctx.down.scriptedConn)
			c.raw = ctx.down
			c.handler = layerSink{w}
			c.closing.Store(tc.closing)
			w.conn.Store(c)
			if tc.wire == nil {
				ctx.down.state = uio.StateReadEnded
			}
			got := w.OnRead(ctx)
			if !errors.Is(got, tc.want) {
				t.Fatalf("read=%v want=%v", got, tc.want)
			}
			if tc.name == "staged-message" && ctx.up.String() != "staged" {
				t.Fatal(ctx.up.String())
			}
			if tc.name == "staged-invalid" && got != nil {
				t.Fatal("protocol close should own transport and return nil from OnRead")
			}
			c.stopCloseTimer()
			c.releaseReadState()
		})
	}
	w := new(wsLayerHandler)
	if err := w.OnRead(nil); err != nil || w.Userdata() != nil || w.Subprotocol() != "" {
		t.Fatal("unopened layer observations differ")
	}
}

func TestClientLayerRequestWriteFailures(t *testing.T) {
	for _, tc := range []struct {
		name  string
		err   error
		short bool
		want  error
	}{{"failure", io.ErrClosedPipe, false, io.ErrClosedPipe}, {"short", nil, true, io.ErrShortWrite}, {"closed", net.ErrClosed, false, nil}} {
		t.Run(tc.name, func(t *testing.T) {
			raw := newScriptedConn()
			raw.writeErr = tc.err
			raw.short = tc.short
			ctx := &contractLayerContext{down: &contractLower{scriptedConn: raw}, up: new(contractUpper)}
			layer := ClientLayer(LayerOptions{Target: "ws://localhost/"})
			handler := layer.Attach(ctx).(*wsLayerHandler)
			handler.OnOpen(ctx)
			if !errors.Is(ctx.aborted, tc.want) {
				t.Fatalf("abort=%v want=%v", ctx.aborted, tc.want)
			}
			handler.OnClose(ctx, io.EOF)
		})
	}
	if got := ServerLayer(LayerOptions{Target: "ws://localhost"}).(*wsLayer).err; !errors.Is(got, ErrInvalidOptions) {
		t.Fatal(got)
	}
	invalid := ClientLayer(LayerOptions{Target: "ws://localhost", Header: http.Header{"Upgrade": {"forbidden"}}}).(*wsLayer)
	if !errors.Is(invalid.err, ErrInvalidOptions) {
		t.Fatal(invalid.err)
	}
}

func TestPullReadCancellationWhileCloseFinishes(t *testing.T) {
	client := newBlockingConn(nil, resolveBlockingOpts(nil, nil))
	client.intakeDone = true
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()
	if _, err := client.ReadMessage(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestLifecycleOptionsAndClosedControlContracts(t *testing.T) {
	for _, opts := range []Options{{HeartbeatTimeout: -1}, {MaxOutboundBuffered: -1}} {
		if _, err := opts.resolve(); !errors.Is(err, ErrInvalidOptions) {
			t.Fatalf("options=%+v err=%v", opts, err)
		}
	}
	if got := tlsLayerOptions(nil, Options{HandshakeTimeout: -1}).HandshakeTimeout; got != 0 {
		t.Fatal(got)
	}
	if readControlError(net.ErrClosed) != ErrClosed {
		t.Fatal("transport close not translated")
	}
	var absent *Conn
	if absent.OutboundBuffered() != 0 || new(Conn).OutboundBuffered() != 0 {
		t.Fatal("unattached output")
	}
	c := testServerConn(newScriptedConn())
	c.closed.Store(true)
	if !errors.Is(c.notifyMessage(Message{}), ErrClosed) {
		t.Fatal("message delivered to closed connection")
	}
	if !errors.Is(c.acceptFrame(frame.Frame{}), ErrClosed) {
		t.Fatal("closed connection accepted frame")
	}
	c.closing.Store(true)
	c.ensureReadPauses()
	if err := c.Close(1000, ""); err != nil {
		t.Fatal(err)
	}
	if err := new(Conn).sendPong(nil); !errors.Is(err, ErrNotReady) {
		t.Fatal(err)
	}
	if err := new(Conn).sendCloseFrame(nil); !errors.Is(err, ErrNotReady) {
		t.Fatal(err)
	}
	if got := new(Conn).batchBlockSize(); got != (Options{}).batchBlockSize() {
		t.Fatal(got)
	}
}

func TestDeferredCloseErrorTerminatesExactlyOnce(t *testing.T) {
	for _, cause := range []error{io.ErrClosedPipe, net.ErrClosed, nil} {
		raw := newScriptedConn()
		raw.writeErr = cause
		c := testServerConn(raw)
		if !c.writes.close.deferClose([]byte{3, 232}) {
			t.Fatal("deferred close refused")
		}
		c.sendDeferredClose()
		c.sendDeferredClose()
		if cause == io.ErrClosedPipe && raw.closes != 1 {
			t.Fatalf("closes=%d", raw.closes)
		}
		if cause == nil && !c.writes.close.closeFrameWasSent() {
			t.Fatal("successful deferred close not marked sent")
		}
	}
}

func TestPendingCloseWaitsForEmptyQueue(t *testing.T) {
	state := new(connCloseProgress)
	state.requestTransportClose()
	if state.claimPendingTransportClose(12) {
		t.Fatal("queued bytes did not hold pending transport close")
	}
	if !state.claimPendingTransportClose(0) || state.claimPendingTransportClose(0) {
		t.Fatal("empty queue did not claim pending transport close exactly once")
	}
}

func TestDeferredClientClosePreservesMaskAndSentState(t *testing.T) {
	raw := newScriptedConn()
	c := &Conn{raw: raw, config: testDialerConfig(nil)}
	c.setRaw(raw)
	c.opened.Store(true)
	c.writes.close.deferClose([]byte{3, 232})
	c.sendDeferredClose()
	if len(raw.written) != 1 {
		t.Fatalf("writes=%d", len(raw.written))
	}
	f, _, complete, err := frame.ParseFrame(raw.written[0], &frame.ParserConfig{ExpectMask: true})
	if err != nil || !complete || !f.Masked || f.Opcode != frame.Close || !bytes.Equal(f.Payload, []byte{3, 232}) {
		t.Fatalf("frame=%+v complete=%v err=%v", f, complete, err)
	}
	if !c.writes.close.closeFrameWasSent() {
		t.Fatal("deferred close frame not marked sent")
	}
}
