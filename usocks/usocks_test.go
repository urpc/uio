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
	"io"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/urpc/uio"
)

func startEcho(t *testing.T) string {
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
	ln, err := ev.Listen("tcp://127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return ln.Addr().String()
}

func startSocks(t *testing.T) string {
	t.Helper()
	return startSocksOpts(t, Options{})
}

func startSocksOpts(t *testing.T, opts Options) string {
	t.Helper()
	srv := NewServer(opts)
	go srv.Events().Serve()
	t.Cleanup(func() { _ = srv.Close() })
	var ln uio.Listener
	var err error
	deadline := time.Now().Add(5 * time.Second)
	for {
		if ln, err = srv.Listen("tcp://127.0.0.1:0"); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("socks listen: %v", err)
		}
		time.Sleep(time.Millisecond)
	}
	return ln.Addr().String()
}

func readN(t *testing.T, c net.Conn, n int) []byte {
	t.Helper()
	buf := make([]byte, n)
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(c, buf); err != nil {
		t.Fatalf("read %d bytes: %v", n, err)
	}
	return buf
}

func splitAddr(t *testing.T, addr string) (string, uint16) {
	t.Helper()
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	p, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatal(err)
	}
	return host, uint16(p)
}

// socksRequest performs the method negotiation and sends a request, returning
// the connection and the reply code without asserting success.
func socksRequest(t *testing.T, socksAddr, host string, port uint16, atyp, cmd byte) (net.Conn, byte) {
	t.Helper()
	c, err := net.Dial("tcp", socksAddr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := c.Write([]byte{Version5, 1, MethodNoAuth}); err != nil {
		t.Fatal(err)
	}
	m := readN(t, c, 2)
	if m[0] != Version5 || m[1] != MethodNoAuth {
		t.Fatalf("method reply = % x", m)
	}
	req := []byte{Version5, cmd, 0, atyp}
	switch atyp {
	case AtypIPv4:
		req = append(req, net.ParseIP(host).To4()...)
	case AtypIPv6:
		req = append(req, net.ParseIP(host).To16()...)
	case AtypDomain:
		req = append(req, byte(len(host)))
		req = append(req, host...)
	}
	req = binary.BigEndian.AppendUint16(req, port)
	if _, err := c.Write(req); err != nil {
		t.Fatal(err)
	}
	head := readN(t, c, 4)
	switch head[3] {
	case AtypIPv4:
		readN(t, c, 4+2)
	case AtypIPv6:
		readN(t, c, 16+2)
	case AtypDomain:
		dl := readN(t, c, 1)[0]
		readN(t, c, int(dl)+2)
	}
	return c, head[1]
}

func socksConnect(t *testing.T, socksAddr, host string, port uint16, atyp byte) net.Conn {
	t.Helper()
	c, rep := socksRequest(t, socksAddr, host, port, atyp, CmdConnect)
	if rep != RepSuccess {
		t.Fatalf("CONNECT reply = %d, want success", rep)
	}
	return c
}

// A CONNECT through the proxy relays bytes to the target and back, over IPv4.
func TestSOCKS5ConnectEchoIPv4(t *testing.T) {
	target := startEcho(t)
	socks := startSocks(t)
	host, port := splitAddr(t, target)
	c := socksConnect(t, socks, host, port, AtypIPv4)
	msg := []byte("hello through socks")
	if _, err := c.Write(msg); err != nil {
		t.Fatal(err)
	}
	if got := readN(t, c, len(msg)); !bytes.Equal(got, msg) {
		t.Fatalf("echo = %q, want %q", got, msg)
	}
}

// A domain-name target is resolved and relayed.
func TestSOCKS5ConnectDomain(t *testing.T) {
	target := startEcho(t)
	socks := startSocks(t)
	_, port := splitAddr(t, target)
	c := socksConnect(t, socks, "localhost", port, AtypDomain)
	msg := []byte("domain target")
	if _, err := c.Write(msg); err != nil {
		t.Fatal(err)
	}
	if got := readN(t, c, len(msg)); !bytes.Equal(got, msg) {
		t.Fatalf("echo = %q, want %q", got, msg)
	}
}

// A large payload crosses the relay intact (exercises buffering/backpressure).
func TestSOCKS5LargeRelay(t *testing.T) {
	target := startEcho(t)
	socks := startSocks(t)
	host, port := splitAddr(t, target)
	c := socksConnect(t, socks, host, port, AtypIPv4)
	payload := make([]byte, 1<<20)
	for i := range payload {
		payload[i] = byte(i*7 + i/251)
	}
	go func() {
		for off := 0; off < len(payload); off += 4096 {
			end := off + 4096
			if end > len(payload) {
				end = len(payload)
			}
			if _, err := c.Write(payload[off:end]); err != nil {
				return
			}
		}
	}()
	got := make([]byte, len(payload))
	_ = c.SetReadDeadline(time.Now().Add(15 * time.Second))
	if _, err := io.ReadFull(c, got); err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatal("large relay corrupted the payload")
	}
}

// A non-CONNECT command is refused with the command-not-supported reply.
func TestSOCKS5CommandNotSupported(t *testing.T) {
	target := startEcho(t)
	socks := startSocks(t)
	host, port := splitAddr(t, target)
	_, rep := socksRequest(t, socks, host, port, AtypIPv4, CmdBind)
	if rep != RepCommandNotSupported {
		t.Fatalf("BIND reply = %d, want command-not-supported", rep)
	}
}

// A client offering no acceptable method is refused and closed.
func TestSOCKS5NoAcceptableMethod(t *testing.T) {
	socks := startSocks(t)
	c, err := net.Dial("tcp", socks)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.Write([]byte{Version5, 1, MethodUserPass}); err != nil {
		t.Fatal(err)
	}
	m := readN(t, c, 2)
	if m[0] != Version5 || m[1] != MethodNoAcceptable {
		t.Fatalf("method reply = % x, want no-acceptable", m)
	}
}

// startHalfCloseEcho serves a target that echoes input and, once the peer ends
// its write half, answers "fin-ack" and ends its own — exercising half-close.
func startHalfCloseEcho(t *testing.T) string {
	t.Helper()
	started := make(chan struct{})
	ev := &uio.Events{
		Pollers:       1,
		AllowHalfOpen: true,
		Handler: uio.FuncHandler{Read: func(c uio.Conn) error {
			if n := c.InboundBuffered(); n > 0 {
				got := c.Peek(make([]byte, n))
				if _, err := c.Write(got); err != nil {
					return err
				}
				_, _ = c.Discard(len(got))
			}
			if c.State()&uio.StateReadEnded != 0 {
				if done, _ := c.Userdata().(bool); !done {
					c.SetUserdata(true)
					if _, err := c.Write([]byte("fin-ack")); err != nil {
						return err
					}
					return c.CloseWrite()
				}
			}
			return nil
		}},
		Hooks: uio.FuncHandler{Start: func(*uio.Events) { close(started) }},
	}
	go ev.Serve()
	t.Cleanup(func() { _ = ev.Close(nil) })
	<-started
	ln, err := ev.Listen("tcp://127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return ln.Addr().String()
}

// A client half-close travels through the SOCKS layer and the relay to the
// target, whose response and own half-close travel back: CloseWrite passes
// through the transparent layer end to end.
func TestSOCKS5HalfClose(t *testing.T) {
	target := startHalfCloseEcho(t)
	socks := startSocks(t)
	host, port := splitAddr(t, target)
	c := socksConnect(t, socks, host, port, AtypIPv4)
	if _, err := c.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	if got := readN(t, c, 4); string(got) != "ping" {
		t.Fatalf("echo = %q, want ping", got)
	}
	if err := c.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	if got := readN(t, c, 7); string(got) != "fin-ack" {
		t.Fatalf("half-close response = %q, want fin-ack", got)
	}
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.Read(make([]byte, 1)); err != io.EOF {
		t.Fatalf("read after peer half-close = %v, want EOF", err)
	}
}

func TestParseRequest(t *testing.T) {
	// IPv4 CONNECT to 127.0.0.1:8080.
	buf := []byte{Version5, CmdConnect, 0, AtypIPv4, 127, 0, 0, 1, 0x1f, 0x90}
	req, n, ok, err := parseRequest(buf)
	if err != nil || !ok || n != len(buf) || req.Host != "127.0.0.1" || req.Port != 8080 || req.Cmd != CmdConnect {
		t.Fatalf("parseRequest = %+v, n=%d ok=%v err=%v", req, n, ok, err)
	}
	// Incomplete: only the header.
	if _, _, ok, err := parseRequest(buf[:4]); ok || err != nil {
		t.Fatalf("incomplete parse ok=%v err=%v, want need-more", ok, err)
	}
	// Domain.
	dbuf := []byte{Version5, CmdConnect, 0, AtypDomain, 9, 'e', 'x', 'a', 'm', 'p', 'l', 'e', '.', 'c', 0x00, 0x50}
	req, n, ok, err = parseRequest(dbuf)
	if err != nil || !ok || n != len(dbuf) || req.Host != "example.c" || req.Port != 80 {
		t.Fatalf("domain parse = %+v, n=%d ok=%v err=%v", req, n, ok, err)
	}
	// Bad reserved byte.
	bad := append([]byte(nil), buf...)
	bad[2] = 1
	if _, _, _, err := parseRequest(bad); err == nil {
		t.Fatal("bad RSV accepted")
	}
}

func TestParseMethodNeg(t *testing.T) {
	methods, n, ok, err := parseMethodNeg([]byte{Version5, 2, MethodNoAuth, MethodUserPass})
	if err != nil || !ok || n != 4 || len(methods) != 2 {
		t.Fatalf("parseMethodNeg = % x n=%d ok=%v err=%v", methods, n, ok, err)
	}
	if _, _, ok, err := parseMethodNeg([]byte{Version5, 2, MethodNoAuth}); ok || err != nil {
		t.Fatalf("incomplete ok=%v err=%v, want need-more", ok, err)
	}
}
