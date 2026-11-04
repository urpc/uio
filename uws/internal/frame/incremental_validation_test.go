package frame

import (
	"errors"
	"testing"
)

// Validation must not depend on whether TCP delivered a header in one read or
// split it into bytes; malformed headers must also fail before allocating a body.
func TestIncrementalHeaderValidationMatchesCompleteParsing(t *testing.T) {
	for _, tc := range []struct {
		name string
		wire []byte
		cfg  ParserConfig
		want error
	}{
		{"reserved-bit", []byte{0xa2, 0}, ParserConfig{}, ErrProtocol},
		{"unnegotiated-compression", []byte{0xc2, 0}, ParserConfig{}, ErrProtocol},
		{"reserved-opcode", []byte{0x83, 0}, ParserConfig{}, ErrProtocol},
		{"missing-mask", []byte{0x82, 0}, ParserConfig{ExpectMask: true}, ErrProtocol},
		{"compressed-control", []byte{0xc9, 0}, ParserConfig{AllowRSV1: true}, ErrProtocol},
		{"fragmented-control", []byte{0x09, 0}, ParserConfig{}, ErrProtocol},
		{"oversized-control", []byte{0x89, 126}, ParserConfig{}, ErrProtocol},
		{"noncanonical-16", []byte{0x82, 126, 0, 125}, ParserConfig{}, ErrProtocol},
		{"noncanonical-64", []byte{0x82, 127, 0, 0, 0, 0, 0, 0, 0, 126}, ParserConfig{}, ErrProtocol},
		{"reserved-length-bit", []byte{0x82, 127, 0x80, 0, 0, 0, 0, 0, 0, 0}, ParserConfig{}, ErrProtocol},
		{"body-limit", []byte{0x82, 126, 1, 0}, ParserConfig{MaxFramePayload: 255}, ErrMessageTooBig},
		{"slice-overflow", []byte{0x82, 127, 0x7f, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}, ParserConfig{}, ErrMessageTooBig},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := NewParser(tc.cfg)
			defer p.Reset()
			var got error
			for _, b := range tc.wire {
				_, got = p.Feed([]byte{b}, func(Frame) error { t.Fatal("invalid header emitted a frame"); return nil })
				if got != nil {
					break
				}
			}
			if !errors.Is(got, tc.want) {
				t.Fatalf("split header error=%v, want %v", got, tc.want)
			}
			if p.payloadBuf != nil {
				t.Fatal("invalid header allocated a payload buffer")
			}
		})
	}
}

func TestIncrementalSinkFailureConsumesOnlyCompletedFrame(t *testing.T) {
	cause := errors.New("consumer stopped")
	first := Append(nil, Frame{Fin: true, Opcode: Binary, Payload: []byte("first")}, [4]byte{})
	next := Append(nil, Frame{Fin: true, Opcode: Binary, Payload: []byte("next")}, [4]byte{})
	for _, split := range []int{0, 1, 3} {
		p := NewParser(ParserConfig{})
		if split > 0 {
			if _, err := p.Feed(first[:split], func(Frame) error { t.Fatal("incomplete frame delivered"); return nil }); err != nil {
				t.Fatal(err)
			}
		}
		n, err := p.Feed(append(append([]byte(nil), first[split:]...), next...), func(f Frame) error {
			if string(f.Payload) != "first" {
				t.Fatalf("payload=%q", f.Payload)
			}
			return cause
		})
		if n != len(first)-split || !errors.Is(err, cause) {
			t.Fatalf("split=%d consumed=%d err=%v", split, n, err)
		}
		if !p.AtFrameBoundary() {
			t.Fatal("completed frame retained parser state")
		}
		if n, err := p.FinishFrame(next, emitFunc(func(Frame) error { t.Fatal("FinishFrame started a new frame"); return nil })); err != nil || n != 0 {
			t.Fatal(err)
		}
		p.Reset()
	}
}

func TestSplitInvalidClosePayloadNeverReachesSink(t *testing.T) {
	wire := []byte{0x88, 1, 0xff}
	p := NewParser(ParserConfig{})
	for i, b := range wire {
		_, err := p.Feed([]byte{b}, func(Frame) error { t.Fatal("invalid close emitted"); return nil })
		if i == len(wire)-1 && !errors.Is(err, ErrProtocol) {
			t.Fatal(err)
		}
	}
	if !p.AtFrameBoundary() {
		t.Fatal("invalid completed frame retained parser state")
	}
}
