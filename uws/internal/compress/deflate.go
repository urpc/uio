// Package compress implements RFC 7692 message DEFLATE with bounded output and
// negotiated context takeover.
package compress

import (
	"bytes"
	stdflate "compress/flate"
	"errors"
	"io"
	"sync"

	kflate "github.com/klauspost/compress/flate"
)

var (
	ErrTooLarge = errors.New("websocket: decompressed message too large")
)

const (
	DefaultWindowBits  = 15
	maxDictionaryBytes = 1 << DefaultWindowBits
	syncFlushTail      = "\x00\x00\xff\xff"
	decodeTail         = syncFlushTail + "\x01\x00\x00\xff\xff"
)

// Params is the normalized RFC 7692 negotiation result for both directions.
type Params struct {
	Enabled                 bool
	ServerNoContextTakeover bool
	ClientNoContextTakeover bool
	ServerMaxWindowBitsSet  bool
	ServerMaxWindowBits     int
	ClientMaxWindowBits     int
	ClientMaxWindowBitsSet  bool
	Level                   int
}

// Compress encodes one no-context-takeover message.
func Compress(payload []byte, level int) ([]byte, error) {
	encoder := NewEncoder(level, true)
	data, err := encoder.Encode(payload)
	if err != nil {
		return nil, err
	}
	encoder.Commit(payload)
	return data, nil
}

// Decompress decodes one no-context-takeover message with an output bound.
func Decompress(payload []byte, maxSize int) ([]byte, error) {
	return NewDecoder(true).Decode(payload, maxSize)
}

// Encoder applies message-scoped DEFLATE and optionally retains the negotiated
// sliding-window dictionary across committed messages.
type Encoder struct {
	level       int
	noContext   bool
	windowBits  int
	windowBytes int
	dictionary  []byte
}

const maxPooledCompressionOutput = 256 << 10

// flateWriterJob keeps a resettable stdlib writer and its bounded output buffer
// together so pool reuse cannot mismatch compression level and storage.
type flateWriterJob struct {
	writer    *stdflate.Writer
	output    bytes.Buffer
	poolIndex int
}

var flateWriterPools [stdflate.BestCompression - stdflate.HuffmanOnly + 1]sync.Pool

func flateWriterPoolIndex(level int) (int, bool) {
	if level < stdflate.HuffmanOnly || level > stdflate.BestCompression {
		return 0, false
	}
	return level - stdflate.HuffmanOnly, true
}

func acquireFlateWriter(level int) (*flateWriterJob, error) {
	index, pooled := flateWriterPoolIndex(level)
	if pooled {
		if job, _ := flateWriterPools[index].Get().(*flateWriterJob); job != nil {
			job.output.Reset()
			return job, nil
		}
	}
	writer, err := stdflate.NewWriter(io.Discard, level)
	if err != nil {
		return nil, err
	}
	if !pooled {
		index = -1
	}
	return &flateWriterJob{writer: writer, poolIndex: index}, nil
}

func releaseFlateWriter(job *flateWriterJob) {
	if job == nil {
		return
	}
	job.writer.Reset(io.Discard)
	if job.output.Cap() > maxPooledCompressionOutput {
		job.output = bytes.Buffer{}
	} else {
		job.output.Reset()
	}
	if job.poolIndex >= 0 {
		flateWriterPools[job.poolIndex].Put(job)
	}
}

// NewEncoder creates an encoder with the RFC default 32 KiB window.
func NewEncoder(level int, noContext bool) *Encoder {
	return NewEncoderWithWindow(level, noContext, DefaultWindowBits)
}

// NewEncoderWithWindow creates an encoder for a negotiated window size.
func NewEncoderWithWindow(level int, noContext bool, windowBits int) *Encoder {
	windowBits = normalizeWindowBits(windowBits)
	return &Encoder{
		level:       level,
		noContext:   noContext,
		windowBits:  windowBits,
		windowBytes: 1 << windowBits,
	}
}

// Encode returns an owned compressed message. Use EncodeBorrowed on hot paths
// that can consume output synchronously.
func (e *Encoder) Encode(payload []byte) ([]byte, error) {
	var result []byte
	err := e.EncodeBorrowed(payload, func(encoded []byte) error {
		result = append([]byte(nil), encoded...)
		return nil
	})
	return result, err
}

// EncodeBorrowed passes the compressed payload to use. The payload remains
// valid only until use returns.
func (e *Encoder) EncodeBorrowed(payload []byte, use func([]byte) error) error {
	if use == nil {
		return errors.New("websocket: nil encode callback")
	}
	if e.windowBits < DefaultWindowBits {
		return e.encodeWindowedBorrowed(payload, use)
	}
	if e.noContext {
		job, err := acquireFlateWriter(e.level)
		if err != nil {
			return err
		}
		defer releaseFlateWriter(job)
		job.writer.Reset(&job.output)
		return encodeWithFlateWriter(job.writer, &job.output, payload, use)
	}
	var output bytes.Buffer
	writer, err := stdflate.NewWriterDict(&output, e.level, e.dictionary)
	if err != nil {
		return err
	}
	return encodeWithFlateWriter(writer, &output, payload, use)
}

func encodeWithFlateWriter(writer *stdflate.Writer, output *bytes.Buffer, payload []byte, use func([]byte) error) error {
	if _, err := writer.Write(payload); err != nil {
		_ = writer.Close()
		return err
	}
	if err := writer.Close(); err != nil {
		return err
	}
	data := output.Bytes()
	if len(data) >= 4 && bytes.Equal(data[len(data)-4:], []byte{0, 0, 0xff, 0xff}) {
		data = data[:len(data)-4]
	}
	return use(data)
}

// encodeWindowedBorrowed uses klauspost's explicit-window encoder when RFC
// negotiation selects less than the stdlib's fixed 32 KiB window.
func (e *Encoder) encodeWindowedBorrowed(payload []byte, use func([]byte) error) error {
	var output bytes.Buffer
	// The custom-window API intentionally uses its fast windowed encoder. The
	// negotiated window size is a wire-level constraint; the configured level
	// remains effective on the default 32 KiB stdlib path.
	writer, err := kflate.NewWriterWindow(&output, e.windowBytes)
	if err != nil {
		return err
	}
	if len(e.dictionary) > 0 {
		writer.ResetDict(&output, e.dictionary)
	}
	if _, err = writer.Write(payload); err != nil {
		_ = writer.Close()
		return err
	}
	if err = writer.Flush(); err != nil {
		_ = writer.Close()
		return err
	}
	data := output.Bytes()
	if len(data) < len(syncFlushTail) || !bytes.Equal(data[len(data)-len(syncFlushTail):], []byte(syncFlushTail)) {
		_ = writer.Close()
		return errors.New("websocket: invalid deflate sync flush")
	}
	resultLen := len(data) - len(syncFlushTail)
	if err = writer.Close(); err != nil {
		return err
	}
	return use(output.Bytes()[:resultLen])
}

// Commit advances the compression context after the encoded message has been
// selected for transmission. A caller that falls back to an uncompressed
// message must not commit the payload, otherwise the peer's context diverges.
func (e *Encoder) Commit(payload []byte) {
	if e == nil || e.noContext {
		return
	}
	e.dictionary = appendDictionary(e.dictionary, payload, e.windowBytes)
}

func (e *Encoder) Close() error {
	return nil
}

// Decoder expands one message at a time and optionally retains the negotiated
// sliding-window dictionary.
type Decoder struct {
	noContext   bool
	windowBytes int
	dictionary  []byte
}

// decodeSource presents compressed payload followed by the synthetic DEFLATE
// tail required to terminate one permessage-deflate message.
type decodeSource struct {
	payload       []byte
	payloadOffset int
	tailOffset    int
}

// Reset starts a new logical compressed message.
func (source *decodeSource) Reset(payload []byte) {
	source.payload = payload
	source.payloadOffset = 0
	source.tailOffset = 0
}

// Read streams payload first and the synthetic termination tail second.
func (source *decodeSource) Read(dst []byte) (int, error) {
	written := 0
	if source.payloadOffset < len(source.payload) {
		n := copy(dst, source.payload[source.payloadOffset:])
		source.payloadOffset += n
		written += n
	}
	if written < len(dst) && source.tailOffset < len(decodeTail) {
		n := copy(dst[written:], decodeTail[source.tailOffset:])
		source.tailOffset += n
		written += n
	}
	if written == 0 {
		return 0, io.EOF
	}
	return written, nil
}

// flateReaderJob groups resettable decoder state and bounded reusable output.
type flateReaderJob struct {
	source decodeSource
	reader io.ReadCloser
	output []byte
	extra  [1]byte
}

var flateReaderPool sync.Pool

// acquireFlateReader resets a pooled decoder with the current message and
// negotiated context dictionary.
func acquireFlateReader(payload, dictionary []byte) (*flateReaderJob, error) {
	job, _ := flateReaderPool.Get().(*flateReaderJob)
	if job == nil {
		job = &flateReaderJob{}
		job.source.Reset(payload)
		job.reader = stdflate.NewReaderDict(&job.source, dictionary)
		return job, nil
	}
	job.source.Reset(payload)
	resetter, ok := job.reader.(stdflate.Resetter)
	if !ok {
		_ = job.reader.Close()
		return nil, errors.New("websocket: flate reader cannot reset")
	}
	if err := resetter.Reset(&job.source, dictionary); err != nil {
		_ = job.reader.Close()
		return nil, err
	}
	job.output = job.output[:0]
	return job, nil
}

func releaseFlateReader(job *flateReaderJob) {
	if job == nil {
		return
	}
	_ = job.reader.Close()
	job.source.Reset(nil)
	if cap(job.output) > maxPooledCompressionOutput {
		job.output = nil
	} else {
		job.output = job.output[:0]
	}
	flateReaderPool.Put(job)
}

// NewDecoder creates a decoder with the RFC default 32 KiB window.
func NewDecoder(noContext bool) *Decoder {
	return NewDecoderWithWindow(noContext, DefaultWindowBits)
}

// NewDecoderWithWindow creates a decoder for a negotiated window size.
func NewDecoderWithWindow(noContext bool, windowBits int) *Decoder {
	windowBits = normalizeWindowBits(windowBits)
	return &Decoder{noContext: noContext, windowBytes: 1 << windowBits}
}

// Decode returns an owned decompressed message bounded by maxSize.
func (d *Decoder) Decode(payload []byte, maxSize int) ([]byte, error) {
	var result []byte
	err := d.DecodeBorrowed(payload, maxSize, func(decoded []byte) error {
		result = append([]byte(nil), decoded...)
		return nil
	})
	return result, err
}

// DecodeBorrowed passes the decompressed payload to use. The payload remains
// valid only until use returns.
func (d *Decoder) DecodeBorrowed(payload []byte, maxSize int, use func([]byte) error) error {
	if use == nil {
		return errors.New("websocket: nil decode callback")
	}
	if maxSize <= 0 {
		maxSize = int(^uint(0) >> 1)
	}
	job, err := acquireFlateReader(payload, d.dictionary)
	if err != nil {
		return err
	}
	output := job.output[:0]
	defer func() {
		job.output = output
		releaseFlateReader(job)
	}()
	for {
		if len(output) == maxSize {
			n, readErr := job.reader.Read(job.extra[:])
			if n > 0 {
				return ErrTooLarge
			}
			if readErr == io.EOF {
				break
			}
			if readErr != nil {
				return readErr
			}
			continue
		}
		writeLimit := min(cap(output), maxSize)
		if len(output) == writeLimit {
			output = growDecodeOutput(output, maxSize)
			writeLimit = cap(output)
		}
		n, readErr := job.reader.Read(output[len(output):writeLimit])
		output = output[:len(output)+n]
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return readErr
		}
	}
	if !d.noContext {
		d.dictionary = appendDictionary(d.dictionary, output, d.windowBytes)
	}
	return use(output)
}

func growDecodeOutput(output []byte, maxSize int) []byte {
	capacity := cap(output) * 2
	if capacity < 32<<10 {
		capacity = 32 << 10
	}
	if capacity > maxSize || capacity < cap(output) {
		capacity = maxSize
	}
	next := make([]byte, len(output), capacity)
	copy(next, output)
	return next
}

func (d *Decoder) Close() error {
	return nil
}

func appendDictionary(dictionary, payload []byte, limit int) []byte {
	if limit <= 0 || limit > maxDictionaryBytes {
		limit = maxDictionaryBytes
	}
	if len(payload) >= limit {
		return append([]byte(nil), payload[len(payload)-limit:]...)
	}
	need := len(dictionary) + len(payload)
	if need <= limit {
		return append(dictionary, payload...)
	}
	keep := limit - len(payload)
	result := make([]byte, 0, limit)
	result = append(result, dictionary[len(dictionary)-keep:]...)
	return append(result, payload...)
}

func normalizeWindowBits(bits int) int {
	if bits < 8 || bits > DefaultWindowBits {
		return DefaultWindowBits
	}
	return bits
}
