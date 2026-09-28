package frame

import "testing"

// A borrowed payload must not reach into the frames after it: a handler that
// appends to one would otherwise overwrite input not yet parsed.
func TestParseFramePayloadCapacityEndsWithFrame(t *testing.T) {
	cfg := &ParserConfig{ExpectMask: true, MaxFramePayload: 1 << 20}
	wire := Append(nil, Frame{Fin: true, Opcode: Binary, Masked: true, Payload: []byte("first")}, [4]byte{1, 2, 3, 4})
	wire = Append(wire, Frame{Fin: true, Opcode: Binary, Masked: true, Payload: []byte("second")}, [4]byte{5, 6, 7, 8})
	first, size, complete, err := ParseFrame(wire, cfg)
	if err != nil || !complete {
		t.Fatalf("ParseFrame = %v, %v", complete, err)
	}
	if cap(first.Payload) != len(first.Payload) {
		t.Fatalf("payload capacity = %d, want %d", cap(first.Payload), len(first.Payload))
	}
	_ = append(first.Payload, "overwrite"...)
	second, _, complete, err := ParseFrame(wire[size:], cfg)
	if err != nil || !complete || string(second.Payload) != "second" {
		t.Fatalf("second frame = %q, %v, %v", second.Payload, complete, err)
	}
}
