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

// paceLayer writes one chunk up on every OnRead until its budget is spent.
type paceLayer struct {
	PassThrough
	chunk     int
	remaining atomic.Int64
	written   atomic.Int64
}

func (p *paceLayer) OnRead(ctx LayerContext) error {
	if p.remaining.Add(-int64(p.chunk)) >= 0 {
		_, _ = ctx.Up().Write(make([]byte, p.chunk))
		p.written.Add(int64(p.chunk))
	}
	return p.PassThrough.OnRead(ctx)
}

// A layer whose delivery to the layer above is full is not called again
// until the layer above reads: the delivery never passes the bound by more
// than one callback's write, and the stream flows on once a read is made.
func TestFullDeliveryPacesTheLayer(t *testing.T) {
	const chunk = 16 << 10
	const calls = 10
	layer := &paceLayer{chunk: chunk}
	layer.remaining.Store(chunk * calls)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		if peer, err := listener.Accept(); err == nil {
			defer peer.Close()
			time.Sleep(500 * time.Millisecond)
		}
	}()
	var armed, read, heldMax atomic.Int64
	events := dialLayered(t, listener.Addr().String(), []Layer{LayerFunc(func(LayerContext) LayerHandler { return layer })}, func(ev *Events) {

		ev.Handler = FuncHandler{
			Read: func(c Conn) error {
				if armed.Load() == 0 {
					if n := int64(c.InboundBuffered()); n > heldMax.Load() {
						heldMax.Store(n)
					}
					return nil // hold what arrived
				}
				read.Add(int64(c.InboundBuffered()))
				_, _ = c.Discard(-1)
				return nil
			},
			Close: func(Conn, error) {},
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), layerWait)
	defer cancel()
	conn, err := events.DialContext(ctx, "tcp://"+listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	// Drive turns while nothing reads above: the layer stalls at its bound.
	for range 50 {
		_ = conn.Wake()
		time.Sleep(time.Millisecond)
	}
	if got := layer.written.Load(); got == 0 {
		t.Fatal("the layer never ran: the pacing assertion would be vacuous")
	} else if got >= chunk*calls {
		t.Fatalf("the layer wrote its whole budget %d past a full delivery", got)
	}
	if bound := int64(DefaultDeliverBound + chunk); heldMax.Load() > bound {
		t.Fatalf("the delivery held %d bytes, want at most %d", heldMax.Load(), bound)
	}
	// A read above makes room: the layer runs on until its budget is spent,
	// and everything arrives.
	armed.Store(1)
	deadline := time.Now().Add(layerWait)
	for read.Load() < chunk*calls && time.Now().Before(deadline) {
		_ = conn.Wake()
		time.Sleep(time.Millisecond)
	}
	if got := read.Load(); got != chunk*calls {
		t.Fatalf("the Handler read %d bytes, want %d", got, chunk*calls)
	}
}

// burstLayer delivers a burst bigger than the bound in one write.
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

// Write takes all of p: the framework bounds what a delivery holds by not
// calling the layer again, never by cutting a write.
func TestUpWriteTakesAll(t *testing.T) {
	layer := &burstLayer{n: 3 * DefaultDeliverBound}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		if peer, err := listener.Accept(); err == nil {
			defer peer.Close()
			time.Sleep(300 * time.Millisecond)
		}
	}()
	var read atomic.Int64
	events := dialLayered(t, listener.Addr().String(), []Layer{LayerFunc(func(LayerContext) LayerHandler { return layer })}, func(ev *Events) {

		ev.Handler = FuncHandler{
			Read: func(c Conn) error {
				read.Add(int64(c.InboundBuffered()))
				_, _ = c.Discard(-1)
				return nil
			},
			Close: func(Conn, error) {},
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), layerWait)
	defer cancel()
	conn, err := events.DialContext(ctx, "tcp://"+listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if got := layer.took.Load(); got != 3*DefaultDeliverBound {
		t.Fatalf("Write took %d bytes of %d, want all of them", got, 3*DefaultDeliverBound)
	}
	if err := conn.Wake(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(layerWait)
	for read.Load() < 3*DefaultDeliverBound && time.Now().Before(deadline) {
		_ = conn.Wake()
		time.Sleep(time.Millisecond)
	}
	if got := read.Load(); got != 3*DefaultDeliverBound {
		t.Fatalf("the Handler read %d bytes, want %d", got, 3*DefaultDeliverBound)
	}
}
