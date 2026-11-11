//go:build linux || darwin || freebsd || dragonfly

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

	"golang.org/x/sys/unix"
)

// SupportsSendfile reports whether the platform provides a kernel sendfile(2)
// implementation in this package.
const SupportsSendfile = true

// maxSendfileChunk caps a single sendfile(2) system call at 1 GiB so the byte
// count fits safely inside a signed 32-bit int across all Unix kernels.
const maxSendfileChunk = 1 << 30

// Sendfile transfers up to count bytes from inFD starting at *offset to outFD
// and advances *offset by the number of bytes written on all platforms
// (including Darwin, whose unix.Sendfile wrapper does not mutate *offset).
func Sendfile(outFD, inFD int, offset *int64, count int) (int, error) {
	if count <= 0 {
		return 0, nil
	}
	if count > maxSendfileChunk {
		count = maxSendfileChunk
	}
	for {
		orig := *offset
		off := orig
		n, err := unix.Sendfile(outFD, inFD, &off, count)
		if n > 0 {
			*offset = orig + int64(n)
		} else {
			n = 0
		}
		if err == syscall.EINTR {
			if n > 0 {
				return n, nil
			}
			continue
		}
		return n, err
	}
}

// IsSendfileUnsupported reports whether err indicates that the kernel or file
// descriptor does not support sendfile(2) and the caller should fall back to
// user-space reads.
func IsSendfileUnsupported(err error) bool {
	if err == syscall.ENOTSUP {
		return true
	}
	switch err {
	case syscall.ENOSYS, syscall.EINVAL, syscall.EOPNOTSUPP, syscall.ENOTSOCK, syscall.ESPIPE, syscall.ENODEV:
		return true
	default:
		return false
	}
}

// IsSendfileSourceError reports an error sendfile(2) met reading its file,
// not writing its socket: the file is not open for reading, failed to read, or
// cannot be read at the offset. The socket is as it was.
func IsSendfileSourceError(err error) bool {
	switch err {
	case syscall.EBADF, syscall.EIO, syscall.EOVERFLOW, syscall.ESPIPE, syscall.ENXIO, syscall.EFAULT:
		return true
	default:
		return false
	}
}
