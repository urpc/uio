//go:build linux

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

package socket

import (
	"errors"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"

	"golang.org/x/sys/unix"
)

const (
	maxSpliceChunk = 1 << 20
	splicePipeSize = 256 << 10
	// splicePipeMin is the smallest pipe worth splicing through: a user
	// over its pipe-user-pages-soft gets pipes of two pages, through which
	// splice(2) moves less per call than a copy does.
	splicePipeMin = 64 << 10
	spliceFlags   = unix.SPLICE_F_MOVE | unix.SPLICE_F_NONBLOCK
)

// errSmallPipe reports a pipe the kernel would not size for splicing.
var errSmallPipe = errors.New("socket: splice pipe below its minimum size")

// smallPipes counts the pipes AcquireSplicePipe refused as too small.
var smallPipes atomic.Int64

// SmallPipeRefusals reports how many pipes AcquireSplicePipe has refused as
// too small, so a test can tell a splice the kernel's pipe budget ruled out.
func SmallPipeRefusals() int64 { return smallPipes.Load() }

// SplicePipe is a non-blocking kernel pipe pair used as an intermediate page
// ring for zero-copy splice(2) transfers on Linux.
type SplicePipe struct {
	R int
	W int
}

var splicePipePool sync.Pool

// testPipeSize, when set, is the size of the pipes AcquireSplicePipe hands
// out, new ones each time and none pooled; see SetTestPipeSize.
var testPipeSize atomic.Int64

// SetTestPipeSize makes AcquireSplicePipe hand out new pipes of size bytes,
// none of them pooled, until the returned function restores the default. It
// lets a test reach the paths of a pipe smaller than a splice asks for.
func SetTestPipeSize(size int) (restore func()) {
	testPipeSize.Store(int64(size))
	return func() { testPipeSize.Store(0) }
}

// AcquireSplicePipe returns an empty non-blocking kernel pipe pair from the
// pool or creates a new one via pipe2(O_NONBLOCK|O_CLOEXEC). A pipe the
// kernel keeps below splicePipeMin — every new pipe of a user over
// pipe-user-pages-soft — is refused: the caller copies instead.
func AcquireSplicePipe() (*SplicePipe, error) {
	size := splicePipeSize
	if s := testPipeSize.Load(); s > 0 {
		size = int(s)
	} else if v := splicePipePool.Get(); v != nil {
		return v.(*SplicePipe), nil
	}
	var fds [2]int
	if err := unix.Pipe2(fds[:], unix.O_NONBLOCK|unix.O_CLOEXEC); err != nil {
		return nil, err
	}
	p := &SplicePipe{R: fds[0], W: fds[1]}
	if got, err := unix.FcntlInt(uintptr(fds[1]), unix.F_SETPIPE_SZ, size); err != nil || got < min(size, splicePipeMin) {
		if got, err = unix.FcntlInt(uintptr(fds[1]), unix.F_GETPIPE_SZ, 0); err != nil || got < splicePipeMin {
			CloseSplicePipe(p)
			smallPipes.Add(1)
			return nil, errSmallPipe
		}
	}
	runtime.SetFinalizer(p, CloseSplicePipe)
	return p, nil
}

// ReleaseSplicePipe returns an empty SplicePipe to the pool for reuse. Pipes
// are shared by every connection: one that may still hold bytes is closed
// with CloseSplicePipe instead, or they would reach another stream.
func ReleaseSplicePipe(p *SplicePipe) {
	if p == nil || p.R < 0 || p.W < 0 {
		return
	}
	if testPipeSize.Load() > 0 {
		CloseSplicePipe(p)
		return
	}
	splicePipePool.Put(p)
}

// CloseSplicePipe closes both descriptors of p without returning it to the pool.
func CloseSplicePipe(p *SplicePipe) {
	if p == nil {
		return
	}
	if p.R >= 0 {
		_ = syscall.Close(p.R)
		p.R = -1
	}
	if p.W >= 0 {
		_ = syscall.Close(p.W)
		p.W = -1
	}
}

// Splice moves up to count bytes between rfd and wfd in kernel space without
// copying through user memory. At least one of rfd or wfd must be a pipe.
func Splice(rfd int, roff *int64, wfd int, woff *int64, count int) (int, error) {
	if count <= 0 {
		return 0, nil
	}
	if count > maxSpliceChunk {
		count = maxSpliceChunk
	}
	for {
		n, err := unix.Splice(rfd, roff, wfd, woff, count, spliceFlags)
		if n < 0 {
			n = 0
		}
		if err == syscall.EINTR {
			if n > 0 {
				return int(n), nil
			}
			continue
		}
		return int(n), err
	}
}

// ReadSplicePipe reads up to len(dst) bytes from pipeR into dst.
func ReadSplicePipe(pipeR int, dst []byte) (int, error) {
	readTotal := 0
	for readTotal < len(dst) {
		nr, err := syscall.Read(pipeR, dst[readTotal:])
		if nr > 0 {
			readTotal += nr
		}
		if err != nil {
			if err == syscall.EINTR {
				continue
			}
			return readTotal, err
		}
		if nr == 0 {
			break
		}
	}
	return readTotal, nil
}

// IsSpliceUnsupported reports whether err indicates that the kernel or
// descriptor pair does not support splice(2) and the caller should fall back
// to user-space buffer transfers.
func IsSpliceUnsupported(err error) bool {
	switch err {
	case syscall.ENOSYS, syscall.EINVAL, syscall.EOPNOTSUPP, syscall.ESPIPE, syscall.EBADF:
		return true
	default:
		return false
	}
}
