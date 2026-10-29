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

package ukcp

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urpc/uio"
)

const testWait = 10 * time.Second

// serveKCP serves handler on an Events with chain installed on every
// listener the tests open through it.
type serveKCP struct {
	*uio.Events
	chain []uio.Layer
}

func newServer(t *testing.T, handler uio.Handler, chain ...uio.Layer) *serveKCP {
	t.Helper()
	return newServerEvents(t, handler, false, chain...)
}

// newServerHalfOpen is newServer with the Events' AllowHalfOpen: the peer's
// end of stream wakes the handler without closing the connection. A half-close
// test needs it on the end it half-closes from — see
// TestHalfCloseThroughKCPAndTLS.
func newServerHalfOpen(t *testing.T, handler uio.Handler, chain ...uio.Layer) *serveKCP {
	t.Helper()
	return newServerEvents(t, handler, true, chain...)
}

func newServerEvents(t *testing.T, handler uio.Handler, halfOpen bool, chain ...uio.Layer) *serveKCP {
	t.Helper()
	started := make(chan struct{})
	events := &uio.Events{Pollers: 2, AllowHalfOpen: halfOpen, Handler: handler, Hooks: uio.FuncHandler{
		Start: func(*uio.Events) { close(started) },
	}}
	done := make(chan error, 1)
	go func() { done <- events.Serve() }()
	t.Cleanup(func() {
		_ = events.Close(nil)
		select {
		case <-done:
		case <-time.After(testWait):
			t.Error("Serve did not return")
		}
	})
	select {
	case <-started:
	case <-time.After(testWait):
		t.Fatal("Serve did not start")
	}
	return &serveKCP{Events: events, chain: chain}
}

func (s *serveKCP) Listen(addr string, opts ...uio.ListenOption) (uio.Listener, error) {
	opts = append([]uio.ListenOption{uio.WithLayers(s.chain...)}, opts...)
	ln, err := s.Events.Listen(addr, opts...)
	if err != nil {
		return nil, err
	}
	return ln, nil
}

// dialKCP serves a client Events with the chain and dials addr through it.
func dialKCP(t *testing.T, handler uio.Handler, addr string, chain ...uio.Layer) (uio.Conn, *uio.Events) {
	t.Helper()
	return dialKCPEvents(t, handler, false, addr, chain...)
}

// dialKCPHalfOpen is dialKCP with the client Events' AllowHalfOpen: the
// peer's end of stream does not close this end, which is what CloseWrite
// needs while the peer's response to it is already on its way back.
func dialKCPHalfOpen(t *testing.T, handler uio.Handler, addr string, chain ...uio.Layer) (uio.Conn, *uio.Events) {
	t.Helper()
	return dialKCPEvents(t, handler, true, addr, chain...)
}

func dialKCPEvents(t *testing.T, handler uio.Handler, halfOpen bool, addr string, chain ...uio.Layer) (uio.Conn, *uio.Events) {
	t.Helper()
	started := make(chan struct{})
	events := &uio.Events{Pollers: 1, AllowHalfOpen: halfOpen, Handler: handler, Hooks: uio.FuncHandler{
		Start: func(*uio.Events) { close(started) },
	}}
	done := make(chan error, 1)
	go func() { done <- events.Serve() }()
	t.Cleanup(func() {
		_ = events.Close(nil)
		select {
		case <-done:
		case <-time.After(testWait):
			t.Error("client Events did not stop")
		}
	})
	select {
	case <-started:
	case <-time.After(testWait):
		t.Fatal("client Events did not start")
	}
	ctx, cancel := context.WithTimeout(context.Background(), testWait)
	defer cancel()
	conn, err := events.DialContext(ctx, addr, uio.WithLayers(chain...))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn, events
}

// collector is a Handler that appends what arrives and reports the close.
type collector struct {
	mu     sync.Mutex
	data   []byte
	closed chan error
}

func newCollector() *collector { return &collector{closed: make(chan error, 1)} }

func (c *collector) handler() uio.Handler {
	return uio.FuncHandler{
		Read: func(conn uio.Conn) error {
			n := conn.InboundBuffered()
			if n == 0 {
				return nil
			}
			buf := make([]byte, n)
			got := conn.Peek(buf)
			if len(got) > 0 {
				c.mu.Lock()
				c.data = append(c.data, got...)
				c.mu.Unlock()
				_, _ = conn.Discard(len(got))
			}
			return nil
		},
		Close: func(_ uio.Conn, err error) {
			select {
			case c.closed <- err:
			default:
			}
		},
	}
}

func (c *collector) bytes() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]byte(nil), c.data...)
}

func (c *collector) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.data)
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(testWait)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// echoHandler writes back everything it is given.
var echoHandler = uio.FuncHandler{
	Read: func(conn uio.Conn) error {
		_, err := conn.WriteTo(conn)
		return err
	},
}

func payloadBytes(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*7 + i/251)
	}
	return b
}

// The zero value runs the Fast profile; out-of-range options are refused.
func TestOptionsResolve(t *testing.T) {
	resolved, err := Options{}.resolve()
	if err != nil {
		t.Fatal(err)
	}
	if resolved.MTU != defaultMTU || resolved.SendWindow != defaultWindow ||
		resolved.RecvWindow != defaultWindow || resolved.Interval != defaultInterval ||
		resolved.Resend != defaultResend || resolved.DeadLink != defaultDeadLink ||
		resolved.KeepAlive != defaultKeepAlive || resolved.MaxBuffered != defaultMaxBuffered {
		t.Fatalf("defaults resolved to %+v", resolved)
	}
	if resolved.NoDelay || resolved.CongestionControl {
		t.Fatalf("the Fast profile enables a low-latency RTO or a congestion window: %+v", resolved)
	}

	for name, opts := range map[string]Options{
		"mtu above the engine's": {MTU: maxMTU + 1},
		"mtu without a payload":  {MTU: kcpOverhead},
		"negative window":        {SendWindow: -1},
		"interval below 10ms":    {Interval: 5 * time.Millisecond},
		"interval above 5s":      {Interval: 6 * time.Second},
		"negative dead link":     {DeadLink: -1},
		"negative buffered":      {MaxBuffered: -1},
	} {
		if _, err := opts.resolve(); !errors.Is(err, ErrInvalidOptions) {
			t.Fatalf("%s: %v, want ErrInvalidOptions", name, err)
		}
	}

	// A negative Resend disables fast retransmit; a negative KeepAlive
	// disables the keepalive.
	off, err := Options{Resend: -1, KeepAlive: -1}.resolve()
	if err != nil || off.Resend != 0 || off.KeepAlive != 0 {
		t.Fatalf("negative switches resolved to %+v, %v", off, err)
	}
}

// A KCP stream runs over a datagram connection: every byte crosses, in order,
// through the engine's segments.
func TestEchoOverPacketConnection(t *testing.T) {
	server := newServer(t, echoHandler, ServerLayer(Options{}))
	ln, err := server.Listen("udp://127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	client := newCollector()
	conn, _ := dialKCP(t, client.handler(), "udp://"+ln.Addr().String(), ClientLayer(Options{}))

	payload := payloadBytes(128 << 10)
	for off := 0; off < len(payload); off += 1024 {
		if _, err := conn.Write(payload[off : off+1024]); err != nil {
			t.Fatalf("write at %d: %v", off, err)
		}
	}
	waitUntil(t, "the echo to return", func() bool { return client.count() >= len(payload) })
	if got := client.bytes(); !bytes.Equal(got[:len(payload)], payload) {
		t.Fatalf("echo corrupted: got %d bytes", len(got))
	}
}

// The same stream over a byte-stream connection: the segments self-frame.
func TestEchoOverStreamConnection(t *testing.T) {
	server := newServer(t, echoHandler, ServerLayer(Options{}))
	ln, err := server.Listen("tcp://127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	client := newCollector()
	conn, _ := dialKCP(t, client.handler(), "tcp://"+ln.Addr().String(), ClientLayer(Options{}))

	payload := payloadBytes(64 << 10)
	for off := 0; off < len(payload); off += 2048 {
		if _, err := conn.Write(payload[off : off+2048]); err != nil {
			t.Fatalf("write at %d: %v", off, err)
		}
	}
	waitUntil(t, "the echo to return", func() bool { return client.count() >= len(payload) })
	if got := client.bytes(); !bytes.Equal(got[:len(payload)], payload) {
		t.Fatalf("echo corrupted: got %d bytes", len(got))
	}
}

// lossyProxy forwards UDP between a client and a server, dropping one packet
// in every few, and can black-hole everything.
type lossyProxy struct {
	front        net.PacketConn
	back         *net.UDPConn
	client       atomic.Value // net.Addr
	dropEvery    int64
	blackhole    atomic.Bool
	reorderEvery atomic.Int64 // hold one packet in this many behind the next
	seen         atomic.Int64 // packets crossing, both directions
	dropped      atomic.Int64
	reordered    atomic.Int64
}

func newLossyProxy(t *testing.T, target string, dropEvery int64) *lossyProxy {
	t.Helper()
	front, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	back, err := net.DialUDP("udp", nil, mustUDPAddr(t, target))
	if err != nil {
		_ = front.Close()
		t.Fatal(err)
	}
	p := &lossyProxy{front: front, back: back, dropEvery: dropEvery}
	t.Cleanup(func() {
		_ = front.Close()
		_ = back.Close()
	})
	go func() { // client → server
		buf := make([]byte, 64<<10)
		var held []byte
		for {
			n, from, err := front.ReadFrom(buf)
			if err != nil {
				return
			}
			p.client.Store(from)
			seen := p.seen.Add(1)
			if p.blackhole.Load() || (p.dropEvery > 0 && seen%p.dropEvery == 0) {
				p.dropped.Add(1)
				continue
			}
			if every := p.reorderEvery.Load(); every > 0 && seen%every == 0 && held == nil {
				held = append([]byte(nil), buf[:n]...)
				continue
			}
			_, _ = back.Write(buf[:n])
			if held != nil {
				_, _ = back.Write(held)
				held = nil
				p.reordered.Add(1)
			}
		}
	}()
	go func() { // server → client
		buf := make([]byte, 64<<10)
		var held []byte
		for {
			n, err := back.Read(buf)
			if err != nil {
				return
			}
			seen := p.seen.Add(1)
			if p.blackhole.Load() || (p.dropEvery > 0 && seen%p.dropEvery == 0) {
				p.dropped.Add(1)
				continue
			}
			if every := p.reorderEvery.Load(); every > 0 && seen%every == 0 && held == nil {
				held = append([]byte(nil), buf[:n]...)
				continue
			}
			if to, ok := p.client.Load().(net.Addr); ok {
				_, _ = front.WriteTo(buf[:n], to)
				if held != nil {
					_, _ = front.WriteTo(held, to)
					held = nil
					p.reordered.Add(1)
				}
			}
		}
	}()
	return p
}

func (p *lossyProxy) addr() string { return p.front.LocalAddr().String() }

func mustUDPAddr(t *testing.T, addr string) *net.UDPAddr {
	t.Helper()
	a, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// Dropped segments are retransmitted: the stream still arrives whole.
func TestLossyLinkStillDelivers(t *testing.T) {
	server := newServer(t, echoHandler, ServerLayer(Options{}))
	ln, err := server.Listen("udp://127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	proxy := newLossyProxy(t, ln.Addr().String(), 3) // every third packet lost
	client := newCollector()
	conn, _ := dialKCP(t, client.handler(), "udp://"+proxy.addr(), ClientLayer(Options{}))

	payload := payloadBytes(64 << 10)
	for off := 0; off < len(payload); off += 1024 {
		if _, err := conn.Write(payload[off : off+1024]); err != nil {
			t.Fatalf("write at %d: %v", off, err)
		}
	}
	waitUntil(t, "the echo to cross the lossy link", func() bool { return client.count() >= len(payload) })
	if got := client.bytes(); !bytes.Equal(got[:len(payload)], payload) {
		t.Fatalf("echo corrupted: got %d bytes", len(got))
	}
	if proxy.dropped.Load() == 0 {
		t.Fatal("the proxy dropped nothing: the lossy path was not exercised")
	}
}

// A peer that goes silent ends the session through the keepalive.
func TestKeepAliveEndsSilentPeer(t *testing.T) {
	server := newServer(t, echoHandler, ServerLayer(Options{}))
	ln, err := server.Listen("udp://127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	proxy := newLossyProxy(t, ln.Addr().String(), 0)
	proxy.blackhole.Store(true)

	client := newCollector()
	dialKCP(t, client.handler(), "udp://"+proxy.addr(), ClientLayer(Options{KeepAlive: 100 * time.Millisecond}))

	select {
	case err := <-client.closed:
		if !errors.Is(err, ErrKeepAliveTimeout) {
			t.Fatalf("closed with %v, want ErrKeepAliveTimeout", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the silent peer never ended the session")
	}
}

// An idle session stays up: the keepalive's WASK probe and the peer's WINS
// answer keep both ends' liveness fresh.
func TestKeepAliveKeepsIdleSession(t *testing.T) {
	server := newServer(t, echoHandler, ServerLayer(Options{}))
	ln, err := server.Listen("udp://127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	client := newCollector()
	conn, _ := dialKCP(t, client.handler(), "udp://"+ln.Addr().String(),
		ClientLayer(Options{KeepAlive: 100 * time.Millisecond}))

	// Six periods of silence: without the probe exchange the session would
	// end at three. The periods leave room for a heavily loaded test host.
	time.Sleep(600 * time.Millisecond)
	select {
	case err := <-client.closed:
		t.Fatalf("the idle session closed: %v", err)
	default:
	}
	if _, err := conn.Write([]byte("still-here")); err != nil {
		t.Fatalf("write after idling: %v", err)
	}
	waitUntil(t, "the echo after idling", func() bool { return client.count() >= len("still-here") })
}

// A full send window refuses writes with ErrOutboundOverflow until the reader
// catches up; then the stream completes.
func TestWriteBackpressure(t *testing.T) {
	const total = 256 << 10
	var received atomic.Int64
	var draining atomic.Bool
	server := newServer(t, uio.FuncHandler{
		Read: func(conn uio.Conn) error {
			if !draining.Load() {
				return nil
			}
			n := conn.InboundBuffered()
			if n > 0 {
				received.Add(int64(n))
				_, _ = conn.Discard(n)
			}
			return nil
		},
	}, ServerLayer(Options{SendWindow: 8, RecvWindow: 8}))
	ln, err := server.Listen("udp://127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	client := newCollector()
	conn, _ := dialKCP(t, client.handler(), "udp://"+ln.Addr().String(),
		ClientLayer(Options{SendWindow: 8, RecvWindow: 8}))

	chunk := make([]byte, 1024)
	accepted := 0
	overflow := false
	for accepted < total {
		n, err := conn.Write(chunk)
		if err == nil {
			accepted += n
			continue
		}
		if errors.Is(err, uio.ErrOutboundOverflow) {
			overflow = true
			break
		}
		t.Fatalf("write: %v", err)
	}
	if !overflow {
		t.Fatal("the sender never ran out of window: backpressure never applied")
	}
	if accepted == 0 {
		t.Fatal("nothing was accepted before the window filled")
	}

	draining.Store(true)
	deadline := time.Now().Add(testWait)
	for accepted < total {
		if time.Now().After(deadline) {
			t.Fatalf("the stream never drained: %d of %d accepted", accepted, total)
		}
		n, err := conn.Write(chunk)
		switch {
		case err == nil:
			accepted += n
		case errors.Is(err, uio.ErrOutboundOverflow):
			time.Sleep(time.Millisecond)
		default:
			t.Fatalf("write: %v", err)
		}
	}
	waitUntil(t, "the server to receive the stream", func() bool { return received.Load() >= total })
}

// CloseWrite ends the peer's stream after the bytes written before it.
func TestCloseWriteEndsThePeerStream(t *testing.T) {
	eof := make(chan string, 1)
	// Half-open on both ends: the peer's close, its answer to this end's FIN,
	// may be back before CloseWrite returns, and without AllowHalfOpen it
	// would close this connection under the call (net.ErrClosed) — a close
	// race no half close should be judged by. See
	// TestHalfCloseThroughKCPAndTLS for the TLS-and-KCP form of it.
	server := newServerHalfOpen(t, uio.FuncHandler{
		Read: func(conn uio.Conn) error {
			buf := make([]byte, 64<<10)
			for {
				n, err := conn.Read(buf)
				if n > 0 {
					continue
				}
				if err != nil {
					eof <- err.Error()
					return err
				}
				return nil
			}
		},
	}, ServerLayer(Options{}))
	ln, err := server.Listen("udp://127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	client := newCollector()
	conn, _ := dialKCPHalfOpen(t, client.handler(), "udp://"+ln.Addr().String(), ClientLayer(Options{}))
	if _, err := conn.Write([]byte("bye")); err != nil {
		t.Fatal(err)
	}
	if err := conn.CloseWrite(); err != nil {
		t.Fatalf("CloseWrite: %v", err)
	}
	select {
	case msg := <-eof:
		if msg != "EOF" {
			t.Fatalf("the peer read %q, want EOF", msg)
		}
	case <-time.After(testWait):
		t.Fatal("the end of the stream never reached the peer")
	}
}

// The session reports itself through FindLayer.
func TestSessionState(t *testing.T) {
	server := newServer(t, echoHandler, ServerLayer(Options{}))
	ln, err := server.Listen("udp://127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	client := newCollector()
	conn, _ := dialKCP(t, client.handler(), "udp://"+ln.Addr().String(), ClientLayer(Options{}))

	state, ok := uio.FindLayer[interface{ State() SessionState }](conn)
	if !ok {
		t.Fatal("the session is not reachable through FindLayer")
	}
	// Readable as soon as the dial returns: the session publishes its state
	// with itself, so OnOpen already sees it.
	if got := state.State(); got.Conv == 0 {
		t.Fatalf("state at open: %+v", got)
	}

	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "the echo to return", func() bool { return client.count() >= 4 })

	if got := state.State(); got.Conv == 0 || got.RTO <= 0 || got.SRTT < 0 {
		t.Fatalf("state %+v", got)
	}

	// And gone once the connection closed: no stale live state remains.
	_ = conn.Close()
	select {
	case <-client.closed:
	case <-time.After(testWait):
		t.Fatal("the connection did not close")
	}
	if got := state.State(); got != (SessionState{}) {
		t.Fatalf("state after close: %+v", got)
	}
}

// A layer built from invalid options ends every connection with the cause,
// and a dial reports it.
func TestInvalidOptionsFailTheConnection(t *testing.T) {
	server := newServer(t, echoHandler, ServerLayer(Options{MTU: 64 << 10}))
	ln, err := server.Listen("udp://127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	client := newCollector()
	started := make(chan struct{})
	events := &uio.Events{Pollers: 1, Handler: client.handler(), Hooks: uio.FuncHandler{
		Start: func(*uio.Events) { close(started) },
	}}
	done := make(chan error, 1)
	go func() { done <- events.Serve() }()
	t.Cleanup(func() {
		_ = events.Close(nil)
		<-done
	})
	select {
	case <-started:
	case <-time.After(testWait):
		t.Fatal("client Events did not start")
	}
	ctx, cancel := context.WithTimeout(context.Background(), testWait)
	defer cancel()
	_, err = events.DialContext(ctx, "udp://"+ln.Addr().String(), uio.WithLayers(ServerLayer(Options{MTU: 64 << 10})))
	if !errors.Is(err, ErrInvalidOptions) {
		t.Fatalf("dial through an invalid layer: %v, want ErrInvalidOptions", err)
	}
}

// TestSegmentScan ignores an incomplete tail: what a stream lower delivered
// mid-segment waits for the rest.
func TestSegmentScan(t *testing.T) {
	// A PUSH segment of three payload bytes.
	seg := make([]byte, kcpOverhead+3)
	seg[4] = 81 // PUSH
	seg[20] = 3
	whole := append(append([]byte(nil), seg...), seg...)

	if !wholeSegments(whole) {
		t.Fatal("two whole segments were refused")
	}
	if wholeSegments(seg[:kcpOverhead+1]) {
		t.Fatal("a segment cut inside its payload was taken whole")
	}
}

// wholeSegments reports whether buf is a whole number of plausible segments;
// the test's mirror of the adapter's scan.
func wholeSegments(buf []byte) bool {
	for len(buf) > 0 {
		if len(buf) < kcpOverhead {
			return false
		}
		total := kcpOverhead + int(binary.LittleEndian.Uint32(buf[20:24]))
		if total > len(buf) {
			return false
		}
		buf = buf[total:]
	}
	return true
}

// An OnBytesWritten observer may read the session state: it runs inside the
// write below the layer, under the engine's lock.
func TestObserverReadsStateWithoutDeadlock(t *testing.T) {
	server := newServer(t, echoHandler, ServerLayer(Options{}))
	ln, err := server.Listen("udp://127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	observed := make(chan struct{}, 1)
	observer := uio.FuncHandler{BytesWritten: func(c uio.Conn, _ int) {
		if s, ok := uio.FindLayer[interface{ State() SessionState }](c); ok {
			_ = s.State()
		}
		select {
		case observed <- struct{}{}:
		default:
		}
	}}
	started := make(chan struct{})
	events := &uio.Events{
		Pollers:  1,
		Handler:  newCollector().handler(),
		Observer: observer,
		Hooks:    uio.FuncHandler{Start: func(*uio.Events) { close(started) }},
	}
	done := make(chan error, 1)
	go func() { done <- events.Serve() }()
	t.Cleanup(func() {
		_ = events.Close(nil)
		<-done
	})
	<-started
	ctx, cancel := context.WithTimeout(context.Background(), testWait)
	defer cancel()
	conn, err := events.DialContext(ctx, "udp://"+ln.Addr().String(), uio.WithLayers(ClientLayer(Options{})))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	wrote := make(chan error, 1)
	go func() {
		_, err := conn.Write([]byte("hello"))
		wrote <- err
	}()
	select {
	case err := <-wrote:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the write blocked: an observer's State() took the engine's lock")
	}
	select {
	case <-observed:
	case <-time.After(5 * time.Second):
		t.Fatal("the observer never ran")
	}
}

// A keepalive probe tells the peer this end's real window: after an idle
// stretch the peer still sees a window to send into.
func TestKeepaliveProbeKeepsTheWindow(t *testing.T) {
	var serverConnMu sync.Mutex
	var serverConn uio.Conn
	server := newServer(t, uio.FuncHandler{
		Read: func(c uio.Conn) error {
			serverConnMu.Lock()
			serverConn = c
			serverConnMu.Unlock()
			_, _ = c.Discard(c.InboundBuffered())
			return nil
		},
	}, ServerLayer(Options{}))
	ln, err := server.Listen("udp://127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	client := newCollector()
	// The period is large enough that a stalled CI runner — a slow turn with
	// no inbound for three periods ends the session — cannot kill the client
	// between probes, and the sleep stays several periods long.
	dialKCP(t, client.handler(), "udp://"+ln.Addr().String(),
		ClientLayer(Options{KeepAlive: 300 * time.Millisecond}))

	// Idle long enough for several probes to cross.
	time.Sleep(time.Second)

	serverConnMu.Lock()
	sc := serverConn
	serverConnMu.Unlock()
	if sc == nil {
		t.Fatal("the server handler never ran")
	}
	state, ok := uio.FindLayer[interface{ State() SessionState }](sc)
	if !ok {
		t.Fatal("no session state on the server connection")
	}
	if wnd := state.State().RemoteWnd; wnd == 0 {
		t.Fatalf("the peer's window reads 0 after the keepalive: a probe advertised a closed window")
	}
	// And the peer can still send into it.
	if _, err := sc.Write([]byte("poke")); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "the peer's send after the keepalive", func() bool { return client.count() >= 4 })
}

// The windows a session accepts fit the header's uint16.
func TestWindowBounds(t *testing.T) {
	if err := (Options{RecvWindow: 65536}).Validate(); !errors.Is(err, ErrInvalidOptions) {
		t.Fatalf("RecvWindow 65536 validated: %v", err)
	}
	if err := (Options{SendWindow: 70000}).Validate(); !errors.Is(err, ErrInvalidOptions) {
		t.Fatalf("SendWindow 70000 validated: %v", err)
	}
	if err := (Options{RecvWindow: 65535, SendWindow: 65535}).Validate(); err != nil {
		t.Fatalf("the largest windows were refused: %v", err)
	}
}

// A layer on an Events whose read buffer cannot hold the MTU refuses the
// session instead of truncating datagrams quietly.
func TestLayerMTUFitsTheReadBuffer(t *testing.T) {
	server := newServer(t, echoHandler, ServerLayer(Options{}))
	ln, err := server.Listen("udp://127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	started := make(chan struct{})
	events := &uio.Events{
		Pollers:       1,
		MaxBufferSize: 512,
		Handler:       newCollector().handler(),
		Hooks:         uio.FuncHandler{Start: func(*uio.Events) { close(started) }},
	}
	done := make(chan error, 1)
	go func() { done <- events.Serve() }()
	t.Cleanup(func() {
		_ = events.Close(nil)
		<-done
	})
	<-started
	ctx, cancel := context.WithTimeout(context.Background(), testWait)
	defer cancel()
	_, err = events.DialContext(ctx, "udp://"+ln.Addr().String(), uio.WithLayers(ClientLayer(Options{})))
	if !errors.Is(err, ErrInvalidOptions) {
		t.Fatalf("dial with a read buffer under the MTU: %v, want ErrInvalidOptions", err)
	}
}
