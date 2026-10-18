package uio

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// serveInboundTest serves events on a fresh loopback port and returns a
// connected client, with the server's view of it once OnOpen has run.
func serveInboundTest(t *testing.T, events *Events) (net.Conn, Conn) {
	t.Helper()
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := probe.Addr().String()
	_ = probe.Close()

	opened := make(chan Conn, 1)
	onOpen := events.OnOpen
	events.OnOpen = func(c Conn) {
		if onOpen != nil {
			onOpen(c)
		}
		select { // only the first connection is returned
		case opened <- c:
		default:
		}
	}
	started := make(chan struct{})
	events.OnStart = func(*Events) { close(started) }
	done := make(chan error, 1)
	go func() { done <- events.Serve("tcp://" + addr) }()
	select {
	case <-started:
	case err := <-done:
		t.Fatalf("Serve: %v", err)
	}
	client, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = client.Close()
		_ = events.Close(nil)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("Serve did not return")
		}
	})
	select {
	case server := <-opened:
		return client, server
	case <-time.After(2 * time.Second):
		t.Fatal("connection did not open")
		return nil, nil
	}
}

// writeUntilStalled writes from the client until the kernel takes nothing for
// a while, and reports how much it took. With the server holding reads, this
// is the socket buffers' worth and the rest of total stays unsent.
func writeUntilStalled(t *testing.T, client net.Conn, total int) int {
	t.Helper()
	chunk := make([]byte, 16<<10)
	sent := 0
	for sent < total {
		_ = client.SetWriteDeadline(time.Now().Add(300 * time.Millisecond))
		n, err := client.Write(chunk[:min(len(chunk), total-sent)])
		sent += n
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				break
			}
			if isPeerGoneWrite(err) {
				break // the server closed, as a deadline test may
			}
			t.Fatal(err)
		}
	}
	_ = client.SetWriteDeadline(time.Time{})
	return sent
}

// A callback that consumes nothing holds the connection's reads once
// MaxInboundBuffered bytes are buffered: the buffer stops within one read of
// the limit while the peer's writes stall in TCP flow control, and the
// connection stays open.
func TestInboundLimitHoldsReadsInsteadOfClosing(t *testing.T) {
	const limit = 64 << 10
	var buffered atomic.Int64
	var closed atomic.Bool
	events := &Events{
		Pollers:            1,
		MaxBufferSize:      4 << 10,
		MaxInboundBuffered: limit,
		OnData: func(c Conn) error {
			buffered.Store(int64(c.InboundBuffered()))
			return nil
		},
		OnClose: func(Conn, error) { closed.Store(true) },
	}
	client, _ := serveInboundTest(t, events)

	sent := writeUntilStalled(t, client, 64<<20)
	if sent >= 64<<20 {
		t.Fatal("the peer was never slowed: reads were not held")
	}
	time.Sleep(100 * time.Millisecond)
	if got := buffered.Load(); got < limit || got > limit+4<<10 {
		t.Fatalf("buffered %d bytes, want [%d, %d]", got, limit, limit+4<<10)
	}
	if closed.Load() {
		t.Fatal("the connection closed instead of holding its reads")
	}
}

// Consuming below the limit in a Wake reads again, and the stream arrives
// whole and in order however often reads are held and resumed.
func TestInboundLimitResumesAfterWakeConsumes(t *testing.T) {
	const (
		limit = 32 << 10
		total = 8 << 20
	)
	var mu sync.Mutex
	var got bytes.Buffer
	consume := atomic.Bool{}
	events := &Events{
		Pollers:            1,
		MaxBufferSize:      4 << 10,
		MaxInboundBuffered: limit,
		OnData: func(c Conn) error {
			if !consume.Load() {
				return nil
			}
			mu.Lock()
			_, err := c.WriteTo(&got)
			mu.Unlock()
			return err
		},
	}
	client, server := serveInboundTest(t, events)

	want := make([]byte, total)
	for i := range want {
		want[i] = byte(i * 131)
	}
	writeDone := make(chan error, 1)
	go func() {
		_, err := client.Write(want)
		writeDone <- err
	}()
	deadline := time.Now().Add(20 * time.Second)
	for {
		// Alternate holding and consuming, so reads are held and resumed
		// many times over the transfer.
		time.Sleep(2 * time.Millisecond)
		consume.Store(true)
		if err := server.Wake(); err != nil {
			t.Fatal(err)
		}
		time.Sleep(2 * time.Millisecond)
		consume.Store(false)
		mu.Lock()
		n := got.Len()
		mu.Unlock()
		if n == total {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("received %d of %d bytes", n, total)
		}
	}
	if err := <-writeDone; err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if !bytes.Equal(got.Bytes(), want) {
		t.Fatal("the stream arrived altered")
	}
}

// The tunnel shape: a relay forwards one connection into another whose reader
// is slow, and holds what it cannot forward. Memory on both sides stays
// bounded, and every byte arrives in order.
func TestInboundLimitBoundsARelayToASlowReader(t *testing.T) {
	const (
		limit = 64 << 10
		total = 16 << 20
	)
	sinkLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer sinkLn.Close()

	var relayIn, relayOut atomic.Value // Conn
	var peakInbound, peakOutbound atomic.Int64
	note := func(peak *atomic.Int64, v int) {
		for {
			old := peak.Load()
			if int64(v) <= old || peak.CompareAndSwap(old, int64(v)) {
				return
			}
		}
	}
	events := &Events{
		Pollers:            1,
		MaxBufferSize:      4 << 10,
		MaxInboundBuffered: limit,
	}
	events.OnData = func(c Conn) error {
		out, _ := relayOut.Load().(Conn)
		if c.Userdata() != nil || out == nil {
			return nil // the outbound leg's replies, or not dialed yet
		}
		note(&peakInbound, c.InboundBuffered())
		if out.OutboundBuffered() >= limit {
			return nil // the far side is behind: leave the rest unconsumed
		}
		_, err := c.WriteTo(out)
		note(&peakOutbound, out.OutboundBuffered())
		return err
	}
	events.OnOutbound = func(c Conn, _ int) {
		if c.Userdata() == nil || c.OutboundBuffered() >= limit/2 {
			return
		}
		if in, _ := relayIn.Load().(Conn); in != nil {
			_ = in.Wake()
		}
	}
	client, server := serveInboundTest(t, events)
	relayIn.Store(server)
	out, err := events.Dial("tcp://"+sinkLn.Addr().String(), "out")
	if err != nil {
		t.Fatal(err)
	}
	relayOut.Store(out)
	sink, err := sinkLn.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Close()
	_ = server.Wake() // forward what arrived before the outbound leg

	want := make([]byte, total)
	for i := range want {
		want[i] = byte(i*7 + i>>9)
	}
	go func() { _, _ = client.Write(want) }()
	got := make([]byte, 0, total)
	buf := make([]byte, 8<<10)
	_ = sink.SetReadDeadline(time.Now().Add(60 * time.Second))
	for len(got) < total {
		n, err := sink.Read(buf)
		got = append(got, buf[:n]...)
		if err != nil {
			t.Fatalf("sink read after %d of %d bytes: %v", len(got), total, err)
		}
		if len(got)%(1<<20) < n {
			time.Sleep(5 * time.Millisecond) // a slow reader
		}
	}
	if !bytes.Equal(got, want) {
		t.Fatal("the relayed stream arrived altered")
	}
	if p := peakInbound.Load(); p > limit+4<<10 {
		t.Fatalf("relay inbound peaked at %d bytes, limit %d", p, limit)
	}
	if p := peakOutbound.Load(); p > 2*limit+4<<10 {
		t.Fatalf("relay outbound peaked at %d bytes, limit %d", p, limit)
	}
}

// A codec that reserves a packet larger than the limit gets
// ErrInboundOverflow, so it never waits on a read the limit holds; packets
// within it decode however the stream is split.
func TestInboundLimitWithReserveInbound(t *testing.T) {
	const limit = 8 << 10
	var mu sync.Mutex
	var packets []int
	var reserveErr atomic.Value
	events := &Events{
		Pollers:            1,
		MaxBufferSize:      1 << 10,
		MaxInboundBuffered: limit,
		OnData: func(c Conn) error {
			var header [4]byte
			for {
				h := c.Peek(header[:])
				if len(h) < 4 {
					return nil
				}
				total := 4 + int(binary.BigEndian.Uint32(h))
				if err := c.ReserveInbound(total); err != nil {
					reserveErr.Store(err)
					return err
				}
				if len(c.PeekChunk()) < total {
					return nil
				}
				mu.Lock()
				packets = append(packets, total-4)
				mu.Unlock()
				_, _ = c.Discard(total)
			}
		},
	}
	client, _ := serveInboundTest(t, events)
	sizes := []int{0, 1, 1000, limit - 4, 3000, 7}
	var wire []byte
	for _, n := range sizes {
		wire = binary.BigEndian.AppendUint32(wire, uint32(n))
		wire = append(wire, make([]byte, n)...)
	}
	for len(wire) > 0 {
		n := min(700, len(wire))
		if _, err := client.Write(wire[:n]); err != nil {
			t.Fatal(err)
		}
		wire = wire[n:]
		time.Sleep(time.Millisecond)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		mu.Lock()
		n := len(packets)
		mu.Unlock()
		if n == len(sizes) || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	mu.Lock()
	if len(packets) != len(sizes) {
		t.Fatalf("decoded %v, want sizes %v", packets, sizes)
	}
	mu.Unlock()

	// A packet larger than the limit is refused, not waited for.
	head := binary.BigEndian.AppendUint32(nil, uint32(limit))
	if _, err := client.Write(head); err != nil {
		t.Fatal(err)
	}
	_ = client.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.ReadAll(client); err != nil {
		t.Fatalf("connection did not close after the oversize reservation: %v", err)
	}
	if err, _ := reserveErr.Load().(error); !errors.Is(err, ErrInboundOverflow) {
		t.Fatalf("ReserveInbound past the limit = %v, want %v", err, ErrInboundOverflow)
	}
}

// A close while reads are held ends the connection promptly, OnClose
// included, and Serve still returns.
func TestInboundLimitCloseWhileHeld(t *testing.T) {
	const limit = 16 << 10
	closed := make(chan error, 1)
	events := &Events{
		Pollers:            1,
		MaxBufferSize:      4 << 10,
		MaxInboundBuffered: limit,
		OnData:             func(Conn) error { return nil },
		OnClose:            func(_ Conn, err error) { closed <- err },
	}
	client, server := serveInboundTest(t, events)
	writeUntilStalled(t, client, 16<<20)
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("OnClose did not run for a connection with held reads")
	}
}

// A peer that sends and then closes while reads are held: the end of stream
// waits behind the held bytes, and once they are consumed the connection
// reads on to it and closes.
func TestInboundLimitHangupBehindHeldReads(t *testing.T) {
	const limit = 16 << 10
	var consume atomic.Bool
	var received atomic.Int64
	closed := make(chan error, 1)
	events := &Events{
		Pollers:            1,
		MaxBufferSize:      4 << 10,
		MaxInboundBuffered: limit,
		OnData: func(c Conn) error {
			if consume.Load() {
				n, _ := c.Discard(-1)
				received.Add(int64(n))
			}
			return nil
		},
		OnClose: func(_ Conn, err error) { closed <- err },
	}
	client, server := serveInboundTest(t, events)
	const total = 256 << 10
	go func() {
		_, _ = client.Write(make([]byte, total))
		_ = client.(*net.TCPConn).CloseWrite()
	}()
	time.Sleep(200 * time.Millisecond)
	select {
	case <-closed:
		t.Fatal("closed before the held bytes were consumed")
	default:
	}
	consume.Store(true)
	if err := server.Wake(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-closed:
		if !errors.Is(err, io.EOF) {
			t.Fatalf("close error = %v, want EOF", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("no close after consuming; received %d of %d", received.Load(), total)
	}
	if got := received.Load(); got != total {
		t.Fatalf("received %d of %d bytes before the end of stream", got, total)
	}
}

// A read deadline still closes a connection whose reads are held: the held
// wait reads no socket, so it must watch the deadline itself, through
// SetReadDeadline and SetDeadline alike, set before or during the hold, moved
// later, and cleared.
func TestInboundLimitDeadlineWhileHeld(t *testing.T) {
	const limit = 16 << 10
	set := map[string]func(Conn, time.Time) error{
		"SetReadDeadline": func(c Conn, d time.Time) error { return c.SetReadDeadline(d) },
		"SetDeadline":     func(c Conn, d time.Time) error { return c.SetDeadline(d) },
	}
	for name, setDeadline := range set {
		t.Run(name, func(t *testing.T) {
			for _, when := range []string{"before hold", "during hold", "moved later", "cleared"} {
				t.Run(when, func(t *testing.T) {
					closed := make(chan error, 1)
					events := &Events{
						Pollers:            1,
						MaxBufferSize:      4 << 10,
						MaxInboundBuffered: limit,
						OnData:             func(Conn) error { return nil },
						OnClose:            func(_ Conn, err error) { closed <- err },
					}
					client, server := serveInboundTest(t, events)
					if when == "before hold" {
						if err := setDeadline(server, time.Now().Add(300*time.Millisecond)); err != nil {
							t.Fatal(err)
						}
					}
					writeUntilStalled(t, client, 16<<20)
					start := time.Now()
					switch when {
					case "during hold":
						if err := setDeadline(server, time.Now().Add(100*time.Millisecond)); err != nil {
							t.Fatal(err)
						}
					case "moved later":
						if err := setDeadline(server, time.Now().Add(50*time.Millisecond)); err != nil {
							t.Fatal(err)
						}
						if err := setDeadline(server, time.Now().Add(400*time.Millisecond)); err != nil {
							t.Fatal(err)
						}
					case "cleared":
						if err := setDeadline(server, time.Now().Add(100*time.Millisecond)); err != nil {
							t.Fatal(err)
						}
						if err := setDeadline(server, time.Time{}); err != nil {
							t.Fatal(err)
						}
						select {
						case err := <-closed:
							t.Fatalf("a cleared deadline closed the connection: %v", err)
						case <-time.After(400 * time.Millisecond):
						}
						return
					}
					select {
					case err := <-closed:
						if !errors.Is(err, os.ErrDeadlineExceeded) {
							t.Fatalf("close error = %v, want a deadline", err)
						}
						if when == "moved later" && time.Since(start) < 350*time.Millisecond {
							t.Fatalf("closed after %v, before the moved deadline", time.Since(start))
						}
					case <-time.After(3 * time.Second):
						t.Fatal("the deadline did not close a connection with held reads")
					}
				})
			}
		})
	}
}

// Moving the read deadline wakes a held wait to re-time it, and must not
// resume the read: the reads stay held, and nothing more is buffered.
func TestInboundLimitDeadlineChangeKeepsReadsHeld(t *testing.T) {
	const limit = 16 << 10
	var buffered atomic.Int64
	events := &Events{
		Pollers:            1,
		MaxBufferSize:      4 << 10,
		MaxInboundBuffered: limit,
		OnData: func(c Conn) error {
			buffered.Store(int64(c.InboundBuffered()))
			return nil
		},
	}
	client, server := serveInboundTest(t, events)
	writeUntilStalled(t, client, 16<<20)
	time.Sleep(50 * time.Millisecond)
	held := buffered.Load()
	for i := range 20 {
		if err := server.SetReadDeadline(time.Now().Add(time.Duration(10+i) * time.Second)); err != nil {
			t.Fatal(err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := server.SetReadDeadline(time.Time{}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if got := buffered.Load(); got != held {
		t.Fatalf("buffered %d bytes after deadline changes, %d while held: the read resumed", got, held)
	}
}

// Deadlines at the edges of time.Time hold their meaning while reads are
// held: the Unix epoch is a deadline long passed, not a cleared one, and a
// deadline centuries away is one that does not expire soon.
func TestInboundLimitDeadlineEdgeValuesWhileHeld(t *testing.T) {
	const limit = 16 << 10
	for _, tc := range []struct {
		name     string
		deadline time.Time
		closes   bool
	}{
		{"unix epoch", time.Unix(0, 0), true},
		{"before unix epoch", time.Unix(-1, 0), true},
		{"year 2500", time.Date(2500, 1, 1, 0, 0, 0, 0, time.UTC), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			closed := make(chan error, 1)
			events := &Events{
				Pollers:            1,
				MaxBufferSize:      4 << 10,
				MaxInboundBuffered: limit,
				OnData:             func(Conn) error { return nil },
				OnClose:            func(_ Conn, err error) { closed <- err },
			}
			client, server := serveInboundTest(t, events)
			writeUntilStalled(t, client, 16<<20)
			if err := server.SetReadDeadline(tc.deadline); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-closed:
				if !tc.closes {
					t.Fatalf("a deadline in %d closed the connection: %v", tc.deadline.Year(), err)
				}
				if !errors.Is(err, os.ErrDeadlineExceeded) {
					t.Fatalf("close error = %v, want a deadline", err)
				}
			case <-time.After(500 * time.Millisecond):
				if tc.closes {
					t.Fatal("a deadline long passed did not close the connection")
				}
			}
		})
	}
}
