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
	"bytes"
	"encoding/binary"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/urpc/uio/uws/internal/frame"
)

// The robustness tests drive the server through a raw socket, so every wire
// edge case is observed the way a peer would see it: the frames on the wire,
// the close code, and whether the server keeps serving.

// robustEchoHandler echoes each message with its own opcode.
type robustEchoHandler struct{}

func (robustEchoHandler) OnOpen(*Conn) {}
func (robustEchoHandler) OnMessage(c *Conn, m Message) {
	if m.Type == TextMessage {
		_ = c.SendText(m.Payload)
	} else {
		_ = c.SendBinary(m.Payload)
	}
}
func (robustEchoHandler) OnClose(*Conn, CloseEvent) {}

// startRobustServer runs a server with opts on an ephemeral port.
func startRobustServer(t *testing.T, opts Options, handler Handler) string {
	t.Helper()
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := probe.Addr().String()
	_ = probe.Close()
	server := NewServer(opts, handler)
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

// robustDial performs the standard upgrade handshake and returns the socket.
func robustDial(t *testing.T, addr string) (net.Conn, *bufio.Reader) {
	t.Helper()
	return dialTestWS(t, addr)
}

// writeClientFrame writes one masked client frame with the given first byte,
// which lets a test set bits the encoder would never produce.
func writeClientFrame(t *testing.T, conn net.Conn, first byte, payload []byte) {
	t.Helper()
	maskKey := [4]byte{1, 2, 3, 4}
	wire := []byte{first}
	switch n := len(payload); {
	case n < 126:
		wire = append(wire, 0x80|byte(n))
	case n <= 0xffff:
		wire = append(wire, 0x80|126, byte(n>>8), byte(n))
	default:
		wire = append(wire, 0x80|127)
		var ext [8]byte
		binary.BigEndian.PutUint64(ext[:], uint64(n))
		wire = append(wire, ext[:]...)
	}
	wire = append(wire, maskKey[:]...)
	start := len(wire)
	wire = append(wire, payload...)
	for i := range wire[start:] {
		wire[start+i] ^= maskKey[i&3]
	}
	if _, err := conn.Write(wire); err != nil {
		t.Fatalf("write frame: %v", err)
	}
}

// writeDataFrame writes a well-formed masked frame through the real encoder.
func writeDataFrame(t *testing.T, conn net.Conn, opcode frame.OpCode, fin bool, payload []byte) {
	t.Helper()
	wire := frame.Append(nil, frame.Frame{Fin: fin, Opcode: opcode, Masked: true, Payload: payload}, [4]byte{9, 8, 7, 6})
	if _, err := conn.Write(wire); err != nil {
		t.Fatalf("write frame: %v", err)
	}
}

// awaitClose reads frames until the peer's Close frame and returns its code.
// A connection that drops without one reports -1. The caller arms the read
// deadline.
func awaitClose(t *testing.T, reader *bufio.Reader) int {
	t.Helper()
	for {
		opcode, payload, err := readFrameWithOpcode(reader)
		if err != nil {
			if err == io.EOF {
				return -1
			}
			// A reset after the Close frame is the peer dropping the
			// connection, which is not the close code under test.
			return -1
		}
		if opcode == frame.Close {
			if len(payload) < 2 {
				return 0
			}
			return int(binary.BigEndian.Uint16(payload))
		}
	}
}

func TestRobustEmptyMessages(t *testing.T) {
	addr := startRobustServer(t, Options{}, robustEchoHandler{})
	conn, reader := robustDial(t, addr)

	writeDataFrame(t, conn, frame.Text, true, nil)
	if opcode, payload, err := readFrameWithOpcode(reader); err != nil || opcode != frame.Text || len(payload) != 0 {
		t.Fatalf("empty text echo = opcode %d len %d err %v", opcode, len(payload), err)
	}
	writeDataFrame(t, conn, frame.Binary, true, []byte{})
	if opcode, payload, err := readFrameWithOpcode(reader); err != nil || opcode != frame.Binary || len(payload) != 0 {
		t.Fatalf("empty binary echo = opcode %d len %d err %v", opcode, len(payload), err)
	}
}

func TestRobustFrameLengthBoundaries(t *testing.T) {
	addr := startRobustServer(t, Options{}, robustEchoHandler{})
	conn, reader := robustDial(t, addr)
	if err := conn.SetReadDeadline(time.Now().Add(testIOTimeout())); err != nil {
		t.Fatal(err)
	}
	for _, size := range []int{1, 125, 126, 127, 65535, 65536} {
		payload := bytes.Repeat([]byte{byte(size)}, size)
		writeDataFrame(t, conn, frame.Binary, true, payload)
		opcode, got, err := readFrameWithOpcode(reader)
		if err != nil {
			t.Fatalf("size %d: %v", size, err)
		}
		if opcode != frame.Binary || !bytes.Equal(got, payload) {
			t.Fatalf("size %d echoed as opcode %d len %d", size, opcode, len(got))
		}
	}
}

// A payload past MaxFramePayload fragments on the way out as well.
func TestRobustOutboundFragmentation(t *testing.T) {
	addr := startRobustServer(t, Options{MaxFramePayload: 64}, robustEchoHandler{})
	conn, reader := robustDial(t, addr)
	if err := conn.SetReadDeadline(time.Now().Add(testIOTimeout())); err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte("fragment-"), 40) // 360 bytes
	// The frame limit bounds what arrives, so the message itself comes in
	// fragments; the reply is over the limit as one frame and must fragment.
	for offset := 0; offset < len(payload); {
		end := offset + 60
		if end > len(payload) {
			end = len(payload)
		}
		opcode := frame.Continuation
		if offset == 0 {
			opcode = frame.Binary
		}
		writeDataFrame(t, conn, opcode, end == len(payload), payload[offset:end])
		offset = end
	}

	var assembled []byte
	frames := 0
	for {
		opcode, part, err := readFrameWithOpcode(reader)
		if err != nil {
			t.Fatalf("read fragment: %v", err)
		}
		if opcode == frame.Close {
			t.Fatalf("fragmented echo closed: %x", part)
		}
		assembled = append(assembled, part...)
		frames++
		if len(part) < 64 || frames > 8 {
			break
		}
	}
	if !bytes.Equal(assembled, payload) || frames < 2 {
		t.Fatalf("fragmented echo = %d frames, %d bytes, want %d bytes in several frames", frames, len(assembled), len(payload))
	}
}

func TestRobustReservedOpcodesClose(t *testing.T) {
	addr := startRobustServer(t, Options{}, robustEchoHandler{})
	for _, first := range []byte{0x80 | 0x03, 0x80 | 0x07, 0x80 | 0x0B, 0x80 | 0x0F} {
		conn, reader := robustDial(t, addr)
		if err := conn.SetReadDeadline(time.Now().Add(testIOTimeout())); err != nil {
			t.Fatal(err)
		}
		writeClientFrame(t, conn, first, []byte("x"))
		if code := awaitClose(t, reader); code != 1002 {
			t.Fatalf("opcode %#x close code = %d, want 1002", first&0x0f, code)
		}
		_ = conn.Close()
	}
}

func TestRobustRSVBitsClose(t *testing.T) {
	addr := startRobustServer(t, Options{}, robustEchoHandler{})
	for _, first := range []byte{0xC1, 0xA1, 0x91} { // RSV1, RSV2, RSV3 with text FIN
		conn, reader := robustDial(t, addr)
		writeClientFrame(t, conn, first, []byte("x"))
		if code := awaitClose(t, reader); code != 1002 {
			t.Fatalf("first byte %#x close code = %d, want 1002", first, code)
		}
		_ = conn.Close()
	}
}

func TestRobustUnmaskedClientFrameCloses(t *testing.T) {
	addr := startRobustServer(t, Options{}, robustEchoHandler{})
	conn, reader := robustDial(t, addr)
	if err := conn.SetReadDeadline(time.Now().Add(testIOTimeout())); err != nil {
		t.Fatal(err)
	}
	// A server frame format from the client: no mask bit, no key.
	if _, err := conn.Write([]byte{0x81, 0x01, 'x'}); err != nil {
		t.Fatal(err)
	}
	if code := awaitClose(t, reader); code != 1002 {
		t.Fatalf("unmasked frame close code = %d, want 1002", code)
	}
}

func TestRobustMalformedControlFrames(t *testing.T) {
	addr := startRobustServer(t, Options{}, robustEchoHandler{})
	cases := []struct {
		name  string
		first byte
		size  int
	}{
		{"fragmented ping", 0x09, 1},
		{"oversized ping", 0x89, 126},
		{"oversized pong", 0x8A, 126},
		{"fragmented close", 0x08, 2},
	}
	for _, tc := range cases {
		conn, reader := robustDial(t, addr)
		if err := conn.SetReadDeadline(time.Now().Add(testIOTimeout())); err != nil {
			t.Fatal(err)
		}
		writeClientFrame(t, conn, tc.first, bytes.Repeat([]byte{'k'}, tc.size))
		if code := awaitClose(t, reader); code != 1002 {
			t.Fatalf("%s close code = %d, want 1002", tc.name, code)
		}
		_ = conn.Close()
	}
}

func TestRobustContinuationWithoutStartCloses(t *testing.T) {
	addr := startRobustServer(t, Options{}, robustEchoHandler{})
	conn, reader := robustDial(t, addr)
	if err := conn.SetReadDeadline(time.Now().Add(testIOTimeout())); err != nil {
		t.Fatal(err)
	}
	writeDataFrame(t, conn, frame.Continuation, true, []byte("orphan"))
	if code := awaitClose(t, reader); code != 1002 {
		t.Fatalf("orphan continuation close code = %d, want 1002", code)
	}
}

func TestRobustDataInsideFragmentCloses(t *testing.T) {
	addr := startRobustServer(t, Options{}, robustEchoHandler{})
	conn, reader := robustDial(t, addr)
	if err := conn.SetReadDeadline(time.Now().Add(testIOTimeout())); err != nil {
		t.Fatal(err)
	}
	writeDataFrame(t, conn, frame.Text, false, []byte("half"))
	writeDataFrame(t, conn, frame.Binary, true, []byte("interleaved"))
	if code := awaitClose(t, reader); code != 1002 {
		t.Fatalf("interleaved data close code = %d, want 1002", code)
	}
}

// Control frames may sit between fragments, and the message still assembles.
func TestRobustPingInsideFragments(t *testing.T) {
	addr := startRobustServer(t, Options{}, robustEchoHandler{})
	conn, reader := robustDial(t, addr)
	if err := conn.SetReadDeadline(time.Now().Add(testIOTimeout())); err != nil {
		t.Fatal(err)
	}
	writeDataFrame(t, conn, frame.Text, false, []byte("he"))
	writeDataFrame(t, conn, frame.Ping, true, []byte("mid"))
	writeDataFrame(t, conn, frame.Continuation, true, []byte("llo"))

	opcode, payload, err := readFrameWithOpcode(reader)
	if err != nil || opcode != frame.Pong || string(payload) != "mid" {
		t.Fatalf("pong = opcode %d payload %q err %v", opcode, payload, err)
	}
	opcode, payload, err = readFrameWithOpcode(reader)
	if err != nil || opcode != frame.Text || string(payload) != "hello" {
		t.Fatalf("assembled echo = opcode %d payload %q err %v", opcode, payload, err)
	}
}

// UTF-8 validation runs over the assembled message, so a multi-byte character
// split across fragments is still valid.
func TestRobustUTF8SplitAcrossFragments(t *testing.T) {
	addr := startRobustServer(t, Options{}, robustEchoHandler{})
	conn, reader := robustDial(t, addr)
	if err := conn.SetReadDeadline(time.Now().Add(testIOTimeout())); err != nil {
		t.Fatal(err)
	}
	text := []byte("你")
	writeDataFrame(t, conn, frame.Text, false, text[:1])
	writeDataFrame(t, conn, frame.Continuation, true, text[1:])
	opcode, payload, err := readFrameWithOpcode(reader)
	if err != nil || opcode != frame.Text || !bytes.Equal(payload, text) {
		t.Fatalf("split UTF-8 echo = opcode %d payload %x err %v", opcode, payload, err)
	}
}

func TestRobustInvalidUTF8AtMessageEndCloses(t *testing.T) {
	addr := startRobustServer(t, Options{}, robustEchoHandler{})
	conn, reader := robustDial(t, addr)
	if err := conn.SetReadDeadline(time.Now().Add(testIOTimeout())); err != nil {
		t.Fatal(err)
	}
	writeDataFrame(t, conn, frame.Text, false, []byte{0xff})
	writeDataFrame(t, conn, frame.Continuation, true, []byte("x"))
	if code := awaitClose(t, reader); code != 1007 {
		t.Fatalf("invalid UTF-8 close code = %d, want 1007", code)
	}
}

func TestRobustPingPayloads(t *testing.T) {
	addr := startRobustServer(t, Options{}, robustEchoHandler{})
	conn, reader := robustDial(t, addr)
	if err := conn.SetReadDeadline(time.Now().Add(testIOTimeout())); err != nil {
		t.Fatal(err)
	}
	for _, size := range []int{0, 125} {
		ping := bytes.Repeat([]byte{byte(size)}, size)
		writeDataFrame(t, conn, frame.Ping, true, ping)
		opcode, payload, err := readFrameWithOpcode(reader)
		if err != nil || opcode != frame.Pong || !bytes.Equal(payload, ping) {
			t.Fatalf("ping %d answered with opcode %d payload %x err %v", size, opcode, payload, err)
		}
	}
}

func TestRobustUnsolicitedPongIgnored(t *testing.T) {
	addr := startRobustServer(t, Options{}, robustEchoHandler{})
	conn, reader := robustDial(t, addr)
	if err := conn.SetReadDeadline(time.Now().Add(testIOTimeout())); err != nil {
		t.Fatal(err)
	}
	writeDataFrame(t, conn, frame.Pong, true, []byte("nobody asked"))
	writeDataFrame(t, conn, frame.Text, true, []byte("after"))
	opcode, payload, err := readFrameWithOpcode(reader)
	if err != nil || opcode != frame.Text || string(payload) != "after" {
		t.Fatalf("echo after unsolicited pong = opcode %d payload %q err %v", opcode, payload, err)
	}
}

func TestRobustClosePayloadEdges(t *testing.T) {
	addr := startRobustServer(t, Options{}, robustEchoHandler{})
	cases := []struct {
		name    string
		payload []byte
		want    int
	}{
		{"one byte", []byte{0x03}, 1002},
		{"reserved 1005", []byte{0x03, 0xED}, 1002},
		{"reserved 1006", []byte{0x03, 0xEE}, 1002},
		{"below range", []byte{0x03, 0xE7}, 1002},
		{"application code", []byte{0x0B, 0xB8}, 3000},
	}
	for _, tc := range cases {
		conn, reader := robustDial(t, addr)
		if err := conn.SetReadDeadline(time.Now().Add(testIOTimeout())); err != nil {
			t.Fatal(err)
		}
		writeDataFrame(t, conn, frame.Close, true, tc.payload)
		if code := awaitClose(t, reader); code != tc.want {
			t.Fatalf("%s close code = %d, want %d", tc.name, code, tc.want)
		}
		_ = conn.Close()
	}
}

// After the peer's Close, the server answers it and ignores anything the peer
// sends afterwards; nothing crashes and nothing is echoed.
func TestRobustDataAfterCloseIgnored(t *testing.T) {
	addr := startRobustServer(t, Options{}, robustEchoHandler{})
	conn, reader := robustDial(t, addr)
	if err := conn.SetReadDeadline(time.Now().Add(testIOTimeout())); err != nil {
		t.Fatal(err)
	}
	writeDataFrame(t, conn, frame.Close, true, []byte{0x03, 0xE8})
	writeDataFrame(t, conn, frame.Text, true, []byte("late"))
	opcode, _, err := readFrameWithOpcode(reader)
	if err != nil || opcode != frame.Close {
		t.Fatalf("reply to peer close = opcode %d err %v", opcode, err)
	}
}

// Many frames in one write arrive as one read and every reply comes back in
// order.
func TestRobustManyFramesInOneWrite(t *testing.T) {
	addr := startRobustServer(t, Options{}, robustEchoHandler{})
	conn, reader := robustDial(t, addr)
	if err := conn.SetReadDeadline(time.Now().Add(testIOTimeout())); err != nil {
		t.Fatal(err)
	}
	const total = 500
	var wire []byte
	for i := 0; i < total; i++ {
		wire = frame.Append(wire, frame.Frame{Fin: true, Opcode: frame.Binary, Masked: true, Payload: []byte{byte(i), byte(i >> 8)}}, [4]byte{5, 4, 3, 2})
	}
	if _, err := conn.Write(wire); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < total; i++ {
		opcode, payload, err := readFrameWithOpcode(reader)
		if err != nil || opcode != frame.Binary || len(payload) != 2 || payload[0] != byte(i) || payload[1] != byte(i>>8) {
			t.Fatalf("reply %d = opcode %d payload %x err %v", i, opcode, payload, err)
		}
	}
}

// A frame split across writes is assembled before it is handled.
func TestRobustFrameSplitAcrossWrites(t *testing.T) {
	addr := startRobustServer(t, Options{}, robustEchoHandler{})
	conn, reader := robustDial(t, addr)
	if err := conn.SetReadDeadline(time.Now().Add(testIOTimeout())); err != nil {
		t.Fatal(err)
	}
	wire := frame.Append(nil, frame.Frame{Fin: true, Opcode: frame.Text, Masked: true, Payload: []byte("byte by byte")}, [4]byte{7, 7, 7, 7})
	for _, b := range wire {
		if _, err := conn.Write([]byte{b}); err != nil {
			t.Fatal(err)
		}
	}
	opcode, payload, err := readFrameWithOpcode(reader)
	if err != nil || opcode != frame.Text || string(payload) != "byte by byte" {
		t.Fatalf("split frame echo = opcode %d payload %q err %v", opcode, payload, err)
	}
}

// robustTCP dials addr, retrying while the server gets its listener up.
func robustTCP(t *testing.T, addr string) net.Conn {
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
	return conn
}

// rawHandshake sends request to addr and returns the socket, its reader, the
// status line and the header lines of the reply.
func rawHandshake(t *testing.T, addr, request string) (net.Conn, *bufio.Reader, string, []string) {
	t.Helper()
	conn := robustTCP(t, addr)
	if _, err := conn.Write([]byte(request)); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(conn)
	if err := conn.SetReadDeadline(time.Now().Add(testIOTimeout())); err != nil {
		t.Fatal(err)
	}
	// A rejected upgrade may be answered (native backend) or the connection
	// dropped without a response (stdio backend); both mean no upgrade.
	status, err := reader.ReadString('\n')
	if err != nil {
		return conn, reader, "", nil
	}
	var headers []string
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return conn, reader, status, headers
		}
		if line == "\r\n" {
			return conn, reader, status, headers
		}
		headers = append(headers, line)
	}
}

// upgradeRequest builds a well-formed upgrade request with extra header lines
// appended.
func upgradeRequest(addr string, extra ...string) string {
	lines := []string{
		"GET / HTTP/1.1",
		"Host: " + addr,
		"Connection: Upgrade",
		"Upgrade: websocket",
		"Sec-WebSocket-Version: 13",
		"Sec-WebSocket-Key: " + testKey,
	}
	lines = append(lines, extra...)
	return strings.Join(lines, "\r\n") + "\r\n\r\n"
}

// A rejected upgrade answers 400 and leaves the server serving.
func TestRobustHandshakeRejections(t *testing.T) {
	addr := startRobustServer(t, Options{}, robustEchoHandler{})
	cases := []struct {
		name    string
		request string
	}{
		{"wrong version", strings.Replace(upgradeRequest(addr), "Version: 13", "Version: 12", 1)},
		{"missing key", strings.Replace(upgradeRequest(addr), "Sec-WebSocket-Key: "+testKey+"\r\n", "", 1)},
		{"bad key", strings.Replace(upgradeRequest(addr), "Sec-WebSocket-Key: "+testKey, "Sec-WebSocket-Key: not-base64", 1)},
		{"post", strings.Replace(upgradeRequest(addr), "GET /", "POST /", 1)},
		{"no upgrade", "GET / HTTP/1.1\r\nHost: " + addr + "\r\n\r\n"},
	}
	for _, tc := range cases {
		_, _, status, _ := rawHandshake(t, addr, tc.request)
		if strings.Contains(status, "101") {
			t.Fatalf("%s was upgraded: %q", tc.name, status)
		}
	}
	// The server still serves a well-formed request afterwards.
	conn, reader := robustDial(t, addr)
	if err := conn.SetReadDeadline(time.Now().Add(testIOTimeout())); err != nil {
		t.Fatal(err)
	}
	writeDataFrame(t, conn, frame.Text, true, []byte("still here"))
	if _, payload, err := readFrameWithOpcode(reader); err != nil || string(payload) != "still here" {
		t.Fatalf("echo after rejected handshakes = %q, %v", payload, err)
	}
}

// Header names are case-insensitive, as HTTP is.
func TestRobustHandshakeMixedCaseHeaders(t *testing.T) {
	addr := startRobustServer(t, Options{}, robustEchoHandler{})
	request := "GET / HTTP/1.1\r\nhost: " + addr + "\r\nconnection: upgrade\r\nupgrade: WEBSOCKET\r\n" +
		"sec-websocket-version: 13\r\nsec-websocket-key: " + testKey + "\r\n\r\n"
	conn, reader, status, _ := rawHandshake(t, addr, request)
	if !strings.Contains(status, "101") {
		t.Fatalf("mixed-case upgrade status = %q, want 101", status)
	}
	if err := conn.SetReadDeadline(time.Now().Add(testIOTimeout())); err != nil {
		t.Fatal(err)
	}
	writeDataFrame(t, conn, frame.Text, true, []byte("case"))
	if _, payload, err := readFrameWithOpcode(reader); err != nil || string(payload) != "case" {
		t.Fatalf("echo after mixed-case upgrade = %q, %v", payload, err)
	}
}

// Headers past MaxHeaderBytes are refused, and the server keeps serving.
func TestRobustHandshakeOversizedHeaders(t *testing.T) {
	addr := startRobustServer(t, Options{MaxHeaderBytes: 256}, robustEchoHandler{})
	big := upgradeRequest(addr, "X-Padding: "+strings.Repeat("p", 1024))
	_, _, status, _ := rawHandshake(t, addr, big)
	if strings.Contains(status, "101") {
		t.Fatalf("oversized headers upgraded: %q", status)
	}
	_, _, status, _ = rawHandshake(t, addr, upgradeRequest(addr))
	if !strings.Contains(status, "101") {
		t.Fatalf("server stopped serving after oversized headers: %q", status)
	}
}

// A request split across writes is assembled by the incremental parser.
func TestRobustHandshakeSplitAcrossWrites(t *testing.T) {
	addr := startRobustServer(t, Options{}, robustEchoHandler{})
	conn := robustTCP(t, addr)
	request := upgradeRequest(addr)
	reader := bufio.NewReader(conn)
	for i := 0; i < len(request); i += 7 {
		end := i + 7
		if end > len(request) {
			end = len(request)
		}
		if _, err := conn.Write([]byte(request[i:end])); err != nil {
			t.Fatal(err)
		}
	}
	if err := conn.SetReadDeadline(time.Now().Add(testIOTimeout())); err != nil {
		t.Fatal(err)
	}
	status, err := reader.ReadString('\n')
	if err != nil || !strings.Contains(status, "101") {
		t.Fatalf("split request status = %q, %v", status, err)
	}
}

// A frame that arrives with the upgrade request in one write is handled after
// the switch, not dropped.
func TestRobustHandshakeFrameInSameWrite(t *testing.T) {
	addr := startRobustServer(t, Options{}, robustEchoHandler{})
	conn := robustTCP(t, addr)
	wire := append([]byte(upgradeRequest(addr)),
		frame.Append(nil, frame.Frame{Fin: true, Opcode: frame.Text, Masked: true, Payload: []byte("with the request")}, [4]byte{3, 1, 4, 1})...)
	if _, err := conn.Write(wire); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(conn)
	if err := conn.SetReadDeadline(time.Now().Add(testIOTimeout())); err != nil {
		t.Fatal(err)
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
	opcode, payload, err := readFrameWithOpcode(reader)
	if err != nil || opcode != frame.Text || string(payload) != "with the request" {
		t.Fatalf("echo of the frame sent with the request = opcode %d payload %q err %v", opcode, payload, err)
	}
}

// The server selects one of the protocols the client offered, preferring its
// own order, and sends no header when nothing overlaps.
func TestRobustHandshakeSubprotocol(t *testing.T) {
	addr := startRobustServer(t, Options{Subprotocols: []string{"chat", "superchat"}}, robustEchoHandler{})
	_, _, status, headers := rawHandshake(t, addr, upgradeRequest(addr, "Sec-WebSocket-Protocol: foo, superchat"))
	if !strings.Contains(status, "101") {
		t.Fatalf("status = %q", status)
	}
	selected := ""
	for _, line := range headers {
		if strings.HasPrefix(strings.ToLower(line), "sec-websocket-protocol:") {
			selected = strings.TrimSpace(line[len("sec-websocket-protocol:"):])
		}
	}
	if selected != "superchat" {
		t.Fatalf("selected subprotocol = %q, want superchat", selected)
	}

	_, _, status2, headers2 := rawHandshake(t, addr, upgradeRequest(addr, "Sec-WebSocket-Protocol: foo"))
	if !strings.Contains(status2, "101") {
		t.Fatalf("no-overlap status = %q, want 101", status2)
	}
	for _, line := range headers2 {
		if strings.HasPrefix(strings.ToLower(line), "sec-websocket-protocol:") {
			t.Fatalf("unexpected subprotocol header without overlap: %q", line)
		}
	}
}
