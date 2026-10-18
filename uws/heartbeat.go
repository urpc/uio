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

package uws

import (
	"sync"
	"time"
)

// heartbeatRunner owns the scanner that pings a Server's or Dialer's
// connections. start replaces any prior scanner and waits for it to exit
// before publishing the new lifecycle channels.
type heartbeatRunner struct {
	mu     sync.Mutex
	stopCh chan struct{}
	doneCh chan struct{}
}

// start launches a scanner over connections. A nil connections map or a
// non-positive interval leaves the runner stopped.
func (r *heartbeatRunner) start(connections *sync.Map, interval, timeout time.Duration) {
	if connections == nil || interval <= 0 {
		return
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	r.mu.Lock()
	previousStop := r.stopCh
	previousDone := r.doneCh
	if previousStop != nil {
		close(previousStop)
	}
	if previousDone != nil {
		<-previousDone
	}
	r.stopCh = stop
	r.doneCh = done
	go r.run(connections, interval, timeout, stop, done)
	r.mu.Unlock()
}

// stop ends the scanner and waits for it to exit.
func (r *heartbeatRunner) stop() {
	r.mu.Lock()
	stop := r.stopCh
	done := r.doneCh
	r.stopCh = nil
	r.doneCh = nil
	if stop != nil {
		close(stop)
	}
	if done != nil {
		<-done
	}
	r.mu.Unlock()
}

func (r *heartbeatRunner) run(connections *sync.Map, interval, timeout time.Duration, stop <-chan struct{}, done chan<- struct{}) {
	defer close(done)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case now := <-ticker.C:
			if !scanHeartbeat(connections, now, timeout, stop) {
				return
			}
		case <-stop:
			return
		}
	}
}
