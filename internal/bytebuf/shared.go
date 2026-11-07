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
	"sync"
	"sync/atomic"
	"unsafe"
)

// sharedView places Buffer at byte offset 0 so ordinary *Buffer values stay
// within their 40-byte size budget while putBuffer can recover the enclosing
// view in O(1) whenever poolKind == sharedViewPool.
type sharedView struct {
	buf    Buffer
	parent *SharedBuffer
}

// Compile-time guarantee that sharedView.buf is at byte offset 0.
var _ [0 - unsafe.Offsetof(sharedView{}.buf)]byte

var sharedViewStructPool = sync.Pool{
	New: func() any { return new(sharedView) },
}

// SharedBuffer holds an immutable, read-only byte payload that can be queued
// across any number of connections without copying or per-connection heap
// allocations. Its backing storage returns to UIO's buffer pool once the
// broadcaster calls Release and all in-flight connection views finish sending.
//
// SharedBuffer instances themselves are intentionally not pooled so a late
// buggy Release or AcquireView after Release always panics deterministically
// instead of racing a recycled handle (no ABA hazard).
type SharedBuffer struct {
	backing  *Buffer
	data     []byte       // [0:n:n] slice: len == cap, so Available() is always 0
	refs     atomic.Int64 // 1 (broadcaster scope) + K (in-flight views)
	released atomic.Bool  // guards the broadcaster's single Release call
}

// FreezeBuffer seals buf into an immutable SharedBuffer and clears *buf in
// place so subsequent mutations or ReleaseBuffer calls on buf cannot affect the
// frozen broadcast bytes.
func FreezeBuffer(buf *Buffer) *SharedBuffer {
	sb := &SharedBuffer{}
	sb.InitFreeze(buf)
	return sb
}

// InitFreeze seals buf into sb in place and clears *buf so subsequent mutations
// or ReleaseBuffer calls on buf cannot affect the frozen broadcast bytes.
func (sb *SharedBuffer) InitFreeze(buf *Buffer) {
	if buf != nil && buf.poolKind == sharedViewPool {
		panic("bytebuf.Buffer: cannot freeze read-only shared view")
	}
	sb.refs.Store(1)
	if buf == nil || buf.Len() == 0 {
		if buf != nil && buf.Cap() > 0 {
			backing := &Buffer{
				buf:      buf.buf,
				off:      buf.off,
				poolKind: buf.poolKind,
			}
			*buf = Buffer{}
			putBuffer(backing)
		} else if buf != nil {
			*buf = Buffer{}
		}
		return
	}
	raw := buf.Bytes()
	n := len(raw)
	backing := &Buffer{
		buf:      buf.buf,
		off:      buf.off,
		poolKind: buf.poolKind,
	}
	*buf = Buffer{}
	sb.backing = backing
	sb.data = raw[:n:n]
}

// InitOwned initializes sb directly from an internal freshly-acquired *Buffer
// that is never exposed to external callers, avoiding the extra *Buffer header
// clone allocation.
func (sb *SharedBuffer) InitOwned(buf *Buffer) {
	if buf != nil && buf.poolKind == sharedViewPool {
		panic("bytebuf.Buffer: cannot freeze read-only shared view")
	}
	sb.refs.Store(1)
	if buf == nil || buf.Len() == 0 {
		if buf != nil {
			ReleaseBuffer(buf)
		}
		return
	}
	raw := buf.Bytes()
	n := len(raw)
	sb.backing = buf
	sb.data = raw[:n:n]
}

// NewSharedBuffer copies p once into a pooled Buffer and freezes it into an
// immutable SharedBuffer. The caller may freely mutate or reuse p afterward.
func NewSharedBuffer(p []byte) *SharedBuffer {
	sb := &SharedBuffer{}
	if len(p) == 0 {
		sb.refs.Store(1)
		return sb
	}
	buf := AcquireBuffer(len(p))
	_, _ = buf.Write(p)
	sb.InitOwned(buf)
	return sb
}

// Bytes returns the immutable payload slice with capacity capped to length.
func (sb *SharedBuffer) Bytes() []byte {
	if sb == nil {
		return nil
	}
	return sb.data
}

// Len returns the byte length of the shared payload.
func (sb *SharedBuffer) Len() int {
	if sb == nil {
		return 0
	}
	return len(sb.data)
}

// AcquireView borrows a pooled per-connection read-only *Buffer view over sb.
// It panics if sb is nil or has already been released.
func (sb *SharedBuffer) AcquireView() *Buffer {
	if sb == nil {
		panic("uio: nil SharedBuffer")
	}
	for {
		r := sb.refs.Load()
		if r <= 0 || sb.released.Load() {
			panic("uio: SharedBuffer used after Release")
		}
		if sb.refs.CompareAndSwap(r, r+1) {
			break
		}
	}
	if len(sb.data) == 0 {
		sb.decRef()
		return &Buffer{}
	}
	v := sharedViewStructPool.Get().(*sharedView)
	v.buf = Buffer{
		buf:      sb.data,
		poolKind: sharedViewPool,
	}
	v.parent = sb
	return &v.buf
}

// Release ends the broadcaster's hold on sb. Once all in-flight connection
// views have also completed, the underlying pooled buffer is recycled. Calling
// Release more than once panics without decrementing in-flight references.
func (sb *SharedBuffer) Release() {
	if sb == nil {
		return
	}
	if !sb.released.CompareAndSwap(false, true) {
		panic("uio: SharedBuffer.Release called more than once")
	}
	sb.decRef()
}

func (sb *SharedBuffer) decRef() {
	if sb.refs.Add(-1) != 0 {
		return
	}
	backing := sb.backing
	sb.backing = nil
	sb.data = nil
	if backing != nil {
		putBuffer(backing)
	}
}

func releaseSharedView(b *Buffer) {
	v := (*sharedView)(unsafe.Pointer(b))
	parent := v.parent
	v.buf = Buffer{}
	v.parent = nil
	sharedViewStructPool.Put(v)
	if parent != nil {
		parent.decRef()
	}
}
