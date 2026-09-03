package uws

import (
	"unicode/utf8"

	"github.com/urpc/uio/uws/internal/compress"
	"github.com/urpc/uio/uws/internal/frame"
)

// BeginMessage starts a fragmented message and holds the connection's write
// ownership until Writer.Close returns. It is non-blocking with respect to
// the socket; callers must close a successful writer. A Write error aborts the
// connection and releases write ownership immediately. With compression enabled,
// writes are compressed incrementally and emitted as continuation frames, so
// large messages do not need a second full plaintext buffer.
func (c *Conn) BeginMessage(typ MessageType) (*Writer, error) {
	if typ != TextMessage && typ != BinaryMessage {
		return nil, frame.ErrProtocol
	}
	if !c.opened.Load() {
		return nil, ErrNotReady
	}
	if c.closed.Load() || c.closing.Load() {
		return nil, ErrClosed
	}
	c.writeMu.Lock()
	if c.closed.Load() || c.closing.Load() {
		c.writeMu.Unlock()
		return nil, ErrClosed
	}
	opcode := frame.Text
	if typ == BinaryMessage {
		opcode = frame.Binary
	}
	writer := &Writer{conn: c, opcode: opcode, first: true}
	if c.compression != nil && c.compression.encoder != nil {
		stream, err := c.compression.encoder.NewStream(writer.emitCompressed)
		if err != nil {
			c.writeMu.Unlock()
			return nil, err
		}
		writer.stream = stream
	}
	return writer, nil
}

// Writer incrementally sends one fragmented WebSocket message.
type Writer struct {
	conn        *Conn
	opcode      frame.OpCode
	first       bool
	closed      bool
	failure     error
	bytes       uint64
	stream      *compress.StreamEncoder
	textTail    [utf8.UTFMax - 1]byte
	textTailLen int
	textInvalid bool
}

// Write appends payload to the message. Any error aborts the connection; later
// calls return the first error without emitting more frames.
func (w *Writer) Write(payload []byte) (int, error) {
	if w == nil || w.conn == nil {
		return 0, ErrWriterClosed
	}
	if w.failure != nil {
		return 0, w.failure
	}
	if w.closed {
		return 0, ErrWriterClosed
	}
	maxMessage := w.conn.maxMessageSize()
	if w.bytes > maxMessage || uint64(len(payload)) > maxMessage-w.bytes {
		return 0, w.fail(frame.ErrMessageTooBig)
	}
	if len(payload) == 0 {
		return 0, nil
	}
	if w.opcode == frame.Text && w.conn.utf8ValidationEnabled() && !w.validateText(payload) {
		w.textInvalid = true
		return 0, w.fail(frame.ErrInvalidUTF8)
	}
	if w.stream != nil {
		n, err := w.stream.Write(payload)
		w.bytes += uint64(n)
		if err != nil {
			return n, w.fail(err)
		}
		return n, err
	}
	maxFrame := w.conn.maxFramePayload()
	if maxFrame == 0 {
		maxFrame = uint64(len(payload))
	}
	written := 0
	for written < len(payload) {
		chunk := len(payload) - written
		if uint64(chunk) > maxFrame {
			chunk = int(maxFrame)
		}
		opcode := frame.Continuation
		if w.first {
			opcode = w.opcode
		}
		if err := w.conn.sendFrameLocked(frame.Frame{Opcode: opcode, Payload: payload[written : written+chunk]}); err != nil {
			return written, w.fail(err)
		}
		w.first = false
		written += chunk
		w.bytes += uint64(chunk)
	}
	return written, nil
}

// Close finishes and flushes the message, then releases connection write ownership.
// After a Write failure it only returns the first error.
func (w *Writer) Close() error {
	if w == nil || w.conn == nil {
		return ErrWriterClosed
	}
	if w.failure != nil {
		return w.failure
	}
	if w.closed {
		return ErrWriterClosed
	}
	var err error
	if w.textInvalid || (w.opcode == frame.Text && w.conn.utf8ValidationEnabled() && w.textTailLen > 0) {
		return w.fail(frame.ErrInvalidUTF8)
	}
	if w.stream != nil {
		err = w.stream.Close()
		if err == nil {
			if w.first {
				err = w.conn.sendFrameLocked(frame.Frame{Fin: true, Opcode: w.opcode})
			} else {
				err = w.conn.sendFrameLocked(frame.Frame{Fin: true, Opcode: frame.Continuation})
			}
		}
		if err == nil {
			w.conn.compression.encoder.CommitStream(w.stream)
		}
	} else {
		opcode := frame.Continuation
		if w.first {
			opcode = w.opcode
		}
		err = w.conn.sendFrameLocked(frame.Frame{Fin: true, Opcode: opcode})
	}
	if err == nil {
		err = w.conn.flush()
	}
	if err != nil {
		return w.fail(err)
	}
	w.closed = true
	w.conn.writeMu.Unlock()
	return nil
}

func (w *Writer) fail(err error) error {
	if w.failure == nil {
		w.failure = err
	}
	if w.closed {
		return w.failure
	}
	w.closed = true
	w.conn.closing.Store(true)
	if w.stream != nil {
		w.stream.Abort()
	}
	w.conn.writeMu.Unlock()
	if w.conn.raw != nil {
		_ = w.conn.raw.CloseWith(w.failure)
	}
	return w.failure
}

func (w *Writer) emitCompressed(payload []byte) error {
	if len(payload) == 0 {
		return nil
	}
	maxFrame := w.conn.maxFramePayload()
	if maxFrame == 0 {
		maxFrame = uint64(len(payload))
	}
	for offset := 0; offset < len(payload); {
		chunk := len(payload) - offset
		if uint64(chunk) > maxFrame {
			chunk = int(maxFrame)
		}
		first := w.first
		opcode := frame.Continuation
		if first {
			opcode = w.opcode
			w.first = false
		}
		if err := w.conn.sendFrameLocked(frame.Frame{
			Fin: false, RSV1: first, Opcode: opcode,
			Payload: payload[offset : offset+chunk],
		}); err != nil {
			return err
		}
		offset += chunk
	}
	return nil
}

func (w *Writer) validateText(payload []byte) bool {
	if w.textTailLen > 0 {
		var runeBytes [utf8.UTFMax]byte
		oldTailLen := w.textTailLen
		n := copy(runeBytes[:], w.textTail[:oldTailLen])
		take := min(len(payload), utf8.UTFMax-n)
		n += copy(runeBytes[n:], payload[:take])
		candidate := runeBytes[:n]
		if !utf8.FullRune(candidate) {
			copy(w.textTail[:], candidate)
			w.textTailLen = n
			return true
		}
		_, size := utf8.DecodeRune(candidate)
		if size == 1 {
			return false
		}
		payload = payload[size-oldTailLen:]
		w.textTailLen = 0
	}
	if utf8.Valid(payload) {
		return true
	}
	for n := 1; n <= utf8.UTFMax-1 && n <= len(payload); n++ {
		prefix := payload[:len(payload)-n]
		suffix := payload[len(payload)-n:]
		if utf8.Valid(prefix) && !utf8.FullRune(suffix) {
			copy(w.textTail[:], suffix)
			w.textTailLen = n
			return true
		}
	}
	return false
}
