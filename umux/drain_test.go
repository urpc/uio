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

package umux

import (
	"bytes"
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urpc/uio"
)

// reportRecorder counts report events: byte writes and queue drains.
type reportRecorder struct {
	drains   atomic.Int64
	positive atomic.Int64
}

func (r *reportRecorder) OnBytesRead(uio.Conn, int) {}

func (r *reportRecorder) OnBytesWritten(_ uio.Conn, n int) {
	if n > 0 {
		r.positive.Add(1)
	}
}

func (r *reportRecorder) OnQueueDrained(uio.Conn) { r.drains.Add(1) }

// TestFlushDrainReportsToObserver pins the invariant the bridge's pause/resume
// relies on: when a flush moves a stream's queued bytes to the transport, the
// observer hears it, exactly as a socket leg reports the kernel draining its
// queue. Without it, a peer paused on this queue waits for a window grant that
// a merely flushed queue never produces.
func TestFlushDrainReportsToObserver(t *testing.T) {
	slow := &slowServer{opened: make(chan *Stream, 1)}
	rec := &reportRecorder{}
	sess, _ := startPair(t,
		Config{Handler: slow, WindowSize: 1 << 20},
		Config{Observer: rec, WindowSize: 1 << 20})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	st, err := sess.Open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer st.CloseWith(nil)
	writeAll(t, st, bytes.Repeat([]byte("d"), 300<<10))
	if rec.positive.Load() == 0 {
		t.Fatal("writes were not reported to the observer")
	}
	// The peer never reads, so no window update can arrive: every drain report
	// can only come from the flush emptying the queue.
	waitFor(t, "drain report", func() bool { return rec.drains.Load() > 0 })
}
