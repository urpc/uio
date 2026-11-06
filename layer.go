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
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// Layer is a protocol layer between the socket and the Handler: TLS,
// compression, an obfuscation, a handshake of its own. It transforms bytes —
// what arrives on its way up, what the Handler writes on its way down — and
// UIO carries everything else: the connection's turns, the Handler's
// callbacks, its addressing and deadlines, its closes. An endpoint stacks
// layers under the [Events.Handler]: [WithLayers] in a [Events.Listen] or in
// a dial names the chain its connections run.
//
// A layer carries one connection: one connection above for each one below.
// A protocol that opens several streams over one connection — HTTP/2, QUIC —
// runs as the Handler, over a connection the layer below it made.
//
// Attach runs once per connection and returns that connection's [LayerHandler];
// the [LayerContext] it receives is the connection's place in the stack: the
// layer reads what arrived with Lower(), hands the layer above what that
// became with Upper().Write, and writes down with Lower().Write. The layer
// above — the next layer or the Handler — gets a [Conn] UIO builds over this
// one: its reads are what this layer delivered, its writes reach this layer's
// OnWrite, and everything else is the connection's own, passed through.
//
// What UIO guarantees every layer, so that none has to remember it:
//
//   - Every turn reaches the layers above, whether or not anything was
//     delivered, so their timers and wakes work through any stack.
//   - Each layer has a timer of its own ([LayerContext.SetTimer] and
//     [LayerContext.SetTimerInterval]), and the Handler's
//     [Conn.SetWakeDeadline] is a slot of its own too: no layer replaces
//     another's.
//   - OnOpen, OnRead, OnTimer, OnClosing and OnClose run on the
//     connection's turns, which never overlap one another or the callbacks
//     of the layers above — the close walk runs on a turn whatever asked for
//     it. The rest run on the goroutine that called for them, and may overlap
//     the turns and one another: OnWrite on the writer's, CloseWrite on the
//     one closing the connection, and OutboundHeld on a writer's. State they
//     share with the turns needs a lock.
//   - UIO calls a layer only on a turn or from above it — the Handler, or
//     the layer above through its LayerContext — never from below or from
//     inside another layer's callback: a layer may hold its lock while it
//     writes below, and nothing calls it back meanwhile.
//     LayerContext.Open only records the request — the layers above open after
//     the calling callback returned, on the same turn. A close a layer asks
//     for (LayerContext.Close or Abort) likewise runs its OnClosing and the
//     walk below on the turn, never inside the call; an end CloseWrite
//     leaves waiting is passed down on one.
//   - Once the connection is closing — any layer, or the Handler, closed
//     it — or ended, UIO starts no call into a layer, checking again after
//     every callback: a write or flush that comes after returns net.ErrClosed,
//     a repeated Close yields or reports net.ErrClosed, an Abort cuts any
//     graceful drain short at once, and only OnClose follows. The close itself
//     is the one caller left: the OnClosing hooks it walks below the layer
//     that began it run while every layer already reads closing. A call
//     already under way on another goroutine may still overlap.
//   - What the layers hold for the layers above counts against
//     MaxInboundBuffered with the socket's own buffer: a stack whose layers
//     hold the limit pulls nothing more from below until a read above makes
//     room. A packet connection keeps no buffer to hold and is never paced:
//     its PacketLayer carries whatever backpressure its protocol provides.
//   - Layers carry streams. A packet connection — UDP, IP, unixgram,
//     unixpacket — takes a [PacketLayer] at the bottom of the chain, which
//     reads its datagrams and hands the layers above the stream it makes of
//     them; a chain without one is refused with ErrLayersNeedStream.
//   - Every connection ends once, top down: the Handler's OnClose first, if
//     it opened, then every layer's. A connection whose stack never opened —
//     a failed handshake — ends without a Handler callback, and a dial
//     reports its cause instead.
//   - The end of a stream travels up: a layer whose peer ended its stream
//     calls Upper().End, and the layer above reads io.EOF once it has
//     read the rest.
type Layer interface {
	// Attach builds the layer's state for one connection. The LayerContext
	// stays valid until the connection's OnClose.
	Attach(ctx LayerContext) LayerHandler
}

// PacketLayer is a Layer that takes packets: installed at the bottom of a
// chain on a packet connection — UDP, IP, unixgram, unixpacket — it reads the
// connection's datagrams and hands the layers above the stream it makes of
// them, a reliable stream for a protocol like KCP that runs over one. A chain
// on a packet connection must start with a PacketLayer; without one the
// endpoint reports ErrLayersNeedStream. On a stream connection the marker
// changes nothing.
//
// The framework hands a PacketLayer one datagram per turn: its Lower()'s
// PeekChunk and Discard see that datagram alone, and what the layer leaves of
// it when OnRead returns is dropped — a packet connection keeps no unread
// bytes between turns, so the layer must consume what the turn brought.
//
// The framework never paces a PacketLayer, whatever the layer above has yet
// to read: a datagram it does not take is gone, so it is called for every one
// that arrives. The layer bounds what it delivers up itself, and its protocol
// carries the backpressure to the peer — a full window, for KCP.
type PacketLayer interface {
	Layer

	// CarriesPackets marks the layer as one that reads datagrams from below.
	CarriesPackets()
}

// isPacketLayer reports a Layer that declares it takes packets from below.
func isPacketLayer(layer Layer) bool {
	_, ok := layer.(PacketLayer)
	return ok
}

// LayerHandler is a layer's instance on one connection: the callbacks UIO runs
// as the connection's bytes and events pass through the layer.
type LayerHandler interface {
	// OnOpen runs once the layers below opened: the place to start a
	// handshake. The layers above open when ctx.Open is called, here
	// or on a later turn; a layer that negotiates nothing calls it here.
	OnOpen(ctx LayerContext)

	// OnRead runs on a turn once the layer opened: bytes from below may
	// have arrived, or none. It reads them from Lower(), consumes
	// them with Discard, and hands up what they became with
	// Upper().Write. It may take one chunk at a time: while it consumes
	// and bytes still wait below, the turn calls it again. The bottom layer is
	// not called at all while the stack holds MaxInboundBuffered unread, until
	// a read above makes room; the layers above it go on moving what the stack
	// already holds up. A layer whose own delivery to the layer above is full
	// waits the same way — its next call is the turn the layer above reads on.
	OnRead(ctx LayerContext) error

	// OnWrite runs for what the layer above writes, on the writer's
	// goroutine and possibly beside other writers: it transforms p, writes
	// the result with Lower().Write, and reports how much of p it took.
	// p is the writer's again once it returns; one call carries one write
	// whole, a Writev's segments joined. What it writes below is never
	// refused for the queue's size: the Handler's writes are checked against
	// MaxOutboundBuffered before they enter the stack. A layer that holds
	// output of its own — a send window — may refuse a write with
	// ErrOutboundOverflow, taking none of it. It runs only once the layer
	// opened the layers above.
	OnWrite(ctx LayerContext, p []byte) (int, error)

	// OnTimer runs on the turn the layer's timer came due, before OnRead.
	OnTimer(ctx LayerContext) error

	// OnClose runs once, as the connection ends, whichever end closed it.
	OnClose(ctx LayerContext, cause error)
}

// CloseWriter is implemented by a LayerHandler that can end one direction of
// its stream — a TLS close_notify sent before the socket's writing half
// shuts. It runs when the layer above calls CloseWrite. A layer without it
// does not support CloseWrite: a protocol that frames its bytes cannot leave
// its peer a stream it did not end itself. PassThroughLayer implements it by
// passing CloseWrite to the layer below (embedding PassThrough does not). UIO
// runs it once what was written before the call has gone below: writes still
// in flight into the layer finished, and what an OutboundHolder still holds
// sent — so the end never overtakes what it follows. Writes the call refused
// never reach the layer.
type CloseWriter interface {
	CloseWrite(ctx LayerContext) error
}

// LayerCloser is implemented by a LayerHandler with a protocol close frame to
// send — a TLS close_notify, a WebSocket Close — when the layer above or this
// layer closes the connection. OnClosing runs on a turn before the close
// passes to the layer below, with the close's cause: nil or io.EOF for a
// graceful close, or a non-nil error for an abort.
//
// A graceful close of a layered connection lets what was queued leave first,
// the layers' close frames included: the socket's queue drains, bounded by
// LingerTimeout without progress, before the connection releases — the
// socket's own Close carries it, and leftover output reaches OnClose as
// UnflushedError. Nothing reaches the layers or the Handler meanwhile, and
// what arrives is discarded. An abort closes at once after the walk. A layer
// that closes the connection itself, through LayerContext.Close or Abort, ends
// it for the layers above too: what they hold is dropped, and its own
// OnClosing runs on a turn, never inside the callback that asked.
type LayerCloser interface {
	OnClosing(ctx LayerContext, cause error)
}

// OutboundHolder is implemented by a LayerHandler that cannot always write
// below at once — a send window waiting for its peer, a compressor gathering
// a block, a gate holding bytes back — and answers for the bytes it took and
// still holds: OutboundHeld reports how many. They count in the
// OutboundBuffered of the connection above it, in the MaxOutboundBuffered
// check, and in what CloseWrite sends before the end: a byte counts until
// its write below has returned. OutboundHeld runs on writers' goroutines and
// beside the turns, so it must not wait on a lock the layer holds while it
// writes below. A holder that sends what it held outside the turns — a send
// window opening on a goroutine of its own — calls LayerContext.Wake once the
// write returns, so what waits for the bytes to leave sees them gone.
type OutboundHolder interface {
	OutboundHeld(ctx LayerContext) int
}

// DefaultDeliverBound bounds what one layer delivers to the layer above
// before that layer reads — each layer's delivery, whichever way the stack is
// stacked. Events.MaxInboundBuffered, when set, is the bound instead.
const DefaultDeliverBound = 64 << 10

// maxLayers is how many layers one stack takes: their timers, and the
// Handler's, share one bit mask.
const maxLayers = 63

// LayerContext is a layer's handle on its place in one connection's stack:
// Lower is the connection below (toward the wire), Upper is the layer above's
// inbound delivery buffer (toward the Handler), and Open/Close/Abort/SetTimer
// control this layer's lifecycle and scheduling.
type LayerContext interface {
	// Lower is the connection below: the socket for the first layer installed,
	// or the connection the layer below built for the others. Its inbound
	// methods — Peek, PeekChunk, Discard, Read — run in OnRead and OnTimer;
	// its writes, deadlines, and addressing may be called at any time. Use
	// [SocketOf] to access OS-level socket options on the underlying socket.
	Lower() Lower

	// Upper is the inbound delivery target above this layer: write decoded
	// bytes to it or mark its stream ended.
	Upper() Upper

	// Open opens the layers above once this layer's negotiation settled — or
	// at once from OnOpen when it has none. A dial returns once the top layer
	// opened. Call it on the connection's turn.
	Open()

	// Close ends the connection from this layer, gracefully: writes are
	// refused at once, and on the connection's next turn this layer's
	// OnClosing runs first, with nil cause, then the layers below run theirs,
	// and the socket drains what was accepted before it closes. Neither this
	// call nor the turn calls back into the callback that asked. Nothing above
	// this layer is called again. A Close while another close is under way
	// yields to it and returns nil; once the connection has closed it reports
	// net.ErrClosed.
	Close() error

	// Abort ends the connection at once from this layer: OnClosing hooks are
	// told the cause on the next turn, nothing is drained, what the layers
	// above hold is dropped, and the socket closes immediately (cutting any
	// graceful drain short at once).
	Abort(cause error) error

	// SetTimer arms this layer's own one-shot timer for when; a zero time
	// cancels it.
	SetTimer(when time.Time) error

	// SetTimerInterval arms this layer's own repeating timer every interval; a
	// zero or negative interval cancels it.
	SetTimerInterval(interval time.Duration) error

	// Wake schedules a turn of the connection.
	Wake() error
}

// Upper is the inbound delivery buffer feeding the layer (or Handler) above.
// Write always takes all of p and never fails. The framework paces the layer,
// not its writes: once a layer's delivery reaches DefaultDeliverBound —
// Events.MaxInboundBuffered, when set — the framework stops calling that
// layer's OnRead until the layer above reads. What one write adds is not
// capped: the write that crosses the bound, and any made from OnOpen, takes
// the delivery past it by that write's size. Its methods run on the
// connection's turn — in OnOpen, OnRead or OnTimer.
type Upper interface {
	io.Writer

	// End tells the layer above the stream ended: once it read what was
	// written, its Read reports io.EOF. A layer calls it when its peer ended
	// its stream — the socket's end, a TLS close_notify. At the top of the
	// stack, an Events without AllowHalfOpen closes the connection with
	// io.EOF once the Handler's OnRead has run; with it, the Handler may go on
	// writing and end its own half with CloseWrite. The stack ends without it
	// too, once the socket ended and the layers handed up everything they
	// could — an input no layer could complete ends the stream as a
	// truncation (io.ErrUnexpectedEOF) instead. Call it in OnRead or
	// OnTimer.
	End()

	// Reserve returns at least n bytes of room past what is written, for the
	// layer to fill in place — decrypting straight into it — and Commit
	// delivers the first n of them. Between the two, write nothing else to
	// Upper: Write appends where Reserve's room begins. Call both in OnRead or
	// OnTimer.
	Reserve(n int) []byte
	Commit(n int)

	// Buffered reports how many delivered bytes the layer above has not read.
	Buffered() int

	// Yield tells UIO the layer stopped short of what it could deliver — its
	// bound was reached, with bytes waiting below or in its own buffers — so
	// the next read above schedules a turn to take the rest.
	Yield()
}

// LayerFunc is a layer written as the function its Attach runs.
type LayerFunc func(ctx LayerContext) LayerHandler

// Attach calls f.
func (f LayerFunc) Attach(ctx LayerContext) LayerHandler { return f(ctx) }

// PassThroughLayer is the layer that changes nothing: a WithLayers override
// that names no layers runs it, so a stack always has a layer. Its handler
// passes CloseWrite down as well, so a stack of it is transparent throughout
// — which embedding PassThrough does not grant (see PassThrough).
var PassThroughLayer Layer = LayerFunc(func(LayerContext) LayerHandler { return passThroughHandler{} })

// passThroughHandler is PassThrough with its half-close: the layer that
// changes nothing ends one direction by passing the end down.
type passThroughHandler struct{ PassThrough }

func (passThroughHandler) CloseWrite(ctx LayerContext) error { return ctx.Lower().CloseWrite() }

// PassThrough is a LayerHandler that changes nothing: it opens at once,
// delivers what arrives, passes the end of the stream up and writes what is
// written. Embed it in a layer's LayerHandler to write only what that layer
// transforms — and mind what it lends: OnOpen opens the layers above at once
// (override it to negotiate first), and CloseWrite is deliberately not
// among its methods, so a layer that frames its bytes does not inherit a
// pass-through half-close it would break; a layer that does pass writes
// through whole implements CloseWriter itself.
type PassThrough struct{}

// OnOpen opens the layers above at once.
func (PassThrough) OnOpen(ctx LayerContext) { ctx.Open() }

// OnRead delivers what arrived as it is, and the end of the stream after it.
func (PassThrough) OnRead(ctx LayerContext) error {
	lower := ctx.Lower()
	if chunk := lower.PeekChunk(); len(chunk) > 0 {
		_, _ = ctx.Upper().Write(chunk)
		if _, err := lower.Discard(len(chunk)); err != nil {
			return err
		}
	}
	if lowerEnded(lower) {
		ctx.Upper().End()
	}
	return nil
}

// lowerEnded reports a stream below that ended and was read to its end, as
// lower.Read(nil) reporting io.EOF does.
func lowerEnded(lower Lower) bool {
	return lower.State()&StateReadEnded != 0
}

// OnWrite writes what is written as it is.
func (PassThrough) OnWrite(ctx LayerContext, p []byte) (int, error) { return ctx.Lower().Write(p) }

// OnTimer does nothing.
func (PassThrough) OnTimer(LayerContext) error { return nil }

// OnClose does nothing.
func (PassThrough) OnClose(LayerContext, error) {}

// stackHandler puts the installed layers between the socket's callbacks and
// handler. It runs under ev.mux, in Serve's initialization.
// newEntry builds what one endpoint's connections run: the callbacks
// resolved for its chain, with the stack driver over the Events' Handler. A
// chain of no layers resolves the plain callbacks — the same the Events use.
func (ev *Events) newEntry(layers []Layer) (*entry, error) {
	if len(layers) > maxLayers {
		return nil, fmt.Errorf("uio: %d layers in the chain, at most %d", len(layers), maxLayers)
	}
	var handler Handler = ev.Handler
	if len(layers) > 0 {
		for _, layer := range layers {
			if layer == nil {
				return nil, errors.New("uio: a nil Layer in the chain")
			}
		}
		if fh, ok := asFuncs(handler); handler == nil || (ok && fh == nil) {
			// No Handler — nil, or a nil *FuncHandler, as resolveCallbacks
			// reads one: no callbacks and the input discarded. FuncHandler's
			// zero value does exactly that.
			handler = FuncHandler{}
		}
		driver := &stackDriver{
			layers:        append([]Layer(nil), layers...),
			top:           handler,
			limit:         ev.effectiveMaxOutbound(),
			inLimit:       ev.MaxInboundBuffered,
			halfOpen:      ev.AllowHalfOpen,
			blockingWrite: ev.BlockingWrite,
		}
		handler = driver
	}
	cb := resolveCallbacks(handler, ev.Observer, ev.Hooks)
	if len(layers) == 0 {
		return &entry{ev: ev, cb: cb, directConsumer: true}, nil
	}
	if cb.onBytesRead != nil {
		next := cb.onBytesRead
		cb.onBytesRead = func(c Conn, n int) { next(observedConn(c), n) }
	}
	if cb.onBytesWritten != nil {
		next := cb.onBytesWritten
		cb.onBytesWritten = func(c Conn, n int) { next(observedConn(c), n) }
	}
	return &entry{
		ev:             ev,
		cb:             cb,
		directConsumer: false,
		dial:           prepareLayeredDial,
	}, nil
}

// plainEntry is the entry of a connection without a chain: the Events' own
// callbacks. Serve builds it with the rest of the configuration; an Events
// built by hand — in tests — gets it here.
func (ev *Events) plainEntry() *entry {
	ev.plainOnce.Do(func() {
		if ev.plain == nil {
			plain, err := ev.newEntry(nil)
			if err != nil {
				panic(err) // a chain of no layers cannot fail
			}
			ev.plain = plain
		}
	})
	return ev.plain
}

// entryFor hands an endpoint its entry: the Events' shared plain one when it
// brings no chain of its own, or the one its chain builds.
func (ev *Events) entryFor(layers []Layer, hasLayers bool) (*entry, error) {
	if !hasLayers {
		return ev.plainEntry(), nil
	}
	return ev.newEntry(layers)
}

// validateChain reports a chain no connection can carry: past the stack's
// layer cap, or a nil layer.
func validateChain(layers []Layer, hasLayers bool) error {
	if !hasLayers {
		return nil
	}
	if len(layers) > maxLayers {
		return fmt.Errorf("uio: %d layers in the chain, at most %d", len(layers), maxLayers)
	}
	for _, layer := range layers {
		if layer == nil {
			return errors.New("uio: a nil Layer in the chain")
		}
	}
	return nil
}

// LayerOf returns the LayerHandler of the layer that built conn
// — the connection a layer hands up — so the layer above can ask it what it
// negotiated, a TLS layer's connection state:
//
//	if tls, ok := uio.LayerOf(conn).(interface{ State() tls.ConnectionState }); ok { … }
//
// It returns nil for a connection no layer built. FindLayer looks further
// down.
func LayerOf(conn Lower) LayerHandler {
	if l, ok := unwrapBase(conn).(*layerNode); ok {
		return l.conn
	}
	return nil
}

// FindLayer returns the first LayerHandler of type T at or below conn —
// starting with the layer that built conn, down to the socket: the TLS state
// under a WebSocket layer:
//
//	tls, ok := uio.FindLayer[interface{ State() tls.ConnectionState }](conn)
func FindLayer[T any](conn Lower) (T, bool) {
	conn = unwrapBase(conn)
	for {
		l, ok := conn.(*layerNode)
		if !ok {
			var none T
			return none, false
		}
		if found, ok := l.conn.(T); ok {
			return found, true
		}
		conn = l.lower
	}
}

// ErrLayersNeedStream reports a packet connection — UDP, IP, unixgram,
// unixpacket — on an endpoint whose layer chain does not start with a
// [PacketLayer]: a layer carries a byte stream, and only a PacketLayer takes
// a packet connection's messages, boundaries kept.
var ErrLayersNeedStream = errors.New("uio: layers carry streams, not packet connections")

// packetNetwork reports a network whose connections carry messages, not a
// byte stream.
func packetNetwork(network string) bool {
	switch {
	case strings.HasPrefix(network, "udp"), strings.HasPrefix(network, "ip"),
		network == "unixgram", network == "unixpacket":
		return true
	}
	return false
}

// refuseLayerPacket refuses a packet network on an endpoint whose chain does
// not start with a layer that takes packets below. An endpoint without a
// chain is not refused: it takes the packets itself.
func refuseLayerPacket(network string, layers []Layer, layered bool) error {
	if !layered || !packetNetwork(network) {
		return nil
	}
	if len(layers) > 0 && isPacketLayer(layers[0]) {
		return nil
	}
	return ErrLayersNeedStream
}

// observedConn is the connection an Observer hears about on a layered
// endpoint: the top layer's once the stack is attached, so it sees the same
// identity as the Handler.
func observedConn(socket Conn) Conn {
	if c := stackOf(socket); c != nil {
		return c.topConn()
	}
	return socket
}
