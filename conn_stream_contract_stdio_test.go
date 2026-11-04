//go:build windows || stdio

package uio

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"testing"
	"time"
)

func queuedStdConnection(t *testing.T, limit int) *fdConn {
	t.Helper()
	events := &Events{MaxOutboundBuffered: limit, readBufferSize: 4096}
	conn := &fdConn{commonConn: commonConn{entry: events.plainEntry()}, conn: newStdRegistrationConn(0), writeSig: make(chan struct{}, 1)}
	t.Cleanup(func() { conn.outbound.Reset() })
	return conn
}

func TestStdSocketOptionsPreserveTCPAndLifecycleErrors(t *testing.T) {
	first, second := tcpConnectionPair(t)
	defer first.Close()
	defer second.Close()
	live := &fdConn{conn: first}
	unsupported := &fdConn{conn: newStdRegistrationConn(0)}
	closed := &fdConn{conn: first}
	closed.closing.Store(true)
	options := []struct {
		name string
		set  func(*fdConn) error
	}{
		{"linger", func(c *fdConn) error { return c.SetLinger(-1) }},
		{"no-delay", func(c *fdConn) error { return c.SetNoDelay(true) }},
		{"keep-alive", func(c *fdConn) error { return c.SetKeepAlive(true) }},
		{"keep-alive-period", func(c *fdConn) error { return c.SetKeepAlivePeriod(7) }},
		{"read-buffer", func(c *fdConn) error { return c.SetReadBuffer(8192) }},
		{"write-buffer", func(c *fdConn) error { return c.SetWriteBuffer(8192) }},
	}
	for _, option := range options {
		t.Run(option.name, func(t *testing.T) {
			if err := option.set(live); err != nil {
				t.Fatalf("live TCP option = %v", err)
			}
			if err := option.set(unsupported); !errors.Is(err, ErrUnsupported) {
				t.Fatalf("custom transport option = %v", err)
			}
			if err := option.set(closed); !errors.Is(err, net.ErrClosed) {
				t.Fatalf("closed lifecycle option = %v", err)
			}
		})
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	for _, option := range options {
		if err := option.set(live); !errors.Is(err, net.ErrClosed) {
			t.Fatalf("%s underlying TCP close = %v", option.name, err)
		}
	}
}

func TestStdVectorAdmissionCopiesOnlyAcceptedInput(t *testing.T) {
	for _, segments := range []int{2, 10} {
		t.Run(fmt.Sprint("segments=", segments), func(t *testing.T) {
			conn := queuedStdConnection(t, 0)
			vec := make([][]byte, segments)
			var want []byte
			for i := range vec {
				vec[i] = bytes.Repeat([]byte{byte('a' + i)}, 4096)
				want = append(want, vec[i]...)
			}
			vec = append(vec, nil)
			n, err := conn.Writev(vec)
			if n != len(want) || err != nil {
				t.Fatalf("Writev = %d, %v", n, err)
			}
			for _, part := range vec {
				clear(part)
			}
			var got bytes.Buffer
			if _, err := conn.outbound.WriteTo(&got); err != nil || !bytes.Equal(got.Bytes(), want) {
				t.Fatal("queued vector retained or reordered caller storage")
			}
		})
	}
	conn := queuedStdConnection(t, 4)
	if n, err := conn.Writev([][]byte{nil, {}}); n != 0 || err != nil || conn.OutboundBuffered() != 0 {
		t.Fatal("empty vector changed queue state")
	}
	if n, err := conn.Writev([][]byte{[]byte("ab"), nil, []byte("c")}); n != 3 || err != nil {
		t.Fatalf("small vector = %d, %v", n, err)
	}
	if err := conn.WriteByte('d'); err != nil {
		t.Fatal(err)
	}
	if n, err := conn.Writev([][]byte{[]byte("e")}); n != 0 || !errors.Is(err, ErrOutboundOverflow) || conn.OutboundBuffered() != 4 {
		t.Fatal("small overflow was not refused whole")
	}
	var got bytes.Buffer
	_, _ = conn.outbound.WriteTo(&got)
	if got.String() != "abcd" {
		t.Fatalf("accepted output = %q", got.String())
	}
}

func TestStdVectorAndOwnedWritesRespectClosingAndExistingBudget(t *testing.T) {
	for _, large := range []bool{false, true} {
		size := 8
		if large {
			size = 8192
		}
		for _, state := range []string{"closing", "draining", "write-shut", "overflow"} {
			t.Run(fmt.Sprintf("large=%t/%s", large, state), func(t *testing.T) {
				conn := queuedStdConnection(t, size)
				if _, err := conn.Write([]byte("old")); err != nil {
					t.Fatal(err)
				}
				switch state {
				case "closing":
					conn.closing.Store(true)
				case "draining":
					conn.draining.Store(true)
				case "write-shut":
					conn.writeShut = true
				}
				want := net.ErrClosed
				if state == "overflow" {
					want = ErrOutboundOverflow
				}
				vec := [][]byte{bytes.Repeat([]byte{'v'}, size/2), bytes.Repeat([]byte{'w'}, size-size/2)}
				if n, err := conn.Writev(vec); n != 0 || !errors.Is(err, want) {
					t.Fatalf("Writev = %d, %v, want %v", n, err, want)
				}
				owned := AcquireBuffer(size)
				_, _ = owned.Write(bytes.Repeat([]byte{'o'}, size))
				if n, err := conn.WriteOwned(owned); n != 0 || !errors.Is(err, want) {
					t.Fatalf("WriteOwned = %d, %v, want %v", n, err, want)
				}
				if state == "overflow" {
					if owned.Len() != size {
						t.Fatal("overflow consumed caller ownership")
					}
					ReleaseBuffer(owned)
				}
				if conn.OutboundBuffered() != 3 {
					t.Fatal("rejected output changed the accepted prefix")
				}
			})
		}
	}
	conn := queuedStdConnection(t, 1)
	if n, err := conn.Writev([][]byte{make([]byte, 4096)}); n != 0 || !errors.Is(err, ErrOutboundOverflow) {
		t.Fatal("oversized vector passed the write budget")
	}
	if n, err := conn.WriteOwned(nil); n != 0 || err != nil {
		t.Fatal("nil ownership was not a no-op")
	}
	if n, err := conn.WriteOwned(AcquireBuffer(0)); n != 0 || err != nil {
		t.Fatal("empty ownership was not consumed as a no-op")
	}
	if _, err := conn.ReserveOutbound(1); !errors.Is(err, ErrReserveUnsupported) {
		t.Fatalf("stdio reservation = %v", err)
	}
	conn.closing.Store(true)
	if _, err := conn.ReserveOutbound(1); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("closed reservation = %v", err)
	}
}

func TestStdConnectedDatagramsPreservePacketsAndReadFailure(t *testing.T) {
	receiver, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer receiver.Close()
	payloads := make(chan string, 4)
	closed := make(chan error, 1)
	cause := errors.New("packet consumer stopped")
	events := &Events{Pollers: 1, Handler: FuncHandler{
		Read: func(c Conn) error {
			payload := string(c.PeekChunk())
			payloads <- payload
			if payload == "stop" {
				return cause
			}
			return nil
		},
		Close: func(_ Conn, err error) { closed <- err },
	}}
	done := startStdTestEvents(t, events)
	t.Cleanup(func() { stopStdTestEvents(t, events, done, nil) })
	conn, err := events.Dial("udp://" + receiver.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	if n, err := conn.Writev([][]byte{nil, []byte("one"), {}, []byte("two")}); n != 6 || err != nil {
		t.Fatalf("packet vector = %d, %v", n, err)
	}
	var sender *net.UDPAddr
	readPacket := func(want string) {
		t.Helper()
		_ = receiver.SetReadDeadline(time.Now().Add(time.Second))
		buffer := make([]byte, 128)
		n, remote, err := receiver.ReadFromUDP(buffer)
		if err != nil || string(buffer[:n]) != want {
			t.Fatalf("packet = %q, %v, want %q", buffer[:n], err, want)
		}
		sender = remote
	}
	readPacket("one")
	readPacket("two")
	owned := AcquireBuffer(5)
	_, _ = owned.WriteString("owned")
	if n, err := conn.WriteOwned(owned); n != 5 || err != nil {
		t.Fatalf("owned packet = %d, %v", n, err)
	}
	readPacket("owned")
	if n, err := conn.Writev([][]byte{[]byte("ok"), make([]byte, 70<<10)}); n != 2 || err == nil {
		t.Fatalf("partial packet vector = %d, %v", n, err)
	}
	readPacket("ok")
	if conn.OutboundBuffered() != 0 {
		t.Fatal("datagram writes created a stream queue")
	}
	for _, payload := range []string{"first", "second", "stop"} {
		if _, err := receiver.WriteToUDP([]byte(payload), sender); err != nil {
			t.Fatal(err)
		}
		if got := readControlAwait(t, payloads, "connected packet input"); got != payload {
			t.Fatalf("unconsumed earlier packet leaked into %q", got)
		}
	}
	if err := readControlAwait(t, closed, "packet callback failure"); !errors.Is(err, cause) {
		t.Fatalf("packet OnClose = %v", err)
	}
}
