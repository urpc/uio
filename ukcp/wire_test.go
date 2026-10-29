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
	"encoding/binary"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urpc/uio"
	"github.com/urpc/uio/ukcp/internal/kcp"
)

// kcpSegment builds one KCP segment for the raw-wire tests: what a client
// speaking the protocol by hand would put on the wire.
func kcpSegment(conv uint32, cmd, frg byte, wnd uint16, sn, una uint32, payload []byte) []byte {
	seg := make([]byte, kcpOverhead+len(payload))
	binary.LittleEndian.PutUint32(seg[0:], conv)
	seg[4] = cmd
	seg[5] = frg
	binary.LittleEndian.PutUint16(seg[6:], wnd)
	binary.LittleEndian.PutUint32(seg[8:], uint32(time.Now().UnixMilli()))
	binary.LittleEndian.PutUint32(seg[12:], sn)
	binary.LittleEndian.PutUint32(seg[16:], una)
	binary.LittleEndian.PutUint32(seg[20:], uint32(len(payload)))
	copy(seg[kcpOverhead:], payload)
	return seg
}

// spyLayer wraps a layer to count the handlers it opens and closes.
type spyLayer struct {
	inner  uio.Layer
	opened atomic.Int32
	closed atomic.Int32
}

func (s *spyLayer) CarriesPackets() {}

func (s *spyLayer) Attach(ctx uio.LayerContext) uio.LayerHandler {
	return &spyHandler{inner: s.inner.Attach(ctx), spy: s}
}

type spyHandler struct {
	inner uio.LayerHandler
	spy   *spyLayer
}

func (h *spyHandler) OnOpen(ctx uio.LayerContext) {
	h.spy.opened.Add(1)
	h.inner.OnOpen(ctx)
}

func (h *spyHandler) OnRead(ctx uio.LayerContext) error { return h.inner.OnRead(ctx) }

func (h *spyHandler) OnWrite(ctx uio.LayerContext, p []byte) (int, error) {
	return h.inner.OnWrite(ctx, p)
}

func (h *spyHandler) OnTimer(ctx uio.LayerContext) error { return h.inner.OnTimer(ctx) }

func (h *spyHandler) OnClose(ctx uio.LayerContext, err error) {
	h.spy.closed.Add(1)
	h.inner.OnClose(ctx, err)
}

// A TCP connection below ending at a segment boundary ends the stream; one
// ending inside a segment is a truncation the Handler hears. Both need
// AllowHalfOpen: without it the framework closes the connection when the
// socket ends, and the layer never sees the turn that carries the news.
func TestStreamLowerEndsTheSession(t *testing.T) {
	serve := func(t *testing.T, handler uio.Handler) string {
		t.Helper()
		started := make(chan struct{})
		events := &uio.Events{
			Pollers:       2,
			Handler:       handler,
			AllowHalfOpen: true,
			Hooks:         uio.FuncHandler{Start: func(*uio.Events) { close(started) }},
		}
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
		<-started
		ln, err := events.Listen("tcp://127.0.0.1:0", uio.WithLayers(ServerLayer(Options{KeepAlive: -1})))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = ln.Close() })
		return ln.Addr().String()
	}
	dialRaw := func(t *testing.T, addr string) net.Conn {
		t.Helper()
		raw, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = raw.Close() })
		return raw
	}

	t.Run("boundary", func(t *testing.T) {
		got := make(chan []byte, 1)
		ended := make(chan error, 1)
		addr := serve(t, uio.FuncHandler{
			Read: func(c uio.Conn) error {
				buf := make([]byte, 64)
				for {
					n, err := c.Read(buf)
					if n > 0 {
						select {
						case got <- append([]byte(nil), buf[:n]...):
						default:
						}
						continue
					}
					if err != nil {
						select {
						case ended <- err:
						default:
						}
						return err
					}
					return nil
				}
			},
		})
		raw := dialRaw(t, addr)
		if _, err := raw.Write(kcpSegment(7, kcp.IKCP_CMD_PUSH, 0, 1024, 0, 0, []byte("hello"))); err != nil {
			t.Fatal(err)
		}
		if tcp, ok := raw.(*net.TCPConn); ok {
			if err := tcp.CloseWrite(); err != nil {
				t.Fatal(err)
			}
		}
		select {
		case data := <-got:
			if string(data) != "hello" {
				t.Fatalf("read %q, want hello", data)
			}
		case <-time.After(testWait):
			t.Fatal("the segment never arrived")
		}
		select {
		case err := <-ended:
			if !errors.Is(err, io.EOF) {
				t.Fatalf("the boundary end read %v, want io.EOF", err)
			}
		case <-time.After(testWait):
			t.Fatal("the stream never ended at a segment boundary")
		}
	})

	t.Run("truncated", func(t *testing.T) {
		closed := make(chan error, 1)
		addr := serve(t, uio.FuncHandler{
			Read: func(c uio.Conn) error {
				_, _ = c.Discard(c.InboundBuffered())
				return nil
			},
			Close: func(_ uio.Conn, err error) {
				select {
				case closed <- err:
				default:
				}
			},
		})
		raw := dialRaw(t, addr)
		frame := kcpSegment(7, kcp.IKCP_CMD_PUSH, 0, 1024, 0, 0, []byte("hello"))
		if _, err := raw.Write(frame); err != nil {
			t.Fatal(err)
		}
		partial := kcpSegment(7, kcp.IKCP_CMD_PUSH, 0, 1024, 1, 0, []byte("truncated"))
		if _, err := raw.Write(partial[:kcpOverhead+3]); err != nil {
			t.Fatal(err)
		}
		if tcp, ok := raw.(*net.TCPConn); ok {
			if err := tcp.CloseWrite(); err != nil {
				t.Fatal(err)
			}
		}
		select {
		case err := <-closed:
			if !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Fatalf("the truncated stream closed with %v, want io.ErrUnexpectedEOF", err)
			}
		case <-time.After(testWait):
			t.Fatal("the truncated stream never ended")
		}
	})
}

// A peer whose first datagram is not a whole segment is released once the
// deadline for its first segment passes.
func TestInvalidFirstSegmentReleased(t *testing.T) {
	spy := &spyLayer{inner: ServerLayer(Options{HandshakeTimeout: 100 * time.Millisecond})}
	server := newServer(t, echoHandler, spy)
	ln, err := server.Listen("udp://127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	raw, err := net.Dial("udp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	if _, err := raw.Write([]byte{0x7f}); err != nil { // not a segment
		t.Fatal(err)
	}
	waitUntil(t, "the peer to be attached", func() bool { return spy.opened.Load() == 1 })
	waitUntil(t, "the silent peer to be released", func() bool { return spy.closed.Load() == 1 })
}

// A segment in message mode (frg != 0) from the session's own peer is a
// protocol error: its sequence number could never be delivered in order, so
// the session ends instead of leaving a hole behind it.
func TestMessageModeSegmentAborts(t *testing.T) {
	var received atomic.Int64
	closed := make(chan error, 1)
	server := newServer(t, uio.FuncHandler{
		Read: func(c uio.Conn) error {
			n := c.InboundBuffered()
			received.Add(int64(n))
			_, _ = c.Discard(n)
			return nil
		},
		Close: func(_ uio.Conn, err error) {
			select {
			case closed <- err:
			default:
			}
		},
	}, ServerLayer(Options{}))
	ln, err := server.Listen("udp://127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	raw, err := net.Dial("udp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = raw.Close() })

	// A dialect segment opens the session, so the abort below has a Handler
	// to report to.
	if _, err := raw.Write(kcpSegment(9, kcp.IKCP_CMD_PUSH, 0, 1024, 0, 0, []byte("hi"))); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "the session to open", func() bool { return received.Load() >= 2 })

	if _, err := raw.Write(kcpSegment(9, kcp.IKCP_CMD_PUSH, 1, 1024, 1, 0, make([]byte, 1000))); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-closed:
		if !errors.Is(err, ErrProtocol) {
			t.Fatalf("closed with %v, want ErrProtocol", err)
		}
	case <-time.After(testWait):
		t.Fatal("the message-mode segment neither ended the session nor was it rejected")
	}
}

// Data queued behind a closed peer window leaves once the window reopens,
// without a write waking the session: the acknowledgement that reopened it
// runs the flush that sends what waited.
func TestQueuedDataLeavesWhenTheWindowReopens(t *testing.T) {
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
	}, ServerLayer(Options{KeepAlive: -1}))
	ln, err := server.Listen("udp://127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	raw, err := net.Dial("udp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = raw.Close() })

	// The peer speaks with a zero window: what the server writes queues.
	if _, err := raw.Write(kcpSegment(11, kcp.IKCP_CMD_PUSH, 0, 0, 0, 0, []byte("x"))); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "the server session", func() bool {
		serverConnMu.Lock()
		defer serverConnMu.Unlock()
		return serverConn != nil
	})
	serverConnMu.Lock()
	sc := serverConn
	serverConnMu.Unlock()
	if _, err := sc.Write([]byte("queued")); err != nil {
		t.Fatal(err)
	}
	state, ok := uio.FindLayer[interface{ State() SessionState }](sc)
	if !ok {
		t.Fatal("no session state on the server connection")
	}
	waitUntil(t, "the queued data to wait", func() bool { return state.State().Queued > 0 })

	// The peer reopens its window: the queued data must leave now.
	if _, err := raw.Write(kcpSegment(11, kcp.IKCP_CMD_PUSH, 0, 64, 1, 0, []byte("y"))); err != nil {
		t.Fatal(err)
	}
	if err := raw.SetReadDeadline(time.Now().Add(testWait)); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 2048)
	for {
		n, err := raw.Read(buf)
		if err != nil {
			t.Fatalf("the queued data never left: %v", err)
		}
		rest := buf[:n]
		for len(rest) >= kcpOverhead {
			total := kcpOverhead + int(binary.LittleEndian.Uint32(rest[20:24]))
			if total > len(rest) {
				break
			}
			if rest[4] == kcp.IKCP_CMD_PUSH && string(rest[kcpOverhead:total]) == "queued" {
				return
			}
			rest = rest[total:]
		}
	}
}

// A receive window of one reopens as the reader consumes: the second byte a
// sender could not send at first goes through once the first is read.
func TestReceiveWindowReopens(t *testing.T) {
	got := newCollector()
	server := newServer(t, got.handler(), ServerLayer(Options{RecvWindow: 1}))
	ln, err := server.Listen("udp://127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	client := newCollector()
	conn, _ := dialKCP(t, client.handler(), "udp://"+ln.Addr().String(), ClientLayer(Options{}))

	if _, err := conn.Write([]byte("a")); err != nil {
		t.Fatal(err)
	}
	// A separate segment, so the window, not the write size, is what holds
	// the second byte back.
	time.Sleep(50 * time.Millisecond)
	if _, err := conn.Write([]byte("b")); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "both bytes after the window reopened", func() bool { return got.count() >= 2 })
	if data := got.bytes(); !bytes.Equal(data[:2], []byte("ab")) {
		t.Fatalf("received %q, want ab", data)
	}
}

// The end of a stream connection below follows the last byte through receive
// backpressure: with the layer above behind, the end waits for the drain
// instead of overtaking the bytes still queued in the engine.
func TestStreamLowerEndAfterBackpressure(t *testing.T) {
	const segmentSize = 1376 // one full segment each: 2752 bytes in all

	var (
		released atomic.Bool
		received atomic.Int64
	)
	conns := make(chan uio.Conn, 1)
	ended := make(chan error, 1)
	started := make(chan struct{})
	events := &uio.Events{
		Pollers:       2,
		AllowHalfOpen: true,
		Hooks:         uio.FuncHandler{Start: func(*uio.Events) { close(started) }},
		Handler: uio.FuncHandler{
			Open: func(c uio.Conn) { conns <- c },
			Read: func(c uio.Conn) error {
				if !released.Load() {
					// The layer above stays behind: what arrives fills the
					// receive queue and the turn yields at its bound.
					return nil
				}
				buf := make([]byte, 64<<10)
				n, err := c.Read(buf)
				if n > 0 {
					received.Add(int64(n))
				}
				if err != nil {
					select {
					case ended <- err:
					default:
					}
					return err
				}
				return nil
			},
		},
	}
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
	<-started

	ln, err := events.Listen("tcp://127.0.0.1:0", uio.WithLayers(ServerLayer(Options{RecvWindow: 1, KeepAlive: -1})))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	raw, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	// The first segment opens the session, as it does on the wire: write
	// before waiting for the Handler to hear of the connection.
	wire := append(kcpSegment(7, kcp.IKCP_CMD_PUSH, 0, 1024, 0, 0, bytes.Repeat([]byte("a"), segmentSize)),
		kcpSegment(7, kcp.IKCP_CMD_PUSH, 0, 1024, 1, 0, bytes.Repeat([]byte("b"), segmentSize))...)
	if _, err := raw.Write(wire); err != nil {
		t.Fatal(err)
	}
	if tcp, ok := raw.(*net.TCPConn); ok {
		if err := tcp.CloseWrite(); err != nil {
			t.Fatal(err)
		}
	}
	var conn uio.Conn
	select {
	case conn = <-conns:
	case <-time.After(testWait):
		t.Fatal("the session never opened")
	}

	// The session sees the stream below end while the layer above still
	// holds its read back: the bytes waiting in the queue must survive it.
	waitUntil(t, "the lower end to arrive", func() bool {
		s, ok := uio.FindLayer[interface{ debugLifecycle() string }](conn)
		return ok && strings.Contains(s.debugLifecycle(), "lower-end")
	})
	if got := received.Load(); got != 0 {
		t.Fatalf("read %d bytes before the release: the layer above was not behind", got)
	}

	released.Store(true)
	_ = conn.Wake() // a turn reads now that the reader is willing

	waitUntil(t, "every byte before the end", func() bool { return received.Load() >= 2*segmentSize })
	select {
	case err := <-ended:
		if !errors.Is(err, io.EOF) {
			t.Fatalf("the stream ended with %v, want io.EOF", err)
		}
	case <-time.After(testWait):
		t.Fatal("the stream never ended after the drain")
	}
}

// pushBytes reads every segment of a staged datagram: a window update or an
// acknowledgement sharing one with a PUSH must not hide the writer's bytes,
// and a datagram that opens with a PUSH must not count its controls.
func TestPushBytesCountsDataInMixedDatagrams(t *testing.T) {
	push := func(payload string) []byte {
		return kcpSegment(7, kcp.IKCP_CMD_PUSH, 0, 1024, 0, 0, []byte(payload))
	}
	wins := kcpSegment(7, kcp.IKCP_CMD_WINS, 0, 64, 0, 0, nil)
	ack := kcpSegment(7, kcp.IKCP_CMD_ACK, 0, 64, 0, 0, nil)

	cases := []struct {
		name     string
		datagram []byte
		want     int64
	}{
		{"push alone", push("x"), 1},
		{"window update ahead of the data it released", append(wins, push("x")...), 1},
		{"acknowledgement behind the data", append(push("ab"), ack...), 2},
		{"controls only", append(append([]byte(nil), ack...), wins...), 0},
		{"two pushes", append(push("ab"), push("cde")...), 5},
		{"a half segment counts nothing more", append(push("ab"), ack[:10]...), 2},
	}
	for _, tc := range cases {
		if got := pushBytes(tc.datagram); got != tc.want {
			t.Errorf("%s: pushBytes = %d, want %d", tc.name, got, tc.want)
		}
	}
}

// Releasing the staging drops every reference the vector held: an idle
// session after a burst keeps no slab of the send buffer alive.
func TestReleaseStagingDropsReferences(t *testing.T) {
	burst := &conn{}
	burst.out = append(burst.out, make([]byte, 128<<10)...)
	burst.outVec = append(burst.outVec, burst.out[:64], burst.out[64:128])
	burst.releaseStaging()
	if burst.out != nil || burst.outVec != nil {
		t.Fatalf("a burst-sized staging stayed: out cap %d, vector cap %d", cap(burst.out), cap(burst.outVec))
	}

	small := &conn{}
	small.out = append(small.out, make([]byte, 128)...)
	small.outVec = append(small.outVec, small.out[:64], small.out[64:])
	small.releaseStaging()
	if len(small.out) != 0 || len(small.outVec) != 0 || cap(small.out) == 0 {
		t.Fatalf("a small staging was not kept for reuse: out %d/%d, vector %d", len(small.out), cap(small.out), cap(small.outVec))
	}
	for i, seg := range small.outVec[:cap(small.outVec)] {
		if seg != nil {
			t.Fatalf("the shortened vector still references the staging at %d: %v", i, seg[:1])
		}
	}
}

// A session that ended publishes no state and holds nothing, whichever path
// ended it: a fatal inside a send must not be undone by the maintenance that
// follows it.
func TestEndedSessionHoldsNoState(t *testing.T) {
	c := &conn{mss: 1376}
	c.kcp = kcp.NewKCP(7, func([]byte, int) {})
	c.held.Store(42)
	c.state.Store(&SessionState{Conv: 7, RemoteWnd: 32})
	c.ended = true
	c.noteHeld()
	if held := c.held.Load(); held != 0 {
		t.Fatalf("an ended session reports held = %d", held)
	}
	if state := c.State(); state != (SessionState{}) {
		t.Fatalf("an ended session reports state %+v", state)
	}
}

// A segment claiming a 2 GiB payload is refused whole. The length is compared
// on the wire's uint32 before any int conversion: on a 32-bit platform
// int(0x80000000) goes negative, and an int-side check would wave it past the
// bounds test into a slice beyond the buffer.
func TestOversizedSegmentLengthRefused(t *testing.T) {
	hugeHeader := func() []byte {
		seg := kcpSegment(7, kcp.IKCP_CMD_PUSH, 0, 1024, 0, 0, nil)
		binary.LittleEndian.PutUint32(seg[20:], 0x80000000)
		return seg
	}

	t.Run("packet", func(t *testing.T) {
		server := newServer(t, echoHandler, ServerLayer(Options{KeepAlive: -1}))
		ln, err := server.Listen("udp://127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = ln.Close() })

		raw, err := net.Dial("udp", ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = raw.Close() })

		// The malformed datagram is dropped whole; the one behind it still
		// opens the session and echoes.
		if _, err := raw.Write(hugeHeader()); err != nil {
			t.Fatal(err)
		}
		if _, err := raw.Write(kcpSegment(7, kcp.IKCP_CMD_PUSH, 0, 1024, 0, 0, []byte("ok"))); err != nil {
			t.Fatal(err)
		}
		if err := raw.SetReadDeadline(time.Now().Add(testWait)); err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, 2048)
		for {
			n, err := raw.Read(buf)
			if err != nil {
				t.Fatalf("the echo never came back: %v", err)
			}
			rest := buf[:n]
			for len(rest) >= kcpOverhead {
				total := kcpOverhead + int(binary.LittleEndian.Uint32(rest[20:24]))
				if total > len(rest) {
					break
				}
				if rest[4] == kcp.IKCP_CMD_PUSH && string(rest[kcpOverhead:total]) == "ok" {
					return
				}
				rest = rest[total:]
			}
		}
	})

	t.Run("stream", func(t *testing.T) {
		closed := make(chan error, 1)
		opened := make(chan struct{}, 1)
		started := make(chan struct{})
		events := &uio.Events{
			Pollers:       2,
			AllowHalfOpen: true,
			Handler: uio.FuncHandler{
				Open: func(uio.Conn) {
					select {
					case opened <- struct{}{}:
					default:
					}
				},
				Read: func(c uio.Conn) error {
					_, _ = c.Discard(c.InboundBuffered())
					return nil
				},
				Close: func(_ uio.Conn, err error) {
					select {
					case closed <- err:
					default:
					}
				},
			},
			Hooks: uio.FuncHandler{Start: func(*uio.Events) { close(started) }},
		}
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
		<-started

		ln, err := events.Listen("tcp://127.0.0.1:0", uio.WithLayers(ServerLayer(Options{KeepAlive: -1})))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = ln.Close() })
		raw, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = raw.Close() })

		// A valid segment first opens the session, so the abort below has a
		// Handler to report to; then a malformed length ends it: the framing
		// cannot be trusted to find the next segment.
		if _, err := raw.Write(kcpSegment(7, kcp.IKCP_CMD_PUSH, 0, 1024, 0, 0, []byte("x"))); err != nil {
			t.Fatal(err)
		}
		select {
		case <-opened:
		case <-time.After(testWait):
			t.Fatal("the session never opened")
		}
		if _, err := raw.Write(hugeHeader()); err != nil {
			t.Fatal(err)
		}
		select {
		case err := <-closed:
			if !errors.Is(err, errSegmentTooLarge) {
				t.Fatalf("the session closed with %v, want %v", err, errSegmentTooLarge)
			}
		case <-time.After(testWait):
			t.Fatal("the malformed frame neither ended the session nor was refused")
		}
	})
}

// A one-byte write is charged one byte: with MaxBuffered at two and the
// peer's window shut, two one-byte writes queue, and the third is what the
// bound refuses.
func TestTinyWritesChargeTheirOwnBytes(t *testing.T) {
	var mu sync.Mutex
	var serverConn uio.Conn
	server := newServer(t, uio.FuncHandler{
		Read: func(c uio.Conn) error {
			mu.Lock()
			serverConn = c
			mu.Unlock()
			_, _ = c.Discard(c.InboundBuffered())
			return nil
		},
	}, ServerLayer(Options{MaxBuffered: 2, KeepAlive: -1}))
	ln, err := server.Listen("udp://127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	raw, err := net.Dial("udp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = raw.Close() })

	// A zero window opens the session and keeps everything the server writes
	// in its queue.
	if _, err := raw.Write(kcpSegment(11, kcp.IKCP_CMD_PUSH, 0, 0, 0, 0, []byte("x"))); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "the server session", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return serverConn != nil
	})
	mu.Lock()
	sc := serverConn
	mu.Unlock()

	if _, err := sc.Write([]byte("a")); err != nil {
		t.Fatalf("the first byte: %v", err)
	}
	if _, err := sc.Write([]byte("b")); err != nil {
		t.Fatalf("the second byte was refused under a two-byte bound: %v", err)
	}
	if got := sc.OutboundBuffered(); got != 2 {
		t.Fatalf("two queued bytes report %d buffered", got)
	}
	if _, err := sc.Write([]byte("c")); !errors.Is(err, uio.ErrOutboundOverflow) {
		t.Fatalf("a third byte past the bound: %v, want ErrOutboundOverflow", err)
	}
}
