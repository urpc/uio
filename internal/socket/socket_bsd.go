//go:build darwin || netbsd || freebsd || openbsd || dragonfly

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
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// stackWritevLimit is the iovec count Writev keeps on the stack. UIO hands a
// writev at most nativeWriteVecLimit buffers, so the common batch never
// allocates.
const stackWritevLimit = 64

// writevBatchLimit keeps every batch within stackWritevLimit stack-backed
// iovecs. Longer vectors go out in batches of that size.
const writevBatchLimit = stackWritevLimit

// Writev invokes the writev system call directly. Longer vectors than one
// writev takes go out in batches.
//
// The iovecs live on the stack: x/sys's RawSyscall passes its arguments as
// uintptr, so the array it points at does not escape, and a batch that fits
// stackWritevLimit allocates nothing.
//
// Note that SYS_WRITEV is about to be deprecated on Darwin
// and the Go team suggested to use libSystem wrappers instead of direct system-calls,
// hence, this way to implement the writev might not be backward-compatible in the future.
func Writev(fd int, vec [][]byte) (int, error) {
	switch len(vec) {
	case 0:
		return 0, nil
	case 1:
		return syscall.Write(fd, vec[0])
	}
	if len(vec) > writevBatchLimit {
		return writevBatches(fd, vec)
	}
	var storage [stackWritevLimit]unix.Iovec
	iovecs := storage[:len(vec)]
	appendBytes(iovecs, vec)
	n, _, err := unix.RawSyscall(unix.SYS_WRITEV, uintptr(fd), uintptr(unsafe.Pointer(&iovecs[0])), uintptr(len(iovecs))) //nolint:staticcheck
	if err != 0 {
		return int(n), err
	}
	return int(n), nil
}

var _zero uintptr

// appendBytes fills vecs, one iovec per element of bs; vecs must hold at
// least len(bs).
func appendBytes(vecs []unix.Iovec, bs [][]byte) {
	for index, b := range bs {
		vecs[index].SetLen(len(b))
		if len(b) > 0 {
			vecs[index].Base = &b[0]
		} else {
			vecs[index].Base = (*byte)(unsafe.Pointer(&_zero))
		}
	}
}
