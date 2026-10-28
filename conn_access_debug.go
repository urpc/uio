//go:build race

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

// The inbound-scope diagnostics exist only in the race-enabled build. They
// record that a connection's inbound callback is in progress and fail loudly
// when the inbound buffer is touched outside one. They are misuse diagnostics,
// not a safety mechanism: the guarantee that a connection's inbound buffer is
// serialized comes from the connection turn (native) and callbackMu (stdio),
// and these check only that a callback runs — not who calls. A foreign
// goroutine reading while a callback runs passes, and a borrowed slice used
// after its callback returned is not detected. Builds without the race
// detector compile the whole mechanism out; see conn_access_release.go.

import "sync/atomic"

// inboundScope records that an inbound callback is in progress.
type inboundScope struct {
	inboundLive atomic.Bool
}

// beginInboundCallback marks the connection's inbound callback in progress,
// reporting whether this call entered it, so nested internal helpers keep the
// outer caller's scope intact.
func (fc *commonConn) beginInboundCallback() bool {
	return fc.inboundLive.CompareAndSwap(false, true)
}

func (fc *commonConn) endInboundCallback(started bool) {
	if started {
		fc.inboundLive.Store(false)
	}
}

func (fc *commonConn) assertInboundAccess() {
	// Unregistered connections are used by low-level helpers and tests before an
	// owner exists. Registered connections always have both fields.
	if fc.loop == nil || fc.events() == nil {
		return
	}
	if fc.inboundLive.Load() {
		return
	}
	panic("uio: inbound access outside a connection callback")
}
