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
	"net"
	"sync/atomic"
	"testing"
	"time"
)

// boundProbe passes bytes up, recording the largest single delivery it made.
type boundProbe struct {
	PassThrough
	maxWrite atomic.Int64
}

func (p *boundProbe) OnRead(ctx LayerContext) error {
	lower := ctx.Down()
	chunk := lower.PeekChunk()
	if len(chunk) == 0 {
		return nil
	}
	n, _ := ctx.Up().Write(chunk)
	if int64(n) > p.maxWrite.Load() {
		p.maxWrite.Store(int64(n))
	}
	if n > 0 {
		if _, err := lower.Discard(n); err != nil {
			return err
		}
	}
	if n < len(chunk) {
		ctx.Up().Yield()
	}
	return nil
}

// One Up().Write never delivers past MaxInboundBuffered, and the stream
// still arrives whole.
func TestUpWriteStopsAtTheDeliveryBound(t *testing.T) {
	const limit = 1024
	const total = 8 << 10
	layer := &boundProbe{}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		peer, err := listener.Accept()
		if err != nil {
			return
		}
		defer peer.Close()
		_, _ = peer.Write(make([]byte, total))
		time.Sleep(200 * time.Millisecond)
	}()
	var read atomic.Int64
	events := dialLayered(t, listener.Addr().String(), func(ev *Events) {
		ev.MaxInboundBuffered = limit
		ev.Use(LayerFunc(func(LayerContext) LayerHandler { return layer }))
		ev.Handler = FuncHandler{Read: func(c Conn) error {
			read.Add(int64(c.InboundBuffered()))
			_, _ = c.Discard(c.InboundBuffered())
			return nil
		}}
	})
	ctx, cancel := context.WithTimeout(context.Background(), layerWait)
	defer cancel()
	conn, err := events.DialContext(ctx, "tcp://"+listener.Addr().String(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	deadline := time.Now().Add(layerWait)
	for read.Load() < total && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := read.Load(); got != total {
		t.Fatalf("the Handler read %d bytes, want %d", got, total)
	}
	if m := layer.maxWrite.Load(); m > limit {
		t.Fatalf("one Up().Write delivered %d bytes past the %d bound", m, limit)
	}
}

// burstLayer delivers a burst the layer above has not read yet, and records
// the largest write it was allowed.
type burstLayer struct {
	PassThrough
	n    int
	took atomic.Int64
}

func (b *burstLayer) OnOpen(ctx LayerContext) {
	n, _ := ctx.Up().Write(make([]byte, b.n))
	b.took.Store(int64(n))
	ctx.Up().Open()
}

// Without MaxInboundBuffered the delivery bound is DefaultDeliverBound.
func TestUpWriteDefaultDeliveryBound(t *testing.T) {
	layer := &burstLayer{n: 3 * DefaultDeliverBound}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		if peer, err := listener.Accept(); err == nil {
			defer peer.Close()
			time.Sleep(200 * time.Millisecond)
		}
	}()
	var read atomic.Int64
	events := dialLayered(t, listener.Addr().String(), func(ev *Events) {
		ev.Use(LayerFunc(func(LayerContext) LayerHandler { return layer }))
		ev.Handler = FuncHandler{
			Read: func(c Conn) error {
				read.Add(int64(c.InboundBuffered()))
				_, _ = c.Discard(c.InboundBuffered())
				return nil
			},
			Close: func(Conn, error) {},
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), layerWait)
	defer cancel()
	conn, err := events.DialContext(ctx, "tcp://"+listener.Addr().String(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if got := layer.took.Load(); got != DefaultDeliverBound {
		t.Fatalf("one Up().Write took %d bytes, want the %d bound", got, DefaultDeliverBound)
	}
	// The delivered part reaches the Handler whole on the next turn.
	if err := conn.Wake(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(layerWait)
	for read.Load() < DefaultDeliverBound && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := read.Load(); got != DefaultDeliverBound {
		t.Fatalf("the Handler read %d bytes, want %d", got, DefaultDeliverBound)
	}
}
