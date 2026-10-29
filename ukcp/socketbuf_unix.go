//go:build unix

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

package ukcp

import "syscall"

// setSocketBuffer sizes one kernel socket buffer: SO_RCVBUF when read, else
// SO_SNDBUF. The kernel may adjust the value — Linux doubles it, and caps it
// at net.core.rmem_max / wmem_max.
func setSocketBuffer(fd uintptr, read bool, bytes int) error {
	option := syscall.SO_SNDBUF
	if read {
		option = syscall.SO_RCVBUF
	}
	return syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, option, bytes)
}
