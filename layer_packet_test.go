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
	"bytes"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// datagramLayer is a test PacketLayer: every datagram a connection receives
// goes to handle, which consumes it, and onOpen and onTimer run in OnOpen
// and OnTimer.
type datagramLayer struct {
	handle  func(ctx LayerContext, chunk []byte)
	onOpen  func(ctx LayerContext)
	onTimer func(ctx LayerContext)
}

func (l *datagramLayer) CarriesPackets() {}

func (l *datagramLayer) Attach(LayerContext) LayerHandler { return &datagramHandler{layer: l} }

type datagramHandler struct{ layer *datagramLayer }

func (h *datagramHandler) OnOpen(ctx LayerContext) {
	if h.layer.onOpen != nil {
		h.layer.onOpen(ctx)
	}
	ctx.Up().Open()
}

func (h *datagramHandler) OnRead(ctx LayerContext) error {
	if h.layer.handle != nil {
		h.layer.handle(ctx, ctx.Down().PeekChunk())
	}
	return nil
}

func (h *datagramHandler) OnWrite(ctx LayerContext, p []byte) (int, error) {
	return ctx.Down().Write(p)
}

func (h *datagramHandler) OnTimer(ctx LayerContext) error {
	if h.layer.onTimer != nil {
		h.layer.onTimer(ctx)
	}
	return nil
}

func (h *datagramHandler) OnClose(LayerContext, error) {}

// servePacketLayer serves handler on a loopback address of network with the
// chain installed on the listener, and returns the running Events and the
// bound address.
func servePacketLayer(t *testing.T, network string, handler FuncHandler, layers ...Layer) (*Events, string) {
	t.Helper()
	addrCh := make(chan string, 1)
	errCh := make(chan error, 1)
	start := handler.Start
	handler.Start = func(ev *Events) {
		if start != nil {
			start(ev)
		}
		ln, err := ev.Listen(network+"://127.0.0.1:0", WithLayers(layers...))
		if err != nil {
			errCh <- err
			return
		}
		addrCh <- ln.Addr().String()
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
	case addr := <-addrCh:
		return events, addr
	case err := <-errCh:
		t.Fatalf("Listen: %v", err)
	case err := <-done:
		t.Fatalf("Serve: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("server did not start")
	}
	return nil, ""
}

// Only a chain that starts with a PacketLayer may run on a packet
// connection; the same chain on a stream connection runs fine.
func TestPacketLayerGate(t *testing.T) {
	events, addr := servePacketLayer(t, "udp", FuncHandler{}, &datagramLayer{})

	if _, err := events.Listen("udp://127.0.0.1:0", WithLayers(PassThroughLayer)); !errors.Is(err, ErrLayersNeedStream) {
		t.Fatalf("udp Listen with a stream layer: %v, want ErrLayersNeedStream", err)
	}
	if _, err := events.Dial("udp://"+addr, WithLayers(PassThroughLayer)); !errors.Is(err, ErrLayersNeedStream) {
		t.Fatalf("udp Dial with a stream layer: %v, want ErrLayersNeedStream", err)
	}
	rejected, err := net.Dial("udp", "127.0.0.1:1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := events.Adopt(rejected, WithLayers(PassThroughLayer)); !errors.Is(err, ErrLayersNeedStream) {
		t.Fatalf("udp Adopt with a stream layer: %v, want ErrLayersNeedStream", err)
	}

	ln, err := events.Listen("udp://127.0.0.1:0", WithLayers(&datagramLayer{}))
	if err != nil {
		t.Fatalf("udp Listen with a PacketLayer: %v", err)
	}
	_ = ln.Close()
	tln, err := events.Listen("tcp://127.0.0.1:0", WithLayers(&datagramLayer{}))
	if err != nil {
		t.Fatalf("tcp Listen with a PacketLayer: %v", err)
	}
	_ = tln.Close()

	conn, err := events.Dial("udp://"+addr, WithLayers(&datagramLayer{}))
	if err != nil {
		t.Fatalf("udp Dial with a PacketLayer: %v", err)
	}
	_ = conn.Close()

	adoptedConn, err := net.Dial("udp", "127.0.0.1:1")
	if err != nil {
		t.Fatal(err)
	}
	adopted, err := events.Adopt(adoptedConn, WithLayers(&datagramLayer{}))
	if err != nil {
		t.Fatalf("udp Adopt with a PacketLayer: %v", err)
	}
	_ = adopted.Close()
}

// A PacketLayer is handed each datagram alone, and one write below is one
// datagram back.
func TestPacketLayerKeepsDatagramBoundaries(t *testing.T) {
	var seen atomic.Int64
	layer := &datagramLayer{handle: func(ctx LayerContext, chunk []byte) {
		seen.Add(1)
		reply := bytes.Clone(chunk)
		for i := range reply {
			if reply[i] >= 'a' && reply[i] <= 'z' {
				reply[i] -= 'a' - 'A'
			}
		}
		_, _ = ctx.Down().Write(reply)
		_, _ = ctx.Down().Discard(len(chunk))
	}}
	_, addr := servePacketLayer(t, "udp", FuncHandler{}, layer)

	client, err := net.Dial("udp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	_ = client.SetDeadline(time.Now().Add(5 * time.Second))

	read := func(want string) {
		t.Helper()
		buf := make([]byte, 64)
		n, err := client.Read(buf)
		if err != nil {
			t.Fatalf("read reply: %v", err)
		}
		if got := string(buf[:n]); got != want {
			t.Fatalf("reply %q, want %q: the datagram was merged or split", got, want)
		}
	}

	if _, err := client.Write([]byte("one")); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Write([]byte("two")); err != nil {
		t.Fatal(err)
	}
	read("ONE")
	read("TWO")
	time.Sleep(20 * time.Millisecond) // a stray extra call would land here
	if got := seen.Load(); got != 2 {
		t.Fatalf("layer saw %d datagrams, want 2", got)
	}
}

// The framework never leaves a datagram unread because the layer above is
// behind: every datagram is handed to the layer.
func TestPacketLayerIsNotPacedByItsDelivery(t *testing.T) {
	const datagrams = 100
	const size = 2 << 10 // 200 KiB in all, past the 64 KiB delivery bound

	var seen atomic.Int64
	layer := &datagramLayer{handle: func(ctx LayerContext, chunk []byte) {
		seen.Add(1)
		_, _ = ctx.Up().Write(chunk) // the Handler never reads what this delivers
		_, _ = ctx.Down().Discard(len(chunk))
	}}
	_, addr := servePacketLayer(t, "udp", FuncHandler{}, layer)

	client, err := net.Dial("udp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })

	payload := make([]byte, size)
	for i := range payload {
		payload[i] = byte(i)
	}
	for i := 0; i < datagrams; i++ {
		if _, err := client.Write(payload); err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
		if i%16 == 15 {
			time.Sleep(time.Millisecond) // let the receiving side take the burst
		}
	}
	waitFor(t, 5*time.Second, func() bool { return seen.Load() == datagrams })
	if got := seen.Load(); got != datagrams {
		t.Fatalf("layer saw %d of %d datagrams: the bottom layer was paced", got, datagrams)
	}
}

// What a PacketLayer leaves of its datagram is dropped when the turn ends,
// not handed to the layer again as if it were another packet.
func TestPacketLayerDropsTheUnreadTail(t *testing.T) {
	type call struct {
		first byte
		n     int
	}
	var mu sync.Mutex
	var calls []call
	layer := &datagramLayer{handle: func(ctx LayerContext, chunk []byte) {
		mu.Lock()
		calls = append(calls, call{first: chunk[0], n: len(chunk)})
		mu.Unlock()
		_, _ = ctx.Down().Discard(len(chunk) / 2)
	}}
	_, addr := servePacketLayer(t, "udp", FuncHandler{}, layer)

	client, err := net.Dial("udp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })

	if _, err := client.Write(bytes.Repeat([]byte{'a'}, 1024)); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Write(bytes.Repeat([]byte{'b'}, 1024)); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 2*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(calls) >= 2
	})
	time.Sleep(100 * time.Millisecond) // a re-read of the tail would land here
	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 2 || calls[0] != (call{'a', 1024}) || calls[1] != (call{'b', 1024}) {
		t.Fatalf("layer was handed %v, want the two datagrams whole", calls)
	}
}

// A PacketLayer can drive itself with its timer: the turn it arms runs on a
// connection whose socket the listener's peers share.
func TestPacketLayerTimerRuns(t *testing.T) {
	fired := make(chan struct{}, 1)
	layer := &datagramLayer{
		onOpen: func(ctx LayerContext) {
			_ = ctx.SetTimer(time.Now().Add(10 * time.Millisecond))
		},
		onTimer: func(LayerContext) {
			select {
			case fired <- struct{}{}:
			default:
			}
		},
	}
	_, addr := servePacketLayer(t, "udp", FuncHandler{}, layer)

	client, err := net.Dial("udp", addr) // the first packet opens the peer
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	if _, err := client.Write([]byte("tick")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-fired:
	case <-time.After(2 * time.Second):
		t.Fatal("the layer's timer never ran")
	}
}
