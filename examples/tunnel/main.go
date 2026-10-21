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

// Tunnel forwards every connection it accepts to an upstream TCP server: a
// relay with end-to-end backpressure, half-close passthrough and aborts that
// stay aborts.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sync/atomic"

	"github.com/urpc/uio"
)

// leg pairs the two connections of one tunnel: the accepted client leg and
// the dialed upstream leg point at each other.
type leg struct {
	peer atomic.Value // uio.Conn
}

func main() {
	listen := flag.String("listen", ":9528", "address the tunnel listens on")
	upstream := flag.String("upstream", "127.0.0.1:9527", "address of the upstream server")
	pollers := flag.Int("pollers", 0, "event loops, 0 for the uio default")
	bufferSize := flag.Int("buffer", 64<<10, "bytes per socket read")
	inbound := flag.Int("inbound", 256<<10, "unconsumed bytes that pause a leg's reads")
	flag.Parse()

	events := &uio.Events{
		Pollers:            *pollers,
		MaxBufferSize:      *bufferSize,
		MaxInboundBuffered: *inbound,
		AllowHalfOpen:      true,
	}
	peerOf := func(c uio.Conn) uio.Conn {
		l, _ := c.Userdata().(*leg)
		if l == nil {
			return nil
		}
		p, _ := l.peer.Load().(uio.Conn)
		return p
	}

	events.OnOpen = func(c uio.Conn) {
		if c.Userdata() != nil {
			return // the upstream leg, paired by the client leg's OnOpen
		}
		client := &leg{}
		c.SetUserdata(client)
		up, err := events.Dial("tcp://"+*upstream, &leg{})
		if err != nil {
			fmt.Println("dial", *upstream, "failed:", err)
			// A failed dial reaches the client as a reset, as a proxy would.
			_ = c.SetLinger(0)
			_ = c.CloseWith(err)
			return
		}
		up.Userdata().(*leg).peer.Store(c)
		client.peer.Store(up)
		// The upstream may have spoken first and found no peer yet; its bytes
		// wait unconsumed, so have them forwarded now.
		_ = up.Wake()
	}

	events.OnData = func(c uio.Conn) error {
		peer := peerOf(c)
		if peer == nil {
			return nil // the upstream is not up yet: hold
		}
		// Forward unless the peer's queue is backed up. Leaving the bytes
		// unconsumed is the backpressure: once enough of them pile up, uio
		// stops reading this leg and TCP flow control slows whoever feeds it.
		if peer.OutboundBuffered() < *inbound {
			if _, err := c.WriteTo(peer); err != nil {
				return err
			}
		}
		// Pass the end of the stream on. Check it even when the peer was
		// backed up: bytes left unconsumed reach OnData again anyway.
		if _, err := c.Read(nil); err == io.EOF {
			return peer.CloseWrite()
		}
		return nil
	}

	events.OnOutbound = func(c uio.Conn, _ int) {
		// The far leg may have stopped reading because this queue was full;
		// wake it once enough of the queue has drained.
		if c.OutboundBuffered() < *inbound/2 {
			if peer := peerOf(c); peer != nil {
				_ = peer.Wake()
			}
		}
	}

	events.OnClose = func(c uio.Conn, err error) {
		peer := peerOf(c)
		if peer == nil || errors.Is(err, io.EOF) {
			return // a graceful close: both halves already went on to the peer
		}
		// An abort: pass it on as an abort, not as a clean end of stream.
		_ = peer.SetLinger(0)
		_ = peer.Close()
	}

	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt)
		<-sig
		_ = events.Close(nil)
	}()

	fmt.Println("tunnel", *listen, "->", *upstream)
	if err := events.Serve(*listen); err != nil {
		fmt.Println("server exited with error:", err)
	}
}
