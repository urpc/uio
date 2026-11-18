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

// Package usocks implements the SOCKS5 protocol (RFC 1928, RFC 1929) as a uio
// Handler: a production CONNECT and UDP ASSOCIATE proxy whose relay data plane
// is ubridge. The connection stays a plain socket for its whole life, so a
// CONNECT relay uses native splice(2).
//
// Governance is pluggable: Authenticator, Policy and Limiter are hooks the
// business layer fills via Options. usocks ships the seams, the wire protocol
// and safe defaults (no-auth, allow-all, unlimited); concrete credential
// stores, rule engines, rate-limit algorithms and hot-reload are the business's
// concern. BIND dispatches on Request.Cmd in a later phase; the request/reply
// abstraction already carries its fields.
package usocks

import (
	"context"
	"encoding/binary"
	"errors"
	"net"
	"net/netip"
	"strconv"
	"time"
)

// SOCKS5 protocol constants (RFC 1928).
const (
	Version5 = 0x05

	CmdConnect      = 0x01
	CmdBind         = 0x02
	CmdUDPAssociate = 0x03

	AtypIPv4   = 0x01
	AtypDomain = 0x03
	AtypIPv6   = 0x04

	MethodNoAuth       = 0x00
	MethodUserPass     = 0x02
	MethodNoAcceptable = 0xFF

	RepSuccess             = 0x00
	RepGeneralFailure      = 0x01
	RepNotAllowed          = 0x02
	RepNetworkUnreachable  = 0x03
	RepHostUnreachable     = 0x04
	RepConnectionRefused   = 0x05
	RepTTLExpired          = 0x06
	RepCommandNotSupported = 0x07
	RepAddressNotSupported = 0x08

	authVersion = 0x01 // RFC 1929 username/password subnegotiation version
)

// maxHandshake bounds the bytes a handshake may accumulate before it is cut.
const maxHandshake = 1024

// Authenticator verifies username/password credentials (RFC 1929). The wire
// exchange is usocks's job; the credential check is this hook. It runs on the
// connection's turn, so it must return promptly — a slow external backend
// belongs behind an async wrapper the business provides. Compare credentials
// in constant time; that is the hook's responsibility. Configuring an
// Authenticator makes authentication mandatory: a client that does not offer
// username/password is refused, never downgraded to no-auth. A nil
// Authenticator offers no-auth only.
type Authenticator interface {
	Authenticate(ctx context.Context, username, password string) error
}

// Policy decides whether a parsed request may proceed, after the handshake and
// before the target dial. A non-nil error denies it (reply: connection not
// allowed). It runs on the connection's turn, so it must return promptly. A
// nil Policy allows all.
type Policy interface {
	Allow(ctx context.Context, req *Request) error
}

// Limiter admits a request against quotas (per-IP, per-user, per-target). A
// non-nil error refuses it (reply: connection not allowed). The returned
// release func (may be nil) is called once when the connection closes, so a
// concurrent-count limiter can decrement what Admit incremented. It runs on
// the connection's turn, so it must return promptly. A nil Limiter is
// unlimited.
type Limiter interface {
	Admit(ctx context.Context, req *Request) (release func(), err error)
}

// Options configures a Server and its handshake layer. The zero value is a
// no-auth, allow-all, unlimited CONNECT proxy with default timeouts.
type Options struct {
	Authenticator        Authenticator
	Policy               Policy
	Limiter              Limiter
	HandshakeTimeout     time.Duration // bounds the whole handshake (Slowloris defense); default 10s
	DialTimeout          time.Duration // bounds the target dial; default 10s
	MaxConcurrentDials   int           // caps in-flight target dials; 0 = unlimited
	InboundLimit         int           // relay backpressure watermark (ubridge); 0 = ubridge default
	UDPIdleTimeout       time.Duration // closes a UDP association idle this long; default 60s
	UDPReassemblyTimeout time.Duration // abandons an incomplete UDP fragment sequence; default 5s (RFC 1928 minimum)
	MaxUDPAssociations   int           // caps live UDP associations; 0 = unlimited

	// Dial customizes how a CONNECT target is reached. It returns an established
	// connection to the target — for example a tunnel through an upstream SOCKS5
	// proxy (see DialSOCKS5) — which the server adopts into its own Events and
	// relays. nil dials the target directly. The returned net.Conn must already
	// be connected to the target, any proxy handshake completed.
	Dial func(ctx context.Context, target string) (net.Conn, error)

	onAuthFailure func() // set by NewServer to count failed authentications
}

func (o Options) handshakeTimeout() time.Duration {
	if o.HandshakeTimeout > 0 {
		return o.HandshakeTimeout
	}
	return 10 * time.Second
}

func (o Options) dialTimeout() time.Duration {
	if o.DialTimeout > 0 {
		return o.DialTimeout
	}
	return 10 * time.Second
}

func (o Options) udpIdleTimeout() time.Duration {
	if o.UDPIdleTimeout > 0 {
		return o.UDPIdleTimeout
	}
	return 60 * time.Second
}

func (o Options) udpReassemblyTimeout() time.Duration {
	if o.UDPReassemblyTimeout > 5*time.Second {
		return o.UDPReassemblyTimeout
	}
	return 5 * time.Second // RFC 1928 §7: the reassembly timer MUST be >= 5s
}

// Request is a parsed SOCKS5 request plus connection metadata. It carries every
// command's fields, so BIND and UDP ASSOCIATE dispatch on Cmd without a new
// shape. ClientAddr and Username are filled by the handshake for the hooks;
// ClientAddr may be invalid on the stdio backend, so hooks must tolerate a
// zero value.
type Request struct {
	Cmd        byte
	Atyp       byte
	Host       string // IP literal or domain name
	Port       uint16
	ClientAddr netip.AddrPort
	Username   string
}

// Target is the host:port a CONNECT dials.
func (r Request) Target() string {
	return net.JoinHostPort(r.Host, strconv.Itoa(int(r.Port)))
}

var (
	errVersion           = errors.New("usocks: bad SOCKS version")
	errRSV               = errors.New("usocks: reserved byte not zero")
	errAtyp              = errors.New("usocks: unsupported address type")
	errDomain            = errors.New("usocks: empty domain name")
	errMethods           = errors.New("usocks: no methods offered")
	errAuthVersion       = errors.New("usocks: bad auth subnegotiation version")
	errAuthUsername      = errors.New("usocks: empty auth username")
	errHandshakeTooLarge = errors.New("usocks: handshake too large")
	errHandshakeTimeout  = errors.New("usocks: handshake timed out")
	errAuthFailed        = errors.New("usocks: authentication failed")
)

// selectMethod picks the method the client offers. With an Authenticator
// configured, authentication is mandatory: username/password is chosen if
// offered, and a client that offers only no-auth is refused (no downgrade).
// Without an Authenticator, no-auth is chosen if offered.
func selectMethod(methods []byte, auth Authenticator) byte {
	if auth != nil {
		for _, m := range methods {
			if m == MethodUserPass {
				return MethodUserPass
			}
		}
		return MethodNoAcceptable
	}
	for _, m := range methods {
		if m == MethodNoAuth {
			return MethodNoAuth
		}
	}
	return MethodNoAcceptable
}

// parseMethodNeg parses a method-negotiation message. ok is false while more
// bytes are needed; n is the bytes consumed once ok.
func parseMethodNeg(buf []byte) (methods []byte, n int, ok bool, err error) {
	if len(buf) < 2 {
		return nil, 0, false, nil
	}
	if buf[0] != Version5 {
		return nil, 0, false, errVersion
	}
	nm := int(buf[1])
	if nm == 0 {
		return nil, 0, false, errMethods
	}
	if len(buf) < 2+nm {
		return nil, 0, false, nil
	}
	return buf[2 : 2+nm], 2 + nm, true, nil
}

// parseAuth parses an RFC 1929 username/password message.
func parseAuth(buf []byte) (user, pass string, n int, ok bool, err error) {
	if len(buf) < 2 {
		return "", "", 0, false, nil
	}
	if buf[0] != authVersion {
		return "", "", 0, false, errAuthVersion
	}
	ulen := int(buf[1])
	if ulen == 0 {
		return "", "", 0, false, errAuthUsername // RFC 1929: ULEN is 1..255
	}
	if len(buf) < 2+ulen+1 {
		return "", "", 0, false, nil
	}
	user = string(buf[2 : 2+ulen])
	off := 2 + ulen
	plen := int(buf[off])
	off++
	if len(buf) < off+plen {
		return "", "", 0, false, nil
	}
	pass = string(buf[off : off+plen])
	off += plen
	return user, pass, off, true, nil
}

// parseRequest parses a CONNECT/BIND/UDP ASSOCIATE request. ok is false while
// more bytes are needed; n is the bytes consumed once ok.
func parseRequest(buf []byte) (req Request, n int, ok bool, err error) {
	if len(buf) < 4 {
		return req, 0, false, nil
	}
	if buf[0] != Version5 {
		return req, 0, false, errVersion
	}
	if buf[2] != 0 {
		return req, 0, false, errRSV
	}
	req.Cmd = buf[1]
	req.Atyp = buf[3]
	off := 4
	switch req.Atyp {
	case AtypIPv4:
		if len(buf) < off+4+2 {
			return req, 0, false, nil
		}
		req.Host = net.IP(buf[off : off+4]).String()
		off += 4
	case AtypIPv6:
		if len(buf) < off+16+2 {
			return req, 0, false, nil
		}
		req.Host = net.IP(buf[off : off+16]).String()
		off += 16
	case AtypDomain:
		if len(buf) < off+1 {
			return req, 0, false, nil
		}
		dl := int(buf[off])
		off++
		if dl == 0 {
			return req, 0, false, errDomain
		}
		if len(buf) < off+dl+2 {
			return req, 0, false, nil
		}
		req.Host = string(buf[off : off+dl])
		off += dl
	default:
		return req, 0, false, errAtyp
	}
	req.Port = binary.BigEndian.Uint16(buf[off : off+2])
	off += 2
	return req, off, true, nil
}

// appendReply appends a reply message with the given bound address.
func appendReply(buf []byte, rep, atyp byte, host string, port uint16) []byte {
	buf = append(buf, Version5, rep, 0, atyp)
	switch atyp {
	case AtypIPv4:
		ip := net.ParseIP(host).To4()
		if ip == nil {
			ip = net.IPv4zero.To4()
		}
		buf = append(buf, ip...)
	case AtypIPv6:
		ip := net.ParseIP(host).To16()
		if ip == nil {
			ip = net.IPv6zero
		}
		buf = append(buf, ip...)
	case AtypDomain:
		buf = append(buf, byte(len(host)))
		buf = append(buf, host...)
	}
	return binary.BigEndian.AppendUint16(buf, port)
}

// successReply is a CONNECT success reply with an unspecified bound address
// (0.0.0.0:0), which RFC 1928 permits for CONNECT.
func successReply() []byte {
	return appendReply(nil, RepSuccess, AtypIPv4, "0.0.0.0", 0)
}

func failureReply(rep byte) []byte {
	return appendReply(nil, rep, AtypIPv4, "0.0.0.0", 0)
}

// parseUDPHeader parses a SOCKS5 UDP request header (RFC 1928 §7):
// RSV(2) FRAG(1) ATYP(1) DST.ADDR DST.PORT, then the payload. ok is false while
// the header is incomplete; payload aliases buf past the header.
func parseUDPHeader(buf []byte) (frag byte, host string, port uint16, payload []byte, ok bool, err error) {
	if len(buf) < 4 {
		return 0, "", 0, nil, false, nil
	}
	if buf[0] != 0 || buf[1] != 0 {
		return 0, "", 0, nil, false, errRSV
	}
	frag = buf[2]
	atyp := buf[3]
	off := 4
	switch atyp {
	case AtypIPv4:
		if len(buf) < off+4+2 {
			return 0, "", 0, nil, false, nil
		}
		host = net.IP(buf[off : off+4]).String()
		off += 4
	case AtypIPv6:
		if len(buf) < off+16+2 {
			return 0, "", 0, nil, false, nil
		}
		host = net.IP(buf[off : off+16]).String()
		off += 16
	case AtypDomain:
		if len(buf) < off+1 {
			return 0, "", 0, nil, false, nil
		}
		dl := int(buf[off])
		off++
		if dl == 0 {
			return 0, "", 0, nil, false, errDomain
		}
		if len(buf) < off+dl+2 {
			return 0, "", 0, nil, false, nil
		}
		host = string(buf[off : off+dl])
		off += dl
	default:
		return 0, "", 0, nil, false, errAtyp
	}
	port = binary.BigEndian.Uint16(buf[off : off+2])
	off += 2
	return frag, host, port, buf[off:], true, nil
}

// appendUDPHeader appends a SOCKS5 UDP header (FRAG=0) for host:port followed by
// payload, the form a relay sends back to the client for a target's reply.
func appendUDPHeader(buf []byte, host string, port uint16, payload []byte) []byte {
	buf = append(buf, 0, 0, 0) // RSV(2), FRAG(0)
	ip := net.ParseIP(host)
	if ip4 := ip.To4(); ip4 != nil {
		buf = append(buf, AtypIPv4)
		buf = append(buf, ip4...)
	} else if ip != nil {
		buf = append(buf, AtypIPv6)
		buf = append(buf, ip.To16()...)
	} else {
		buf = append(buf, AtypDomain, byte(len(host)))
		buf = append(buf, host...)
	}
	buf = binary.BigEndian.AppendUint16(buf, port)
	return append(buf, payload...)
}
