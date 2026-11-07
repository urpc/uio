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

package bytebuf

import (
	"math/bits"
	"sync"

	"github.com/urpc/uio/internal/pool"
)

// bufferPoolKind remembers which retention policy must receive a released block.
type bufferPoolKind uint8

const (
	defaultBufferPool bufferPoolKind = iota
	ownedBufferPool
	sharedViewPool
)

// Ordinary copied payloads stay bounded at 64 KiB. Explicit owned buffers and
// large gathered blocks up to 16 MiB use 1/8-octave sub-classes above 64 KiB so
// power-of-two payloads plus small protocol headers (e.g. 1 MiB + 14 B or
// 4 MiB + 11 B) round up by at most 12.5% instead of doubling to 2 MiB / 8 MiB.
const (
	maxCopiedBlock = 64 << 10 // 2^16
	maxOwnedBlock  = 16 << 20 // 2^24
	largeBaseShift = 16       // log2(maxCopiedBlock)
	largeSubBits   = 3        // 8 page-aligned sub-classes per octave
	largeOctaves   = 24 - largeBaseShift
	largeClasses   = largeOctaves << largeSubBits // 64 classes covering (64 KiB, 16 MiB]
)

var bufferPool = pool.New[*Buffer](maxCopiedBlock)
var largeOwnedPools [largeClasses]sync.Pool

// largeSubClass maps size in (maxCopiedBlock, maxOwnedBlock] to its 1/8-octave
// rounded capacity (always a multiple of 8 KiB) and its bucket index in [0, 64).
func largeSubClass(size int) (rounded int, idx int) {
	u := uint(size - 1)
	k := bits.Len(u) - 1
	shift := uint(k - largeSubBits)
	stepMask := (uint(1) << shift) - 1
	rounded = int((u | stepMask) + 1)
	sub := int((u >> shift) & ((1 << largeSubBits) - 1))
	idx = (k-largeBaseShift)<<largeSubBits + sub
	return rounded, idx
}

func getBuffer(capacity int) *Buffer {
	return getBufferFrom(bufferPool, defaultBufferPool, capacity)
}

// getBlock returns an empty block for bytes copied into a composite buffer.
// Blocks too large for the ordinary pool come from the owned pool, so a large
// copied write or ReserveInbound gather is reused without 2x rounding bloat.
func getBlock(capacity int) *Buffer {
	if capacity > maxCopiedBlock {
		return getLargeOwnedBuffer(capacity)
	}
	return getBuffer(capacity)
}

func getBufferFrom(owner *pool.Pool[*Buffer], kind bufferPoolKind, capacity int) *Buffer {
	buffer, n := owner.Get(capacity)
	if nil != buffer {
		buffer.poolKind = kind
		return buffer
	}
	buffer = NewBuffer(make([]byte, 0, n))
	buffer.poolKind = kind
	return buffer
}

func getLargeOwnedBuffer(capacity int) *Buffer {
	if capacity > maxOwnedBlock {
		buffer := NewBuffer(make([]byte, 0, capacity))
		buffer.poolKind = ownedBufferPool
		return buffer
	}
	rounded, idx := largeSubClass(capacity)
	if v := largeOwnedPools[idx].Get(); v != nil {
		buffer := v.(*Buffer)
		buffer.poolKind = ownedBufferPool
		return buffer
	}
	buffer := NewBuffer(make([]byte, 0, rounded))
	buffer.poolKind = ownedBufferPool
	return buffer
}

// AcquireBuffer returns an empty pooled buffer with at least capacity bytes of
// writable space. Release it with ReleaseBuffer when ownership is not
// transferred to a CompositeBuffer.
func AcquireBuffer(capacity int) *Buffer {
	if capacity <= maxCopiedBlock {
		return getBufferFrom(bufferPool, ownedBufferPool, capacity)
	}
	return getLargeOwnedBuffer(capacity)
}

func putBuffer(buffer *Buffer) {
	if buffer.poolKind == sharedViewPool {
		releaseSharedView(buffer)
		return
	}
	kind := buffer.poolKind
	buffer.Reset()
	capBytes := buffer.Cap()
	if kind == ownedBufferPool && capBytes > maxCopiedBlock {
		if capBytes <= maxOwnedBlock {
			if rounded, idx := largeSubClass(capBytes); rounded == capBytes {
				largeOwnedPools[idx].Put(buffer)
			}
		}
		return
	}
	bufferPool.Put(buffer, capBytes)
}

// CloneBuffer returns a pooled buffer containing one copy of p. The caller
// owns the returned buffer until it is appended to a CompositeBuffer or
// released with ReleaseBuffer.
func CloneBuffer(p []byte) *Buffer {
	buffer := getBuffer(len(p))
	_, _ = buffer.Write(p)
	return buffer
}

// CloneBuffers returns one owned buffer containing one copy of every segment.
func CloneBuffers(vec [][]byte, size int) *Buffer {
	buffer := getBuffer(size)
	for _, segment := range vec {
		_, _ = buffer.Write(segment)
	}
	return buffer
}

// CloneBuffersFrom copies size bytes from vec after skipping the first skip
// bytes into one owned buffer.
func CloneBuffersFrom(vec [][]byte, skip, size int) *Buffer {
	buffer := getBuffer(size)
	for _, segment := range vec {
		if skip >= len(segment) {
			skip -= len(segment)
			continue
		}
		_, _ = buffer.Write(segment[skip:])
		skip = 0
	}
	return buffer
}

// ReleaseBuffer returns an owned buffer to the pool.
func ReleaseBuffer(buffer *Buffer) {
	if buffer != nil {
		putBuffer(buffer)
	}
}
