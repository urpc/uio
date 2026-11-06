package utls

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	crand "crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"errors"
	"github.com/urpc/uio"
	"io"
	"math/big"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestHandshakeTimeout(t *testing.T) {
	serverConfig, _ := testConfigs(t)
	server := newEcho()
	transport := serveTLS(t, server, ServerLayer(LayerOptions{TLSConfig: serverConfig, HandshakeTimeout: 150 * time.Millisecond}))
	listener, err := transport.Listen("tcp://127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	silent, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer silent.Close()
	_ = silent.SetReadDeadline(time.Now().Add(testWait))
	began := time.Now()
	if _, err := silent.Read(make([]byte, 1)); err == nil {
		t.Fatal("the silent peer read bytes from a server that never answered")
	} else if errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatal("the silent peer's connection never closed")
	}
	if elapsed := time.Since(began); elapsed > 2*time.Second {
		t.Fatalf("closed after %v, want the 150ms handshake timeout", elapsed)
	}
	select {
	case <-server.opened:
		t.Fatal("a connection whose handshake timed out opened the handler above")
	case <-server.closed:
		t.Fatal("a connection that never opened reached the handler's OnClose")
	default:
	}
}

func TestCloseNotifyEndsTheStream(t *testing.T) {
	serverConfig, clientConfig := testConfigs(t)
	for _, halfOpen := range []bool{false, true} {
		t.Run(map[bool]string{false: "closes", true: "half open"}[halfOpen], func(t *testing.T) {
			ended := make(chan error, 4)
			closed := make(chan error, 1)
			opened := make(chan uio.Conn, 1)
			top := uio.FuncHandler{Open: func(c uio.Conn) {
				signal(opened, c)
			}, Read: func(c uio.Conn) error {
				buf := make([]byte, 64)
				for {
					n, err := c.Read(buf)
					if err != nil {
						ended <- err
						return nil
					}
					if n == 0 {
						return nil
					}
				}
			}, Close: func(_ uio.Conn, err error) {
				signal(closed, err)
			}}
			transport := serveTLSWith(t, top, func(ev *uio.Events) {
				ev.AllowHalfOpen = halfOpen
			}, ServerLayer(LayerOptions{TLSConfig: serverConfig}))
			listener, err := transport.Listen("tcp://127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			conn, client := dialTLS(t, ClientLayer(LayerOptions{TLSConfig: clientConfig}), "tcp://"+listener.Addr().String())
			if _, err := conn.Write([]byte("before the end")); err != nil {
				t.Fatal(err)
			}
			if err := conn.CloseWrite(); err != nil {
				t.Fatal(err)
			}
			if err := wait(t, ended, "the server's io.EOF"); err != io.EOF {
				t.Fatalf("the server read %v at the end, want io.EOF", err)
			}
			if !halfOpen {
				if err := wait(t, closed, "the server's close"); !errors.Is(err, io.EOF) {
					t.Fatalf("the server closed with %v, want io.EOF", err)
				}
				return
			}
			serverConn := wait(t, opened, "the server's open")
			if _, err := serverConn.Write([]byte("still writing")); err != nil {
				t.Fatalf("a half-open server could not write: %v", err)
			}
			if got := string(wait(t, client.data, "the client's read")); got != "still writing" {
				t.Fatalf("the client read %q", got)
			}
		})
	}
}

func TestHandshakeOverAHoldingLayer(t *testing.T) {
	serverConfig, clientConfig := testConfigs(t)
	server := newEcho()
	transport := serveTLS(t, server, ServerLayer(LayerOptions{TLSConfig: serverConfig}))
	listener, err := transport.Listen("tcp://127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	conn, client := dialTLS(t, uio.LayerFunc(func(uio.LayerContext) uio.LayerHandler {
		return &holdUntilFlush{}
	}), ClientLayer(LayerOptions{TLSConfig: clientConfig}), "tcp://"+listener.Addr().String())
	if _, err := conn.Write([]byte("through the hold")); err != nil {
		t.Fatal(err)
	}
	_ = conn.Flush()
	if got := string(wait(t, client.data, "the echo")); got != "through the hold" {
		t.Fatalf("echo = %q", got)
	}
}

type holdUntilFlush struct {
	uio.PassThrough
	mu      sync.Mutex
	held    []byte
	writeMu sync.Mutex
}

func (h *holdUntilFlush) OnWrite(ctx uio.LayerContext, p []byte) (int, error) {
	h.mu.Lock()
	h.held = append(h.held, p...)
	h.mu.Unlock()
	_ = ctx.Wake()
	return len(p), nil
}

func (h *holdUntilFlush) OutboundHeld(uio.LayerContext) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.held)
}

func (h *holdUntilFlush) OnRead(ctx uio.LayerContext) error {
	if err := h.PassThrough.OnRead(ctx); err != nil {
		return err
	}
	return h.drain(ctx)
}

func (h *holdUntilFlush) OnClosing(ctx uio.LayerContext, _ error) {
	_ = h.drain(ctx)
}

func (h *holdUntilFlush) drain(ctx uio.LayerContext) error {
	h.writeMu.Lock()
	defer h.writeMu.Unlock()
	h.mu.Lock()
	held := h.held
	h.held = nil
	h.mu.Unlock()
	if len(held) == 0 {
		return nil
	}
	_, err := ctx.Lower().Write(held)
	return err
}

func TestHandshakeFloodIsNotBuffered(t *testing.T) {
	serverConfig, _ := testConfigs(t)
	server := newEcho()
	transport := serveTLS(t, server, ServerLayer(LayerOptions{TLSConfig: serverConfig, HandshakeTimeout: 10 * time.Second}))
	listener, err := transport.Listen("tcp://127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	peer, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	record := make([]byte, 5+16<<10)
	record[0], record[1], record[2] = 22, 3, 1
	binary.BigEndian.PutUint16(record[3:5], 16<<10)
	record[5], record[6], record[7], record[8] = 1, 0, 0xff, 0xff
	began := time.Now()
	sent := 0
	for time.Since(began) < 5*time.Second {
		_ = peer.SetWriteDeadline(time.Now().Add(time.Second))
		n, err := peer.Write(record)
		sent += n
		if err != nil {
			break
		}
		record[5], record[6], record[7], record[8] = 0, 0, 0, 0
	}
	if sent > 64<<20 {
		t.Fatalf("the server took %d MiB of a handshake flood", sent>>20)
	}
	if took := time.Since(began); took > 4*time.Second {
		t.Fatalf("the flood ran %v: the server kept reading it", took)
	}
}

func TestHandshakeWithATinyStagingBound(t *testing.T) {
	serverConfig, clientConfig := testConfigs(t)
	server := newEcho()
	transport := serveTLS(t, server, ServerLayer(LayerOptions{TLSConfig: serverConfig, StagingBound: 512}))
	listener, err := transport.Listen("tcp://127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	conn, client := dialTLS(t, ClientLayer(LayerOptions{TLSConfig: clientConfig, StagingBound: 512}), "tcp://"+listener.Addr().String())
	payload := bytes.Repeat([]byte("x"), 8<<10)
	if _, err := conn.Write(payload); err != nil {
		t.Fatal(err)
	}
	var got []byte
	for len(got) < len(payload) {
		got = append(got, wait(t, client.data, "the echo")...)
	}
	if !bytes.Equal(got, payload) {
		t.Fatal("the echo came back changed")
	}
}

func TestEndBelowWithoutCloseNotify(t *testing.T) {
	for _, truncated := range []bool{false, true} {
		t.Run(map[bool]string{false: "at a record's edge", true: "inside a record"}[truncated], func(t *testing.T) {
			serverConfig, clientConfig := testConfigs(t)
			listener, err := tls.Listen("tcp", "127.0.0.1:0", serverConfig)
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			release := make(chan struct{})
			defer close(release)
			go func() {
				peer, err := listener.Accept()
				if err != nil {
					return
				}
				defer peer.Close()
				tc := peer.(*tls.Conn)
				if tc.Handshake() != nil {
					return
				}
				raw := tc.NetConn().(*net.TCPConn)
				if truncated {
					_, _ = raw.Write([]byte{23, 3, 3, 0, 40, 1, 2, 3})
				}
				_ = raw.CloseWrite()
				<-release
			}()
			ended := make(chan error, 4)
			handler := uio.FuncHandler{Read: func(c uio.Conn) error {
				if _, err := c.Read(nil); err != nil {
					ended <- err
				}
				return nil
			}, Close: func(_ uio.Conn, err error) {
				signal(ended, err)
			}}
			events := serveTLSWith(t, handler, func(ev *uio.Events) {
				ev.AllowHalfOpen = true
			}, ClientLayer(LayerOptions{TLSConfig: clientConfig}))
			ctx, cancel := context.WithTimeout(context.Background(), testWait)
			defer cancel()
			if _, err := events.DialContext(ctx, "tcp://"+listener.Addr().String()); err != nil {
				t.Fatal(err)
			}
			err = wait(t, ended, "the end below")
			if truncated {
				if err == io.EOF || err == nil {
					t.Fatalf("a record cut short ended as %v, want an error", err)
				}
			} else if err != io.EOF {
				t.Fatalf("the end at a record's edge reached the handler as %v, want io.EOF", err)
			}
		})
	}
}

func TestStateDuringAHandshake(t *testing.T) {
	_, clientConfig := testConfigs(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	gotHello := make(chan struct{})
	go func() {
		peer, err := listener.Accept()
		if err != nil {
			return
		}
		defer peer.Close()
		if n, _ := peer.Read(make([]byte, 8192)); n > 0 {
			close(gotHello)
		}
		time.Sleep(testWait)
	}()
	attached := make(chan *conn, 1)
	layer := ClientLayer(LayerOptions{TLSConfig: clientConfig})
	events := serveTLS(t, uio.FuncHandler{}, uio.LayerFunc(func(ctx uio.LayerContext) uio.LayerHandler {
		c := layer.Attach(ctx).(*conn)
		attached <- c
		return c
	}))
	ctx, cancel := context.WithCancel(context.Background())
	dialed := make(chan error, 1)
	go func() {
		_, err := events.DialContext(ctx, "tcp://"+listener.Addr().String())
		dialed <- err
	}()
	engine := <-attached
	<-gotHello
	stated := make(chan ConnectionState, 1)
	go func() {
		stated <- engine.State()
	}()
	select {
	case state := <-stated:
		if state.HandshakeComplete {
			t.Fatal("a handshake still waiting reported itself complete")
		}
	case <-time.After(time.Second):
		t.Fatal("State blocked on a handshake waiting for its peer")
	}
	cancel()
	if err := wait(t, dialed, "the canceled dial"); err == nil {
		t.Fatal("the canceled dial succeeded")
	}
}

var gate uio.Layer = uio.LayerFunc(func(uio.LayerContext) uio.LayerHandler {
	return &gateLayer{}
})

type gateLayer struct {
	uio.PassThrough
	pending []byte
	ready   bool
}

func (g *gateLayer) OnOpen(ctx uio.LayerContext) {
	_, _ = ctx.Lower().Write([]byte("HELLO?"))
}

func (g *gateLayer) OnRead(ctx uio.LayerContext) error {
	if g.ready {
		return g.PassThrough.OnRead(ctx)
	}
	lower := ctx.Lower()
	chunk := lower.PeekChunk()
	g.pending = append(g.pending, chunk...)
	_, _ = lower.Discard(len(chunk))
	for !g.ready && len(g.pending) >= len("HELLO?") {
		switch string(g.pending[:len("HELLO?")]) {
		case "HELLO?":
			g.pending = g.pending[len("HELLO?"):]
			_, _ = ctx.Lower().Write([]byte("HELLO!"))
		case "HELLO!":
			g.pending = g.pending[len("HELLO!"):]
			g.ready = true
		default:
			g.ready = true
		}
	}
	if g.ready {
		_, _ = ctx.Upper().Write(g.pending)
		g.pending = nil
		ctx.Open()
	}
	return nil
}

func roundTripThrough(t *testing.T, conn uio.Conn, echo *collector, payload []byte) {
	t.Helper()
	written := make(chan error, 1)
	go func() {
		_, err := conn.Write(payload)
		written <- err
	}()
	select {
	case <-echo.done:
	case <-time.After(testWait):
		t.Fatalf("no echo of %d bytes within %v", len(payload), testWait)
	}
	if err := <-written; err != nil {
		t.Fatalf("write %d bytes: %v", len(payload), err)
	}
	if got := echo.payload(); !bytes.Equal(got[:len(payload)], payload) {
		t.Fatalf("the echo altered the %d bytes it answered", len(payload))
	}
}

type collector struct {
	target int64
	mu     sync.Mutex
	got    []byte
	done   chan struct{}
	closed chan error
	answer bool
}

func newCollector(target int64) *collector {
	return &collector{target: target, done: make(chan struct{}), closed: make(chan error, 4)}
}

func (e *collector) OnOpen(uio.Conn) {
}

func (e *collector) OnRead(c uio.Conn) error {
	chunk := c.PeekChunk()
	n := len(chunk)
	e.mu.Lock()
	e.got = append(e.got, chunk...)
	reached := int64(len(e.got)) >= e.target
	e.mu.Unlock()
	if e.answer {
		if _, err := c.Write(chunk); err != nil {
			return err
		}
	}
	_, _ = c.Discard(n)
	if reached {
		select {
		case <-e.done:
		default:
			close(e.done)
		}
	}
	return nil
}

func (e *collector) OnTimer(uio.Conn) error {
	return nil
}

func (e *collector) OnClose(_ uio.Conn, err error) {
	e.closed <- err
}

func (e *collector) payload() []byte {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]byte(nil), e.got...)
}

func startClient(t *testing.T, handler uio.Handler, layers ...uio.Layer) *layeredEvents {
	t.Helper()
	started := make(chan struct{})
	events := &uio.Events{Pollers: 1, Handler: handler, Hooks: uio.FuncHandler{Start: func(*uio.Events) {
		close(started)
	}}}
	done := make(chan error, 1)
	go func() {
		done <- events.Serve()
	}()
	t.Cleanup(func() {
		_ = events.Close(nil)
		<-done
	})
	<-started
	return &layeredEvents{Events: events, layers: layers}
}

func dialThrough(t *testing.T, events *layeredEvents, addr string) uio.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testWait)
	defer cancel()
	conn, err := events.DialContext(ctx, "tcp://"+addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = conn.Close()
	})
	return conn
}

func TestInteropExplicitVersions(t *testing.T) {
	for _, version := range []struct {
		name  string
		value uint16
	}{{"tls 1.2", tls.VersionTLS12}, {"tls 1.3", tls.VersionTLS13}} {
		t.Run(version.name, func(t *testing.T) {
			certificate, pool := testCertificate(t)
			serverConfig := &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: version.value, MaxVersion: version.value}
			clientConfig := &tls.Config{ServerName: "localhost", RootCAs: pool, MinVersion: version.value, MaxVersion: version.value}
			server := newEcho()
			transport := serveTLS(t, server, ServerLayer(LayerOptions{TLSConfig: serverConfig}))
			listener, err := transport.Listen("tcp://127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			client, err := tls.Dial("tcp", listener.Addr().String(), clientConfig)
			if err != nil {
				t.Fatal(err)
			}
			if state := client.ConnectionState(); state.Version != version.value {
				t.Fatalf("negotiated %#x, want %#x", state.Version, version.value)
			}
			_ = client.Close()
			where := stdEcho(t, serverConfig)
			sink := newCollector(5)
			events := startClient(t, sink, ClientLayer(LayerOptions{TLSConfig: clientConfig}))
			conn := dialThrough(t, events, string(where))
			state := uio.LayerOf(conn).(interface{ State() ConnectionState }).State()
			if state.Version != version.value {
				t.Fatalf("negotiated %#x, want %#x", state.Version, version.value)
			}
			roundTripThrough(t, conn, sink, []byte("hello"))
		})
	}
}

func stdEcho(t *testing.T, config *tls.Config) addr {
	t.Helper()
	listener, err := tls.Listen("tcp", "127.0.0.1:0", config)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	t.Cleanup(func() {
		<-done
	})
	go func() {
		defer close(done)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				buf := make([]byte, 4096)
				for {
					n, err := conn.Read(buf)
					if err != nil {
						return
					}
					if _, err := conn.Write(buf[:n]); err != nil {
						return
					}
				}
			}()
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
	})
	return addr(listener.Addr().String())
}

type addr string

func TestTLSOverGateLayer(t *testing.T) {
	serverConfig, clientConfig := testConfigs(t)
	server := newEcho()
	transport := serveTLS(t, server, ServerLayer(LayerOptions{TLSConfig: serverConfig}), gate)
	listener, err := transport.Listen("tcp://127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	client, echo := dialTLS(t, ClientLayer(LayerOptions{TLSConfig: clientConfig}), gate, "tcp://"+listener.Addr().String())
	if _, err := client.Write([]byte("through the gate")); err != nil {
		t.Fatal(err)
	}
	if got := string(wait(t, echo.data, "echo through gate and TLS")); got != "through the gate" {
		t.Fatalf("read %q through gate and TLS", got)
	}
	_ = client.Close()
	wait(t, server.closed, "server close")
}

type contractLower struct {
	uio.Lower
	local, remote net.Addr
	written       bytes.Buffer
	writeErr      error
	inbound       []byte
}

func (l *contractLower) LocalAddr() net.Addr {
	return l.local
}

func (l *contractLower) RemoteAddr() net.Addr {
	return l.remote
}

func (l *contractLower) Write(p []byte) (int, error) {
	if l.writeErr != nil {
		return 0, l.writeErr
	}
	return l.written.Write(p)
}

func (l *contractLower) PeekChunk() []byte {
	return l.inbound
}

func (l *contractLower) Discard(n int) (int, error) {
	l.inbound = l.inbound[n:]
	return n, nil
}

func (*contractLower) State() uio.ConnState {
	return 0
}

type contractUpper struct {
	uio.Upper
	buffered int
	yielded  bool
}

func (u *contractUpper) Buffered() int {
	return u.buffered
}

func (u *contractUpper) Yield() {
	u.yielded = true
}

type contractContext struct {
	uio.LayerContext
	lower *contractLower
	upper *contractUpper
	cause error
}

func (c *contractContext) Lower() uio.Lower {
	return c.lower
}

func (c *contractContext) Upper() uio.Upper {
	return c.upper
}

func (c *contractContext) Abort(err error) error {
	c.cause = err
	return nil
}

func TestCiphertextTransportPreservesRecordsAcrossPendingAndBorrowedChunks(t *testing.T) {
	record := func(p string) []byte {
		wire := append([]byte{23, 3, 3, 0, 0}, []byte(p)...)
		binary.BigEndian.PutUint16(wire[3:], uint16(len(p)))
		return wire
	}
	first, second := record("first-record"), record("next")
	wire := append(append([]byte(nil), first...), second...)
	for split := 0; split <= len(wire); split++ {
		for _, readSize := range []int{1, 3, len(wire) + 1} {
			var pending []byte
			if split != 0 {
				pending = bytes.Clone(wire[:split])
			}
			c := &conn{pending: pending, live: wire[split:]}
			transport := &transport{c: c}
			var got []byte
			for len(got) < len(wire) {
				p := make([]byte, readSize)
				n, err := transport.Read(p)
				if err != nil || n == 0 {
					t.Fatalf("split=%d size=%d Read=%d,%v", split, readSize, n, err)
				}
				if len(got) < len(first) && len(got)+n > len(first) {
					t.Fatal("one transport read crossed the TLS record boundary")
				}
				got = append(got, p[:n]...)
			}
			if !bytes.Equal(got, wire) || !c.rec.atBoundary() || c.pending != nil {
				t.Fatal("record assembly changed bytes or retained drained ciphertext")
			}
			if n, err := transport.Read(make([]byte, 1)); n != 0 || err != errWouldBlock {
				t.Fatalf("empty live stream=%d,%v, want would block", n, err)
			}
		}
	}
}

func TestCiphertextTransportClosureSuspensionAndDelegation(t *testing.T) {
	lower := &contractLower{local: &net.TCPAddr{Port: 12}, remote: &net.TCPAddr{Port: 34}}
	ctx := &contractContext{lower: lower, upper: &contractUpper{}}
	c := &conn{ctx: ctx}
	wire := &transport{c: c}
	if wire.LocalAddr() != lower.local || wire.RemoteAddr() != lower.remote {
		t.Fatal("transport changed endpoint addressing")
	}
	for _, set := range []func(time.Time) error{wire.SetDeadline, wire.SetReadDeadline, wire.SetWriteDeadline} {
		if err := set(time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := wire.Write([]byte("record")); n != 6 || err != nil || lower.written.String() != "record" {
		t.Fatalf("transport write=%d,%v", n, err)
	}
	cause := errors.New("underlying writer failed")
	lower.writeErr = cause
	if n, err := wire.Write([]byte("refused")); n != 0 || !errors.Is(err, cause) {
		t.Fatalf("transport write error=%d,%v", n, err)
	}
	c.yield = func(struct{}) bool {
		c.mu.Lock()
		c.live = []byte{23, 3, 3, 0, 1, 'x'}
		c.mu.Unlock()
		return true
	}
	if n, err := wire.Read(make([]byte, 6)); n != 6 || err != nil {
		t.Fatalf("resumed transport read=%d,%v", n, err)
	}
	c.yield = func(struct{}) bool {
		return false
	}
	if n, err := wire.Read(make([]byte, 1)); n != 0 || !errors.Is(err, net.ErrClosed) {
		t.Fatalf("canceled coroutine read=%d,%v", n, err)
	}
	c.yield, c.lowerEnded = nil, true
	if n, err := wire.Read(make([]byte, 1)); n != 0 || err != io.EOF {
		t.Fatalf("underlying EOF=%d,%v", n, err)
	}
	if err := wire.Close(); err != nil {
		t.Fatal(err)
	}
	if n, err := wire.Read(make([]byte, 1)); n != 0 || err != errClosed {
		t.Fatalf("closed transport read=%d,%v", n, err)
	}
	if n, err := wire.Write([]byte("closed")); n != 0 || err != errClosed {
		t.Fatalf("closed transport write=%d,%v", n, err)
	}
	if errWouldBlock.Error() == "" || !errWouldBlock.(net.Error).Temporary() || errWouldBlock.(net.Error).Timeout() {
		t.Fatal("would-block error did not retain its temporary/non-timeout contract")
	}
}

func TestTLSLayerRejectsUnopenedAndInvalidLifecycleOperations(t *testing.T) {
	ctx := &contractContext{lower: &contractLower{}, upper: &contractUpper{}}
	c := &conn{ctx: ctx, tlsLayer: &tlsLayer{}}
	if _, err := c.OnWrite(ctx, []byte("not negotiated")); !errors.Is(err, net.ErrClosed) {
		t.Fatal("an unopened TLS engine accepted application output")
	}
	if err := c.CloseWrite(ctx); !errors.Is(err, net.ErrClosed) {
		t.Fatal("an unopened TLS engine accepted CloseWrite")
	}
	c.OnClosing(ctx, nil)
	if err := c.OnRead(ctx); err != nil {
		t.Fatal(err)
	}
	c.ready = true
	ctx.upper.buffered = DefaultStagingBound
	if err := c.OnRead(ctx); err != nil || !ctx.upper.yielded {
		t.Fatal("a full application delivery was not paced")
	}
	if err := c.OnTimer(ctx); err != nil {
		t.Fatal("a ready TLS connection still timed out its handshake")
	}
	c.pending, c.readAt = []byte("consumed-kept"), len("consumed-")
	c.compactLocked()
	if string(c.pending) != "kept" || c.readAt != 0 {
		t.Fatal("ciphertext compaction lost the unread suffix")
	}
	cause := errors.New("invalid TLS layer")
	bad := &conn{tlsLayer: &tlsLayer{err: cause}}
	bad.OnOpen(ctx)
	if !errors.Is(ctx.cause, cause) || bad.tls != nil {
		t.Fatal("invalid layer created an engine instead of aborting")
	}
	ctx.cause = nil
	bad = &conn{tlsLayer: &tlsLayer{client: true, config: &tls.Config{}, handshakeTimeout: -1}}
	bad.OnOpen(ctx)
	if ctx.cause == nil || bad.ready {
		t.Fatal("failed TLS negotiation opened its application layer")
	}
	bad.OnClose(ctx, nil)
}

const testWait = 10 * time.Second

const cleanupWait = 30 * time.Second

func testCertificate(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), crand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "utls test"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), DNSNames: []string{"localhost", "127.0.0.1"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}}
	der, err := x509.CreateCertificate(crand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	certificate := tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	return certificate, pool
}

func testConfigs(t *testing.T) (server *tls.Config, client *tls.Config) {
	t.Helper()
	certificate, pool := testCertificate(t)
	return &tls.Config{Certificates: []tls.Certificate{certificate}}, &tls.Config{ServerName: "localhost", RootCAs: pool}
}

type echo struct {
	opened chan uio.Conn
	data   chan []byte
	closed chan uio.Conn
}

func newEcho() *echo {
	return &echo{opened: make(chan uio.Conn, 8), data: make(chan []byte, 64), closed: make(chan uio.Conn, 8)}
}

func (e *echo) OnOpen(c uio.Conn) {
	e.opened <- c
}

func (e *echo) OnRead(c uio.Conn) error {
	payload := append([]byte(nil), c.PeekChunk()...)
	if len(payload) == 0 {
		return nil
	}
	_, _ = c.Discard(len(payload))
	e.data <- payload
	_, err := c.Write(payload)
	return err
}

func (e *echo) OnTimer(uio.Conn) error {
	return nil
}

func (e *echo) OnClose(c uio.Conn, _ error) {
	e.closed <- c
}

func wait[T any](t *testing.T, c chan T, what string) T {
	t.Helper()
	select {
	case v := <-c:
		return v
	case <-time.After(testWait):
		t.Fatalf("no %s within %v", what, testWait)
		return *new(T)
	}
}

func serveTLS(t *testing.T, top uio.Handler, layers ...uio.Layer) *layeredEvents {
	t.Helper()
	return serveTLSWith(t, top, nil, layers...)
}

type layeredEvents struct {
	*uio.Events
	layers []uio.Layer
}

func (e *layeredEvents) Listen(addr string, opts ...uio.ListenOption) (uio.Listener, error) {
	if len(e.layers) > 0 {
		opts = append([]uio.ListenOption{uio.WithLayers(e.layers...)}, opts...)
	}
	return e.Events.Listen(addr, opts...)
}

func (e *layeredEvents) Dial(addr string, opts ...uio.DialOption) (uio.Conn, error) {
	return e.DialContext(context.Background(), addr, opts...)
}

func (e *layeredEvents) DialContext(ctx context.Context, addr string, opts ...uio.DialOption) (uio.Conn, error) {
	if len(e.layers) > 0 {
		opts = append([]uio.DialOption{uio.WithLayers(e.layers...)}, opts...)
	}
	return e.Events.DialContext(ctx, addr, opts...)
}

func serveTLSWith(t *testing.T, top uio.Handler, setup func(*uio.Events), layers ...uio.Layer) *layeredEvents {
	t.Helper()
	started := make(chan struct{})
	events := &uio.Events{Pollers: 2, Handler: top, Hooks: uio.FuncHandler{Start: func(*uio.Events) {
		close(started)
	}}}
	if setup != nil {
		setup(events)
	}
	done := make(chan error, 1)
	go func() {
		done <- events.Serve()
	}()
	t.Cleanup(func() {
		_ = events.Close(nil)
		select {
		case <-done:
		case <-time.After(cleanupWait):
			t.Error("Serve did not return")
		}
	})
	select {
	case <-started:
	case <-time.After(testWait):
		t.Fatal("Serve did not start")
	}
	return &layeredEvents{Events: events, layers: layers}
}

type record struct {
	opened chan uio.Conn
	data   chan []byte
	closed chan error
	turn   atomic.Pointer[chan struct{}]
}

func newRecord() *record {
	return &record{opened: make(chan uio.Conn, 4), data: make(chan []byte, 64), closed: make(chan error, 4)}
}

func (r *record) OnOpen(c uio.Conn) {
	r.opened <- c
}

func (r *record) OnRead(c uio.Conn) error {
	payload := append([]byte(nil), c.PeekChunk()...)
	if len(payload) == 0 {
		if turn := r.turn.Load(); turn != nil {
			select {
			case *turn <- struct{}{}:
			default:
			}
		}
		return nil
	}
	_, _ = c.Discard(len(payload))
	r.data <- payload
	return nil
}

func (r *record) OnTimer(uio.Conn) error {
	return nil
}

func (r *record) OnClose(_ uio.Conn, err error) {
	r.closed <- err
}

func dialTLS(t *testing.T, layers ...any) (uio.Conn, *record) {
	t.Helper()
	addr, _ := layers[len(layers)-1].(string)
	client := newRecord()
	started := make(chan struct{})
	events := &uio.Events{Pollers: 1, Handler: client, Hooks: uio.FuncHandler{Start: func(*uio.Events) {
		close(started)
	}}}
	var chain []uio.Layer
	for _, layer := range layers[:len(layers)-1] {
		chain = append(chain, layer.(uio.Layer))
	}
	done := make(chan error, 1)
	go func() {
		done <- events.Serve()
	}()
	t.Cleanup(func() {
		_ = events.Close(nil)
		<-done
	})
	select {
	case <-started:
	case <-time.After(testWait):
		t.Fatal("Serve did not start")
	}
	ctx, cancel := context.WithTimeout(context.Background(), testWait)
	defer cancel()
	conn, err := events.DialContext(ctx, addr, uio.WithLayers(chain...))
	if err != nil {
		t.Fatal(err)
	}
	return conn, client
}

func signal[T any](ch chan<- T, v T) {
	select {
	case ch <- v:
	default:
	}
}

func TestInvalidOptionsRefused(t *testing.T) {
	if err := (LayerOptions{}).Validate(); !errors.Is(err, ErrInvalidOptions) {
		t.Fatalf("Validate of empty options = %v, want ErrInvalidOptions", err)
	}
	layer, ok := ServerLayer(LayerOptions{}).(*tlsLayer)
	if !ok || !errors.Is(layer.err, ErrInvalidOptions) {
		t.Fatalf("layer error = %v, want ErrInvalidOptions", layer.err)
	}
	if err := (LayerOptions{TLSConfig: &Config{}}).Validate(); err != nil {
		t.Fatalf("Validate of a TLSConfig = %v, want nil", err)
	}
}

type windowedLowerLayer struct {
	limit   atomic.Int32
	written atomic.Int32
	failErr atomic.Pointer[error]
}

func (l *windowedLowerLayer) Attach(uio.LayerContext) uio.LayerHandler {
	return &windowedLowerHandler{parent: l}
}

type windowedLowerHandler struct {
	uio.PassThrough
	parent *windowedLowerLayer
}

func (h *windowedLowerHandler) OnWrite(ctx uio.LayerContext, p []byte) (int, error) {
	if pErr := h.parent.failErr.Load(); pErr != nil {
		return 0, *pErr
	}
	lim := int(h.parent.limit.Load())
	if lim == 0 {
		return 0, uio.ErrOutboundOverflow
	}
	if lim > 0 && len(p) > lim {
		n, err := ctx.Lower().Write(p[:lim])
		h.parent.limit.Store(0)
		return n, func() error {
			if err != nil {
				return err
			}
			return uio.ErrOutboundOverflow
		}()
	}
	h.parent.written.Add(int32(len(p)))
	return ctx.Lower().Write(p)
}

func TestUTLSOutboundOverflowStagingPreservesSession(t *testing.T) {
	srvCfg, cliCfg := testConfigs(t)
	wl := &windowedLowerLayer{}
	wl.limit.Store(-1) // unlimited during handshake

	gotData := make(chan []byte, 4)
	var acc bytes.Buffer
	transport := serveTLS(t, uio.FuncHandler{
		Read: func(c uio.Conn) error {
			_, _ = c.WriteTo(&acc)
			return nil
		},
		Close: func(uio.Conn, error) {
			gotData <- append([]byte(nil), acc.Bytes()...)
		},
	}, ServerLayer(LayerOptions{TLSConfig: srvCfg}))
	ln, err := transport.Listen("tcp://127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	addr := "tcp://" + ln.Addr().String()

	cli, _ := dialTLS(t, wl, ClientLayer(LayerOptions{TLSConfig: cliCfg}), addr)

	// Constrain lower layer to accept only 16 bytes before returning ErrOutboundOverflow!
	wl.limit.Store(16)
	msg1 := bytes.Repeat([]byte("A"), 256)
	if n, err := cli.Write(msg1); err != nil || n != len(msg1) {
		t.Fatalf("first TLS Write over full lower = %d, %v", n, err)
	}
	if cli.OutboundBuffered() == 0 {
		t.Fatal("expected staged outPending bytes to show in OutboundBuffered")
	}
	// While lower remains full (limit == 0), subsequent OnWrite returns ErrOutboundOverflow cleanly BEFORE encrypting!
	if _, err := cli.Write([]byte("second")); !errors.Is(err, uio.ErrOutboundOverflow) {
		t.Fatalf("second Write while outPending full = %v, want ErrOutboundOverflow", err)
	}

	// Reopen lower window and trigger a Wake turn so OnTurnEnd drains outPending and subsequent Write + Close succeed!
	wl.limit.Store(-1)
	_ = cli.Wake()
	for deadline := time.Now().Add(time.Second); cli.OutboundBuffered() > 0 && time.Now().Before(deadline); {
		time.Sleep(time.Millisecond)
	}
	msg2 := bytes.Repeat([]byte("B"), 128)
	if n, err := cli.Write(msg2); err != nil || n != len(msg2) {
		t.Fatalf("post-recovery TLS Write = %d, %v", n, err)
	}
	_ = cli.Close()

	select {
	case received := <-gotData:
		want := append(append([]byte(nil), msg1...), msg2...)
		if !bytes.Equal(received, want) {
			t.Fatalf("received %d bytes, want %d", len(received), len(want))
		}
	case <-time.After(testWait):
		t.Fatal("server did not receive recovered TLS stream")
	}

	// Also exercise fatal non-overflow error during flushOutPendingLocked!
	wlFail := &windowedLowerLayer{}
	wlFail.limit.Store(-1)
	cli2, _ := dialTLS(t, wlFail, ClientLayer(LayerOptions{TLSConfig: cliCfg}), addr)
	wlFail.limit.Store(0)
	_, _ = cli2.Write(bytes.Repeat([]byte("Z"), 70<<10))
	boom := errors.New("lower broken")
	wlFail.failErr.Store(&boom)
	if _, err := cli2.Write([]byte("fail")); !errors.Is(err, boom) {
		t.Fatalf("fatal lower error = %v, want %v", err, boom)
	}
	_ = cli2.Abort(boom)
}
