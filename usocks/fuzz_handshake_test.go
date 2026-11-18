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
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/urpc/uio"
)

var (
	fuzzOnce sync.Once
	fuzzAddr string
)

// startFuzzServer starts a no-auth SOCKS5 proxy and an echo target once, shared
// across fuzz executions.
func startFuzzServer() string {
	started := make(chan struct{})
	target := &uio.Events{
		Handler: uio.FuncHandler{Read: func(c uio.Conn) error {
			_, err := c.WriteTo(c)
			return err
		}},
		Hooks: uio.FuncHandler{Start: func(*uio.Events) { close(started) }},
	}
	go target.Serve()
	<-started
	if _, err := target.Listen("tcp://127.0.0.1:0"); err != nil {
		panic(err)
	}

	srv := NewServer(Options{HandshakeTimeout: 200 * time.Millisecond})
	go srv.Events().Serve()
	for i := 0; i < 500; i++ {
		ln, err := srv.Listen("tcp://127.0.0.1:0")
		if err == nil {
			return ln.Addr().String()
		}
		time.Sleep(time.Millisecond)
	}
	panic("fuzz server did not start")
}

// FuzzHandshake drives the full handshake state machine with arbitrary byte
// streams over a real connection: the server must never panic or hang on
// malformed, fragmented, or oversized input. Writes are fragmented to exercise
// cross-read accumulation, and the client outlasts the server's handshake
// timeout so the timeout path runs too. This is a crash/race gate; the parsers
// are fuzzed separately at high volume.
func FuzzHandshake(f *testing.F) {
	f.Add([]byte{Version5, 1, MethodNoAuth, Version5, CmdConnect, 0, AtypIPv4, 127, 0, 0, 1, 0, 80})
	f.Add([]byte{Version5, 1, MethodNoAuth})
	f.Add([]byte{Version5, 255})
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, data []byte) {
		fuzzOnce.Do(func() { fuzzAddr = startFuzzServer() })
		c, err := net.Dial("tcp", fuzzAddr)
		if err != nil {
			t.Fatalf("fuzz server unreachable (infrastructure death, not bad input): %v", err)
		}
		defer c.Close()
		_ = c.SetDeadline(time.Now().Add(300 * time.Millisecond)) // > server's 200ms handshake timeout
		for i := 0; i < 3 && len(data) > 0; i++ {
			end := len(data) / (3 - i)
			if end == 0 {
				end = len(data)
			}
			if _, err := c.Write(data[:end]); err != nil {
				break
			}
			data = data[end:]
		}
		_, _ = io.Copy(io.Discard, c)
	})
}
