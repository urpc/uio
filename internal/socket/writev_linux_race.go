//go:build linux && race

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

const writevBatchLimit = maxWritevBuffers

// Writev retains x/sys's race synchronization annotations in race builds.
// Longer vectors than one writev takes go out in batches.
func Writev(fd int, buffers [][]byte) (int, error) {
	switch len(buffers) {
	case 0:
		return 0, nil
	case 1:
		return syscall.Write(fd, buffers[0])
	}
	if len(buffers) > writevBatchLimit {
		return writevBatches(fd, buffers)
	}
	return unix.Writev(fd, buffers)
}
