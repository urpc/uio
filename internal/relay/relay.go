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

// Package relay lets a relay, ubridge's, move a connection's input straight
// to another connection's socket with splice(2) from the source's turns,
// without methods on the transport's exported connection types.
package relay

// Splicer is a connection whose turns can relay its socket's input with
// splice(2).
type Splicer interface {
	// EnableSplice switches the relay on or off and reports whether the
	// connection relays. While it is on, each read round first runs the
	// handler with nothing read, so the handler can call TrySplice before
	// the round reads the socket itself.
	EnableSplice(enabled bool) bool
	// TrySpliceTo moves up to max bytes of the socket's input into dst's
	// socket, or into dst's queue behind what it holds. It runs on the
	// source's turn, with nothing read into the connection's own buffer.
	// handled reports that the input was taken care of: bytes moved, or the
	// socket had none to give; the round then ends unless the stream did,
	// whose end the round goes on to read. false leaves the input to the
	// round's ordinary read.
	TrySpliceTo(dst any, max int) (n int, handled bool, err error)
}

// Getter resolves a connection's Splicer, nil when it has none. Implement it
// on a private zero-sized type in the connection's package.
type Getter interface {
	GetSplicer(source any) Splicer
}

// Access embeds the capability in a connection: a zero-sized value whose one
// method only this package can call.
type Access[T Getter] struct{}

func (Access[T]) relaySplicer(source any) Splicer {
	var getter T
	return getter.GetSplicer(source)
}

func splicerOf(source any) Splicer {
	if access, ok := source.(interface{ relaySplicer(any) Splicer }); ok {
		return access.relaySplicer(source)
	}
	return nil
}

// EnableSplicePair switches the relay on for a and b, or for neither when
// either cannot relay.
func EnableSplicePair(a, b any) {
	sa, sb := splicerOf(a), splicerOf(b)
	if sa == nil || sb == nil {
		return
	}
	if sa.EnableSplice(true) && !sb.EnableSplice(true) {
		sa.EnableSplice(false)
	}
}

// TrySplice relays up to max bytes from src's socket to dst; see
// Splicer.TrySpliceTo. A src that cannot relay reports false.
func TrySplice(dst, src any, max int) (int64, bool, error) {
	if s := splicerOf(src); s != nil {
		n, handled, err := s.TrySpliceTo(dst, max)
		return int64(n), handled, err
	}
	return 0, false, nil
}
