package uws

import (
	"bytes"
	"fmt"
	"sync/atomic"
	"testing"
	"unicode/utf8"

	"github.com/urpc/uio"
	"github.com/urpc/uio/uws/internal/frame"
)

// echoPathConn stands in for a native UIO connection inside its own task:
// inbound is one borrowed chunk and ReserveOutbound hands out room in a reused
// slice, so the benchmarks below measure what UWS itself spends per message.
type echoPathConn struct {
	uio.Conn
	inbound  []byte
	out      []byte
	userdata any
	closes   atomic.Int32 // CloseWith calls, from the close timer among others
}

func (c *echoPathConn) PeekChunk() []byte { return c.inbound }

func (c *echoPathConn) OutboundBuffered() int { return 0 }
func (c *echoPathConn) InboundBuffered() int  { return len(c.inbound) }
func (c *echoPathConn) Userdata() any         { return c.userdata }
func (c *echoPathConn) Flush() error          { return nil }
func (c *echoPathConn) YieldRead() error      { return nil }

func (c *echoPathConn) CloseWith(error) error {
	c.closes.Add(1)
	return nil
}

func (c *echoPathConn) Discard(n int) (int, error) {
	c.inbound = c.inbound[n:]
	return n, nil
}

func (c *echoPathConn) ReserveOutbound(n int) ([]byte, error) {
	start := len(c.out)
	c.out = c.out[:start+n]
	return c.out[start : start+n : start+n], nil
}

// WriteOwned takes a write batch the way UIO's queue does, without a copy.
func (c *echoPathConn) WriteOwned(buffer *uio.Buffer) (int, error) {
	n := buffer.Len()
	uio.ReleaseBuffer(buffer)
	return n, nil
}

type echoPathHandler struct{}

func (echoPathHandler) OnOpen(*Conn) {}

func (echoPathHandler) OnMessage(conn *Conn, message Message) {
	_ = conn.SendBinary(message.Payload)
}

func (echoPathHandler) OnClose(*Conn, CloseEvent) {}

// Run with:
//
//	go test -run '^$' -bench '^BenchmarkServerEchoPath$' -benchmem ./uws
//
// Frames is how many client frames arrive in one read, as a pipelining client
// sends them; each is echoed as the benchmark servers do.
func BenchmarkServerEchoPath(b *testing.B) {
	for _, frames := range []int{1, 100} {
		for _, size := range []int{128, 1024} {
			b.Run(fmt.Sprintf("frames=%d/payload=%d", frames, size), func(b *testing.B) {
				server := NewServer(Options{Handler: echoPathHandler{}})
				server.config = testServerConfig(server)
				raw := &echoPathConn{}
				conn := server.newConnection(raw)
				conn.opened.Store(true)
				raw.userdata = conn

				var wire []byte
				for range frames {
					wire = frame.Append(wire, frame.Frame{
						Fin: true, Opcode: frame.Binary, Masked: true, Payload: make([]byte, size),
					}, [4]byte{1, 2, 3, 4})
				}
				chunk := make([]byte, len(wire))
				raw.out = make([]byte, 0, frames*(size+14))

				b.SetBytes(int64(frames * size))
				b.ReportAllocs()
				b.ResetTimer()
				for range b.N {
					// Parsing unmasks in place, so every read starts from the
					// original masked bytes.
					copy(chunk, wire)
					raw.inbound = chunk
					raw.out = raw.out[:0]
					if err := server.onData(raw); err != nil {
						b.Fatal(err)
					}
				}
				b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*frames), "ns/frame")
			})
		}
	}
}

// Run with:
//
//	go test -run '^$' -bench '^BenchmarkServerEchoPathSplitReads$' -benchmem ./uws
//
// A pipelining client keeps several batches in flight, so a read round takes
// them in full reads that end inside a frame: here 248 binary frames of 256
// bytes arrive as four 16 KiB reads, as go-websocket-benchmark's pipeline
// sends them, and every read after the first starts mid-frame.
func BenchmarkServerEchoPathSplitReads(b *testing.B) {
	const readSize = 16 << 10
	for _, size := range []int{64, 256, 1024} {
		b.Run(fmt.Sprintf("payload=%d", size), func(b *testing.B) {
			server := NewServer(Options{Handler: echoPathHandler{}})
			server.config = testServerConfig(server)
			raw := &echoPathConn{}
			conn := server.newConnection(raw)
			conn.opened.Store(true)
			raw.userdata = conn

			var wire []byte
			frames := 0
			for len(wire) < 4*readSize-size {
				wire = frame.Append(wire, frame.Frame{
					Fin: true, Opcode: frame.Binary, Masked: true, Payload: make([]byte, size),
				}, [4]byte{1, 2, 3, 4})
				frames++
			}
			chunk := make([]byte, len(wire))
			raw.out = make([]byte, 0, frames*(size+14))

			b.SetBytes(int64(frames * size))
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				copy(chunk, wire)
				raw.out = raw.out[:0]
				for read := chunk; len(read) > 0; {
					n := min(readSize, len(read))
					raw.inbound = read[:n]
					if err := server.onData(raw); err != nil {
						b.Fatal(err)
					}
					if len(raw.inbound) != 0 {
						b.Fatalf("%d bytes of a read left unconsumed", len(raw.inbound))
					}
					read = read[n:]
				}
			}
			b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*frames), "ns/frame")
		})
	}
}

type echoTextHandler struct{}

func (echoTextHandler) OnOpen(*Conn) {}

func (echoTextHandler) OnMessage(conn *Conn, message Message) { _ = conn.SendText(message.Payload) }

func (echoTextHandler) OnClose(*Conn, CloseEvent) {}

// Run with:
//
//	go test -run '^$' -bench '^BenchmarkServerEchoPathText$' -benchmem ./uws
//
// BenchmarkServerEchoPath for text frames, which also pay the UTF-8 check:
// ASCII JSON, and text that is mostly multibyte.
func BenchmarkServerEchoPathText(b *testing.B) {
	for _, multibyte := range []bool{false, true} {
		for _, size := range []int{128, 1024, 4000} {
			b.Run(fmt.Sprintf("multibyte=%v/payload=%d", multibyte, size), func(b *testing.B) {
				server := NewServer(Options{Handler: echoTextHandler{}})
				server.config = testServerConfig(server)
				raw := &echoPathConn{}
				conn := server.newConnection(raw)
				conn.opened.Store(true)
				raw.userdata = conn

				unit := []byte(`{"k":"v"}`)
				if multibyte {
					unit = []byte("文本帧")
				}
				payload := bytes.Repeat(unit, size/len(unit)+1)[:size/len(unit)*len(unit)]
				// ASCII fills the rest, so every case is exactly its size and
				// the 128-byte one reaches frame.VectorUnmaskMin.
				payload = append(payload, bytes.Repeat([]byte{'x'}, size-len(payload))...)
				if len(payload) != size || !utf8.Valid(payload) {
					b.Fatalf("payload of %d bytes (valid UTF-8 %v), want %d", len(payload), utf8.Valid(payload), size)
				}
				const frames = 8
				var wire []byte
				for range frames {
					wire = frame.Append(wire, frame.Frame{Fin: true, Opcode: frame.Text, Masked: true, Payload: payload}, [4]byte{1, 2, 3, 4})
				}
				chunk := make([]byte, len(wire))
				raw.out = make([]byte, 0, frames*(len(payload)+14))

				b.SetBytes(int64(frames * len(payload)))
				b.ReportAllocs()
				b.ResetTimer()
				for range b.N {
					copy(chunk, wire)
					raw.inbound = chunk
					raw.out = raw.out[:0]
					if err := server.onData(raw); err != nil {
						b.Fatal(err)
					}
				}
				b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*frames), "ns/frame")
			})
		}
	}
}

func BenchmarkClientMaskedFrameWrite(b *testing.B) {
	for _, size := range []int{1024, 1 << 20} {
		b.Run(fmt.Sprintf("%d", size), func(b *testing.B) {
			conn := &Conn{
				raw:    &writeProbeConn{},
				config: testDialerConfig(&Dialer{Options: Options{MaxFramePayload: uint64(size)}}),
			}
			message := frame.Frame{Fin: true, Opcode: frame.Binary, Payload: make([]byte, size)}
			b.ReportAllocs()
			b.SetBytes(int64(size))
			b.ResetTimer()
			for range b.N {
				if err := conn.sendFrameLocked(message); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkServerUnmaskedFrameWrite(b *testing.B) {
	for _, size := range []int{1024, 1 << 20} {
		b.Run(fmt.Sprintf("%d", size), func(b *testing.B) {
			conn := &Conn{
				raw:    &writeProbeConn{},
				config: testServerConfig(&Server{Options: Options{MaxFramePayload: uint64(size)}}),
			}
			message := frame.Frame{Fin: true, Opcode: frame.Binary, Payload: make([]byte, size)}
			b.ReportAllocs()
			b.SetBytes(int64(size))
			b.ResetTimer()
			for b.Loop() {
				if err := conn.sendFrameLocked(message); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
