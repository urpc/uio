//go:build windows || stdio

package uws

import (
	"github.com/urpc/uio"
	"github.com/urpc/uio/uws/internal/frame"
	"testing"
)

type stdFrameSink struct {
	uio.Conn
	dst     []byte
	writes  int
	writevs int
	copied  int
}

func (s *stdFrameSink) Write(payload []byte) (int, error) {
	s.writes++
	n := copy(s.dst, payload)
	s.copied += n
	return n, nil
}

func (s *stdFrameSink) Writev(vec [][]byte) (int, error) {
	s.writevs++
	written := 0
	for _, payload := range vec {
		written += copy(s.dst[written:], payload)
	}
	s.copied += written
	return written, nil
}

func (s *stdFrameSink) WriteOwned(buffer *uio.Buffer) (int, error) {
	s.writes++
	written := copy(s.dst, buffer.Bytes())
	s.copied += written
	uio.ReleaseBuffer(buffer)
	return written, nil
}

func BenchmarkStdServerFrame1MiB(b *testing.B) {
	payload := make([]byte, 1<<20)
	raw := &stdFrameSink{dst: make([]byte, len(payload)+14)}
	conn := &Conn{raw: raw, config: testServerConfig(&Server{Options: Options{MaxFramePayload: uint64(len(payload))}})}
	message := frame.Frame{Fin: true, Opcode: frame.Binary, Payload: payload}
	b.ReportAllocs()
	b.SetBytes(int64(len(payload)))
	b.ResetTimer()
	for range b.N {
		if err := conn.sendFrameLocked(message); err != nil {
			b.Fatal(err)
		}
	}
}
