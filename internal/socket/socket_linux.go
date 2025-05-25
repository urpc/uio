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
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// SetKeepAlivePeriod sets period between keep-alives.
func SetKeepAlivePeriod(fd int, secs int) error {
	if err := syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_KEEPALIVE, 1); err != nil {
		return os.NewSyscallError("setsockopt", err)
	}
	if err := syscall.SetsockoptInt(fd, syscall.IPPROTO_TCP, syscall.TCP_KEEPINTVL, secs); err != nil {
		return os.NewSyscallError("setsockopt", err)
	}
	if err := syscall.SetsockoptInt(fd, syscall.IPPROTO_TCP, syscall.TCP_KEEPIDLE, secs); nil != err {
		return os.NewSyscallError("setsockopt", err)
	}
	return nil
}

// Writev calls writev() on Linux.
func Writev(fd int, iov [][]byte) (int, error) {
	switch len(iov) {
	case 0:
		return 0, nil
	case 1:
		return syscall.Write(fd, iov[0])
	default:
		return unix.Writev(fd, iov)
	}
}
