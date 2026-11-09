//go:build linux && race && (amd64 || arm64 || riscv64 || loong64 || ppc64 || ppc64le || s390x || mips64 || mips64le)

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
	"runtime"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Race builds keep runtime-accounted syscalls so the race detector's syscall
// synchronization annotations remain intact.
func sendtoSyscall(fd int, pkt []byte, name *byte, nameLen uint32) (int, error) {
	for {
		n, _, errno := unix.Syscall6(
			unix.SYS_SENDTO,
			uintptr(fd),
			uintptr(unsafe.Pointer(&pkt[0])),
			uintptr(len(pkt)),
			0,
			uintptr(unsafe.Pointer(name)),
			uintptr(nameLen),
		)
		runtime.KeepAlive(pkt)
		if errno == unix.EINTR {
			continue
		}
		if errno != 0 {
			return 0, errno
		}
		return int(n), nil
	}
}

func sendmmsgSyscall(fd int, messages []mmsghdr) (int, error) {
	sent, _, errno := unix.Syscall6(unix.SYS_SENDMMSG, uintptr(fd), uintptr(unsafe.Pointer(&messages[0])), uintptr(len(messages)), 0, 0, 0)
	runtime.KeepAlive(messages)
	if errno != 0 {
		return int(sent), errno
	}
	return int(sent), nil
}

func recvmmsgSyscall(fd int, messages []mmsghdr) (int, error) {
	got, _, errno := unix.Syscall6(unix.SYS_RECVMMSG, uintptr(fd), uintptr(unsafe.Pointer(&messages[0])), uintptr(len(messages)), uintptr(unix.MSG_TRUNC), 0, 0)
	runtime.KeepAlive(messages)
	if errno != 0 {
		return int(got), errno
	}
	return int(got), nil
}
