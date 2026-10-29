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

// The recorder lives without a platform tag: the stdio and Windows wake
// tests use it too, and those builds see no unix test files.

package uio

import (
	"sync"
	"sync/atomic"
	"time"
)

// wakeRecorder counts read turns and timer fires, and records when they ran.
type wakeRecorder struct {
	mu     sync.Mutex
	times  []time.Time
	timers []time.Time
	opened atomic.Int32
	closed atomic.Int32
}

func (r *wakeRecorder) OnOpen(Conn) { r.opened.Add(1) }

func (r *wakeRecorder) OnRead(Conn) error {
	r.mu.Lock()
	r.times = append(r.times, time.Now())
	r.mu.Unlock()
	return nil
}

func (r *wakeRecorder) OnTimer(Conn) error {
	r.mu.Lock()
	r.timers = append(r.timers, time.Now())
	r.mu.Unlock()
	return nil
}

func (r *wakeRecorder) OnClose(Conn, error) { r.closed.Add(1) }

func (r *wakeRecorder) timerFires() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.timers)
}

func (r *wakeRecorder) fires() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.times)
}

func (r *wakeRecorder) firstAt() time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.times[0]
}
