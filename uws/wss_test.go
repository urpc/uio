package uws

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	crand "crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urpc/uio"
	"github.com/urpc/uio/uws/internal/frame"
)

// wssConfigs makes a matching pair: a certificate for the server, a client
// that trusts it.
func wssConfigs(t testing.TB) (serverTLS *tls.Config, clientTLS *tls.Config) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), crand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "uws wss test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(crand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	serverTLS = &tls.Config{Certificates: []tls.Certificate{{
		Certificate: [][]byte{der},
		PrivateKey:  key,
		Leaf:        leaf,
	}}}
	clientTLS = &tls.Config{ServerName: "localhost", RootCAs: pool}
	return serverTLS, clientTLS
}

// wssRecorder is the handler each side of a wss test reports through.
type wssRecorder struct {
	opened chan *Conn
	got    chan string
	closed chan error
}

func newWSSRecorder() *wssRecorder {
	return &wssRecorder{
		opened: make(chan *Conn, 4),
		got:    make(chan string, 16),
		closed: make(chan error, 4),
	}
}

func (h *wssRecorder) OnOpen(c *Conn) { h.opened <- c }

func (h *wssRecorder) OnMessage(c *Conn, m Message) {
	h.got <- string(m.Payload)
	_ = c.SendText(m.Text()) // echo
}

func (h *wssRecorder) OnClose(_ *Conn, event CloseEvent) { h.closed <- event.Err }

// startWSS serves an echoing wss server and returns its address.
func startWSS(t *testing.T, serverTLS *tls.Config) string {
	t.Helper()
	server, addr, serveDone := startTestServer(t, func() *Server {
		return NewServer(Options{
			Handler:   newWSSRecorder(),
			TLSConfig: serverTLS,
		})
	})
	t.Cleanup(func() {
		_ = server.Close(nil)
		<-serveDone
	})
	return addr
}

func wssWait[T any](t *testing.T, c chan T, what string) T {
	t.Helper()
	select {
	case v := <-c:
		return v
	case <-time.After(testIOTimeout()):
		t.Fatalf("no %s within %v", what, testIOTimeout())
		return *new(T)
	}
}

// A dialer with a TLSConfig speaks wss to a server with one: the upgrade
// travels inside TLS, messages go both ways through it, and a close travels
// back as the close handshake.
func TestWSSRoundTrip(t *testing.T) {
	serverTLS, clientTLS := wssConfigs(t)
	addr := startWSS(t, serverTLS)

	// The client's handler must not echo: the server's own recorder does,
	// and an echoing client would ping-pong "secure hello" between the two
	// recorders until the close, filling their queues — a turn blocked on
	// one of them then holds the shutdown up.
	client := quietRecorder{newWSSRecorder()}
	dialer := &Dialer{Options: Options{
		Handler:   client,
		TLSConfig: clientTLS,
	}}
	t.Cleanup(func() { _ = dialer.Close(nil) })
	ctx, cancel := context.WithTimeout(context.Background(), testIOTimeout())
	defer cancel()
	if err := dialer.Dial(ctx, "wss://"+addr+"/", nil); err != nil {
		t.Fatal(err)
	}
	conn := wssWait(t, client.opened, "client open over wss")
	if err := conn.SendText("secure hello"); err != nil {
		t.Fatal(err)
	}
	if got := wssWait(t, client.got, "client message"); got != "secure hello" {
		t.Fatalf("echo = %q", got)
	}
	_ = conn.Close(1000, "done")
	select {
	case <-client.closed:
	case <-time.After(testIOTimeout()):
		t.Fatal("the client never closed")
	}
}

// A client whose TLSConfig trusts nothing fails the dial: the handshake's
// failure is what Dial reports, the WebSocket handshake never begins, and the
// Handler hears nothing of the connection.
func TestWSSRefusedCertificate(t *testing.T) {
	serverTLS, _ := wssConfigs(t)
	addr := startWSS(t, serverTLS)

	client := newWSSRecorder()
	dialer := &Dialer{Options: Options{
		Handler: client,
		TLSConfig: &tls.Config{
			ServerName: "localhost", // trusts no certificate
		},
	}}
	t.Cleanup(func() { _ = dialer.Close(nil) })
	ctx, cancel := context.WithTimeout(context.Background(), testIOTimeout())
	defer cancel()
	if err := dialer.Dial(ctx, "wss://"+addr+"/", nil); err == nil {
		t.Fatal("a refused certificate dialed cleanly")
	}
	select {
	case <-client.opened:
		t.Fatal("a refused TLS handshake still opened the WebSocket")
	default:
	}
	select {
	case <-client.closed:
		t.Fatal("a failed dial closed a connection the Handler never saw")
	default:
	}
}

// wss without a TLSConfig is a configuration error, before anything is
// dialed.
func TestWSSNeedsATLSConfig(t *testing.T) {
	dialer := &Dialer{}
	t.Cleanup(func() { _ = dialer.Close(nil) })
	err := dialer.Dial(context.Background(), "wss://127.0.0.1:1/", nil)
	if !errors.Is(err, ErrInvalidOptions) {
		t.Fatalf("Dial wss without a TLSConfig = %v, want ErrInvalidOptions", err)
	}
}

// The heartbeat runs on the transport's timed turns, and over TLS those turns
// carry no plaintext: a TLS layer that kept them to itself stopped the
// heartbeat, and a silent wss peer was never closed.
func TestWSSHeartbeatClosesSilentPeer(t *testing.T) {
	serverTLS, clientTLS := wssConfigs(t)
	listener, err := tls.Listen("tcp", "127.0.0.1:0", serverTLS)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
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
		// Silent from here: consume everything, answer no Ping.
		_, _ = io.Copy(io.Discard, reader)
	}()

	watcher := newCloseWatcher()
	dialer := NewDialer(Options{
		Handler:               watcher,
		Pollers:               1,
		HeartbeatInterval:     10 * time.Millisecond,
		HeartbeatTimeout:      30 * time.Millisecond,
		OutboundHighWatermark: -1,
		TLSConfig:             clientTLS,
	})
	t.Cleanup(func() { _ = dialer.Close(nil) })
	if err := dialer.Dial(context.Background(), "wss://"+listener.Addr().String()+"/", nil); err != nil {
		t.Fatal(err)
	}
	wssWait(t, watcher.open, "OnOpen")
	if event := wssWait(t, watcher.closed, "the heartbeat close"); event.Code != 1001 {
		t.Fatalf("heartbeat close code = %d, want 1001", event.Code)
	}
}

// A wss peer that stops reading: the client's outbound fills, the outbound
// limit refuses the heartbeat's Ping, and nobody answers. The stall deadline
// still closes the connection — through the TLS layer, whose timed turns
// carry no plaintext — with the heartbeat's close code, whether its Close
// frame was refused or queued behind what never drains.
func TestWSSHeartbeatClosesBackpressuredPeer(t *testing.T) {
	serverTLS, clientTLS := wssConfigs(t)
	listener, err := tls.Listen("tcp", "127.0.0.1:0", serverTLS)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	hold := make(chan struct{})
	defer close(hold)
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
		<-hold // read nothing more: the client's outbound fills
		_, _ = io.Copy(io.Discard, reader)
	}()
	watcher := &connCloseWatcher{closeWatcher: newCloseWatcher(), conns: make(chan *Conn, 1)}
	// The heartbeat's Close frame may fit the queue, which never drains: the
	// close handshake then ends at CloseTimeout.
	dialer := NewDialer(Options{
		Handler:               watcher,
		Pollers:               1,
		HeartbeatInterval:     10 * time.Millisecond,
		HeartbeatTimeout:      60 * time.Millisecond,
		CloseTimeout:          100 * time.Millisecond,
		OutboundHighWatermark: -1,
		MaxOutboundBuffered:   64 << 10,
		TLSConfig:             clientTLS,
	})
	t.Cleanup(func() { _ = dialer.Close(nil) })
	if err := dialer.Dial(context.Background(), "wss://"+listener.Addr().String()+"/", nil); err != nil {
		t.Fatal(err)
	}
	conn := wssWait(t, watcher.conns, "OnOpen")
	// Fill the outbound so the next Ping is refused.
	big := make([]byte, 16<<10)
	for i := 0; i < 64; i++ {
		if err := conn.SendBinary(big); err != nil {
			break
		}
	}
	if event := wssWait(t, watcher.closed, "the heartbeat close"); event.Code != 1001 || event.Reason != "heartbeat timeout" {
		t.Fatalf("closed with %d %q, want the heartbeat timeout", event.Code, event.Reason)
	}
}

type connCloseWatcher struct {
	*closeWatcher
	conns chan *Conn
}

func (w *connCloseWatcher) OnOpen(c *Conn) {
	w.closeWatcher.OnOpen(c)
	w.conns <- c
}

// A Dialer's TLSConfig that names no server verifies the host each wss://
// URL names, as crypto/tls's own dial does: crypto/tls refuses a client
// Config that names neither a server nor InsecureSkipVerify.
func TestWSSDialVerifiesTheURLHost(t *testing.T) {
	serverTLS, clientTLS := wssConfigs(t)
	addr := startWSS(t, serverTLS)
	clientTLS.ServerName = ""

	client := newWSSRecorder()
	dialer := &Dialer{Options: Options{Handler: client, TLSConfig: clientTLS}}
	t.Cleanup(func() { _ = dialer.Close(nil) })
	ctx, cancel := context.WithTimeout(context.Background(), testIOTimeout())
	defer cancel()
	if err := dialer.Dial(ctx, "wss://"+addr+"/", nil); err != nil {
		t.Fatal(err)
	}
	conn := wssWait(t, client.opened, "client open over wss")
	state, ok := uio.LayerOf(conn.raw).(interface{ State() tls.ConnectionState })
	if !ok {
		t.Fatal("the connection runs no TLS layer")
	}
	if chains := state.State().VerifiedChains; len(chains) == 0 {
		t.Fatal("the server's certificate was not verified")
	}
}

// One Dialer with a TLSConfig dials ws:// and wss:// alike: TLS runs for the
// wss:// dial only.
func TestWSSDialerDialsWSAndWSS(t *testing.T) {
	serverTLS, clientTLS := wssConfigs(t)
	secure := startWSS(t, serverTLS)
	plain := startEchoServer(t, robustEchoHandler{})

	client := quietRecorder{newWSSRecorder()}
	dialer := &Dialer{Options: Options{Handler: client, TLSConfig: clientTLS}}
	t.Cleanup(func() { _ = dialer.Close(nil) })
	ctx, cancel := context.WithTimeout(context.Background(), testIOTimeout())
	defer cancel() // it bounds the handshakes too
	for _, target := range []string{"ws://" + plain + "/", "wss://" + secure + "/"} {
		if err := dialer.Dial(ctx, target, nil); err != nil {
			t.Fatalf("Dial %s: %v", target, err)
		}
		conn := wssWait(t, client.opened, "open of "+target)
		if _, tlsRuns := uio.LayerOf(conn.raw).(interface{ State() tls.ConnectionState }); tlsRuns != strings.HasPrefix(target, "wss") {
			t.Fatalf("%s runs TLS = %v", target, tlsRuns)
		}
		if err := conn.SendText("hello " + target); err != nil {
			t.Fatal(err)
		}
		if got := wssWait(t, client.got, "echo of "+target); got != "hello "+target {
			t.Fatalf("echo = %q", got)
		}
	}
}

// A Server with a TLSConfig adopts net/http's plain upgrades without TLS of
// its own.
func TestWSSServerAdoptsHTTPUpgradesWithoutTLS(t *testing.T) {
	serverTLS, _ := wssConfigs(t)
	server := NewServer(Options{Handler: newWSSRecorder(), TLSConfig: serverTLS, Pollers: 1})
	serveWithoutListener(t, server)
	httpServer := httptest.NewServer(server)
	t.Cleanup(httpServer.Close)

	client := quietRecorder{newWSSRecorder()}
	dialer := &Dialer{Options: Options{Handler: client}}
	t.Cleanup(func() { _ = dialer.Close(nil) })
	ctx, cancel := context.WithTimeout(context.Background(), testIOTimeout())
	defer cancel()
	if err := dialer.Dial(ctx, "ws"+strings.TrimPrefix(httpServer.URL, "http"), nil); err != nil {
		t.Fatal(err)
	}
	conn := wssWait(t, client.opened, "client open through net/http")
	if err := conn.SendText("adopted"); err != nil {
		t.Fatal(err)
	}
	if got := wssWait(t, client.got, "the adopted connection's echo"); got != "adopted" {
		t.Fatalf("echo = %q", got)
	}
}

// quietRecorder records what it reads and answers nothing, so a peer that
// echoes sends each message back once.
type quietRecorder struct{ *wssRecorder }

func (h quietRecorder) OnMessage(_ *Conn, m Message) { h.got <- string(m.Payload) }

// Authorize sees the TLS state a wss connection settled on, and so does the
// connection: the server name the client asked for.
func TestWSSAuthorizeSeesTLS(t *testing.T) {
	serverTLS, clientTLS := wssConfigs(t)
	seen := make(chan string, 1)
	server := newWSSRecorder()
	srv, addr, done := startTestServer(t, func() *Server {
		s := NewServer(Options{Handler: server, TLSConfig: serverTLS})
		s.Authorize = func(r *http.Request) (any, error) {
			if r.TLS == nil {
				seen <- "no TLS"
			} else {
				seen <- r.TLS.ServerName
			}
			return nil, nil
		}
		return s
	})
	t.Cleanup(func() { stopTestServer(t, srv, done) })
	dialer := &Dialer{Options: Options{Handler: quietRecorder{newWSSRecorder()}, TLSConfig: clientTLS}}
	t.Cleanup(func() { _ = dialer.Close(nil) })
	ctx, cancel := context.WithTimeout(context.Background(), testIOTimeout())
	defer cancel()
	if err := dialer.Dial(ctx, "wss://"+addr+"/", nil); err != nil {
		t.Fatal(err)
	}
	if got := wssWait(t, seen, "Authorize"); got != "localhost" {
		t.Fatalf("Authorize saw server name %q, want localhost", got)
	}
	conn := wssWait(t, server.opened, "the server's open")
	if state, ok := conn.TLSState(); !ok || !state.HandshakeComplete {
		t.Fatalf("TLSState = %v, %v", state.HandshakeComplete, ok)
	}
}

// A frame batched in a read round is still owed while it sits in the batch:
// on a plain transport a report retires what was handed down, not what has
// not been yet. (A tracked transport keeps no pending count at all: its Close
// frame's receipt orders after whatever was queued before it.)
func TestBatchedFrameStaysOwed(t *testing.T) {
	addr := startEchoServer(t, robustEchoHandler{})
	client := quietRecorder{newWSSRecorder()}
	dialer := &Dialer{Options: Options{Handler: client}}
	t.Cleanup(func() { _ = dialer.Close(nil) })
	ctx, cancel := context.WithTimeout(context.Background(), testIOTimeout())
	defer cancel()
	if err := dialer.Dial(ctx, "ws://"+addr+"/", nil); err != nil {
		t.Fatal(err)
	}
	conn := wssWait(t, client.opened, "the client's open")
	deadline := time.Now().Add(testIOTimeout())
	for conn.raw.OutboundBuffered() != 0 || conn.writes.close.pendingBytes.Load() != 0 {
		if time.Now().After(deadline) {
			t.Fatal("the handshake's bytes never left")
		}
		time.Sleep(time.Millisecond)
	}
	conn.writes.mu.Lock()
	conn.batching.Store(true) // as a read round does
	payload := []byte("batched")
	if err := conn.sendFrameLocked(frame.Frame{Fin: true, Opcode: frame.Binary, Payload: payload}); err != nil {
		conn.batching.Store(false)
		conn.writes.mu.Unlock()
		t.Fatal(err)
	}
	conn.transportSent(1) // the queue below is empty: all handed down left
	owed := conn.writes.close.pendingBytes.Load() + int64(conn.batchUntracked)
	conn.batching.Store(false)
	conn.unlockWrite()
	if want := int64(frameWireSize(len(payload), true)); owed < want {
		t.Fatalf("%d bytes owed with a %d-byte frame still in the batch", owed, want)
	}
}

// Heartbeats on both ends of a wss connection under a steady pipelined load:
// the Pings follow their frames through TLS by wire receipt, are answered,
// and no end is closed for a heartbeat it kept.
func TestWSSHeartbeatUnderPipelinedLoad(t *testing.T) {
	serverTLS, clientTLS := wssConfigs(t)
	// Pings every 20ms through a load that stays in flight; a timeout a
	// loaded test host keeps.
	heartbeat := Options{HeartbeatInterval: 20 * time.Millisecond, HeartbeatTimeout: time.Second}
	serverClosed := make(chan CloseEvent, 1)
	srv, addr, done := startTestServer(t, func() *Server {
		opts := heartbeat
		opts.Handler = echoUntilClose{closed: serverClosed}
		opts.TLSConfig = serverTLS
		return NewServer(opts)
	})
	t.Cleanup(func() { stopTestServer(t, srv, done) })

	counter := &countingClient{opened: make(chan *Conn, 1), closed: make(chan CloseEvent, 1)}
	opts := heartbeat
	opts.Handler, opts.TLSConfig = counter, clientTLS
	dialer := &Dialer{Options: opts}
	t.Cleanup(func() { _ = dialer.Close(nil) })
	ctx, cancel := context.WithTimeout(context.Background(), testIOTimeout())
	defer cancel()
	if err := dialer.Dial(ctx, "wss://"+addr+"/", nil); err != nil {
		t.Fatal(err)
	}
	conn := wssWait(t, counter.opened, "the client's open")
	payload := make([]byte, 1024)
	sent := 0
	closedWith := func() string {
		select {
		case event := <-counter.closed:
			return fmt.Sprintf("the client closed with %d %q", event.Code, event.Reason)
		case event := <-serverClosed:
			return fmt.Sprintf("the server closed with %d %q", event.Code, event.Reason)
		case <-time.After(time.Second):
			return "no close reported"
		}
	}
	for began := time.Now(); time.Since(began) < 2*time.Second; {
		// Pipelined, with at most 512 messages in flight.
		for sent-int(counter.got.Load()) < 512 {
			if err := conn.SendBinary(payload); err != nil {
				t.Fatalf("send %d: %v; %s", sent, err, closedWith())
			}
			sent++
		}
		time.Sleep(time.Millisecond)
	}
	deadline := time.Now().Add(testIOTimeout())
	for counter.got.Load() < int64(sent) {
		select {
		case event := <-counter.closed:
			t.Fatalf("the client closed with %d %q after %d of %d echoes", event.Code, event.Reason, counter.got.Load(), sent)
		case event := <-serverClosed:
			t.Fatalf("the server closed with %d %q", event.Code, event.Reason)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d of %d echoes", counter.got.Load(), sent)
		}
		time.Sleep(time.Millisecond)
	}
}

type echoUntilClose struct{ closed chan CloseEvent }

func (echoUntilClose) OnOpen(*Conn) {}
func (echoUntilClose) OnMessage(c *Conn, m Message) {
	_ = c.SendBinary(m.Bytes())
}
func (e echoUntilClose) OnClose(_ *Conn, event CloseEvent) {
	select {
	case e.closed <- event:
	default:
	}
}

type countingClient struct {
	opened chan *Conn
	closed chan CloseEvent
	got    atomic.Int64
}

func (c *countingClient) OnOpen(conn *Conn)        { c.opened <- conn }
func (c *countingClient) OnMessage(*Conn, Message) { c.got.Add(1) }
func (c *countingClient) OnClose(_ *Conn, e CloseEvent) {
	select {
	case c.closed <- e:
	default:
	}
}

// startPlain serves an echoing plain ws server and returns its address.
func startPlain(t *testing.T) string {
	t.Helper()
	server, addr, serveDone := startTestServer(t, func() *Server {
		return NewServer(Options{Handler: newWSSRecorder()})
	})
	t.Cleanup(func() {
		_ = server.Close(nil)
		<-serveDone
	})
	return addr
}

// One Dialer speaks both schemes: the URL decides this connection's chain — a
// TLS layer for wss://, the layer that changes nothing for ws:// — so the
// choice is per dial, not per Dialer.
func TestDialerMixedSchemes(t *testing.T) {
	serverTLS, clientTLS := wssConfigs(t)
	wssAddr := startWSS(t, serverTLS)
	plainAddr := startPlain(t)

	// quietRecorder, not the echoing server recorder: an echoing client
	// handler ping-pongs every echo back until the close, and under load
	// the two queues fill and a blocked turn holds the shutdown up.
	client := quietRecorder{newWSSRecorder()}
	dialer := &Dialer{Options: Options{Handler: client, TLSConfig: clientTLS}}
	t.Cleanup(func() { _ = dialer.Close(nil) })
	ctx, cancel := context.WithTimeout(context.Background(), testIOTimeout())
	defer cancel()

	if err := dialer.Dial(ctx, "wss://"+wssAddr+"/", nil); err != nil {
		t.Fatal(err)
	}
	waitMessage := func(want string) {
		t.Helper()
		deadline := time.After(testIOTimeout())
		for {
			select {
			case got := <-client.got:
				if got == want {
					return
				}
			case <-deadline:
				t.Fatalf("no %q echo within %v", want, testIOTimeout())
			}
		}
	}
	secure := wssWait(t, client.opened, "the wss connection")
	if err := secure.SendText("secure"); err != nil {
		t.Fatal(err)
	}
	waitMessage("secure")

	// The same Dialer, in the clear this time.
	if err := dialer.Dial(ctx, "ws://"+plainAddr+"/", nil); err != nil {
		t.Fatal(err)
	}
	clear := wssWait(t, client.opened, "the ws connection")
	if err := clear.SendText("clear"); err != nil {
		t.Fatal(err)
	}
	waitMessage("clear")
	_ = secure.Close(1000, "done")
	_ = clear.Close(1000, "done")
}

// signal delivers v to ch without blocking: a callback that signals must not
// hold up the turn that runs it.
func signal[T any](ch chan<- T, v T) {
	select {
	case ch <- v:
	default:
	}
}
