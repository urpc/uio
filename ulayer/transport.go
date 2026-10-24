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

package ulayer

import (
	"context"
	"sync"

	"github.com/urpc/uio"
)

// Transport produces connections a stack has settled: UIO's Events is the
// bottom one — its Dial and Listen deliver whatever connections the address
// carries — and Over runs a stack over it whose Dial returns a connection
// only once every layer settled. A stack is a transport itself, so one
// stack can run over another wherever a layer is written that produces one.
type Transport interface {
	// DialContext connects to addr and returns the connection the stack
	// hands upward, once its layers settled: a layer's handshake that fails
	// or is cut short by ctx reports the error, and no callback of the
	// stack's handler runs for that connection. userdata is the connection's
	// Userdata, as Events.DialContext's is.
	DialContext(ctx context.Context, addr string, userdata any, opts ...uio.DialOption) (uio.Conn, error)

	// Listen accepts on addr through the stack: every connection the stack's
	// handler receives arrives settled by its layers. It is Events.Listen
	// over the events the stack runs on.
	Listen(addr string, userdata any, opts ...uio.ListenOption) (uio.Listener, error)

	// Dial connects without a deadline; see DialContext.
	Dial(addr string, userdata any, opts ...uio.DialOption) (uio.Conn, error)
}

// stack is a transport whose connections run through layers.
type stack struct {
	events *uio.Events
	layers []Layer
	top    uio.Handler

	// dials are the connections of DialContexts in flight, waiting for their
	// stack to settle, until they open or close.
	mu    sync.Mutex
	dials map[uio.Conn]*dialWait
}

// Over runs layers over events and returns their transport. It sets
// events.Handler, so call it before Serve, once; the events' Hooks and
// Observer are not its to touch and go on running. The connections events
// accepts and dials then run through the layers, and top receives the ones
// that settled: OnOpen with the connection the stack hands upward, OnData of
// what its layers deliver, and OnClose at the end — a dial's connection
// included, once DialContext has returned it. layers[0] is the layer next to
// top, the last the layer next to the wire, as in Wrap.
func Over(events *uio.Events, top uio.Handler, layers ...Layer) (Transport, error) {
	if err := Validate(layers...); err != nil {
		return nil, err
	}
	s := &stack{events: events, layers: layers, top: top, dials: make(map[uio.Conn]*dialWait)}
	router := uio.FuncHandler{
		Open:  routerOpen(s),
		Data:  routerData(s),
		Close: routerClose(s),
	}
	events.Handler = Wrap(router, layers...)
	return s, nil
}

// Dial connects without a deadline; see DialContext.
func (s *stack) Dial(addr string, userdata any, opts ...uio.DialOption) (uio.Conn, error) {
	return s.DialContext(context.Background(), addr, userdata, opts...)
}

func (s *stack) DialContext(ctx context.Context, addr string, userdata any, opts ...uio.DialOption) (uio.Conn, error) {
	if len(s.layers) > 0 {
		if want, got := requires(s.layers[len(s.layers)-1]), kindOfAddr(addr); want != got {
			return nil, &KindError{Layer: len(s.layers), Needs: want, Serves: got}
		}
	}
	wait := &dialWait{
		userdata: userdata,
		ready:    make(chan uio.Conn, 1),
		failed:   make(chan error, 1),
	}
	raw, err := s.events.DialContext(ctx, addr, wait, opts...)
	if err != nil {
		return nil, err // no callback follows for a dial that never opened
	}
	select {
	case conn := <-wait.ready:
		return conn, nil
	case err := <-wait.failed:
		return nil, err
	case <-ctx.Done():
		select {
		case conn := <-wait.ready: // settled before the deadline hit
			return conn, nil
		default:
		}
		// The close settles the stack; its report goes to the waiting
		// channels, which no one reads any more.
		_ = raw.CloseWith(ctx.Err())
		return nil, ctx.Err()
	}
}

func (s *stack) Listen(addr string, userdata any, opts ...uio.ListenOption) (uio.Listener, error) {
	if len(s.layers) > 0 {
		if want, got := requires(s.layers[len(s.layers)-1]), kindOfAddr(addr); want != got {
			return nil, &KindError{Layer: len(s.layers), Needs: want, Serves: got}
		}
	}
	return s.events.Listen(addr, userdata, opts...)
}

// dialWait marks the connection of a dial in flight: its userdata on the
// wire until the stack settles, and the dial's outcome after.
type dialWait struct {
	userdata any
	ready    chan uio.Conn
	failed   chan error
	opened   bool
}

func (w *dialWait) settle(conn uio.Conn) {
	w.opened = true
	select {
	case w.ready <- conn:
	default:
	}
}

func (w *dialWait) fail(err error) {
	select {
	case w.failed <- err:
	default:
	}
}

// The router is the top of a stack's handler chain: every layer settles
// beneath it and hands its connection here. A dial in flight is routed to
// its DialContext; a connection that closed before its stack settled is
// reported to that dial alone, so a failed dial runs no callback of top.
func routerOpen(s *stack) func(uio.Conn) {
	return func(c uio.Conn) {
		if wait, ok := c.Userdata().(*dialWait); ok {
			s.mu.Lock()
			s.dials[c] = wait
			s.mu.Unlock()
			c.SetUserdata(wait.userdata)
			wait.settle(c)
		}
		s.top.OnOpen(c)
	}
}

func routerData(s *stack) func(uio.Conn) error {
	return func(c uio.Conn) error { return s.top.OnData(c) }
}

func routerClose(s *stack) func(uio.Conn, error) {
	return func(c uio.Conn, err error) {
		s.mu.Lock()
		wait := s.dials[c]
		delete(s.dials, c)
		s.mu.Unlock()
		// A dial whose stack closed before it settled has not been in the
		// map: its marker is still the connection's userdata.
		if wait == nil {
			wait, _ = c.Userdata().(*dialWait)
		}
		if wait != nil && !wait.opened {
			wait.fail(err)
			return
		}
		s.top.OnClose(c, err)
	}
}
