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
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

// receiptPeer starts a plain listener whose accepted peer reads want bytes
// and reports them.
func receiptPeer(t *testing.T, want int) (addr string, got <-chan string) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	read := make(chan string, 1)
	go func() {
		peer, err := listener.Accept()
		if err != nil {
			return
		}
		defer peer.Close()
		all := make([]byte, want)
		n, _ := io.ReadFull(peer, all)
		read <- string(all[:n])
	}()
	return listener.Addr().String(), read
}

// waitRead takes the peer's read from a receive-only channel.
func waitRead(t *testing.T, c <-chan string) string {
	t.Helper()
	select {
	case v := <-c:
		return v
	case <-time.After(layerWait):
		t.Fatal("no peer read within", layerWait)
		return ""
	}
}

// trackedLayer writes what OnOpen says through a tracked write, and records
// the receipts it hears.
type trackedLayer struct {
	PassThrough
	writes   [][]byte
	sent     chan SendReceipt
	noWriter atomic.Bool
}

func (l *trackedLayer) OnOpen(ctx LayerContext) {
	tw, ok := ctx.Down().(TrackedWriter)
	if !ok {
		l.noWriter.Store(true)
		ctx.Up().Open()
		return
	}
	for _, p := range l.writes {
		if _, _, err := tw.WriteTracked(p); err != nil {
			ctx.Up().Open()
			return
		}
	}
	ctx.Up().Open()
}

func (l *trackedLayer) OnSent(_ LayerContext, r SendReceipt) {
	select {
	case l.sent <- r:
	default:
	}
}

func (l *trackedLayer) OnWrite(ctx LayerContext, p []byte) (int, error) {
	return ctx.Down().Write(p)
}

// A tracked write's receipt reports once the bytes leave the wire.
func TestWriteTrackedReportsWhenBytesLeave(t *testing.T) {
	layer := &trackedLayer{writes: [][]byte{[]byte("tracked")}, sent: make(chan SendReceipt, 2)}
	addr, got := receiptPeer(t, 7)
	events := dialLayered(t, addr, []Layer{LayerFunc(func(LayerContext) LayerHandler { return layer })}, func(ev *Events) {

		ev.Handler = FuncHandler{Close: func(Conn, error) {}}
	})
	ctx, cancel := context.WithTimeout(context.Background(), layerWait)
	defer cancel()
	conn, err := events.DialContext(ctx, "tcp://"+addr, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if layer.noWriter.Load() {
		t.Fatal("the connection below a layer does not implement TrackedWriter")
	}
	layerWaitFor(t, layer.sent, "the receipt")
	if out := waitRead(t, got); out != "tracked" {
		t.Fatalf("the peer read %q, want %q", out, "tracked")
	}
}

// Two tracked writes report in the order they entered.
func TestWriteTrackedReportsInOrder(t *testing.T) {
	layer := &trackedLayer{
		writes: [][]byte{[]byte("one"), []byte("two")},
		sent:   make(chan SendReceipt, 4),
	}
	addr, got := receiptPeer(t, 6)
	events := dialLayered(t, addr, []Layer{LayerFunc(func(LayerContext) LayerHandler { return layer })}, func(ev *Events) {

		ev.Handler = FuncHandler{Close: func(Conn, error) {}}
	})
	ctx, cancel := context.WithTimeout(context.Background(), layerWait)
	defer cancel()
	conn, err := events.DialContext(ctx, "tcp://"+addr, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	first := layerWaitFor(t, layer.sent, "the first receipt")
	second := layerWaitFor(t, layer.sent, "the second receipt")
	if first.wire > second.wire {
		t.Fatalf("receipts reported out of order: %d then %d", first.wire, second.wire)
	}
	if out := waitRead(t, got); out != "onetwo" {
		t.Fatalf("the peer read %q, want %q", out, "onetwo")
	}
}

// A receipt still outstanding when the connection ends is not reported:
// OnClose is the notice.
func TestWriteTrackedStopsReportingAtClose(t *testing.T) {
	layer := &trackedLayer{writes: [][]byte{make([]byte, 16<<20)}, sent: make(chan SendReceipt, 4)}
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
		if raw, ok := peer.(*net.TCPConn); ok {
			_ = raw.SetReadBuffer(4 << 10) // the queue cannot leave
		}
		time.Sleep(500 * time.Millisecond) // and nothing reads it
	}()
	events := dialLayered(t, listener.Addr().String(), []Layer{LayerFunc(func(LayerContext) LayerHandler { return layer })}, func(ev *Events) {

		ev.Handler = FuncHandler{Close: func(Conn, error) {}}
	})
	ctx, cancel := context.WithTimeout(context.Background(), layerWait)
	defer cancel()
	conn, err := events.DialContext(ctx, "tcp://"+listener.Addr().String(), nil)
	if err != nil {
		t.Fatal(err)
	}
	// The write abandoned: its bytes cannot all leave, and the end is the
	// notice, not a receipt.
	if err := conn.CloseWith(errors.New("abandoned")); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-layer.sent:
		t.Fatalf("a receipt reported after the end: %+v", r)
	case <-time.After(200 * time.Millisecond):
	}
}

// A write into the top reports to no one and does not fault.
func TestWriteTrackedOnTopConn(t *testing.T) {
	addr, got := receiptPeer(t, 2)
	events := dialLayered(t, addr, []Layer{LayerFunc(func(LayerContext) LayerHandler { return PassThrough{} })}, func(ev *Events) {

		ev.Handler = FuncHandler{Close: func(Conn, error) {}}
	})
	ctx, cancel := context.WithTimeout(context.Background(), layerWait)
	defer cancel()
	conn, err := events.DialContext(ctx, "tcp://"+addr, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	tw, ok := conn.(TrackedWriter)
	if !ok {
		t.Fatal("the layered connection does not implement TrackedWriter")
	}
	if _, _, err := tw.WriteTracked([]byte("hi")); err != nil {
		t.Fatal(err)
	}
	if out := waitRead(t, got); out != "hi" {
		t.Fatalf("the peer read %q, want %q", out, "hi")
	}
}
