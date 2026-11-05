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

// Package umux multiplexes bidirectional byte streams over one uio.Conn.
//
// The frame layout keeps the wire format of hashicorp/yamux v1 (12-byte
// header, big-endian, version 0); the session and stream semantics follow
// uio's turn model rather than yamux's blocking goroutines.
package umux

import (
	"encoding/binary"
	"errors"
)

const (
	// protoVersion is the only wire version. 0 keeps the frames compatible
	// with yamux v1.
	protoVersion uint8 = 0

	// headerSize is the fixed frame header size: version, type, flags,
	// stream id and length.
	headerSize = 12
)

// Frame types.
const (
	typeData uint8 = iota
	typeWindowUpdate
	typePing
	typeGoAway
	// typeSynMetadata opens a stream like a WindowUpdate|SYN frame and carries
	// the opener's opaque metadata. It is a umux extension outside yamux v1's
	// type space, so it is only sent when the application attaches metadata
	// (Session.OpenWith); a yamux peer rejects it.
	typeSynMetadata
)

// Frame flags. SYN may carry data and opens a stream; ACK answers it. FIN
// half-closes the sender's direction and may carry data. RST resets the
// stream unconditionally.
const (
	flagSYN uint16 = 1 << iota
	flagACK
	flagFIN
	flagRST
)

const (
	// allFlags is every defined flag; anything else is a protocol error.
	allFlags = flagSYN | flagACK | flagFIN | flagRST
)

// GoAway causes, echoed as the frame's length field.
const (
	goAwayNormal uint32 = iota
	goAwayProtocol
	goAwayInternal
)

var (
	errFrameShort   = errors.New("umux: short frame header")
	errFrameVersion = errors.New("umux: invalid frame version")
	errFrameType    = errors.New("umux: invalid frame type")
	errFrameFlags   = errors.New("umux: invalid frame flags")
	errFrameLength  = errors.New("umux: frame length exceeds the limit")
)

// frameHeader is one frame's fixed header, decoded.
type frameHeader struct {
	version  uint8
	typ      uint8
	flags    uint16
	streamID uint32
	length   uint32
}

// decodeHeader parses the frame header at the start of b, which must hold at
// least headerSize bytes.
func decodeHeader(b []byte) (frameHeader, error) {
	if len(b) < headerSize {
		return frameHeader{}, errFrameShort
	}
	h := frameHeader{
		version:  b[0],
		typ:      b[1],
		flags:    binary.BigEndian.Uint16(b[2:4]),
		streamID: binary.BigEndian.Uint32(b[4:8]),
		length:   binary.BigEndian.Uint32(b[8:12]),
	}
	if h.version != protoVersion {
		return frameHeader{}, errFrameVersion
	}
	if h.typ > typeSynMetadata {
		return frameHeader{}, errFrameType
	}
	if h.flags&^allFlags != 0 {
		return frameHeader{}, errFrameFlags
	}
	return h, nil
}

// encodeHeader writes h into the first headerSize bytes of dst. The version
// is always the protocol version.
func encodeHeader(dst []byte, h frameHeader) {
	dst[0] = protoVersion
	dst[1] = h.typ
	binary.BigEndian.PutUint16(dst[2:4], h.flags)
	binary.BigEndian.PutUint32(dst[4:8], h.streamID)
	binary.BigEndian.PutUint32(dst[8:12], h.length)
}

// validateFrame applies the per-type bounds before a session acts on a
// frame: data frames carry at most maxData payload bytes, every frame's
// flags are a combination its type gives meaning to, window updates and
// data name a stream, a GoAway names the session itself, and a metadata SYN
// carries its four-byte window delta plus at most maxMeta opaque bytes.
//
// SYN or ACK may share a frame with FIN, as yamux uses when opening or
// acknowledging a stream while half-closing its sending direction. RST stays
// exclusive so a hard reset cannot be confused with a data or FIN delivery.
func validateFrame(h frameHeader, maxData, maxMeta uint32) error {
	switch h.typ {
	case typeData:
		// SYN and ACK may ride on data in yamux. FIN may ride with either
		// open or acknowledge; RST remains a standalone hard reset.
		switch h.flags {
		case 0, flagSYN, flagFIN, flagSYN | flagFIN,
			flagACK, flagACK | flagFIN, flagRST:
		default:
			return errFrameFlags
		}
		if h.streamID == 0 {
			return errFrameFlags
		}
		if h.length > maxData {
			return errFrameLength
		}
	case typeWindowUpdate:
		// SYN and ACK may ride on a window update, and either may carry FIN.
		// A plain FIN may carry a delta; RST remains a standalone reset.
		switch h.flags {
		case 0, flagSYN, flagSYN | flagFIN,
			flagACK, flagACK | flagFIN, flagFIN, flagRST:
		default:
			return errFrameFlags
		}
		if h.streamID == 0 {
			return errFrameFlags
		}
	case typePing:
		// A probe or its answer, on the session.
		switch h.flags {
		case flagSYN, flagACK:
		default:
			return errFrameFlags
		}
	case typeGoAway:
		if h.flags != 0 || h.streamID != 0 {
			return errFrameFlags
		}
	case typeSynMetadata:
		if h.flags != flagSYN || h.streamID == 0 {
			return errFrameFlags
		}
		// The payload is the opener's window delta (4 bytes) followed by its
		// metadata, whose bytes this end never interprets.
		if h.length < 4 || h.length-4 > maxMeta {
			return errFrameLength
		}
	}
	return nil
}
