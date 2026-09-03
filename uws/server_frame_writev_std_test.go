//go:build windows || stdio

package uws

import (
	"bytes"
	"testing"

	"github.com/urpc/uio"
	"github.com/urpc/uio/uws/internal/frame"
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

func TestStdServerFrameUsesSingleTransportCopy(t *testing.T) {
	payload := bytes.Repeat([]byte{0xa5}, 1<<20)
	raw := &stdFrameSink{dst: make([]byte, len(payload)+14)}
	conn := &Conn{
		raw: raw,
		config: testServerConfig(&Server{
			MaxFramePayload:  uint64(len(payload)),
			MaxOutboundBytes: -1,
		}),
	}
	if err := conn.sendFrameLocked(frame.Frame{Fin: true, Opcode: frame.Binary, Payload: payload}); err != nil {
		t.Fatal(err)
	}
	if raw.writes != 0 || raw.writevs != 1 {
		t.Fatalf("transport calls = Write:%d Writev:%d, want 0/1", raw.writes, raw.writevs)
	}
	if raw.copied != len(payload)+10 {
		t.Fatalf("transport copied bytes = %d, want %d", raw.copied, len(payload)+10)
	}
	if !bytes.Equal(raw.dst[10:raw.copied], payload) {
		t.Fatal("transport copy differs from caller payload")
	}
	raw.dst[10] ^= 0xff
	if payload[0] != 0xa5 {
		t.Fatal("transport retained and modified caller payload")
	}
}

func TestStdClientFrameKeepsMaskedCopy(t *testing.T) {
	payload := bytes.Repeat([]byte{0xa5}, 1<<20)
	raw := &stdFrameSink{dst: make([]byte, len(payload)+14)}
	conn := &Conn{
		raw:    raw,
		config: testDialerConfig(&Dialer{MaxFramePayload: uint64(len(payload)), MaxOutboundBytes: -1}),
	}
	if err := conn.sendFrameLocked(frame.Frame{Fin: true, Opcode: frame.Binary, Payload: payload}); err != nil {
		t.Fatal(err)
	}
	if raw.writes != 1 || raw.writevs != 0 {
		t.Fatalf("transport calls = Write:%d Writev:%d, want 1/0", raw.writes, raw.writevs)
	}
	if !bytes.Equal(payload, bytes.Repeat([]byte{0xa5}, len(payload))) {
		t.Fatal("client masking modified caller payload")
	}
}

func BenchmarkStdServerFrame1MiB(b *testing.B) {
	payload := make([]byte, 1<<20)
	raw := &stdFrameSink{dst: make([]byte, len(payload)+14)}
	conn := &Conn{
		raw: raw,
		config: testServerConfig(&Server{
			MaxFramePayload:  uint64(len(payload)),
			MaxOutboundBytes: -1,
		}),
	}
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
