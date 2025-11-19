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

package poller

// Events is a bitmask for read/write/error events. Poller implementations
// may pass these to OnEvent callback to handle multiple events
// in a single call (avoids double Get/dispatch for read+write).
type Events uint32

const (
	ReadEvents Events = 1 << iota
	WriteEvents
)

type EventHandler interface {
	OnEvent(ep *NetPoller, fd int, events Events)
	OnClose(ep *NetPoller, err error)
}
