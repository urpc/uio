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

package uws

import (
	"bufio"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

// closeWatcher records the lifecycle of one client connection.
type closeWatcher struct {
	open   chan struct{}
	closed chan CloseEvent
}

func newCloseWatcher() *closeWatcher {
	return &closeWatcher{open: make(chan struct{}, 1), closed: make(chan CloseEvent, 1)}
}

func (h *closeWatcher) OnOpen(*Conn) {
	select {
	case h.open <- struct{}{}:
	default:
	}
}

func (h *closeWatcher) OnMessage(*Conn, Message) {}

func (h *closeWatcher) OnClose(_ *Conn, event CloseEvent) {
	select {
	case h.closed <- event:
	default:
	}
}

func testAcceptKey(key string) string {
	sum := sha1.Sum([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	return base64.StdEncoding.EncodeToString(sum[:])
}

// A Dialer heartbeat keeps pinging and the uws server answers Pong, so a
// responsive connection stays open across several ping intervals.
func TestDialerHeartbeatKeepsResponsivePeer(t *testing.T) {
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := probe.Addr().String()
	_ = probe.Close()

	server := NewServer(Options{Pollers: 1}, newCloseWatcher())
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(addr) }()
	t.Cleanup(func() {
		_ = server.Close(nil)
		select {
		case <-serveDone:
		case <-time.After(testIOTimeout()):
			t.Error("server did not stop")
		}
	})

	dialer := NewDialer(Options{
		Pollers:               1,
		HeartbeatInterval:     5 * time.Millisecond,
		HeartbeatTimeout:      40 * time.Millisecond,
		OutboundHighWatermark: -1,
	})
	t.Cleanup(func() { _ = dialer.Close(nil) })
	watcher := newCloseWatcher()
	var client *Conn
	for deadline := time.Now().Add(testIOTimeout()); client == nil && time.Now().Before(deadline); {
		client, err = dialer.Dial(context.Background(), "ws://"+addr+"/", watcher)
		if err != nil {
			time.Sleep(time.Millisecond)
		}
	}
	if client == nil {
		t.Fatalf("dial: %v", err)
	}
	select {
	case <-watcher.open:
	case <-time.After(testIOTimeout()):
		t.Fatal("dialer OnOpen never ran")
	}

	time.Sleep(100 * time.Millisecond)
	select {
	case event := <-watcher.closed:
		t.Fatalf("a responsive peer was closed: %+v", event)
	default:
	}
	_ = client.Close(1000, "")
}

// A peer that accepts the upgrade and then never answers a Ping is closed by
// the Dialer's heartbeat with code 1001.
func TestDialerHeartbeatClosesSilentPeer(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		reader := bufio.NewReader(conn)
		key := ""
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				return
			}
			trimmed := strings.TrimSpace(line)
			if len(trimmed) > len("sec-websocket-key:") && strings.EqualFold(trimmed[:len("sec-websocket-key:")], "sec-websocket-key:") {
				key = strings.TrimSpace(trimmed[len("sec-websocket-key:"):])
			}
			if line == "\r\n" {
				break
			}
		}
		_, _ = fmt.Fprintf(conn, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n", testAcceptKey(key))
		// Silent from here: consume everything, answer no Ping.
		_, _ = io.Copy(io.Discard, reader)
	}()

	dialer := NewDialer(Options{
		Pollers:               1,
		HeartbeatInterval:     10 * time.Millisecond,
		HeartbeatTimeout:      30 * time.Millisecond,
		OutboundHighWatermark: -1,
	})
	t.Cleanup(func() { _ = dialer.Close(nil) })
	watcher := newCloseWatcher()
	client, err := dialer.Dial(context.Background(), "ws://"+listener.Addr().String()+"/", watcher)
	if err != nil {
		t.Fatal(err)
	}
	_ = client
	select {
	case <-watcher.open:
	case <-time.After(testIOTimeout()):
		t.Fatal("dialer OnOpen never ran")
	}
	select {
	case event := <-watcher.closed:
		if event.Code != 1001 {
			t.Fatalf("heartbeat close code = %d, want 1001", event.Code)
		}
	case <-time.After(testIOTimeout()):
		t.Fatal("dialer heartbeat did not close the silent peer")
	}
}
