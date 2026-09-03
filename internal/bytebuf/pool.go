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

import "github.com/urpc/uio/internal/pool"

type bufferPoolKind uint8

const (
	defaultBufferPool bufferPoolKind = iota
	ownedBufferPool
)

// Ordinary copied payloads stay bounded at 64 KiB. Explicit owned buffers may
// retain a frame up to 16 MiB without expanding retention for every Write.
var bufferPool = pool.New[*Buffer](64 << 10)
var largeOwnedBufferPool = pool.New[*Buffer](16 << 20)

func getBuffer(capacity int) *Buffer {
	return getBufferFrom(bufferPool, defaultBufferPool, capacity)
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

// AcquireBuffer returns an empty pooled buffer with at least capacity bytes of
// writable space. Release it with ReleaseBuffer when ownership is not
// transferred to a CompositeBuffer.
func AcquireBuffer(capacity int) *Buffer {
	return getBufferFrom(largeOwnedBufferPool, ownedBufferPool, capacity)
}

func putBuffer(buffer *Buffer) {
	kind := buffer.poolKind
	buffer.Reset()
	if kind == ownedBufferPool {
		largeOwnedBufferPool.Put(buffer, buffer.Cap())
		return
	}
	bufferPool.Put(buffer, buffer.Cap())
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
