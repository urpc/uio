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

import (
	"sync"

	"github.com/urpc/uio/internal/fdmap"
)

const (
	eventBatch              = 1024
	defaultTCPKeepAliveSecs = 15
	ioStopBit               = uint64(1 << 63)
)

var unixFdMap *fdmap.Map[fdConn]
var unixFdMapOnce sync.Once

func newFdMap() *fdmap.Map[fdConn] {
	// Unix can index directly by fd, so all loops share one sparse table.
	// Windows falls back to a typed, mutex-protected map per loop.
	if fdmap.UseSingleInstance {
		unixFdMapOnce.Do(func() { unixFdMap = fdmap.NewMap[fdConn]() })
		return unixFdMap
	}
	return fdmap.NewMap[fdConn]()
}

// cacheLineSize separates fields that every connection turn writes from the
// read-mostly fields next to them, so reading the latter does not keep
// missing on a line other CPUs just modified.
const cacheLineSize = 64
