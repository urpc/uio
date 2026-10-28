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

// Handler receives the callbacks of every connection an Events serves or
// dials, set as Events.Handler. It is LayerHandler without OnWrite — the
// top of a stack, whose outbound is the Write method it calls. Lifecycle
// and data callbacks are serialized per connection, while callbacks for
// different connections may run concurrently. FuncHandler adapts plain
// functions to it.
type Handler interface {
	// OnOpen fires first for every connection, in the connection's first
	// turn, also for one closed before that turn ran: OnClose follows, and
	// writes from OnOpen return net.ErrClosed. On Unix a stream connection's
	// first turn reads whatever its peer already sent right after OnOpen
	// returns, in the same turn.
	OnOpen(c Conn)

	// OnRead fires on a turn once the connection opened, with inbound data
	// available or none: a Wake runs one too, so it may arrive with nothing
	// to read. Inbound access methods are valid only for this callback
	// invocation. An error closes the connection. A layer hears the same
	// moment as its OnRead; here, at the top of the stack, outbound is not a
	// callback but Write, a method the Handler calls — which is why there is
	// no OnWrite next to it.
	OnRead(c Conn) error

	// OnTimer fires on the turn the connection's own timer came due — the
	// one SetWakeDeadline and SetWakeInterval arm — before OnRead in the same
	// turn. An error closes the connection.
	OnTimer(c Conn) error

	// OnClose is the final callback for a connection and never overlaps its
	// OnOpen or OnRead callback; only a connection that got OnOpen gets it.
	// Close only requests the close: the connection is released, and OnClose
	// runs, once the callback that closed it has returned.
	OnClose(c Conn, err error)
}

// TrafficObserver, set as Events.Observer, is told the bytes each connection
// reads and writes. Both callbacks are observation only: they report I/O that
// just happened. They are named for the bytes they count: OnRead and
// OnWrite name the data path's callbacks — LayerHandler.OnRead and
// OnWrite, and the Handler's OnRead — not its byte totals, which is why
// the counts here carry Bytes in their names.
type TrafficObserver interface {
	// OnBytesRead reports bytes read from the socket before OnRead and
	// shares its inbound-access scope.
	OnBytesRead(c Conn, n int)

	// OnBytesWritten reports bytes successfully written to the socket. It may
	// run on a backend writer goroutine or a native write turn, concurrently
	// with the connection's other callbacks, and does not grant inbound-buffer
	// access. The final flush of a closing native connection reports from the
	// goroutine that releases it, which during shutdown is the one returning
	// from Serve. Calls for one connection never overlap each other: on the
	// native backend a Write made inside OnBytesWritten is queued and sent
	// after it returns. A UDP connection's datagrams leave on the goroutines
	// that write them, and OnBytesWritten may report several of them, on any
	// one of those goroutines, after its send. With layers installed it may run
	// inside a layer's write below, so it must not write to or close the
	// connection.
	OnBytesWritten(c Conn, n int)
}

// ServeHooks, set as Events.Hooks, runs code around Serve.
type ServeHooks interface {
	// OnStart runs synchronously after initialization and before listeners
	// begin accepting connections.
	OnStart(ev *Events)

	// OnStop runs once after Serve has stopped its loops and connection tasks.
	OnStop(ev *Events)
}

// FuncHandler adapts plain functions to Handler, TrafficObserver and
// ServeHooks; set the value as whichever of Events.Handler, Observer and Hooks
// its fields fill. A nil field is a callback not set: a nil Read discards
// what each connection reads, a nil Timer never fires, and a nil BytesRead or
// BytesWritten reports nothing.
type FuncHandler struct {
	Open         func(c Conn)
	Read         func(c Conn) error
	Timer        func(c Conn) error
	Close        func(c Conn, err error)
	BytesRead    func(c Conn, n int)
	BytesWritten func(c Conn, n int)
	// Sent hears the receipts of the Handler's own tracked writes over a
	// layer stack; see SentObserver.
	Sent  func(c Conn, r SendReceipt)
	Start func(ev *Events)
	Stop  func(ev *Events)
}

func (h FuncHandler) OnOpen(c Conn) {
	if h.Open != nil {
		h.Open(c)
	}
}

func (h FuncHandler) OnRead(c Conn) error {
	if h.Read != nil {
		return h.Read(c)
	}
	_, _ = c.Discard(-1)
	return nil
}

func (h FuncHandler) OnTimer(c Conn) error {
	if h.Timer != nil {
		return h.Timer(c)
	}
	return nil
}

func (h FuncHandler) OnClose(c Conn, err error) {
	if h.Close != nil {
		h.Close(c, err)
	}
}

func (h FuncHandler) OnBytesRead(c Conn, n int) {
	if h.BytesRead != nil {
		h.BytesRead(c, n)
	}
}

// OnSent implements SentObserver.
func (h FuncHandler) OnSent(c Conn, r SendReceipt) {
	if h.Sent != nil {
		h.Sent(c, r)
	}
}

func (h FuncHandler) OnBytesWritten(c Conn, n int) {
	if h.BytesWritten != nil {
		h.BytesWritten(c, n)
	}
}

func (h FuncHandler) OnStart(ev *Events) {
	if h.Start != nil {
		h.Start(ev)
	}
}

func (h FuncHandler) OnStop(ev *Events) {
	if h.Stop != nil {
		h.Stop(ev)
	}
}

// asFuncs reports the FuncHandler behind v, a value or a pointer to one.
func asFuncs(v any) (*FuncHandler, bool) {
	switch fh := v.(type) {
	case FuncHandler:
		return &fh, true
	case *FuncHandler:
		return fh, true
	}
	return nil, false
}

// callbacks are Events' Handler, Observer and Hooks resolved once, when Serve
// starts: the hot paths test a field and call it. A nil field is a callback
// not configured, so traffic reports cost nothing without an Observer. A
// FuncHandler contributes its fields themselves, so an unset one stays nil
// instead of becoming a call that does nothing.
type callbacks struct {
	onOpen         func(c Conn)
	onRead         func(c Conn) error
	onTimer        func(c Conn) error
	onClose        func(c Conn, err error)
	onBytesRead    func(c Conn, n int)
	onBytesWritten func(c Conn, n int)
	onStart        func(ev *Events)
	onStop         func(ev *Events)
}

func resolveCallbacks(handler Handler, observer TrafficObserver, hooks ServeHooks) callbacks {
	var cb callbacks
	if fh, ok := asFuncs(handler); ok {
		if fh != nil {
			cb.onOpen, cb.onRead, cb.onTimer, cb.onClose = fh.Open, fh.Read, fh.Timer, fh.Close
		}
	} else if handler != nil {
		cb.onOpen, cb.onRead, cb.onTimer, cb.onClose = handler.OnOpen, handler.OnRead, handler.OnTimer, handler.OnClose
	}
	if fh, ok := asFuncs(observer); ok {
		if fh != nil {
			cb.onBytesRead, cb.onBytesWritten = fh.BytesRead, fh.BytesWritten
		}
	} else if observer != nil {
		cb.onBytesRead, cb.onBytesWritten = observer.OnBytesRead, observer.OnBytesWritten
	}
	if fh, ok := asFuncs(hooks); ok {
		if fh != nil {
			cb.onStart, cb.onStop = fh.Start, fh.Stop
		}
	} else if hooks != nil {
		cb.onStart, cb.onStop = hooks.OnStart, hooks.OnStop
	}
	return cb
}
