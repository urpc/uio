//go:build (linux || darwin || netbsd || freebsd || openbsd || dragonfly) && !stdio

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
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// markLayer records that it opened, and opens the layers above it at once.
type markLayer struct {
	PassThrough
	opened atomic.Int32
}

func (m *markLayer) OnOpen(ctx LayerContext) {
	m.opened.Add(1)
	ctx.Up().Open()
}

// layerOf installs one handler as the layer every connection through it gets.
func layerOf(h LayerHandler) Layer {
	return LayerFunc(func(LayerContext) LayerHandler { return h })
}

// overrideListener serves count plain connections that each linger.
func overrideListener(t *testing.T, count int) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		var held []net.Conn
		defer func() {
			for _, peer := range held {
				_ = peer.Close()
			}
		}()
		for range count {
			peer, err := listener.Accept()
			if err != nil {
				return
			}
			held = append(held, peer)
		}
		time.Sleep(200 * time.Millisecond)
	}()
	return listener.Addr().String()
}

func dialOverride(t *testing.T, events *Events, addr string, opts ...DialOption) Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), layerWait)
	defer cancel()
	conn, err := events.DialContext(ctx, "tcp://"+addr, nil, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// A dial's own chain runs instead of the Events' default chain, for that
// connection alone: a later dial without one takes the default again.
func TestDialWithLayersOverridesTheChain(t *testing.T) {
	def, own := &markLayer{}, &markLayer{}
	addr := overrideListener(t, 2)
	events := dialLayered(t, addr, func(ev *Events) {
		ev.Use(LayerFunc(func(LayerContext) LayerHandler { return def }))
	})
	dialOverride(t, events, addr, WithLayers(layerOf(own)))
	if got := own.opened.Load(); got != 1 {
		t.Fatalf("the dial's own layer opened %d times, want 1", got)
	}
	if got := def.opened.Load(); got != 0 {
		t.Fatalf("the default chain opened %d times for an overridden dial, want 0", got)
	}
	dialOverride(t, events, addr)
	if got := def.opened.Load(); got != 1 {
		t.Fatalf("the default chain opened %d times without an override, want 1", got)
	}
}

// An override of no layers is a pass-through connection: it settles, and
// writes still leave.
func TestDialWithLayersEmptyIsPassThrough(t *testing.T) {
	def := &markLayer{}
	addr := overrideListener(t, 1)
	events := dialLayered(t, addr, func(ev *Events) {
		ev.Use(LayerFunc(func(LayerContext) LayerHandler { return def }))
	})
	conn := dialOverride(t, events, addr, WithLayers())
	if _, ok := conn.(*layerNode); !ok {
		t.Fatal("a layered dial did not return the top layer's connection")
	}
	if got := def.opened.Load(); got != 0 {
		t.Fatalf("the default chain ran under an empty override %d times", got)
	}
}

// A dial or adoption asking for its own chain on an Events with no layers is
// refused before the socket moves.
func TestWithLayersNeedsTheStack(t *testing.T) {
	addr := overrideListener(t, 0)
	events := dialLayered(t, addr, nil) // no Use: no stack
	ctx, cancel := context.WithTimeout(context.Background(), layerWait)
	defer cancel()
	_, err := events.DialContext(ctx, "tcp://"+addr, nil, WithLayers(layerOf(&markLayer{})))
	if !errors.Is(err, ErrLayersWithoutStack) {
		t.Fatalf("DialContext = %v, want ErrLayersWithoutStack", err)
	}
	raw, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	_, err = events.Adopt(raw, nil, WithLayers(layerOf(&markLayer{})))
	if !errors.Is(err, ErrLayersWithoutStack) {
		t.Fatalf("Adopt = %v, want ErrLayersWithoutStack", err)
	}
}

// A chain past the stack's layer cap is refused before dialing.
func TestWithLayersLayerCap(t *testing.T) {
	addr := overrideListener(t, 0)
	events := dialLayered(t, addr, func(ev *Events) {
		ev.Use(PassThroughLayer)
	})
	chain := make([]Layer, maxLayers+1)
	for i := range chain {
		chain[i] = PassThroughLayer
	}
	ctx, cancel := context.WithTimeout(context.Background(), layerWait)
	defer cancel()
	if _, err := events.DialContext(ctx, "tcp://"+addr, nil, WithLayers(chain...)); err == nil {
		t.Fatal("a chain past the layer cap was accepted")
	}
}

// A packet connection carries no layers, whether they come from Use or from
// the dial itself.
func TestWithLayersRefusesPacketConnections(t *testing.T) {
	events := dialLayered(t, overrideListener(t, 0), nil)
	ctx, cancel := context.WithTimeout(context.Background(), layerWait)
	defer cancel()
	if _, err := events.DialContext(ctx, "udp://127.0.0.1:1", nil, WithLayers(PassThroughLayer)); !errors.Is(err, ErrLayersNeedStream) {
		t.Fatalf("DialContext = %v, want ErrLayersNeedStream", err)
	}
}

// Concurrent dials with different chains do not cross: each connection opens
// exactly its own.
func TestWithLayersConcurrentDials(t *testing.T) {
	const dials = 8
	addr := overrideListener(t, dials)
	events := dialLayered(t, addr, func(ev *Events) {
		ev.Use(PassThroughLayer)
	})
	own := make([]*markLayer, dials)
	var wg sync.WaitGroup
	for i := range dials {
		own[i] = &markLayer{}
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), layerWait)
			defer cancel()
			conn, err := events.DialContext(ctx, "tcp://"+addr, nil, WithLayers(layerOf(own[i])))
			if err != nil {
				t.Error(err)
				return
			}
			_ = conn.Close()
		}()
	}
	wg.Wait()
	for i, layer := range own {
		if got := layer.opened.Load(); got != 1 {
			t.Fatalf("dial %d's own layer opened %d times, want 1", i, got)
		}
	}
}
