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

package uio

import (
	"context"
	"io"
	"net"
	"time"

	"github.com/urpc/uio/internal/blockio"
)

// ErrBlockingOnTurn reports an attempt to invoke a blocking read or write on a
// connection turn or event-loop goroutine.
var ErrBlockingOnTurn = blockio.ErrBlockingOnTurn

// DefaultBlockingBuffer is the default per-direction buffer limit (64 KiB) for
// BlockingReader and BlockingWriter when Events does not specify a limit.
const DefaultBlockingBuffer = blockio.DefaultWindow

type blockingConfig struct {
	bufferSize int
}

// BlockingOption configures a BlockingReader, BlockingWriter, or BlockingConn.
type BlockingOption interface {
	applyBlocking(*blockingConfig)
}

type blockingBufferOption int

func (o blockingBufferOption) applyBlocking(cfg *blockingConfig) {
	cfg.bufferSize = int(o)
}

// WithBufferSize sets the buffer window (in bytes) for a BlockingReader,
// BlockingWriter, or BlockingConn. Values <= 0 leave the default window in effect.
func WithBufferSize(bytes int) BlockingOption {
	return blockingBufferOption(bytes)
}

func resolveBlockingOptions(opts []BlockingOption) blockingConfig {
	var cfg blockingConfig
	for _, opt := range opts {
		if opt != nil {
			opt.applyBlocking(&cfg)
		}
	}
	return cfg
}

func resolveBlockState(c Conn) (*blockio.State, bool) {
	var cur Lower = c
	for cur != nil {
		if st, ok := blockio.StateOf(cur); ok {
			return st, st != nil
		}
		cur = Unwrap(cur)
	}
	return nil, false
}

// BlockingReader is the pull-mode blocking inbound endpoint of a stream Conn.
// It does not implement io.Closer; lifecycle ownership stays on Conn.
type BlockingReader struct {
	r *blockio.Reader
}

// NewBlockingReader switches c's inbound direction from OnRead push callbacks
// to pull-mode blocking reads and returns its single *BlockingReader.
// Calling NewBlockingReader again on the same connection returns the same
// instance. Packet (UDP) connections return (nil, ErrUnsupported).
func NewBlockingReader(c Conn, opts ...BlockingOption) (*BlockingReader, error) {
	if c == nil {
		return nil, ErrUnsupported
	}
	st, ok := resolveBlockState(c)
	if !ok {
		return nil, ErrUnsupported
	}
	cfg := resolveBlockingOptions(opts)
	r, err := st.Reader(cfg.bufferSize)
	if err != nil {
		return nil, err
	}
	return &BlockingReader{r: r}, nil
}

// Read reads up to len(p) bytes from the connection, blocking until data
// arrives, EOF is reached, the connection closes, or the read deadline expires.
func (r *BlockingReader) Read(p []byte) (int, error) { return r.r.Read(p) }

// WriteTo copies stream data to w until EOF or error, transferring pooled
// buffers with zero intermediate copies when w is a *BlockingWriter or *BlockingConn.
func (r *BlockingReader) WriteTo(w io.Writer) (int64, error) { return r.r.WriteTo(w) }

// SetReadDeadline sets the deadline for blocked Read and WriteTo calls without
// closing the underlying connection when it expires.
func (r *BlockingReader) SetReadDeadline(t time.Time) error { return r.r.SetReadDeadline(t) }

// Buffered reports unread bytes currently buffered in the reader's pipe.
func (r *BlockingReader) Buffered() int { return r.r.Buffered() }

// BlockingWriter is the blocking outbound endpoint of a stream Conn.
// Multiple goroutines may invoke methods on a BlockingWriter concurrently, and
// it may coexist with non-blocking Conn.Write calls. It does not implement io.Closer.
type BlockingWriter struct {
	w *blockio.Writer
}

// NewBlockingWriter returns c's single *BlockingWriter for backpressured
// blocking writes from application goroutines. Packet (UDP) connections return
// (nil, ErrUnsupported).
func NewBlockingWriter(c Conn, opts ...BlockingOption) (*BlockingWriter, error) {
	if c == nil {
		return nil, ErrUnsupported
	}
	st, ok := resolveBlockState(c)
	if !ok {
		return nil, ErrUnsupported
	}
	cfg := resolveBlockingOptions(opts)
	return &BlockingWriter{w: st.Writer(cfg.bufferSize)}, nil
}

// InternalWriter exposes the underlying blockio.Writer for zero-copy WriteTo relays.
func (w *BlockingWriter) InternalWriter() *blockio.Writer { return w.w }

// OnTurn reports whether the calling goroutine is currently executing inside an
// event-loop or connection-turn callback, where blocking I/O must not block.
func OnTurn() bool { return blockio.OnTurn() }

// Write writes all of p to the connection, suspending while the outbound window
// is full. Each call is atomic among BlockingWriter callers.
func (w *BlockingWriter) Write(p []byte) (int, error) { return w.w.Write(p) }

// WriteContext writes all of p to the connection, suspending until space is
// available, the connection closes, the write deadline expires, or ctx ends.
func (w *BlockingWriter) WriteContext(ctx context.Context, p []byte) (int, error) {
	return w.w.WriteContext(ctx, p)
}

// Writev writes all segments of vec atomically among BlockingWriter callers.
func (w *BlockingWriter) Writev(vec [][]byte) (int, error) { return w.w.Writev(vec) }

// WritevContext writes all segments of vec atomically among BlockingWriter
// callers, honoring ctx cancellation and deadlines while suspended.
func (w *BlockingWriter) WritevContext(ctx context.Context, vec [][]byte) (int, error) {
	return w.w.WritevContext(ctx, vec)
}

// WriteOwned transfers ownership of buf to the connection, suspending while the
// outbound window is full, and always consumes or releases buf.
func (w *BlockingWriter) WriteOwned(buf *Buffer) (int, error) { return w.w.WriteOwned(buf) }

// WriteOwnedContext transfers ownership of buf to the connection, suspending
// while the outbound window is full until space drains, the connection closes,
// the write deadline expires, or ctx ends. Always consumes or releases buf.
func (w *BlockingWriter) WriteOwnedContext(ctx context.Context, buf *Buffer) (int, error) {
	return w.w.WriteOwnedContext(ctx, buf)
}

// ReadFrom copies from r into the connection using pooled zero-copy WriteOwned
// buffers.
func (w *BlockingWriter) ReadFrom(r io.Reader) (int64, error) { return w.w.ReadFrom(r) }

// Flush flushes queued outbound bytes and blocks until the outbound buffer
// drains, the connection closes, or the write deadline expires.
func (w *BlockingWriter) Flush() error { return w.w.Flush() }

// FlushContext flushes queued outbound bytes and blocks until the outbound
// buffer drains, the connection closes, the write deadline expires, or ctx ends.
func (w *BlockingWriter) FlushContext(ctx context.Context) error { return w.w.FlushContext(ctx) }

// SetWriteDeadline sets the deadline for blocked Write, ReadFrom, and Flush
// calls without closing the underlying connection when it expires.
func (w *BlockingWriter) SetWriteDeadline(t time.Time) error { return w.w.SetWriteDeadline(t) }

// Buffered reports accepted outbound bytes not yet flushed to the wire.
func (w *BlockingWriter) Buffered() int { return w.w.Buffered() }

// BlockingConn wraps a stream Conn with its BlockingReader and BlockingWriter
// to satisfy the standard library's [net.Conn], [io.ReaderFrom], and [io.WriterTo]
// interfaces.
type BlockingConn struct {
	conn Conn
	r    *BlockingReader
	w    *BlockingWriter
}

// NewBlockingConn switches c's inbound direction to pull mode and returns a
// standard [net.Conn] backed by c's BlockingReader and BlockingWriter.
func NewBlockingConn(c Conn, opts ...BlockingOption) (*BlockingConn, error) {
	r, err := NewBlockingReader(c, opts...)
	if err != nil {
		return nil, err
	}
	w, err := NewBlockingWriter(c, opts...)
	if err != nil {
		return nil, err
	}
	return &BlockingConn{conn: c, r: r, w: w}, nil
}

func (bc *BlockingConn) Read(p []byte) (int, error)  { return bc.r.Read(p) }
func (bc *BlockingConn) Write(p []byte) (int, error) { return bc.w.Write(p) }
func (bc *BlockingConn) WriteContext(ctx context.Context, p []byte) (int, error) {
	return bc.w.WriteContext(ctx, p)
}
func (bc *BlockingConn) ReadFrom(r io.Reader) (int64, error)    { return bc.w.ReadFrom(r) }
func (bc *BlockingConn) WriteTo(w io.Writer) (int64, error)     { return bc.r.WriteTo(w) }
func (bc *BlockingConn) Flush() error                           { return bc.w.Flush() }
func (bc *BlockingConn) FlushContext(ctx context.Context) error { return bc.w.FlushContext(ctx) }
func (bc *BlockingConn) Close() error                           { return bc.conn.Close() }
func (bc *BlockingConn) CloseWrite() error                      { return bc.conn.CloseWrite() }
func (bc *BlockingConn) Abort(cause error) error                { return bc.conn.Abort(cause) }
func (bc *BlockingConn) LocalAddr() net.Addr                    { return bc.conn.LocalAddr() }
func (bc *BlockingConn) RemoteAddr() net.Addr                   { return bc.conn.RemoteAddr() }
func (bc *BlockingConn) Unwrap() Conn                           { return bc.conn }
func (bc *BlockingConn) Reader() *BlockingReader                { return bc.r }
func (bc *BlockingConn) Writer() *BlockingWriter                { return bc.w }
func (bc *BlockingConn) InternalWriter() *blockio.Writer        { return bc.w.w }

// SetDeadline sets both the read and write blocking wait deadlines without
// closing the underlying connection when they expire.
func (bc *BlockingConn) SetDeadline(t time.Time) error {
	_ = bc.r.SetReadDeadline(t)
	return bc.w.SetWriteDeadline(t)
}

func (bc *BlockingConn) SetReadDeadline(t time.Time) error  { return bc.r.SetReadDeadline(t) }
func (bc *BlockingConn) SetWriteDeadline(t time.Time) error { return bc.w.SetWriteDeadline(t) }

var _ net.Conn = (*BlockingConn)(nil)
