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

import (
	"errors"
	"syscall"
)

// setSocketBuffer sizes one kernel socket buffer: SO_RCVBUF when read, else
// SO_SNDBUF. Linux clamps oversized requests automatically, whereas the BSDs
// return ENOBUFS/EINVAL above kern.ipc.maxsockbuf; halving down to 64 KiB
// adapts gracefully across BSDs without failing default Serve/Dial calls.
func setSocketBuffer(fd uintptr, read bool, bytes int) error {
	option := syscall.SO_SNDBUF
	if read {
		option = syscall.SO_RCVBUF
	}
	for cur := bytes; ; {
		err := syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, option, cur)
		if err == nil {
			return nil
		}
		if (errors.Is(err, syscall.ENOBUFS) || errors.Is(err, syscall.EINVAL)) && cur > 64<<10 {
			cur = max(cur/2, 64<<10)
			continue
		}
		return err
	}
}
