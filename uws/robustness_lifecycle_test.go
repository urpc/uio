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
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urpc/uio/uws/internal/frame"
)

const wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// startRawWSServer runs a one-connection raw WebSocket server: it answers the
// upgrade itself and hands the socket to handle. It lets a test drive a uws
// client with frames no well-behaved server would send.
func startRawWSServer(t *testing.T, handle func(conn net.Conn, br *bufio.Reader)) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				br := bufio.NewReader(conn)
				key := ""
				for {
					line, err := br.ReadString('\n')
					if err != nil {
						return
					}
					lower := strings.ToLower(line)
					if strings.HasPrefix(lower, "sec-websocket-key:") {
						key = strings.TrimSpace(line[len("sec-websocket-key:"):])
					}
					if line == "\r\n" {
						break
					}
				}
				sum := sha1.Sum([]byte(key + wsGUID))
				accept := base64.StdEncoding.EncodeToString(sum[:])
				if _, err := fmt.Fprintf(conn, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n", accept); err != nil {
					return
				}
				handle(conn, br)
			}()
		}
	}()
	return ln.Addr().String()
}

// writeServerFrame writes one unmasked frame from a raw server.
func writeServerFrame(t *testing.T, conn net.Conn, opcode frame.OpCode, fin bool, payload []byte) {
	t.Helper()
	wire := frame.Append(nil, frame.Frame{Fin: fin, Opcode: opcode, Payload: payload}, [4]byte{})
	if _, err := conn.Write(wire); err != nil {
		t.Fatalf("server frame: %v", err)
	}
}

// writeMaskedServerFrame writes a frame a server must never send: masked.
func writeMaskedServerFrame(t *testing.T, conn net.Conn, payload []byte) {
	t.Helper()
	wire := frame.Append(nil, frame.Frame{Fin: true, Opcode: frame.Text, Masked: true, Payload: payload}, [4]byte{1, 1, 1, 1})
	if _, err := conn.Write(wire); err != nil {
		t.Fatalf("masked server frame: %v", err)
	}
}

// readClientFrame reads one masked client frame.
func readClientFrame(br *bufio.Reader) (frame.OpCode, []byte, error) {
	var header [2]byte
	if _, err := io.ReadFull(br, header[:]); err != nil {
		return 0, nil, err
	}
	if header[1]&0x80 == 0 {
		return 0, nil, errors.New("client frame is not masked")
	}
	length := int(header[1] & 0x7f)
	switch length {
	case 126:
		var ext [2]byte
		if _, err := io.ReadFull(br, ext[:]); err != nil {
			return 0, nil, err
		}
		length = int(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err := io.ReadFull(br, ext[:]); err != nil {
			return 0, nil, err
		}
		length = int(binary.BigEndian.Uint64(ext[:]))
	}
	var key [4]byte
	if _, err := io.ReadFull(br, key[:]); err != nil {
		return 0, nil, err
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(br, payload); err != nil {
		return 0, nil, err
	}
	for i := range payload {
		payload[i] ^= key[i&3]
	}
	return frame.OpCode(header[0] & 0x0f), payload, nil
}

// clientEvents records what a uws client handler sees.
type clientEvents struct {
	opened   chan struct{}
	closed   chan struct{}
	onceOpen sync.Once
	onceShut sync.Once
	mu       sync.Mutex
	msgs     []string
	types    []MessageType
	closeErr error
	conn     *Conn
}

func newClientEvents() *clientEvents {
	return &clientEvents{opened: make(chan struct{}), closed: make(chan struct{})}
}

func (e *clientEvents) OnOpen(c *Conn) {
	e.mu.Lock()
	e.conn = c
	e.mu.Unlock()
	e.onceOpen.Do(func() { close(e.opened) })
}

func (e *clientEvents) OnMessage(_ *Conn, m Message) {
	e.mu.Lock()
	e.msgs = append(e.msgs, string(m.Payload))
	e.types = append(e.types, m.Type)
	e.mu.Unlock()
}

func (e *clientEvents) OnClose(_ *Conn, ev CloseEvent) {
	e.mu.Lock()
	e.closeErr = ev.Err
	e.mu.Unlock()
	e.onceShut.Do(func() { close(e.closed) })
}

func (e *clientEvents) wait(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(testIOTimeout()):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// A server that masks a frame violates RFC 6455; the client must close with
// 1002 instead of accepting it.
func TestRobustClientRejectsMaskedServerFrame(t *testing.T) {
	gotClose := make(chan int, 1)
	addr := startRawWSServer(t, func(conn net.Conn, br *bufio.Reader) {
		writeMaskedServerFrame(t, conn, []byte("masked"))
		_ = conn.SetReadDeadline(time.Now().Add(testIOTimeout()))
		for {
			opcode, payload, err := readClientFrame(br)
			if err != nil {
				gotClose <- -1
				return
			}
			if opcode == frame.Close {
				code := 0
				if len(payload) >= 2 {
					code = int(binary.BigEndian.Uint16(payload))
				}
				gotClose <- code
				return
			}
		}
	})
	events := newClientEvents()
	dialer := NewDialer(Options{Handler: events})
	ctx, cancel := context.WithTimeout(context.Background(), testIOTimeout())
	defer cancel()
	// The violation can close the transport before Dial returns its flush;
	// ErrClosed then means the same as a connection returned to close at once.
	// What the peer observes is asserted below.
	if _, err := dialer.Dial(ctx, "ws://"+addr+"/", nil); err != nil && !errors.Is(err, ErrClosed) {
		t.Fatal(err)
	}
	defer func() { _ = dialer.Close(nil) }()
	select {
	case code := <-gotClose:
		if code != 1002 {
			t.Fatalf("client close code = %d, want 1002", code)
		}
	case <-time.After(testIOTimeout()):
		t.Fatal("client never closed the masked-frame connection")
	}
}

// A message past MaxMessageSize closes the client's connection with 1009.
func TestRobustClientRejectsOversizedMessage(t *testing.T) {
	gotClose := make(chan int, 1)
	addr := startRawWSServer(t, func(conn net.Conn, br *bufio.Reader) {
		writeServerFrame(t, conn, frame.Text, true, bytes.Repeat([]byte("x"), 64))
		_ = conn.SetReadDeadline(time.Now().Add(testIOTimeout()))
		for {
			opcode, payload, err := readClientFrame(br)
			if err != nil {
				gotClose <- -1
				return
			}
			if opcode == frame.Close {
				code := 0
				if len(payload) >= 2 {
					code = int(binary.BigEndian.Uint16(payload))
				}
				gotClose <- code
				return
			}
		}
	})
	events := newClientEvents()
	dialer := NewDialer(Options{Handler: events, MaxMessageSize: 32})
	ctx, cancel := context.WithTimeout(context.Background(), testIOTimeout())
	defer cancel()
	// The violation can close the transport before Dial returns its flush;
	// ErrClosed then means the same as a connection returned to close at once.
	// What the peer observes is asserted below.
	if _, err := dialer.Dial(ctx, "ws://"+addr+"/", nil); err != nil && !errors.Is(err, ErrClosed) {
		t.Fatal(err)
	}
	defer func() { _ = dialer.Close(nil) }()
	select {
	case code := <-gotClose:
		if code != 1009 {
			t.Fatalf("client close code = %d, want 1009", code)
		}
	case <-time.After(testIOTimeout()):
		t.Fatal("client never closed the oversized connection")
	}
}

// A fragmented message from the server is assembled before OnMessage.
func TestRobustClientAssemblesServerFragments(t *testing.T) {
	addr := startRawWSServer(t, func(conn net.Conn, br *bufio.Reader) {
		writeServerFrame(t, conn, frame.Text, false, []byte("hel"))
		writeServerFrame(t, conn, frame.Continuation, true, []byte("lo"))
		_, _, _ = readClientFrame(br) // hold the connection until the test ends
	})
	events := newClientEvents()
	dialer := NewDialer(Options{Handler: events})
	ctx, cancel := context.WithTimeout(context.Background(), testIOTimeout())
	defer cancel()
	if _, err := dialer.Dial(ctx, "ws://"+addr+"/", nil); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dialer.Close(nil) }()
	deadline := time.Now().Add(testIOTimeout())
	for {
		events.mu.Lock()
		msgs := append([]string(nil), events.msgs...)
		types := append([]MessageType(nil), events.types...)
		events.mu.Unlock()
		if len(msgs) == 1 {
			if msgs[0] != "hello" || types[0] != TextMessage {
				t.Fatalf("assembled message = %q type %v", msgs[0], types[0])
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no assembled message, got %v", msgs)
		}
		time.Sleep(time.Millisecond)
	}
}

// A ping from the server is answered with a pong carrying the same payload.
func TestRobustClientAnswersServerPing(t *testing.T) {
	gotPong := make(chan []byte, 1)
	addr := startRawWSServer(t, func(conn net.Conn, br *bufio.Reader) {
		writeServerFrame(t, conn, frame.Ping, true, []byte("hb"))
		_ = conn.SetReadDeadline(time.Now().Add(testIOTimeout()))
		for {
			opcode, payload, err := readClientFrame(br)
			if err != nil {
				gotPong <- nil
				return
			}
			if opcode == frame.Pong {
				gotPong <- payload
				return
			}
		}
	})
	events := newClientEvents()
	dialer := NewDialer(Options{Handler: events})
	ctx, cancel := context.WithTimeout(context.Background(), testIOTimeout())
	defer cancel()
	if _, err := dialer.Dial(ctx, "ws://"+addr+"/", nil); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dialer.Close(nil) }()
	select {
	case pong := <-gotPong:
		if string(pong) != "hb" {
			t.Fatalf("pong payload = %q, want hb", pong)
		}
	case <-time.After(testIOTimeout()):
		t.Fatal("client never answered the ping")
	}
}

// Many goroutines send on one connection while another closes it: every frame
// that reaches the wire is whole, the peers' close follows them, and sends
// report nil or ErrClosed, never a torn stream.
func TestRobustConcurrentSendsRacingClose(t *testing.T) {
	const (
		senders = 8
		each    = 200
	)
	raw := &limitedWire{limit: 8 << 20}
	conn := newLimitedConn(raw, time.Hour)
	t.Cleanup(conn.stopCloseTimer)

	var nilSends, closedSends atomic.Int64
	start := make(chan struct{})
	var wg sync.WaitGroup
	for g := 0; g < senders; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			<-start
			payload := bytes.Repeat([]byte{byte(g)}, 64)
			for i := 0; i < each; i++ {
				switch err := conn.SendBinary(payload); {
				case err == nil:
					nilSends.Add(1)
				case errors.Is(err, ErrClosed):
					closedSends.Add(1)
				default:
					t.Errorf("send %d/%d = %v", g, i, err)
					return
				}
			}
		}(g)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		time.Sleep(time.Millisecond)
		_ = conn.Close(1000, "bye")
	}()
	close(start)
	wg.Wait()

	frames := raw.frames(t)
	last := len(frames) - 1
	if last < 0 || frames[last].Opcode != frame.Close {
		t.Fatalf("stream ends with %v, want the Close frame", frames)
	}
	closes := 0
	for i, f := range frames {
		if f.Opcode == frame.Close {
			closes++
			if i != last {
				t.Fatalf("Close frame at %d of %d", i, len(frames))
			}
		}
		if f.Opcode != frame.Close && len(f.Payload) != 64 {
			t.Fatalf("frame %d torn: %d bytes", i, len(f.Payload))
		}
	}
	if closes != 1 {
		t.Fatalf("close frames = %d, want 1", closes)
	}
	if nilSends.Load()+closedSends.Load() != senders*each {
		t.Fatalf("sends accounted = %d, want %d", nilSends.Load()+closedSends.Load(), senders*each)
	}
}

// Many clients echo concurrently: each connection's replies stay whole and in
// order under load.
func TestRobustManyClientsEchoConcurrently(t *testing.T) {
	addr := startRobustServer(t, Options{}, robustEchoHandler{})
	const clients, each = 16, 100
	var wg sync.WaitGroup
	errs := make(chan error, clients)
	for c := 0; c < clients; c++ {
		wg.Add(1)
		go func(c int) {
			defer wg.Done()
			sock, reader := robustDial(t, addr)
			if err := sock.SetReadDeadline(time.Now().Add(testIOTimeout())); err != nil {
				errs <- err
				return
			}
			for i := 0; i < each; i++ {
				payload := []byte(fmt.Sprintf("c%d-m%d", c, i))
				writeDataFrame(t, sock, frame.Binary, true, payload)
				if _, got, err := readFrameWithOpcode(reader); err != nil || !bytes.Equal(got, payload) {
					errs <- fmt.Errorf("client %d message %d: %v", c, i, err)
					return
				}
			}
			errs <- nil
		}(c)
	}
	wg.Wait()
	for i := 0; i < clients; i++ {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
}

// Pings from many goroutines all get their pong, each with its own payload.
func TestRobustConcurrentPings(t *testing.T) {
	addr := startRobustServer(t, Options{}, robustEchoHandler{})
	conn, reader := robustDial(t, addr)
	if err := conn.SetReadDeadline(time.Now().Add(testIOTimeout())); err != nil {
		t.Fatal(err)
	}
	const goroutines, each = 8, 100
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < each; i++ {
				payload := []byte{byte(g), byte(i)}
				writeDataFrame(t, conn, frame.Ping, true, payload)
			}
		}(g)
	}
	seen := make(map[[2]byte]int)
	for i := 0; i < goroutines*each; i++ {
		opcode, payload, err := readFrameWithOpcode(reader)
		if err != nil {
			t.Fatalf("pong %d: %v", i, err)
		}
		if opcode != frame.Pong || len(payload) != 2 {
			t.Fatalf("pong %d = opcode %d payload %x", i, opcode, payload)
		}
		key := [2]byte{payload[0], payload[1]}
		seen[key]++
	}
	wg.Wait()
	if len(seen) != goroutines*each {
		t.Fatalf("distinct pongs = %d, want %d", len(seen), goroutines*each)
	}
	for key, count := range seen {
		if count != 1 {
			t.Fatalf("pong %v seen %d times", key, count)
		}
	}
}

// Closing the server while clients stream ends every connection without a
// panic and Serve returns.
func TestRobustServerCloseWhileClientsStream(t *testing.T) {
	server, addr, serveDone := startTestServer(t, func() *Server {
		return NewServer(Options{Handler: robustEchoHandler{}})
	})

	const clients = 8
	clientDone := make(chan struct{}, clients)
	streaming := make(chan struct{}, clients)
	for c := 0; c < clients; c++ {
		go func() {
			defer func() { clientDone <- struct{}{} }()
			sock, reader := robustDial(t, addr)
			_ = sock.SetReadDeadline(time.Now().Add(testIOTimeout()))
			writeDataFrame(t, sock, frame.Binary, true, []byte("stream"))
			streaming <- struct{}{}
			for {
				opcode, _, err := readFrameWithOpcode(reader)
				if err != nil {
					return
				}
				if opcode == frame.Close {
					return
				}
			}
		}()
	}
	// Close once every client has streamed a frame. The point is to race the
	// close against live connections; a handshake still in flight may be
	// reset by the server, which the clients would rightly report as fatal.
	for i := 0; i < clients; i++ {
		select {
		case <-streaming:
		case <-time.After(testIOTimeout()):
			t.Fatal("a client never started streaming")
		}
	}
	_ = server.Close(nil)
	select {
	case <-serveDone:
	case <-time.After(testIOTimeout()):
		t.Fatal("Serve did not return after Close")
	}
	for i := 0; i < clients; i++ {
		select {
		case <-clientDone:
		case <-time.After(testIOTimeout()):
			t.Fatal("a client did not finish after the server closed")
		}
	}
	// The server can still be asked to serve a new address and refuses.
	if err := server.Serve(addr); err == nil {
		// Serve on a closed server returns ErrClosed (or the ready state is
		// gone); it must not restart silently.
		t.Fatal("closed server accepted Serve")
	}
}

// A peer that vanishes without a Close frame is cleaned up and the server
// keeps serving.
func TestRobustAbruptPeerDisconnect(t *testing.T) {
	addr := startRobustServer(t, Options{}, robustEchoHandler{})
	for i := 0; i < 5; i++ {
		conn, _ := robustDial(t, addr)
		if tcp, ok := conn.(*net.TCPConn); ok {
			_ = tcp.SetLinger(0) // reset instead of FIN
		}
		_ = conn.Close()
	}
	conn, reader := robustDial(t, addr)
	if err := conn.SetReadDeadline(time.Now().Add(testIOTimeout())); err != nil {
		t.Fatal(err)
	}
	writeDataFrame(t, conn, frame.Text, true, []byte("after resets"))
	if _, payload, err := readFrameWithOpcode(reader); err != nil || string(payload) != "after resets" {
		t.Fatalf("echo after resets = %q, %v", payload, err)
	}
}

// These tests run the framework under concurrent senders, closers, dialers
// and acceptors, where the write path, the close handshake and the scheduler
// interact. They assert the observable contract: whole frames, one Close
// frame last, and every call finishing.

// echoClientHandler echoes nothing back; it records what arrives and lets a
// test send from OnOpen.
type echoClientHandler struct {
	opened chan struct{}
	closed chan struct{}
	onceO  sync.Once
	onceC  sync.Once
	conn   chan *Conn
	msgs   chan []byte
	errs   chan error
}

// dialUserdataHandler forwards every callback to the dial's userdata, which
// carries the per-connection state a handler shared by a whole Dialer cannot.
type dialUserdataHandler struct{}

func (dialUserdataHandler) OnOpen(c *Conn) { c.Userdata().(*echoClientHandler).OnOpen(c) }
func (dialUserdataHandler) OnMessage(c *Conn, m Message) {
	c.Userdata().(*echoClientHandler).OnMessage(c, m)
}
func (dialUserdataHandler) OnClose(c *Conn, e CloseEvent) {
	c.Userdata().(*echoClientHandler).OnClose(c, e)
}

func newEchoClientHandler() *echoClientHandler {
	return &echoClientHandler{
		opened: make(chan struct{}),
		closed: make(chan struct{}),
		conn:   make(chan *Conn, 1),
		msgs:   make(chan []byte, 1024),
		errs:   make(chan error, 1),
	}
}

func (h *echoClientHandler) OnOpen(c *Conn) {
	h.conn <- c
	h.onceO.Do(func() { close(h.opened) })
}
func (h *echoClientHandler) OnMessage(_ *Conn, m Message) {
	select {
	case h.msgs <- append([]byte(nil), m.Payload...):
	default:
	}
}
func (h *echoClientHandler) OnClose(_ *Conn, e CloseEvent) {
	if e.Err != nil {
		select {
		case h.errs <- e.Err:
		default:
		}
	}
	h.onceC.Do(func() { close(h.closed) })
}

// Many concurrent dials through one Dialer, each echoing a burst: the shared
// transport and dialer state hold up and every connection sees its own echoes.
func TestRobustConcurrentDials(t *testing.T) {
	addr := startRobustServer(t, Options{}, robustEchoHandler{})
	// Wait for the listener before the storm: a Dialer attempt that lands
	// before the server binds fails instead of retrying.
	warm, _ := robustDial(t, addr)
	_ = warm.Close()
	const clients, each = 24, 50
	dialer := NewDialer(Options{Handler: dialUserdataHandler{}})
	t.Cleanup(func() { _ = dialer.Close(nil) })

	var wg sync.WaitGroup
	fail := make(chan error, clients)
	for c := 0; c < clients; c++ {
		wg.Add(1)
		go func(c int) {
			defer wg.Done()
			handler := newEchoClientHandler()
			ctx, cancel := context.WithTimeout(context.Background(), testIOTimeout())
			defer cancel()
			conn, err := dialer.Dial(ctx, "ws://"+addr+"/", handler)
			if err != nil {
				fail <- fmt.Errorf("dial %d: %w", c, err)
				return
			}
			select {
			case <-handler.opened:
			case <-time.After(testIOTimeout()):
				fail <- fmt.Errorf("dial %d: no OnOpen", c)
				return
			}
			go func() {
				for i := 0; i < each; i++ {
					if err := conn.SendBinary([]byte(fmt.Sprintf("c%d-%d", c, i))); err != nil {
						return
					}
				}
			}()
			for i := 0; i < each; i++ {
				select {
				case got := <-handler.msgs:
					if want := []byte(fmt.Sprintf("c%d-%d", c, i)); !bytes.Equal(got, want) {
						fail <- fmt.Errorf("client %d message %d = %q", c, i, got)
						return
					}
				case <-time.After(testIOTimeout()):
					fail <- fmt.Errorf("client %d: message %d never arrived", c, i)
					return
				}
			}
			fail <- nil
		}(c)
	}
	wg.Wait()
	for i := 0; i < clients; i++ {
		if err := <-fail; err != nil {
			t.Fatal(err)
		}
	}
}

// Close from many goroutines at once: exactly one Close frame reaches the
// wire, last, and every call returns without a panic.
func TestRobustConcurrentClose(t *testing.T) {
	raw := &limitedWire{limit: 1 << 20}
	conn := newLimitedConn(raw, time.Hour)
	t.Cleanup(conn.stopCloseTimer)

	const closers = 8
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < closers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_ = conn.Close(1000, "many")
		}()
	}
	close(start)
	wg.Wait()

	frames := raw.frames(t)
	if len(frames) != 1 || frames[0].Opcode != frame.Close {
		t.Fatalf("wire = %v, want one Close frame", frames)
	}
}

// Senders and pings race the close on a real socket: frames stay whole, the
// Close frame is last, and the server keeps serving afterwards.
func TestRobustSendsAndPingsRacingClose(t *testing.T) {
	addr := startRobustServer(t, Options{}, robustEchoHandler{})
	conn, reader := robustDial(t, addr)
	if err := conn.SetReadDeadline(time.Now().Add(testIOTimeout())); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				writeDataFrame(t, conn, frame.Binary, true, []byte{byte(g), byte(i), 'd', 'a', 't', 'a'})
			}
		}(g)
	}
	for g := 0; g < 2; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				writeDataFrame(t, conn, frame.Ping, true, []byte{byte(g), byte(i)})
			}
		}(g)
	}
	closed := make(chan struct{})
	go func() {
		defer close(closed)
		wg.Wait()
		writeDataFrame(t, conn, frame.Close, true, []byte{0x03, 0xE8})
	}()

	seenClose := false
	data, pongs := 0, 0
	for {
		opcode, payload, err := readFrameWithOpcode(reader)
		if err != nil {
			break
		}
		switch opcode {
		case frame.Binary:
			if len(payload) != 6 {
				t.Fatalf("torn echo: %d bytes", len(payload))
			}
			data++
		case frame.Pong:
			if len(payload) != 2 {
				t.Fatalf("torn pong: %d bytes", len(payload))
			}
			pongs++
		case frame.Close:
			seenClose = true
		default:
			t.Fatalf("unexpected opcode %d under load", opcode)
		}
		if seenClose {
			break
		}
	}
	<-closed
	if !seenClose || data != 400 || pongs < 50 {
		t.Fatalf("after the storm: close=%v echoes=%d pongs=%d", seenClose, data, pongs)
	}
}

// Backpressure under concurrency: every sender either gets its message queued
// or a clean ErrBackpressure, never a broken stream or a stall.
func TestRobustConcurrentBackpressure(t *testing.T) {
	raw := &limitedWire{limit: 4 << 10}
	server := NewServer(Options{})
	server.Options.MaxOutboundBuffered = raw.limit
	server.Options.OutboundHighWatermark = -1
	conn := &Conn{raw: raw, config: testServerConfig(server)}
	conn.opened.Store(true)

	const senders, each = 8, 200
	var queued, refused atomic.Int64
	var wg sync.WaitGroup
	for g := 0; g < senders; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			payload := bytes.Repeat([]byte{byte(g)}, 100)
			for i := 0; i < each; i++ {
				switch err := conn.SendBinary(payload); {
				case err == nil:
					queued.Add(1)
				case errors.Is(err, ErrBackpressure):
					refused.Add(1)
				case errors.Is(err, ErrClosed):
					return
				default:
					t.Errorf("send %d/%d = %v", g, i, err)
					return
				}
			}
		}(g)
	}
	wg.Wait()
	if queued.Load() == 0 || refused.Load() == 0 {
		t.Fatalf("queued %d refused %d, want both to occur", queued.Load(), refused.Load())
	}
	if total := queued.Load() + refused.Load(); total > senders*each {
		t.Fatalf("sends accounted %d > %d", total, senders*each)
	}
}

// Concurrent Dial storms while the server closes: dials fail cleanly or the
// connections end, nothing panics, and the dialer shuts down.
func TestRobustDialStormDuringServerClose(t *testing.T) {
	server, addr, serveDone := startTestServer(t, func() *Server {
		return NewServer(Options{Handler: robustEchoHandler{}})
	})

	dialer := NewDialer(Options{Handler: dialUserdataHandler{}})
	const dials = 16
	var wg sync.WaitGroup
	for i := 0; i < dials; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			handler := newEchoClientHandler()
			ctx, cancel := context.WithTimeout(context.Background(), testIOTimeout())
			defer cancel()
			_, _ = dialer.Dial(ctx, "ws://"+addr+"/", handler)
		}()
	}
	time.Sleep(20 * time.Millisecond)
	_ = server.Close(nil)
	select {
	case <-serveDone:
	case <-time.After(testIOTimeout()):
		t.Fatal("Serve did not stop")
	}
	_ = dialer.Close(nil)
	wg.Wait()
	// Dialing after everything closed fails cleanly.
	handler := newEchoClientHandler()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := dialer.Dial(ctx, "ws://"+addr+"/", handler); err == nil {
		select {
		case <-handler.closed:
		case <-time.After(testIOTimeout()):
			t.Fatal("dial after shutdown neither failed nor closed")
		}
	}
}

// Heartbeats and traffic share the connection under load: the peer stays up
// and no ping or message is lost or duplicated.
func TestRobustHeartbeatUnderLoad(t *testing.T) {
	server, addr, serveDone := startTestServer(t, func() *Server {
		return NewServer(Options{Handler: robustEchoHandler{}, HeartbeatInterval: 50 * time.Millisecond, HeartbeatTimeout: 200 * time.Millisecond})
	})
	t.Cleanup(func() { stopTestServer(t, server, serveDone) })

	conn, reader := robustDial(t, addr)
	if err := conn.SetReadDeadline(time.Now().Add(testIOTimeout())); err != nil {
		t.Fatal(err)
	}
	// Keep a steady stream going while the server pings: pongs and echoes
	// interleave on the wire, and every echo matches its message.
	const total = 200
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < total; i++ {
			writeDataFrame(t, conn, frame.Binary, true, []byte(fmt.Sprintf("m%d", i)))
			time.Sleep(time.Millisecond)
		}
	}()
	next := 0
	for next < total {
		opcode, payload, err := readFrameWithOpcode(reader)
		if err != nil {
			t.Fatalf("after %d echoes: %v", next, err)
		}
		switch opcode {
		case frame.Binary:
			if string(payload) != fmt.Sprintf("m%d", next) {
				t.Fatalf("echo %d = %q", next, payload)
			}
			next++
		case frame.Ping:
			writeDataFrame(t, conn, frame.Pong, true, payload)
		case frame.Close:
			t.Fatalf("heartbeat closed a live peer after %d echoes", next)
		}
	}
	<-done
}
