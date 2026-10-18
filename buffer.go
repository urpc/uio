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

import "github.com/urpc/uio/internal/bytebuf"

// Buffer is reusable byte storage that can be transferred to a connection
// with [Conn.WriteOwned]. Newly acquired writable capacity is not zeroed; every
// committed byte must be initialized. A Buffer must not be used after its
// ownership is transferred or it is released.
type Buffer = bytebuf.Buffer

// AcquireBuffer returns an empty pooled buffer with at least capacity bytes of
// writable space. Release it when ownership is not transferred to WriteOwned.
func AcquireBuffer(capacity int) *Buffer {
	if capacity < 0 {
		panic("uio: negative buffer capacity")
	}
	return bytebuf.AcquireBuffer(capacity)
}

// ReleaseBuffer returns an unsubmitted buffer to the pool. It must be called at
// most once, and the buffer must not be used afterward.
func ReleaseBuffer(buffer *Buffer) { bytebuf.ReleaseBuffer(buffer) }
