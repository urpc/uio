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
