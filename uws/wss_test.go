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
	"strings"
	"testing"
	"time"

	"github.com/urpc/uio"
	"github.com/urpc/uio/utls"
)

// wssConfigs makes a matching pair: a certificate for the server, a client
// that trusts it.
func wssConfigs(t *testing.T) (serverTLS *tls.Config, clientTLS *tls.Config) {
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

// startWSS serves an echoing wss server — a TLS layer under the WebSocket
// protocol — and returns its address.
func startWSS(t *testing.T, serverTLS *tls.Config) string {
	t.Helper()
	server, addr, serveDone := startTestServer(t, func() *Server {
		return NewServer(Options{
			Handler: newWSSRecorder(),
			Layers:  uio.Use(utls.Server(serverTLS)),
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

// A dialer with a TLS layer speaks wss to a server with one: the upgrade
// travels inside TLS, messages go both ways through it, and a close travels
// back as the close handshake.
func TestWSSRoundTrip(t *testing.T) {
	serverTLS, clientTLS := wssConfigs(t)
	addr := startWSS(t, serverTLS)

	client := newWSSRecorder()
	dialer := &Dialer{Options: Options{
		Handler: client,
		Layers:  uio.Use(utls.Client(clientTLS)),
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
	// The server echoes, and the client's own echo handler answers back.
	for i := 0; i < 2; i++ {
		if got := wssWait(t, client.got, "client message"); got != "secure hello" {
			t.Fatalf("message %d = %q", i, got)
		}
	}
	_ = conn.Close(1000, "done")
	select {
	case <-client.closed:
	case <-time.After(testIOTimeout()):
		t.Fatal("the client never closed")
	}
}

// A client whose TLS layer trusts nothing fails the dial: the handshake's
// failure is what Dial reports, the WebSocket handshake never begins, and the
// Handler hears nothing of the connection.
func TestWSSRefusedCertificate(t *testing.T) {
	serverTLS, _ := wssConfigs(t)
	addr := startWSS(t, serverTLS)

	client := newWSSRecorder()
	dialer := &Dialer{Options: Options{
		Handler: client,
		Layers: uio.Use(utls.Client(&tls.Config{
			ServerName: "localhost", // trusts no certificate
		})),
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

// wss without a secure layer is a configuration error, before anything is
// dialed.
func TestWSSNeedsALayer(t *testing.T) {
	dialer := &Dialer{}
	t.Cleanup(func() { _ = dialer.Close(nil) })
	err := dialer.Dial(context.Background(), "wss://127.0.0.1:1/", nil)
	if !errors.Is(err, ErrInvalidOptions) {
		t.Fatalf("Dial wss without a layer = %v, want ErrInvalidOptions", err)
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
		Layers:                uio.Use(utls.Client(clientTLS)),
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
// carry no plaintext.
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
	dialer := NewDialer(Options{
		Handler:               watcher,
		Pollers:               1,
		HeartbeatInterval:     10 * time.Millisecond,
		HeartbeatTimeout:      60 * time.Millisecond,
		OutboundHighWatermark: -1,
		MaxOutboundBuffered:   64 << 10,
		Layers:                uio.Use(utls.Client(clientTLS)),
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
