//go:build linux && !stdio

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

package ubridge

import (
	"bytes"
	"encoding/binary"
	"io"
	"net"
	"testing"
	"time"

	"github.com/urpc/uio/internal/socket"
	"golang.org/x/sys/unix"
)

// stdEchoUpstream is a plain net echo server, so only the bridge is uio.
func stdEchoUpstream(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				_, _ = io.Copy(c, c)
			}()
		}
	}()
	return ln.Addr().String()
}

// echoThroughBridge sends size bytes in chunk-sized writes through a bridge to
// an echo upstream and reads them back without ending its side: a relay that
// stops forwarding before the end stalls the read, which fails after stall
// without progress.
func echoThroughBridge(t *testing.T, size, chunk int, stall time.Duration) {
	t.Helper()
	br := &Bridge{Upstream: stdEchoUpstream(t)}
	cli, err := net.Dial("tcp", bridgeServe(t, br.Events()))
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	_ = cli.(*net.TCPConn).SetNoDelay(true)
	want := make([]byte, size)
	for i := 0; i+8 <= size; i += 8 {
		binary.LittleEndian.PutUint64(want[i:], uint64(i))
	}
	go func() {
		for off := 0; off < size; off += chunk {
			if _, err := cli.Write(want[off:min(off+chunk, size)]); err != nil {
				return
			}
		}
	}()
	got := make([]byte, 0, size)
	buf := make([]byte, 256<<10)
	for len(got) < size {
		_ = cli.SetReadDeadline(time.Now().Add(stall))
		n, err := cli.Read(buf)
		got = append(got, buf[:n]...)
		if err != nil {
			t.Fatalf("relay stalled after %d of %d bytes: %v", len(got), size, err)
		}
	}
	if !bytes.Equal(got, want) {
		t.Fatal("relayed bytes differ from those sent")
	}
}

// The bridge splices both ways on Linux; splice(2) refuses with EAGAIN both
// when the socket is empty and when the pipe is full, and only the first may
// end a round under edge-triggered polling.
func TestBridgeSpliceRelaysWithoutHalfClose(t *testing.T) {
	for _, chunk := range []int{4 << 10, 16 << 10, 64 << 10} {
		echoThroughBridge(t, 32<<20, chunk, 5*time.Second)
	}
}

// Pipes smaller than a round asks for — the default size, which a pipe keeps
// when F_SETPIPE_SZ cannot grow it — fill on every round.
func TestBridgeSpliceRelaysThroughSmallPipes(t *testing.T) {
	defer socket.SetTestPipeSize(64 << 10)()
	p, err := socket.AcquireSplicePipe()
	if err != nil {
		t.Skipf("no splice pipe for this user: %v", err)
	}
	size, err := unix.FcntlInt(uintptr(p.W), unix.F_GETPIPE_SZ, 0)
	socket.ReleaseSplicePipe(p)
	if err != nil || size != 64<<10 {
		t.Fatalf("test pipe holds %d bytes (%v), want %d", size, err, 64<<10)
	}
	echoThroughBridge(t, 8<<20, 64<<10, 5*time.Second)
}
