//go:build linux && !race && (amd64 || arm64 || riscv64 || loong64 || ppc64 || ppc64le || s390x || mips64 || mips64le)

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
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// UDP batches are non-blocking, but sendmmsg/recvmmsg can spend a meaningful
// amount of time in the kernel while processing a burst. On hosts with spare
// Ps, keep that interval on the current P just like the connected stream path.
// Small hosts retain runtime-accounted calls so a blocked batch does not pin a
// large fraction of the scheduler.
const udpRawMinProcs = 8

var udpRawCallsEnabled = runtime.GOMAXPROCS(0) >= udpRawMinProcs

func sendmmsgSyscall(fd int, messages []mmsghdr) (int, error) {
	var sent uintptr
	var errno syscall.Errno
	if udpRawCallsEnabled {
		sent, _, errno = unix.RawSyscall6(unix.SYS_SENDMMSG, uintptr(fd), uintptr(unsafe.Pointer(&messages[0])), uintptr(len(messages)), 0, 0, 0)
	} else {
		sent, _, errno = unix.Syscall6(unix.SYS_SENDMMSG, uintptr(fd), uintptr(unsafe.Pointer(&messages[0])), uintptr(len(messages)), 0, 0, 0)
	}
	runtime.KeepAlive(messages)
	if errno != 0 {
		return int(sent), errno
	}
	return int(sent), nil
}

func recvmmsgSyscall(fd int, messages []mmsghdr) (int, error) {
	var got uintptr
	var errno syscall.Errno
	if udpRawCallsEnabled {
		got, _, errno = unix.RawSyscall6(unix.SYS_RECVMMSG, uintptr(fd), uintptr(unsafe.Pointer(&messages[0])), uintptr(len(messages)), uintptr(unix.MSG_TRUNC), 0, 0)
	} else {
		got, _, errno = unix.Syscall6(unix.SYS_RECVMMSG, uintptr(fd), uintptr(unsafe.Pointer(&messages[0])), uintptr(len(messages)), uintptr(unix.MSG_TRUNC), 0, 0)
	}
	runtime.KeepAlive(messages)
	if errno != 0 {
		return int(got), errno
	}
	return int(got), nil
}
