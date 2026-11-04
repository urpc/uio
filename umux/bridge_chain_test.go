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

// streamEcho answers a umux stream with an echo until the peer half-closes.
var streamEcho = uio.FuncHandler{Read: func(c uio.Conn) error {
	buf := make([]byte, 8<<10)
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
	if c.State()&uio.StatePeerEnded != 0 {
		return c.CloseWrite()
	}
	return nil
}}

// serveWait runs an Events and waits until its loops are up, so dials that
// follow cannot race the start.
func serveWait(ev *uio.Events, addrs ...string) <-chan struct{} {
	started := make(chan struct{})
	hooks := ev.Hooks
	ev.Hooks = uio.FuncHandler{Start: func(e *uio.Events) {
		if hooks != nil {
			hooks.OnStart(e)
		}
		close(started)
	}, Stop: func(e *uio.Events) {
		if hooks != nil {
			hooks.OnStop(e)
		}
	}}
	go func() { _ = ev.Serve(addrs...) }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		panic("events did not start")
	}
	return started
}

// tcpEcho is the local service the agent-side bridge dials.
func startTCPEcho(t *testing.T) string {
	t.Helper()
	ev := &uio.Events{Handler: streamEcho, AllowHalfOpen: true}
	addr := freeAddr(t)
	stopped := make(chan struct{})
	started := serveWaitWithStop(ev, &stopped, "tcp://"+addr)
	_ = started
	t.Cleanup(func() { _ = ev.Close(nil); <-stopped })
	return addr
}

func serveWaitWithStop(ev *uio.Events, stopped *chan struct{}, addrs ...string) <-chan struct{} {
	started := make(chan struct{})
	hooks := ev.Hooks
	ev.Hooks = uio.FuncHandler{Start: func(e *uio.Events) {
		if hooks != nil {
			hooks.OnStart(e)
		}
		close(started)
	}, Stop: func(e *uio.Events) {
		if hooks != nil {
			hooks.OnStop(e)
		}
	}}
	go func() { defer close(*stopped); _ = ev.Serve(addrs...) }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		panic("events did not start")
	}
	return started
}

// dialRetry connects once the listener accepts.
func dialRetry(addr string) (net.Conn, error) {
	var last error
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, time.Second)
		if err == nil {
			return conn, nil
		}
		last = err
		time.Sleep(10 * time.Millisecond)
	}
	return nil, last
}

func readEchoUntilEOF(t *testing.T, conn net.Conn) []byte {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	var out bytes.Buffer
	if _, err := io.Copy(&out, conn); err != nil {
		t.Fatalf("read: %v", err)
	}
	return out.Bytes()
}

// TestBridgeRelayHop drives the relay shape: user sockets bridged onto streams
// a Dial override opens and pairs.
func TestBridgeRelayHop(t *testing.T) {
	br := &uio.Bridge{}
	// The relay's transport session runs the bridge as its handler, so the
	// streams it opens report their reads back to the bridge.
	srvAddr := freeAddr(t)
	srv := NewServer(Config{Handler: br, Observer: br})
	go func() { _ = srv.Serve(srvAddr) }()
	t.Cleanup(func() { _ = srv.Close(nil) })
	// The far end echoes the streams it accepts.
	cli := NewClient(Config{Handler: streamEcho})
	t.Cleanup(func() { _ = cli.Close(nil) })
	var far *Session
	waitFor(t, "client session", func() bool {
		s, err := cli.Dial(context.Background(), "tcp://"+srvAddr)
		far = s
		return err == nil
	})
	_ = far
	var sess *Session
	select {
	case sess = <-srv.Sessions():
	case <-time.After(5 * time.Second):
		t.Fatal("relay session missing")
	}

	br.Dial = func(ctx context.Context, _ uio.Conn, _ uio.DialFunc) (uio.Conn, error) {
		st, err := sess.Open(ctx)
		if err != nil {
			return nil, err
		}
		return st, nil
	}
	users := br.Events()
	userAddr := freeAddr(t)
	done := make(chan struct{})
	go func() { defer close(done); _ = users.Serve("tcp://" + userAddr) }()
	t.Cleanup(func() { _ = users.Close(nil); <-done })

	conn, err := dialRetry(userAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	payload := []byte("relay-hop")
	if _, err := conn.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := conn.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	if got := readEchoUntilEOF(t, conn); !bytes.Equal(got, payload) {
		t.Fatalf("echo = %q", got)
	}
}

// TestBridgeAgentHop drives the agent shape: a bridge is the session handler
// and dials the local service for every accepted stream.
func TestBridgeAgentHop(t *testing.T) {
	target := startTCPEcho(t)
	br := &uio.Bridge{Upstream: target}
	targets := br.Events()
	serveWait(targets)
	t.Cleanup(func() { _ = targets.Close(nil) })

	srvAddr := freeAddr(t)
	srv := NewServer(Config{Handler: br, Observer: br})
	go func() { _ = srv.Serve(srvAddr) }()
	t.Cleanup(func() { _ = srv.Close(nil) })
	cli := NewClient(Config{})
	t.Cleanup(func() { _ = cli.Close(nil) })
	var sess *Session
	waitFor(t, "client session", func() bool {
		s, err := cli.Dial(context.Background(), "tcp://"+srvAddr)
		sess = s
		return err == nil
	})
	st, err := sess.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("agent-hop")
	if _, err := st.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := st.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 64)
	var got []byte
	waitFor(t, "echo", func() bool {
		n, rerr := st.Read(buf)
		if n > 0 {
			got = append(got, buf[:n]...)
			return len(got) >= len(payload)
		}
		if rerr != nil && !errors.Is(rerr, io.EOF) {
			t.Fatalf("read: %v", rerr)
		}
		return false
	})
	if !bytes.Equal(got, payload) {
		t.Fatalf("echo = %q", got)
	}
}
