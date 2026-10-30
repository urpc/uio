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

// Xorlayer shows a uio layer stack with a plain TCP peer that does the same
// XOR by hand, so what each layer does is visible from both ends of the wire:
//
//	     plain client                      server (uio)
//	net.Dial, XOR by hand     handler: reads "hello", echoes it
//	       │                                  ▲
//	       │                   layer 2 xor: ciphertext ⇄ plaintext
//	       │                                  ▲
//	       │                 layer 1 wiretap: prints every byte
//	       │                                  ▲
//	       └────────────── wire ──────────────┘ socket
//
// The first layer installed sits next to the socket, so the wiretap prints
// what actually crosses the wire — the XOR'd bytes — while the handler above
// the xor layer reads and writes plaintext. The echo travels back through
// both layers in reverse. A uio client installs the same stack with
// uio.WithLayers on its Dial; this example uses a plain peer so the wire is
// visible on both sides.
//
// Run it:
//
//	go run ./xorlayer
package main

import (
	"fmt"
	"io"
	"log"
	"net"

	"github.com/urpc/uio"
)

// key is the one-byte XOR key; the client applies it by hand.
const key = 0x5a

func xor(b []byte) []byte {
	out := make([]byte, len(b))
	for i, c := range b {
		out[i] = c ^ key
	}
	return out
}

// xorLayer is the Layer: configured once, it builds one LayerHandler per
// connection. It transforms bytes both ways.
type xorLayer struct{}

func (xorLayer) Attach(uio.LayerContext) uio.LayerHandler { return xorConn{} }

// xorConn writes only the two callbacks that transform bytes; PassThrough
// supplies the rest (OnOpen opens the layer above at once).
type xorConn struct{ uio.PassThrough }

// OnRead runs when bytes arrived from below: it XORs them into the space the
// layer above reads from, and consumes them below.
func (xorConn) OnRead(ctx uio.LayerContext) error {
	lower := ctx.Down()
	chunk := lower.PeekChunk()
	if len(chunk) == 0 {
		return nil
	}
	out := ctx.Up().Reserve(len(chunk))[:len(chunk)]
	copy(out, xor(chunk))
	ctx.Up().Commit(len(chunk))
	_, err := lower.Discard(len(chunk))
	return err
}

// OnWrite runs when the layer above writes: it XORs the bytes and writes
// them below.
func (xorConn) OnWrite(ctx uio.LayerContext, p []byte) (int, error) {
	if _, err := ctx.Down().Write(xor(p)); err != nil {
		return 0, err
	}
	return len(p), nil
}

// wiretap prints the bytes that cross it and changes none of them.
type wiretap struct{}

func (wiretap) Attach(uio.LayerContext) uio.LayerHandler { return tapConn{} }

type tapConn struct{ uio.PassThrough }

func (t tapConn) OnRead(ctx uio.LayerContext) error {
	if chunk := ctx.Down().PeekChunk(); len(chunk) > 0 {
		fmt.Printf("wire in   % x\n", chunk)
	}
	return t.PassThrough.OnRead(ctx) // deliver them as they are
}

func (t tapConn) OnWrite(ctx uio.LayerContext, p []byte) (int, error) {
	fmt.Printf("wire out  % x\n", p)
	return ctx.Down().Write(p)
}

func main() {
	// The server echoes what its handler reads: plaintext, above the layers.
	// WriteTo(c) moves the inbound bytes to this same connection, so the echo
	// travels back down through the xor layer and the wiretap.
	events := &uio.Events{Handler: uio.FuncHandler{Read: func(c uio.Conn) error {
		_, err := c.WriteTo(c)
		return err
	}}}

	// Events.Listen/Dial need the runtime up: serve first, join its start.
	started := make(chan struct{})
	events.Hooks = uio.FuncHandler{Start: func(*uio.Events) { close(started) }}
	go func() { _ = events.Serve() }()
	<-started
	defer events.Close(nil)

	// Bottom up: the first layer installed sits next to the socket.
	listener, err := events.Listen("tcp://127.0.0.1:0",
		uio.WithLayers(wiretap{}, xorLayer{}))
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("server on %s, wire key 0x%02x\n", listener.Addr(), key)

	// A plain TCP client, XORing by hand: the bytes on the wire are exactly
	// what the xor layer produces on the server.
	conn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close()

	message := []byte("hello")
	fmt.Printf("client writes %q, wire % x\n", message, xor(message))
	if _, err := conn.Write(xor(message)); err != nil {
		log.Fatal(err)
	}
	echo := make([]byte, len(message))
	if _, err := io.ReadFull(conn, echo); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("client reads %q from wire % x\n", xor(echo), echo)
}
