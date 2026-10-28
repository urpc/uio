package uio

import (
	"bytes"
	"errors"
	"io"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestUnflushedErrorDescriptionAndCause(t *testing.T) {
	err := UnflushedError{Remaining: 7}
	if got, want := err.Error(), "uio: connection closed with unflushed data: 7 bytes"; got != want {
		t.Fatalf("Error() = %q, want %q", got, want)
	}
	if !errors.Is(err, ErrUnflushedData) {
		t.Fatal("UnflushedError does not unwrap to ErrUnflushedData")
	}
}

func TestCommonConnMetadataAndUnsupportedDeadlines(t *testing.T) {
	local := &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1}
	remote := &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 2}
	conn := &commonConn{addr: &addrPair{local: local}, remoteAddr: remoteAddrFrom(remote)}
	conn.entry = bareEntry()
	conn.SetUserdata("userdata")
	if conn.LocalAddr() != local || conn.Userdata() != "userdata" {
		t.Fatal("connection metadata was not preserved")
	}
	if got := conn.remoteAddr; got != remoteAddrFrom(remote) {
		t.Fatalf("remoteAddr = %v, want %v", got, remoteAddrFrom(remote))
	}
	for name, set := range map[string]func() error{
		"deadline":       func() error { return conn.SetDeadline(time.Now()) },
		"read deadline":  func() error { return conn.SetReadDeadline(time.Now()) },
		"write deadline": func() error { return conn.SetWriteDeadline(time.Now()) },
	} {
		t.Run(name, func(t *testing.T) {
			if err := set(); !errors.Is(err, ErrUnsupported) {
				t.Fatalf("error = %v, want %v", err, ErrUnsupported)
			}
		})
	}
}

func TestCommonConnReadAndWriteToAcrossBuffers(t *testing.T) {
	conn := &commonConn{inboundTail: []byte("cd")}
	conn.entry = bareEntry()
	_, _ = conn.inbound.WriteString("ab")
	var dst bytes.Buffer
	n, err := conn.WriteTo(&dst)
	if err != nil || n != 4 || dst.String() != "abcd" {
		t.Fatalf("WriteTo = %d, %v, %q", n, err, dst.String())
	}
	if conn.InboundBuffered() != 0 {
		t.Fatalf("buffered after WriteTo = %d", conn.InboundBuffered())
	}

	conn.inboundTail = []byte("cd")
	_, _ = conn.inbound.WriteString("ab")
	if n, err = conn.WriteTo(failingWriter{}); n != 0 || !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("failing WriteTo = %d, %v", n, err)
	}
	if got := string(conn.inboundTail); got != "cd" {
		t.Fatalf("tail after failed WriteTo = %q", got)
	}

	conn = &commonConn{inboundTail: []byte("cd")}
	conn.entry = bareEntry()
	_, _ = conn.inbound.WriteString("ab")
	buffer := make([]byte, 3)
	if n, err := conn.Read(buffer); err != nil || n != 3 || string(buffer) != "abc" {
		t.Fatalf("Read = %d, %v, %q", n, err, buffer)
	}
	if n, err := conn.Read(buffer[:1]); err != nil || n != 1 || buffer[0] != 'd' {
		t.Fatalf("tail Read = %d, %v, %q", n, err, buffer[:1])
	}

	conn = &commonConn{}
	conn.entry = bareEntry()
	_, _ = conn.inbound.WriteString("ab")
	if n, err := conn.Read(buffer[:1]); err != nil || n != 1 || buffer[0] != 'a' {
		t.Fatalf("buffer-only Read = %d, %v, %q", n, err, buffer[:1])
	}
}

func TestCommonConnPeekVariants(t *testing.T) {
	conn := &commonConn{inboundTail: []byte("tail")}
	conn.entry = bareEntry()
	if got := conn.Peek(nil); got != nil {
		t.Fatalf("Peek(nil) = %q", got)
	}
	if got := (&commonConn{}).Peek(make([]byte, 1)); got != nil {
		t.Fatalf("empty Peek = %q", got)
	}
	if got := string(conn.Peek(make([]byte, 2))); got != "ta" {
		t.Fatalf("tail Peek = %q", got)
	}
	if got := string(conn.PeekChunk()); got != "tail" {
		t.Fatalf("tail PeekChunk = %q", got)
	}

	conn = &commonConn{inboundTail: []byte("cd")}
	conn.entry = bareEntry()
	_, _ = conn.inbound.WriteString("ab")
	buffer := make([]byte, 4)
	if got := string(conn.Peek(buffer)); got != "abcd" {
		t.Fatalf("combined Peek = %q", got)
	}
	if got := string(conn.Peek(buffer[:1])); got != "a" {
		t.Fatalf("buffer-only Peek = %q", got)
	}
	if got := string(conn.PeekChunk()); got != "ab" {
		t.Fatalf("buffer-first PeekChunk = %q", got)
	}
	if got := (&commonConn{}).PeekChunk(); got != nil {
		t.Fatalf("empty PeekChunk = %q", got)
	}
}

func TestCommonConnDiscardVariants(t *testing.T) {
	tests := []struct {
		name      string
		buffered  string
		tail      string
		discard   int
		wantN     int
		wantBytes string
	}{
		{name: "zero", tail: "ab", discard: 0, wantBytes: "ab"},
		{name: "tail only", tail: "abc", discard: 1, wantN: 1, wantBytes: "bc"},
		{name: "within buffer", buffered: "abc", tail: "de", discard: 2, wantN: 2, wantBytes: "cde"},
		{name: "across buffers", buffered: "abc", tail: "de", discard: 4, wantN: 4, wantBytes: "e"},
		{name: "all", buffered: "abc", tail: "de", discard: -1, wantN: 5},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			conn := &commonConn{inboundTail: []byte(test.tail)}
			conn.entry = bareEntry()
			_, _ = conn.inbound.WriteString(test.buffered)
			n, err := conn.Discard(test.discard)
			if err != nil || n != test.wantN {
				t.Fatalf("Discard = %d, %v, want %d, nil", n, err, test.wantN)
			}
			var remaining bytes.Buffer
			_, _ = conn.WriteTo(&remaining)
			if remaining.String() != test.wantBytes {
				t.Fatalf("remaining = %q, want %q", remaining.String(), test.wantBytes)
			}
		})
	}
}

// Two uio endpoints that both write faster than they read must still deliver
// everything both ways. Each end's read rounds pause at their budget while its
// own output fills the socket; a paused read that waited for writability would
// wait for a peer waiting on it in turn, with both directions' buffers full.
func TestBidirectionalBulkTransferBetweenPeers(t *testing.T) {
	const (
		total = 64 << 20
		chunk = 64 << 10
	)
	var serverReceived, clientReceived atomic.Int64
	writeErrs := make(chan error, 2)
	payload := make([]byte, chunk)
	newEvents := func(received *atomic.Int64, started chan<- struct{}) *Events {
		return withFuncs(&Events{
			Pollers:       1,
			MaxBufferSize: chunk,
		}, FuncHandler{
			Start: func(*Events) { close(started) },
			Open: func(conn Conn) {
				go func() {
					for sent := 0; sent < total; sent += chunk {
						if _, err := conn.Write(payload); err != nil {
							writeErrs <- err
							return
						}
					}
				}()
			},
			Read: func(conn Conn) error {
				n, _ := conn.Discard(conn.InboundBuffered())
				received.Add(int64(n))
				return nil
			},
		})
	}
	serverStarted, clientStarted := make(chan struct{}), make(chan struct{})
	server := newEvents(&serverReceived, serverStarted)
	client := newEvents(&clientReceived, clientStarted)
	addr, serverDone := serveLoopback(t, server)
	clientDone := make(chan error, 1)
	go func() { clientDone <- client.Serve() }()
	t.Cleanup(func() {
		_ = client.Close(nil)
		_ = server.Close(nil)
		<-clientDone
		<-serverDone
	})
	select {
	case <-clientStarted:
	case err := <-clientDone:
		t.Fatalf("client Serve: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("event loops did not start")
	}
	if _, err := client.Dial("tcp://" + addr); err != nil {
		t.Fatal(err)
	}

	// A slow machine may move bytes slowly; only a transfer that stops moving
	// altogether fails.
	last, lastMoved := int64(-1), time.Now()
	for {
		select {
		case err := <-writeErrs:
			t.Fatalf("write failed: %v", err)
		default:
		}
		s, c := serverReceived.Load(), clientReceived.Load()
		if s == total && c == total {
			return
		}
		if s+c != last {
			last, lastMoved = s+c, time.Now()
		} else if time.Since(lastMoved) > 10*time.Second {
			t.Fatalf("transfer stopped: server received %d and client %d of %d bytes each", s, c, total)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestUserdataMayBeSerializedOutsideCallback(t *testing.T) {
	conn := &commonConn{loop: &eventLoop{}}
	conn.entry = bareEntry()
	conn.SetUserdata("value")
	if conn.Userdata() != "value" {
		t.Fatal("userdata was not available outside callback")
	}
}

// BenchmarkInboundAccessAssertion measures one inbound access on the
// connection's own callback path. In race-enabled builds it includes the
// scope diagnostic; without the race detector the diagnostic is compiled out
// and this is the access's own cost.
func BenchmarkInboundAccessAssertion(b *testing.B) {
	loop := &eventLoop{}
	conn := &commonConn{loop: loop, entry: &entry{ev: &Events{}}}
	b.Run("current-callback", func(b *testing.B) {
		started := conn.beginInboundCallback()
		for b.Loop() {
			conn.InboundBuffered()
		}
		conn.endInboundCallback(started)
	})
}

// OnOpen may Dial on either backend: it never runs on an event loop.
func TestDialFromOnOpen(t *testing.T) {
	target, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	go func() {
		for {
			c, err := target.Accept()
			if err != nil {
				return
			}
			go func() { _, _ = io.Copy(io.Discard, c); _ = c.Close() }()
		}
	}()
	result := make(chan error, 1)
	events := &Events{Pollers: 1}
	funcs(events).Open = func(c Conn) {
		if c.Userdata() != nil {
			return // the dialed connection's own OnOpen
		}
		dialed, err := events.Dial("tcp://"+target.Addr().String(), WithUserdata("dialed"))
		if dialed != nil {
			_ = dialed.Close()
		}
		result <- err
	}
	serveInboundTest(t, events)
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("Dial from OnOpen: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Dial from OnOpen did not return")
	}
}

// A connection whose OnOpen blocks holds up no other connection of the same
// loop: one accepted after it opens and is served meanwhile.
func TestSlowOnOpenDoesNotStallTheLoop(t *testing.T) {
	release := make(chan struct{})
	var accepted atomic.Int32
	events := withFuncs(&Events{
		Pollers: 1,
	}, FuncHandler{
		Open: func(c Conn) {
			if accepted.Add(1) == 2 {
				<-release // the second accepted connection's OnOpen blocks
			}
		},
		Read: func(c Conn) error {
			_, err := c.WriteTo(c)
			return err
		},
	})
	first, _ := serveInboundTest(t, events)
	defer close(release)
	addr := first.RemoteAddr().String()
	slow, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer slow.Close()
	for accepted.Load() < 2 {
		time.Sleep(time.Millisecond)
	}
	fast, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer fast.Close()
	if _, err := fast.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	_ = fast.SetReadDeadline(time.Now().Add(3 * time.Second))
	got := make([]byte, 4)
	if _, err := io.ReadFull(fast, got); err != nil || string(got) != "ping" {
		t.Fatalf("a connection echoed %q, %v while another's OnOpen blocked", got, err)
	}
}

// A Wake made before OnOpen has run, as on a connection Dial has just
// returned, runs its OnData after OnOpen, never before it.
func TestWakeBeforeOnOpenRunsAfterIt(t *testing.T) {
	var mu sync.Mutex
	var seq []string
	note := func(s string) {
		mu.Lock()
		seq = append(seq, s)
		mu.Unlock()
	}
	woke := make(chan struct{}, 1)
	events := withFuncs(&Events{
		Pollers: 1,
	}, FuncHandler{
		Open: func(c Conn) {
			if c.Userdata() == "dialed" {
				time.Sleep(50 * time.Millisecond)
				note("open")
			}
		},
		Read: func(c Conn) error {
			if c.Userdata() == "dialed" {
				note("data")
				select {
				case woke <- struct{}{}:
				default:
				}
			}
			return nil
		},
	})
	serveInboundTest(t, events)
	target, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	go func() {
		c, err := target.Accept()
		if err == nil {
			defer c.Close()
			time.Sleep(2 * time.Second)
		}
	}()
	dialed, err := events.Dial("tcp://"+target.Addr().String(), WithUserdata("dialed"))
	if err != nil {
		t.Fatal(err)
	}
	if err := dialed.Wake(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-woke:
	case <-time.After(3 * time.Second):
		t.Fatal("the Wake never ran OnData")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seq) < 2 || seq[0] != "open" || seq[1] != "data" {
		t.Fatalf("callbacks ran as %v, want OnOpen before the Wake's OnData", seq)
	}
}

// A connection closed right after Dial returns, possibly before its OnOpen,
// never gets OnClose without OnOpen.
func TestCloseBeforeOnOpenSkipsOnClose(t *testing.T) {
	target, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	go func() {
		for {
			c, err := target.Accept()
			if err != nil {
				return
			}
			go func() { _, _ = io.Copy(io.Discard, c); _ = c.Close() }()
		}
	}()
	var mu sync.Mutex
	opened := map[Conn]bool{}
	var orphans, closes atomic.Int32
	events := withFuncs(&Events{
		Pollers: 1,
	}, FuncHandler{
		Open: func(c Conn) {
			mu.Lock()
			opened[c] = true
			mu.Unlock()
		},
		Close: func(c Conn, _ error) {
			closes.Add(1)
			mu.Lock()
			if !opened[c] {
				orphans.Add(1)
			}
			mu.Unlock()
		},
	})
	serveInboundTest(t, events)
	for range 200 {
		c, err := events.Dial("tcp://"+target.Addr().String(), WithUserdata("dialed"))
		if err != nil {
			t.Fatal(err)
		}
		_ = c.Close()
	}
	time.Sleep(200 * time.Millisecond)
	if n := orphans.Load(); n != 0 {
		t.Fatalf("%d of %d closes reached a connection that never got OnOpen", n, closes.Load())
	}
}

// A read deadline on one UDP server child ends that child alone: the server
// and its other peers keep running. The children share their server's
// socket, so a deadline that reached the socket would end them all.
func TestUDPChildReadDeadlineClosesOnlyThatChild(t *testing.T) {
	probe, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := probe.LocalAddr().String()
	_ = probe.Close()
	type closeEvent struct {
		peer string
		err  error
	}
	closed := make(chan closeEvent, 4)
	started := make(chan struct{})
	events := withFuncs(&Events{
		Pollers: 1,
	}, FuncHandler{
		Start: func(*Events) { close(started) },
		Read: func(c Conn) error {
			payload := c.Peek(make([]byte, c.InboundBuffered()))
			if string(payload) == "expire" {
				if err := c.SetReadDeadline(time.Now().Add(50 * time.Millisecond)); err != nil {
					return err
				}
			}
			_, err := c.WriteTo(c)
			return err
		},
		Close: func(c Conn, err error) { signal(closed, closeEvent{c.RemoteAddr().String(), err}) },
	})
	done := make(chan error, 1)
	go func() { done <- events.Serve("udp://" + addr) }()
	select {
	case <-started:
	case err := <-done:
		t.Fatalf("Serve: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("server did not start")
	}
	t.Cleanup(func() {
		_ = events.Close(nil)
		<-done
	})
	echo := func(c net.Conn, payload string) {
		t.Helper()
		if _, err := c.Write([]byte(payload)); err != nil {
			t.Fatal(err)
		}
		_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
		buf := make([]byte, 64)
		n, err := c.Read(buf)
		if err != nil || string(buf[:n]) != payload {
			t.Fatalf("echo of %q = %q, %v", payload, buf[:n], err)
		}
	}
	expiring, err := net.Dial("udp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer expiring.Close()
	staying, err := net.Dial("udp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer staying.Close()
	echo(staying, "hello")
	echo(expiring, "expire")
	select {
	case event := <-closed:
		if event.peer != expiring.LocalAddr().String() {
			t.Fatalf("the deadline closed %s, want only %s", event.peer, expiring.LocalAddr())
		}
		if !errors.Is(event.err, os.ErrDeadlineExceeded) {
			t.Fatalf("OnClose error = %v, want a deadline", event.err)
		}
	case err := <-done:
		t.Fatalf("a child's read deadline stopped the server: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("the read deadline never closed its child")
	}
	// The other peer and the server outlive it.
	time.Sleep(100 * time.Millisecond)
	echo(staying, "still here")
	select {
	case event := <-closed:
		t.Fatalf("another connection closed too: %s: %v", event.peer, event.err)
	case err := <-done:
		t.Fatalf("the server stopped: %v", err)
	default:
	}
}

// A UDP server's child connection opens when its first datagram arrives and,
// having had OnOpen, gets OnClose when it closes.
func TestUDPChildGetsOnCloseAfterOnOpen(t *testing.T) {
	probe, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := probe.LocalAddr().String()
	_ = probe.Close()
	opened := make(chan struct{}, 1)
	closed := make(chan struct{}, 1)
	started := make(chan struct{})
	events := withFuncs(&Events{
		Pollers: 1,
	}, FuncHandler{
		Start: func(*Events) { close(started) },
		Open:  func(Conn) { signal(opened, struct{}{}) },
		Read: func(c Conn) error {
			_, _ = c.Discard(-1)
			return c.Close()
		},
		Close: func(Conn, error) { signal(closed, struct{}{}) },
	})
	done := make(chan error, 1)
	go func() { done <- events.Serve("udp://" + addr) }()
	<-started
	t.Cleanup(func() {
		_ = events.Close(nil)
		<-done
	})
	client, err := net.Dial("udp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if _, err := client.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	for _, step := range []struct {
		name string
		ch   chan struct{}
	}{{"OnOpen", opened}, {"OnClose", closed}} {
		select {
		case <-step.ch:
		case <-time.After(2 * time.Second):
			t.Fatalf("the UDP child got no %s", step.name)
		}
	}
}
