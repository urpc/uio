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
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/urpc/uio"
)

// startServer serves opts on addrs and returns once every listener bound.
func startServer(t *testing.T, opts Options, addrs ...string) *Server {
	t.Helper()
	srv := NewServer(opts)
	done := make(chan error, 1)
	go func() { done <- srv.Serve(addrs...) }()
	t.Cleanup(func() {
		_ = srv.Close()
		select {
		case <-done:
		case <-time.After(testWait):
			t.Error("Serve did not return")
		}
	})
	deadline := time.Now().Add(testWait)
	for len(srv.Addrs()) < len(addrs) {
		select {
		case err := <-done:
			done <- err // the cleanup hears it too
			t.Fatalf("Serve: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("the server did not bind")
		}
		time.Sleep(2 * time.Millisecond)
	}
	return srv
}

func dialStandalone(t *testing.T, dialer *Dialer, addr string) uio.Conn {
	t.Helper()
	conn, err := dialer.Dial(addr)
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func expectEcho(t *testing.T, conn uio.Conn, client *collector, size int) {
	t.Helper()
	payload := payloadBytes(size)
	for off := 0; off < len(payload); off += 1024 {
		if _, err := conn.Write(payload[off : off+1024]); err != nil {
			t.Fatalf("write at %d: %v", off, err)
		}
	}
	waitUntil(t, "the echo", func() bool { return client.count() >= len(payload) })
	if got := client.bytes(); !bytes.Equal(got[:len(payload)], payload) {
		t.Fatalf("echo corrupted: %d bytes", len(got))
	}
}

// The standalone server and dialer carry an echo over UDP.
func TestStandaloneEcho(t *testing.T) {
	srv := startServer(t, Options{Handler: echoHandler, Pollers: 2}, "udp://127.0.0.1:0")
	client := newCollector()
	dialer := NewDialer(Options{Handler: client.handler()})
	t.Cleanup(func() { _ = dialer.Close() })

	conn := dialStandalone(t, dialer, "udp://"+srv.Addrs()[0].String())
	expectEcho(t, conn, client, 64<<10)
}

// An address without a scheme dials over UDP, the network KCP runs on.
func TestStandaloneSchemeDefault(t *testing.T) {
	srv := startServer(t, Options{Handler: echoHandler}, "127.0.0.1:0")
	client := newCollector()
	dialer := &Dialer{Options: Options{Handler: client.handler()}}
	t.Cleanup(func() { _ = dialer.Close() })

	conn := dialStandalone(t, dialer, srv.Addrs()[0].String())
	expectEcho(t, conn, client, 8<<10)
}

// The standalone pair runs over TCP as well.
func TestStandaloneTCP(t *testing.T) {
	srv := startServer(t, Options{Handler: echoHandler}, "tcp://127.0.0.1:0")
	client := newCollector()
	dialer := &Dialer{Options: Options{Handler: client.handler()}}
	t.Cleanup(func() { _ = dialer.Close() })

	conn := dialStandalone(t, dialer, "tcp://"+srv.Addrs()[0].String())
	expectEcho(t, conn, client, 16<<10)
}

// One dialer serves several sessions through one transport.
func TestDialerSharesTransport(t *testing.T) {
	srv := startServer(t, Options{Handler: echoHandler}, "udp://127.0.0.1:0")
	client := newCollector()
	dialer := &Dialer{Options: Options{Handler: client.handler()}}
	t.Cleanup(func() { _ = dialer.Close() })
	addr := "udp://" + srv.Addrs()[0].String()

	first := dialStandalone(t, dialer, addr)
	if _, err := first.Write([]byte("first-stream")); err != nil {
		t.Fatal(err)
	}
	second := dialStandalone(t, dialer, addr)
	if _, err := second.Write([]byte("second-stream")); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "both echoes", func() bool {
		got := client.bytes()
		return bytes.Contains(got, []byte("first-stream")) && bytes.Contains(got, []byte("second-stream"))
	})
}

// The server serves once; a close stops it for good.
func TestServerLifecycle(t *testing.T) {
	srv := startServer(t, Options{Handler: echoHandler}, "udp://127.0.0.1:0")

	if err := srv.Serve("udp://127.0.0.1:0"); !errors.Is(err, ErrServerStarted) {
		t.Fatalf("second Serve: %v, want ErrServerStarted", err)
	}
	if err := srv.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := srv.Serve("udp://127.0.0.1:0"); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("Serve after Close: %v, want net.ErrClosed", err)
	}
	if err := srv.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

// A closed dialer refuses to dial again.
func TestDialerCloseStopsDials(t *testing.T) {
	srv := startServer(t, Options{Handler: echoHandler}, "udp://127.0.0.1:0")
	client := newCollector()
	dialer := &Dialer{Options: Options{Handler: client.handler()}}
	addr := "udp://" + srv.Addrs()[0].String()

	conn := dialStandalone(t, dialer, addr)
	expectEcho(t, conn, client, 4<<10)

	if err := dialer.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), testWait)
	defer cancel()
	if _, err := dialer.DialContext(ctx, addr); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("dial after Close: %v, want net.ErrClosed", err)
	}
	if err := dialer.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

// The standalone endpoints refuse what no session can run with.
func TestStandaloneValidation(t *testing.T) {
	cases := []struct {
		name  string
		opts  Options
		addrs []string
	}{
		{"no handler", Options{}, []string{"udp://127.0.0.1:0"}},
		{"no address", Options{Handler: echoHandler}, nil},
		{"mtu above the engine's", Options{Handler: echoHandler, MTU: maxMTU + 1}, []string{"udp://127.0.0.1:0"}},
		{"read buffer below the mtu", Options{Handler: echoHandler, MaxBufferSize: 512}, []string{"udp://127.0.0.1:0"}},
	}
	for _, tc := range cases {
		if err := NewServer(tc.opts).Serve(tc.addrs...); !errors.Is(err, ErrInvalidOptions) {
			t.Fatalf("%s: %v, want ErrInvalidOptions", tc.name, err)
		}
	}

	dialer := &Dialer{}
	if _, err := dialer.DialContext(context.Background(), "udp://127.0.0.1:1"); !errors.Is(err, ErrInvalidOptions) {
		t.Fatalf("dial without a handler: %v, want ErrInvalidOptions", err)
	}
}
