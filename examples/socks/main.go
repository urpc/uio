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

// Command socks runs a SOCKS5 proxy server (RFC 1928, RFC 1929) built on uio
// and ubridge: the usocks handshake layer negotiates and authenticates each
// client, then ubridge relays it to the requested target with backpressure and
// half-close passthrough.
//
// Run it from the examples directory:
//
//	go run ./socks -listen :1080
//	go run ./socks -listen :1080 -auth alice:secret   # require username/password
//
// Point any SOCKS5 client at it, e.g.:
//
//	curl --socks5-hostname 127.0.0.1:1080 https://example.com
//	ssh -D 1080 ...   # or a browser's SOCKS proxy setting
//
//	client ── SOCKS5 handshake ──► usocks layer ── CONNECT(target) ──► dial
//	client ◄══════════════ ubridge relay (backpressure) ══════════════► target
//
// Governance is pluggable: -auth wires a static username/password Authenticator
// (compared in constant time). Policy (allow/deny targets) and Limiter (quotas)
// are the same kind of hook — fill usocks.Options with your own implementations;
// concrete rule engines and hot-reload belong to the business layer, not here.
package main

import (
	"context"
	"crypto/subtle"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/urpc/uio/usocks"
)

func main() {
	listen := flag.String("listen", ":1080", "SOCKS5 listen address")
	auth := flag.String("auth", "", "require username/password auth (user:pass); empty allows no-auth")
	handshakeTimeout := flag.Duration("handshake-timeout", 10*time.Second, "bounds the SOCKS handshake (Slowloris defense)")
	dialTimeout := flag.Duration("dial-timeout", 10*time.Second, "bounds each target dial")
	maxDials := flag.Int("max-concurrent-dials", 0, "cap in-flight target dials; 0 = unlimited")
	flag.Parse()

	opts := usocks.Options{
		HandshakeTimeout:   *handshakeTimeout,
		DialTimeout:        *dialTimeout,
		MaxConcurrentDials: *maxDials,
	}
	if *auth != "" {
		user, pass, ok := strings.Cut(*auth, ":")
		if !ok || user == "" {
			fmt.Println("invalid -auth, want user:pass")
			os.Exit(1)
		}
		opts.Authenticator = staticAuth{user: user, pass: pass}
	}

	srv := usocks.NewServer(opts)

	addr := *listen
	if !strings.Contains(addr, "://") {
		addr = "tcp://" + addr
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sig
		fmt.Println("shutting down")
		_ = srv.Close()
	}()

	fmt.Printf("socks5 proxy listening on %s (auth=%t handshake-timeout=%s dial-timeout=%s max-concurrent-dials=%d)\n",
		addr, opts.Authenticator != nil, *handshakeTimeout, *dialTimeout, *maxDials)

	if err := srv.Serve(addr); err != nil {
		fmt.Println("socks exited:", err)
		os.Exit(1)
	}
	_ = srv.Wait()
}

// staticAuth is a minimal Authenticator: one fixed credential pair, compared in
// constant time. A real deployment plugs in its own credential store here.
type staticAuth struct{ user, pass string }

func (a staticAuth) Authenticate(_ context.Context, user, pass string) error {
	u := subtle.ConstantTimeCompare([]byte(user), []byte(a.user))
	p := subtle.ConstantTimeCompare([]byte(pass), []byte(a.pass))
	if u == 1 && p == 1 {
		return nil
	}
	return errors.New("invalid credentials")
}
