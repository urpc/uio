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
	"net/netip"
	"testing"
	"time"
)

func TestParseUDPHeader(t *testing.T) {
	// IPv4 target 1.2.3.4:53 with payload "abc".
	buf := appendUDPHeader(nil, "1.2.3.4", 53, []byte("abc"))
	frag, host, port, payload, ok, err := parseUDPHeader(buf)
	if err != nil || !ok || frag != 0 || host != "1.2.3.4" || port != 53 || string(payload) != "abc" {
		t.Fatalf("parseUDPHeader = frag%d %s:%d %q ok=%v err=%v", frag, host, port, payload, ok, err)
	}
	// IPv6 roundtrip.
	buf6 := appendUDPHeader(nil, "::1", 5353, []byte("xyz"))
	_, host6, port6, payload6, ok6, err6 := parseUDPHeader(buf6)
	if err6 != nil || !ok6 || host6 != "::1" || port6 != 5353 || string(payload6) != "xyz" {
		t.Fatalf("ipv6 parse = %s:%d %q ok=%v err=%v", host6, port6, payload6, ok6, err6)
	}
	// Domain roundtrip.
	bufd := appendUDPHeader(nil, "dns.example", 53, []byte("q"))
	_, hostd, _, _, okd, errd := parseUDPHeader(bufd)
	if errd != nil || !okd || hostd != "dns.example" {
		t.Fatalf("domain parse = %s ok=%v err=%v", hostd, okd, errd)
	}
	// Incomplete header.
	if _, _, _, _, ok, err := parseUDPHeader([]byte{0, 0, 0}); ok || err != nil {
		t.Fatalf("incomplete ok=%v err=%v, want need-more", ok, err)
	}
	// Bad RSV.
	if _, _, _, _, _, err := parseUDPHeader([]byte{0, 1, 0, AtypIPv4, 1, 2, 3, 4, 0, 53}); err == nil {
		t.Fatal("bad RSV accepted")
	}
}

func FuzzParseUDPHeader(f *testing.F) {
	f.Add(appendUDPHeader(nil, "1.2.3.4", 53, []byte("abc")))
	f.Add(appendUDPHeader(nil, "dns.example", 5353, []byte("query")))
	f.Add([]byte{0, 0, 0, AtypIPv4})
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, buf []byte) {
		_, _, _, payload, ok, err := parseUDPHeader(buf)
		if ok {
			if len(payload) > len(buf) {
				t.Fatal("payload longer than input")
			}
			if err != nil {
				t.Fatal("ok with error")
			}
		}
	})
}

// udpEchoTarget is a plain UDP echo server (the relay's target).
func udpEchoTarget(t *testing.T) netip.AddrPort {
	t.Helper()
	pc, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	go func() {
		buf := make([]byte, 64<<10)
		for {
			n, addr, err := pc.ReadFromUDP(buf)
			if err != nil {
				return
			}
			_, _ = pc.WriteToUDP(buf[:n], addr)
		}
	}()
	return pc.LocalAddr().(*net.UDPAddr).AddrPort()
}

// socksUDPAssociate performs the TCP handshake and a UDP ASSOCIATE request,
// returning the control connection and the relay's bound UDP address.
func socksUDPAssociate(t *testing.T, socksAddr string) (net.Conn, netip.AddrPort) {
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
	// UDP ASSOCIATE, client source unknown (0.0.0.0:0).
	if _, err := c.Write([]byte{Version5, CmdUDPAssociate, 0, AtypIPv4, 0, 0, 0, 0, 0, 0}); err != nil {
		t.Fatal(err)
	}
	head := readN(t, c, 4)
	if head[1] != RepSuccess {
		t.Fatalf("UDP ASSOCIATE rep = %d, want success", head[1])
	}
	var ip net.IP
	switch head[3] {
	case AtypIPv4:
		ip = net.IP(readN(t, c, 4))
	case AtypIPv6:
		ip = net.IP(readN(t, c, 16))
	default:
		t.Fatalf("bound atyp %d", head[3])
	}
	port := binary.BigEndian.Uint16(readN(t, c, 2))
	addr, ok := netip.AddrFromSlice(ip)
	if !ok {
		t.Fatalf("bad bound ip %v", ip)
	}
	return c, netip.AddrPortFrom(addr.Unmap(), port)
}

// A UDP ASSOCIATE relays a framed datagram to the target and returns the echo
// framed with the target's address.
func TestUDPAssociateEcho(t *testing.T) {
	target := udpEchoTarget(t)
	socks := startSocks(t)
	ctrl, bnd := socksUDPAssociate(t, socks)
	defer ctrl.Close()

	cli, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()

	payload := []byte("udp through socks")
	framed := appendUDPHeader(nil, target.Addr().String(), target.Port(), payload)
	if _, err := cli.WriteToUDP(framed, net.UDPAddrFromAddrPort(bnd)); err != nil {
		t.Fatal(err)
	}
	_ = cli.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 64<<10)
	n, _, err := cli.ReadFromUDP(buf)
	if err != nil {
		t.Fatalf("read reply: %v", err)
	}
	_, host, port, got, ok, err := parseUDPHeader(buf[:n])
	if err != nil || !ok {
		t.Fatalf("reply parse ok=%v err=%v", ok, err)
	}
	if host != target.Addr().String() || port != target.Port() {
		t.Fatalf("reply target = %s:%d, want %s", host, port, target)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("reply payload = %q, want %q", got, payload)
	}
}

// One association relays to two different targets, each reply framed with its
// own target address.
func TestUDPAssociateMultiTarget(t *testing.T) {
	t1 := udpEchoTarget(t)
	t2 := udpEchoTarget(t)
	socks := startSocks(t)
	ctrl, bnd := socksUDPAssociate(t, socks)
	defer ctrl.Close()
	cli, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()

	for _, tg := range []netip.AddrPort{t1, t2} {
		framed := appendUDPHeader(nil, tg.Addr().String(), tg.Port(), []byte("ping"))
		if _, err := cli.WriteToUDP(framed, net.UDPAddrFromAddrPort(bnd)); err != nil {
			t.Fatal(err)
		}
		_ = cli.SetReadDeadline(time.Now().Add(5 * time.Second))
		buf := make([]byte, 4096)
		n, _, err := cli.ReadFromUDP(buf)
		if err != nil {
			t.Fatalf("read reply for %s: %v", tg, err)
		}
		_, host, port, got, ok, err := parseUDPHeader(buf[:n])
		if err != nil || !ok || host != tg.Addr().String() || port != tg.Port() || string(got) != "ping" {
			t.Fatalf("reply = %s:%d %q ok=%v err=%v, want %s", host, port, got, ok, err, tg)
		}
	}
}

// Closing the TCP control channel terminates the UDP association: the relay
// stops answering.
func TestUDPAssociateTCPCloseTerminates(t *testing.T) {
	target := udpEchoTarget(t)
	socks := startSocks(t)
	ctrl, bnd := socksUDPAssociate(t, socks)
	cli, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()

	// One round works while the control channel is open.
	framed := appendUDPHeader(nil, target.Addr().String(), target.Port(), []byte("x"))
	if _, err := cli.WriteToUDP(framed, net.UDPAddrFromAddrPort(bnd)); err != nil {
		t.Fatal(err)
	}
	_ = cli.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, _, err := cli.ReadFromUDP(make([]byte, 4096)); err != nil {
		t.Fatalf("first round: %v", err)
	}

	_ = ctrl.Close() // terminate the association
	time.Sleep(200 * time.Millisecond)
	if _, err := cli.WriteToUDP(framed, net.UDPAddrFromAddrPort(bnd)); err != nil {
		t.Fatal(err)
	}
	_ = cli.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	if _, _, err := cli.ReadFromUDP(make([]byte, 4096)); err == nil {
		t.Fatal("relay still answered after the TCP control channel closed")
	}
}

// A datagram from a source other than the locked client address is dropped.
func TestUDPAssociateRejectsForeignSource(t *testing.T) {
	target := udpEchoTarget(t)
	socks := startSocks(t)
	ctrl, bnd := socksUDPAssociate(t, socks)
	defer ctrl.Close()
	cli, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()

	// Lock the client address with a legitimate round.
	framed := appendUDPHeader(nil, target.Addr().String(), target.Port(), []byte("x"))
	if _, err := cli.WriteToUDP(framed, net.UDPAddrFromAddrPort(bnd)); err != nil {
		t.Fatal(err)
	}
	_ = cli.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, _, err := cli.ReadFromUDP(make([]byte, 4096)); err != nil {
		t.Fatalf("lock round: %v", err)
	}

	// A different source port must not be relayed.
	other, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if _, err := other.WriteToUDP(framed, net.UDPAddrFromAddrPort(bnd)); err != nil {
		t.Fatal(err)
	}
	_ = other.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	if _, _, err := other.ReadFromUDP(make([]byte, 4096)); err == nil {
		t.Fatal("a foreign source was relayed")
	}
}

// udpFrame builds a SOCKS UDP datagram with an explicit FRAG byte (appendUDPHeader
// always sets FRAG=0).
func udpFrame(frag byte, host string, port uint16, payload []byte) []byte {
	buf := []byte{0, 0, frag, AtypIPv4}
	buf = append(buf, net.ParseIP(host).To4()...)
	buf = binary.BigEndian.AppendUint16(buf, port)
	return append(buf, payload...)
}

// udpDomainFrame builds a SOCKS UDP datagram with a domain-name target.
func udpDomainFrame(host string, port uint16, payload []byte) []byte {
	buf := []byte{0, 0, 0, AtypDomain, byte(len(host))}
	buf = append(buf, host...)
	buf = binary.BigEndian.AppendUint16(buf, port)
	return append(buf, payload...)
}

// A fragment sequence whose reassembled size would exceed the UDP maximum is
// abandoned (memory-abuse defense), not relayed.
func TestUDPFragmentOversizeAbandoned(t *testing.T) {
	target := udpEchoTarget(t)
	socks := startSocks(t)
	ctrl, bnd := socksUDPAssociate(t, socks)
	defer ctrl.Close()
	cli, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()

	big := make([]byte, 8000)
	_ = cli.SetWriteBuffer(1 << 20)
	// 9 fragments of 8000B reassemble to 72000 > 65535: abandoned.
	for i := 1; i <= 9; i++ {
		frag := byte(i)
		if i == 9 {
			frag |= 0x80
		}
		f := udpFrame(frag, target.Addr().String(), target.Port(), big)
		if _, err := cli.WriteToUDP(f, net.UDPAddrFromAddrPort(bnd)); err != nil {
			t.Fatal(err)
		}
	}
	_ = cli.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	if _, _, err := cli.ReadFromUDP(make([]byte, 64<<10)); err == nil {
		t.Fatal("an oversize fragment sequence was relayed")
	}
}

// socksUDPAssociateRep is socksUDPAssociate but reports the reply code instead
// of failing on a non-success reply.
func socksUDPAssociateRep(t *testing.T, socksAddr string) (net.Conn, byte) {
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
	readN(t, c, 2)
	if _, err := c.Write([]byte{Version5, CmdUDPAssociate, 0, AtypIPv4, 0, 0, 0, 0, 0, 0}); err != nil {
		t.Fatal(err)
	}
	head := readN(t, c, 4)
	switch head[3] {
	case AtypIPv4:
		readN(t, c, 6)
	case AtypIPv6:
		readN(t, c, 18)
	}
	return c, head[1]
}

// A domain-name UDP target is resolved off the turn and relayed. The target
// listens dual-stack so whichever family the resolver picks for "localhost"
// reaches it.
func TestUDPDomainTarget(t *testing.T) {
	pc, err := net.ListenUDP("udp", &net.UDPAddr{IP: nil, Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	go func() {
		buf := make([]byte, 64<<10)
		for {
			n, addr, err := pc.ReadFromUDP(buf)
			if err != nil {
				return
			}
			_, _ = pc.WriteToUDP(buf[:n], addr)
		}
	}()
	port := uint16(pc.LocalAddr().(*net.UDPAddr).Port)

	socks := startSocks(t)
	ctrl, bnd := socksUDPAssociate(t, socks)
	defer ctrl.Close()
	cli, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()

	payload := []byte("domain udp target")
	frame := udpDomainFrame("localhost", port, payload)
	if _, err := cli.WriteToUDP(frame, net.UDPAddrFromAddrPort(bnd)); err != nil {
		t.Fatal(err)
	}
	_ = cli.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 4096)
	n, _, err := cli.ReadFromUDP(buf)
	if err != nil {
		t.Fatalf("read reply: %v", err)
	}
	_, _, _, got, ok, err := parseUDPHeader(buf[:n])
	if err != nil || !ok || !bytes.Equal(got, payload) {
		t.Fatalf("reply = %q ok=%v err=%v, want %q", got, ok, err, payload)
	}
}

// An association idle past UDPIdleTimeout is reclaimed by the sweep.
func TestUDPIdleTimeout(t *testing.T) {
	target := udpEchoTarget(t)
	socks := startSocksOpts(t, Options{UDPIdleTimeout: 200 * time.Millisecond})
	ctrl, bnd := socksUDPAssociate(t, socks)
	defer ctrl.Close()
	cli, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()

	frame := udpFrame(0, target.Addr().String(), target.Port(), []byte("x"))
	if _, err := cli.WriteToUDP(frame, net.UDPAddrFromAddrPort(bnd)); err != nil {
		t.Fatal(err)
	}
	_ = cli.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, _, err := cli.ReadFromUDP(make([]byte, 4096)); err != nil {
		t.Fatalf("first round: %v", err)
	}
	// The sweep ticks at >=1s; wait past it with the association idle.
	time.Sleep(1500 * time.Millisecond)
	if _, err := cli.WriteToUDP(frame, net.UDPAddrFromAddrPort(bnd)); err != nil {
		t.Fatal(err)
	}
	_ = cli.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	if _, _, err := cli.ReadFromUDP(make([]byte, 4096)); err == nil {
		t.Fatal("idle association was not reclaimed")
	}
}

// MaxUDPAssociations refuses a further association once the cap is reached.
func TestUDPMaxAssociations(t *testing.T) {
	socks := startSocksOpts(t, Options{MaxUDPAssociations: 1})
	ctrl1, rep1 := socksUDPAssociateRep(t, socks)
	if rep1 != RepSuccess {
		t.Fatalf("first association rep = %d, want success", rep1)
	}
	defer ctrl1.Close()
	_, rep2 := socksUDPAssociateRep(t, socks)
	if rep2 == RepSuccess {
		t.Fatal("second association succeeded past the cap")
	}
}

// A datagram split into ordered fragments is reassembled and relayed whole.
func TestUDPFragmentReassembly(t *testing.T) {
	target := udpEchoTarget(t)
	socks := startSocks(t)
	ctrl, bnd := socksUDPAssociate(t, socks)
	defer ctrl.Close()
	cli, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()

	payload := []byte("a fragmented udp datagram relayed through socks5")
	parts := [][]byte{payload[:10], payload[10:25], payload[25:]}
	for i, part := range parts {
		frag := byte(i + 1)
		if i == len(parts)-1 {
			frag |= 0x80 // last fragment
		}
		frame := udpFrame(frag, target.Addr().String(), target.Port(), part)
		if _, err := cli.WriteToUDP(frame, net.UDPAddrFromAddrPort(bnd)); err != nil {
			t.Fatal(err)
		}
	}
	_ = cli.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 64<<10)
	n, _, err := cli.ReadFromUDP(buf)
	if err != nil {
		t.Fatalf("read reassembled reply: %v", err)
	}
	_, _, _, got, ok, err := parseUDPHeader(buf[:n])
	if err != nil || !ok || !bytes.Equal(got, payload) {
		t.Fatalf("reassembled reply = %q ok=%v err=%v, want %q", got, ok, err, payload)
	}
}

// A fragment whose position regresses below the highest seen resets the queue
// (RFC 1928 §7); the restarted sequence still reassembles.
func TestUDPFragmentRegressionResets(t *testing.T) {
	target := udpEchoTarget(t)
	socks := startSocks(t)
	ctrl, bnd := socksUDPAssociate(t, socks)
	defer ctrl.Close()
	cli, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()

	payload := []byte("regression reset reassembly")
	p1, p2, p3 := payload[:8], payload[8:16], payload[16:]
	send := func(frag byte, part []byte) {
		frame := udpFrame(frag, target.Addr().String(), target.Port(), part)
		if _, err := cli.WriteToUDP(frame, net.UDPAddrFromAddrPort(bnd)); err != nil {
			t.Fatal(err)
		}
	}
	send(2, p2)      // highest = 2
	send(1, p1)      // 1 < 2: queue resets, starts at 1
	send(2, p2)      // 2 again
	send(3|0x80, p3) // last: 1,2,3 present -> reassemble
	_ = cli.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 4096)
	n, _, err := cli.ReadFromUDP(buf)
	if err != nil {
		t.Fatalf("read reply: %v", err)
	}
	_, _, _, got, ok, err := parseUDPHeader(buf[:n])
	if err != nil || !ok || !bytes.Equal(got, payload) {
		t.Fatalf("reply = %q ok=%v err=%v, want %q", got, ok, err, payload)
	}
}
