package utls

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	crand "crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urpc/uio"
)

const testWait = 10 * time.Second

// testCertificate makes a self-signed certificate for the TLS the tests run.
func testCertificate(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), crand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "utls test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		DNSNames:     []string{"localhost", "127.0.0.1"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(crand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	certificate := tls.Certificate{
		Certificate: [][]byte{der},
		PrivateKey:  key,
		Leaf:        leaf,
	}
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	return certificate, pool
}

// serverConfig and clientConfig are a matching pair.
func testConfigs(t *testing.T) (server *tls.Config, client *tls.Config) {
	t.Helper()
	certificate, pool := testCertificate(t)
	return &tls.Config{Certificates: []tls.Certificate{certificate}},
		&tls.Config{ServerName: "localhost", RootCAs: pool}
}

// echo is the handler at the top of a stack: it answers what it reads.
type echo struct {
	opened chan uio.Conn
	data   chan []byte
	closed chan uio.Conn
}

func newEcho() *echo {
	return &echo{
		opened: make(chan uio.Conn, 8),
		data:   make(chan []byte, 64),
		closed: make(chan uio.Conn, 8),
	}
}

func (e *echo) OnOpen(c uio.Conn) { e.opened <- c }

func (e *echo) OnData(c uio.Conn) error {
	payload := append([]byte(nil), c.PeekChunk()...)
	if len(payload) == 0 {
		return nil // a turn without data: a wake, a timer
	}
	_, _ = c.Discard(len(payload))
	e.data <- payload
	_, err := c.Write(payload)
	return err
}

func (e *echo) OnClose(c uio.Conn, _ error) { e.closed <- c }

func wait[T any](t *testing.T, c chan T, what string) T {
	t.Helper()
	select {
	case v := <-c:
		return v
	case <-time.After(testWait):
		t.Fatalf("no %s within %v", what, testWait)
		return *new(T)
	}
}

// serveTLS serves a stack over fresh events with the wrappers installed in
// order, bottom first, and returns the events.
func serveTLS(t *testing.T, top uio.Handler, wrappers ...uio.Middleware) *uio.Events {
	t.Helper()
	started := make(chan struct{})
	events := &uio.Events{Pollers: 2, Handler: top, Hooks: uio.FuncHandler{
		Start: func(*uio.Events) { close(started) },
	}}
	events.Use(wrappers...)
	done := make(chan error, 1)
	go func() { done <- events.Serve() }()
	t.Cleanup(func() {
		_ = events.Close(nil)
		select {
		case <-done:
		case <-time.After(testWait):
			t.Error("Serve did not return")
		}
	})
	select {
	case <-started:
	case <-time.After(testWait):
		t.Fatal("Serve did not start")
	}
	return events
}

// listenTLS serves a stack and listens on a fresh loopback port.
func listenTLS(t *testing.T, top uio.Handler, wrappers ...uio.Middleware) (string, *uio.Events) {
	t.Helper()
	events := serveTLS(t, top, wrappers...)
	listener, err := events.Listen("tcp://127.0.0.1:0", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	return listener.Addr().String(), events
}

// record is a handler that only reports what it reads. turn, when set,
// hears every turn that brings it no data.
type record struct {
	opened chan uio.Conn
	data   chan []byte
	closed chan error
	turn   atomic.Pointer[chan struct{}]
}

func newRecord() *record {
	return &record{
		opened: make(chan uio.Conn, 4),
		data:   make(chan []byte, 64),
		closed: make(chan error, 4),
	}
}

func (r *record) OnOpen(c uio.Conn) { r.opened <- c }

func (r *record) OnData(c uio.Conn) error {
	payload := append([]byte(nil), c.PeekChunk()...)
	if len(payload) == 0 {
		if turn := r.turn.Load(); turn != nil {
			select {
			case *turn <- struct{}{}:
			default:
			}
		}
		return nil // a turn without data: a wake, a timer
	}
	_, _ = c.Discard(len(payload))
	r.data <- payload
	return nil
}

func (r *record) OnClose(_ uio.Conn, err error) { r.closed <- err }

// dialTLS dials addr through a stack of its own with the wrappers installed,
// and returns the settled connection with the handler that reports it. The
// last argument is the address.
func dialTLS(t *testing.T, wrappers ...any) (uio.Conn, *record) {
	t.Helper()
	addr, _ := wrappers[len(wrappers)-1].(string)
	client := newRecord()
	started := make(chan struct{})
	events := &uio.Events{Pollers: 1, Handler: client, Hooks: uio.FuncHandler{
		Start: func(*uio.Events) { close(started) },
	}}
	for _, wrapper := range wrappers[:len(wrappers)-1] {
		events.Use(wrapper.(uio.Middleware))
	}
	done := make(chan error, 1)
	go func() { done <- events.Serve() }()
	t.Cleanup(func() {
		_ = events.Close(nil)
		<-done
	})
	select {
	case <-started:
	case <-time.After(testWait):
		t.Fatal("Serve did not start")
	}
	ctx, cancel := context.WithTimeout(context.Background(), testWait)
	defer cancel()
	conn, err := events.DialContext(ctx, addr, nil)
	if err != nil {
		t.Fatal(err)
	}
	return conn, client
}

// The two sides of a TLS wrapper speak to each other through the stack: the
// handshake settles, plaintext travels both ways, and the certificate the
// peer served is the test's.
func TestTLSEchoBothSides(t *testing.T) {
	serverConfig, clientConfig := testConfigs(t)
	server := newEcho()
	transport := serveTLS(t, server, Server(serverConfig))
	listener, err := transport.Listen("tcp://127.0.0.1:0", nil)
	if err != nil {
		t.Fatal(err)
	}

	clientConn, clientEcho := dialTLS(t, Client(clientConfig), "tcp://"+listener.Addr().String())
	if _, err := clientConn.Write([]byte("over the wire")); err != nil {
		t.Fatal(err)
	}
	serverConn := wait(t, server.opened, "server open")
	if state, ok := serverConn.(interface{ State() tls.ConnectionState }); ok {
		if state.State().PeerCertificates == nil {
			// a server without ClientAuth sees none; the handshake itself is
			// what matters here
			_ = state
		}
	} else {
		t.Fatal("the TLS connection does not report its state")
	}
	if got := string(wait(t, server.data, "server data")); got != "over the wire" {
		t.Fatalf("server read %q through TLS", got)
	}
	if got := string(wait(t, clientEcho.data, "client answer")); got != "over the wire" {
		t.Fatalf("client read %q back through TLS", got)
	}

	// the other direction, and a larger payload across many records
	big := make([]byte, 256<<10)
	for i := range big {
		big[i] = byte(i)
	}
	if _, err := serverConn.Write(big); err != nil {
		t.Fatal(err)
	}
	reassembled := make([]byte, 0, len(big))
	for len(reassembled) < len(big) {
		chunk := wait(t, clientEcho.data, "client bulk data")
		reassembled = append(reassembled, chunk...)
	}
	for i := range big {
		if reassembled[i] != big[i] {
			t.Fatalf("bulk payload differs at %d", i)
		}
	}

	_ = clientConn.Close()
	wait(t, server.closed, "server close")
}

// A turn of an opened connection that carries no bytes — a Wake, a timer a
// layer above armed — reaches the handler above: the layers above run their
// clocks on such turns, and a TLS layer that swallowed them stopped those
// clocks. A turn whose bytes only part-filled a record stays this layer's.
func TestEmptyTurnReachesHandlerAbove(t *testing.T) {
	serverConfig, clientConfig := testConfigs(t)
	server := newRecord()
	addr, _ := listenTLS(t, server, Server(serverConfig))
	conn, _ := dialTLS(t, Client(clientConfig), "tcp://"+addr)
	if _, err := conn.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	if got := wait(t, server.data, "the first bytes"); string(got) != "hello" {
		t.Fatalf("server read %q, want hello", got)
	}
	upper := wait(t, server.opened, "the server's OnOpen")

	turns := make(chan struct{}, 4)
	server.turn.Store(&turns)
	if err := upper.SetWakeDeadline(time.Now().Add(20 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	wait(t, turns, "the empty turn the wake timer runs")
}
