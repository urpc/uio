package uws

import (
	"fmt"
	"testing"

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
}

func (c *echoPathConn) PeekChunk() []byte    { return c.inbound }
func (c *echoPathConn) InboundBuffered() int { return len(c.inbound) }
func (c *echoPathConn) Userdata() any        { return c.userdata }
func (c *echoPathConn) Flush() error         { return nil }
func (c *echoPathConn) YieldRead() error     { return nil }

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
				server := NewServer(echoPathHandler{})
				server.config = newServerConnConfig(server)
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
