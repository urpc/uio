package utls

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urpc/uio"
)

// zeroes reads as zeros, forever.
type zeroes struct{}

func (zeroes) Read(b []byte) (int, error) {
	for i := range b {
		b[i] = 0
	}
	return len(b), nil
}

// The staging bound stills the wire: a stack whose top reads nothing holds
// its bound, the bytes below reach the transport's inbound limit, and a
// plain crypto/tls sender stops completing writes — until the top reads.
func TestStagingBoundStillsTheWire(t *testing.T) {
	serverConfig, clientConfig := testConfigs(t)
	release := make(chan struct{})
	var stalled atomic.Int64
	var top atomic.Pointer[uio.Conn]
	started := make(chan struct{})
	events := &uio.Events{Pollers: 2, MaxInboundBuffered: 32 << 10, Hooks: uio.FuncHandler{
		Start: func(*uio.Events) { close(started) },
	}}
	events.Handler = uio.FuncHandler{
		Open: func(c uio.Conn) {
			_ = c.SetReadBuffer(32 << 10) // the staging bound, not the kernel's
			top.Store(&c)
		},
		Read: func(c uio.Conn) error {
			select {
			case <-release:
				_, _ = c.Discard(c.InboundBuffered())
			default:
				stalled.Store(int64(c.InboundBuffered())) // what the layer staged for the handler
			}
			return nil
		},
		Close: func(uio.Conn, error) {},
	}
	events.Use(Server(serverConfig, StagingBound(16<<10)))
	done := make(chan error, 1)
	go func() { done <- events.Serve() }()
	t.Cleanup(func() { _ = events.Close(nil); <-done })
	select {
	case <-started:
	case <-time.After(testWait):
		t.Fatal("Serve did not start")
	}
	listener, err := events.Listen("tcp://127.0.0.1:0", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()

	client, err := tls.Dial("tcp", listener.Addr().String(), clientConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	// Small socket buffers on both ends, so the staging bound and the
	// transport's inbound limit are what hold the sender, not the kernel's
	// loopback buffers.
	if tcp, ok := client.NetConn().(*net.TCPConn); ok {
		_ = tcp.SetWriteBuffer(32 << 10)
		_ = tcp.SetReadBuffer(32 << 10)
	}
	const total = 4 << 20
	written := make(chan error, 1)
	go func() {
		_, err := io.CopyN(client, zeroes{}, total)
		written <- err
	}()
	deadline := time.Now().Add(testWait)
	for stalled.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if stalled.Load() == 0 {
		t.Fatal("the stack never held bytes below its layer")
	}
	select {
	case err := <-written:
		t.Fatalf("the sender finished with the stack stalled: %v", err)
	case <-time.After(time.Second):
	}
	if held := stalled.Load(); held > int64((16<<10)+(32<<10)+(64<<10)) {
		t.Fatalf("the layer held %d bytes, far past its bound", held)
	}

	close(release) // the reader above reads again, and its round needs a wake
	if err := (*top.Load()).Wake(); err != nil {
		t.Fatalf("wake the stalled stack: %v", err)
	}
	if err := <-written; err != nil {
		t.Fatalf("the sender failed after the release: %v", err)
	}
}

// A close sends the peer close_notify; everything accepted before it
// arrives, and the peer that shut its writing half is read to its end.
func TestCloseAndHalfClose(t *testing.T) {
	t.Run("close", func(t *testing.T) {
		serverConfig, clientConfig := testConfigs(t)
		server := newEcho()
		transport := serveTLS(t, server, Server(serverConfig))
		listener, err := transport.Listen("tcp://127.0.0.1:0", nil)
		if err != nil {
			t.Fatal(err)
		}
		conn, _ := dialTLS(t, Client(clientConfig), "tcp://"+listener.Addr().String())
		if _, err := conn.Write([]byte("last words")); err != nil {
			t.Fatal(err)
		}
		if got := string(wait(t, server.data, "server data")); got != "last words" {
			t.Fatalf("server read %q", got)
		}
		if err := conn.Close(); err != nil {
			t.Fatal(err)
		}
		wait(t, server.closed, "server close after close_notify")
	})

	t.Run("half close", func(t *testing.T) {
		serverConfig, clientConfig := testConfigs(t)
		server := newEcho()
		// The server stays open past the client's close_notify, as a socket
		// stays open past its peer's end of stream, only with AllowHalfOpen.
		transport := serveTLSWith(t, server, func(ev *uio.Events) { ev.AllowHalfOpen = true }, Server(serverConfig))
		listener, err := transport.Listen("tcp://127.0.0.1:0", nil)
		if err != nil {
			t.Fatal(err)
		}
		conn, client := dialTLS(t, Client(clientConfig), "tcp://"+listener.Addr().String())
		if _, err := conn.Write([]byte("one way")); err != nil {
			t.Fatal(err)
		}
		if err := conn.CloseWrite(); err != nil {
			t.Fatal(err)
		}
		if got := string(wait(t, server.data, "server data")); got != "one way" {
			t.Fatalf("server read %q", got)
		}
		// The server's echo of what the client sent, and its own answer,
		// both arrive after the client's writing half ended.
		serverConn := wait(t, server.opened, "server open")
		if _, err := serverConn.Write([]byte("answer")); err != nil {
			t.Fatal(err)
		}
		var got []byte
		for len(got) < len("one wayanswer") {
			select {
			case chunk := <-client.data:
				got = append(got, chunk...)
			case <-time.After(testWait):
				t.Fatalf("client read %q after CloseWrite, want the echo and the answer", got)
			}
		}
		if string(got) != "one wayanswer" {
			t.Fatalf("client read %q after CloseWrite, want the echo then the answer", got)
		}
		// A half-open server closes once it ends its own half too.
		if err := serverConn.CloseWrite(); err != nil {
			t.Fatal(err)
		}
		wait(t, server.closed, "server close")
	})
}

// A close from below keeps its cause: an abrupt end reports the error it
// ended with, not a close_notify's clean finish.
func TestAbruptCloseKeepsItsCause(t *testing.T) {
	serverConfig, clientConfig := testConfigs(t)
	sentinel := errors.New("connection cut")
	server := newEcho()
	transport := serveTLS(t, server, Server(serverConfig))
	listener, err := transport.Listen("tcp://127.0.0.1:0", nil)
	if err != nil {
		t.Fatal(err)
	}

	closed := make(chan error, 1)
	started := make(chan struct{})
	events := &uio.Events{Pollers: 1, Hooks: uio.FuncHandler{Start: func(*uio.Events) { close(started) }}}
	events.Handler = uio.FuncHandler{
		Open:  func(uio.Conn) {},
		Read:  func(uio.Conn) error { return nil },
		Close: func(_ uio.Conn, err error) { closed <- err },
	}
	events.Use(Client(clientConfig))
	done := make(chan error, 1)
	go func() { done <- events.Serve() }()
	t.Cleanup(func() { _ = events.Close(nil); <-done })
	<-started
	raw, err := events.Dial("tcp://"+listener.Addr().String(), nil)
	if err != nil {
		t.Fatal(err)
	}
	// The stack settles first, then the wire is cut under it.
	time.Sleep(100 * time.Millisecond)
	_ = raw.CloseWith(sentinel)
	select {
	case err := <-closed:
		if !errors.Is(err, sentinel) {
			t.Fatalf("closed with %v, want the cause the close named", err)
		}
	case <-time.After(testWait):
		t.Fatal("the stack never closed")
	}
	wait(t, server.closed, "server close")
}

// A handshake timeout ends the connection with the timeout's cause, and the
// dialing side reports it without running a callback.
func TestHandshakeTimeout(t *testing.T) {
	serverConfig, _ := testConfigs(t)
	server := newEcho()
	transport := serveTLS(t, server, Server(serverConfig, HandshakeTimeout(150*time.Millisecond)))
	listener, err := transport.Listen("tcp://127.0.0.1:0", nil)
	if err != nil {
		t.Fatal(err)
	}
	// A peer that connects and says nothing.
	silent, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer silent.Close()
	// The timeout closes the connection: the silent peer reads its end. The
	// handler above never opened, so it hears nothing of the connection.
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

// A dial cut short by its context reports the deadline and runs no callback
// of the stack's handler.
func TestDialCanceledMidHandshake(t *testing.T) {
	// A server that accepts and never answers.
	silent, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer silent.Close()
	go func() {
		for {
			c, err := silent.Accept()
			if err != nil {
				return
			}
			_ = c // held open by nothing but the kernel: the peer says nothing
		}
	}()

	var mu sync.Mutex
	calls := 0
	started := make(chan struct{})
	events := &uio.Events{Pollers: 1, Hooks: uio.FuncHandler{Start: func(*uio.Events) { close(started) }}}
	events.Handler = uio.FuncHandler{
		Open:  func(uio.Conn) { mu.Lock(); calls++; mu.Unlock() },
		Read:  func(uio.Conn) error { return nil },
		Close: func(uio.Conn, error) { mu.Lock(); calls++; mu.Unlock() },
	}
	events.Use(Client(&tls.Config{InsecureSkipVerify: true}))
	done := make(chan error, 1)
	go func() { done <- events.Serve() }()
	t.Cleanup(func() { _ = events.Close(nil); <-done })
	<-started
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err = events.DialContext(ctx, "tcp://"+silent.Addr().String(), nil)
	// The deadline is what asked for the close; a transport error that beat
	// it to the report tells the same story.
	if err == nil {
		t.Fatal("DialContext of a stalled handshake returned no error")
	}
	mu.Lock()
	got := calls
	mu.Unlock()
	if got != 0 {
		t.Fatalf("a canceled dial ran %d callbacks of the stack's handler", got)
	}
}

// A peer's close_notify is the end of its stream, passed up: without
// AllowHalfOpen it closes the connection with io.EOF once the handler read
// what came before it; with AllowHalfOpen the handler reads io.EOF and goes
// on writing.
func TestCloseNotifyEndsTheStream(t *testing.T) {
	serverConfig, clientConfig := testConfigs(t)
	for _, halfOpen := range []bool{false, true} {
		t.Run(map[bool]string{false: "closes", true: "half open"}[halfOpen], func(t *testing.T) {
			ended := make(chan error, 4)
			closed := make(chan error, 1)
			opened := make(chan uio.Conn, 1)
			top := uio.FuncHandler{
				Open: func(c uio.Conn) { opened <- c },
				Read: func(c uio.Conn) error {
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
				},
				Close: func(_ uio.Conn, err error) { closed <- err },
			}
			transport := serveTLSWith(t, top, func(ev *uio.Events) { ev.AllowHalfOpen = halfOpen }, Server(serverConfig))
			listener, err := transport.Listen("tcp://127.0.0.1:0", nil)
			if err != nil {
				t.Fatal(err)
			}
			conn, client := dialTLS(t, Client(clientConfig), "tcp://"+listener.Addr().String())
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

// A layer under TLS that holds its output until flushed: the handshake's
// flights leave at the end of the turns that wrote them, and the dial
// settles.
func TestHandshakeOverAHoldingLayer(t *testing.T) {
	serverConfig, clientConfig := testConfigs(t)
	server := newEcho()
	transport := serveTLS(t, server, Server(serverConfig))
	listener, err := transport.Listen("tcp://127.0.0.1:0", nil)
	if err != nil {
		t.Fatal(err)
	}
	conn, client := dialTLS(t,
		uio.LayerFunc(func(uio.LayerContext) uio.LayerHandler { return &holdUntilFlush{} }),
		Client(clientConfig), "tcp://"+listener.Addr().String())
	if _, err := conn.Write([]byte("through the hold")); err != nil {
		t.Fatal(err)
	}
	_ = conn.Flush()
	if got := string(wait(t, client.data, "the echo")); got != "through the hold" {
		t.Fatalf("echo = %q", got)
	}
}

// holdUntilFlush holds what is written to it until Flush.
type holdUntilFlush struct {
	uio.PassThrough
	mu   sync.Mutex
	held []byte
}

func (h *holdUntilFlush) OnWrite(_ uio.LayerContext, p []byte) (int, error) {
	h.mu.Lock()
	h.held = append(h.held, p...)
	h.mu.Unlock()
	return len(p), nil
}

func (h *holdUntilFlush) OutboundHeld(uio.LayerContext) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.held)
}

func (h *holdUntilFlush) Flush(ctx uio.LayerContext) error {
	h.mu.Lock()
	held := h.held
	h.held = nil
	h.mu.Unlock()
	if len(held) == 0 {
		return nil
	}
	_, err := ctx.Down().Write(held)
	return err
}

// A peer that floods a handshake cannot make the layer buffer it: the
// handshake reads what arrives as it arrives — it waits only once nothing
// is left unread — and crypto/tls bounds a handshake message, so the flood
// ends the handshake long before the timeout, having been held one read at
// a time.
func TestHandshakeFloodIsNotBuffered(t *testing.T) {
	serverConfig, _ := testConfigs(t)
	server := newEcho()
	transport := serveTLS(t, server, Server(serverConfig, HandshakeTimeout(10*time.Second)))
	listener, err := transport.Listen("tcp://127.0.0.1:0", nil)
	if err != nil {
		t.Fatal(err)
	}
	peer, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	// Handshake records that declare one ClientHello as large as crypto/tls
	// allows, and then go on past it.
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
		record[5], record[6], record[7], record[8] = 0, 0, 0, 0 // fragments after the first
	}
	if sent > 64<<20 {
		t.Fatalf("the server took %d MiB of a handshake flood", sent>>20)
	}
	if took := time.Since(began); took > 4*time.Second {
		t.Fatalf("the flood ran %v: the server kept reading it", took)
	}
}

// A staging bound far below a handshake flight still lets the handshake
// through: the layer takes what it has room for, the handshake reads it, and
// the turn comes back for the rest.
func TestHandshakeWithATinyStagingBound(t *testing.T) {
	serverConfig, clientConfig := testConfigs(t)
	server := newEcho()
	transport := serveTLS(t, server, Server(serverConfig, StagingBound(512)))
	listener, err := transport.Listen("tcp://127.0.0.1:0", nil)
	if err != nil {
		t.Fatal(err)
	}
	conn, client := dialTLS(t, Client(clientConfig, StagingBound(512)), "tcp://"+listener.Addr().String())
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

// The least staging bound still handshakes promptly: the handshake takes a
// whole record at a time whatever the bound.
func TestHandshakeWithTheLeastStagingBound(t *testing.T) {
	serverConfig, clientConfig := testConfigs(t)
	server := newEcho()
	transport := serveTLS(t, server, Server(serverConfig, StagingBound(1)))
	listener, err := transport.Listen("tcp://127.0.0.1:0", nil)
	if err != nil {
		t.Fatal(err)
	}
	began := time.Now()
	conn, client := dialTLS(t, Client(clientConfig, StagingBound(1)), "tcp://"+listener.Addr().String())
	if took := time.Since(began); took > time.Second {
		t.Fatalf("the handshake took %v", took)
	}
	if _, err := conn.Write([]byte("tiny")); err != nil {
		t.Fatal(err)
	}
	if got := string(wait(t, client.data, "the echo")); got != "tiny" {
		t.Fatalf("echo = %q", got)
	}
}

// The stream below ending without a close_notify still reaches the handler
// of a half-open connection: at a record's edge as the end of the stream,
// read as io.EOF; inside a record as a truncation, which closes the
// connection with an error.
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
					_, _ = raw.Write([]byte{23, 3, 3, 0, 40, 1, 2, 3}) // a record's start
				}
				_ = raw.CloseWrite() // no close_notify
				<-release
			}()
			ended := make(chan error, 4)
			handler := uio.FuncHandler{
				Read: func(c uio.Conn) error {
					if _, err := c.Read(nil); err != nil {
						ended <- err
					}
					return nil
				},
				Close: func(_ uio.Conn, err error) { ended <- err },
			}
			events := serveTLSWith(t, handler, func(ev *uio.Events) { ev.AllowHalfOpen = true }, Client(clientConfig))
			ctx, cancel := context.WithTimeout(context.Background(), testWait)
			defer cancel()
			if _, err := events.DialContext(ctx, "tcp://"+listener.Addr().String(), nil); err != nil {
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

// State during a handshake that waits for the peer reports the zero state
// at once, and the connection still closes: asking crypto/tls would wait for
// the handshake's lock, which the suspended handshake holds.
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
		time.Sleep(testWait) // never answers
	}()
	attached := make(chan *conn, 1)
	layer := Client(clientConfig)
	events := serveTLS(t, uio.FuncHandler{}, uio.LayerFunc(func(ctx uio.LayerContext) uio.LayerHandler {
		c := layer.Attach(ctx).(*conn)
		attached <- c
		return c
	}))
	ctx, cancel := context.WithCancel(context.Background())
	dialed := make(chan error, 1)
	go func() { _, err := events.DialContext(ctx, "tcp://"+listener.Addr().String(), nil); dialed <- err }()
	engine := <-attached
	<-gotHello
	stated := make(chan ConnectionState, 1)
	go func() { stated <- engine.State() }()
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

// The end of the stream below arriving with the last records, whose
// plaintext exactly fills the staging bound: once the handler reads it, the
// end follows without another wake — the layer stalled for it as it does
// for ciphertext still to read.
func TestEndBelowAtTheStagingBound(t *testing.T) {
	serverConfig, clientConfig := testConfigs(t)
	serverConfig.SessionTicketsDisabled = true
	serverConfig.DynamicRecordSizingDisabled = true
	listener, err := tls.Listen("tcp", "127.0.0.1:0", serverConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	opened := make(chan struct{})
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
		<-opened
		_, _ = tc.Write(bytes.Repeat([]byte{'x'}, DefaultStagingBound))
		_ = tc.NetConn().(*net.TCPConn).CloseWrite() // no close_notify
		<-release
	}()
	below := &endWithTheLast{}
	ended := make(chan error, 8)
	read := make(chan int, 8)
	handler := uio.FuncHandler{
		Open: func(uio.Conn) { below.active.Store(true); close(opened) },
		Read: func(c uio.Conn) error {
			if n, _ := c.Discard(-1); n > 0 {
				read <- n
			}
			if _, err := c.Read(nil); err != nil {
				ended <- err
			}
			return nil
		},
		Close: func(_ uio.Conn, err error) { ended <- err },
	}
	events := serveTLSWith(t, handler, func(ev *uio.Events) { ev.AllowHalfOpen = true },
		uio.LayerFunc(func(uio.LayerContext) uio.LayerHandler { return below }), Client(clientConfig))
	ctx, cancel := context.WithTimeout(context.Background(), testWait)
	defer cancel()
	if _, err := events.DialContext(ctx, "tcp://"+listener.Addr().String(), nil); err != nil {
		t.Fatal(err)
	}
	total := 0
	for total < DefaultStagingBound {
		total += wait(t, read, "the plaintext")
	}
	if total != DefaultStagingBound {
		t.Fatalf("read %d bytes, want exactly the staging bound", total)
	}
	if err := wait(t, ended, "the end of the stream"); err != io.EOF {
		t.Fatalf("the end reached the handler as %v, want io.EOF", err)
	}
}

// endWithTheLast passes the handshake through; once active, it holds what
// arrives and delivers it all, with the end, as the stream below ends.
type endWithTheLast struct {
	uio.PassThrough
	active atomic.Bool
	held   []byte
}

func (e *endWithTheLast) OnRead(ctx uio.LayerContext) error {
	if !e.active.Load() {
		return e.PassThrough.OnRead(ctx)
	}
	lower := ctx.Down()
	if chunk := lower.PeekChunk(); len(chunk) > 0 {
		e.held = append(e.held, chunk...)
		_, _ = lower.Discard(len(chunk))
	}
	if _, err := lower.Read(nil); err == io.EOF {
		// Deliver what was held in the chunks the delivery takes, and the
		// end after the last of it.
		n, _ := ctx.Up().Write(e.held)
		e.held = e.held[n:]
		if len(e.held) > 0 {
			ctx.Up().Yield()
			return nil
		}
		ctx.Up().End()
	}
	return nil
}
