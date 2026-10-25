package utls

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"testing"
	"time"

	"github.com/urpc/uio"
)

// stdEcho serves an echo over a standard crypto/tls listener.
func stdEcho(t *testing.T, config *tls.Config) addr {
	t.Helper()
	listener, err := tls.Listen("tcp", "127.0.0.1:0", config)
	if err != nil {
		t.Fatal(err)
	}
	// Cleanups run last-registered-first: the listener closes before the
	// loop that reads it is joined.
	done := make(chan struct{})
	t.Cleanup(func() { <-done })
	go func() {
		defer close(done)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				buf := make([]byte, 4096)
				for {
					n, err := conn.Read(buf)
					if err != nil {
						return
					}
					if _, err := conn.Write(buf[:n]); err != nil {
						return
					}
				}
			}()
		}
	}()
	t.Cleanup(func() { _ = listener.Close() })
	return addr(listener.Addr().String())
}

type addr string

// The stack speaks with the standard library on the wire, both ways, over
// TLS 1.3 and 1.2, with ALPN and session resumption.
func TestInteropWithStdlib(t *testing.T) {
	t.Run("our client over a stdlib server", func(t *testing.T) {
		serverConfig, clientConfig := testConfigs(t)
		where := stdEcho(t, serverConfig)
		conn, client := dialTLS(t, Client(clientConfig), "tcp://"+string(where))
		if _, err := conn.Write([]byte("interop")); err != nil {
			t.Fatal(err)
		}
		if got := string(wait(t, client.data, "client echo")); got != "interop" {
			t.Fatalf("client read %q back", got)
		}
		_ = conn.Close()
	})

	t.Run("stdlib client over our server", func(t *testing.T) {
		serverConfig, clientConfig := testConfigs(t)
		server := newEcho()
		transport := serveTLS(t, server, Server(serverConfig))
		listener, err := transport.Listen("tcp://127.0.0.1:0", nil)
		if err != nil {
			t.Fatal(err)
		}
		client, err := tls.Dial("tcp", listener.Addr().String(), clientConfig)
		if err != nil {
			t.Fatal(err)
		}
		defer client.Close()
		if _, err := client.Write([]byte("other way")); err != nil {
			t.Fatal(err)
		}
		if got := string(wait(t, server.data, "server data")); got != "other way" {
			t.Fatalf("server read %q", got)
		}
		answer := make([]byte, 64)
		client.SetReadDeadline(time.Now().Add(testWait))
		n, err := client.Read(answer)
		if err != nil {
			t.Fatal(err)
		}
		if string(answer[:n]) != "other way" {
			t.Fatalf("stdlib client read %q back", answer[:n])
		}
	})

	t.Run("tls 1.2", func(t *testing.T) {
		certificate, pool := testCertificate(t)
		serverConfig := &tls.Config{
			Certificates: []tls.Certificate{certificate},
			MaxVersion:   tls.VersionTLS12,
		}
		clientConfig := &tls.Config{
			ServerName: "localhost", RootCAs: pool,
			MaxVersion: tls.VersionTLS12,
		}
		server := newEcho()
		transport := serveTLS(t, server, Server(serverConfig))
		listener, err := transport.Listen("tcp://127.0.0.1:0", nil)
		if err != nil {
			t.Fatal(err)
		}
		client, err := tls.Dial("tcp", listener.Addr().String(), clientConfig)
		if err != nil {
			t.Fatal(err)
		}
		defer client.Close()
		if state := client.ConnectionState(); state.Version != tls.VersionTLS12 {
			t.Fatalf("negotiated %#x, want TLS 1.2", state.Version)
		}
		if _, err := client.Write([]byte("twelve")); err != nil {
			t.Fatal(err)
		}
		if got := string(wait(t, server.data, "server data")); got != "twelve" {
			t.Fatalf("server read %q", got)
		}
	})

	t.Run("alpn", func(t *testing.T) {
		serverConfig, _ := testConfigs(t)
		serverConfig.NextProtos = []string{"h2", "http/1.1"}
		clientConfig := &tls.Config{
			ServerName: "localhost",
			RootCAs:    mustPool(t, serverConfig),
			NextProtos: []string{"h2"},
		}
		server := newEcho()
		transport := serveTLS(t, server, Server(serverConfig))
		listener, err := transport.Listen("tcp://127.0.0.1:0", nil)
		if err != nil {
			t.Fatal(err)
		}
		conn, _ := dialTLS(t, Client(clientConfig), "tcp://"+listener.Addr().String())
		state := conn.(interface{ State() tls.ConnectionState }).State()
		if state.NegotiatedProtocol != "h2" {
			t.Fatalf("ALPN negotiated %q, want h2", state.NegotiatedProtocol)
		}
		_ = conn.Close()
	})

	t.Run("session resumption", func(t *testing.T) {
		serverConfig, _ := testConfigs(t)
		clientConfig := &tls.Config{
			ServerName:         "localhost",
			RootCAs:            mustPool(t, serverConfig),
			ClientSessionCache: tls.NewLRUClientSessionCache(4),
		}
		server := newEcho()
		transport := serveTLS(t, server, Server(serverConfig))
		listener, err := transport.Listen("tcp://127.0.0.1:0", nil)
		if err != nil {
			t.Fatal(err)
		}
		first, _ := dialTLS(t, Client(clientConfig), "tcp://"+listener.Addr().String())
		_ = first.Close()
		wait(t, server.closed, "first close")
		second, _ := dialTLS(t, Client(clientConfig), "tcp://"+listener.Addr().String())
		defer func() { _ = second.Close() }()
		state := second.(interface{ State() tls.ConnectionState }).State()
		if !state.DidResume {
			t.Fatal("the second handshake did not resume the first's session")
		}
	})

	t.Run("rejected certificate", func(t *testing.T) {
		serverConfig, _ := testConfigs(t)
		// A client that trusts nothing: every certificate is refused.
		clientConfig := &tls.Config{ServerName: "localhost", RootCAs: nil}
		server := newEcho()
		transport := serveTLS(t, server, Server(serverConfig))
		listener, err := transport.Listen("tcp://127.0.0.1:0", nil)
		if err != nil {
			t.Fatal(err)
		}
		_, err = tls.Dial("tcp", listener.Addr().String(), clientConfig)
		if err == nil {
			t.Fatal("a client that trusts nothing got in")
		}
		// And our own client, through the stack, reports it without a
		// callback.
		var callbacks int
		started := make(chan struct{})
		events := &uio.Events{Pollers: 1, Hooks: uio.FuncHandler{Start: func(*uio.Events) { close(started) }}}
		counting := uio.FuncHandler{
			Open:  func(uio.Conn) { callbacks++ },
			Data:  func(uio.Conn) error { return nil },
			Close: func(uio.Conn, error) { callbacks++ },
		}
		events.Handler = counting
		events.Use(Client(clientConfig))
		done := make(chan error, 1)
		go func() { done <- events.Serve() }()
		t.Cleanup(func() { _ = events.Close(nil); <-done })
		<-started
		ctx, cancel := context.WithTimeout(context.Background(), testWait)
		defer cancel()
		if _, err := events.DialContext(ctx, "tcp://"+listener.Addr().String(), nil); err == nil {
			t.Fatal("a refused certificate reported no error")
		}
		if callbacks != 0 {
			t.Fatalf("a failed handshake ran %d callbacks", callbacks)
		}
	})
}

func mustPool(t *testing.T, config *tls.Config) *x509.CertPool {
	t.Helper()
	pool := x509.NewCertPool()
	pool.AddCert(config.Certificates[0].Leaf)
	return pool
}

// TLS over TLS: the stack nests, and the inner handshake runs over the
// outer wrapper's connection as any stream does.
func TestTLSOverTLS(t *testing.T) {
	outerServer, outerClient := testConfigs(t)
	innerServer, innerClient := testConfigs(t)
	server := newEcho()
	transport := serveTLS(t, server, Server(outerServer), Server(innerServer))
	listener, err := transport.Listen("tcp://127.0.0.1:0", nil)
	if err != nil {
		t.Fatal(err)
	}
	client, echo := dialTLS(t, Client(outerClient), Client(innerClient), "tcp://"+listener.Addr().String())
	if _, err := client.Write([]byte("nested")); err != nil {
		t.Fatal(err)
	}
	if got := string(wait(t, echo.data, "echo through two TLS wrappers")); got != "nested" {
		t.Fatalf("read %q through TLS over TLS", got)
	}
	_ = client.Close()
	wait(t, server.closed, "server close")
}

// TLS runs over a wrapper with a handshake of its own: the stack settles one
// wrapper at a time, gate first, TLS over it.
func TestTLSOverGateWrapper(t *testing.T) {
	serverConfig, clientConfig := testConfigs(t)
	server := newEcho()
	transport := serveTLS(t, server, Server(serverConfig), gate)
	listener, err := transport.Listen("tcp://127.0.0.1:0", nil)
	if err != nil {
		t.Fatal(err)
	}
	client, echo := dialTLS(t, Client(clientConfig), uio.Middleware(gate), "tcp://"+listener.Addr().String())
	if _, err := client.Write([]byte("through the gate")); err != nil {
		t.Fatal(err)
	}
	if got := string(wait(t, echo.data, "echo through gate and TLS")); got != "through the gate" {
		t.Fatalf("read %q through gate and TLS", got)
	}
	_ = client.Close()
	wait(t, server.closed, "server close")
}
