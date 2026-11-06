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

// Command matryoshka sends one hello world through a doll of different
// protocols, one shell inside another, over a single UDP socket:
//
//	udp ─ kcp ─ tls ─ ws ─ xor ─ kcp ─ tls ─ ws ─ tls ─ the echo doll
//
//	the client writes "hello"; the doll answers "hello world!"
//
// Every shell is a uio.Layer, and no shell knows what it wraps. KCP turns
// datagrams into a reliable stream — directly on UDP at the first ring and
// again at the fifth, where its segments self-frame on the stream below it;
// TLS wraps a stream in records; the WebSocket ring carries the rings inside
// it as binary messages; and the XOR ring is this example's own layer, a
// dozen lines long. The whole stack settles before the dial returns.
package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"math/big"
	"time"

	"github.com/urpc/uio"
	"github.com/urpc/uio/ukcp"
	"github.com/urpc/uio/utls"
	"github.com/urpc/uio/uws"
)

// shells names the doll's layers from the socket outward. Both sides run
// the same list, each through its own constructor.
var shells = []string{"kcp", "tls", "ws", "xor", "kcp", "tls", "ws", "tls"}

// serverTLS and clientTLS are shared by every TLS shell.
var (
	serverTLS = &tls.Config{Certificates: []tls.Certificate{selfSigned()}}
	clientTLS = &tls.Config{InsecureSkipVerify: true} // a doll, not a bank
)

func main() {
	events := &uio.Events{Handler: uio.FuncHandler{Read: answer}}
	serve(events)
	listener, err := events.Listen("udp://127.0.0.1:0", uio.WithLayers(chain(true)...))
	if err != nil {
		panic(err)
	}
	defer listener.Close()

	// The reply climbs the same shells, and a connection's reads belong to
	// its callbacks: the dialer's handler hands the finished reply to main.
	reply := make(chan string, 1)
	dialer := &uio.Events{Handler: uio.FuncHandler{Read: collect(reply)}}
	serve(dialer)

	// The dial unwraps nothing: the whole stack settles before DialContext
	// returns, and the connection it hands back talks to the innermost doll.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := dialer.DialContext(ctx, "udp://"+listener.Addr().String(), uio.WithLayers(chain(false)...))
	if err != nil {
		panic(err)
	}
	defer conn.Close()

	fmt.Printf("connected through %d shells; sending hello\n", len(shells))
	if _, err := conn.Write([]byte("hello")); err != nil {
		panic(err)
	}
	fmt.Printf("%d shells deep, the doll says: %s\n", len(shells), <-reply)
}

// serve runs ev and returns once it is serving: a Listen, and a layered
// Dial, both need that.
func serve(ev *uio.Events) {
	started := make(chan struct{})
	ev.Hooks = uio.FuncHandler{Start: func(*uio.Events) { close(started) }}
	go func() { _ = ev.Serve() }()
	<-started
}

// answer is the innermost doll: what arrives leaves with " world!". A ws
// shell cannot half-close, so the request is framed by its content — the
// doll answers once the whole "hello" climbed up, and the Discard leaves
// nothing for a later callback to answer again.
func answer(c uio.Conn) error {
	if c.InboundBuffered() < len("hello") {
		return nil // a piece of the request still on its way up
	}
	request := append([]byte(nil), c.PeekChunk()[:len("hello")]...)
	_, _ = c.Discard(len("hello"))
	_, err := c.Write(append(request, " world!"...))
	return err
}

// collect hands the reply to main once the whole of it arrived. One reply
// comes back, so one buffered send needs no non-blocking dance.
func collect(reply chan<- string) func(c uio.Conn) error {
	return func(c uio.Conn) error {
		received := append([]byte(nil), c.PeekChunk()...)
		_, _ = c.Discard(len(received))
		if len(received) >= len("hello world!") {
			reply <- string(received)
		}
		return nil
	}
}

// chain builds one side of the doll: the shells, next to the socket first.
func chain(server bool) []uio.Layer {
	layers := make([]uio.Layer, len(shells))
	for i, shell := range shells {
		layers[i] = layer(shell, server)
	}
	return layers
}

// layer builds one shell for one side.
func layer(shell string, server bool) uio.Layer {
	switch shell {
	case "kcp":
		if server {
			return ukcp.ServerLayer(ukcp.Options{})
		}
		return ukcp.ClientLayer(ukcp.Options{})
	case "tls":
		if server {
			return utls.ServerLayer(utls.LayerOptions{TLSConfig: serverTLS})
		}
		return utls.ClientLayer(utls.LayerOptions{TLSConfig: clientTLS})
	case "ws":
		if server {
			return uws.ServerLayer(uws.LayerOptions{})
		}
		return uws.ClientLayer(uws.LayerOptions{Target: "ws://matryoshka/"})
	case "xor":
		return xorLayer{}
	}
	panic("matryoshka: unknown shell " + shell)
}

// xorLayer is this example's own protocol: every byte crossing it is
// scrambled with a fixed mask, both ways.
type xorLayer struct{}

func (xorLayer) Attach(uio.LayerContext) uio.LayerHandler { return xorHandler{} }

type xorHandler struct{}

const mask = 0x5a

func (xorHandler) OnOpen(ctx uio.LayerContext) { ctx.Open() }

// OnRead hands what waits below up, scrambled.
func (xorHandler) OnRead(ctx uio.LayerContext) error {
	lower := ctx.Lower()
	for {
		chunk := lower.PeekChunk()
		if len(chunk) == 0 {
			return nil
		}
		if _, err := ctx.Upper().Write(xor(chunk)); err != nil {
			return err
		}
		if _, err := lower.Discard(len(chunk)); err != nil {
			return err
		}
	}
}

// OnWrite carries what the layer above wrote down, scrambled.
func (xorHandler) OnWrite(ctx uio.LayerContext, p []byte) (int, error) {
	return ctx.Lower().Write(xor(p))
}

func (xorHandler) OnTimer(uio.LayerContext) error { return nil }

func (xorHandler) OnClose(uio.LayerContext, error) {}

func xor(b []byte) []byte {
	out := make([]byte, len(b))
	for i, c := range b {
		out[i] = c ^ mask
	}
	return out
}

// selfSigned mints the one certificate every TLS shell shares.
func selfSigned() tls.Certificate {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		panic(err)
	}
	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "matryoshka"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		DNSNames:     []string{"matryoshka"},
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		panic(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		panic(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}
}
