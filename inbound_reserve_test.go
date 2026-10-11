package uio

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestReserveInboundJoinsBufferedBytes(t *testing.T) {
	conn := &commonConn{events: &Events{}, loop: &eventLoop{}}
	started := conn.beginInboundCallback()
	defer conn.endInboundCallback(started)
	_, _ = conn.inbound.Write(bytes.Repeat([]byte("a"), 3000))
	_, _ = conn.inbound.Write(bytes.Repeat([]byte("b"), 5000))
	conn.inboundTail = bytes.Repeat([]byte("c"), 100)
	want := append(conn.Peek(make([]byte, conn.InboundBuffered())), []byte(nil)...)

	const unit = 256 << 10
	if err := conn.reserveInbound(unit, 0); err != nil {
		t.Fatal(err)
	}
	if conn.inboundTail != nil || !bytes.Equal(conn.PeekChunk(), want) {
		t.Fatalf("after ReserveInbound the first chunk has %d of %d bytes", len(conn.PeekChunk()), len(want))
	}
	_, _ = conn.inbound.Write(make([]byte, unit-len(want)))
	if chunk := conn.PeekChunk(); len(chunk) != unit {
		t.Fatalf("the completed unit spans chunks: first chunk %d bytes, want %d", len(chunk), unit)
	}
	if n, _ := conn.Discard(unit); n != unit || conn.InboundBuffered() != 0 {
		t.Fatalf("Discard(%d) = %d, %d bytes left", unit, n, conn.InboundBuffered())
	}
}

func TestReserveInboundTakesOnlyTheUnitFromTheTail(t *testing.T) {
	conn := &commonConn{events: &Events{}, loop: &eventLoop{}}
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
	conn := &commonConn{events: &Events{}, loop: &eventLoop{}}
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

func TestReserveInboundAboveTheLimitOverflows(t *testing.T) {
	conn := &commonConn{events: &Events{}, loop: &eventLoop{}}
	started := conn.beginInboundCallback()
	defer conn.endInboundCallback(started)
	conn.inboundTail = []byte("head")
	if err := conn.reserveInbound(1<<20, 1<<10); !errors.Is(err, ErrInboundOverflow) {
		t.Fatalf("ReserveInbound past MaxInboundBuffered = %v, want %v", err, ErrInboundOverflow)
	}
	if string(conn.inboundTail) != "head" || !conn.inbound.Empty() {
		t.Fatal("a refused reservation moved the buffered bytes")
	}
}

// packetDecoder reads 4-byte little-endian length-prefixed packets the way an
// RPC codec does: a packet inside the first chunk is used in place, any other
// is copied out with Peek into a pooled buffer. reserve decides whether a
// packet that is not whole in the first chunk is reserved instead, from its
// length and the bytes buffered; a reserved packet is decoded in place once
// complete.
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
				continue // now whole in the first chunk
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

// startPacketServer serves one decoder on a loopback port with RPC-sized read
// buffers and returns a client connection to it.
func startPacketServer(tb testing.TB, decoder *packetDecoder) net.Conn {
	tb.Helper()
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		tb.Fatal(err)
	}
	addr := probe.Addr().String()
	_ = probe.Close()
	started := make(chan struct{})
	server := &Events{
		Pollers:       1,
		MaxBufferSize: 64 << 10,
		OnStart:       func(*Events) { close(started) },
		OnData:        decoder.onData,
	}
	done := make(chan error, 1)
	go func() { done <- server.Serve("tcp://" + addr) }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		tb.Fatal("server did not start")
	}
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

// Packets that span many reads arrive whole and in place once the decoder
// reserves them, whatever the sizes and however the writes split them.
func TestReservedPacketsArriveWholeInOneChunk(t *testing.T) {
	sizes := []int{0, 1, 100, 64<<10 - 4, 64 << 10, 200 << 10, 1 << 20, 3<<20 + 7}
	decoder := &packetDecoder{
		reserve: func(int, int) bool { return true },
		done:    make(chan struct{}),
		want:    int64(len(sizes)),
	}
	client := startPacketServer(t, decoder)
	var stream []byte
	var want uint32
	for i, size := range sizes {
		payload := bytes.Repeat([]byte{byte(i + 1)}, size)
		want += crc32.ChecksumIEEE(payload)
		stream = append(stream, packetWire(payload)...)
	}
	for written := 0; written < len(stream); {
		step := min(len(stream)-written, 1+written%37000)
		if _, err := client.Write(stream[written : written+step]); err != nil {
			t.Fatal(err)
		}
		written += step
	}
	select {
	case <-decoder.done:
	case <-time.After(10 * time.Second):
		t.Fatalf("decoded %d of %d packets", decoder.packets.Load(), len(sizes))
	}
	if decoder.sum.Load() != want {
		t.Fatal("packet contents differ from what was sent")
	}
	if decoder.inPlace.Load() != int64(len(sizes)) {
		t.Fatalf("%d of %d packets were decoded in place, want all", decoder.inPlace.Load(), len(sizes))
	}
}

// Run with:
//
//	go test -run '^$' -bench '^BenchmarkLargePacketDecode$' -benchmem .
//
// The server decodes packets a client streams over loopback, with 64 KiB
// reads as drpc configures them. copy is the codec without a reservation,
// which copies every packet that spans reads out with Peek; reserve reserves
// an incomplete packet as soon as its header arrives; half waits until half
// of it is buffered, which bounds what a peer can make the server allocate
// to twice what it has sent. cpu-ns/op is the CPU time of the whole process,
// the client's writes and both sides' kernel work included, per packet.
func BenchmarkLargePacketDecode(b *testing.B) {
	policies := []struct {
		name    string
		reserve func(total, buffered int) bool
	}{
		{"copy", nil},
		{"reserve", func(int, int) bool { return true }},
		{"half", func(total, buffered int) bool { return 2*buffered >= total }},
	}
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
