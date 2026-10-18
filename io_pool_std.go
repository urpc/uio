//go:build windows || stdio

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

// stdio keeps its dedicated blocking read/write goroutines. The native
// connection-task pool is intentionally absent on this backend.
type ioTaskPool struct{}

func newIOTaskPool(Executor) *ioTaskPool                          { return nil }
func (pool *ioTaskPool) stop()                                    {}
func (pool *ioTaskPool) submitBatch([]IOTask) bool                { return false }
func (pool *ioTaskPool) submitConnBatch([]*fdConn, []IOTask) bool { return false }
