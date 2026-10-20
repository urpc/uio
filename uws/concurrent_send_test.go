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
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/urpc/uio/uws/internal/frame"
)

// captureEchoHandler echoes every message and publishes its connection, so a
// test can send from a goroutine of its own while a read round is batching.
type captureEchoHandler struct {
	conn chan *Conn
	seen chan int
	once sync.Once
}

func newCaptureEchoHandler() *captureEchoHandler {
	return &captureEchoHandler{conn: make(chan *Conn, 1), seen: make(chan int, 1024)}
}

func (h *captureEchoHandler) OnOpen(c *Conn) {
	h.once.Do(func() { h.conn <- c })
}

func (h *captureEchoHandler) OnMessage(c *Conn, m Message) {
	select {
	case h.seen <- len(m.Payload):
	default:
	}
	_ = c.SendBinary(m.Payload)
}

func (h *captureEchoHandler) OnClose(*Conn, CloseEvent) {}

// startEchoServer runs a server on an ephemeral port and returns its address.
func startEchoServer(t *testing.T, handler Handler) string {
	t.Helper()
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := probe.Addr().String()
	_ = probe.Close()
	server := NewServer(Options{Pollers: 1}, handler)
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
	return addr
}

// dialTestWS opens a raw WebSocket client to addr and completes the upgrade.
func dialTestWS(t *testing.T, addr string) (net.Conn, *bufio.Reader) {
	t.Helper()
	var conn net.Conn
	var err error
	for deadline := time.Now().Add(testIOTimeout()); conn == nil && time.Now().Before(deadline); {
		conn, err = net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err != nil {
			time.Sleep(time.Millisecond)
		}
	}
	if conn == nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	request := "GET / HTTP/1.1\r\nHost: " + addr + "\r\n" +
		"Connection: Upgrade\r\nUpgrade: websocket\r\n" +
		"Sec-WebSocket-Version: 13\r\nSec-WebSocket-Key: " + testKey + "\r\n\r\n"
	if _, err := conn.Write([]byte(request)); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(conn)
	if line, err := reader.ReadString('\n'); err != nil || len(line) < 12 || line[:12] != "HTTP/1.1 101" {
		t.Fatalf("handshake response = %q, %v", line, err)
	}
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if line == "\r\n" {
			break
		}
	}
	return conn, reader
}

func echoPayload(i int) []byte {
	payload := make([]byte, 256)
	binary.BigEndian.PutUint32(payload, uint32(i))
	for j := 4; j < len(payload); j++ {
		payload[j] = byte(i)
	}
	return payload
}

func externalPayload(j int) []byte {
	payload := make([]byte, 64)
	copy(payload, fmt.Sprintf("ext-%04d", j))
	for i := len(fmt.Sprintf("ext-%04d", j)); i < len(payload); i++ {
		payload[i] = byte(j)
	}
	return payload
}

// A handler that echoes while another goroutine sends to the same connection
// keeps every frame whole: the round's replies and the outside sends never
// share a buffer, so no message is truncated, merged or byte-interleaved.
// Each sender's order holds; the order between them does not.
func TestConcurrentSendsKeepFramesWhole(t *testing.T) {
	const (
		echoes    = 4096
		externals = 512
	)
	handler := newCaptureEchoHandler()
	addr := startEchoServer(t, handler)
	conn, reader := dialTestWS(t, addr)
	var serverConn *Conn
	select {
	case serverConn = <-handler.conn:
	case <-time.After(testIOTimeout()):
		t.Fatal("OnOpen never ran")
	}

	externalDone := make(chan error, 1)
	go func() {
		for j := 0; j < externals; j++ {
			if err := serverConn.SendBinary(externalPayload(j)); err != nil {
				externalDone <- fmt.Errorf("external %d: %w", j, err)
				return
			}
			if j%16 == 0 {
				time.Sleep(100 * time.Microsecond)
			}
		}
		externalDone <- nil
	}()

	// Read replies while writing, so the server never pauses its reads.
	if err := conn.SetReadDeadline(time.Now().Add(testIOTimeout())); err != nil {
		t.Fatal(err)
	}
	frames := make(chan []byte, echoes+externals)
	readErr := make(chan error, 1)
	go func() {
		defer close(frames)
		for i := 0; i < echoes+externals; i++ {
			payload, err := readEchoFrame(reader)
			if err != nil {
				readErr <- err
				return
			}
			frames <- payload
		}
		readErr <- nil
	}()

	maskKey := [4]byte{1, 2, 3, 4}
	var wire []byte
	for i := 0; i < echoes; i++ {
		wire = frame.Append(wire[:0], frame.Frame{Fin: true, Opcode: frame.Binary, Masked: true, Payload: echoPayload(i)}, maskKey)
		if _, err := conn.Write(wire); err != nil {
			t.Fatalf("write echo %d: %v", i, err)
		}
	}

	nextEcho, nextExternal := 0, 0
	for payload := range frames {
		switch len(payload) {
		case 256:
			if nextEcho >= echoes {
				t.Fatalf("more echoes than sent")
			}
			if want := echoPayload(nextEcho); string(payload) != string(want) {
				t.Fatalf("echo %d corrupted", nextEcho)
			}
			nextEcho++
		case 64:
			if nextExternal >= externals {
				t.Fatalf("more external sends than made")
			}
			if want := externalPayload(nextExternal); string(payload) != string(want) {
				t.Fatalf("external %d corrupted or out of order", nextExternal)
			}
			nextExternal++
		default:
			t.Fatalf("unexpected frame of %d bytes", len(payload))
		}
	}
	if err := <-readErr; err != nil {
		t.Fatalf("after echo %d, external %d: %v", nextEcho, nextExternal, err)
	}
	if nextEcho != echoes || nextExternal != externals {
		t.Fatalf("received %d echoes and %d outside sends, want %d and %d", nextEcho, nextExternal, echoes, externals)
	}
	select {
	case err := <-externalDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(testIOTimeout()):
		t.Fatal("external sends did not finish")
	}
}

// A Close from another goroutine while a read round is batching waits for the
// round's replies: the peer sees every accepted echo before the Close frame
// and nothing after it.
func TestExternalCloseFollowsTheRoundsReplies(t *testing.T) {
	const total = 4096
	handler := newCaptureEchoHandler()
	addr := startEchoServer(t, handler)
	conn, reader := dialTestWS(t, addr)
	var serverConn *Conn
	select {
	case serverConn = <-handler.conn:
	case <-time.After(testIOTimeout()):
		t.Fatal("OnOpen never ran")
	}

	// Stream requests; the server is batching its replies while it reads.
	var wire []byte
	maskKey := [4]byte{5, 6, 7, 8}
	go func() {
		for i := 0; i < total; i++ {
			wire = frame.Append(wire[:0], frame.Frame{Fin: true, Opcode: frame.Binary, Masked: true, Payload: echoPayload(i)}, maskKey)
			if _, err := conn.Write(wire); err != nil {
				return
			}
		}
	}()
	// Close from a goroutine of its own once replies are certainly flowing.
	time.Sleep(20 * time.Millisecond)
	if err := serverConn.Close(1000, "bye"); err != nil && err != ErrClosed {
		t.Fatalf("close: %v", err)
	}

	// Read until the Close frame: the echoes before it must be whole and in
	// order, and nothing may follow it.
	if err := conn.SetReadDeadline(time.Now().Add(testIOTimeout())); err != nil {
		t.Fatal(err)
	}
	for next := 0; ; {
		opcode, payload, err := readFrameWithOpcode(reader)
		if err != nil {
			t.Fatalf("after %d echoes: %v", next, err)
		}
		switch opcode {
		case frame.Binary:
			if want := echoPayload(next); string(payload) != string(want) {
				t.Fatalf("echo %d corrupted before the Close frame", next)
			}
			next++
		case frame.Close:
			// Answer the close handshake, then make sure no data follows.
			reply := frame.Append(nil, frame.Frame{Fin: true, Opcode: frame.Close, Masked: true, Payload: payload}, maskKey)
			_, _ = conn.Write(reply)
			if err := conn.SetReadDeadline(time.Now().Add(250 * time.Millisecond)); err != nil {
				t.Fatal(err)
			}
			if extra, extraPayload, err := readFrameWithOpcode(reader); err == nil {
				t.Fatalf("frame after the Close frame: opcode %d, %d bytes", extra, len(extraPayload))
			}
			return
		default:
			t.Fatalf("unexpected opcode %d before the Close frame", opcode)
		}
	}
}

// readFrameWithOpcode reads one unmasked server frame and reports its opcode.
func readFrameWithOpcode(reader *bufio.Reader) (frame.OpCode, []byte, error) {
	var header [2]byte
	if _, err := io.ReadFull(reader, header[:]); err != nil {
		return 0, nil, err
	}
	length := int(header[1] & 0x7f)
	switch length {
	case 126:
		var ext [2]byte
		if _, err := io.ReadFull(reader, ext[:]); err != nil {
			return 0, nil, err
		}
		length = int(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err := io.ReadFull(reader, ext[:]); err != nil {
			return 0, nil, err
		}
		length = int(binary.BigEndian.Uint64(ext[:]))
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(reader, payload); err != nil {
		return 0, nil, err
	}
	return frame.OpCode(header[0] & 0x0f), payload, nil
}
