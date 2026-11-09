// The MIT License (MIT)
//
// # Copyright (c) 2015 xtaci
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in all
// copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
// SOFTWARE.

package kcp

import (
	"errors"
	"unsafe"

	"github.com/urpc/uio/internal/plocal"
)

// pre-allocated error to avoid repeated allocations
var errBufferSizeMismatch = errors.New("buffer size mismatch")

// A system-wide packet buffer shared among sending and receiving using per-P
// lock-free caching of fixed packetBuf pointers (zero slice-header boxing
// allocations on Put, and wrapped in a struct so Go's generic shape stenciler
// never slices a go.shape.*uint8 pointer-to-array return value).
var defaultBufferPool = newBufferPool(mtuLimit)

type packetBuf struct {
	data [mtuLimit]byte
}

type bufferPool struct {
	cache plocal.Cache[*packetBuf]
}

// newBufferPool creates a new buffer pool with mtuLimit-byte packet buffers.
func newBufferPool(_ int) *bufferPool {
	bp := &bufferPool{}
	bp.cache.Init(func() *packetBuf {
		return new(packetBuf)
	})
	return bp
}

// Get retrieves a buffer from the pool.
func (bp *bufferPool) Get() []byte {
	b := bp.cache.Get()
	return b.data[:]
}

// Put returns a buffer to the pool without boxing a slice header into any.
func (bp *bufferPool) Put(buf []byte) error {
	if cap(buf) != mtuLimit {
		return errBufferSizeMismatch
	}
	buf = buf[:mtuLimit]
	bp.cache.Put((*packetBuf)(unsafe.Pointer(&buf[0])))
	return nil
}
