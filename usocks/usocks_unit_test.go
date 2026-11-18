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
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"syscall"
	"testing"
)

// repForDialError maps each dial failure class to its SOCKS reply code,
// including wrapped errors.
func TestRepForDialError(t *testing.T) {
	cases := []struct {
		err  error
		want byte
	}{
		{syscall.ECONNREFUSED, RepConnectionRefused},
		{fmt.Errorf("dial: %w", syscall.ECONNREFUSED), RepConnectionRefused},
		{syscall.ENETUNREACH, RepNetworkUnreachable},
		{syscall.EHOSTUNREACH, RepHostUnreachable},
		{context.DeadlineExceeded, RepTTLExpired},
		{&net.DNSError{Err: "no such host", Name: "x"}, RepHostUnreachable},
		{errors.New("something else"), RepGeneralFailure},
	}
	for _, c := range cases {
		if got := repForDialError(c.err); got != c.want {
			t.Errorf("repForDialError(%v) = %d, want %d", c.err, got, c.want)
		}
	}
}

// appendReply encodes IPv6 and domain bound addresses, not only IPv4.
func TestAppendReplyEncodings(t *testing.T) {
	// IPv6 bound address.
	buf := appendReply(nil, RepSuccess, AtypIPv6, "::1", 8080)
	if buf[0] != Version5 || buf[1] != RepSuccess || buf[3] != AtypIPv6 {
		t.Fatalf("ipv6 reply header = % x", buf[:4])
	}
	if ip := net.IP(buf[4:20]); ip.String() != "::1" {
		t.Fatalf("ipv6 bound addr = %s", ip)
	}
	if port := binary.BigEndian.Uint16(buf[20:22]); port != 8080 {
		t.Fatalf("ipv6 bound port = %d", port)
	}
	// Domain bound address.
	buf = appendReply(nil, RepSuccess, AtypDomain, "example.com", 443)
	if buf[3] != AtypDomain || int(buf[4]) != len("example.com") {
		t.Fatalf("domain reply header = % x", buf[:5])
	}
	if host := string(buf[5 : 5+len("example.com")]); host != "example.com" {
		t.Fatalf("domain bound host = %q", host)
	}
	off := 5 + len("example.com")
	if port := binary.BigEndian.Uint16(buf[off : off+2]); port != 443 {
		t.Fatalf("domain bound port = %d", port)
	}
}

type stubAddr struct{ s string }

func (a stubAddr) Network() string { return "stub" }
func (a stubAddr) String() string  { return a.s }

// addrPortOf handles TCP/UDP addresses, falls back to parsing another address
// type, and yields a zero value for nil or unparseable addresses.
func TestAddrPortOf(t *testing.T) {
	want := netip.MustParseAddrPort("1.2.3.4:53")
	eq := func(got netip.AddrPort) bool { return got.Addr().Unmap() == want.Addr() && got.Port() == want.Port() }
	if got := addrPortOf(&net.TCPAddr{IP: net.ParseIP("1.2.3.4"), Port: 53}); !eq(got) {
		t.Fatalf("TCPAddr = %s", got)
	}
	if got := addrPortOf(&net.UDPAddr{IP: net.ParseIP("1.2.3.4"), Port: 53}); !eq(got) {
		t.Fatalf("UDPAddr = %s", got)
	}
	if got := addrPortOf(stubAddr{"1.2.3.4:53"}); !eq(got) {
		t.Fatalf("stub fallback = %s", got)
	}
	if got := addrPortOf(nil); got.IsValid() {
		t.Fatalf("nil = %s, want zero", got)
	}
	if got := addrPortOf(stubAddr{"not an address"}); got.IsValid() {
		t.Fatalf("unparseable = %s, want zero", got)
	}
}
