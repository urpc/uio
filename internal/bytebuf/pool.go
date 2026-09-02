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

// Payloads up to 64 KiB use size-classed pools; larger buffers are not kept.
var bufferPool = pool.New[*Buffer](65536)

func getBuffer(capacity int) *Buffer {
	buffer, n := bufferPool.Get(capacity)
	if nil != buffer {
		return buffer
	}
	return NewBuffer(make([]byte, 0, n))
}

func putBuffer(buffer *Buffer) {
	buffer.Reset()
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
