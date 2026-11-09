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

// Package uio provides event-driven connections whose native Unix data path
// runs in serialized connection tasks outside epoll/kqueue loops.
package uio

import (
	"errors"

	"github.com/urpc/uio/internal/blockio"
	"github.com/urpc/uio/internal/bytebuf"
)

// Buffer is reusable byte storage that can be transferred to a connection
// with [Conn.WriteOwned]. Newly acquired writable capacity is not zeroed; every
// committed byte must be initialized. A Buffer must not be used after its
// ownership is transferred or it is released.
type Buffer = bytebuf.Buffer

// SharedBuffer holds an immutable, read-only byte payload that can be sent to
// many connections via [WriteShared] or [BlockingWriter.WriteShared] without
// copying payload bytes or allocating per connection. Call [SharedBuffer.Release]
// once the broadcast loop finishes; the underlying pooled storage returns to
// the pool automatically after all in-flight connection sends complete.
type SharedBuffer = bytebuf.SharedBuffer

// AcquireBuffer returns an empty pooled buffer with at least capacity bytes of
// writable space. Release it when ownership is not transferred to WriteOwned
// or FreezeBuffer.
func AcquireBuffer(capacity int) *Buffer {
	if capacity < 0 {
		panic("uio: negative buffer capacity")
	}
	return bytebuf.AcquireBuffer(capacity)
}

// ReleaseBuffer returns an unsubmitted buffer to the pool. It must be called at
// most once, and the buffer must not be used afterward.
func ReleaseBuffer(buffer *Buffer) { bytebuf.ReleaseBuffer(buffer) }

// FreezeBuffer seals buffer into an immutable [SharedBuffer] and clears *buffer
// in place so subsequent mutations or ReleaseBuffer calls on buffer cannot
// affect the frozen broadcast payload.
func FreezeBuffer(buffer *Buffer) *SharedBuffer { return bytebuf.FreezeBuffer(buffer) }

// NewSharedBuffer copies p once into a pooled Buffer and freezes it into an
// immutable [SharedBuffer]. The caller may freely mutate or reuse p afterward.
func NewSharedBuffer(p []byte) *SharedBuffer { return bytebuf.NewSharedBuffer(p) }

// WriteShared submits sb to conn without copying payload bytes on stream
// connections (and transparently passes through any protocol [Layer] stack or
// multiplexed stream). Reference counting is handled internally across all
// success, backpressure ([ErrOutboundOverflow]), and close paths.
func WriteShared(conn Lower, sb *SharedBuffer) (int, error) {
	if sb == nil {
		return 0, nil
	}
	view := sb.AcquireView()
	if view.Len() == 0 {
		return 0, nil
	}
	n, err := conn.WriteOwned(view)
	if errors.Is(err, ErrOutboundOverflow) {
		ReleaseBuffer(view)
	}
	return n, err
}

// TryWriteOwned submits buffer to conn without blocking even when
// Events.BlockingWrite is enabled. If conn's outbound window is full or a
// BlockingWriter is currently contended/parked on conn, it returns
// ErrOutboundOverflow without releasing buffer so the caller can fall back to
// BlockingWriter.WriteOwnedContext.
func TryWriteOwned(conn Conn, buffer *Buffer) (int, error) {
	if fdc, ok := conn.(*fdConn); ok {
		if !fdc.hasBlockWriter() {
			return fdc.writeOwnedNonBlocking(buffer)
		}
		n, err := fdc.ensureBlockWriter().TryWriteOwned(buffer)
		if err == blockio.ErrOverflow {
			return n, ErrOutboundOverflow
		}
		return n, err
	}
	if conn == nil {
		ReleaseBuffer(buffer)
		return 0, ErrUnsupported
	}
	bw, err := NewBlockingWriter(conn)
	if err != nil {
		return conn.WriteOwned(buffer)
	}
	return bw.TryWriteOwned(buffer)
}
