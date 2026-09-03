package frame

import "testing"

func BenchmarkCompleteFrameParsing(b *testing.B) {
	cfg := &ParserConfig{MaxFramePayload: 1024}
	wire := Append(nil, Frame{Fin: true, Opcode: Binary, Payload: make([]byte, 1024)}, [4]byte{})
	emit := func(Frame) error { return nil }
	b.SetBytes(int64(len(wire)))
	b.ReportAllocs()

	b.Run("stateless", func(b *testing.B) {
		for b.Loop() {
			if _, _, complete, err := ParseFrame(wire, cfg); err != nil || !complete {
				b.Fatal(err)
			}
		}
	})
	b.Run("feed", func(b *testing.B) {
		parser := &Parser{}
		parser.Init(cfg)
		for b.Loop() {
			if _, err := parser.Feed(wire, emit); err != nil {
				b.Fatal(err)
			}
		}
	})
}
