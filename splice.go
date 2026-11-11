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

	"github.com/urpc/uio/internal/blockio"
)

// Splice moves count bytes of src's stream into dst, or all of it up to EOF
// when count < 0, and reports how many it moved. A stream that ends before
// count bytes fails as io.ReadFull does: io.EOF when nothing moved,
// io.ErrUnexpectedEOF otherwise. It reads src through its BlockingReader (see
// NewBlockingReader), which switches src to pull mode for good: its Handler's
// OnRead runs no more.
//
// A Conn dst — and a BlockingWriter or a BlockingConn — is written through its
// BlockingWriter, so the transfer waits for room in its queue. On Linux, when
// src is a native stream connection and dst is one too, or an *os.File, the
// bytes src already read go first and the rest of the stream moves in the
// kernel with splice(2); otherwise Splice copies through pooled buffers.
//
// ctx, and src's read deadline while the transfer reads src, end it early:
// the bytes Splice took from src and has not handed to dst stay with src's
// BlockingReader, for the next read. Bytes handed to a dst that then fails are
// lost with it.
func Splice(ctx context.Context, dst io.Writer, src Conn, count int64) (int64, error) {
	if OnTurn() {
		return 0, ErrBlockingOnTurn
	}
	if src == nil || dst == nil {
		return 0, ErrUnsupported
	}
	br, err := NewBlockingReader(src)
	if err != nil {
		return 0, err
	}
	return br.SpliceTo(ctx, dst, count)
}

// SpliceTo moves count bytes of r's stream into dst, or all of it up to EOF
// when count < 0, as Splice does.
func (r *BlockingReader) SpliceTo(ctx context.Context, dst io.Writer, count int64) (int64, error) {
	if dst == nil {
		return 0, ErrUnsupported
	}
	return r.r.SpliceTo(ctx, blockingDestination(dst), count)
}

// WriteTo copies r's stream into w up to EOF, as SpliceTo(ctx, w, -1) does
// with a context that never ends: a Conn w is written through its
// BlockingWriter, and the bytes move with splice(2) where SpliceTo's do. It
// is what io.Copy(w, r) calls.
func (r *BlockingReader) WriteTo(w io.Writer) (int64, error) {
	if w == nil {
		return 0, ErrUnsupported
	}
	return r.r.WriteTo(blockingDestination(w))
}

// blockingDestination is where a BlockingReader writes w's bytes: a Conn's
// BlockingWriter, so the transfer waits for room in its queue rather than
// failing once it is full.
func blockingDestination(w io.Writer) io.Writer {
	if _, ok := w.(interface{ InternalWriter() *blockio.Writer }); ok {
		return w
	}
	if c, ok := w.(Conn); ok {
		if st, ok := resolveBlockState(c); ok {
			return st.Writer(0)
		}
	}
	return w
}

// SpliceTo moves count bytes of bc's stream into dst, or all of it up to EOF
// when count < 0; see BlockingReader.SpliceTo.
func (bc *BlockingConn) SpliceTo(ctx context.Context, dst io.Writer, count int64) (int64, error) {
	return bc.r.SpliceTo(ctx, dst, count)
}
