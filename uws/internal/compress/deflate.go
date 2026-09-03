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

func Compress(payload []byte, level int) ([]byte, error) {
	encoder := NewEncoder(level, true)
	data, err := encoder.Encode(payload)
	if err != nil {
		return nil, err
	}
	encoder.Commit(payload)
	return data, nil
}

func Decompress(payload []byte, maxSize int) ([]byte, error) {
	return NewDecoder(true).Decode(payload, maxSize)
}

type Encoder struct {
	level       int
	noContext   bool
	windowBits  int
	windowBytes int
	dictionary  []byte
}

const maxPooledCompressionOutput = 256 << 10

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

func NewEncoder(level int, noContext bool) *Encoder {
	return NewEncoderWithWindow(level, noContext, DefaultWindowBits)
}

func NewEncoderWithWindow(level int, noContext bool, windowBits int) *Encoder {
	windowBits = normalizeWindowBits(windowBits)
	return &Encoder{
		level:       level,
		noContext:   noContext,
		windowBits:  windowBits,
		windowBytes: 1 << windowBits,
	}
}

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

// NewStream creates a message-scoped encoder. Compressed bytes are delivered
// to emit as they become available. The caller must call Close exactly once.
func (e *Encoder) NewStream(emit func([]byte) error) (*StreamEncoder, error) {
	return newStreamEncoder(e.level, e.dictionary, e.windowBits, !e.noContext, emit)
}

// SetDictionary publishes a completed message's history after its compressed
// frames have been accepted for transmission.
func (e *Encoder) SetDictionary(dictionary []byte) {
	if e == nil || e.noContext {
		return
	}
	e.dictionary = appendDictionary(nil, dictionary, e.windowBytes)
}

// CommitStream transfers a completed stream's rolling dictionary into the
// encoder without copying it again.
func (e *Encoder) CommitStream(stream *StreamEncoder) {
	if e == nil || e.noContext || stream == nil || !stream.closed {
		return
	}
	e.dictionary = stream.takeDictionary()
}

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

type StreamEncoder struct {
	writer          streamWriter
	writerJob       *flateWriterJob
	trunc           *streamTruncWriter
	dictionary      rollingDictionary
	trackDictionary bool
	flushed         bool
	closed          bool
}

type streamWriter interface {
	io.Writer
	io.Closer
	Flush() error
}

func newStreamEncoder(level int, dictionary []byte, windowBits int, trackDictionary bool, emit func([]byte) error) (*StreamEncoder, error) {
	if emit == nil {
		return nil, errors.New("websocket: nil deflate stream callback")
	}
	windowBits = normalizeWindowBits(windowBits)
	trunc := &streamTruncWriter{emit: emit}
	stream := &StreamEncoder{
		trunc:           trunc,
		trackDictionary: trackDictionary,
	}
	if trackDictionary {
		stream.dictionary = newRollingDictionary(1<<windowBits, dictionary)
	}
	var err error
	if windowBits < DefaultWindowBits {
		stream.writer, err = kflate.NewWriterWindow(trunc, 1<<windowBits)
		if err == nil && len(dictionary) > 0 {
			stream.writer.(*kflate.Writer).ResetDict(trunc, dictionary)
		}
	} else if !trackDictionary && len(dictionary) == 0 {
		stream.writerJob, err = acquireFlateWriter(level)
		if err == nil {
			stream.writerJob.writer.Reset(trunc)
			stream.writer = stream.writerJob.writer
		}
	} else {
		stream.writer, err = stdflate.NewWriterDict(trunc, level, dictionary)
	}
	if err != nil {
		return nil, err
	}
	return stream, nil
}

func (s *StreamEncoder) Write(payload []byte) (int, error) {
	if s == nil || s.closed {
		return 0, io.ErrClosedPipe
	}
	s.flushed = false
	n, err := s.writer.Write(payload)
	if n > 0 && s.trackDictionary {
		s.dictionary.Append(payload[:n])
	}
	if err == nil {
		err = s.writer.Flush()
		if err == nil {
			s.flushed = true
		}
	}
	return n, err
}

func (s *StreamEncoder) Dictionary() []byte {
	if s == nil || !s.trackDictionary {
		return nil
	}
	return s.dictionary.Clone()
}

func (s *StreamEncoder) takeDictionary() []byte {
	if s == nil || !s.trackDictionary {
		return nil
	}
	s.trackDictionary = false
	return s.dictionary.Take()
}

func (s *StreamEncoder) Close() error {
	if s == nil || s.closed {
		return io.ErrClosedPipe
	}
	s.closed = true
	var err error
	if !s.flushed {
		err = s.writer.Flush()
	}
	if err == nil {
		err = s.trunc.finish()
	}
	// Close may emit a final DEFLATE block after the sync-flush marker. The
	// truncation writer discards bytes after finish so they never reach frames.
	closeErr := s.writer.Close()
	if s.writerJob != nil {
		releaseFlateWriter(s.writerJob)
		s.writerJob = nil
		s.writer = nil
	}
	if err != nil {
		return err
	}
	return closeErr
}

// Abort releases stream resources without emitting any more compressed data.
func (s *StreamEncoder) Abort() {
	if s == nil || s.closed {
		return
	}
	s.closed = true
	if s.trunc != nil {
		s.trunc.finished = true
		s.trunc.pending = nil
	}
	if s.writer != nil {
		_ = s.writer.Close()
	}
	if s.writerJob != nil {
		releaseFlateWriter(s.writerJob)
		s.writerJob = nil
		s.writer = nil
	}
}

type streamTruncWriter struct {
	emit     func([]byte) error
	pending  []byte
	finished bool
}

func (w *streamTruncWriter) Write(payload []byte) (int, error) {
	if w.finished {
		return len(payload), nil
	}
	w.pending = append(w.pending, payload...)
	if len(w.pending) <= len(syncFlushTail) {
		return len(payload), nil
	}
	cut := len(w.pending) - len(syncFlushTail)
	if err := w.emit(w.pending[:cut]); err != nil {
		return 0, err
	}
	copy(w.pending, w.pending[cut:])
	w.pending = w.pending[:len(syncFlushTail)]
	return len(payload), nil
}

func (w *streamTruncWriter) finish() error {
	if len(w.pending) != len(syncFlushTail) || !bytes.Equal(w.pending, []byte(syncFlushTail)) {
		return errors.New("websocket: invalid deflate sync flush")
	}
	w.finished = true
	w.pending = nil
	return nil
}

type rollingDictionary struct {
	data  []byte
	limit int
	start int
	size  int
}

func newRollingDictionary(limit int, initial []byte) rollingDictionary {
	if limit <= 0 || limit > maxDictionaryBytes {
		limit = maxDictionaryBytes
	}
	dictionary := rollingDictionary{limit: limit}
	dictionary.Append(initial)
	return dictionary
}

func (d *rollingDictionary) Append(payload []byte) {
	if d == nil || d.limit == 0 || len(payload) == 0 {
		return
	}
	if d.data == nil {
		d.data = make([]byte, d.limit)
	}
	if len(payload) >= d.limit {
		copy(d.data, payload[len(payload)-d.limit:])
		d.start = 0
		d.size = d.limit
		return
	}
	if d.size < d.limit {
		n := min(len(payload), d.limit-d.size)
		d.writeAt((d.start+d.size)%d.limit, payload[:n])
		d.size += n
		payload = payload[n:]
	}
	if len(payload) > 0 {
		d.writeAt(d.start, payload)
		d.start = (d.start + len(payload)) % d.limit
	}
}

func (d *rollingDictionary) writeAt(offset int, payload []byte) {
	n := min(len(payload), d.limit-offset)
	copy(d.data[offset:], payload[:n])
	copy(d.data, payload[n:])
}

func (d *rollingDictionary) Clone() []byte {
	if d == nil || d.size == 0 {
		return nil
	}
	result := make([]byte, d.size)
	n := copy(result, d.data[d.start:])
	copy(result[n:], d.data[:d.start])
	return result
}

func (d *rollingDictionary) Take() []byte {
	if d == nil || d.size == 0 {
		return nil
	}
	if d.start != 0 {
		reverseBytes(d.data[:d.start])
		reverseBytes(d.data[d.start:])
		reverseBytes(d.data)
	}
	result := d.data[:d.size]
	*d = rollingDictionary{}
	return result
}

func reverseBytes(data []byte) {
	for left, right := 0, len(data)-1; left < right; left, right = left+1, right-1 {
		data[left], data[right] = data[right], data[left]
	}
}

func (e *Encoder) Close() error {
	return nil
}

type Decoder struct {
	noContext   bool
	windowBytes int
	dictionary  []byte
}

type decodeSource struct {
	payload       []byte
	payloadOffset int
	tailOffset    int
}

func (source *decodeSource) Reset(payload []byte) {
	source.payload = payload
	source.payloadOffset = 0
	source.tailOffset = 0
}

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

type flateReaderJob struct {
	source decodeSource
	reader io.ReadCloser
	output []byte
	extra  [1]byte
}

var flateReaderPool sync.Pool

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

func NewDecoder(noContext bool) *Decoder {
	return NewDecoderWithWindow(noContext, DefaultWindowBits)
}

func NewDecoderWithWindow(noContext bool, windowBits int) *Decoder {
	windowBits = normalizeWindowBits(windowBits)
	return &Decoder{noContext: noContext, windowBytes: 1 << windowBits}
}

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
