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
	"fmt"
	"io"
	"net"
	"strconv"
	"time"
)

// SOCKS5Auth is a username/password credential for an upstream SOCKS5 proxy
// (RFC 1929). A nil *SOCKS5Auth offers no authentication.
type SOCKS5Auth struct {
	Username string
	Password string
}

// DialSOCKS5 dials target through an upstream SOCKS5 proxy at upstreamAddr
// (host:port), performing the client handshake (method negotiation, optional
// username/password auth, CONNECT). The returned net.Conn is a tunnel to
// target, ready for the server to adopt — set it as Options.Dial to chain
// proxies:
//
//	srv := usocks.NewServer(usocks.Options{
//	    Dial: func(ctx context.Context, target string) (net.Conn, error) {
//	        return usocks.DialSOCKS5(ctx, "upstream:1080", target, nil)
//	    },
//	})
//
// The handshake runs on the caller's goroutine over a plain net.Conn, bounded
// by ctx; the server adopts the established tunnel into its own Events.
func DialSOCKS5(ctx context.Context, upstreamAddr, target string, auth *SOCKS5Auth) (net.Conn, error) {
	var d net.Dialer
	nc, err := d.DialContext(ctx, "tcp", upstreamAddr)
	if err != nil {
		return nil, err
	}
	if err := socks5ClientHandshake(ctx, nc, target, auth); err != nil {
		_ = nc.Close()
		return nil, err
	}
	return nc, nil
}

// defaultClientHandshakeTimeout bounds the upstream handshake when ctx carries
// no deadline, so a slow or silent upstream cannot hang the dial forever.
const defaultClientHandshakeTimeout = 10 * time.Second

// socks5ClientHandshake negotiates a CONNECT to target over nc, bounded by ctx.
func socks5ClientHandshake(ctx context.Context, nc net.Conn, target string, auth *SOCKS5Auth) error {
	// Bound the handshake by the ctx deadline, or a default when ctx has none.
	deadline := time.Now().Add(defaultClientHandshakeTimeout)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}
	_ = nc.SetDeadline(deadline)
	defer nc.SetDeadline(time.Time{})
	// Honor ctx cancellation even without a deadline: closing nc unblocks the
	// pending read/write. stop() on the success path prevents the close, so a
	// live tunnel is never returned closed.
	stop := context.AfterFunc(ctx, func() { _ = nc.Close() })
	if err := socks5ClientExchange(nc, target, auth); err != nil {
		stop()
		return err
	}
	if !stop() {
		// ctx was cancelled during the handshake; nc has been closed.
		return ctx.Err()
	}
	return nil
}

// socks5ClientExchange runs the method negotiation, optional auth and CONNECT
// over nc. The caller has already bounded nc by a deadline and ctx cancellation.
func socks5ClientExchange(nc net.Conn, target string, auth *SOCKS5Auth) error {
	// With credentials configured, offer only username/password: offering
	// no-auth alongside would let the upstream silently downgrade and skip
	// authentication.
	methods := []byte{MethodNoAuth}
	if auth != nil {
		methods = []byte{MethodUserPass}
	}
	if _, err := nc.Write(append([]byte{Version5, byte(len(methods))}, methods...)); err != nil {
		return err
	}
	var sel [2]byte
	if _, err := io.ReadFull(nc, sel[:]); err != nil {
		return err
	}
	if sel[0] != Version5 {
		return fmt.Errorf("usocks: upstream bad version %d", sel[0])
	}
	switch sel[1] {
	case MethodNoAuth:
		if auth != nil {
			return fmt.Errorf("usocks: upstream selected no-auth despite configured credentials")
		}
	case MethodUserPass:
		if auth == nil {
			return fmt.Errorf("usocks: upstream demands auth but none configured")
		}
		if err := socks5ClientAuth(nc, auth); err != nil {
			return err
		}
	case MethodNoAcceptable:
		return fmt.Errorf("usocks: upstream accepted no method")
	default:
		return fmt.Errorf("usocks: upstream selected unsupported method %d", sel[1])
	}
	// CONNECT request.
	req, err := connectRequest(target)
	if err != nil {
		return err
	}
	if _, err := nc.Write(req); err != nil {
		return err
	}
	// Reply.
	var head [4]byte
	if _, err := io.ReadFull(nc, head[:]); err != nil {
		return err
	}
	if head[0] != Version5 {
		return fmt.Errorf("usocks: upstream reply bad version %d", head[0])
	}
	if head[1] != RepSuccess {
		return fmt.Errorf("usocks: upstream CONNECT failed rep=%d", head[1])
	}
	return discardBoundAddr(nc, head[3])
}

func socks5ClientAuth(nc net.Conn, auth *SOCKS5Auth) error {
	if len(auth.Username) > 255 || len(auth.Password) > 255 {
		return fmt.Errorf("usocks: upstream credential too long")
	}
	msg := []byte{authVersion, byte(len(auth.Username))}
	msg = append(msg, auth.Username...)
	msg = append(msg, byte(len(auth.Password)))
	msg = append(msg, auth.Password...)
	if _, err := nc.Write(msg); err != nil {
		return err
	}
	var st [2]byte
	if _, err := io.ReadFull(nc, st[:]); err != nil {
		return err
	}
	if st[0] != authVersion || st[1] != 0 {
		return fmt.Errorf("usocks: upstream auth failed status=%d", st[1])
	}
	return nil
}

func connectRequest(target string) ([]byte, error) {
	host, portStr, err := net.SplitHostPort(target)
	if err != nil {
		return nil, err
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 0 || port > 65535 {
		return nil, fmt.Errorf("usocks: bad target port %q", portStr)
	}
	req := []byte{Version5, CmdConnect, 0}
	if ip := net.ParseIP(host); ip != nil {
		if ip4 := ip.To4(); ip4 != nil {
			req = append(req, AtypIPv4)
			req = append(req, ip4...)
		} else {
			req = append(req, AtypIPv6)
			req = append(req, ip.To16()...)
		}
	} else {
		if len(host) > 255 {
			return nil, fmt.Errorf("usocks: target domain too long")
		}
		req = append(req, AtypDomain, byte(len(host)))
		req = append(req, host...)
	}
	return binary.BigEndian.AppendUint16(req, uint16(port)), nil
}

func discardBoundAddr(nc net.Conn, atyp byte) error {
	var n int
	switch atyp {
	case AtypIPv4:
		n = 4 + 2
	case AtypIPv6:
		n = 16 + 2
	case AtypDomain:
		var l [1]byte
		if _, err := io.ReadFull(nc, l[:]); err != nil {
			return err
		}
		n = int(l[0]) + 2
	default:
		return fmt.Errorf("usocks: upstream reply bad atyp %d", atyp)
	}
	_, err := io.CopyN(io.Discard, nc, int64(n))
	return err
}
