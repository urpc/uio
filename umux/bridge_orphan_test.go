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

// TestBridgeReleasesStreams drives the tunnel shape — a uio.Bridge whose
// upstream legs are umux streams — through clean, half-close and abort
// patterns, and asserts every stream is released once its user connection is
// gone.
func TestBridgeReleasesStreams(t *testing.T) {
	echo := newEchoServer()
	echoAddr := freeAddr(t)
	srv := NewServer(Config{Handler: echo})
	go func() { _ = srv.Serve(echoAddr) }()
	t.Cleanup(func() { _ = srv.Close(nil) })

	br := &uio.Bridge{}
	cli := NewClient(Config{Handler: br, Observer: br})
	var sess *Session
	dialDeadline := time.Now().Add(5 * time.Second)
	for {
		var err error
		sess, err = cli.Dial(context.Background(), echoAddr)
		if err == nil {
			break
		}
		if time.Now().After(dialDeadline) {
			t.Fatalf("dial: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Cleanup(func() { _ = cli.Close(nil) })
	br.Dial = func(ctx context.Context, client uio.Conn, _ uio.DialFunc) (uio.Conn, error) {
		st, err := sess.Open(ctx)
		if err != nil {
			return nil, err
		}
		br.Pair(client, st)
		return st, nil
	}
	ev := br.Events()
	userAddr := freeAddr(t)
	go func() { _ = ev.Serve("tcp://" + userAddr) }()
	t.Cleanup(func() { _ = ev.Close(nil) })

	streams := func() int {
		sess.mu.Lock()
		defer sess.mu.Unlock()
		return len(sess.streams)
	}
	waitNoStreams := func(what string) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for {
			if n := streams(); n == 0 {
				return
			} else if time.Now().After(deadline) {
				t.Fatalf("%s: %d streams still open", what, n)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	dialUser := func() *net.TCPConn {
		deadline := time.Now().Add(5 * time.Second)
		for {
			conn, err := net.Dial("tcp", userAddr)
			if err == nil {
				return conn.(*net.TCPConn)
			}
			if time.Now().After(deadline) {
				t.Fatalf("dial user: %v", err)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	payload := bytes.Repeat([]byte("x"), 32<<10)
	readN := func(c net.Conn, n int) []byte {
		_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
		buf := make([]byte, n)
		if _, err := io.ReadFull(c, buf); err != nil {
			t.Fatalf("read %d: %v", n, err)
		}
		return buf
	}

	// Clean echo, then a full close without a half-close.
	c1 := dialUser()
	if _, err := c1.Write(payload); err != nil {
		t.Fatal(err)
	}
	if got := readN(c1, len(payload)); !bytes.Equal(got, payload) {
		t.Fatal("echo mismatched")
	}
	_ = c1.Close()
	waitNoStreams("clean echo")

	// Half-close: the echo arrives, then EOF, then the user closes.
	c2 := dialUser()
	if _, err := c2.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := c2.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	if got := readN(c2, len(payload)); !bytes.Equal(got, payload) {
		t.Fatal("half-close echo mismatched")
	}
	_ = c2.SetReadDeadline(time.Now().Add(10 * time.Second))
	if n, err := c2.Read(make([]byte, 1)); n != 0 || !errors.Is(err, io.EOF) {
		t.Fatalf("half-close end: n = %d, err = %v", n, err)
	}
	_ = c2.Close()
	waitNoStreams("half close")

	// Abort.
	c3 := dialUser()
	if _, err := c3.Write(payload); err != nil {
		t.Fatal(err)
	}
	_ = c3.SetLinger(0)
	_ = c3.Close()
	waitNoStreams("abort")
}
