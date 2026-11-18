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
	"context"
	"errors"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// layeredPacketListener serves a UDP listener carrying layers and the given
// handler, and hands back the PacketListener so a test can dial peers on it.
func layeredPacketListener(t *testing.T, handler FuncHandler, layers ...Layer) (*Events, PacketListener) {
	t.Helper()
	lnCh := make(chan PacketListener, 1)
	errCh := make(chan error, 1)
	start := handler.Start
	handler.Start = func(ev *Events) {
		if start != nil {
			start(ev)
		}
		ln, err := ev.Listen("udp://127.0.0.1:0", WithLayers(layers...))
		if err != nil {
			errCh <- err
			return
		}
		lnCh <- ln.(PacketListener)
	}
	events := withFuncs(&Events{Pollers: 2}, handler)
	done := make(chan error, 1)
	go func() { done <- events.Serve() }()
	t.Cleanup(func() {
		_ = events.Close(nil)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("Serve did not return")
		}
	})
	select {
	case ln := <-lnCh:
		return events, ln
	case err := <-errCh:
		t.Fatalf("Listen: %v", err)
	case err := <-done:
		t.Fatalf("Serve: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("server did not start")
	}
	return nil, nil
}

// passPacketLayer delivers each datagram up unchanged and writes datagrams
// down: a packet bottom that opens at once, so a dial settles without traffic.
func passPacketLayer() Layer {
	return &datagramLayer{handle: func(ctx LayerContext, chunk []byte) {
		if len(chunk) > 0 {
			_, _ = ctx.Upper().Write(chunk)
			_, _ = ctx.Lower().Discard(len(chunk))
		}
	}}
}

// A layered DialPeer returns the stack's top, not the socket: a write reaches
// the peer through the layer, and a datagram the peer sends back is delivered
// to that same connection and answered by the Handler above the stack.
func TestLayeredDialPeerReturnsStackTop(t *testing.T) {
	echo := FuncHandler{Read: func(c Conn) error {
		n := c.InboundBuffered()
		if n == 0 {
			return nil
		}
		buf := make([]byte, n)
		got := c.Peek(buf)
		if len(got) > 0 {
			_, _ = c.Write(got)
			_, _ = c.Discard(len(got))
		}
		return nil
	}}
	_, ln := layeredPacketListener(t, echo, passPacketLayer())
	client, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	raddr := client.LocalAddr().(*net.UDPAddr).AddrPort()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	top, err := ln.DialPeerContext(ctx, raddr, "ud")
	if err != nil {
		t.Fatalf("DialPeerContext: %v", err)
	}
	if LayerOf(top) == nil {
		t.Fatal("DialPeer returned the socket, not the stack top")
	}
	if top.Userdata() != "ud" {
		t.Fatalf("userdata = %v, want ud", top.Userdata())
	}
	// The stack opened before the dial returned, so a write goes through now.
	if _, err := top.Write([]byte("hello")); err != nil {
		t.Fatalf("write through the stack: %v", err)
	}
	buf := make([]byte, 64)
	_ = client.SetReadDeadline(time.Now().Add(5 * time.Second))
	n, _, err := client.ReadFrom(buf)
	if err != nil || string(buf[:n]) != "hello" {
		t.Fatalf("client read = %q, %v; want hello", buf[:n], err)
	}
	// The peer's reply routes to the dialed connection and the Handler echoes.
	if _, err := client.WriteTo([]byte("ping"), ln.Addr()); err != nil {
		t.Fatal(err)
	}
	_ = client.SetReadDeadline(time.Now().Add(5 * time.Second))
	n, _, err = client.ReadFrom(buf)
	if err != nil || string(buf[:n]) != "ping" {
		t.Fatalf("echo = %q, %v; want ping", buf[:n], err)
	}
}

// A peer that already exists is reported with ErrPeerExists carrying its
// stack's top — the same Conn the first dial returned, keeping its userdata.
func TestLayeredDialPeerErrPeerExists(t *testing.T) {
	_, ln := layeredPacketListener(t, FuncHandler{}, passPacketLayer())
	raddr := netip.MustParseAddrPort("127.0.0.1:9999")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	first, err := ln.DialPeerContext(ctx, raddr, "first")
	if err != nil {
		t.Fatalf("first dial: %v", err)
	}
	second, err := ln.DialPeerContext(ctx, raddr, "second")
	if !errors.Is(err, ErrPeerExists) {
		t.Fatalf("second dial = %v, want ErrPeerExists", err)
	}
	if second != first {
		t.Fatal("ErrPeerExists did not return the existing stack top")
	}
	if second.Userdata() != "first" {
		t.Fatalf("userdata = %v, want the first dial's", second.Userdata())
	}
}

// A layered dial waits for a turn to open the stack, so it refuses to run on
// one itself, the way the blocking write APIs do.
func TestLayeredDialPeerRefusesOnTurn(t *testing.T) {
	var lnV atomic.Value
	res := make(chan error, 1)
	h := FuncHandler{Read: func(c Conn) error {
		ln := lnV.Load().(PacketListener)
		_, err := ln.DialPeerContext(context.Background(), netip.MustParseAddrPort("127.0.0.1:9998"), nil)
		select {
		case res <- err:
		default:
		}
		_, _ = c.Discard(-1)
		return nil
	}}
	_, ln := layeredPacketListener(t, h, passPacketLayer())
	lnV.Store(ln)
	client, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	if _, err := client.WriteTo([]byte("x"), ln.Addr()); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-res:
		if !errors.Is(err, ErrDialOnEventLoop) {
			t.Fatalf("on-turn DialPeer = %v, want ErrDialOnEventLoop", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no Read ran on a turn")
	}
}

// holdOpenPacketLayer opens its connection but never opens the layers above,
// so a dial waits until its context gives up or the peer closes. reached closes
// once, when a connection's OnOpen has run: its stack is built but not open.
type holdOpenPacketLayer struct {
	reached chan struct{}
	once    sync.Once
}

func newHoldOpenPacketLayer() *holdOpenPacketLayer {
	return &holdOpenPacketLayer{reached: make(chan struct{})}
}

func (l *holdOpenPacketLayer) CarriesPackets() {}
func (l *holdOpenPacketLayer) Attach(LayerContext) LayerHandler {
	return holdOpenHandler{l: l}
}

type holdOpenHandler struct {
	PassThrough
	l *holdOpenPacketLayer
}

func (h holdOpenHandler) OnOpen(LayerContext) {
	h.l.once.Do(func() { close(h.l.reached) })
}

// A cancelled context bounds the wait for a stack that never opens, aborts the
// peer, and reports the context's cause.
func TestLayeredDialPeerContextCancel(t *testing.T) {
	_, ln := layeredPacketListener(t, FuncHandler{}, newHoldOpenPacketLayer())
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := ln.DialPeerContext(ctx, netip.MustParseAddrPort("127.0.0.1:9997"), nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancelled dial = %v, want context.DeadlineExceeded", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("the context did not bound the wait")
	}
}

// While a peer's stack is built but not yet open, ErrPeerExists reports nil,
// not a stack top: the stack is published before its layers attach, so handing
// out that top for off-turn use would race the attach.
func TestLayeredDialPeerErrPeerExistsWhileOpening(t *testing.T) {
	layer := newHoldOpenPacketLayer()
	_, ln := layeredPacketListener(t, FuncHandler{}, layer)
	client, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	raddr := client.LocalAddr().(*net.UDPAddr).AddrPort()
	// An incoming datagram builds the peer's stack; its OnOpen never opens the
	// layers above, so the stack stays built-but-not-open.
	if _, err := client.WriteTo([]byte("x"), ln.Addr()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-layer.reached:
	case <-time.After(5 * time.Second):
		t.Fatal("the peer's stack was never built")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	top, err := ln.DialPeerContext(ctx, raddr, nil)
	if !errors.Is(err, ErrPeerExists) {
		t.Fatalf("dial = %v, want ErrPeerExists", err)
	}
	if top != nil {
		t.Fatal("ErrPeerExists returned a stack top before it opened; that top races the attach")
	}
}

// Closing the listener while a layered dial waits for its stack to open fails
// the dial through the stack's close, rather than leaving it waiting forever.
func TestLayeredDialPeerCloseWhileOpening(t *testing.T) {
	layer := newHoldOpenPacketLayer()
	_, ln := layeredPacketListener(t, FuncHandler{}, layer)
	res := make(chan error, 1)
	go func() {
		_, err := ln.DialPeerContext(context.Background(), netip.MustParseAddrPort("127.0.0.1:9996"), nil)
		res <- err
	}()
	select {
	case <-layer.reached:
	case <-time.After(5 * time.Second):
		t.Fatal("the dial's stack was never built")
	}
	_ = ln.Close()
	select {
	case err := <-res:
		if err == nil {
			t.Fatal("the dial survived the listener closing")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the dial did not return when the listener closed")
	}
}