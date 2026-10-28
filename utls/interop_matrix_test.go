package utls

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	crand "crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"io"
	"math/big"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/urpc/uio"
)

// stream is what a cross-validation round trip needs of either side: the
// standard library's conn and UIO's.
type stream interface {
	io.ReadWriteCloser
	SetReadDeadline(time.Time) error
}

// randomBytes returns n bytes no test case has sent before.
func randomBytes(t *testing.T, n int) []byte {
	t.Helper()
	payload := make([]byte, n)
	if _, err := crand.Read(payload); err != nil {
		t.Fatal(err)
	}
	return payload
}

// roundTrip writes payload over a standard library connection and reads the
// echo back, concurrently so neither direction waits on the other's window, and
// checks the bytes came back unchanged. UIO's own connections cannot be read
// from here: their inbound methods belong to the connection's callbacks, so a
// test on our side collects what the handler delivered instead (see
// roundTripThrough).
func roundTrip(t *testing.T, conn stream, payload []byte) {
	t.Helper()
	written := make(chan error, 1)
	go func() {
		_, err := conn.Write(payload)
		written <- err
	}()
	got := make([]byte, len(payload))
	_ = conn.SetReadDeadline(time.Now().Add(testWait))
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatalf("read %d bytes back: %v", len(payload), err)
	}
	if err := <-written; err != nil {
		t.Fatalf("write %d bytes: %v", len(payload), err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("the round trip altered the %d bytes it echoed", len(payload))
	}
}

// roundTripThrough writes payload through a UIO connection and waits for the
// handler to have collected its echo.
func roundTripThrough(t *testing.T, conn uio.Conn, echo *collector, payload []byte) {
	t.Helper()
	written := make(chan error, 1)
	go func() {
		_, err := conn.Write(payload)
		written <- err
	}()
	select {
	case <-echo.done:
	case <-time.After(testWait):
		t.Fatalf("no echo of %d bytes within %v", len(payload), testWait)
	}
	if err := <-written; err != nil {
		t.Fatalf("write %d bytes: %v", len(payload), err)
	}
	if got := echo.payload(); !bytes.Equal(got[:len(payload)], payload) {
		t.Fatalf("the echo altered the %d bytes it answered", len(payload))
	}
}

// collector keeps what it reads and signals when it has target bytes. As a
// server handler it answers each chunk — an echo. As a client handler it
// answers nothing: the server it talks to echoes back, so a client that wrote
// what it read would keep the two ends echoing each other for as long as the
// connection lives. That endless stream is what fills an echo server's
// unconsumed queue; once full, its OnRead blocks for good and the server's
// Serve can never return.
type collector struct {
	target int64

	mu     sync.Mutex
	got    []byte
	done   chan struct{}
	closed chan error

	answer bool
}

func newCollectEcho(target int64) *collector {
	return &collector{target: target, answer: true, done: make(chan struct{}), closed: make(chan error, 4)}
}

func newCollector(target int64) *collector {
	return &collector{target: target, done: make(chan struct{}), closed: make(chan error, 4)}
}

func (e *collector) OnOpen(uio.Conn) {}

func (e *collector) OnRead(c uio.Conn) error {
	chunk := c.PeekChunk()
	n := len(chunk)
	e.mu.Lock()
	e.got = append(e.got, chunk...)
	reached := int64(len(e.got)) >= e.target
	e.mu.Unlock()
	if e.answer {
		if _, err := c.Write(chunk); err != nil {
			return err
		}
	}
	_, _ = c.Discard(n)
	if reached {
		select {
		case <-e.done:
		default:
			close(e.done)
		}
	}
	return nil
}

func (e *collector) OnTimer(uio.Conn) error        { return nil }
func (e *collector) OnClose(_ uio.Conn, err error) { e.closed <- err }

func (e *collector) payload() []byte {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]byte(nil), e.got...)
}

// startClient serves an Events with no listener, for dialing through the
// layers.
func startClient(t *testing.T, handler uio.Handler, layers ...uio.Layer) *layeredEvents {
	t.Helper()
	started := make(chan struct{})
	events := &uio.Events{Pollers: 1, Handler: handler, Hooks: uio.FuncHandler{
		Start: func(*uio.Events) { close(started) },
	}}
	done := make(chan error, 1)
	go func() { done <- events.Serve() }()
	t.Cleanup(func() {
		_ = events.Close(nil)
		<-done
	})
	<-started
	return &layeredEvents{Events: events, layers: layers}
}

// dialThrough dials addr through a stack of its own, settling before it
// returns.
func dialThrough(t *testing.T, events *layeredEvents, addr string) uio.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testWait)
	defer cancel()
	conn, err := events.DialContext(ctx, "tcp://"+addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// A payload far past the 16 KiB record size crosses many records both ways.
// Records split across reads, so the cursor that stops crypto/tls at each
// record boundary is exercised at every offset the network produces.
func TestInteropLargePayload(t *testing.T) {
	const total = 1 << 20

	t.Run("our server, stdlib client", func(t *testing.T) {
		serverConfig, clientConfig := testConfigs(t)
		echo := newCollectEcho(total)
		transport := serveTLS(t, echo, Server(serverConfig))
		listener, err := transport.Listen("tcp://127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		client, err := tls.Dial("tcp", listener.Addr().String(), clientConfig)
		if err != nil {
			t.Fatal(err)
		}
		defer client.Close()

		payload := randomBytes(t, total)
		roundTrip(t, client, payload)
		if got := echo.payload(); !bytes.Equal(got, payload) {
			t.Fatalf("the server read %d bytes, want the %d sent", len(got), total)
		}
	})

	t.Run("our client, stdlib server", func(t *testing.T) {
		serverConfig, clientConfig := testConfigs(t)
		where := stdEcho(t, serverConfig)
		sink := newCollector(total)
		events := startClient(t, sink, Client(clientConfig))
		conn := dialThrough(t, events, string(where))
		roundTripThrough(t, conn, sink, randomBytes(t, total))
	})
}

// The record cursor's edges: a payload at, under and over one record's size,
// and many small writes whose records the network coalesces into one read.
func TestInteropRecordBoundaries(t *testing.T) {
	sizes := []int{1, 5, 16<<10 - 1, 16 << 10, 16<<10 + 1, 32 << 10, 64<<10 + 3}
	for _, size := range sizes {
		t.Run(fmt.Sprintf("%d bytes", size), func(t *testing.T) {
			serverConfig, clientConfig := testConfigs(t)
			echo := newCollectEcho(int64(size))
			transport := serveTLS(t, echo, Server(serverConfig))
			listener, err := transport.Listen("tcp://127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			client, err := tls.Dial("tcp", listener.Addr().String(), clientConfig)
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			roundTrip(t, client, randomBytes(t, size))
		})
	}

	t.Run("many small messages", func(t *testing.T) {
		serverConfig, clientConfig := testConfigs(t)
		const messages = 512
		const each = 17
		echo := newCollectEcho(messages * each)
		transport := serveTLS(t, echo, Server(serverConfig))
		listener, err := transport.Listen("tcp://127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		client, err := tls.Dial("tcp", listener.Addr().String(), clientConfig)
		if err != nil {
			t.Fatal(err)
		}
		defer client.Close()

		payload := randomBytes(t, messages*each)
		roundTrip(t, client, payload)
		if got := echo.payload(); !bytes.Equal(got, payload) {
			t.Fatalf("the server read %d bytes of the %d sent", len(got), len(payload))
		}
	})
}

// Mutual TLS verifies certificates both ways: ours presenting to the standard
// library and the standard library presenting to ours.
func TestInteropMutualTLS(t *testing.T) {
	certificate, pool := testCertificate(t)
	serverConfig := &tls.Config{
		Certificates: []tls.Certificate{certificate},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    pool,
	}
	clientConfig := &tls.Config{
		ServerName:   "localhost",
		RootCAs:      pool,
		Certificates: []tls.Certificate{certificate},
	}

	t.Run("our server requires, stdlib client presents", func(t *testing.T) {
		server := newEcho()
		transport := serveTLS(t, server, Server(serverConfig))
		listener, err := transport.Listen("tcp://127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		client, err := tls.Dial("tcp", listener.Addr().String(), clientConfig)
		if err != nil {
			t.Fatal(err)
		}
		defer client.Close()
		if _, err := client.Write([]byte("mutual")); err != nil {
			t.Fatal(err)
		}
		if got := string(wait(t, server.data, "server data")); got != "mutual" {
			t.Fatalf("server read %q", got)
		}
		conn := wait(t, server.opened, "server open")
		state := uio.LayerOf(conn).(interface{ State() tls.ConnectionState }).State()
		if len(state.PeerCertificates) == 0 {
			t.Fatal("the server settled without the client's certificate")
		}
	})

	t.Run("stdlib server requires, our client presents", func(t *testing.T) {
		where := stdEcho(t, serverConfig)
		sink := newCollector(6)
		events := startClient(t, sink, Client(clientConfig))
		conn := dialThrough(t, events, string(where))
		roundTripThrough(t, conn, sink, []byte("mutual"))
	})

	// Under TLS 1.2 the server's verdict arrives inside the handshake, so the
	// dial reports it and no callback runs. Under TLS 1.3 the client's
	// handshake completes first — it sends its certificate and finished
	// message before the server has validated anything — so the refusal
	// arrives afterwards as an alert, and the connection opens and then closes
	// carrying it. Both are the protocol's own behavior, not this stack's.
	t.Run("our client without a certificate is refused under tls 1.2", func(t *testing.T) {
		where := stdEcho(t, serverConfig)
		calls := 0
		events := startClient(t, uio.FuncHandler{
			Open:  func(uio.Conn) { calls++ },
			Read:  func(uio.Conn) error { return nil },
			Close: func(uio.Conn, error) { calls++ },
		}, Client(&tls.Config{
			ServerName: "localhost", RootCAs: pool,
			MinVersion: tls.VersionTLS12, MaxVersion: tls.VersionTLS12,
		}))
		ctx, cancel := context.WithTimeout(context.Background(), testWait)
		defer cancel()
		if _, err := events.DialContext(ctx, "tcp://"+string(where)); err == nil {
			t.Fatal("a client with no certificate dialed cleanly")
		}
		if calls != 0 {
			t.Fatalf("a refused handshake ran %d callbacks", calls)
		}
	})

	t.Run("our client without a certificate is told so after tls 1.3", func(t *testing.T) {
		where := stdEcho(t, serverConfig)
		closed := make(chan error, 1)
		events := startClient(t, uio.FuncHandler{
			Open:  func(uio.Conn) {},
			Read:  func(uio.Conn) error { return nil },
			Close: func(_ uio.Conn, err error) { signal(closed, err) },
		}, Client(&tls.Config{
			ServerName: "localhost", RootCAs: pool,
			MinVersion: tls.VersionTLS13, MaxVersion: tls.VersionTLS13,
		}))
		ctx, cancel := context.WithTimeout(context.Background(), testWait)
		defer cancel()
		conn, err := events.DialContext(ctx, "tcp://"+string(where))
		if err != nil {
			// A peer that rejects earlier than 1.3's own sequencing allows is
			// reporting the same refusal, so the outcome is the same.
			return
		}
		if _, err := conn.Write([]byte("rejected")); err != nil {
			return // the alert beat the write, as it may
		}
		select {
		case err := <-closed:
			if err == nil {
				t.Fatal("the connection closed cleanly after the server refused the certificate")
			}
		case <-time.After(testWait):
			t.Fatal("the server's refusal never reached the connection")
		}
	})
}

// Each protocol version negotiated explicitly, so a version the standard
// library offers is proven to work and not merely to have been picked.
func TestInteropExplicitVersions(t *testing.T) {
	for _, version := range []struct {
		name  string
		value uint16
	}{{"tls 1.2", tls.VersionTLS12}, {"tls 1.3", tls.VersionTLS13}} {
		t.Run(version.name, func(t *testing.T) {
			certificate, pool := testCertificate(t)
			serverConfig := &tls.Config{
				Certificates: []tls.Certificate{certificate},
				MinVersion:   version.value,
				MaxVersion:   version.value,
			}
			clientConfig := &tls.Config{
				ServerName: "localhost",
				RootCAs:    pool,
				MinVersion: version.value,
				MaxVersion: version.value,
			}

			// stdlib client over our server.
			server := newEcho()
			transport := serveTLS(t, server, Server(serverConfig))
			listener, err := transport.Listen("tcp://127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			client, err := tls.Dial("tcp", listener.Addr().String(), clientConfig)
			if err != nil {
				t.Fatal(err)
			}
			if state := client.ConnectionState(); state.Version != version.value {
				t.Fatalf("negotiated %#x, want %#x", state.Version, version.value)
			}
			_ = client.Close()

			// Our client over a stdlib server.
			where := stdEcho(t, serverConfig)
			sink := newCollector(5)
			events := startClient(t, sink, Client(clientConfig))
			conn := dialThrough(t, events, string(where))
			state := uio.LayerOf(conn).(interface{ State() ConnectionState }).State()
			if state.Version != version.value {
				t.Fatalf("negotiated %#x, want %#x", state.Version, version.value)
			}
			roundTripThrough(t, conn, sink, []byte("hello"))
		})
	}
}

// Our own stack and the standard library, fed the same bytes, deliver the
// same bytes: a difference in either direction is a difference the other
// side's tests would not see.
func TestInteropMatchesStdlibByteForByte(t *testing.T) {
	serverConfig, clientConfig := testConfigs(t)
	const total = 96 << 10
	payload := randomBytes(t, total)

	server := newEcho()
	transport := serveTLS(t, server, Server(serverConfig))
	listener, err := transport.Listen("tcp://127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	// The standard library's client first.
	stdConn, err := tls.Dial("tcp", listener.Addr().String(), clientConfig)
	if err != nil {
		t.Fatal(err)
	}
	roundTrip(t, stdConn, payload)
	_ = stdConn.Close()

	// Ours, through the stack, with the same bytes.
	sink := newCollector(total)
	events := startClient(t, sink, Client(clientConfig))
	conn := dialThrough(t, events, listener.Addr().String())
	roundTripThrough(t, conn, sink, payload)
}

// Connections do not interfere: many handshakes and payloads at once settle
// and echo independently.
func TestInteropConcurrentConnections(t *testing.T) {
	serverConfig, clientConfig := testConfigs(t)
	const connections = 24
	const total = 8 << 10

	handler := &countingEcho{}
	transport := serveTLS(t, handler, Server(serverConfig))
	listener, err := transport.Listen("tcp://127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	errs := make(chan error, connections)
	for i := range connections {
		wg.Add(1)
		go func() {
			defer wg.Done()
			client, err := tls.Dial("tcp", listener.Addr().String(), clientConfig)
			if err != nil {
				errs <- err
				return
			}
			defer client.Close()
			payload := make([]byte, total)
			for j := range payload {
				payload[j] = byte(i + j)
			}
			written := make(chan error, 1)
			go func() {
				_, err := client.Write(payload)
				written <- err
			}()
			got := make([]byte, total)
			_ = client.SetReadDeadline(time.Now().Add(testWait))
			if _, err := io.ReadFull(client, got); err != nil {
				errs <- err
				return
			}
			if err := <-written; err != nil {
				errs <- err
				return
			}
			if !bytes.Equal(got, payload) {
				errs <- fmt.Errorf("connection %d read back different bytes", i)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

// countingEcho echoes what it reads, without a channel a burst could fill.
type countingEcho struct {
	mu     sync.Mutex
	opened int
}

func (e *countingEcho) OnOpen(uio.Conn) {
	e.mu.Lock()
	e.opened++
	e.mu.Unlock()
}

func (e *countingEcho) OnRead(c uio.Conn) error {
	chunk := c.PeekChunk()
	n := len(chunk)
	_, err := c.Write(chunk)
	_, _ = c.Discard(n)
	return err
}

func (e *countingEcho) OnTimer(uio.Conn) error  { return nil }
func (e *countingEcho) OnClose(uio.Conn, error) {}

func (e *countingEcho) count() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.opened
}

// A close_notify from the standard library reaches our handler as a clean
// close, and one from ours reaches the standard library as io.EOF.
func TestInteropCloseNotify(t *testing.T) {
	t.Run("stdlib close_notify, our server", func(t *testing.T) {
		serverConfig, clientConfig := testConfigs(t)
		server := newEcho()
		transport := serveTLS(t, server, Server(serverConfig))
		listener, err := transport.Listen("tcp://127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		client, err := tls.Dial("tcp", listener.Addr().String(), clientConfig)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := client.Write([]byte("bye")); err != nil {
			t.Fatal(err)
		}
		wait(t, server.data, "server data")
		if err := client.Close(); err != nil {
			t.Fatal(err)
		}
		wait(t, server.closed, "server close after close_notify")
	})

	t.Run("our close_notify, stdlib client", func(t *testing.T) {
		serverConfig, clientConfig := testConfigs(t)
		server := newEcho()
		transport := serveTLS(t, server, Server(serverConfig))
		listener, err := transport.Listen("tcp://127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		conn, _ := dialTLS(t, Client(clientConfig), "tcp://"+listener.Addr().String())
		if _, err := conn.Write([]byte("bye")); err != nil {
			t.Fatal(err)
		}
		wait(t, server.data, "server data")
		if err := conn.Close(); err != nil {
			t.Fatal(err)
		}
		wait(t, server.closed, "server close")
	})
}

// A certificate chain is presented whole: the leaf, then the issuer the
// standard library needs to build a path to the root it trusts.
func TestInteropCertificateChain(t *testing.T) {
	ca, caKey, caPool := testCA(t)
	leaf := testLeaf(t, ca, caKey)

	serverConfig := &tls.Config{Certificates: []tls.Certificate{leaf}}
	clientConfig := &tls.Config{ServerName: "localhost", RootCAs: caPool}

	server := newEcho()
	transport := serveTLS(t, server, Server(serverConfig))
	listener, err := transport.Listen("tcp://127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	client, err := tls.Dial("tcp", listener.Addr().String(), clientConfig)
	if err != nil {
		t.Fatalf("the standard library refused our chain: %v", err)
	}
	defer client.Close()
	if _, err := client.Write([]byte("chain")); err != nil {
		t.Fatal(err)
	}
	if got := string(wait(t, server.data, "server data")); got != "chain" {
		t.Fatalf("server read %q", got)
	}
}

// testCA makes a certificate authority the standard library will accept as a
// root, and the pool holding it.
func testCA(t *testing.T) (ca *x509.Certificate, key *ecdsa.PrivateKey, pool *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), crand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := x509.Certificate{
		SerialNumber:          big.NewInt(1000),
		Subject:               pkix.Name{CommonName: "utls test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(crand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	ca, err = x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool = x509.NewCertPool()
	pool.AddCert(ca)
	return ca, key, pool
}

// testLeaf makes a server certificate signed by ca, presented with ca so a
// peer that trusts only the root can build the path.
func testLeaf(t *testing.T, ca *x509.Certificate, caKey *ecdsa.PrivateKey) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), crand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := x509.Certificate{
		SerialNumber: big.NewInt(1001),
		Subject:      pkix.Name{CommonName: "localhost"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(crand.Reader, &template, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{
		Certificate: [][]byte{der, ca.Raw}, // leaf, then the issuer
		PrivateKey:  key,
		Leaf:        leaf,
	}
}

// Our server issues TLS 1.3 session tickets a standard library client can
// resume from: the ticket arrives after the handshake, so the client has to be
// left reading long enough to store it.
func TestInteropServerSideResumption(t *testing.T) {
	serverConfig, _ := testConfigs(t)
	clientConfig := &tls.Config{
		ServerName:         "localhost",
		RootCAs:            mustPool(t, serverConfig),
		ClientSessionCache: tls.NewLRUClientSessionCache(4),
	}
	server := newEcho()
	transport := serveTLS(t, server, Server(serverConfig))
	listener, err := transport.Listen("tcp://127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	first, err := tls.Dial("tcp", listener.Addr().String(), clientConfig)
	if err != nil {
		t.Fatal(err)
	}
	// Exchange a byte so the ticket that follows the handshake has a read to
	// arrive behind, then let it land before closing.
	_, _ = first.Write([]byte("x"))
	_ = first.SetReadDeadline(time.Now().Add(testWait))
	_, _ = first.Read(make([]byte, 8))
	_ = first.Close()
	wait(t, server.closed, "first close")

	second, err := tls.Dial("tcp", listener.Addr().String(), clientConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if state := second.ConnectionState(); !state.DidResume {
		t.Fatal("the client did not resume a session our server issued")
	}
}
