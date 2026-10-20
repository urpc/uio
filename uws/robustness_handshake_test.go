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
	"net"
	"strings"
	"testing"
	"time"

	"github.com/urpc/uio/uws/internal/frame"
)

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
