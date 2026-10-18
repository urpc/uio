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

	"github.com/petermattis/goid"
)

// Only long-lived event loops are registered. This is queried by rare
// synchronous APIs that would otherwise wait for another event loop; it is
// never updated around application callbacks or connection-task dispatch.
var activeEventLoops sync.Map

// currentGoroutineID recognizes the callback, connection task, or event loop
// that already owns an operation. It never provides synchronization: the
// callback mutex or connection scheduler must establish ownership first.
// Keep the runtime-dependent representation isolated here.
func currentGoroutineID() int64 {
	return goid.Get()
}

func isEventLoopGoroutine() bool {
	_, ok := activeEventLoops.Load(currentGoroutineID())
	return ok
}
