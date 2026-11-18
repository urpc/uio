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
	"io"
	"net"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"
)

// socksUDPAssociateAuth authenticates (RFC 1929) then issues a UDP ASSOCIATE,
// returning the control connection and the relay's bound address.
func socksUDPAssociateAuth(t *testing.T, socksAddr, user, pass string) (net.Conn, netip.AddrPort) {
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
	if m := readN(t, c, 2); m[1] != MethodUserPass {
		t.Fatalf("method reply % x, want user/pass", m)
	}
	auth := []byte{authVersion, byte(len(user))}
	auth = append(auth, user...)
	auth = append(auth, byte(len(pass)))
	auth = append(auth, pass...)
	if _, err := c.Write(auth); err != nil {
		t.Fatal(err)
	}
	if st := readN(t, c, 2); st[1] != 0 {
		t.Fatalf("auth status %d, want success", st[1])
	}
	if _, err := c.Write([]byte{Version5, CmdUDPAssociate, 0, AtypIPv4, 0, 0, 0, 0, 0, 0}); err != nil {
		t.Fatal(err)
	}
	head := readN(t, c, 4)
	if head[1] != RepSuccess {
		t.Fatalf("UDP ASSOCIATE rep = %d, want success", head[1])
	}
	ip := net.IP(readN(t, c, 4))
	p := readN(t, c, 2)
	addr, ok := netip.AddrFromSlice(ip)
	if !ok {
		t.Fatalf("bad bound ip %v", ip)
	}
	return c, netip.AddrPortFrom(addr.Unmap(), uint16(p[0])<<8|uint16(p[1]))
}

func udpClient(t *testing.T) *net.UDPConn {
	t.Helper()
	cli, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cli.Close() })
	return cli
}

// socksUDPAssociateAddr issues a UDP ASSOCIATE advertising host:port as the
// client's UDP source address (RFC 1928 §6), returning the control connection
// and the relay's bound address.
func socksUDPAssociateAddr(t *testing.T, socksAddr, host string, port uint16) (net.Conn, netip.AddrPort) {
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
	if m := readN(t, c, 2); m[1] != MethodNoAuth {
		t.Fatalf("method reply % x", m)
	}
	req := []byte{Version5, CmdUDPAssociate, 0, AtypIPv4}
	req = append(req, net.ParseIP(host).To4()...)
	req = binary.BigEndian.AppendUint16(req, port)
	if _, err := c.Write(req); err != nil {
		t.Fatal(err)
	}
	head := readN(t, c, 4)
	if head[1] != RepSuccess {
		t.Fatalf("UDP ASSOCIATE rep = %d, want success", head[1])
	}
	ip := net.IP(readN(t, c, 4))
	p := readN(t, c, 2)
	addr, ok := netip.AddrFromSlice(ip)
	if !ok {
		t.Fatalf("bad bound ip %v", ip)
	}
	return c, netip.AddrPortFrom(addr.Unmap(), uint16(p[0])<<8|uint16(p[1]))
}

// A client that advertises its IP with port 0 ("port unknown", the analogue of
// 0.0.0.0:0) is pinned by IP only — its OS-assigned source port must not be
// required to equal 0.
func TestUDPAssociateAdvertisedPortZero(t *testing.T) {
	target := udpEchoTarget(t)
	socks := startSocks(t)
	ctrl, bnd := socksUDPAssociateAddr(t, socks, "127.0.0.1", 0)
	defer ctrl.Close()
	cli := udpClient(t)

	framed := appendUDPHeader(nil, target.Addr().String(), target.Port(), []byte("x"))
	if _, err := cli.WriteToUDP(framed, net.UDPAddrFromAddrPort(bnd)); err != nil {
		t.Fatal(err)
	}
	_ = cli.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, _, err := cli.ReadFromUDP(make([]byte, 4096)); err != nil {
		t.Fatalf("advertised port 0 dropped the legitimate client: %v", err)
	}
}

// A foreign source cannot inject a forged reply to the client: only a target the
// client actually contacted may send replies back (the reply whitelist).
func TestUDPReplyInjectionBlocked(t *testing.T) {
	target := udpEchoTarget(t)
	socks := startSocks(t)
	ctrl, bnd := socksUDPAssociate(t, socks)
	defer ctrl.Close()
	cli := udpClient(t)

	// A legitimate round locks the client address and whitelists the target.
	framed := appendUDPHeader(nil, target.Addr().String(), target.Port(), []byte("x"))
	if _, err := cli.WriteToUDP(framed, net.UDPAddrFromAddrPort(bnd)); err != nil {
		t.Fatal(err)
	}
	_ = cli.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, _, err := cli.ReadFromUDP(make([]byte, 4096)); err != nil {
		t.Fatalf("lock round: %v", err)
	}

	// A different source port sends a raw datagram trying to pose as a target
	// reply. It is not a contacted target, so the client must receive nothing.
	other := udpClient(t)
	if _, err := other.WriteToUDP([]byte("forged reply"), net.UDPAddrFromAddrPort(bnd)); err != nil {
		t.Fatal(err)
	}
	_ = cli.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	if _, _, err := cli.ReadFromUDP(make([]byte, 4096)); err == nil {
		t.Fatal("a foreign source injected a reply to the client")
	}

	// The legitimate client still works afterward.
	if _, err := cli.WriteToUDP(framed, net.UDPAddrFromAddrPort(bnd)); err != nil {
		t.Fatal(err)
	}
	_ = cli.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, _, err := cli.ReadFromUDP(make([]byte, 4096)); err != nil {
		t.Fatalf("legit round after injection attempt: %v", err)
	}
}

// The idle sweep closes the TCP control channel, not just the relay socket, so
// a forgotten association does not leak the control connection's FD.
func TestUDPIdleSweepClosesTCP(t *testing.T) {
	target := udpEchoTarget(t)
	socks := startSocksOpts(t, Options{UDPIdleTimeout: 200 * time.Millisecond})
	ctrl, bnd := socksUDPAssociate(t, socks)
	defer ctrl.Close()
	cli := udpClient(t)

	frame := udpFrame(0, target.Addr().String(), target.Port(), []byte("x"))
	if _, err := cli.WriteToUDP(frame, net.UDPAddrFromAddrPort(bnd)); err != nil {
		t.Fatal(err)
	}
	_ = cli.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, _, err := cli.ReadFromUDP(make([]byte, 4096)); err != nil {
		t.Fatalf("first round: %v", err)
	}

	// Wait past the sweep tick (>=1s) with the association idle.
	time.Sleep(1500 * time.Millisecond)
	start := time.Now()
	_ = ctrl.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := ctrl.Read(make([]byte, 1)); err == nil {
		t.Fatal("idle sweep left the TCP control channel open")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("TCP control channel was not closed by the sweep (read timed out)")
	}
}

// Policy gates UDP ASSOCIATE itself: a denied client cannot allocate a relay.
func TestUDPPolicyGatesAssociate(t *testing.T) {
	pol := &fakePolicy{err: errors.New("denied")}
	socks := startSocksOpts(t, Options{Policy: pol})
	_, rep := socksUDPAssociateRep(t, socks)
	if rep != RepNotAllowed {
		t.Fatalf("UDP ASSOCIATE rep = %d, want not-allowed", rep)
	}
}

// The authenticated username reaches the per-datagram Policy hook, so a policy
// can decide UDP targets per user.
func TestUDPPolicyUsername(t *testing.T) {
	target := udpEchoTarget(t)
	auth := &fakeAuth{}
	pol := &fakePolicy{}
	socks := startSocksOpts(t, Options{Authenticator: auth, Policy: pol})
	ctrl, bnd := socksUDPAssociateAuth(t, socks, "alice", "secret")
	defer ctrl.Close()
	cli := udpClient(t)

	framed := appendUDPHeader(nil, target.Addr().String(), target.Port(), []byte("q"))
	if _, err := cli.WriteToUDP(framed, net.UDPAddrFromAddrPort(bnd)); err != nil {
		t.Fatal(err)
	}
	_ = cli.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, _, err := cli.ReadFromUDP(make([]byte, 4096)); err != nil {
		t.Fatalf("read reply: %v", err)
	}
	// The last Policy call is the per-datagram one (the ASSOCIATE request came
	// first): it must carry the username and the datagram's target.
	got := pol.snapshot()
	if got.Username != "alice" {
		t.Fatalf("per-datagram policy username = %q, want alice", got.Username)
	}
	if got.Cmd != CmdUDPAssociate {
		t.Fatalf("per-datagram policy cmd = %d, want UDP ASSOCIATE", got.Cmd)
	}
	if got.Host != target.Addr().String() || got.Port != target.Port() {
		t.Fatalf("per-datagram policy target = %s:%d, want %s", got.Host, got.Port, target)
	}
}

// A retransmitted fragment replaces its position rather than inflating the
// reassembled total: a sequence whose distinct size fits the UDP maximum is not
// abandoned just because duplicates arrived. This exercises reassemble directly
// because the end-to-end path cannot carry a datagram this large on every
// platform (a dual-stack relay socket hits the IPv6 minimum MTU on macOS).
func TestReassembleDuplicateRetransmit(t *testing.T) {
	r := &udpRelay{opts: Options{}}
	a := &udpAssociation{}
	p1 := bytes.Repeat([]byte("a"), 30000)
	p2 := bytes.Repeat([]byte("b"), 30000)
	// Distinct total 60000 < 65535; counting every arrival of the retransmitted
	// first fragment (3 x 30000 + 30000 = 120000) would exceed the UDP maximum.
	for i := 0; i < 3; i++ {
		if complete, _ := r.reassemble(a, 1, p1); complete {
			t.Fatal("fragment 1 alone completed the sequence")
		}
	}
	complete, full := r.reassemble(a, 2|0x80, p2)
	if !complete {
		t.Fatal("duplicate retransmits inflated the total and abandoned the sequence")
	}
	want := append(append([]byte{}, p1...), p2...)
	if !bytes.Equal(full, want) {
		t.Fatalf("reassembled len=%d, want %d", len(full), len(want))
	}
}

// DialSOCKS5 honors ctx cancellation even without a deadline: a silent upstream
// does not hang the dial past the cancel.
func TestDialSOCKS5ContextCancel(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	var mu sync.Mutex
	var conns []net.Conn
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, c) // hold open and silent
			mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		mu.Lock()
		for _, c := range conns {
			_ = c.Close()
		}
		mu.Unlock()
	})

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	_, err = DialSOCKS5(ctx, ln.Addr().String(), "127.0.0.1:80", nil)
	if err == nil {
		t.Fatal("expected an error from the cancelled context")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("ctx cancellation not honored (took %v)", time.Since(start))
	}
}

// With credentials configured, DialSOCKS5 offers only username/password and
// rejects an upstream that tries to downgrade to no-auth.
func TestDialSOCKS5NoDowngrade(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		var hdr [2]byte
		if _, err := io.ReadFull(c, hdr[:]); err != nil {
			return
		}
		methods := make([]byte, hdr[1])
		if _, err := io.ReadFull(c, methods); err != nil {
			return
		}
		// The client must offer only username/password, never no-auth alongside.
		for _, m := range methods {
			if m == MethodNoAuth {
				return // offering no-auth is the bug; let the dial time out
			}
		}
		_, _ = c.Write([]byte{Version5, MethodNoAuth}) // attempt a downgrade
		time.Sleep(300 * time.Millisecond)
	}()

	start := time.Now()
	_, err = DialSOCKS5(context.Background(), ln.Addr().String(), "127.0.0.1:80", &SOCKS5Auth{Username: "u", Password: "p"})
	if err == nil {
		t.Fatal("client accepted a no-auth downgrade despite configured credentials")
	}
	if !strings.Contains(err.Error(), "no-auth") {
		t.Fatalf("error = %v, want a no-auth downgrade rejection", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("downgrade not rejected promptly (took %v)", time.Since(start))
	}
}

// RFC 1929: ULEN is 1..255; a zero-length username is malformed and rejected.
func TestParseAuthEmptyUsername(t *testing.T) {
	buf := []byte{authVersion, 0, 0} // ULEN=0, PLEN=0
	if _, _, _, ok, err := parseAuth(buf); ok || err == nil {
		t.Fatalf("parseAuth ULEN=0: ok=%v err=%v, want an error", ok, err)
	}
}
