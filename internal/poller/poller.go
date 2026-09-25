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

// Package poller normalizes epoll, kqueue, and blocking-backend wake semantics.
package poller

import "errors"

// Events is a bitmask for read/write/error events. Poller implementations
// may pass these to OnEvent callback to handle multiple events
// in a single call (avoids double Get/dispatch for read+write).
type Events uint32

const (
	ReadEvents Events = 1 << iota
	WriteEvents
)

// Interest is the desired readiness state for a watched descriptor.
type Interest uint8

const (
	Readable Interest = 1 << iota
	Writable
)

// Event is one normalized readiness notification. A backend may use level or
// edge triggering; users must follow the registration contract of that fd.
// Tag echoes the value set with SetTag when the fd was registered, so a
// consumer that races with descriptor reuse can recognize a stale event.
// Backends that cannot carry it report zero.
type Event struct {
	FD     int
	Events Events
	Tag    uint32
}

var errInvalidInterest = errors.New("poller: empty interest")

// EventHandler consumes normalized readiness and terminal poller closure.
type EventHandler interface {
	OnEvent(ep *NetPoller, fd int, events Events)
	OnClose(ep *NetPoller, err error)
}
