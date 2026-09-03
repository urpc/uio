// Package frame implements the wire-level WebSocket frame codec.
package frame

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"unicode/utf8"

	"github.com/urpc/uio/internal/pool"
)

type OpCode byte

const (
	Continuation OpCode = 0x0
	Text         OpCode = 0x1
	Binary       OpCode = 0x2
	Close        OpCode = 0x8
	Ping         OpCode = 0x9
	Pong         OpCode = 0xa
)

var (
	ErrProtocol      = errors.New("websocket: protocol error")
	ErrMessageTooBig = errors.New("websocket: message too big")
	ErrInvalidUTF8   = errors.New("websocket: invalid utf-8")
)

const (
	initialPayloadBuffer        = 32 << 10
	maxPooledIncrementalPayload = 64 << 10
)

type parserPayloadBuffer struct {
	data []byte
}

var parserPayloadPool = pool.New[*parserPayloadBuffer](maxPooledIncrementalPayload)

type Frame struct {
	Fin      bool
	RSV1     bool
	Opcode   OpCode
	Masked   bool
	Borrowed bool // When true, Payload is valid only during the Feed callback.
	Payload  []byte
}

type ParserConfig struct {
	// ExpectMask is true for a server receiving client frames and false for a
	// client receiving server frames.
	ExpectMask bool
	AllowRSV1  bool
	// A zero limit means the largest representable Go slice.
	MaxFramePayload uint64
}

func parserExpectMask(cfg *ParserConfig) bool {
	return cfg != nil && cfg.ExpectMask
}

func parserAllowRSV1(cfg *ParserConfig) bool {
	return cfg != nil && cfg.AllowRSV1
}

func parserMaxFramePayload(cfg *ParserConfig) uint64 {
	if cfg == nil || cfg.MaxFramePayload == 0 {
		return uint64(maxInt())
	}
	return cfg.MaxFramePayload
}

type Parser struct {
	cfg         *ParserConfig // shared immutable owner configuration
	header      [14]byte
	headerLen   int
	headerNeed  int
	fin         bool
	rsv1        bool
	opcode      OpCode
	masked      bool
	payloadLen  uint64
	payloadRead uint64
	maskKey     [4]byte
	maskOffset  int
	payload     []byte
	payloadBuf  *parserPayloadBuffer
	payloadSize int
}

func NewParser(cfg ParserConfig) *Parser {
	p := &Parser{}
	p.Init(&cfg)
	return p
}

// Init resets p and assigns an immutable configuration. A nil configuration
// uses the protocol defaults and is useful before returning p to a pool.
func (p *Parser) Init(cfg *ParserConfig) {
	p.Reset()
	p.cfg = cfg
}

func (p *Parser) Reset() {
	p.releasePayload()
	p.resetState()
}

func (p *Parser) resetState() {
	p.headerLen = 0
	p.headerNeed = 2
	p.fin = false
	p.rsv1 = false
	p.opcode = 0
	p.masked = false
	p.payloadLen = 0
	p.payloadRead = 0
	p.maskKey = [4]byte{}
	p.maskOffset = 0
	p.payload = nil
	p.payloadBuf = nil
	p.payloadSize = 0
}

// AtFrameBoundary reports whether p has no incremental frame state.
func (p *Parser) AtFrameBoundary() bool { return p.headerLen == 0 }

// Feed consumes as much of src as possible. It preserves incomplete frame
// state between calls and invokes emit once for every complete frame.
func (p *Parser) Feed(src []byte, emit func(Frame) error) (int, error) {
	if emit == nil {
		return 0, errors.New("websocket: nil frame callback")
	}
	consumed := 0
	for consumed < len(src) {
		if p.headerLen == 0 {
			frame, size, complete, err := ParseFrame(src[consumed:], p.cfg)
			if err != nil {
				return consumed, err
			}
			if complete {
				if err := emit(frame); err != nil {
					return consumed + size, err
				}
				consumed += size
				continue
			}
		}
		if p.headerLen < p.headerNeed {
			n := copy(p.header[p.headerLen:p.headerNeed], src[consumed:])
			p.headerLen += n
			consumed += n
			if p.headerLen < p.headerNeed {
				continue
			}
			if p.headerNeed == 2 {
				if err := p.prepareHeader(); err != nil {
					return consumed, err
				}
				if p.headerLen < p.headerNeed {
					continue
				}
			}
			if p.payload == nil && p.payloadLen > 0 {
				if err := p.finishHeader(); err != nil {
					return consumed, err
				}
			}
		}

		if p.headerLen < p.headerNeed {
			continue
		}
		if p.payload == nil && p.payloadLen == 0 {
			if err := p.finishHeader(); err != nil {
				return consumed, err
			}
		}

		remaining := int(p.payloadLen - p.payloadRead)
		if remaining > 0 {
			n := len(src) - consumed
			if n > remaining {
				n = remaining
			}
			start := len(p.payload)
			p.growPayload(start + n)
			p.payload = append(p.payload, src[consumed:consumed+n]...)
			unmask(p.payload[start:], p.maskKey, p.maskOffset)
			p.maskOffset = (p.maskOffset + n) & 3
			p.payloadRead += uint64(n)
			consumed += n
			if p.payloadRead < p.payloadLen {
				continue
			}
		}

		f := Frame{
			Fin:      p.fin,
			RSV1:     p.rsv1,
			Opcode:   p.opcode,
			Masked:   p.masked,
			Borrowed: p.payloadBuf != nil,
			Payload:  p.payload,
		}
		payloadBuf, payloadSize := p.payloadBuf, p.payloadSize
		p.payload = nil
		p.payloadBuf = nil
		p.payloadSize = 0
		p.resetState()
		if f.Opcode == Close {
			if err := ValidateClosePayload(f.Payload); err != nil {
				releaseParserPayload(payloadBuf, payloadSize)
				return consumed, err
			}
		}
		if err := emitParserFrame(emit, f, payloadBuf, payloadSize); err != nil {
			return consumed, err
		}
	}
	return consumed, nil
}

// fastFrame handles a complete frame already present in src without creating a
// payload buffer. The caller owns src for the duration of emit; incremental
// frames continue through the stateful path below.
// ParseFrame parses one complete frame directly from src without retaining
// state. complete is false when src does not contain the entire frame.
func ParseFrame(src []byte, cfg *ParserConfig) (Frame, int, bool, error) {
	if len(src) < 2 {
		return Frame{}, 0, false, nil
	}
	b0, b1 := src[0], src[1]
	fin := b0&0x80 != 0
	rsv1 := b0&0x40 != 0
	opcode := OpCode(b0 & 0x0f)
	masked := b1&0x80 != 0
	lengthCode := b1 & 0x7f
	if b0&0x30 != 0 {
		return Frame{}, 0, true, protocolError("reserved bits RSV2/RSV3 are set")
	}
	if rsv1 && !parserAllowRSV1(cfg) {
		return Frame{}, 0, true, protocolError("unexpected RSV1")
	}
	if !validOpcode(opcode) {
		return Frame{}, 0, true, protocolError("invalid opcode 0x%x", byte(opcode))
	}
	if masked != parserExpectMask(cfg) {
		return Frame{}, 0, true, protocolError("invalid mask bit")
	}
	if opcode >= 0x8 && (!fin || lengthCode >= 126 || rsv1) {
		return Frame{}, 0, true, protocolError("invalid control frame")
	}
	headerSize := 2
	switch lengthCode {
	case 126:
		headerSize += 2
	case 127:
		headerSize += 8
	default:
	}
	if masked {
		headerSize += 4
	}
	if len(src) < headerSize {
		return Frame{}, 0, false, nil
	}
	var payloadLen uint64
	switch lengthCode {
	case 126:
		payloadLen = uint64(binary.BigEndian.Uint16(src[2:4]))
		if payloadLen < 126 {
			return Frame{}, 0, true, protocolError("non-canonical payload length")
		}
	case 127:
		if src[2]&0x80 != 0 {
			return Frame{}, 0, true, protocolError("payload length uses reserved bit")
		}
		payloadLen = binary.BigEndian.Uint64(src[2:10])
		if payloadLen < 65536 {
			return Frame{}, 0, true, protocolError("non-canonical payload length")
		}
	default:
		payloadLen = uint64(lengthCode)
	}
	if payloadLen > parserMaxFramePayload(cfg) || payloadLen > uint64(maxInt()-headerSize) {
		return Frame{}, 0, true, ErrMessageTooBig
	}
	total := headerSize + int(payloadLen)
	if len(src) < total {
		return Frame{}, 0, false, nil
	}
	var maskKey [4]byte
	if masked {
		copy(maskKey[:], src[headerSize-4:headerSize])
	}
	payload := src[headerSize:total]
	if masked {
		unmask(payload, maskKey, 0)
	}
	if opcode == Close {
		if err := ValidateClosePayload(payload); err != nil {
			return Frame{}, 0, true, err
		}
	}
	return Frame{Fin: fin, RSV1: rsv1, Opcode: opcode, Masked: masked, Borrowed: true, Payload: payload}, total, true, nil
}

func (p *Parser) prepareHeader() error {
	b0, b1 := p.header[0], p.header[1]
	p.fin = b0&0x80 != 0
	p.rsv1 = b0&0x40 != 0
	p.opcode = OpCode(b0 & 0x0f)
	p.masked = b1&0x80 != 0
	lengthCode := b1 & 0x7f

	if b0&0x30 != 0 {
		return protocolError("reserved bits RSV2/RSV3 are set")
	}
	if p.rsv1 && !parserAllowRSV1(p.cfg) {
		return protocolError("unexpected RSV1")
	}
	if !validOpcode(p.opcode) {
		return protocolError("invalid opcode 0x%x", byte(p.opcode))
	}
	if p.masked != parserExpectMask(p.cfg) {
		return protocolError("invalid mask bit")
	}
	if p.opcode >= 0x8 {
		if p.rsv1 {
			return protocolError("RSV1 on control frame")
		}
		if !p.fin || lengthCode >= 126 {
			return protocolError("invalid control frame")
		}
	}

	switch lengthCode {
	case 126:
		p.headerNeed = 4
	case 127:
		p.headerNeed = 10
	default:
		p.payloadLen = uint64(lengthCode)
		p.headerNeed = 2
	}
	if p.masked {
		p.headerNeed += 4
	}
	return nil
}

func (p *Parser) finishHeader() error {
	lengthCode := p.header[1] & 0x7f
	switch lengthCode {
	case 126:
		p.payloadLen = uint64(binary.BigEndian.Uint16(p.header[2:4]))
		if p.payloadLen < 126 {
			return protocolError("non-canonical payload length")
		}
	case 127:
		if p.header[2]&0x80 != 0 {
			return protocolError("payload length uses reserved bit")
		}
		p.payloadLen = binary.BigEndian.Uint64(p.header[2:10])
		if p.payloadLen < 65536 {
			return protocolError("non-canonical payload length")
		}
	}
	if p.payloadLen > parserMaxFramePayload(p.cfg) || p.payloadLen > uint64(maxInt()-p.headerNeed) {
		return ErrMessageTooBig
	}
	maskOffset := 2
	if lengthCode == 126 {
		maskOffset = 4
	} else if lengthCode == 127 {
		maskOffset = 10
	}
	if p.masked {
		copy(p.maskKey[:], p.header[maskOffset:maskOffset+4])
	}
	if p.payloadLen > 0 {
		p.acquirePayload(initialPayloadCapacity(p.payloadLen))
	} else {
		p.payload = []byte{}
	}
	p.payloadRead = 0
	p.maskOffset = 0
	return nil
}

func initialPayloadCapacity(payloadLen uint64) int {
	if payloadLen < initialPayloadBuffer {
		return int(payloadLen)
	}
	return initialPayloadBuffer
}

func (p *Parser) growPayload(required int) {
	if required <= cap(p.payload) {
		return
	}
	capacity := cap(p.payload) * 2
	if capacity < initialPayloadBuffer {
		capacity = initialPayloadBuffer
	}
	if capacity < required {
		capacity = required
	}
	if capacity > int(p.payloadLen) {
		capacity = int(p.payloadLen)
	}
	buffer, poolSize := acquireParserPayload(capacity)
	buffer.data = append(buffer.data[:0], p.payload...)
	oldBuffer, oldSize := p.payloadBuf, p.payloadSize
	p.payload = buffer.data
	p.payloadBuf = buffer
	p.payloadSize = poolSize
	releaseParserPayload(oldBuffer, oldSize)
}

func (p *Parser) acquirePayload(capacity int) {
	buffer, poolSize := acquireParserPayload(capacity)
	p.payload = buffer.data[:0]
	p.payloadBuf = buffer
	p.payloadSize = poolSize
}

func (p *Parser) releasePayload() {
	releaseParserPayload(p.payloadBuf, p.payloadSize)
	p.payload = nil
	p.payloadBuf = nil
	p.payloadSize = 0
}

func acquireParserPayload(capacity int) (*parserPayloadBuffer, int) {
	if capacity > maxPooledIncrementalPayload {
		return &parserPayloadBuffer{data: make([]byte, 0, capacity)}, capacity
	}
	buffer, poolSize := parserPayloadPool.Get(capacity)
	if buffer == nil {
		buffer = &parserPayloadBuffer{data: make([]byte, 0, poolSize)}
	} else {
		buffer.data = buffer.data[:0]
	}
	return buffer, poolSize
}

func releaseParserPayload(buffer *parserPayloadBuffer, poolSize int) {
	if buffer == nil {
		return
	}
	buffer.data = buffer.data[:0]
	parserPayloadPool.Put(buffer, poolSize)
}

func emitParserFrame(emit func(Frame) error, frame Frame, buffer *parserPayloadBuffer, poolSize int) error {
	defer releaseParserPayload(buffer, poolSize)
	return emit(frame)
}

func validOpcode(op OpCode) bool {
	switch op {
	case Continuation, Text, Binary, Close, Ping, Pong:
		return true
	default:
		return false
	}
}

func protocolError(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrProtocol, fmt.Sprintf(format, args...))
}

func maxInt() int {
	return int(^uint(0) >> 1)
}

type Message struct {
	Opcode     OpCode
	Payload    []byte
	Compressed bool
	Borrowed   bool
}

// AssemblerConfig contains immutable message assembly limits.
type AssemblerConfig struct {
	MaxMessage           uint64
	MaxCompressedPayload uint64
	ValidateUTF8         bool
}

// Assembler validates data-frame sequencing and joins fragmented messages.
type Assembler struct {
	cfg        *AssemblerConfig
	fragmented bool
	opcode     OpCode
	compressed bool
	payload    []byte
}

func NewAssembler(maxMessage uint64) *Assembler {
	return NewAssemblerWithLimits(maxMessage, maxMessage)
}

func NewAssemblerWithLimits(maxMessage, maxCompressedPayload uint64) *Assembler {
	return &Assembler{cfg: &AssemblerConfig{
		MaxMessage:           maxMessage,
		MaxCompressedPayload: maxCompressedPayload,
		ValidateUTF8:         true,
	}}
}

// Init resets a and assigns an immutable configuration. A nil configuration is
// useful before returning an assembler to a pool.
func (a *Assembler) Init(cfg *AssemblerConfig) {
	a.Reset()
	a.cfg = cfg
}

func (a *Assembler) Reset() {
	a.fragmented = false
	a.opcode = 0
	a.compressed = false
	a.payload = nil
}

// AtMessageBoundary reports whether no fragmented message is being assembled.
func (a *Assembler) AtMessageBoundary() bool { return !a.fragmented }

// AcceptSingle applies message-level rules to a control frame or complete data
// frame without retaining assembly state.
func AcceptSingle(f Frame, cfg *AssemblerConfig, onControl func(Frame) error, onMessage func(Message) error) error {
	if f.Opcode >= 0x8 {
		if f.RSV1 {
			return protocolError("RSV1 on control frame")
		}
		if onControl == nil {
			return errors.New("websocket: nil control callback")
		}
		return onControl(f)
	}
	if onMessage == nil {
		return errors.New("websocket: nil message callback")
	}
	if f.Opcode != Text && f.Opcode != Binary {
		return protocolError("invalid data opcode 0x%x", byte(f.Opcode))
	}
	if !f.Fin {
		return protocolError("fragmented data frame requires an assembler")
	}
	a := Assembler{cfg: cfg}
	if a.exceedsPayloadLimit(len(f.Payload), f.RSV1) {
		return ErrMessageTooBig
	}
	return a.emitMessage(Message{
		Opcode: f.Opcode, Payload: f.Payload, Compressed: f.RSV1, Borrowed: f.Borrowed,
	}, onMessage)
}

// Accept applies message-level rules. Control frames are delivered through
// onControl and complete data messages through onMessage.
func (a *Assembler) Accept(f Frame, onControl func(Frame) error, onMessage func(Message) error) error {
	if f.Opcode >= 0x8 {
		if f.RSV1 {
			return protocolError("RSV1 on control frame")
		}
		if onControl == nil {
			return errors.New("websocket: nil control callback")
		}
		return onControl(f)
	}
	if onMessage == nil {
		return errors.New("websocket: nil message callback")
	}

	switch f.Opcode {
	case Text, Binary:
		if a.fragmented {
			return protocolError("new data frame while fragmented message is open")
		}
		// Compressed frames use an independent wire-size limit. The caller
		// enforces maxMessage on the decoded payload.
		if a.exceedsPayloadLimit(len(f.Payload), f.RSV1) {
			return ErrMessageTooBig
		}
		if f.Fin {
			return a.emitMessage(Message{Opcode: f.Opcode, Payload: f.Payload, Compressed: f.RSV1, Borrowed: f.Borrowed}, onMessage)
		}
		a.fragmented = true
		a.opcode = f.Opcode
		a.compressed = f.RSV1
		a.payload = append(a.payload[:0], f.Payload...)
		return nil
	case Continuation:
		if !a.fragmented {
			return protocolError("continuation frame without fragmented message")
		}
		if f.RSV1 {
			return protocolError("RSV1 on continuation frame")
		}
		if a.exceedsPayloadLimit(len(f.Payload), a.compressed) {
			return ErrMessageTooBig
		}
		a.payload = append(a.payload, f.Payload...)
		if !f.Fin {
			return nil
		}
		message := Message{Opcode: a.opcode, Payload: a.payload, Compressed: a.compressed}
		a.Reset()
		return a.emitMessage(message, onMessage)
	default:
		return protocolError("invalid data opcode 0x%x", byte(f.Opcode))
	}
}

func (a *Assembler) exceedsPayloadLimit(incoming int, compressed bool) bool {
	limit := assemblerMaxMessage(a.cfg)
	if compressed {
		limit = assemblerMaxCompressedPayload(a.cfg)
	}
	current := uint64(len(a.payload))
	n := uint64(incoming)
	return current > limit || n > limit-current
}

func (a *Assembler) emitMessage(m Message, onMessage func(Message) error) error {
	if assemblerValidateUTF8(a.cfg) && m.Opcode == Text && !m.Compressed && !utf8.Valid(m.Payload) {
		return ErrInvalidUTF8
	}
	return onMessage(m)
}

func assemblerMaxMessage(cfg *AssemblerConfig) uint64 {
	if cfg == nil || cfg.MaxMessage == 0 {
		return uint64(maxInt())
	}
	return cfg.MaxMessage
}

func assemblerMaxCompressedPayload(cfg *AssemblerConfig) uint64 {
	if cfg == nil || cfg.MaxCompressedPayload == 0 {
		return uint64(maxInt())
	}
	return cfg.MaxCompressedPayload
}

func assemblerValidateUTF8(cfg *AssemblerConfig) bool {
	return cfg == nil || cfg.ValidateUTF8
}

func IsControl(op OpCode) bool { return op >= 0x8 }

func Append(dst []byte, f Frame, maskKey [4]byte) []byte {
	dst = AppendHeader(dst, f, maskKey)
	start := len(dst)
	dst = append(dst, f.Payload...)
	if f.Masked {
		unmask(dst[start:], maskKey, 0)
	}
	return dst
}

func AppendHeader(dst []byte, f Frame, maskKey [4]byte) []byte {
	first := byte(f.Opcode)
	if f.Fin {
		first |= 0x80
	}
	if f.RSV1 {
		first |= 0x40
	}
	dst = append(dst, first)

	length := len(f.Payload)
	maskBit := byte(0)
	if f.Masked {
		maskBit = 0x80
	}
	switch {
	case length < 126:
		dst = append(dst, maskBit|byte(length))
	case uint64(length) <= math.MaxUint16:
		dst = append(dst, maskBit|126, 0, 0)
		binary.BigEndian.PutUint16(dst[len(dst)-2:], uint16(length))
	default:
		dst = append(dst, maskBit|127, 0, 0, 0, 0, 0, 0, 0, 0)
		binary.BigEndian.PutUint64(dst[len(dst)-8:], uint64(length))
	}
	if f.Masked {
		dst = append(dst, maskKey[:]...)
	}
	return dst
}

func ValidateClosePayload(payload []byte) error {
	if len(payload) == 0 {
		return nil
	}
	if len(payload) == 1 {
		return protocolError("close payload has odd length")
	}
	code := binary.BigEndian.Uint16(payload[:2])
	if !validCloseCode(code) {
		return protocolError("invalid close code %d", code)
	}
	if !utf8.Valid(payload[2:]) {
		return ErrInvalidUTF8
	}
	return nil
}

func CloseCode(payload []byte) uint16 {
	if len(payload) < 2 {
		return 1005
	}
	return binary.BigEndian.Uint16(payload[:2])
}

func validCloseCode(code uint16) bool {
	switch code {
	case 1000, 1001, 1002, 1003, 1007, 1008, 1009, 1010, 1011:
		return true
	default:
		return code >= 3000 && code <= 4999
	}
}
