//go:build linux || darwin || netbsd || freebsd || openbsd || dragonfly

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

// maxWritevBuffers is IOV_MAX on Linux and the BSDs: a writev or sendmsg
// with more buffers fails with EINVAL.
const maxWritevBuffers = 1024

// writevBatches writes a vector longer than writevBatchLimit as one writev
// would, without the limit on how many buffers a call takes. Each Writev
// call takes one batch, and the batches stop at the first one the socket does
// not take in full, so what is left is only what the socket refused. An error
// after some bytes went out is left for the next call: the count returned is
// always what was written, since callers queue the rest, and a would-block
// that hid the count would make them send those bytes again.
func writevBatches(fd int, buffers [][]byte) (int, error) {
	written := 0
	for len(buffers) > 0 {
		batch := buffers[:min(len(buffers), writevBatchLimit)]
		buffers = buffers[len(batch):]
		size := 0
		for _, buffer := range batch {
			size += len(buffer)
		}
		n, err := Writev(fd, batch)
		if err != nil {
			if written > 0 {
				return written, nil
			}
			return n, err
		}
		written += n
		if n < size {
			break
		}
	}
	return written, nil
}
