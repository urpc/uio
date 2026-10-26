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

import (
	"context"
	"net"
	"sync"
	"time"
)

// Middleware wraps the handler above it and returns the handler of the
// connection below: what the connection below delivered reaches the handler
// above through it, and what the handler above writes leaves through it. A
// middleware hands the handler above a [Conn] of its own — what that
// connection reads is what the middleware delivered, and what is written to it
// leaves through the middleware — and opens it to the handler above once
// whatever it runs settled: a handshake, a negotiation. [Events.Use] installs
// middleware under the [Events.Handler].
//
// It runs once per connection, not per message: it wraps a connection's whole
// life, so per-message work belongs in OnData.
//
// Three rules carry UIO's own machinery; everything else a middleware owes the
// handlers above it is between them.
//
//   - The callbacks it makes to the handler above never overlap. A middleware
//     driven only by the connection below inherits UIO's serialization; one
//     with goroutines of its own serializes what it delivers itself.
//   - OnClose always reaches the handler above, to one that never saw OnOpen
//     included: the failure of a handshake is the close of the connection it
//     never became, and a dial's report rides on it.
//   - The connection it hands above forwards Userdata and SetUserdata to the
//     connection it wraps: a dial recognizes itself by a marker there.
type Middleware = func(next Handler) Handler

// Use collects middleware for [Options.Middleware] and [Events.Use]:
//
//	uws.Options{Middleware: uio.Use(utls.Server(cfg), trace)}
//
// It returns a new slice of its arguments, so a middleware list can be built
// in a struct literal without spelling out the type.
func Use(middleware ...Middleware) []Middleware {
	return middleware
}

// Use installs middleware under the Handler, the first installed closest to
// the wire, so what arrives passes them in install order on its way up and
// what the Handler writes passes them in reverse on its way down:
//
//	events.Use(kcp, tls).Use(trace)
//
// reads bottom up — kcp next to the wire, tls over it, trace just under the
// Handler. Use before Serve: Serve reads the middleware once, with the
// Handler, and a Use after it does nothing. It appends, and returns the
// Events, so installs chain. An empty Use does nothing.
//
// Every connection the Events accepts, adopts and dials runs the middleware.
// DialContext returns only once the whole chain settled, with the topmost
// middleware's connection; one that fails before that reports its error from
// DialContext, and the Handler hears nothing of the connection.
func (ev *Events) Use(middleware ...Middleware) *Events {
	ev.mux.Lock()
	defer ev.mux.Unlock()
	if ev.ready.Load() {
		return ev // Serve read the middleware already
	}
	ev.use = append(ev.use, middleware...)
	return ev
}

// wrapHandler composes the installed middleware around handler, the
// innermost dial probe included when any are installed. It runs under
// ev.mux, in Serve's initialization.
func (ev *Events) wrapHandler(handler Handler) Handler {
	if len(ev.use) == 0 {
		return handler
	}
	handler = dialProbing{next: handler}
	for i := len(ev.use) - 1; i >= 0; i-- {
		handler = ev.use[i](handler)
	}
	return handler
}

// PerConn adapts middleware that has state per connection — a handshake, a
// reassembly buffer — to the Middleware shape. Callbacks arrive carrying
// the connection below, and what f builds serves the connection above it:
//
//	events.Use(uio.PerConn(func(lower uio.Conn, next uio.Handler) uio.Handler {
//		conn := &tlsConn{InnerConn: uio.InnerConn{Conn: lower}}
//		return uio.FuncHandler{
//			Open:  func(uio.Conn) { next.OnOpen(conn) },
//			Data:  func(uio.Conn) error { conn.pump(); return next.OnData(conn) },
//			Close: func(_ uio.Conn, err error) { next.OnClose(conn, err) },
//		}
//	}))
//
// Middleware without per-connection state needs none of this: it is a
// function from the handler above to the handler of the connection below.
func PerConn(f func(lower Conn, next Handler) Handler) Middleware {
	return func(next Handler) Handler {
		return &perConn{build: f, next: next, serving: make(map[Conn]Handler)}
	}
}

type perConn struct {
	build   func(Conn, Handler) Handler
	next    Handler
	mu      sync.Mutex
	serving map[Conn]Handler
}

func (l *perConn) handler(lower Conn) Handler {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.serving[lower]
}

func (l *perConn) OnOpen(lower Conn) {
	handler := l.build(lower, l.next)
	l.mu.Lock()
	l.serving[lower] = handler
	l.mu.Unlock()
	handler.OnOpen(lower)
}

func (l *perConn) OnData(lower Conn) error {
	if handler := l.handler(lower); handler != nil {
		return handler.OnData(lower)
	}
	return nil
}

func (l *perConn) OnClose(lower Conn, err error) {
	l.mu.Lock()
	handler := l.serving[lower]
	delete(l.serving, lower)
	l.mu.Unlock()
	if handler != nil {
		handler.OnClose(lower, err)
	}
}

// dialMarker replaces the dial's userdata with a probe when middleware is
// installed, so the chain's top can report the dial. The second return is
// the userdata the connection carries.
func (ev *Events) dialMarker(userdata any) (*dialProbe, any) {
	if len(ev.use) == 0 {
		return nil, userdata
	}
	probe := &dialProbe{user: userdata, done: make(chan struct{})}
	return probe, probe
}

// dialProbe is the marker a wrapped dial carries until it settles: the probe
// at the top of the chain reports into it, and DialContext waits on it.
type dialProbe struct {
	user any
	done chan struct{} // closed once, with conn or err in place
	conn Conn
	err  error
}

func (p *dialProbe) settle(conn Conn) {
	p.conn = conn
	close(p.done)
}

func (p *dialProbe) fail(err error) {
	p.err = err
	close(p.done)
}

// dialProbing sits at the top of the middleware chain, just under the Handler:
// a dialed connection announces itself by the marker it carries and hands
// over its topmost connection; everything else passes through untouched.
type dialProbing struct{ next Handler }

func (h dialProbing) OnOpen(c Conn) {
	if probe, ok := c.Userdata().(*dialProbe); ok {
		c.SetUserdata(probe.user)
		probe.settle(c)
	}
	h.next.OnOpen(c)
}

func (h dialProbing) OnData(c Conn) error { return h.next.OnData(c) }

func (h dialProbing) OnClose(c Conn, err error) {
	if probe, ok := c.Userdata().(*dialProbe); ok {
		// The dial never settled: the Handler never saw the connection, and
		// its close belongs to the dial that asked for it.
		probe.fail(err)
		return
	}
	h.next.OnClose(c, err)
}

// awaitDial waits for a wrapped dial's chain to settle and reports what it
// became. raw is the wire connection, for a cancellation to close.
func (ev *Events) awaitDial(ctx context.Context, raw Conn, probe *dialProbe) (Conn, error) {
	select {
	case <-probe.done:
		if probe.err != nil {
			return nil, probe.err
		}
		return probe.conn, nil
	case <-ctx.Done():
		select {
		case <-probe.done: // settled as the deadline arrived
			if probe.err != nil {
				return nil, probe.err
			}
			return probe.conn, nil
		default:
		}
		cause := context.Cause(ctx)
		if cause == nil {
			cause = ctx.Err()
		}
		_ = raw.CloseWith(cause)
		return nil, cause
	}
}

// InnerConn carries the Conn methods middleware keeps as they are: the
// addressing, the userdata, the deadlines and the fd-shaped options of the
// connection below, handed on unchanged. Embed it in the conn a middleware
// hands upward and define what it transforms — the reads, the writes, the
// buffered counts it holds bytes of its own for, and the closes, which are
// never InnerConn's to pass on: a middleware's goodbye is its own.
type InnerConn struct {
	// Conn is the connection the middleware reads and writes through.
	Conn Conn
}

func (f InnerConn) LocalAddr() net.Addr           { return f.Conn.LocalAddr() }
func (f InnerConn) RemoteAddr() net.Addr          { return f.Conn.RemoteAddr() }
func (f InnerConn) Userdata() any                 { return f.Conn.Userdata() }
func (f InnerConn) SetUserdata(value any)         { f.Conn.SetUserdata(value) }
func (f InnerConn) SetLinger(seconds int) error   { return f.Conn.SetLinger(seconds) }
func (f InnerConn) SetNoDelay(noDelay bool) error { return f.Conn.SetNoDelay(noDelay) }
func (f InnerConn) SetKeepAlive(keepAlive bool) error {
	return f.Conn.SetKeepAlive(keepAlive)
}
func (f InnerConn) SetKeepAlivePeriod(seconds int) error {
	return f.Conn.SetKeepAlivePeriod(seconds)
}
func (f InnerConn) SetReadBuffer(size int) error  { return f.Conn.SetReadBuffer(size) }
func (f InnerConn) SetWriteBuffer(size int) error { return f.Conn.SetWriteBuffer(size) }
func (f InnerConn) SetDeadline(deadline time.Time) error {
	return f.Conn.SetDeadline(deadline)
}
func (f InnerConn) SetReadDeadline(deadline time.Time) error {
	return f.Conn.SetReadDeadline(deadline)
}
func (f InnerConn) SetWriteDeadline(deadline time.Time) error {
	return f.Conn.SetWriteDeadline(deadline)
}
func (f InnerConn) Control(fn func(fd uintptr)) error { return f.Conn.Control(fn) }

// Wake reaches the connection below; a middleware that must first pump its own
// protocol overrides it.
func (f InnerConn) Wake() error { return f.Conn.Wake() }

// SetWakeDeadline and SetWakeInterval reach the connection below: a
// middleware's timing is the connection it wraps.
func (f InnerConn) SetWakeDeadline(when time.Time) error {
	return f.Conn.SetWakeDeadline(when)
}

func (f InnerConn) SetWakeInterval(interval time.Duration) error {
	return f.Conn.SetWakeInterval(interval)
}
