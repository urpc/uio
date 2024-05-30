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
	"unsafe"

	"golang.org/x/sys/unix"
)

// Writev invokes the writev system call directly.
//
// Note that SYS_WRITEV is about to be deprecated on Darwin
// and the Go team suggested to use libSystem wrappers instead of direct system-calls,
// hence, this way to implement the writev might not be backward-compatible in the future.
func Writev(fd int, vec [][]byte) (int, error) {
	switch len(vec) {
	case 0:
		return 0, nil
	case 1:
		return unix.Write(fd, vec[0])
	default:
		iovecs := make([]unix.Iovec, 0, minIovec)
		iovecs = appendBytes(iovecs, vec)
		n, _, err := unix.RawSyscall(unix.SYS_WRITEV, uintptr(fd), uintptr(unsafe.Pointer(&iovecs[0])), uintptr(len(iovecs))) //nolint:staticcheck
		if err != 0 {
			return int(n), err
		}
		return int(n), nil
	}
}

// minIovec is the size of the small initial allocation used by
// Readv, Writev, etc.
//
// This small allocation gets stack allocated, which lets the
// common use case of len(iovs) <= minIovs avoid more expensive
// heap allocations.
const minIovec = 8

var _zero uintptr

// appendBytes converts bs to Iovecs and appends them to vecs.
func appendBytes(vecs []unix.Iovec, bs [][]byte) []unix.Iovec {
	for _, b := range bs {
		var v unix.Iovec
		v.SetLen(len(b))
		if len(b) > 0 {
			v.Base = &b[0]
		} else {
			v.Base = (*byte)(unsafe.Pointer(&_zero))
		}
		vecs = append(vecs, v)
	}
	return vecs
}
