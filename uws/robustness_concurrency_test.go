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
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urpc/uio/uws/internal/frame"
)

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
	dialer := NewDialer(Options{})
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
	server := NewServer(Options{}, nil)
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
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := probe.Addr().String()
	_ = probe.Close()
	server := NewServer(Options{}, robustEchoHandler{})
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(addr) }()

	dialer := NewDialer(Options{})
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
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := probe.Addr().String()
	_ = probe.Close()
	server := NewServer(Options{HeartbeatInterval: 50 * time.Millisecond, HeartbeatTimeout: 200 * time.Millisecond}, robustEchoHandler{})
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
