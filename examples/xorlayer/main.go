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

// Xorlayer shows uio's protocol layers with a stack of two, on both ends of
// one connection in this process:
//
//	       client                               server
//	handler: Write("hello")              handler: reads "hello", echoes it
//	   │                                       ▲
//	layer 2 xor:     plaintext → ciphertext   ciphertext → plaintext
//	   │                                       ▲
//	layer 1 wiretap: prints the bytes        prints the bytes
//	   │                                       ▲
//	socket ═══════════════ wire ═══════════════ socket
//
// The first layer installed sits next to the socket, so the wiretap prints
// what really crosses the wire — the XOR'd bytes — while the handlers above
// the xor layer read and write plaintext. The echo travels back the same way,
// through the same two layers in reverse.
//
// Run it:
//
//	go run ./xorlayer -key 0x5a
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/urpc/uio"
)

// xorLayer obfuscates a connection's bytes with a one-byte key, both ways.
// It is the Layer: configured once, it builds one xorConn per connection.
type xorLayer struct{ key byte }

func (l xorLayer) Attach(uio.LayerContext) uio.LayerConn { return &xorConn{key: l.key} }

// xorConn is the layer on one connection. PassThrough gives it the callbacks
// it does not change — OnOpen opens the layer above at once, OnTimer and
// OnClose do nothing — so it writes only the two that transform bytes.
type xorConn struct {
	uio.PassThrough
	key byte
}

// OnInbound runs when bytes arrived from below: it XORs them straight into
// the space the layer above reads from, and consumes them below.
func (x *xorConn) OnInbound(ctx uio.LayerContext) error {
	lower := ctx.Lower()
	chunk := lower.PeekChunk()
	if len(chunk) == 0 {
		return nil
	}
	out := ctx.Space(len(chunk))[:len(chunk)]
	for i, b := range chunk {
		out[i] = b ^ x.key
	}
	ctx.Commit(len(chunk)) // hand the plaintext up
	_, err := lower.Discard(len(chunk))
	return err
}

// OnOutbound runs when the layer above writes: it XORs the bytes and writes
// them below. p belongs to the writer, so the result goes in a buffer of its
// own.
func (x *xorConn) OnOutbound(ctx uio.LayerContext, p []byte) (int, error) {
	out := make([]byte, len(p))
	for i, b := range p {
		out[i] = b ^ x.key
	}
	if _, err := ctx.Write(out); err != nil {
		return 0, err
	}
	return len(p), nil
}

// wiretap prints the bytes that cross it and changes none of them.
type wiretap struct{ name string }

func (w wiretap) Attach(uio.LayerContext) uio.LayerConn { return tapConn{name: w.name} }

type tapConn struct {
	uio.PassThrough
	name string
}

func (t tapConn) OnInbound(ctx uio.LayerContext) error {
	if chunk := ctx.Lower().PeekChunk(); len(chunk) > 0 {
		fmt.Printf("%-6s wire in   % x\n", t.name, chunk)
	}
	return t.PassThrough.OnInbound(ctx) // deliver them as they are
}

func (t tapConn) OnOutbound(ctx uio.LayerContext, p []byte) (int, error) {
	fmt.Printf("%-6s wire out  % x\n", t.name, p)
	return ctx.Write(p)
}

func main() {
	key := flag.Uint("key", 0x5a, "the XOR key, one byte")
	flag.Parse()
	stack := func(name string) []uio.Layer {
		// Bottom up: the first installed sits next to the socket.
		return []uio.Layer{wiretap{name: name}, xorLayer{key: byte(*key)}}
	}

	// The server echoes what its handler reads: plaintext, above the layers.
	server := &uio.Events{Pollers: 1, Handler: uio.FuncHandler{
		Open: func(uio.Conn) {},
		Data: func(c uio.Conn) error {
			chunk := c.PeekChunk()
			if len(chunk) == 0 {
				return nil
			}
			fmt.Printf("server handler reads %q, echoes it\n", chunk)
			if _, err := c.Write(chunk); err != nil {
				return err
			}
			_, err := c.Discard(len(chunk))
			return err
		},
		Close: func(uio.Conn, error) {},
	}}
	server.Use(stack("server")...)
	start(server)
	defer server.Close(nil)
	listener, err := server.Listen("tcp://127.0.0.1:0", nil)
	if err != nil {
		fail(err)
	}

	// The client prints the echoes its handler reads.
	echoes := make(chan string, 1)
	client := &uio.Events{Pollers: 1, Handler: uio.FuncHandler{
		Open: func(uio.Conn) {},
		Data: func(c uio.Conn) error {
			chunk := c.PeekChunk()
			if len(chunk) == 0 {
				return nil
			}
			echoes <- string(chunk)
			_, err := c.Discard(len(chunk))
			return err
		},
		Close: func(uio.Conn, error) {},
	}}
	client.Use(stack("client")...)
	start(client)
	defer client.Close(nil)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// The connection DialContext returns is the top layer's: what is written
	// to it passes the xor layer, then the wiretap, then the socket.
	conn, err := client.DialContext(ctx, "tcp://"+listener.Addr().String(), nil)
	if err != nil {
		fail(err)
	}
	fmt.Printf("key 0x%02x\n", byte(*key))
	for _, message := range []string{"hello", "layers"} {
		fmt.Printf("\nclient handler writes %q\n", message)
		if _, err := conn.Write([]byte(message)); err != nil {
			fail(err)
		}
		select {
		case echo := <-echoes:
			fmt.Printf("client handler reads %q\n", echo)
		case <-ctx.Done():
			fail(ctx.Err())
		}
	}
	_ = conn.Close()
}

// start runs events in the background and returns once it serves.
func start(events *uio.Events) {
	started := make(chan struct{})
	events.Hooks = uio.FuncHandler{Start: func(*uio.Events) { close(started) }}
	go func() { _ = events.Serve() }()
	<-started
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
