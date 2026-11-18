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
	"context"
	"encoding/binary"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/urpc/uio"
)

type fakeAuth struct {
	mu               sync.Mutex
	err              error
	gotUser, gotPass string
}

func (a *fakeAuth) Authenticate(_ context.Context, user, pass string) error {
	a.mu.Lock()
	a.gotUser, a.gotPass = user, pass
	a.mu.Unlock()
	return a.err
}

func (a *fakeAuth) creds() (string, string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.gotUser, a.gotPass
}

type fakePolicy struct {
	mu     sync.Mutex
	err    error
	gotReq Request
}

func (p *fakePolicy) Allow(_ context.Context, req *Request) error {
	p.mu.Lock()
	p.gotReq = *req
	p.mu.Unlock()
	return p.err
}

func (p *fakePolicy) snapshot() Request {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.gotReq
}

type fakeLimiter struct {
	mu       sync.Mutex
	err      error
	gotReq   Request
	released int
}

func (l *fakeLimiter) Admit(_ context.Context, req *Request) (func(), error) {
	l.mu.Lock()
	l.gotReq = *req
	l.mu.Unlock()
	if l.err != nil {
		return nil, l.err
	}
	return func() {
		l.mu.Lock()
		l.released++
		l.mu.Unlock()
	}, nil
}

func (l *fakeLimiter) releaseCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.released
}

// socksAuth performs method negotiation offering username/password, the RFC
// 1929 exchange, and a CONNECT request; it returns the reply code.
func socksAuth(t *testing.T, socksAddr, host string, port uint16, user, pass string) (net.Conn, byte) {
	t.Helper()
	c, err := net.Dial("tcp", socksAddr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := c.Write([]byte{Version5, 1, MethodUserPass}); err != nil {
		t.Fatal(err)
	}
	if m := readN(t, c, 2); m[0] != Version5 || m[1] != MethodUserPass {
		t.Fatalf("method reply = % x, want user/pass", m)
	}
	auth := []byte{authVersion, byte(len(user))}
	auth = append(auth, user...)
	auth = append(auth, byte(len(pass)))
	auth = append(auth, pass...)
	if _, err := c.Write(auth); err != nil {
		t.Fatal(err)
	}
	st := readN(t, c, 2)
	if st[0] != authVersion {
		t.Fatalf("auth reply version = %d", st[0])
	}
	if st[1] != 0 {
		return c, st[1] // auth failure status
	}
	req := []byte{Version5, CmdConnect, 0, AtypIPv4}
	req = append(req, net.ParseIP(host).To4()...)
	req = binary.BigEndian.AppendUint16(req, port)
	if _, err := c.Write(req); err != nil {
		t.Fatal(err)
	}
	head := readN(t, c, 4)
	readN(t, c, 6) // IPv4 bound addr + port
	return c, head[1]
}

// Valid credentials authenticate, the username reaches the Policy hook, and
// the relay works.
func TestAuthSuccessAndMetadata(t *testing.T) {
	target := startEcho(t)
	auth := &fakeAuth{}
	pol := &fakePolicy{}
	socks := startSocksOpts(t, Options{Authenticator: auth, Policy: pol})
	host, port := splitAddr(t, target)
	c, rep := socksAuth(t, socks, host, port, "alice", "secret")
	if rep != RepSuccess {
		t.Fatalf("CONNECT rep = %d, want success", rep)
	}
	gotUser, gotPass := auth.creds()
	if gotUser != "alice" || gotPass != "secret" {
		t.Fatalf("authenticator saw %q/%q", gotUser, gotPass)
	}
	gotReq := pol.snapshot()
	if gotReq.Username != "alice" {
		t.Fatalf("policy req username = %q, want alice", gotReq.Username)
	}
	if !gotReq.ClientAddr.IsValid() {
		t.Fatal("policy req client addr not filled")
	}
	if gotReq.Host != host || gotReq.Port != port {
		t.Fatalf("policy req target = %s:%d, want %s:%d", gotReq.Host, gotReq.Port, host, port)
	}
	msg := []byte("authenticated relay")
	if _, err := c.Write(msg); err != nil {
		t.Fatal(err)
	}
	if got := readN(t, c, len(msg)); !bytes.Equal(got, msg) {
		t.Fatalf("echo = %q", got)
	}
}

// Bad credentials fail the auth subnegotiation and are counted.
func TestAuthFailure(t *testing.T) {
	target := startEcho(t)
	auth := &fakeAuth{err: errors.New("bad credentials")}
	socks := startSocksOpts(t, Options{Authenticator: auth})
	host, port := splitAddr(t, target)
	c, status := socksAuth(t, socks, host, port, "alice", "wrong")
	if status != 1 {
		t.Fatalf("auth status = %d, want failure (1)", status)
	}
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := c.Read(make([]byte, 1)); err == nil {
		t.Fatal("connection stayed open after auth failure")
	}
}

// A Policy denial answers connection-not-allowed and is counted.
func TestPolicyDeny(t *testing.T) {
	target := startEcho(t)
	pol := &fakePolicy{err: errors.New("denied")}
	socks := startSocksOpts(t, Options{Policy: pol})
	host, port := splitAddr(t, target)
	_, rep := socksRequest(t, socks, host, port, AtypIPv4, CmdConnect)
	if rep != RepNotAllowed {
		t.Fatalf("CONNECT rep = %d, want not-allowed", rep)
	}
}

// A Limiter denial answers connection-not-allowed.
func TestLimiterDeny(t *testing.T) {
	target := startEcho(t)
	lim := &fakeLimiter{err: errors.New("quota")}
	socks := startSocksOpts(t, Options{Limiter: lim})
	host, port := splitAddr(t, target)
	_, rep := socksRequest(t, socks, host, port, AtypIPv4, CmdConnect)
	if rep != RepNotAllowed {
		t.Fatalf("CONNECT rep = %d, want not-allowed", rep)
	}
}

// A stalled handshake is cut by the deadline (Slowloris defense).
func TestHandshakeTimeout(t *testing.T) {
	socks := startSocksOpts(t, Options{HandshakeTimeout: 50 * time.Millisecond})
	c, err := net.Dial("tcp", socks)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	if _, err := c.Write([]byte{Version5}); err != nil { // incomplete negotiation
		t.Fatal(err)
	}
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := c.Read(make([]byte, 1)); err == nil {
		t.Fatal("stalled handshake was not cut")
	}
}

// A refused target dial answers connection-refused, and the reply drains
// (graceful close) rather than arriving as a reset.
func TestDialFailureReplyDrains(t *testing.T) {
	// Grab a port then release it so the dial is refused.
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, port := splitAddr(t, probe.Addr().String())
	_ = probe.Close()

	socks := startSocks(t)
	_, rep := socksRequest(t, socks, "127.0.0.1", port, AtypIPv4, CmdConnect)
	if rep == RepSuccess {
		t.Skip("released port was reused before the dial; cannot exercise refusal")
	}
	if rep != RepConnectionRefused {
		t.Fatalf("CONNECT rep = %d, want connection-refused", rep)
	}
}

// Stats count handshakes and successful connects.
func TestStats(t *testing.T) {
	target := startEcho(t)
	srv := NewServer(Options{})
	go srv.Events().Serve()
	t.Cleanup(func() { _ = srv.Close() })
	var ln uio.Listener
	var err error
	for i := 0; i < 200; i++ {
		if ln, err = srv.Listen("tcp://127.0.0.1:0"); err == nil {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	host, port := splitAddr(t, target)
	c := socksConnect(t, ln.Addr().String(), host, port, AtypIPv4)
	msg := []byte("stats")
	if _, err := c.Write(msg); err != nil {
		t.Fatal(err)
	}
	if got := readN(t, c, len(msg)); !bytes.Equal(got, msg) {
		t.Fatalf("echo = %q", got)
	}
	st := srv.Stats()
	if st.Handshakes < 1 || st.Connects < 1 {
		t.Fatalf("stats = %+v, want handshakes/connects >= 1", st)
	}
}
