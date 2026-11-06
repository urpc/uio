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

	"github.com/urpc/uio/internal/plocal"
)

// pre-allocated error to avoid repeated allocations
var errBufferSizeMismatch = errors.New("buffer size mismatch")

// A system-wide packet buffer shared among sending and receiving using per-P
// lock-free caching of fixed *[mtuLimit]byte pointers (zero slice-header boxing
// allocations on Put).
var defaultBufferPool = newBufferPool(mtuLimit)

type bufferPool struct {
	cache plocal.Cache[*[mtuLimit]byte]
}

// newBufferPool creates a new buffer pool with mtuLimit-byte packet buffers.
func newBufferPool(_ int) *bufferPool {
	bp := &bufferPool{}
	bp.cache.Init(func() *[mtuLimit]byte {
		return new([mtuLimit]byte)
	})
	return bp
}

// Get retrieves a buffer from the pool.
func (bp *bufferPool) Get() []byte {
	return bp.cache.Get()[:]
}

// Put returns a buffer to the pool without boxing a slice header into any.
func (bp *bufferPool) Put(buf []byte) error {
	if cap(buf) != mtuLimit {
		return errBufferSizeMismatch
	}
	bp.cache.Put((*[mtuLimit]byte)(buf[:mtuLimit]))
	return nil
}
