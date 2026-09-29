package uws

import (
	"fmt"
	"testing"

	"github.com/urpc/uio/uws/internal/frame"
)

func BenchmarkClientMaskedFrameWrite(b *testing.B) {
	for _, size := range []int{1024, 1 << 20} {
		b.Run(fmt.Sprintf("%d", size), func(b *testing.B) {
			conn := &Conn{
				raw: &writeProbeConn{},
				config: testDialerConfig(&Dialer{
					MaxFramePayload: uint64(size),
				}),
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
				raw: &writeProbeConn{},
				config: testServerConfig(&Server{
					MaxFramePayload: uint64(size),
				}),
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
