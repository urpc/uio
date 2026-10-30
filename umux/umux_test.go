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

package umux

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/urpc/uio"
)

// echoServer echoes every stream and half-closes when the peer ends.
type echoServer struct {
	opened chan *Stream
	closed chan error
}

func newEchoServer() *echoServer {
	return &echoServer{opened: make(chan *Stream, 16), closed: make(chan error, 16)}
}

func (h *echoServer) OnOpen(c uio.Conn) {
	select {
	case h.opened <- c.(*Stream):
	default:
	}
}

func (h *echoServer) OnRead(c uio.Conn) error {
	st := c.(*Stream)
	buf := make([]byte, 8<<10)
	for {
		n, err := st.Read(buf)
		if n > 0 {
			if _, werr := st.Write(buf[:n]); werr != nil {
				return werr
			}
			continue
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return err
		}
		break
	}
	if st.State()&uio.StatePeerEnded != 0 {
		return st.CloseWrite()
	}
	return nil
}

func (h *echoServer) OnTimer(uio.Conn) error { return nil }

func (h *echoServer) OnClose(c uio.Conn, err error) {
	select {
	case h.closed <- err:
	default:
	}
}

// freeAddr reserves a loopback port.
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

// writeAll writes p through the stream's window, retrying what a short write
// leaves behind.
func writeAll(t *testing.T, st *Stream, p []byte) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for len(p) > 0 {
		n, err := st.Write(p)
		p = p[n:]
		if err == nil {
			continue
		}
		if !errors.Is(err, uio.ErrOutboundOverflow) {
			t.Fatalf("write: %v", err)
		}
		if len(p) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("window never reopened with %d bytes left", len(p))
		}
		time.Sleep(time.Millisecond)
	}
}

// dialTCP retries the dial until the listener accepts.
func dialTCP(t *testing.T, addr string) net.Conn {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		conn, err := net.DialTimeout("tcp", addr, time.Second)
		if err == nil {
			return conn
		}
		if time.Now().After(deadline) {
			t.Fatalf("dial %s: %v", addr, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// startPair runs a server and dials one client session.
func startPair(t *testing.T, serverCfg, clientCfg Config) (*Session, *echoServer) {
	t.Helper()
	addr := freeAddr(t)
	echo := newEchoServer()
	if serverCfg.Handler == nil {
		serverCfg.Handler = echo
	}
	srv := NewServer(serverCfg)
	go func() { _ = srv.Serve(addr) }()
	cli := NewClient(clientCfg)
	t.Cleanup(func() {
		_ = cli.Close(nil)
		_ = srv.Close(nil)
	})
	deadline := time.Now().Add(5 * time.Second)
	for {
		sess, err := cli.Dial(context.Background(), "tcp://"+addr)
		if err == nil {
			t.Cleanup(func() { _ = sess.Close(nil) })
			return sess, echo
		}
		if time.Now().After(deadline) {
			t.Fatalf("dial: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func readFull(t *testing.T, st *Stream, n int) []byte {
	t.Helper()
	buf := make([]byte, n)
	off := 0
	deadline := time.Now().Add(5 * time.Second)
	for off < n {
		m, err := st.Read(buf[off:])
		if err != nil {
			t.Fatalf("read %d/%d: %v", off, n, err)
		}
		if m > 0 {
			off += m
			continue
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out at %d/%d bytes", off, n)
		}
		time.Sleep(200 * time.Microsecond)
	}
	return buf
}

func waitEOF(t *testing.T, st *Stream) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	buf := make([]byte, 256)
	for {
		n, err := st.Read(buf)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return
			}
			t.Fatalf("read: %v", err)
		}
		if n > 0 {
			continue
		}
		if time.Now().After(deadline) {
			t.Fatal("no EOF received")
		}
		time.Sleep(200 * time.Microsecond)
	}
}

func TestEchoOverStreams(t *testing.T) {
	sess, echo := startPair(t, Config{}, Config{})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	payloads := [][]byte{
		bytes.Repeat([]byte("a"), 32),
		bytes.Repeat([]byte("b"), 4<<10),
		bytes.Repeat([]byte("c"), 96<<10),
	}
	streams := make([]*Stream, len(payloads))
	for i := range payloads {
		st, err := sess.Open(ctx)
		if err != nil {
			t.Fatalf("open %d: %v", i, err)
		}
		streams[i] = st
	}
	// Every open surfaces on the server.
	for range payloads {
		select {
		case <-echo.opened:
		case <-time.After(5 * time.Second):
			t.Fatal("server did not open the streams")
		}
	}
	for i, st := range streams {
		if _, err := st.Write(payloads[i]); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	for i, st := range streams {
		if got := readFull(t, st, len(payloads[i])); !bytes.Equal(got, payloads[i]) {
			t.Fatalf("stream %d echoed %d bytes, want %d", i, len(got), len(payloads[i]))
		}
	}
	// Half-close each stream; the echo server answers with its own FIN.
	for _, st := range streams {
		if err := st.CloseWrite(); err != nil {
			t.Fatalf("close write: %v", err)
		}
		waitEOF(t, st)
	}
}

func TestWindowPressure(t *testing.T) {
	const window = 256 << 10
	sess, _ := startPair(t, Config{WindowSize: window}, Config{})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	st, err := sess.Open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte("w"), window)
	if n, err := st.Write(payload); err != nil || n != len(payload) {
		t.Fatalf("first write = %d, %v", n, err)
	}
	// The whole window is spent: the next write must be refused, not block.
	if _, err := st.Write([]byte("x")); !errors.Is(err, uio.ErrOutboundOverflow) {
		t.Fatalf("write past the window: err = %v, want ErrOutboundOverflow", err)
	}
	// Draining the echo returns credit.
	if got := readFull(t, st, len(payload)); !bytes.Equal(got, payload) {
		t.Fatalf("echo mismatched: %d bytes", len(got))
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := st.Write([]byte("y")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("window never reopened")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestResetDeliversToPeer(t *testing.T) {
	sess, echo := startPair(t, Config{}, Config{})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	st, err := sess.Open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-echo.opened:
	case <-time.After(5 * time.Second):
		t.Fatal("server did not open the stream")
	}
	boom := errors.New("boom")
	_ = st.CloseWith(boom)
	select {
	case err := <-echo.closed:
		if !errors.Is(err, ErrStreamReset) {
			t.Fatalf("server OnClose err = %v, want ErrStreamReset", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("reset never reached the peer")
	}
}
