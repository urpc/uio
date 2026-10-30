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

import "errors"

var (
	errWindowExceeded = errors.New("umux: flow-control window exceeded")
	errWindowInvalid  = errors.New("umux: invalid flow-control update")
)

// sendWindow is the credit a sender spends on one stream: it starts at the
// peer's initial window, every queued data byte takes from it, and a
// WindowUpdate frame grants it back.
type sendWindow struct {
	avail int64
	max   int64
}

func newSendWindow(size int64) sendWindow {
	return sendWindow{avail: size, max: size}
}

// reset sets the credit and its ceiling, as a peer's SYN/ACK does: the window
// it announces replaces the protocol-floor guess outright.
func (w *sendWindow) reset(size int64) {
	w.avail, w.max = size, size
}

// take spends n bytes of credit, reporting whether the window covered them.
func (w *sendWindow) take(n int64) bool {
	if n < 0 || n > w.avail {
		return false
	}
	w.avail -= n
	return true
}

// grant adds a peer's WindowUpdate. A grant that grows past the configured
// window is a protocol violation.
func (w *sendWindow) grant(delta int64) error {
	if delta <= 0 {
		return errWindowInvalid
	}
	if w.avail+delta > w.max {
		return errWindowExceeded
	}
	w.avail += delta
	return nil
}

// available reports the credit left.
func (w *sendWindow) available() int64 { return w.avail }

// recvWindow is a receiver's view of one stream: the window it advertised,
// what is buffered unread, and the consumption not yet granted back.
type recvWindow struct {
	size     int64
	buffered int64
	consumed int64
	// grant is the consumed threshold that earns a window update. It tracks
	// the smaller of the two windows: a sender capped below it could be
	// blocked on credit the update would have returned, so a threshold sized
	// by this end's window alone would never fire.
	grant int64
}

func newRecvWindow(size int64) recvWindow {
	w := recvWindow{size: size}
	w.retune(size)
	return w
}

// retune sizes the grant threshold by the smaller of this window and the
// peer's announced one (the ceiling on what it can send in flight).
func (w *recvWindow) retune(peerWindow int64) {
	small := w.size
	if peerWindow < small {
		small = peerWindow
	}
	t := small / 4
	if t < 1 {
		t = 1
	}
	w.grant = t
}

// onData accounts an arriving data frame's payload; more than the advertised
// window is unread bytes is a protocol violation.
func (w *recvWindow) onData(n int64) error {
	if n < 0 {
		return errWindowInvalid
	}
	w.buffered += n
	if w.buffered > w.size {
		return errWindowExceeded
	}
	return nil
}

// consume accounts n bytes read by the application and reports the grant to
// send now: at least half a window of consumption earns a WindowUpdate, and
// returning the whole accumulated amount keeps the peer's credit aligned with
// this window.
func (w *recvWindow) consume(n int64) (grant int64) {
	if n <= 0 || n > w.buffered {
		return 0
	}
	w.buffered -= n
	w.consumed += n
	// A quarter of the smaller window keeps credit ahead of a sender whose
	// chunks land near the boundary; half a window lets a 256 KiB sender
	// stall at exactly the last chunk it can afford.
	threshold := w.grant
	if threshold <= 0 {
		threshold = 1
	}
	if w.consumed >= threshold {
		grant = w.consumed
		w.consumed = 0
	}
	return grant
}

// bufferedBytes reports the unread bytes this window holds.
func (w *recvWindow) bufferedBytes() int64 { return w.buffered }
