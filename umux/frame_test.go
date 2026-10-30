/*
 * Copyright 2024 the urpc project
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *      https://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package umux

import (
	"bytes"
	"errors"
	"testing"
)

func encoded(h frameHeader) []byte {
	var b [headerSize]byte
	encodeHeader(b[:], h)
	return b[:]
}

func TestHeaderRoundTrip(t *testing.T) {
	cases := []frameHeader{
		{typ: typeData, flags: 0, streamID: 1, length: 0},
		{typ: typeData, flags: flagSYN, streamID: 1, length: 4096},
		{typ: typeData, flags: flagACK | flagFIN, streamID: 0xffffffff, length: 1<<31 - 1},
		{typ: typeWindowUpdate, flags: flagRST, streamID: 7, length: 65536},
		{typ: typePing, flags: flagSYN, streamID: 0, length: 0xdeadbeef},
		{typ: typeGoAway, flags: 0, streamID: 0, length: goAwayProtocol},
	}
	for _, want := range cases {
		want.version = protoVersion
		got, err := decodeHeader(encoded(want))
		if err != nil {
			t.Fatalf("decode(%v): %v", want, err)
		}
		if got != want {
			t.Fatalf("round trip = %+v, want %+v", got, want)
		}
	}
}

func TestDecodeHeaderErrors(t *testing.T) {
	cases := []struct {
		name string
		b    []byte
		want error
	}{
		{"short header", make([]byte, headerSize-1), errFrameShort},
		{"wrong version", func() []byte {
			b := encoded(frameHeader{typ: typeData, streamID: 1})
			b[0] = protoVersion + 1
			return b
		}(), errFrameVersion},
		{"unknown type", func() []byte {
			b := encoded(frameHeader{typ: typeData, streamID: 1})
			b[1] = typeGoAway + 1
			return b
		}(), errFrameType},
		{"undefined flag", func() []byte {
			b := encoded(frameHeader{typ: typeData, streamID: 1})
			b[3] = 0x80
			return b
		}(), errFrameFlags},
	}
	for _, tc := range cases {
		if _, err := decodeHeader(tc.b); !errors.Is(err, tc.want) {
			t.Fatalf("%s: err = %v, want %v", tc.name, err, tc.want)
		}
	}
}

func TestValidateFrame(t *testing.T) {
	const maxData = 64 << 10
	cases := []struct {
		name string
		h    frameHeader
		want error
	}{
		{"data within the limit", frameHeader{typ: typeData, streamID: 1, length: maxData}, nil},
		{"data beyond the limit", frameHeader{typ: typeData, streamID: 1, length: maxData + 1}, errFrameLength},
		{"window update names a stream", frameHeader{typ: typeWindowUpdate, streamID: 2, length: 1}, nil},
		{"window update without a stream", frameHeader{typ: typeWindowUpdate, streamID: 0, length: 1}, errFrameFlags},
		{"goaway names the session", frameHeader{typ: typeGoAway, streamID: 0, length: goAwayNormal}, nil},
		{"goaway names a stream", frameHeader{typ: typeGoAway, streamID: 3, length: goAwayNormal}, errFrameFlags},
	}
	for _, tc := range cases {
		if err := validateFrame(tc.h, maxData); !errors.Is(err, tc.want) {
			t.Fatalf("%s: err = %v, want %v", tc.name, err, tc.want)
		}
	}
}

// FuzzDecodeHeader checks that any accepted header re-encodes to the same
// bytes: a decoder that accepts something the encoder would not reproduce is a
// protocol split.
func FuzzDecodeHeader(f *testing.F) {
	f.Add(make([]byte, headerSize))
	f.Add(encoded(frameHeader{typ: typeData, flags: flagSYN, streamID: 1, length: 4}))
	f.Add(encoded(frameHeader{typ: typeWindowUpdate, flags: flagFIN, streamID: 9, length: 1024}))
	f.Fuzz(func(t *testing.T, b []byte) {
		h, err := decodeHeader(b)
		if err != nil {
			return
		}
		var out [headerSize]byte
		encodeHeader(out[:], h)
		if !bytes.Equal(out[:], b[:headerSize]) {
			t.Fatalf("re-encode %x != input %x", out[:], b[:headerSize])
		}
	})
}
