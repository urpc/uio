//go:build darwin && !stdio

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

package uio

import "golang.org/x/sys/unix"

// listenQueueCap is how many connections a listener's accept queue holds, or
// 0 when the kernel does not say. A burst longer than this overflows the
// queue, and the overflowing connections never reach the server.
func listenQueueCap() int {
	n, err := unix.SysctlUint32("kern.ipc.somaxconn")
	if err != nil {
		return 0
	}
	return int(n)
}
