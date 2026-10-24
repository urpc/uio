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

// Package ulayer stacks protocol layers over a UIO transport: TLS over TCP, a
// reliable stream over UDP datagrams, any transform over any transport.
//
// UIO's own interfaces are the seams, and a layer is a function between them.
// Downward, every layer hands the one above a [uio.Conn]: what the layer
// above reads from it is what this layer delivered, and what the layer above
// writes to it leaves through this layer. Upward, every layer receives the
// [uio.Handler] above it and returns the handler of the connection below:
// the lower connection's OnOpen, OnData and OnClose arrive, and the layer
// calls the handler above with the connection it wrapped. [Wrap] composes
// them, [Over] runs a composed stack over an [uio.Events]:
//
//	events := &uio.Events{Pollers: 4}
//	transport := layer.Over(events, appHandler, tlsLayer, kcpLayer)
//
// reads tlsLayer first, the layer next to the application, and kcpLayer
// last, the layer next to the wire; a byte written by the application leaves
// through every layer in that order and arrives back through them in
// reverse. The layers need not end there: a stack is a transport like the
// events are, so [Over] runs another stack over it as soon as one of its
// layers is written that way.
//
// # The layer contract
//
// A layer keeps the following rules; [Forward] carries the ones that are the
// same for every layer.
//
// Upward serialization. The callbacks a layer makes to the handler above
// never overlap, whatever goroutines its own protocol runs on. A layer
// driven only by the connection below inherits UIO's serialization; one with
// timers or workers of its own serializes what it delivers itself.
//
// Readiness. OnOpen reaches a layer when the connection below opens. A layer
// with a protocol of its own — a handshake, a negotiation — settles it
// before the layer above learns of the connection: OnOpen, then OnData of
// what arrives, then exactly one OnClose. A layer may also call OnOpen
// before it settles, to let the layer above send with its first flight, but
// then it holds what is sent, in order and bounded, until it does settle,
// and a settle that fails delivers OnClose instead. Which of the two a layer
// does is its own choice; the layer above cannot tell. OnClose is always
// delivered, to a handler above that never saw OnOpen included: the failure
// of a handshake is the close of the connection it never became.
//
// Backpressure. What a layer reads below and cannot deliver above it holds,
// bounded: once that bound is reached it stops consuming the connection
// below, whose unread bytes then reach the limit the transport configured
// and stop the socket reads, so a pause at the top stills the wire.
// InboundBuffered reports what the layer holds for the connection above
// together with what it has left unread below, and OutboundBuffered what is
// undelivered of what was written to it together with what the connection
// below still carries; both add up along a stack. A write the layer cannot
// take reports [uio.ErrBackpressure] or [uio.ErrOutboundOverflow] as a
// connection below would.
//
// Closing. Close and CloseWith end the layer's own protocol — its goodbye
// message, when it has one — then the connection below, once what was
// accepted has left, bounded by a timeout of the layer's own; CloseWrite
// says goodbye one way. A close from below delivers what the layer already
// holds whole to the layer above and then OnClose, with the cause kept
// unless the layer's protocol adds to it.
//
// Buffers. A layer reads below through Peek, PeekChunk and Discard and hands
// what it delivers as a view the layer above may keep past the callback only
// if it copied it, the connection below's own rule; a layer that can deliver
// what it read in place, as a cipher opened there, holds no copy of it and
// delays its Discard below until the layer above discards its view.
//
// Kinds. A connection is a stream of ordered reliable bytes or a run of
// datagrams; a layer says which it serves above and which it needs below,
// [StreamKind] when it says nothing. [Validate] checks a stack's layers
// against each other, and [Over] the stack's layers against the addresses it
// listens on and dials; anything composes, but a layer that got the wrong
// kind below finds out when the connection opens.
//
// The connection below. The fd-shaped methods — Control, the socket options,
// the deadlines — reach through Forward to the connection below; a layer
// whose protocol has no such thing, or whose goodbye they would break,
// overrides them. Wake reaches the connection below, so a pause anywhere in
// a stack still comes back.
package ulayer

import "github.com/urpc/uio"

// Layer turns the connections of the transport below it into the connections
// the layer above sees.
type Layer interface {
	// Handler returns the handler of one connection: its OnOpen receives
	// the connection below, and it calls next's callbacks with the
	// connection it hands upward, by the contract in the package
	// documentation. The returned handler serves one connection below; a
	// layer may hand every connection the same handler.
	Handler(next uio.Handler) uio.Handler
}

// Kind is what a connection carries: ordered reliable bytes, or datagrams.
type Kind uint8

const (
	// StreamKind is an ordered, reliable run of bytes: TCP and unix streams,
	// and every layer that turns what it reads below into one.
	StreamKind Kind = iota + 1
	// DatagramKind is unordered datagrams of bounded size: UDP, and the
	// layers that tunnel them.
	DatagramKind
)

func (k Kind) String() string {
	switch k {
	case StreamKind:
		return "stream"
	case DatagramKind:
		return "datagram"
	}
	return "unknown"
}

// A layer declares what it needs below with Requires when it is not a
// stream, and what it serves above with Provides when it is not a stream.
type (
	Requires interface{ Requires() Kind }
	Provides interface{ Provides() Kind }
)

func requires(layer Layer) Kind {
	if r, ok := layer.(Requires); ok {
		return r.Requires()
	}
	return StreamKind
}

func provides(layer Layer) Kind {
	if p, ok := layer.(Provides); ok {
		return p.Provides()
	}
	return StreamKind
}

// Validate reports whether the layers stack: each layer needs below what the
// layer under it serves above. It cannot see the transport at the bottom;
// Over checks that side.
func Validate(layers ...Layer) error {
	for i := 0; i+1 < len(layers); i++ {
		if want, got := requires(layers[i]), provides(layers[i+1]); want != got {
			return &KindError{Layer: i, Needs: want, Serves: got}
		}
	}
	return nil
}

// KindError is a layer stacked over what it cannot use.
type KindError struct {
	Layer  int  // the layer's place in the stack, 0 next to the application
	Needs  Kind // what the layer needs below
	Serves Kind // what the layer under it serves above
}

func (e *KindError) Error() string {
	return "layer " + itoa(e.Layer) + " needs a " + e.Needs.String() +
		" below, the layer under it serves a " + e.Serves.String()
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	var digits [20]byte
	i := len(digits)
	for v > 0 {
		i--
		digits[i] = byte('0' + v%10)
		v /= 10
	}
	return string(digits[i:])
}

// Wrap composes layers around top, bottom layer outermost: layers[0] is the
// layer next to the application, the last the layer next to the wire, and
// the handler it returns serves the wire-most layer's connections. It panics
// on layers Validate refuses; Over runs the same check and reports it.
func Wrap(top uio.Handler, layers ...Layer) uio.Handler {
	if err := Validate(layers...); err != nil {
		panic(err)
	}
	handler := top
	for i := len(layers) - 1; i >= 0; i-- {
		handler = layers[i].Handler(handler)
	}
	return handler
}

// kindOfAddr reports what an address in UIO's syntax carries.
func kindOfAddr(addr string) Kind {
	scheme := addr
	for i := 0; i+2 < len(addr); i++ {
		if addr[i] == ':' && addr[i+1] == '/' && addr[i+2] == '/' {
			scheme = addr[:i]
			break
		}
	}
	switch scheme {
	case "udp", "udp4", "udp6", "unixgram":
		return DatagramKind
	}
	return StreamKind
}
