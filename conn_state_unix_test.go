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
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// stateOnTurn drives a turn and reports the state the Handler's Read saw
// once want is contained, or the last one seen at the deadline.
func stateOnTurn(t *testing.T, conn Conn, seen *atomic.Uint32, want ConnState) ConnState {
	t.Helper()
	deadline := time.Now().Add(layerWait)
	for time.Now().Before(deadline) {
		_ = conn.Wake()
		if state := ConnState(seen.Load()); state&want == want {
			return state
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("State() = %b, want bits %b", ConnState(seen.Load()), want)
	return 0
}

// The peer's end shows once what it sent is read, the writing half's once
// CloseWrite ran, and the close's when both halves ended — each read on a
// turn.
func TestConnStateBits(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	peerDone := make(chan struct{})
	peerRelease := make(chan struct{})
	release := sync.OnceFunc(func() { close(peerRelease) })
	t.Cleanup(release)
	go func() {
		defer close(peerDone)
		peer, err := listener.Accept()
		if err != nil {
			return
		}
		defer peer.Close()
		if _, err := peer.Write([]byte("hi")); err != nil {
			return
		}
		<-peerRelease
		_ = peer.(*net.TCPConn).CloseWrite()
		// Keep the reading half open so the connection under test ends when
		// both its halves have, not on the peer's full close.
		_, _ = io.Copy(io.Discard, peer)
	}()
	opened := make(chan Conn, 1)
	var seen, closeSeen atomic.Uint32
	events := dialLayered(t, listener.Addr().String(), nil, func(ev *Events) {
		ev.AllowHalfOpen = true
		ev.Handler = FuncHandler{
			Open: func(c Conn) { signal(opened, c) },
			Read: func(c Conn) error {
				seen.Store(uint32(c.State()))
				_, _ = c.Discard(c.InboundBuffered())
				return nil
			},
			Close: func(c Conn, _ error) { closeSeen.Store(uint32(c.State())) },
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), layerWait)
	defer cancel()
	if _, err := events.DialContext(ctx, "tcp://"+listener.Addr().String()); err != nil {
		t.Fatal(err)
	}
	server := <-opened
	// Shut while the peer's stream is still open: once both halves have
	// ended, the turn that shuts this one closes the connection itself (see
	// closeIfEnded), leaving no later turn to see the state on.
	if err := server.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	if state := stateOnTurn(t, server, &seen, StateWriteShut); state&StatePeerEnded != 0 {
		t.Fatalf("State() = %b, the peer has not ended yet", state)
	}
	release()
	stateOnTurn(t, server, &seen, StatePeerEnded|StateWriteShut)
	deadline := time.Now().Add(layerWait)
	for ConnState(closeSeen.Load())&StateClosed == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	both := StatePeerEnded | StateWriteShut | StateClosing | StateClosed
	if state := ConnState(closeSeen.Load()); state&both != both {
		t.Fatalf("OnClose saw State() = %b, want both ends ended, closing and closed", state)
	}
	<-peerDone
}

// stateProbe records the state of the connection below each time its layer
// reads, and the last state it saw.
type stateProbe struct {
	PassThrough
	last atomic.Uint32
}

func (p *stateProbe) OnRead(ctx LayerContext) error {
	p.last.Store(uint32(ctx.Down().State()))
	return p.PassThrough.OnRead(ctx)
}

// A layer reads its connection's state without a probe read: the peer's end
// reports once the layer above can read to the end.
func TestLayerDownStatePeerEnded(t *testing.T) {
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
		_, _ = peer.Write([]byte("bye"))
		_ = peer.(*net.TCPConn).CloseWrite()
		time.Sleep(300 * time.Millisecond)
	}()
	probe := &stateProbe{}
	var read atomic.Int64
	events := dialLayered(t, listener.Addr().String(), []Layer{LayerFunc(func(LayerContext) LayerHandler { return probe })}, func(ev *Events) {
		ev.AllowHalfOpen = true

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
	conn, err := events.DialContext(ctx, "tcp://"+listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	deadline := time.Now().Add(layerWait)
	for read.Load() < 3 && time.Now().Before(deadline) {
		_ = conn.Wake()
		time.Sleep(time.Millisecond)
	}
	if got := read.Load(); got != 3 {
		t.Fatalf("the Handler read %d bytes, want 3", got)
	}
	deadline = time.Now().Add(layerWait)
	for ConnState(probe.last.Load())&StatePeerEnded == 0 && time.Now().Before(deadline) {
		_ = conn.Wake()
		time.Sleep(time.Millisecond)
	}
	if state := ConnState(probe.last.Load()); state&StatePeerEnded == 0 {
		t.Fatalf("the layer below never reported the peer's end: %b", state)
	}
}
