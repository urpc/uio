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

import "syscall"

// pendingNetworkError reports the network errors Linux passes from a pending
// connection through accept; accept(2) asks to treat them like EAGAIN.
func pendingNetworkError(errno syscall.Errno) bool {
	switch errno {
	case syscall.ENETDOWN, syscall.ENOPROTOOPT, syscall.EHOSTDOWN, syscall.ENONET,
		syscall.EHOSTUNREACH, syscall.EOPNOTSUPP, syscall.ENETUNREACH:
		return true
	}
	return false
}
