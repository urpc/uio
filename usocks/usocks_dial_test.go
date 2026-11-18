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
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

// A front proxy dials its target through an upstream SOCKS5 proxy (a two-level
// chain): client -> front -> upstream -> target, and the echo returns.
func TestProxyChain(t *testing.T) {
	target := startEcho(t)
	upstream := startSocks(t) // direct-dial upstream proxy
	front := startSocksOpts(t, Options{
		Dial: func(ctx context.Context, target string) (net.Conn, error) {
			return DialSOCKS5(ctx, upstream, target, nil)
		},
	})
	host, port := splitAddr(t, target)
	c := socksConnect(t, front, host, port, AtypIPv4)
	msg := []byte("through two chained proxies")
	if _, err := c.Write(msg); err != nil {
		t.Fatal(err)
	}
	if got := readN(t, c, len(msg)); !bytes.Equal(got, msg) {
		t.Fatalf("echo = %q, want %q", got, msg)
	}
}

// The chain works when the upstream proxy requires username/password auth.
func TestProxyChainAuth(t *testing.T) {
	target := startEcho(t)
	upstream := startSocksOpts(t, Options{Authenticator: &fakeAuth{}})
	front := startSocksOpts(t, Options{
		Dial: func(ctx context.Context, target string) (net.Conn, error) {
			return DialSOCKS5(ctx, upstream, target, &SOCKS5Auth{Username: "u", Password: "p"})
		},
	})
	host, port := splitAddr(t, target)
	c := socksConnect(t, front, host, port, AtypIPv4)
	msg := []byte("authenticated chain")
	if _, err := c.Write(msg); err != nil {
		t.Fatal(err)
	}
	if got := readN(t, c, len(msg)); !bytes.Equal(got, msg) {
		t.Fatalf("echo = %q, want %q", got, msg)
	}
}

// A larger payload crosses the chain intact (both relay hops).
func TestProxyChainLargeRelay(t *testing.T) {
	target := startEcho(t)
	upstream := startSocks(t)
	front := startSocksOpts(t, Options{
		Dial: func(ctx context.Context, target string) (net.Conn, error) {
			return DialSOCKS5(ctx, upstream, target, nil)
		},
	})
	host, port := splitAddr(t, target)
	c := socksConnect(t, front, host, port, AtypIPv4)
	payload := make([]byte, 256<<10)
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
	if _, err := io.ReadFull(c, got); err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatal("chain relay corrupted the payload")
	}
}

// DialSOCKS5 against an upstream that refuses the target reports the failure.
func TestDialSOCKS5UpstreamRefused(t *testing.T) {
	upstream := startSocksOpts(t, Options{Policy: &fakePolicy{err: errors.New("denied")}})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := DialSOCKS5(ctx, upstream, "127.0.0.1:9", nil)
	if err == nil {
		t.Fatal("DialSOCKS5 succeeded though the upstream denied the target")
	}
}

// The chain carries a domain-name target (the client handshake encodes ATYP
// domain, the upstream resolves it).
func TestProxyChainDomainTarget(t *testing.T) {
	target := startEcho(t)
	upstream := startSocks(t)
	front := startSocksOpts(t, Options{
		Dial: func(ctx context.Context, target string) (net.Conn, error) {
			return DialSOCKS5(ctx, upstream, target, nil)
		},
	})
	_, port := splitAddr(t, target)
	c := socksConnect(t, front, "localhost", port, AtypDomain)
	msg := []byte("domain target through the chain")
	if _, err := c.Write(msg); err != nil {
		t.Fatal(err)
	}
	if got := readN(t, c, len(msg)); !bytes.Equal(got, msg) {
		t.Fatalf("echo = %q, want %q", got, msg)
	}
}

// DialSOCKS5 surfaces an upstream authentication failure.
func TestDialSOCKS5AuthFailure(t *testing.T) {
	upstream := startSocksOpts(t, Options{Authenticator: &fakeAuth{err: errors.New("bad credentials")}})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := DialSOCKS5(ctx, upstream, "127.0.0.1:9", &SOCKS5Auth{Username: "u", Password: "wrong"})
	if err == nil {
		t.Fatal("DialSOCKS5 succeeded though upstream auth failed")
	}
}
