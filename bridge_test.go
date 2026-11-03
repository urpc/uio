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

package uio

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

// bridgeFreeAddr reserves a loopback port for a listener.
func bridgeFreeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

// bridgeEchoHandler echoes and half-closes when its peer ends.
var bridgeEchoHandler = FuncHandler{Read: func(c Conn) error {
	buf := make([]byte, 16<<10)
	for {
		n, err := c.Read(buf)
		if n > 0 {
			if _, werr := c.Write(buf[:n]); werr != nil {
				return werr
			}
			continue
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		break
	}
	if c.State()&StatePeerEnded != 0 {
		return c.CloseWrite()
	}
	return nil
}}

// startBridgeEcho returns its upstream address once the listener is bound.
func startBridgeEcho(t *testing.T, handler Handler) string {
	t.Helper()
	if handler == nil {
		handler = bridgeEchoHandler
	}
	ready := make(chan struct{})
	ev := &Events{
		Handler:       handler,
		AllowHalfOpen: true,
		Hooks:         FuncHandler{Start: func(*Events) { close(ready) }},
	}
	addr := bridgeFreeAddr(t)
	stopped := make(chan struct{})
	var serveErr error
	t.Cleanup(func() {
		_ = ev.Close(nil)
		select {
		case <-stopped:
		case <-time.After(5 * time.Second):
		}
	})
	go func() {
		defer close(stopped)
		serveErr = ev.Serve("tcp://" + addr)
	}()
	select {
	case <-ready:
	case <-stopped:
		t.Fatalf("upstream Serve exited before startup: %v", serveErr)
	case <-time.After(5 * time.Second):
		t.Fatal("upstream did not start")
	}
	return addr
}

// bridgeDial retries until the listener accepts.
func bridgeDial(t *testing.T, addr string) *net.TCPConn {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		conn, err := net.DialTimeout("tcp", addr, time.Second)
		if err == nil {
			return conn.(*net.TCPConn)
		}
		if time.Now().After(deadline) {
			t.Fatalf("dial %s: %v", addr, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// readN reads exactly n bytes or fails.
func readN(t *testing.T, conn net.Conn, n int) []byte {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	buf := make([]byte, n)
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("read %d bytes: %v", n, err)
	}
	return buf
}

func TestBridgeRelaysBothWays(t *testing.T) {
	upstream := startBridgeEcho(t, nil)
	br := &Bridge{Upstream: upstream}
	ev := br.Events()
	addr := bridgeFreeAddr(t)
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		_ = ev.Serve("tcp://" + addr)
	}()
	t.Cleanup(func() {
		_ = ev.Close(nil)
		<-stopped
	})

	conn := bridgeDial(t, addr)
	defer conn.Close()
	for _, size := range []int{16, 1 << 20} {
		payload := bytes.Repeat([]byte("abcdefgh"), size/8)
		payload[0], payload[len(payload)-1] = 'A', 'Z'
		if _, err := conn.Write(payload); err != nil {
			t.Fatalf("write %d: %v", size, err)
		}
		if got := readN(t, conn, len(payload)); !bytes.Equal(got, payload) {
			t.Fatalf("echo of %d bytes mismatched", size)
		}
	}
	// Half-close travels to the upstream and back.
	if err := conn.CloseWrite(); err != nil {
		t.Fatalf("close write: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.ReadFull(conn, make([]byte, 1)); err != io.EOF {
		t.Fatalf("read after half-close = %v, want EOF", err)
	}
}

func TestBridgeDialOverride(t *testing.T) {
	first := startBridgeEcho(t, nil)
	second := startBridgeEcho(t, nil)
	// Dial runs on a uio goroutine; the echo reaching the client orders
	// nothing in Go's memory model (the stdio backend writes it without the
	// race detector's syscall annotation), so its report travels by channel.
	type dialSeen struct {
		client bool
		to     string
	}
	seen := make(chan dialSeen, 1)
	br := &Bridge{
		Upstream: first, // wrong on purpose: Dial must win
		Dial: func(ctx context.Context, client Conn, dial DialFunc) (Conn, error) {
			seen <- dialSeen{client: client != nil, to: second}
			return dial(ctx, second)
		},
	}
	ev := br.Events()
	addr := bridgeFreeAddr(t)
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		_ = ev.Serve("tcp://" + addr)
	}()
	t.Cleanup(func() {
		_ = ev.Close(nil)
		<-stopped
	})
	conn := bridgeDial(t, addr)
	defer conn.Close()
	payload := bytes.Repeat([]byte("route"), 1024)
	if _, err := conn.Write(payload); err != nil {
		t.Fatal(err)
	}
	if got := readN(t, conn, len(payload)); !bytes.Equal(got, payload) {
		t.Fatal("dial-override echo mismatched")
	}
	select {
	case got := <-seen:
		if !got.client || got.to != second {
			t.Fatalf("Dial saw client=%v dialedTo=%q", got.client, got.to)
		}
	default:
		t.Fatal("the echo arrived without Dial having run")
	}
}

// abortUpstream closes the connection with an error on its first read.
var abortUpstream = FuncHandler{Read: func(c Conn) error {
	_ = c.SetLinger(0)
	return errors.New("upstream aborted")
}}

func TestBridgePassesAbortOn(t *testing.T) {
	upstream := startBridgeEcho(t, abortUpstream)
	br := &Bridge{Upstream: upstream}
	ev := br.Events()
	addr := bridgeFreeAddr(t)
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		_ = ev.Serve("tcp://" + addr)
	}()
	t.Cleanup(func() {
		_ = ev.Close(nil)
		<-stopped
	})
	conn := bridgeDial(t, addr)
	defer conn.Close()
	if _, err := conn.Write([]byte("trigger")); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(conn, make([]byte, 1)); err == nil {
		t.Fatal("client read succeeded after the upstream abort")
	}
}

func TestBridgeAbortResetsUpstream(t *testing.T) {
	closed := make(chan error, 4)
	upstream := startBridgeEcho(t, FuncHandler{
		Read: bridgeEchoHandler.Read,
		Close: func(_ Conn, err error) {
			select {
			case closed <- err:
			default:
			}
		},
	})
	br := &Bridge{Upstream: upstream}
	ev := br.Events()
	addr := bridgeFreeAddr(t)
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		_ = ev.Serve("tcp://" + addr)
	}()
	t.Cleanup(func() {
		_ = ev.Close(nil)
		<-stopped
	})
	conn := bridgeDial(t, addr)
	if _, err := conn.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	_ = readN(t, conn, 1) // the pair is up
	_ = conn.SetLinger(0)
	_ = conn.Close() // reset, not a half-close
	select {
	case err := <-closed:
		if err == nil || errors.Is(err, io.EOF) {
			t.Fatalf("upstream close error = %v, want a reset", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("upstream leg never closed after the client reset")
	}
}

func TestBridgeWithoutUpstreamResetsClient(t *testing.T) {
	br := &Bridge{}
	ev := br.Events()
	addr := bridgeFreeAddr(t)
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		_ = ev.Serve("tcp://" + addr)
	}()
	t.Cleanup(func() {
		_ = ev.Close(nil)
		<-stopped
	})
	conn := bridgeDial(t, addr)
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(conn, make([]byte, 1)); err == nil {
		t.Fatal("client read succeeded without an upstream")
	}
}
