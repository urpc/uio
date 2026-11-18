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

package usocks

import (
	"bytes"
	"encoding/binary"
	"net"
	"testing"
	"time"

	"github.com/urpc/uio"
)

func startEchoOn(t *testing.T, listen string) string {
	t.Helper()
	started := make(chan struct{})
	ev := &uio.Events{
		Pollers: 1,
		Handler: uio.FuncHandler{Read: func(c uio.Conn) error {
			_, err := c.WriteTo(c)
			return err
		}},
		Hooks: uio.FuncHandler{Start: func(*uio.Events) { close(started) }},
	}
	go ev.Serve()
	t.Cleanup(func() { _ = ev.Close(nil) })
	<-started
	ln, err := ev.Listen(listen)
	if err != nil {
		t.Fatal(err)
	}
	return ln.Addr().String()
}

// Configuring an Authenticator makes auth mandatory: a client offering only
// no-auth is refused, not downgraded (the authentication-bypass regression).
func TestAuthDowngradeRefused(t *testing.T) {
	socks := startSocksOpts(t, Options{Authenticator: &fakeAuth{}})
	c, err := net.Dial("tcp", socks)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.Write([]byte{Version5, 1, MethodNoAuth}); err != nil {
		t.Fatal(err)
	}
	m := readN(t, c, 2)
	if m[0] != Version5 || m[1] != MethodNoAcceptable {
		t.Fatalf("method reply = % x, want no-acceptable (auth mandatory)", m)
	}
}

// An IPv6 target is relayed end to end.
func TestSOCKS5ConnectIPv6(t *testing.T) {
	target := startEchoOn(t, "tcp://[::1]:0")
	socks := startSocks(t)
	_, port := splitAddr(t, target)
	c := socksConnect(t, socks, "::1", port, AtypIPv6)
	msg := []byte("ipv6 relay")
	if _, err := c.Write(msg); err != nil {
		t.Fatal(err)
	}
	if got := readN(t, c, len(msg)); !bytes.Equal(got, msg) {
		t.Fatalf("echo = %q, want %q", got, msg)
	}
}

// A slow drip of handshake bytes does not reset the deadline (Slowloris).
func TestHandshakeSlowDrip(t *testing.T) {
	socks := startSocksOpts(t, Options{HandshakeTimeout: 300 * time.Millisecond})
	c, err := net.Dial("tcp", socks)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	// A valid method-negotiation header claiming 255 methods, then a slow drip
	// of method bytes: the handshake stays incomplete (no protocol error) and
	// the bytes must not reset the deadline.
	if _, err := c.Write([]byte{Version5, 255}); err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := c.Write([]byte{MethodNoAuth}); err != nil {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
	}()
	t.Cleanup(func() { close(stop) })
	start := time.Now()
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := c.Read(make([]byte, 1)); err == nil {
		t.Fatal("slow-drip handshake was not cut")
	}
	if elapsed := time.Since(start); elapsed < 250*time.Millisecond || elapsed > 2*time.Second {
		t.Fatalf("handshake cut after %v, want ~300ms (deadline, not reset by bytes)", elapsed)
	}
}

// Optimistic pipelining: the request and a large early payload arrive in one
// write. The payload is relayed, not counted against the handshake bound.
func TestOptimisticPipelining(t *testing.T) {
	target := startEcho(t)
	socks := startSocks(t)
	host, port := splitAddr(t, target)
	c, err := net.Dial("tcp", socks)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))

	payload := bytes.Repeat([]byte("x"), 1100) // > maxHandshake, pipelined after the request
	var buf []byte
	buf = append(buf, Version5, 1, MethodNoAuth)
	buf = append(buf, Version5, CmdConnect, 0, AtypIPv4)
	buf = append(buf, net.ParseIP(host).To4()...)
	buf = binary.BigEndian.AppendUint16(buf, port)
	buf = append(buf, payload...)
	if _, err := c.Write(buf); err != nil {
		t.Fatal(err)
	}
	if m := readN(t, c, 2); m[1] != MethodNoAuth {
		t.Fatalf("method reply = % x", m)
	}
	head := readN(t, c, 4)
	if head[1] != RepSuccess {
		t.Fatalf("CONNECT rep = %d, want success", head[1])
	}
	readN(t, c, 6)
	if got := readN(t, c, len(payload)); !bytes.Equal(got, payload) {
		t.Fatalf("pipelined payload echo corrupted (%d bytes)", len(got))
	}
}

// The largest legal pipelined handshake (255 methods + 255/255 credentials +
// 255-byte domain = 1032 bytes) parses fully instead of being cut.
func TestMaxPipelinedHandshake(t *testing.T) {
	socks := startSocksOpts(t, Options{Authenticator: &fakeAuth{}})
	c, err := net.Dial("tcp", socks)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))

	var buf []byte
	buf = append(buf, Version5, 255) // 255 methods
	methods := bytes.Repeat([]byte{MethodNoAuth}, 254)
	methods = append(methods, MethodUserPass)
	buf = append(buf, methods...)
	buf = append(buf, authVersion, 255) // 255-byte username
	buf = append(buf, bytes.Repeat([]byte("u"), 255)...)
	buf = append(buf, 255) // 255-byte password
	buf = append(buf, bytes.Repeat([]byte("p"), 255)...)
	buf = append(buf, Version5, CmdConnect, 0, AtypDomain, 255) // 255-byte domain
	buf = append(buf, bytes.Repeat([]byte("d"), 255)...)
	buf = binary.BigEndian.AppendUint16(buf, 80)
	if len(buf) != 1032 {
		t.Fatalf("handshake size = %d, want 1032", len(buf))
	}
	if _, err := c.Write(buf); err != nil {
		t.Fatal(err)
	}
	if m := readN(t, c, 2); m[1] != MethodUserPass {
		t.Fatalf("method reply = % x, want user/pass (handshake was cut?)", m)
	}
	if a := readN(t, c, 2); a[0] != authVersion || a[1] != 0 {
		t.Fatalf("auth reply = % x, want success", a)
	}
	// The domain is unresolvable, so the CONNECT reply is a failure — but it
	// arrives, proving the whole 1032-byte handshake parsed.
	head := readN(t, c, 4)
	readN(t, c, 6)
	if head[1] == RepSuccess {
		t.Fatal("unresolvable domain unexpectedly succeeded")
	}
}

// A peer that ends its write half mid-handshake is closed promptly, not held
// to the handshake deadline.
func TestPartialHandshakeFIN(t *testing.T) {
	socks := startSocksOpts(t, Options{HandshakeTimeout: 5 * time.Second})
	c, err := net.Dial("tcp", socks)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.Write([]byte{Version5, 1, MethodNoAuth}); err != nil {
		t.Fatal(err)
	}
	readN(t, c, 2)                                                      // method reply
	if _, err := c.Write([]byte{Version5, CmdConnect, 0}); err != nil { // incomplete request
		t.Fatal(err)
	}
	if err := c.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := c.Read(make([]byte, 1)); err == nil {
		t.Fatal("partial handshake + FIN was not closed")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("close waited for the deadline instead of the FIN")
	}
}

// The Limiter's release runs when the connection closes (concurrent-count seam).
func TestLimiterReleaseOnClose(t *testing.T) {
	target := startEcho(t)
	lim := &fakeLimiter{}
	socks := startSocksOpts(t, Options{Limiter: lim})
	host, port := splitAddr(t, target)
	c := socksConnect(t, socks, host, port, AtypIPv4)
	if _, err := c.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	readN(t, c, 1)
	_ = c.Close()
	deadline := time.Now().Add(3 * time.Second)
	for lim.releaseCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if lim.releaseCount() == 0 {
		t.Fatal("Limiter release was not called on close")
	}
}

// A failed dial releases its concurrency slot: after several refused dials a
// live dial still admits (no semaphore leak).
func TestDialCapReleasesOnFailure(t *testing.T) {
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, deadPort := splitAddr(t, probe.Addr().String())
	_ = probe.Close()

	target := startEcho(t)
	socks := startSocksOpts(t, Options{MaxConcurrentDials: 1})
	host, port := splitAddr(t, target)
	for i := 0; i < 3; i++ {
		if _, rep := socksRequest(t, socks, "127.0.0.1", deadPort, AtypIPv4, CmdConnect); rep == RepSuccess {
			t.Skip("dead port was reused; cannot exercise refused dial")
		}
	}
	c := socksConnect(t, socks, host, port, AtypIPv4) // would be busy if a slot leaked
	msg := []byte("after failures")
	if _, err := c.Write(msg); err != nil {
		t.Fatal(err)
	}
	if got := readN(t, c, len(msg)); !bytes.Equal(got, msg) {
		t.Fatalf("echo = %q", got)
	}
}
