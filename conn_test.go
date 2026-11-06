package uio

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestAcquireBufferRejectsNegativeCapacity(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("negative capacity did not panic")
		}
	}()
	AcquireBuffer(-1)
}

func dialConnectedUDP(t *testing.T) (Conn, chan error) {
	t.Helper()
	receiver, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = receiver.Close()
	})
	closed := make(chan error, 1)
	started := make(chan struct{})
	events := withFuncs(&Events{Pollers: 1}, FuncHandler{Start: func(*Events) {
		close(started)
	}, Read: func(Conn) error {
		return nil
	}, Close: func(_ Conn, err error) {
		signal(closed, err)
	}})
	done := make(chan error, 1)
	go func() {
		done <- events.Serve()
	}()
	t.Cleanup(func() {
		_ = events.Close(nil)
		<-done
	})
	<-started
	conn, err := events.Dial("udp://" + receiver.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = conn.Close()
	})
	return conn, closed
}

func TestConnDeadlineUDPIdleWriteDeadlineCloses(t *testing.T) {
	conn, closed := dialConnectedUDP(t)
	if err := conn.SetWriteDeadline(time.Now().Add(50 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-closed:
		if !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("closed with %v, want the write deadline", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("an idle datagram connection outlived its write deadline")
	}
}

func TestConnDeadlineUDPClearedWriteDeadlineNeverCloses(t *testing.T) {
	conn, closed := dialConnectedUDP(t)
	if err := conn.SetWriteDeadline(time.Now().Add(100 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if err := conn.SetWriteDeadline(time.Time{}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-closed:
		t.Fatalf("a cleared write deadline closed the connection: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
}

func serveLoopback(tb testing.TB, events *Events) (string, <-chan error) {
	tb.Helper()
	addrs := make(chan string, 1)
	onStart := funcs(events).Start
	funcs(events).Start = func(ev *Events) {
		if onStart != nil {
			onStart(ev)
		}
		for _, l := range ev.acceptor.listeners {
			addrs <- l.ln.Addr().String()
			return
		}
	}
	tb.Cleanup(func() {
		_ = events.Close(nil)
		_ = events.Wait()
	})
	done := make(chan error, 1)
	go func() {
		done <- events.Serve("tcp://127.0.0.1:0")
	}()
	select {
	case addr := <-addrs:
		return addr, done
	case err := <-done:
		tb.Fatalf("Serve: %v", err)
	case <-time.After(5 * time.Second):
		tb.Fatal("server did not start")
	}
	return "", nil
}

func serveInboundTest(t *testing.T, events *Events) (net.Conn, Conn) {
	t.Helper()
	opened := make(chan Conn, 1)
	onOpen := funcs(events).Open
	funcs(events).Open = func(c Conn) {
		if onOpen != nil {
			onOpen(c)
		}
		select {
		case opened <- c:
		default:
		}
	}
	addr, done := serveLoopback(t, events)
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

func TestInboundLimitWithReserveInbound(t *testing.T) {
	const limit = 8 << 10
	var mu sync.Mutex
	var packets []int
	var reserveErr atomic.Value
	events := withFuncs(&Events{Pollers: 1, ReadBufferSize: 1 << 10, MaxInboundBuffered: limit}, FuncHandler{Read: func(c Conn) error {
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
	}})
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

func TestInboundLimitDeadlineWhileHeld(t *testing.T) {
	const limit = 16 << 10
	set := map[string]func(Conn, time.Time) error{"SetReadDeadline": func(c Conn, d time.Time) error {
		return c.SetReadDeadline(d)
	}, "SetDeadline": func(c Conn, d time.Time) error {
		return c.SetDeadline(d)
	}}
	for name, setDeadline := range set {
		t.Run(name, func(t *testing.T) {
			for _, when := range []string{"before hold", "during hold", "moved later", "cleared"} {
				t.Run(when, func(t *testing.T) {
					closed := make(chan error, 1)
					events := withFuncs(&Events{Pollers: 1, ReadBufferSize: 4 << 10, MaxInboundBuffered: limit}, FuncHandler{Read: func(Conn) error {
						return nil
					}, Close: func(_ Conn, err error) {
						signal(closed, err)
					}})
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

func TestInboundLimitDeadlineEdgeValuesWhileHeld(t *testing.T) {
	const limit = 16 << 10
	for _, tc := range []struct {
		name     string
		deadline time.Time
		closes   bool
	}{{"unix epoch", time.Unix(0, 0), true}, {"before unix epoch", time.Unix(-1, 0), true}, {"year 2500", time.Date(2500, 1, 1, 0, 0, 0, 0, time.UTC), false}} {
		t.Run(tc.name, func(t *testing.T) {
			closed := make(chan error, 1)
			events := withFuncs(&Events{Pollers: 1, ReadBufferSize: 4 << 10, MaxInboundBuffered: limit}, FuncHandler{Read: func(Conn) error {
				return nil
			}, Close: func(_ Conn, err error) {
				signal(closed, err)
			}})
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

func TestReserveInboundTakesOnlyTheUnitFromTheTail(t *testing.T) {
	conn := &commonConn{loop: &eventLoop{}}
	conn.entry = (&Events{}).plainEntry()
	started := conn.beginInboundCallback()
	defer conn.endInboundCallback(started)
	_, _ = conn.inbound.Write(bytes.Repeat([]byte("a"), 3000))
	_, _ = conn.inbound.Write(bytes.Repeat([]byte("b"), 5000))
	tail := append(bytes.Repeat([]byte("c"), 2000), []byte("next unit")...)
	conn.inboundTail = tail
	if err := conn.reserveInbound(10000, 0); err != nil {
		t.Fatal(err)
	}
	if len(conn.PeekChunk()) != 10000 || string(conn.inboundTail) != "next unit" || &conn.inboundTail[0] != &tail[2000] {
		t.Fatalf("first chunk %d bytes, tail %q: want the unit whole and the rest still borrowed", len(conn.PeekChunk()), conn.inboundTail)
	}
	if got := conn.Peek(make([]byte, conn.InboundBuffered())); len(got) != 10009 || string(got[10000:]) != "next unit" {
		t.Fatal("the stream order changed")
	}
}

func TestReserveInboundLeavesAWholeTailAlone(t *testing.T) {
	conn := &commonConn{loop: &eventLoop{}}
	conn.entry = (&Events{}).plainEntry()
	started := conn.beginInboundCallback()
	defer conn.endInboundCallback(started)
	tail := []byte("a whole unit")
	conn.inboundTail = tail
	if err := conn.reserveInbound(len(tail), 0); err != nil {
		t.Fatal(err)
	}
	if &conn.inboundTail[0] != &tail[0] || !conn.inbound.Empty() {
		t.Fatal("ReserveInbound copied a unit the borrowed tail already held")
	}
}

type packetDecoder struct {
	reserve func(total, buffered int) bool
	pool    sync.Pool
	packets atomic.Int64
	sum     atomic.Uint32
	inPlace atomic.Int64
	done    chan struct{}
	want    int64
}

func (d *packetDecoder) consume(packet []byte) {
	d.sum.Add(crc32.ChecksumIEEE(packet))
	if d.packets.Add(1) == d.want {
		close(d.done)
	}
}

func (d *packetDecoder) onData(conn Conn) error {
	for {
		buffered := conn.InboundBuffered()
		if buffered < 4 {
			return nil
		}
		if chunk := conn.PeekChunk(); len(chunk) >= 4 {
			if size := int(binary.LittleEndian.Uint32(chunk)); len(chunk) >= 4+size {
				d.inPlace.Add(1)
				d.consume(chunk[4 : 4+size])
				_, _ = conn.Discard(4 + size)
				continue
			}
		}
		var header [4]byte
		size := int(binary.LittleEndian.Uint32(conn.Peek(header[:])))
		total := 4 + size
		if d.reserve != nil && d.reserve(total, buffered) {
			if err := conn.ReserveInbound(total); err != nil {
				return err
			}
			if buffered >= total {
				continue
			}
		}
		if buffered < total {
			return nil
		}
		buffer, _ := d.pool.Get().(*[]byte)
		if buffer == nil || cap(*buffer) < size {
			b := make([]byte, size)
			buffer = &b
		}
		_, _ = conn.Discard(4)
		d.consume(conn.Peek((*buffer)[:size]))
		_, _ = conn.Discard(size)
		d.pool.Put(buffer)
	}
}

func startPacketServer(tb testing.TB, decoder *packetDecoder) net.Conn {
	tb.Helper()
	server := withFuncs(&Events{Pollers: 1, ReadBufferSize: 64 << 10}, FuncHandler{Read: decoder.onData})
	addr, done := serveLoopback(tb, server)
	client, err := net.Dial("tcp", addr)
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() {
		_ = client.Close()
		_ = server.Close(nil)
		<-done
	})
	return client
}

func packetWire(payload []byte) []byte {
	wire := make([]byte, 4+len(payload))
	binary.LittleEndian.PutUint32(wire, uint32(len(payload)))
	copy(wire[4:], payload)
	return wire
}

func BenchmarkLargePacketDecode(b *testing.B) {
	policies := []struct {
		name    string
		reserve func(total, buffered int) bool
	}{{"copy", nil}, {"reserve", func(int, int) bool {
		return true
	}}, {"half", func(total, buffered int) bool {
		return 2*buffered >= total
	}}}
	for _, size := range []int{64 << 10, 256 << 10, 1 << 20, 4 << 20} {
		wire := packetWire(make([]byte, size))
		for _, policy := range policies {
			b.Run(fmt.Sprintf("size=%dKiB/policy=%s", size>>10, policy.name), func(b *testing.B) {
				decoder := &packetDecoder{reserve: policy.reserve, done: make(chan struct{}), want: int64(b.N)}
				client := startPacketServer(b, decoder)
				b.SetBytes(int64(size))
				b.ReportAllocs()
				b.ResetTimer()
				cpu := processCPUTime()
				go func() {
					for range b.N {
						if _, err := client.Write(wire); err != nil {
							return
						}
					}
				}()
				select {
				case <-decoder.done:
				case <-time.After(time.Minute):
					b.Fatalf("decoded %d of %d packets", decoder.packets.Load(), b.N)
				}
				b.StopTimer()
				b.ReportMetric(float64(processCPUTime()-cpu)/float64(b.N), "cpu-ns/op")
				b.ReportMetric(100*float64(decoder.inPlace.Load())/float64(b.N), "%inplace")
			})
		}
	}
}

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
	for name, set := range map[string]func() error{"deadline": func() error {
		return conn.SetDeadline(time.Now())
	}, "read deadline": func() error {
		return conn.SetReadDeadline(time.Now())
	}, "write deadline": func() error {
		return conn.SetWriteDeadline(time.Now())
	}} {
		t.Run(name, func(t *testing.T) {
			if err := set(); !errors.Is(err, ErrUnsupported) {
				t.Fatalf("error = %v, want %v", err, ErrUnsupported)
			}
		})
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
	}{{name: "zero", tail: "ab", discard: 0, wantBytes: "ab"}, {name: "tail only", tail: "abc", discard: 1, wantN: 1, wantBytes: "bc"}, {name: "within buffer", buffered: "abc", tail: "de", discard: 2, wantN: 2, wantBytes: "cde"}, {name: "across buffers", buffered: "abc", tail: "de", discard: 4, wantN: 4, wantBytes: "e"}, {name: "all", buffered: "abc", tail: "de", discard: -1, wantN: 5}}
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
	events := withFuncs(&Events{Pollers: 1}, FuncHandler{Start: func(*Events) {
		close(started)
	}, Read: func(c Conn) error {
		payload := c.Peek(make([]byte, c.InboundBuffered()))
		if string(payload) == "expire" {
			if err := c.SetReadDeadline(time.Now().Add(50 * time.Millisecond)); err != nil {
				return err
			}
		}
		_, err := c.WriteTo(c)
		return err
	}, Close: func(c Conn, err error) {
		signal(closed, closeEvent{c.RemoteAddr().String(), err})
	}})
	done := make(chan error, 1)
	go func() {
		done <- events.Serve("udp://" + addr)
	}()
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

type contractRawConn struct {
	fd  uintptr
	err error
}

func (r contractRawConn) Control(fn func(uintptr)) error {
	if r.err != nil {
		return r.err
	}
	fn(r.fd)
	return nil
}

func (contractRawConn) Read(func(uintptr) bool) error {
	return ErrUnsupported
}

func (contractRawConn) Write(func(uintptr) bool) error {
	return ErrUnsupported
}

func TestDialLocalAddressAndControlValidation(t *testing.T) {
	for _, network := range []string{"tcp", "udp"} {
		if _, err := netDialerFor(network, dialConfig{localAddr: "127.0.0.1:65536"}); err == nil {
			t.Fatalf("%s accepted an invalid local port", network)
		}
	}
	if _, err := netDialerFor("ip:icmp", dialConfig{localAddr: "127.0.0.1"}); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("unsupported local network = %v", err)
	}
	dialer, err := netDialerFor("unix", dialConfig{localAddr: "unix://local.sock"})
	if err != nil || dialer.LocalAddr.(*net.UnixAddr).Name != "local.sock" {
		t.Fatalf("unix local address = %v, %v", dialer, err)
	}
	cause := errors.New("application control denied")
	calls := 0
	dialer, err = netDialerFor("tcp", dialConfig{control: func(network, address string, fd uintptr) error {
		calls++
		if network != "tcp" || address != "target" || fd != 17 {
			t.Error("control metadata changed")
		}
		return cause
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := dialer.Control("tcp", "target", contractRawConn{fd: 17}); !errors.Is(err, cause) || calls != 1 {
		t.Fatal("dial control error/metadata was not preserved")
	}
	rawCause := errors.New("descriptor unavailable")
	if err := rawControl(func(string, string, uintptr) error {
		t.Error("called application without a descriptor")
		return nil
	})("tcp", "target", contractRawConn{err: rawCause}); !errors.Is(err, rawCause) {
		t.Fatalf("raw descriptor error = %v", err)
	}
	config := netListenConfigFor(true, func(string, string, uintptr) error {
		t.Error("application control ran after reuseport failed")
		return nil
	})
	if err := config.Control("tcp", "target", contractRawConn{fd: ^uintptr(0)}); err == nil {
		t.Fatal("reuseport accepted an invalid socket")
	}
}

func TestAddressCopyOnWriteAndInboundReservationPreserveData(t *testing.T) {
	if (&commonConn{}).LocalAddr() != nil {
		t.Fatal("unset local address must be nil")
	}
	local := &net.UnixAddr{Name: "listener", Net: "unix"}
	oldRemote := &net.UnixAddr{Name: "old", Net: "unix"}
	shared := &addrPair{local: local, remote: oldRemote}
	conn := &commonConn{addr: shared}
	conn.setRemoteAddr(nil)
	if conn.addr != shared {
		t.Fatal("nil remote changed address ownership")
	}
	remote := &net.UnixAddr{Name: "new", Net: "unix"}
	conn.setRemoteAddr(remote)
	if conn.addr == shared || shared.remote != oldRemote || conn.addr.local != local || conn.addr.remote != remote {
		t.Fatal("remote update mutated a shared listener address pair")
	}
	fresh := &commonConn{}
	fresh.setRemoteAddr(remote)
	if fresh.addr == nil || fresh.addr.remote != remote {
		t.Fatal("non-IP address was lost")
	}
	_, _ = fresh.inbound.Write([]byte("prefix"))
	fresh.inboundTail = []byte("tail")
	chunk := fresh.PeekChunk()
	if err := fresh.reserveInbound(3, 16); err != nil {
		t.Fatal(err)
	}
	if next := fresh.PeekChunk(); len(next) == 0 || &next[0] != &chunk[0] {
		t.Fatal("reserving an existing prefix replaced its borrowed chunk")
	}
	var got strings.Builder
	if _, err := fresh.WriteTo(&got); err != nil || got.String() != "prefixtail" {
		t.Fatalf("reserved input = %q, %v", got.String(), err)
	}
}

func TestCloseCounterWaitsForEveryInFlightClose(t *testing.T) {
	var counter closeCounter
	counter.add()
	counter.add()
	done := make(chan struct{})
	go func() {
		counter.wait()
		close(done)
	}()
	waitFor(t, time.Second, func() bool {
		counter.mu.Lock()
		defer counter.mu.Unlock()
		return counter.cond != nil
	})
	counter.mu.Lock()
	installed := counter.cond != nil
	counter.mu.Unlock()
	if !installed {
		t.Fatal("close waiter was not installed")
	}
	counter.done()
	readControlQuiet(t, done, "lifecycle join with a close still in flight")
	counter.done()
	readControlAwait(t, done, "all close references released")
	counter.wait()
}

func TestCurrentGoroutineIDIsStableAndAllocationFree(t *testing.T) {
	want := currentGoroutineID()
	if want == 0 {
		t.Fatal("current goroutine ID is zero")
	}
	allocations := testing.AllocsPerRun(1000, func() {
		if got := currentGoroutineID(); got != want {
			panic("goroutine ID changed")
		}
	})
	if allocations != 0 {
		t.Fatalf("currentGoroutineID allocations = %v", allocations)
	}
}

func halfCloseTestData(size int) []byte {
	data := make([]byte, size)
	for i := range data {
		data[i] = byte(i*29 + i>>11)
	}
	return data
}

func readAllWithin(t *testing.T, conn net.Conn, d time.Duration) []byte {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(d))
	got, err := io.ReadAll(conn)
	if err != nil {
		t.Fatalf("read to the end of stream after %d bytes: %v", len(got), err)
	}
	return got
}

func TestWritesAfterCloseWriteAreRefused(t *testing.T) {
	results := make(chan error, 8)
	events := withFuncs(&Events{Pollers: 1}, FuncHandler{Read: func(c Conn) error {
		_, _ = c.Discard(-1)
		if err := c.CloseWrite(); err != nil {
			results <- err
			return nil
		}
		_, err := c.Write([]byte("x"))
		results <- err
		_, err = c.Writev([][]byte{[]byte("y")})
		results <- err
		buf := AcquireBuffer(1)
		buf.CommitWrite(copy(buf.AvailableBuffer()[:1], "z"))
		if _, err = c.WriteOwned(buf); err == ErrOutboundOverflow {
			ReleaseBuffer(buf)
		}
		results <- err
		results <- c.CloseWrite()
		return nil
	}})
	client, server := serveInboundTest(t, events)
	if _, err := client.Write([]byte("go")); err != nil {
		t.Fatal(err)
	}
	for i, want := range []error{net.ErrClosed, net.ErrClosed, net.ErrClosed, nil} {
		select {
		case err := <-results:
			if !errors.Is(err, want) && !(want == nil && err == nil) {
				t.Fatalf("call %d after CloseWrite = %v, want %v", i, err, want)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("OnData did not run")
		}
	}
	if _, err := server.Write([]byte("outside")); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("write from another goroutine after CloseWrite = %v, want net.ErrClosed", err)
	}
	if got := readAllWithin(t, client, 2*time.Second); len(got) != 0 {
		t.Fatalf("the peer read %q after CloseWrite", got)
	}
}

func TestAllowHalfOpenKeepsWritingAfterPeerEOF(t *testing.T) {
	reply := halfCloseTestData(1 << 20)
	var request bytes.Buffer
	closed := make(chan error, 1)
	events := withFuncs(&Events{Pollers: 1, AllowHalfOpen: true}, FuncHandler{Read: func(c Conn) error {
		buf := make([]byte, 4096)
		for {
			n, err := c.Read(buf)
			request.Write(buf[:n])
			if err == io.EOF {
				if _, err := c.Write(reply); err != nil {
					return err
				}
				return c.CloseWrite()
			}
			if n == 0 {
				return err
			}
		}
	}, Close: func(_ Conn, err error) {
		signal(closed, err)
	}})
	client, _ := serveInboundTest(t, events)
	if _, err := client.Write([]byte("request")); err != nil {
		t.Fatal(err)
	}
	if err := client.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	if got := readAllWithin(t, client, 10*time.Second); !bytes.Equal(got, reply) {
		t.Fatalf("read %d reply bytes, want %d intact", len(got), len(reply))
	}
	select {
	case err := <-closed:
		if !errors.Is(err, io.EOF) {
			t.Fatalf("close error = %v, want EOF", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("both halves shut, but the connection did not close")
	}
	if request.String() != "request" {
		t.Fatalf("server read %q, want %q", request.String(), "request")
	}
}

func TestHalfCloseThroughARelay(t *testing.T) {
	request := halfCloseTestData(2 << 20)
	reply := halfCloseTestData(3 << 20)[1:]
	sinkLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer sinkLn.Close()
	sinkDone := make(chan error, 1)
	go func() {
		sink, err := sinkLn.Accept()
		if err != nil {
			sinkDone <- err
			return
		}
		defer sink.Close()
		got, err := io.ReadAll(sink)
		if err != nil || !bytes.Equal(got, request) {
			sinkDone <- errors.New("the sink did not get the whole request before its end")
			return
		}
		if _, err := sink.Write(reply); err != nil {
			sinkDone <- err
			return
		}
		sinkDone <- sink.(*net.TCPConn).CloseWrite()
	}()
	type leg struct{ peer atomic.Value }
	var closes atomic.Int32
	events := &Events{Pollers: 1, AllowHalfOpen: true}
	forward := func(c Conn) error {
		l, _ := c.Userdata().(*leg)
		if l == nil {
			return nil
		}
		peer, _ := l.peer.Load().(Conn)
		if peer == nil {
			return nil
		}
		if _, err := c.WriteTo(peer); err != nil {
			return err
		}
		if _, err := c.Read(nil); err == io.EOF {
			return peer.CloseWrite()
		}
		return nil
	}
	funcs(events).Read = forward
	funcs(events).Close = func(Conn, error) {
		closes.Add(1)
	}
	client, in := serveInboundTest(t, events)
	inLeg := &leg{}
	in.SetUserdata(inLeg)
	outLeg := &leg{}
	outLeg.peer.Store(in)
	out, err := events.Dial("tcp://"+sinkLn.Addr().String(), WithUserdata(outLeg))
	if err != nil {
		t.Fatal(err)
	}
	inLeg.peer.Store(out)
	_ = in.Wake()
	go func() {
		_, _ = client.Write(request)
		_ = client.(*net.TCPConn).CloseWrite()
	}()
	if got := readAllWithin(t, client, 20*time.Second); !bytes.Equal(got, reply) {
		t.Fatalf("client read %d reply bytes, want %d intact", len(got), len(reply))
	}
	if err := <-sinkDone; err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for closes.Load() < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("%d of the relay's two legs closed after both ends shut", closes.Load())
		}
		time.Sleep(5 * time.Millisecond)
	}
} // Conn

func TestAllowHalfOpenNoticesAResetAfterEOF(t *testing.T) {
	eof := make(chan struct{})
	closed := make(chan error, 1)
	events := withFuncs(&Events{Pollers: 1, AllowHalfOpen: true}, FuncHandler{Read: func(c Conn) error {
		if _, err := c.Read(make([]byte, 64)); err == io.EOF {
			close(eof)
		}
		return nil
	}, Close: func(_ Conn, err error) {
		signal(closed, err)
	}})
	client, server := serveInboundTest(t, events)
	tcp := client.(*net.TCPConn)
	_ = tcp.CloseWrite()
	select {
	case <-eof:
	case <-time.After(2 * time.Second):
		t.Fatal("OnData did not see the end of stream")
	}
	select {
	case err := <-closed:
		t.Fatalf("closed at the peer's end of stream: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	_ = tcp.SetLinger(0)
	_ = tcp.Close()
	if stdioBackend {
		time.Sleep(100 * time.Millisecond)
		for range 3 {
			_, _ = server.Write([]byte("x"))
			time.Sleep(20 * time.Millisecond)
		}
	}
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("a reset after the end of stream left the connection open")
	}
}

func TestCloseWriteRacingWritesKeepsTheStreamWhole(t *testing.T) {
	events := &Events{Pollers: 1, MaxOutboundBuffered: 4 << 20}
	client, server := serveInboundTest(t, events)
	var accepted atomic.Int64
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		chunk := make([]byte, 1024)
		stop := time.Now().Add(5 * time.Second)
		for seq := 0; ; {
			if time.Now().After(stop) {
				t.Error("writes were still accepted 5s after CloseWrite")
				return
			}
			for i := range chunk {
				chunk[i] = byte(seq)
			}
			n, err := server.Write(chunk)
			accepted.Add(int64(n))
			if errors.Is(err, ErrOutboundOverflow) {
				time.Sleep(time.Millisecond)
				continue
			}
			if err != nil {
				if !errors.Is(err, net.ErrClosed) {
					t.Errorf("write %d: %v", seq, err)
				}
				return
			}
			seq++
		}
	}()
	readDone := make(chan error, 1)
	var read int64
	go func() {
		buf := make([]byte, 64<<10)
		_ = client.SetReadDeadline(time.Now().Add(20 * time.Second))
		for {
			n, err := client.Read(buf)
			for i := range n {
				if pos := read + int64(i); buf[i] != byte(pos/1024) {
					readDone <- errors.New("the stream was reordered")
					return
				}
			}
			read += int64(n)
			if err == io.EOF {
				readDone <- nil
				return
			}
			if err != nil {
				readDone <- err
				return
			}
		}
	}()
	time.Sleep(20 * time.Millisecond)
	if err := server.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	<-writerDone
	if err := <-readDone; err != nil {
		t.Fatalf("after %d bytes: %v", read, err)
	}
	if read != accepted.Load() {
		t.Fatalf("peer read %d bytes, %d were accepted", read, accepted.Load())
	}
}

func TestCloseWriteUnsupportedOnUDP(t *testing.T) {
	probe, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := probe.LocalAddr().String()
	_ = probe.Close()
	started := make(chan struct{})
	events := withFuncs(&Events{Pollers: 1}, FuncHandler{Start: func(*Events) {
		close(started)
	}})
	done := make(chan error, 1)
	go func() {
		done <- events.Serve()
	}()
	<-started
	t.Cleanup(func() {
		_ = events.Close(nil)
		<-done
	})
	conn, err := events.Dial("udp://" + addr)
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.CloseWrite(); err == nil {
		t.Fatal("CloseWrite on a UDP connection succeeded")
	}
}

func TestHandlerTimerCloseSkipsRead(t *testing.T) {
	var mu sync.Mutex
	var order []string
	note := func(s string) {
		mu.Lock()
		order = append(order, s)
		mu.Unlock()
	}
	conn := serveAndDial(t, FuncHandler{Read: func(Conn) error {
		note("read")
		return nil
	}, Timer: func(c Conn) error {
		note("timer")
		_ = c.Close()
		return nil
	}, Close: func(Conn, error) {
		note("close")
	}})
	if err := conn.SetWakeDeadline(time.Now().Add(20 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		got := append([]string(nil), order...)
		mu.Unlock()
		if len(got) > 0 && got[len(got)-1] == "close" {
			for _, what := range got {
				if what == "read" {
					t.Fatalf("OnRead ran in the turn whose OnTimer closed: %v", got)
				}
			}
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("no close within 2s: %v", order)
}

func TestWakeIntervalCatchUpIsBounded(t *testing.T) {
	conn := serveAndDial(t, FuncHandler{Read: func(Conn) error {
		return nil
	}, Timer: func(Conn) error {
		return nil
	}, Close: func(Conn, error) {
	}})
	if err := conn.SetWakeInterval(time.Nanosecond); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	before := time.Now()
	if err := conn.SetWakeInterval(0); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(before); elapsed > 5*time.Second {
		t.Fatalf("stopping a backed-up 1ns interval took %v, want the catch-up skipped", elapsed)
	}
}

type wakeRecorder struct {
	mu     sync.Mutex
	times  []time.Time
	timers []time.Time
	opened atomic.Int32
	closed atomic.Int32
}

func (r *wakeRecorder) OnOpen(Conn) {
	r.opened.Add(1)
}

func (r *wakeRecorder) OnRead(Conn) error {
	r.mu.Lock()
	r.times = append(r.times, time.Now())
	r.mu.Unlock()
	return nil
}

func (r *wakeRecorder) OnTimer(Conn) error {
	r.mu.Lock()
	r.timers = append(r.timers, time.Now())
	r.mu.Unlock()
	return nil
}

func (r *wakeRecorder) OnClose(Conn, error) {
	r.closed.Add(1)
}

func (r *wakeRecorder) timerFires() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.timers)
}

func (r *wakeRecorder) fires() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.times)
}

func (r *wakeRecorder) firstAt() time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.times[0]
}
