/*
 * Copyright 2024 the urpc project
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *      https://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package ukcp

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urpc/uio"
	"github.com/urpc/uio/utls"
	"github.com/urpc/uio/uws"
)

// testTLSConfigs builds a self-signed pair for 127.0.0.1 and localhost.
func testTLSConfigs(t *testing.T) (*tls.Config, *tls.Config) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "ukcp.test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	server := &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
	client := &tls.Config{RootCAs: pool, ServerName: "127.0.0.1"}
	return server, client
}

// A KCP stream carries every layer above it on either bottom: TLS over KCP
// over UDP or TCP, and KCP itself over TLS over TCP.
func TestComposedEchoMatrix(t *testing.T) {
	serverTLS, clientTLS := testTLSConfigs(t)
	serverKCP := func() uio.Layer { return ServerLayer(Options{}) }
	clientKCP := func() uio.Layer { return ClientLayer(Options{}) }

	cases := []struct {
		name    string
		network string
		server  []uio.Layer
		client  []uio.Layer
	}{
		{"udp_kcp", "udp", []uio.Layer{serverKCP()}, []uio.Layer{clientKCP()}},
		{"tcp_kcp", "tcp", []uio.Layer{serverKCP()}, []uio.Layer{clientKCP()}},
		{"udp_kcp_tls", "udp",
			[]uio.Layer{serverKCP(), utls.Server(serverTLS)},
			[]uio.Layer{clientKCP(), utls.Client(clientTLS)}},
		{"tcp_kcp_tls", "tcp",
			[]uio.Layer{serverKCP(), utls.Server(serverTLS)},
			[]uio.Layer{clientKCP(), utls.Client(clientTLS)}},
		{"tcp_tls_kcp", "tcp",
			[]uio.Layer{utls.Server(serverTLS), serverKCP()},
			[]uio.Layer{utls.Client(clientTLS), clientKCP()}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := newServer(t, echoHandler, tc.server...)
			ln, err := server.Listen(tc.network + "://127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = ln.Close() })

			client := newCollector()
			conn, _ := dialKCP(t, client.handler(), tc.network+"://"+ln.Addr().String(), tc.client...)

			payload := payloadBytes(64 << 10)
			for off := 0; off < len(payload); off += 1024 {
				if _, err := conn.Write(payload[off : off+1024]); err != nil {
					t.Fatalf("write at %d: %v", off, err)
				}
			}
			waitUntil(t, "the echo to return", func() bool { return client.count() >= len(payload) })
			if got := client.bytes(); !bytes.Equal(got[:len(payload)], payload) {
				t.Fatalf("echo corrupted: %d bytes", len(got))
			}
		})
	}
}

// TLS records survive a lossy KCP link: every byte crosses, dropped segments
// and all.
func TestComposedEchoThroughLoss(t *testing.T) {
	serverTLS, clientTLS := testTLSConfigs(t)
	server := newServer(t, echoHandler, ServerLayer(Options{}), utls.Server(serverTLS))
	ln, err := server.Listen("udp://127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	proxy := newLossyProxy(t, ln.Addr().String(), 3)
	client := newCollector()
	conn, _ := dialKCP(t, client.handler(), "udp://"+proxy.addr(),
		ClientLayer(Options{}), utls.Client(clientTLS))

	payload := payloadBytes(64 << 10)
	for off := 0; off < len(payload); off += 1024 {
		if _, err := conn.Write(payload[off : off+1024]); err != nil {
			t.Fatalf("write at %d: %v", off, err)
		}
	}
	waitUntil(t, "the echo across the lossy link", func() bool { return client.count() >= len(payload) })
	if got := client.bytes(); !bytes.Equal(got[:len(payload)], payload) {
		t.Fatalf("echo corrupted: %d bytes", len(got))
	}
	if proxy.dropped.Load() == 0 {
		t.Fatal("the proxy dropped nothing: the lossy path was not exercised")
	}
}

// Reordered datagrams are put back in order by the engine.
func TestComposedEchoThroughReorder(t *testing.T) {
	server := newServer(t, echoHandler, ServerLayer(Options{}))
	ln, err := server.Listen("udp://127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	proxy := newLossyProxy(t, ln.Addr().String(), 0)
	proxy.reorderEvery.Store(4) // every fourth packet arrives behind the next
	client := newCollector()
	conn, _ := dialKCP(t, client.handler(), "udp://"+proxy.addr(), ClientLayer(Options{}))

	payload := payloadBytes(64 << 10)
	for off := 0; off < len(payload); off += 1024 {
		if _, err := conn.Write(payload[off : off+1024]); err != nil {
			t.Fatalf("write at %d: %v", off, err)
		}
	}
	waitUntil(t, "the echo across the reordering link", func() bool { return client.count() >= len(payload) })
	if got := client.bytes(); !bytes.Equal(got[:len(payload)], payload) {
		t.Fatalf("echo corrupted: %d bytes", len(got))
	}
	if proxy.reordered.Load() == 0 {
		t.Fatal("no packet was reordered: the path was not exercised")
	}
}

// A packet endpoint still refuses a chain whose bottom cannot take packets,
// TLS's included.
func TestPacketEndpointRefusesTLSBottom(t *testing.T) {
	serverTLS, _ := testTLSConfigs(t)
	server := newServer(t, echoHandler)
	if _, err := server.Events.Listen("udp://127.0.0.1:0", uio.WithLayers(utls.Server(serverTLS))); !errors.Is(err, uio.ErrLayersNeedStream) {
		t.Fatalf("udp Listen with TLS at the bottom: %v, want ErrLayersNeedStream", err)
	}
}

// A half close crosses both layers: TLS's close_notify rides KCP's end of
// stream to the peer.
func TestHalfCloseThroughKCPAndTLS(t *testing.T) {
	serverTLS, clientTLS := testTLSConfigs(t)
	sawEOF := make(chan struct{}, 1)
	// The failure this test once flaked with — CloseWrite reporting a closed
	// connection — means the stack had begun closing; these carry who ended
	// it and why into the failure.
	serverSaid := make(chan error, 1)   // what the Handler's Read returned
	serverClosed := make(chan error, 1) // the server connection's close cause
	var serverConnMu sync.Mutex
	var serverConn uio.Conn
	server := newServer(t, uio.FuncHandler{
		Read: func(conn uio.Conn) error {
			serverConnMu.Lock()
			serverConn = conn
			serverConnMu.Unlock()
			buf := make([]byte, 64<<10)
			for {
				n, err := conn.Read(buf)
				if n > 0 {
					continue
				}
				if err != nil {
					if errors.Is(err, io.EOF) {
						select {
						case sawEOF <- struct{}{}:
						default:
						}
					}
					select {
					case serverSaid <- err:
					default:
					}
					return err
				}
				return nil
			}
		},
		Close: func(_ uio.Conn, err error) {
			select {
			case serverClosed <- err:
			default:
			}
		},
	}, ServerLayer(Options{}), utls.Server(serverTLS))
	ln, err := server.Listen("udp://127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	client := newCollector()
	conn, _ := dialKCP(t, client.handler(), "udp://"+ln.Addr().String(),
		ClientLayer(Options{}), utls.Client(clientTLS))

	if _, err := conn.Write([]byte("bye")); err != nil {
		t.Fatal(err)
	}
	if err := conn.CloseWrite(); err != nil {
		var why []string
		select {
		case cerr := <-client.closed:
			why = append(why, fmt.Sprintf("client conn closed with %v", cerr))
		case <-time.After(100 * time.Millisecond):
			why = append(why, "client conn still open")
		}
		select {
		case serr := <-serverSaid:
			why = append(why, fmt.Sprintf("server handler returned %v", serr))
		default:
			why = append(why, "server handler silent")
		}
		select {
		case serr := <-serverClosed:
			why = append(why, fmt.Sprintf("server conn closed with %v", serr))
		default:
			why = append(why, "server conn open")
		}
		if s, ok := uio.FindLayer[interface{ debugLifecycle() string }](conn); ok {
			why = append(why, "client kcp: "+s.debugLifecycle())
		}
		serverConnMu.Lock()
		sc := serverConn
		serverConnMu.Unlock()
		if sc != nil {
			if s, ok := uio.FindLayer[interface{ debugLifecycle() string }](sc); ok {
				why = append(why, "server kcp: "+s.debugLifecycle())
			}
		}
		t.Fatalf("CloseWrite: %v (%s)", err, strings.Join(why, "; "))
	}
	select {
	case <-sawEOF:
	case <-time.After(testWait):
		t.Fatal("the peer never saw the end of the stream")
	}
}

// Any stream protocol rides KCP: a WebSocket handshake and messages over
// KCP over UDP.
func TestUWSOverKCP(t *testing.T) {
	server := newServer(t, echoHandler, ServerLayer(Options{}), uws.ServerLayer(uws.LayerOptions{}))
	ln, err := server.Listen("udp://127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	client := newCollector()
	target := "ws://" + ln.Addr().String() + "/"
	conn, _ := dialKCP(t, client.handler(), "udp://"+ln.Addr().String(),
		ClientLayer(Options{}), uws.ClientLayer(target, uws.LayerOptions{}))

	payload := payloadBytes(32 << 10)
	for off := 0; off < len(payload); off += 1024 {
		if _, err := conn.Write(payload[off : off+1024]); err != nil {
			t.Fatalf("write at %d: %v", off, err)
		}
	}
	waitUntil(t, "the websocket echo", func() bool { return client.count() >= len(payload) })
	if got := client.bytes(); !bytes.Equal(got[:len(payload)], payload) {
		t.Fatalf("echo corrupted: %d bytes", len(got))
	}
}

// A KCP layer over another KCP layer reads the layer below as the byte stream
// it is, whatever the socket beneath carries.
func TestNestedKCP(t *testing.T) {
	server := newServer(t, echoHandler,
		ServerLayer(Options{}), ServerLayer(Options{}))
	ln, err := server.Listen("udp://127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	client := newCollector()
	conn, _ := dialKCP(t, client.handler(), "udp://"+ln.Addr().String(),
		ClientLayer(Options{}), ClientLayer(Options{}))

	payload := payloadBytes(16 << 10)
	for off := 0; off < len(payload); off += 1024 {
		if _, err := conn.Write(payload[off : off+1024]); err != nil {
			t.Fatalf("write at %d: %v", off, err)
		}
	}
	waitUntil(t, "the nested echo", func() bool { return client.count() >= len(payload) })
	if got := client.bytes(); !bytes.Equal(got[:len(payload)], payload) {
		t.Fatalf("nested echo corrupted: %d bytes", len(got))
	}
}

// A listener with a chain refuses DialPeer: the socket-level connection would
// bypass every layer.
func TestLayeredDialPeerRefused(t *testing.T) {
	server := newServer(t, echoHandler, ServerLayer(Options{}))
	ln, err := server.Listen("udp://127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	packets, ok := ln.(uio.PacketListener)
	if !ok {
		t.Fatal("a UDP listener is not a PacketListener")
	}
	addr := ln.Addr().(*net.UDPAddr).AddrPort()
	if _, err := packets.DialPeer(addr, nil); !errors.Is(err, uio.ErrUnsupported) {
		t.Fatalf("layered DialPeer = %v, want ErrUnsupported", err)
	}
}
