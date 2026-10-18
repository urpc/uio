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
	"io"
	"net"
	"testing"
	"time"

	"github.com/urpc/uio/uws/internal/frame"
)

type echoAllHandler struct{}

func (echoAllHandler) OnOpen(*Conn)                 {}
func (echoAllHandler) OnMessage(c *Conn, m Message) { _ = c.SendBinary(m.Payload) }
func (echoAllHandler) OnClose(*Conn, CloseEvent)    {}

// readServerFrame reads one unmasked frame from the server.
func readEchoFrame(reader *bufio.Reader) ([]byte, error) {
	var header [2]byte
	if _, err := io.ReadFull(reader, header[:]); err != nil {
		return nil, err
	}
	length := int(header[1] & 0x7f)
	switch length {
	case 126:
		var ext [2]byte
		if _, err := io.ReadFull(reader, ext[:]); err != nil {
			return nil, err
		}
		length = int(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err := io.ReadFull(reader, ext[:]); err != nil {
			return nil, err
		}
		length = int(binary.BigEndian.Uint64(ext[:]))
	}
	payload := make([]byte, length)
	_, err := io.ReadFull(reader, payload)
	return payload, err
}

// A peer that stops reading its replies makes the server pause that
// connection's reads once the outbound watermark is crossed. Reading again
// drains the queue, the pause lifts, and every message is echoed exactly once.
func TestSlowPeerBackpressureKeepsEveryMessage(t *testing.T) {
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := probe.Addr().String()
	_ = probe.Close()

	const (
		// Enough traffic that kernel socket buffers cannot absorb it: the
		// server must reach its watermark and stop reading.
		messages    = 8192
		payloadSize = 8 << 10
		high        = 32 << 10
	)
	server := NewServer(Options{
		Pollers:               1,
		ReadBufferSize:        4 << 10,
		OutboundHighWatermark: high,
		OutboundLowWatermark:  high / 4,
	}, echoAllHandler{})
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

	var conn net.Conn
	for deadline := time.Now().Add(testIOTimeout()); conn == nil && time.Now().Before(deadline); {
		conn, err = net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err != nil {
			time.Sleep(time.Millisecond)
		}
	}
	if conn == nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	if tcp, ok := conn.(*net.TCPConn); ok {
		// Keep the client's receive buffer small so the server's replies back
		// up quickly instead of disappearing into kernel buffers.
		_ = tcp.SetReadBuffer(64 << 10)
	}

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

	// Write requests without reading a single reply.
	writeDone := make(chan error, 1)
	go func() {
		maskKey := [4]byte{9, 8, 7, 6}
		var wire []byte
		for i := 0; i < messages; i++ {
			payload := make([]byte, payloadSize)
			for j := range payload {
				payload[j] = byte(i)
			}
			wire = frame.Append(wire[:0], frame.Frame{Fin: true, Opcode: frame.Binary, Masked: true, Payload: payload}, maskKey)
			if _, err := conn.Write(wire); err != nil {
				writeDone <- err
				return
			}
		}
		writeDone <- nil
	}()

	// The server must stop reading once its queued output crosses the
	// watermark, long before all requests are in.
	pausedBy := time.Now().Add(testIOTimeout())
	for {
		if server.Stats().ReadsPaused > 0 {
			break
		}
		if time.Now().After(pausedBy) {
			stats := server.Stats()
			t.Fatalf("reads were never paused: %+v", stats)
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Drain the replies: the pause lifts and every request is answered once.
	if err := conn.SetReadDeadline(time.Now().Add(testIOTimeout())); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < messages; i++ {
		payload, err := readEchoFrame(reader)
		if err != nil {
			t.Fatalf("reply %d: %v", i, err)
		}
		if len(payload) != payloadSize {
			t.Fatalf("reply %d length = %d, want %d", i, len(payload), payloadSize)
		}
		if payload[0] != byte(i) || payload[len(payload)-1] != byte(i) {
			t.Fatalf("reply %d payload = %x..%x, want %x", i, payload[0], payload[len(payload)-1], byte(i))
		}
	}
	select {
	case err := <-writeDone:
		if err != nil {
			t.Fatalf("client write: %v", err)
		}
	case <-time.After(testIOTimeout()):
		t.Fatal("client did not finish writing")
	}
	if stats := server.Stats(); stats.ReadsResumed == 0 {
		t.Fatalf("reads never resumed: %+v", stats)
	}
}
