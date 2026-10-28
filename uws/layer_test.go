package uws

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urpc/uio"
	"github.com/urpc/uio/utls"
)

// streamPeer is the Handler on top of a stack with a WebSocket layer: it
// gathers the stream it reads, and echoes it when told to.
type streamPeer struct {
	echo   bool
	opened chan uio.Conn
	closed chan error
	mu     sync.Mutex
	read   bytes.Buffer
	more   chan struct{}
}

func newStreamPeer(echo bool) *streamPeer {
	return &streamPeer{
		echo:   echo,
		opened: make(chan uio.Conn, 4),
		closed: make(chan error, 4),
		more:   make(chan struct{}, 1),
	}
}

func (p *streamPeer) OnOpen(c uio.Conn) { p.opened <- c }

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

func (p *streamPeer) OnTimer(uio.Conn) error        { return nil }
func (p *streamPeer) OnClose(_ uio.Conn, err error) { p.closed <- err }

// waitRead waits until the peer read n bytes and returns them.
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

// layerEvents serves events with the layers installed and handler on top,
// listening when listen is set; it returns the events and the address.
func layerEvents(t *testing.T, handler uio.Handler, listen bool, layers ...uio.Layer) (*uio.Events, string) {
	t.Helper()
	started := make(chan struct{})
	events := &uio.Events{Pollers: 1, Handler: handler, Hooks: uio.FuncHandler{
		Start: func(*uio.Events) { close(started) },
	}}
	events.Use(layers...)
	done := make(chan error, 1)
	go func() { done <- events.Serve() }()
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
		return events, ""
	}
	listener, err := events.Listen("tcp://127.0.0.1:0", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	return events, listener.Addr().String()
}

func dialLayer(t *testing.T, events *uio.Events, addr string) uio.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testIOTimeout())
	defer cancel()
	conn, err := events.DialContext(ctx, "tcp://"+addr, nil)
	if err != nil {
		t.Fatal(err)
	}
	return conn
}

// A ClientLayer speaks to a uws Server: what the layer above writes arrives
// as Binary messages, and what the Server sends comes back up as bytes.
func TestClientLayerTalksToAServer(t *testing.T) {
	addr := startEchoServer(t, robustEchoHandler{})
	peer := newStreamPeer(false)
	client, _ := layerEvents(t, peer, false, (ClientLayer("ws://"+addr+"/", LayerOptions{})))
	conn := dialLayer(t, client, addr)
	if _, err := conn.Write([]byte("through the layer")); err != nil {
		t.Fatal(err)
	}
	if got := string(peer.waitRead(t, len("through the layer"))); got != "through the layer" {
		t.Fatalf("echo = %q", got)
	}
}

// A uws Dialer speaks to a ServerLayer: its messages, Text or Binary, arrive
// above the layer as one stream, and what the layer above writes reaches the
// Dialer as Binary messages.
func TestServerLayerTalksToADialer(t *testing.T) {
	peer := newStreamPeer(true)
	_, addr := layerEvents(t, peer, true, (ServerLayer(LayerOptions{})))

	client := quietRecorder{newWSSRecorder()}
	dialer := &Dialer{Options: Options{Handler: client}}
	t.Cleanup(func() { _ = dialer.Close(nil) })
	ctx, cancel := context.WithTimeout(context.Background(), testIOTimeout())
	defer cancel()
	if err := dialer.Dial(ctx, "ws://"+addr+"/", nil); err != nil {
		t.Fatal(err)
	}
	conn := wssWait(t, client.opened, "the Dialer's open")
	if err := conn.SendText("text,"); err != nil {
		t.Fatal(err)
	}
	if err := conn.SendBinary([]byte("binary")); err != nil {
		t.Fatal(err)
	}
	if got := string(peer.waitRead(t, len("text,binary"))); got != "text,binary" {
		t.Fatalf("the layer above read %q", got)
	}
	var echoed string
	for len(echoed) < len("text,binary") {
		echoed += wssWait(t, client.got, "the echo")
	}
	if echoed != "text,binary" {
		t.Fatalf("the Dialer read %q", echoed)
	}
}

// xorLayer stands for any layer above or below the WebSocket one.
type xorLayer struct{ uio.PassThrough }

func (xorLayer) OnRead(ctx uio.LayerContext) error {
	lower := ctx.Down()
	chunk := lower.PeekChunk()
	if len(chunk) == 0 {
		return nil
	}
	out := ctx.Up().Reserve(len(chunk))[:len(chunk)]
	for i, b := range chunk {
		out[i] = b ^ 0x5a
	}
	ctx.Up().Commit(len(chunk))
	_, err := lower.Discard(len(chunk))
	return err
}

func (xorLayer) OnWrite(ctx uio.LayerContext, p []byte) (int, error) {
	out := make([]byte, len(p))
	for i, b := range p {
		out[i] = b ^ 0x5a
	}
	if _, err := ctx.Down().Write(out); err != nil {
		return 0, err
	}
	return len(p), nil
}

func xorLayerOf() uio.Layer {
	return uio.LayerFunc(func(uio.LayerContext) uio.LayerHandler { return xorLayer{} })
}

// TLS, WebSocket and a layer of the application's, stacked on both ends,
// carry a megabyte each way intact — far more than the WebSocket layer hands
// up before the layer above reads.
func TestWebSocketLayerInAStack(t *testing.T) {
	serverTLS, clientTLS := wssConfigs(t)
	echo := newStreamPeer(true)
	_, addr := layerEvents(t, echo, true,
		utls.Server(serverTLS), (ServerLayer(LayerOptions{})), xorLayerOf())
	peer := newStreamPeer(false)
	client, _ := layerEvents(t, peer, false,
		utls.Client(clientTLS), (ClientLayer("wss://"+addr+"/", LayerOptions{})), xorLayerOf())
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

// A ServerLayer whose Authorize refuses answers the refusal; the dial through
// a ClientLayer reports it as a HandshakeError, and the Handlers above the
// layers hear of no connection.
func TestLayerHandshakeRefusal(t *testing.T) {
	server := newStreamPeer(false)
	_, addr := layerEvents(t, server, true, (ServerLayer(LayerOptions{
		Authorize: func(*http.Request) (any, error) {
			return nil, &RejectError{Status: http.StatusUnauthorized, Reason: "no token"}
		},
	})))
	peer := newStreamPeer(false)
	client, _ := layerEvents(t, peer, false, (ClientLayer("ws://"+addr+"/", LayerOptions{})))
	ctx, cancel := context.WithTimeout(context.Background(), testIOTimeout())
	defer cancel()
	_, err := client.DialContext(ctx, "tcp://"+addr, nil)
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

// Authorize's userdata is the connection's, and the subprotocol the
// handshake chose is the layer's to report.
func TestServerLayerUserdataAndSubprotocol(t *testing.T) {
	server := newStreamPeer(false)
	_, addr := layerEvents(t, server, true, (ServerLayer(LayerOptions{
		Options:   Options{Subprotocols: []string{"chat"}},
		Authorize: func(r *http.Request) (any, error) { return "user:" + r.URL.Path, nil },
	})))
	peer := newStreamPeer(false)
	client, _ := layerEvents(t, peer, false, (ClientLayer("ws://"+addr+"/room", LayerOptions{
		Options: Options{Subprotocols: []string{"chat"}},
	})))
	conn := dialLayer(t, client, addr)
	accepted := wssWait(t, server.opened, "the server's open")
	if got := accepted.Userdata(); got != "user:/room" {
		t.Fatalf("server userdata = %v, want Authorize's", got)
	}
	for _, c := range []uio.Conn{conn, accepted} {
		ws, ok := uio.LayerOf(c).(interface{ Subprotocol() string })
		if !ok || ws.Subprotocol() != "chat" {
			t.Fatalf("subprotocol through LayerOf = %v", ws)
		}
	}
}

// Closing from above sends the peer a normal Close.
func TestLayerCloseSendsACloseFrame(t *testing.T) {
	watcher := &connCloseWatcher{closeWatcher: newCloseWatcher(), conns: make(chan *Conn, 1)}
	addr := startEchoServer(t, watcher)
	peer := newStreamPeer(false)
	client, _ := layerEvents(t, peer, false, (ClientLayer("ws://"+addr+"/", LayerOptions{})))
	conn := dialLayer(t, client, addr)
	wssWait(t, watcher.conns, "the server's open")
	_ = conn.Close()
	if event := wssWait(t, watcher.closed, "the server's close"); event.Code != 1000 {
		t.Fatalf("the server saw close code %d, want 1000", event.Code)
	}
	wssWait(t, peer.closed, "the client's close")
}

// The heartbeat runs in a layer: a peer that answers no Ping is closed, and
// one that answers keeps the connection open.
func TestLayerHeartbeat(t *testing.T) {
	// A timeout a loaded test host keeps — under -race beside other packages
	// a Pong can take tens of milliseconds — and an answering peer watched
	// for well past it.
	heartbeat := LayerOptions{Options: Options{
		HeartbeatInterval: 20 * time.Millisecond,
		HeartbeatTimeout:  400 * time.Millisecond,
	}}
	t.Run("silent peer", func(t *testing.T) {
		addr := silentWebSocketPeer(t)
		peer := newStreamPeer(false)
		client, _ := layerEvents(t, peer, false, (ClientLayer("ws://"+addr+"/", heartbeat)))
		dialLayer(t, client, addr)
		wssWait(t, peer.closed, "the heartbeat close")
	})
	t.Run("answering peer", func(t *testing.T) {
		addr := startEchoServer(t, robustEchoHandler{})
		peer := newStreamPeer(false)
		client, _ := layerEvents(t, peer, false, (ClientLayer("ws://"+addr+"/", heartbeat)))
		dialLayer(t, client, addr)
		select {
		case err := <-peer.closed:
			t.Fatalf("a peer answering Pings was closed: %v", err)
		case <-time.After(3 * heartbeat.HeartbeatTimeout):
		}
	})
}

// silentWebSocketPeer accepts one upgrade and then reads without answering.
func silentWebSocketPeer(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
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

// The transport's settings belong to the Events a layer runs on: options
// that name them fail every connection through the layer, and Validate says
// so up front.
func TestLayerOptionsRefuseTransportSettings(t *testing.T) {
	for name, opts := range map[string]LayerOptions{
		"Handler":             {Options: Options{Handler: robustEchoHandler{}}},
		"Pollers":             {Options: Options{Pollers: 2}},
		"TLSConfig":           {Options: Options{TLSConfig: &tls.Config{}}},
		"MaxOutboundBuffered": {Options: Options{MaxOutboundBuffered: 1 << 20}},
	} {
		if err := opts.Validate(""); !errors.Is(err, ErrInvalidOptions) {
			t.Fatalf("Validate with %s = %v, want ErrInvalidOptions", name, err)
		}
	}
	if err := (LayerOptions{}).Validate("http://example.org/"); !errors.Is(err, ErrInvalidOptions) {
		t.Fatalf("Validate of an http URL = %v, want ErrInvalidOptions", err)
	}
	addr := startEchoServer(t, robustEchoHandler{})
	peer := newStreamPeer(false)
	client, _ := layerEvents(t, peer, false, ClientLayer("ws://"+addr+"/", LayerOptions{Options: Options{Pollers: 2}}))
	ctx, cancel := context.WithTimeout(context.Background(), testIOTimeout())
	defer cancel()
	if _, err := client.DialContext(ctx, "tcp://"+addr, nil); !errors.Is(err, ErrInvalidOptions) {
		t.Fatalf("a dial through a misconfigured layer = %v, want ErrInvalidOptions", err)
	}
}

// A layer above that stops reading stops the WebSocket layer too: it hands
// up its bound and no more, however much the peer sends, and goes on once
// the layer above reads again.
func TestWebSocketLayerStopsForTheLayerAbove(t *testing.T) {
	holder := &holdingPeer{held: make(chan int, 1024), release: make(chan struct{}), conn: make(chan uio.Conn, 1)}
	_, addr := layerEvents(t, holder, true, ServerLayer(LayerOptions{}))
	peer := newStreamPeer(false)
	client, _ := layerEvents(t, peer, false, ClientLayer("ws://"+addr+"/", LayerOptions{}))
	conn := dialLayer(t, client, addr)

	const total = 4 << 20
	chunk := make([]byte, 16<<10)
	go func() {
		for sent := 0; sent < total; sent += len(chunk) {
			if _, err := conn.Write(chunk); err != nil {
				return
			}
		}
	}()
	// While held, what is handed up stops near the bound: one read round
	// past it at most, never the megabytes the peer sends.
	most := 0
	quiet := time.After(300 * time.Millisecond)
watch:
	for {
		select {
		case n := <-holder.held:
			most = max(most, n)
		case <-quiet:
			break watch
		}
	}
	if most == 0 || most > 4*uio.DefaultDeliverBound {
		t.Fatalf("handed up %d bytes the layer above did not read, want about %d", most, uio.DefaultDeliverBound)
	}
	close(holder.release)
	accepted := <-holder.conn
	_ = accepted.Wake()
	deadline := time.Now().Add(testIOTimeout())
	for holder.total.Load() < total {
		if time.Now().After(deadline) {
			t.Fatalf("read %d of %d bytes after the layer above read again", holder.total.Load(), total)
		}
		time.Sleep(time.Millisecond)
	}
}

// holdingPeer reads nothing until released, reporting what waits for it.
type holdingPeer struct {
	held    chan int
	release chan struct{}
	conn    chan uio.Conn
	total   atomic.Int64
}

func (h *holdingPeer) OnOpen(c uio.Conn) { h.conn <- c }

func (h *holdingPeer) OnRead(c uio.Conn) error {
	select {
	case <-h.release:
	default:
		select {
		case h.held <- len(c.PeekChunk()):
		default:
		}
		return nil
	}
	n, _ := c.Discard(len(c.PeekChunk()))
	h.total.Add(int64(n))
	return nil
}

func (h *holdingPeer) OnTimer(uio.Conn) error  { return nil }
func (h *holdingPeer) OnClose(uio.Conn, error) {}

// An abort from above never passes for a normal close: its Close frame says
// 1011. An abort does not wait for the frame to leave, so a backend without
// a last flush on close — stdio — may drop it, and the peer sees no code.
func TestLayerAbortSendsInternalError(t *testing.T) {
	watcher := &connCloseWatcher{closeWatcher: newCloseWatcher(), conns: make(chan *Conn, 1)}
	addr := startEchoServer(t, watcher)
	peer := newStreamPeer(false)
	client, _ := layerEvents(t, peer, false, ClientLayer("ws://"+addr+"/", LayerOptions{}))
	conn := dialLayer(t, client, addr)
	wssWait(t, watcher.conns, "the server's open")
	_ = conn.CloseWith(errors.New("gave up"))
	if event := wssWait(t, watcher.closed, "the server's close"); event.Code != 1011 && event.Code != 0 {
		t.Fatalf("the server saw close code %d, want 1011 or none", event.Code)
	}
}

// A write larger than MaxMessageSize leaves as several messages, none past
// the limit; an empty write sends nothing.
func TestLayerSplitsWritesAtTheMessageLimit(t *testing.T) {
	sizes := make(chan int, 16)
	addr := startEchoServer(t, sizeRecorder{sizes})
	peer := newStreamPeer(false)
	client, _ := layerEvents(t, peer, false, ClientLayer("ws://"+addr+"/", LayerOptions{
		Options: Options{MaxMessageSize: 1000},
	}))
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

func (sizeRecorder) OnOpen(*Conn)                   {}
func (r sizeRecorder) OnMessage(_ *Conn, m Message) { r.sizes <- len(m.Payload) }
func (sizeRecorder) OnClose(*Conn, CloseEvent)      {}

// Authorize returning no userdata leaves the connection's own.
func TestServerLayerKeepsUserdataAuthorizeLeaves(t *testing.T) {
	server := newStreamPeer(false)
	started := make(chan struct{})
	events := &uio.Events{Pollers: 1, Handler: server, Hooks: uio.FuncHandler{Start: func(*uio.Events) { close(started) }}}
	events.Use(ServerLayer(LayerOptions{Authorize: func(*http.Request) (any, error) { return nil, nil }}))
	done := make(chan error, 1)
	go func() { done <- events.Serve() }()
	t.Cleanup(func() { _ = events.Close(nil); <-done })
	<-started
	listener, err := events.Listen("tcp://127.0.0.1:0", "listener's")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	peer := newStreamPeer(false)
	client, _ := layerEvents(t, peer, false, ClientLayer("ws://"+listener.Addr().String()+"/", LayerOptions{}))
	dialLayer(t, client, listener.Addr().String())
	if got := wssWait(t, server.opened, "the server's open").Userdata(); got != "listener's" {
		t.Fatalf("userdata = %v, want the listener's kept", got)
	}
}

// doubling writes every byte twice and reads every other: a layer under the
// WebSocket one that sends more bytes than it is given.
type doubling struct{ uio.PassThrough }

func (doubling) OnRead(ctx uio.LayerContext) error {
	lower := ctx.Down()
	chunk := lower.PeekChunk()
	pairs := len(chunk) / 2
	if pairs == 0 {
		return nil
	}
	out := ctx.Up().Reserve(pairs)[:pairs]
	for i := range out {
		out[i] = chunk[2*i]
	}
	ctx.Up().Commit(pairs)
	_, err := lower.Discard(2 * pairs)
	return err
}

func (doubling) OnWrite(ctx uio.LayerContext, p []byte) (int, error) {
	out := make([]byte, 2*len(p))
	for i, b := range p {
		out[2*i], out[2*i+1] = b, b
	}
	if _, err := ctx.Down().Write(out); err != nil {
		return 0, err
	}
	return len(p), nil
}

// The WebSocket layer's own close — the answer to a peer's Close — waits for
// what it queued, whatever the layers below do to its bytes: a layer below
// that doubles them makes the socket report twice the bytes the protocol
// queued, and the close used to come early and drop the rest.
func TestLayerCloseAnswerKeepsTheBacklog(t *testing.T) {
	const messages, size = 64, 64 << 10
	writer := uio.FuncHandler{
		Open: func(c uio.Conn) {
			for range messages {
				_, _ = c.Write(make([]byte, size))
			}
		},
		Read:  func(c uio.Conn) error { _, _ = c.Discard(c.InboundBuffered()); return nil },
		Close: func(uio.Conn, error) {},
	}
	_, addr := layerEvents(t, writer, true, uio.LayerFunc(func(uio.LayerContext) uio.LayerHandler { return doubling{} }), ServerLayer(LayerOptions{}))

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
	request := "GET / HTTP/1.1\r\nHost: x\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n" +
		"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\n\r\n"
	if _, err := peer.Write(double([]byte(request))); err != nil {
		t.Fatal(err)
	}
	// A masked Close 1000, with a zero mask.
	if _, err := peer.Write(double([]byte{0x88, 0x82, 0, 0, 0, 0, 0x03, 0xe8})); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond) // the server answers behind its backlog
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

// A layer that holds its output until flushed under a ClientLayer: the
// upgrade request leaves at the end of the turn that wrote it.
func TestClientLayerOverAHoldingLayer(t *testing.T) {
	addr := startEchoServer(t, robustEchoHandler{})
	peer := newStreamPeer(false)
	client, _ := layerEvents(t, peer, false,
		uio.LayerFunc(func(uio.LayerContext) uio.LayerHandler { return &holdUntilFlush{} }),
		ClientLayer("ws://"+addr+"/", LayerOptions{}))
	conn := dialLayer(t, client, addr)
	if _, err := conn.Write([]byte("held, then flushed")); err != nil {
		t.Fatal(err)
	}
	_ = conn.Flush()
	if got := string(peer.waitRead(t, len("held, then flushed"))); got != "held, then flushed" {
		t.Fatalf("echo = %q", got)
	}
}

// holdUntilFlush holds what is written to it until Flush.
type holdUntilFlush struct {
	uio.PassThrough
	mu   sync.Mutex
	held []byte
}

func (h *holdUntilFlush) OnWrite(_ uio.LayerContext, p []byte) (int, error) {
	h.mu.Lock()
	h.held = append(h.held, p...)
	h.mu.Unlock()
	return len(p), nil
}

func (h *holdUntilFlush) OutboundHeld(uio.LayerContext) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.held)
}

func (h *holdUntilFlush) Flush(ctx uio.LayerContext) error {
	h.mu.Lock()
	held := h.held
	h.held = nil
	h.mu.Unlock()
	if len(held) == 0 {
		return nil
	}
	_, err := ctx.Down().Write(held)
	return err
}

// Over a layer that changes how many bytes go on the wire, the heartbeat
// follows the Ping by wire position: progress on a backlog does not mark a
// Ping sent that still waits behind it. Counted in the protocol's own bytes,
// the wire's doubled progress passed the Ping's position long before.
func TestLayerPingFollowsTheWire(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		peer, err := listener.Accept()
		if err != nil {
			return
		}
		defer peer.Close()
		// The upgrade, every byte doubled; then read whatever comes.
		var request []byte
		one := make([]byte, 2)
		for !bytes.HasSuffix(request, []byte("\r\n\r\n")) {
			if _, err := io.ReadFull(peer, one); err != nil {
				return
			}
			request = append(request, one[0])
		}
		key := ""
		for _, line := range strings.Split(string(request), "\r\n") {
			if name, value, ok := strings.Cut(line, ":"); ok && strings.EqualFold(name, "Sec-WebSocket-Key") {
				key = strings.TrimSpace(value)
			}
		}
		response := fmt.Sprintf("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n", testAcceptKey(key))
		doubled := make([]byte, 0, 2*len(response))
		for i := range len(response) {
			doubled = append(doubled, response[i], response[i])
		}
		_, _ = peer.Write(doubled)
		_, _ = io.Copy(io.Discard, peer)
	}()
	gate := &sendGate{}
	peer := newStreamPeer(false)
	client, _ := layerEvents(t, peer, false,
		uio.LayerFunc(func(uio.LayerContext) uio.LayerHandler { return gate }),
		uio.LayerFunc(func(uio.LayerContext) uio.LayerHandler { return doubling{} }),
		ClientLayer("ws://"+listener.Addr().String()+"/", LayerOptions{Options: Options{
			HeartbeatInterval: 100 * time.Millisecond,
			HeartbeatTimeout:  10 * time.Second,
		}}))
	conn := dialLayer(t, client, listener.Addr().String())
	ws, ok := uio.FindLayer[*wsLayerHandler](conn)
	if !ok {
		t.Fatal("no WebSocket layer")
	}
	heartbeat := ws.conn.Load().heartbeat

	gate.close() // from here on the gate decides what reaches the socket
	for range 16 {
		if _, err := conn.Write(make([]byte, 16<<10)); err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.Now().Add(testIOTimeout())
	for !heartbeat.pingOutstanding.Load() || gate.held() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("no Ping was queued behind the backlog")
		}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond) // the Ping is handed down behind the backlog
	// Three quarters of what is held leaves; the Ping, at the end, has not.
	gate.release(gate.held() * 3 / 4)
	time.Sleep(100 * time.Millisecond)
	heartbeat.mu.Lock()
	sentAt := heartbeat.pingSentAt
	heartbeat.mu.Unlock()
	if sentAt != 0 {
		t.Fatalf("the Ping was marked sent with %d bytes still held ahead of it", gate.held())
	}
	gate.release(gate.held()) // the rest, the Ping with it
	deadline = time.Now().Add(testIOTimeout())
	for {
		heartbeat.mu.Lock()
		sentAt, outstanding := heartbeat.pingSentAt, heartbeat.pingOutstanding.Load()
		heartbeat.mu.Unlock()
		if sentAt != 0 || !outstanding {
			break // sent, or already answered
		}
		if time.Now().After(deadline) {
			t.Fatal("the Ping was never marked sent once it left")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// sendGate passes what is written through until closed; from then on it
// holds it, and lets go of what release says.
type sendGate struct {
	uio.PassThrough
	mu     sync.Mutex
	ctx    uio.LayerContext
	closed bool
	queue  []byte
}

func (g *sendGate) OnOpen(ctx uio.LayerContext) {
	g.mu.Lock()
	g.ctx = ctx
	g.mu.Unlock()
	ctx.Up().Open()
}

func (g *sendGate) OnWrite(ctx uio.LayerContext, p []byte) (int, error) {
	g.mu.Lock()
	if g.closed {
		g.queue = append(g.queue, p...)
		g.mu.Unlock()
		return len(p), nil
	}
	g.mu.Unlock()
	return ctx.Down().Write(p)
}

func (g *sendGate) OutboundHeld(uio.LayerContext) int { return g.held() }

func (g *sendGate) close() {
	g.mu.Lock()
	g.closed = true
	g.mu.Unlock()
}

func (g *sendGate) held() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.queue)
}

func (g *sendGate) release(n int) {
	g.mu.Lock()
	out := append([]byte(nil), g.queue[:n]...)
	g.queue = g.queue[n:]
	ctx := g.ctx
	g.mu.Unlock()
	_, _ = ctx.Down().Write(out)
}

// The same over TLS, whose handshake went out before the WebSocket layer
// opened: the Ping's progress is read from the stack's own wire positions,
// which count from the first byte, so a Ping that left is marked sent even
// when the peer never answers it.
func TestLayerPingFollowsTheWireOverTLS(t *testing.T) {
	serverTLS, clientTLS := wssConfigs(t)
	listener, err := tls.Listen("tcp", "127.0.0.1:0", serverTLS)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		peer, err := listener.Accept()
		if err != nil {
			return
		}
		defer peer.Close()
		reader := bufio.NewReader(peer)
		key := ""
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				return
			}
			if name, value, ok := strings.Cut(line, ":"); ok && strings.EqualFold(name, "Sec-WebSocket-Key") {
				key = strings.TrimSpace(value)
			}
			if line == "\r\n" {
				break
			}
		}
		_, _ = fmt.Fprintf(peer, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n", testAcceptKey(key))
		_, _ = io.Copy(io.Discard, reader) // reads everything, answers no Ping
	}()
	gate := &sendGate{}
	peer := newStreamPeer(false)
	client, _ := layerEvents(t, peer, false,
		uio.LayerFunc(func(uio.LayerContext) uio.LayerHandler { return gate }),
		utls.Client(clientTLS),
		ClientLayer("wss://"+listener.Addr().String()+"/", LayerOptions{Options: Options{
			HeartbeatInterval: 100 * time.Millisecond,
			HeartbeatTimeout:  10 * time.Second,
		}}))
	conn := dialLayer(t, client, listener.Addr().String())
	ws, ok := uio.FindLayer[*wsLayerHandler](conn)
	if !ok {
		t.Fatal("no WebSocket layer")
	}
	heartbeat := ws.conn.Load().heartbeat

	gate.close()
	for range 16 {
		if _, err := conn.Write(make([]byte, 16<<10)); err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.Now().Add(testIOTimeout())
	for !heartbeat.pingOutstanding.Load() || gate.held() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("no Ping was queued behind the backlog")
		}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)
	gate.release(gate.held() * 3 / 4)
	time.Sleep(100 * time.Millisecond)
	heartbeat.mu.Lock()
	sentAt := heartbeat.pingSentAt
	heartbeat.mu.Unlock()
	if sentAt != 0 {
		t.Fatalf("the Ping was marked sent with %d bytes still held ahead of it", gate.held())
	}
	gate.release(gate.held())
	deadline = time.Now().Add(testIOTimeout())
	for {
		heartbeat.mu.Lock()
		sentAt = heartbeat.pingSentAt
		heartbeat.mu.Unlock()
		if sentAt != 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the Ping left but was never marked sent")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// What a layer above the WebSocket one holds is not on the WebSocket
// layer's wire: its WireQueued counts the layers below it only, so a Ping
// sent below a holding layer is marked sent once it left — the peer here
// never answers it, so nothing else would end it.
func TestPingBelowAHoldingLayer(t *testing.T) {
	addr := silentWebSocketPeer(t)
	gate := &sendGate{}
	peer := newStreamPeer(false)
	client, _ := layerEvents(t, peer, false,
		passThroughLayerOf(),
		ClientLayer("ws://"+addr+"/", LayerOptions{Options: Options{
			HeartbeatInterval: 50 * time.Millisecond,
			HeartbeatTimeout:  10 * time.Second,
		}}),
		uio.LayerFunc(func(uio.LayerContext) uio.LayerHandler { return gate }))
	conn := dialLayer(t, client, addr)
	ws, ok := uio.FindLayer[*wsLayerHandler](conn)
	if !ok {
		t.Fatal("no WebSocket layer")
	}
	engine := ws.conn.Load()
	counter, ok := engine.raw.(uio.WireCounter)
	if !ok {
		t.Fatal("the WebSocket layer's transport counts no wire positions")
	}
	gate.close()
	before := counter.WireQueued()
	if _, err := conn.Write(make([]byte, 4096)); err != nil {
		t.Fatal(err)
	}
	if gate.held() == 0 {
		t.Fatal("the gate above held nothing")
	}
	if after := counter.WireQueued(); after != before {
		t.Fatalf("WireQueued below the gate moved from %d to %d for bytes the gate holds", before, after)
	}
	heartbeat := engine.heartbeat
	deadline := time.Now().Add(testIOTimeout())
	for {
		heartbeat.mu.Lock()
		sentAt := heartbeat.pingSentAt
		heartbeat.mu.Unlock()
		if sentAt != 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("a Ping below the holding layer left but was never marked sent")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func passThroughLayerOf() uio.Layer {
	return uio.LayerFunc(func(uio.LayerContext) uio.LayerHandler { return uio.PassThrough{} })
}

// A layer below that sends fewer bytes than it holds puts the Ping's wire
// position past where the wire will ever end: the Ping is marked sent once
// the queue below is empty all the same.
func TestPingBelowAShrinkingLayer(t *testing.T) {
	addr := silentWebSocketPeer(t)
	gate := &sendGate{}
	peer := newStreamPeer(false)
	client, _ := layerEvents(t, peer, false,
		uio.LayerFunc(func(uio.LayerContext) uio.LayerHandler { return gate }),
		ClientLayer("ws://"+addr+"/", LayerOptions{Options: Options{
			HeartbeatInterval: 50 * time.Millisecond,
			HeartbeatTimeout:  10 * time.Second,
		}}))
	conn := dialLayer(t, client, addr)
	ws, ok := uio.FindLayer[*wsLayerHandler](conn)
	if !ok {
		t.Fatal("no WebSocket layer")
	}
	heartbeat := ws.conn.Load().heartbeat
	gate.close() // the next Ping waits in the gate as its position is read
	deadline := time.Now().Add(testIOTimeout())
	for {
		heartbeat.mu.Lock()
		target := heartbeat.pingWireTarget
		heartbeat.mu.Unlock()
		if target != 0 && gate.held() > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no Ping was held in the gate")
		}
		time.Sleep(time.Millisecond)
	}
	gate.releaseHalf() // the gate sends half of what it held, and holds nothing
	for {
		heartbeat.mu.Lock()
		sentAt, target := heartbeat.pingSentAt, heartbeat.pingWireTarget
		heartbeat.mu.Unlock()
		if sentAt != 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the queue below emptied but the Ping was never marked sent: target %d, wire sent %d",
				target, ws.conn.Load().raw.(uio.WireCounter).WireSent())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// releaseHalf lets go of everything held, sending only the first half.
func (g *sendGate) releaseHalf() {
	g.mu.Lock()
	out := append([]byte(nil), g.queue[:(len(g.queue)+1)/2]...)
	g.queue = nil
	ctx := g.ctx
	g.mu.Unlock()
	_, _ = ctx.Down().Write(out)
}

// A write from above that the WebSocket layer splits at MaxMessageSize goes
// out whole: another writer's messages never land between its parts.
func TestLayerSplitWritesStayWhole(t *testing.T) {
	sink := &payloadSink{}
	conn := &Conn{raw: sink, config: testServerConfig(&Server{Options: Options{MaxMessageSize: 1, MaxFramePayload: 1}})}
	conn.opened.Store(true)
	w := &wsLayerHandler{}
	w.conn.Store(conn)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for _, letter := range []byte{'A', 'B'} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if n, err := w.OnWrite(nil, bytes.Repeat([]byte{letter}, 2000)); n != 2000 || err != nil {
				t.Errorf("OnOutbound = %d, %v", n, err)
			}
		}()
	}
	close(start)
	wg.Wait()
	switches := 0
	for i := 1; i < len(sink.payload); i++ {
		if sink.payload[i] != sink.payload[i-1] {
			switches++
		}
	}
	if switches > 1 {
		t.Fatalf("two writes interleaved %d times across %d bytes", switches, len(sink.payload))
	}
}

// payloadSink is a transport that keeps each one-byte message's payload and
// takes a moment over every send, as a busy socket does.
type payloadSink struct {
	uio.Conn
	mu      sync.Mutex
	payload []byte
}

func (s *payloadSink) OutboundBuffered() int               { return 0 }
func (s *payloadSink) ReserveOutbound(int) ([]byte, error) { return nil, uio.ErrReserveUnsupported }
func (s *payloadSink) Flush() error                        { return nil }
func (s *payloadSink) WriteOwned(b *uio.Buffer) (int, error) {
	n, err := s.Write(b.Bytes())
	uio.ReleaseBuffer(b)
	return n, err
}

func (s *payloadSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	s.payload = append(s.payload, p[len(p)-1])
	s.mu.Unlock()
	time.Sleep(time.Microsecond)
	return len(p), nil
}

func (s *payloadSink) Writev(vec [][]byte) (int, error) {
	n := 0
	for _, p := range vec {
		n += len(p)
	}
	s.mu.Lock()
	s.payload = append(s.payload, vec[len(vec)-1]...)
	s.mu.Unlock()
	time.Sleep(time.Microsecond)
	return n, nil
}

// The heartbeat goes on while the layer above reads nothing: a peer that
// floods data and never answers a Ping is found out though the WebSocket
// layer stopped delivering.
func TestLayerHeartbeatWhileTheLayerAboveHolds(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
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
		go func() { _, _ = io.Copy(io.Discard, reader) }() // Pings, never answered
		frame := append([]byte{0x82, 126, 0x40, 0x00}, make([]byte, 16<<10)...)
		for {
			if _, err := conn.Write(frame); err != nil {
				return
			}
		}
	}()
	closed := make(chan error, 1)
	idle := uio.FuncHandler{
		Read:  func(uio.Conn) error { return nil }, // reads nothing
		Close: func(_ uio.Conn, err error) { closed <- err },
	}
	heartbeat := LayerOptions{Options: Options{
		HeartbeatInterval: 20 * time.Millisecond,
		HeartbeatTimeout:  200 * time.Millisecond,
	}}
	addr := listener.Addr().String()
	client, _ := layerEvents(t, idle, false, ClientLayer("ws://"+addr+"/", heartbeat))
	dialLayer(t, client, addr)
	select {
	case <-closed:
	case <-time.After(10 * heartbeat.HeartbeatTimeout):
		t.Fatal("a peer answering no Ping stayed open while the layer above held")
	}
}
