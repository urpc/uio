package uws

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	crand "crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math/big"
	"net"
	"testing"
	"time"

	"github.com/urpc/uio/ulayer"
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
			Layers:  []ulayer.Layer{utls.Server(serverTLS)},
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
		Layers:  []ulayer.Layer{utls.Client(clientTLS)},
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

// A client whose TLS layer trusts nothing still gets its connection, and
// the handshake's failure is the connection's close cause — the WebSocket
// handshake never begins.
func TestWSSRefusedCertificate(t *testing.T) {
	serverTLS, _ := wssConfigs(t)
	addr := startWSS(t, serverTLS)

	client := newWSSRecorder()
	dialer := &Dialer{Options: Options{
		Handler: client,
		Layers: []ulayer.Layer{utls.Client(&tls.Config{
			ServerName: "localhost", // trusts no certificate
		})},
	}}
	t.Cleanup(func() { _ = dialer.Close(nil) })
	ctx, cancel := context.WithTimeout(context.Background(), testIOTimeout())
	defer cancel()
	if err := dialer.Dial(ctx, "wss://"+addr+"/", nil); err != nil {
		t.Fatal(err) // the connection was made; the TLS handshake reports by close
	}
	select {
	case err := <-client.closed:
		if err == nil {
			t.Fatal("the refused handshake closed cleanly")
		}
	case <-time.After(testIOTimeout()):
		t.Fatal("the refused handshake never closed the connection")
	}
	select {
	case <-client.opened:
		t.Fatal("a refused TLS handshake still opened the WebSocket")
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
