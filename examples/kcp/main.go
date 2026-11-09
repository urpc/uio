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

// Command kcp is the smallest KCP hello-world example: a UDP listener and a
// UDP dialer each attach ukcp as a uio.Layer, turning unreliable datagrams into
// a reliable byte stream.
//
// Run it from the examples directory:
//
//	go run ./kcp
package main

import (
	"fmt"
	"log"

	"github.com/urpc/uio"
	"github.com/urpc/uio/ukcp"
)

func main() {
	// 1. Server: listen on UDP with ukcp.ServerLayer attached.
	serverReady := make(chan struct{})
	server := &uio.Events{
		Hooks: uio.FuncHandler{
			Start: func(*uio.Events) { close(serverReady) },
		},
		Handler: uio.FuncHandler{
			Read: func(c uio.Conn) error {
				if c.InboundBuffered() == 0 {
					return nil
				}
				msg := make([]byte, c.InboundBuffered())
				_, _ = c.Read(msg)
				fmt.Printf("[server] received %q from %s\n", msg, c.RemoteAddr())
				_, err := c.Write(append(msg, " world!"...))
				return err
			},
		},
	}
	go func() {
		if err := server.Serve(); err != nil {
			log.Fatal(err)
		}
	}()
	<-serverReady
	defer server.Close(nil)

	ln, err := server.Listen("udp://127.0.0.1:0", uio.WithLayers(ukcp.ServerLayer(ukcp.Options{})))
	if err != nil {
		log.Fatal(err)
	}
	defer ln.Close()

	// 2. Client: dial UDP with ukcp.ClientLayer and exchange "hello" -> "hello world!".
	clientReady := make(chan struct{})
	replyCh := make(chan string, 1)
	client := &uio.Events{
		Hooks: uio.FuncHandler{
			Start: func(*uio.Events) { close(clientReady) },
		},
		Handler: uio.FuncHandler{
			Read: func(c uio.Conn) error {
				if c.InboundBuffered() == 0 {
					return nil
				}
				reply := make([]byte, c.InboundBuffered())
				_, _ = c.Read(reply)
				replyCh <- string(reply)
				return nil
			},
		},
	}
	go func() {
		if err := client.Serve(); err != nil {
			log.Fatal(err)
		}
	}()
	<-clientReady
	defer client.Close(nil)

	conn, err := client.Dial("udp://"+ln.Addr().String(), uio.WithLayers(ukcp.ClientLayer(ukcp.Options{})))
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close()

	if _, err := conn.Write([]byte("hello")); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("[client] received %q\n", <-replyCh)
}
