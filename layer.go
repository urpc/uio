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
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Layer is a protocol layer between the socket and the Handler: TLS,
// compression, an obfuscation, a handshake of its own. It transforms bytes —
// what arrives on its way up, what the Handler writes on its way down — and
// UIO carries everything else: the connection's turns, the Handler's
// callbacks, its addressing and options, its closes. An endpoint stacks
// layers under the [Events.Handler]: [WithLayers] in a [Events.Listen] or in
// a dial names the chain its connections run.
//
// A layer carries one connection: one connection above for each one below.
// A protocol that opens several streams over one connection — HTTP/2, QUIC —
// runs as the Handler, over a connection the layer below it made.
//
// Attach runs once per connection and returns that connection's [LayerHandler];
// the [LayerContext] it receives is the connection's place in the stack: the
// layer reads what arrived with Down, hands the layer above
// what that became with Up().Write, and writes down with
// Down().Write. The layer above — the next layer or the Handler — gets
// a [Conn] UIO builds over this one: its reads are what this layer
// delivered, its writes reach this layer's OnWrite, and everything else is
// the connection's own, passed through.
//
// What UIO guarantees every layer, so that none has to remember it:
//
//   - Every turn reaches the layers above, whether or not anything was
//     delivered, so their timers and wakes work through any stack.
//   - Each layer has a timer of its own, [SetTimer], and the
//     Handler's [Conn.SetWakeDeadline] is a slot of its own too: no layer
//     replaces another's.
//   - OnOpen, OnRead, OnTimer, OnWireSent and OnClose run on the
//     connection's turns, which never overlap one another or the callbacks
//     of the layers above. The rest run on the goroutine that called for
//     them, and may overlap the turns and one another: OnWrite on the
//     writer's, CloseGracefully and CloseWrite on the one closing the
//     connection, OutboundHeld on a writer's and Flush on a turn's or the
//     caller of Flush. State they share with the turns needs a lock.
//   - UIO calls a layer only on a turn or from above it — the Handler, or
//     the layer above through its LayerContext — never from below or from
//     inside another layer's callback: a layer may hold its lock while it
//     writes or flushes below, and nothing calls it back meanwhile.
//     LayerUp.Open only records the request — the layers above open after
//     the calling callback returned, on the same turn. A close a layer asks
//     for (LayerContext.Close or Abort) likewise runs its goodbye and the
//     walk below on the turn, never inside the call. Send reports, which can
//     come inside a write, reach the layers on a turn; an end CloseWrite
//     leaves waiting is passed down on one.
//   - Once the connection is closing — any layer, or the Handler, closed
//     it — or ended, UIO starts no call into a layer, checking again after
//     every callback, so a layer that closes from OnWireSent or Flush is the
//     last called: a write, flush or close that comes after returns
//     net.ErrClosed, and only OnClose follows. The close itself is the one
//     caller left: the goodbyes it walks, and the flushes it makes below the
//     layer that began it, run while every layer already reads closing. A
//     call already under way on another goroutine may still overlap.
//   - What the layers hold for the layers above counts against
//     MaxInboundBuffered with the socket's own buffer: a stack whose layers
//     hold the limit pulls nothing more from below until a read above makes
//     room.
//   - Layers carry streams. An Events with layers refuses packet
//     connections — UDP, IP, unixgram, unixpacket — with
//     ErrLayersNeedStream.
//   - Every connection ends once, top down: the Handler's OnClose first, if
//     it opened, then every layer's. A connection whose stack never opened —
//     a failed handshake — ends without a Handler callback, and a dial
//     reports its cause instead.
//   - The end of a stream travels up: a layer whose peer ended its stream
//     calls Up().End, and the layer above reads io.EOF once it has
//     read the rest.
type Layer interface {
	// Attach builds the layer's state for one connection. The LayerContext
	// stays valid until the connection's OnClose.
	Attach(ctx LayerContext) LayerHandler
}

// LayerHandler is a layer's instance on one connection: the callbacks UIO runs
// as the connection's bytes and events pass through the layer.
type LayerHandler interface {
	// OnOpen runs once the layers below opened: the place to start a
	// handshake. The layers above open when Up().Open is called, here
	// or on a later turn; a layer that negotiates nothing calls it here.
	OnOpen(ctx LayerContext)

	// OnRead runs on a turn once the layer opened: bytes from below may
	// have arrived, or none. It reads them from Down, consumes
	// them with Discard, and hands up what they became with
	// Up().Write. It may take one chunk at a time: while it consumes
	// and bytes still wait below, the turn calls it again. The bottom layer is
	// not called at all while the stack holds MaxInboundBuffered unread, until
	// a read above makes room; the layers above it go on moving what the stack
	// already holds up. A layer whose own delivery to the layer above is full
	// waits the same way — its next call is the turn the layer above reads on.
	OnRead(ctx LayerContext) error

	// OnWrite runs for what the layer above writes, on the writer's
	// goroutine and possibly beside other writers: it transforms p, writes
	// the result with Down().Write, and reports how much of p it took.
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
// its peer a stream it did not end itself. PassThrough implements it by
// passing CloseWrite to the layer below. UIO runs it once what was written
// before the call has gone below: writes still in flight into the layer
// finished, the layer flushed if it is a Flusher, and what an OutboundHolder
// still holds sent — so the end never overtakes what it follows. Writes the
// call refused never reach the layer.
type CloseWriter interface {
	CloseWrite(ctx LayerContext) error
}

// GracefulCloser is implemented by a LayerHandler with a goodbye to send —
// a TLS close_notify, a WebSocket Close — when the layer above closes the
// connection. It runs before the close passes to the layer below, with the
// close's cause: nil, io.EOF or io.ErrUnexpectedEOF for a graceful close —
// Close's cause is io.ErrUnexpectedEOF, as the socket's own Close is — an
// error for an abort.
//
// A graceful close of a layered connection lets what was queued leave first,
// the goodbyes included: the socket's queue drains, bounded by LingerTimeout
// without progress, before the connection releases — the socket's own
// CloseGracefully carries it, and leftover output reaches OnClose as
// UnflushedError. Nothing reaches the layers or the Handler meanwhile, and
// what arrives is discarded. An abort closes at once. A layer that closes the
// connection itself, through LayerContext.Close or Abort, ends it for the
// layers above too: what they hold is dropped. Its own CloseGracefully runs
// on a later turn, never inside the callback that asked, and UIO does not
// flush such a layer on its way — it writes what it holds below itself
// before it closes.
type GracefulCloser interface {
	CloseGracefully(ctx LayerContext, cause error)
}

// WireCounterObserver watches the [WireCounter] every layered Conn shares:
// OnWireSent is its WireSent moving, pushed. A protocol whose state moves as
// its output leaves — a ping whose timeout starts once it is on the wire, a
// close that waits for its frame to go — implements it instead of polling.
// OnWireSent reports n bytes the socket sent since the last call, counted on
// the wire: below every layer, so a layer under which others add or remove
// bytes cannot match n to its own. What it can rely on is the queue: once
// Lower's OutboundBuffered is zero, everything it wrote left. It runs on the
// connection's turn — the one running as the send finished, or one the
// report asks for — as the turn ends, possibly more than once before that
// turn ends, and may write: the socket may report a
// send inside the write that made it, where no layer can be called.
//
// There is no observing the wire the other way, because a layer never needs
// to be told what arrived: it reads what reached it itself, from Lower, on
// its own OnRead, so inbound has no hidden progress to report. Outbound
// does: what a layer wrote below leaves later, transformed by layers it
// cannot see, and only the socket knows when.
type WireCounterObserver interface {
	OnWireSent(ctx LayerContext, n int)
}

// OutboundHolder is implemented by a LayerHandler that cannot always write
// below at once — a send window waiting for its peer, a compressor gathering
// a block, a gate holding bytes back — and answers for the bytes it took and
// still holds: OutboundHeld reports how many. They count in the
// OutboundBuffered of the connection above it, in the MaxOutboundBuffered
// check, and in what CloseWrite sends before the end: a byte counts until
// its write below has returned. A graceful close waits for the socket's
// queue alone; a holder's bytes travel there through Flush. OutboundHeld runs on writers' goroutines and in send reports,
// beside the turns, so it must not wait on a lock the layer holds while it
// writes below. A holder that sends what it held outside Flush and the turns
// — a send window opening on a goroutine of its own — calls
// LayerContext.Wake once the write returns, so what waits for the bytes to
// leave sees them gone.
type OutboundHolder interface {
	OutboundHeld(ctx LayerContext) int
}

// Flusher is implemented by a LayerHandler that holds output it can send now.
// UIO flushes it at the end of every turn — top down, so what one layer
// flushes reaches the next — and as a graceful close from above passes it,
// after its goodbye; it also runs when the layer above calls Flush, before
// the flush passes below. Output a layer holds from a write outside the connection's turns
// waits for the next turn: such a layer calls LayerContext.Wake, or arms its
// timer, to have one.
type Flusher interface {
	Flush(ctx LayerContext) error
}

// WireCounter is implemented by the Conn a layer builds: positions on the
// wire, below every layer. WireSent is what the socket reported sent so far,
// and only grows. WireQueued is where the queue below this Conn ends: what
// the socket was handed, with what the layers below — this Conn's own layer
// and those under it, never those above — still hold for it.
//
// A protocol above layers that change how many bytes go out follows its own
// output by them: what it wrote has left once WireSent reaches the WireQueued
// it read after writing — never before. The reading is no earlier than the
// true end, and may be later: other writers' bytes count too, a layer handing
// bytes down as it is read counts them twice, and bytes a layer holds count
// as they are, though it may send fewer. So a target may never be reached;
// once the Conn's OutboundBuffered is zero, everything written to it left,
// whatever WireSent says. A LayerHandler that would rather be told than poll
// implements [WireCounterObserver]: OnWireSent is WireSent's change, pushed.
type WireCounter interface {
	WireQueued() uint64
	WireSent() uint64
}

// SendReceipt marks one tracked write's place on the wire. The write has
// left once WireSent reaches the receipt's position, the queue below the
// connection it entered is empty — everything handed down left, whatever the
// counts say — or the connection ended; on an end the receipt is not
// reported, and the layer's OnClose is the notice. The zero value marks no
// bytes and is never reported.
type SendReceipt struct{ wire uint64 }

// done reports a receipt the wire passed; zero marks nothing and is done.
func (r SendReceipt) done(sent uint64) bool { return r.wire == 0 || sent >= r.wire }

// TrackedWriter is implemented by the connection below a layer — what
// LayerContext.Down returns — when it can report when one of its writes has
// left the wire: WireCounter tells a layer where the wire is, and a tracked
// write tells it where its own bytes are.
//
//	n, r, err := ctx.Down().(uio.TrackedWriter).WriteTracked(p)
//
// The receipt is the write's output position, taken once the write returned:
// above layers that add or remove bytes, it counts the position the write
// left at, never one guessed from byte counts. A layer that wants the
// notice implements [ReceiptObserver].
type TrackedWriter interface {
	WriteTracked(p []byte) (int, SendReceipt, error)
}

// ReceiptObserver is implemented by a LayerHandler whose tracked writes it
// wants to hear about: OnSent runs on the connection's turn, once per
// receipt, and may write — a write it makes gets the turn that carries it.
// Receipts retire when the wire passes them, roughly in write order: a
// target is a wire position, so a layer whose output shrinks below it (a
// compressor holding plaintext) can retire a later write first. A receipt
// still outstanding when the close begins is not reported — once a close
// starts no layer call follows but the close itself, and OnClose is the
// notice.
type ReceiptObserver interface {
	OnSent(ctx LayerContext, r SendReceipt)
}

// DefaultDeliverBound bounds what one layer delivers to the layer above
// before that layer reads — each layer's delivery, whichever way the stack is
// stacked. Events.MaxInboundBuffered, when set, is the bound instead.
const DefaultDeliverBound = 64 << 10

// maxLayers is how many layers one stack takes: their timers, and the
// Handler's, share one bit mask.
const maxLayers = 63

// LayerContext is a layer's handle on its place in one connection's stack:
// a small hub with two faces. Down is the connection below, Up the layer
// above's input — everything the layer does goes through one of them.
type LayerContext interface {
	// Down is the connection below: the socket, for the first layer
	// installed; the one the layer below built, for the others. Its inbound
	// methods — Peek, PeekChunk, Discard, Read — are this layer's to call, in
	// OnRead and OnTimer; its writes, options and addressing at any time.
	// Its Userdata is the connection's, and its SetWakeDeadline arms this
	// layer's own timer, as SetTimer does. It carries no lifecycle: see
	// Close and Abort.
	Down() LayerDown

	// Up is the layer above's input: write to it, end it, open it.
	Up() LayerUp

	// Close ends the connection from this layer, gracefully: writes are
	// refused at once, and on the connection's next turn this layer's goodbye
	// says first, with cause, then the layers below say theirs, and the
	// socket drains what was accepted before it closes — the same algorithm
	// as a close from above, begun here. Neither this call nor the turn calls
	// back into the callback that asked. Nothing above this layer is called
	// again. net.ErrClosed reports a connection already closing.
	Close(cause error) error

	// Abort ends the connection at once from this layer: goodbyes are told
	// the cause — on the next turn, as Close's are — but nothing is flushed
	// or drained, what the layers above it hold is dropped, and the socket
	// closes. The mode is this call's, never read back from the cause.
	Abort(cause error) error

	// SetTimer arms this layer's own timer for when; a zero time stops it.
	// Down's SetWakeDeadline and SetWakeInterval arm the same timer.
	SetTimer(when time.Time) error

	// Wake runs a turn of the connection.
	Wake() error
}

// LayerDown is the connection below a layer as the layer sees it: its
// bytes, its options and its addressing — [Conn] without the lifecycle
// verbs, because a layer ends the connection through
// [LayerContext.Close] or [LayerContext.Abort], where the stack runs the one
// close algorithm from the layer that began it. CloseWrite stays: ending the
// writing half one direction is part of a layer's own protocol.
type LayerDown interface {
	io.Reader
	io.Writer
	io.StringWriter
	io.ByteWriter

	LocalAddr() net.Addr
	RemoteAddr() net.Addr
	Userdata() any
	SetUserdata(value any)
	SetLinger(secs int) error
	SetNoDelay(nodelay bool) error
	SetKeepAlive(keepalive bool) error
	SetKeepAlivePeriod(secs int) error
	Control(fn func(fd uintptr)) error
	SetReadBuffer(size int) error
	SetWriteBuffer(size int) error
	SetDeadline(t time.Time) error
	SetReadDeadline(t time.Time) error
	SetWriteDeadline(t time.Time) error

	Peek(b []byte) []byte
	PeekChunk() []byte
	Discard(n int) (int, error)
	InboundBuffered() int
	OutboundBuffered() int
	ReserveInbound(n int) error
	Writev(vec [][]byte) (int, error)
	WriteOwned(buffer *Buffer) (int, error)
	ReserveOutbound(n int) ([]byte, error)
	Flush() error

	// State reports where the connection's streams stand: the peer's end
	// once everything it sent was read, this end's writing half, and a close.
	// It reads state the turns keep, so call it on the connection's turn.
	State() ConnState

	Wake() error
	SetWakeDeadline(when time.Time) error
	SetWakeInterval(interval time.Duration) error
	YieldRead() error
	CloseWrite() error
}

// LayerUp is the layer above's input, as the layer below writes to it. Write
// always takes all of p and never fails. What the delivery holds is bounded
// by the framework, not by the writer: once a layer's delivery to the layer
// above reaches DefaultDeliverBound — Events.MaxInboundBuffered, when set —
// the framework stops calling that layer until the layer above reads, so a
// layer never paces its own writes. A layer may hold the Up it got.
// Its methods run on the connection's turn — in OnOpen, OnRead or OnTimer;
// Write also in OnWireSent and OnSent, whose turns carry what it wrote —
// and a call made anywhere else races the turn.
type LayerUp interface {
	io.Writer

	// End tells the layer above the stream ended: once it read what was
	// written, its Read reports io.EOF. A layer calls it when its peer ended
	// its stream — the socket's end, a TLS close_notify. At the top of the
	// stack, an Events without AllowHalfOpen closes the connection with
	// io.EOF once the Handler's OnRead has run; with it, the Handler may go on
	// writing and end its own half with CloseWrite. Call it in OnRead or
	// OnTimer.
	End()

	// Open opens the layers above, once the layer's negotiation settled — at
	// once from OnOpen if it has none. A dial returns once the top layer
	// opened. Call it on the connection's turn.
	Open()

	// Reserve returns at least n bytes of room past what is written, for the
	// layer to fill in place — decrypting straight into it — and Commit
	// delivers the first n of them. Between the two, write nothing else up:
	// Write appends where Reserve's room begins. Call both in OnRead or
	// OnTimer.
	Reserve(n int) []byte
	Commit(n int)

	// Buffered is how many written bytes the layer above has not read.
	Buffered() int

	// Yield tells UIO the layer stopped short of what it could write up —
	// its bound was reached, with bytes waiting below or in its own buffers —
	// so the next read above runs a turn to take the rest. A layer that
	// writes up everything it is given never needs it.
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

func (passThroughHandler) CloseWrite(ctx LayerContext) error { return ctx.Down().CloseWrite() }

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
func (PassThrough) OnOpen(ctx LayerContext) { ctx.Up().Open() }

// OnRead delivers what arrived as it is, and the end of the stream after
// it.
func (PassThrough) OnRead(ctx LayerContext) error {
	lower := ctx.Down()
	if chunk := lower.PeekChunk(); len(chunk) > 0 {
		_, _ = ctx.Up().Write(chunk)
		if _, err := lower.Discard(len(chunk)); err != nil {
			return err
		}
	}
	if lowerEnded(lower) {
		ctx.Up().End()
	}
	return nil
}

// lowerEnded reports a stream below that ended and was read to its end, as
// lower.Read(nil) reporting io.EOF does.
func lowerEnded(lower LayerDown) bool {
	return lower.State()&StatePeerEnded != 0
}

// OnWrite writes what is written as it is.
func (PassThrough) OnWrite(ctx LayerContext, p []byte) (int, error) { return ctx.Down().Write(p) }

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
		handler = &stackDriver{
			layers:   append([]Layer(nil), layers...),
			top:      handler,
			limit:    ev.MaxOutboundBuffered,
			inLimit:  ev.MaxInboundBuffered,
			halfOpen: ev.AllowHalfOpen,
		}
	}
	cb := resolveCallbacks(handler, ev.Observer, ev.Hooks)
	if len(layers) > 0 {
		cb.onBytesWritten = stackSent(cb.onBytesWritten)
	}
	return &entry{ev: ev, cb: cb, layered: len(layers) > 0}, nil
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

// stackDriver is the socket's Handler when layers are installed: it builds
// each connection's stack on OnOpen and drives it from then on. The stack
// lives in the socket's userdata slot; the userdata the connection came with
// moves to the stack, where every layer's Userdata reads it.
type stackDriver struct {
	layers   []Layer
	top      Handler
	limit    int  // MaxOutboundBuffered, which the stack's top enforces
	inLimit  int  // MaxInboundBuffered, which bounds what the layers hold too
	halfOpen bool // AllowHalfOpen: the Handler may outlive its peer's end
}

func (h *stackDriver) OnOpen(socket Conn) {
	c := &stack{top: h.top, socket: socket, limit: h.limit, inLimit: h.inLimit, halfOpen: h.halfOpen}
	held := socket.Userdata()
	if probe, ok := held.(*dialProbe); ok {
		c.probe.Store(probe)
		held = probe.user
	}
	layers := h.layers
	c.userdata.Store(&held)
	c.layers = make([]*layerNode, len(layers))
	var below Conn = socketLower{Conn: socket, stack: c}
	for i := range layers {
		node := &layerNode{stack: c, index: i, lower: below}
		node.up = layerUp{node}
		c.layers[i] = node
		below = node
	}
	socket.SetUserdata(c)
	for i, layer := range layers {
		node := c.layers[i]
		node.conn = layer.Attach(layerCtx{node})
		node.ops = opsOf(node.conn)
		if node.ops.wireSent != nil {
			c.sends = append(c.sends, node)
		}
		if node.ops.held != nil {
			c.holders = true
		}
	}
	for i := len(c.layers) - 1; i >= 0; i-- {
		if c.layers[i].ops.flush != nil {
			c.flushers = append(c.flushers, c.layers[i])
		}
	}
	c.inTurn.Store(true)
	defer c.inTurn.Store(false)
	c.openUpTo()
	if err := c.finishTurn(); err != nil {
		_ = c.closeSocket(err)
	}
}

func (h *stackDriver) OnRead(socket Conn) error {
	if c, ok := socket.Userdata().(*stack); ok {
		return c.turn()
	}
	return nil
}

// OnTimer does nothing: with layers, the stack's own timer table drives the
// turn, and run dispatches the Handler's slot to the Handler itself. The
// socket-level mirror the stack aims at is therefore already accounted for.
func (h *stackDriver) OnTimer(socket Conn) error { return nil }

func (h *stackDriver) OnClose(socket Conn, err error) {
	switch held := socket.Userdata().(type) {
	case *stack:
		held.closed(err)
	case *dialProbe:
		// Closed before OnOpen built the stack: the Handler never heard of
		// the connection, and the dial that asked for it hears the cause.
		held.fail(err)
	}
}

// stack is one connection's layers, bottom up, with the Handler they serve
// and the state they share.
type stack struct {
	top      Handler
	socket   Conn
	layers   []*layerNode
	probe    atomic.Pointer[dialProbe] // a dial waiting for the stack to open, until it does
	limit    int                       // MaxOutboundBuffered; see reserve
	reserved atomic.Int64              // bytes admitted at the top, not yet queued below
	halfOpen bool                      // AllowHalfOpen
	inLimit  int                       // MaxInboundBuffered; see inboundFull
	sends    []*layerNode              // the layers that observe sends, bottom up
	holders  bool                      // some layer holds output of its own
	flushers []*layerNode              // the layers that hold output to flush, top down

	// closeCause is the cause of the close that began the stack's, for the
	// dial a closing open stage fails; closePlan is a close a layer began for
	// itself, walked on the turn, never inside the callback that asked.
	closeCause atomic.Pointer[error]
	closePlan  atomic.Pointer[closePlan]

	// receiptMu guards receipts, the outstanding tracked writes in the order
	// they entered the stack; receiptCount spares a turn the lock while none
	// is outstanding.
	receiptMu    sync.Mutex
	receipts     []pendingReceipt
	receiptCount atomic.Int32

	// inHeld marks a stack that stopped pulling from below because what its
	// layers hold reached inLimit; a read above that makes room wakes it.
	inHeld atomic.Bool

	userdata atomic.Pointer[any] // every layer's Userdata, the Handler's

	topOpen atomic.Bool // the Handler's OnOpen ran; set on a turn
	// openedCount is how many layers opened, bottom up: the prefix of the
	// stack openUpTo resumes from. Only turns touch it.
	openedCount int

	// state packs the lifecycle phase (low bits) with the close initiator's
	// index+1 (high bits), so a close publishes both in one store: a write
	// that sees closing also sees which layers may still write down, and a
	// turn that sees closed sees it before any OnClose runs.
	state atomic.Uint32

	// shutsPending counts the layers whose CloseWrite has not passed below
	// yet; see shutDown.
	shutsPending atomic.Int32

	// inTurn is set while a turn runs; what waits for one — send reports for
	// the layers, an end CloseWrite left waiting — is done as it ends rather
	// than asking for another. sentPending is what the socket reported sent
	// that the layers have not heard; see stackSent. turnAsked marks a turn
	// requestTurn asked for and no turn has begun since: one is enough for
	// any number of requests.
	inTurn      atomic.Bool
	turnAsked   atomic.Bool
	sentPending atomic.Int64
	// upPending marks a layer that wrote up in OnWireSent: the turn running
	// has delivered already, so settle asks for the one that carries it.
	upPending atomic.Bool

	// wireQueued and wireSent count, on the wire, what the socket was handed
	// and what it reported sent; see WireCounter.
	wireQueued atomic.Uint64
	wireSent   atomic.Uint64

	// timers holds the timer of every layer, and in its last slot the one
	// the Handler armed with SetWakeDeadline; the socket's own wake slot
	// carries the earliest of them. hasTimers spares a turn the lock while
	// none was ever armed.
	hasTimers atomic.Bool
	timerMu   sync.Mutex
	timers    []timerSlot
	armed     time.Time
}

// closePlan is a close a layer began for itself — through LayerContext.Close
// or Abort — or a callback's failure: walked on the connection's turn, so no
// layer is called back inside the callback that asked. from is the
// initiator's index, stackNoWriters for the socket alone, len(layers) for a
// close from the Handler.
type closePlan struct {
	cause    error
	graceful bool
	from     int32
}

// timerSlot is one layer's timer: when it is due, and the interval it
// repeats at, if any.
type timerSlot struct {
	when  time.Time
	every time.Duration
}

// turn is one turn of the socket: each opened layer bottom up, its due
// timer first, then the Handler. Every turn reaches the Handler once it is
// open, whether or not a layer delivered anything.
func (c *stack) turn() error {
	if c.closedStack() {
		return nil
	}
	// Cleared before the turn looks at anything, so a request after this
	// point asks for a turn of its own.
	if c.turnAsked.Load() {
		c.turnAsked.Store(false)
	}
	if c.over() {
		// Closing — a graceful close waiting for its queue, or aborted with
		// the socket's close still to come: what arrives is no longer
		// anyone's, and kept it would grow for as long as the queue takes
		// to drain. A close a layer began for itself walks here.
		_, _ = c.socket.Discard(c.socket.InboundBuffered())
		c.runClosePlan()
		c.settle()
		return nil
	}
	c.inTurn.Store(true)
	defer c.inTurn.Store(false) // a panic an Executor recovers leaves none set
	err := c.run()
	if err != nil {
		// A layer failed: the connection aborts — the goodbyes are told the
		// cause, nothing is flushed or drained, and nothing reaches a layer
		// after this point. The walk runs here, off the failing callback; the
		// core's close follows this turn, and OnClose with it.
		c.inTurn.Store(false)
		if !c.over() {
			c.recordCloseCause(err)
			c.beginClosing(len(c.layers))
			c.closePlan.Store(&closePlan{cause: err, from: int32(len(c.layers))})
		}
		c.runClosePlan()
		return err
	}
	if flushErr := c.finishTurn(); err == nil {
		err = flushErr
	}
	return err
}

// over reports a stack no layer is called on any more: one that ended, or
// one that is closing. A layer's callback can end it — a close from OnWireSent,
// a protocol error — so every call into a layer is followed by this check
// before the next.
func (c *stack) over() bool { return c.state.Load()&stackPhaseMask >= stackClosing }

// closedStack reports the socket's OnClose run: no turn and no callback is
// owed any more.
func (c *stack) closedStack() bool { return c.state.Load()&stackPhaseMask == stackClosed }

// stackNoWriters is the close initiator of a close that came from no layer —
// the socket's directly: no layer, and no Handler write, may go down.
const stackNoWriters = -1

// beginClosing publishes the stack as closing on behalf of the layer at
// initiator, reporting whether this call published it. The close that does
// runs the goodbyes; one that finds the stack closing already passes its
// cause to the socket alone.
func (c *stack) beginClosing(initiator int) bool {
	for {
		state := c.state.Load()
		if state&stackPhaseMask >= stackClosing {
			return false
		}
		next := uint32(initiator+1)<<stackPhaseBits | stackClosing
		if c.state.CompareAndSwap(state, next) {
			return true
		}
	}
}

// markOpen turns an opening stack open: every layer and the Handler opened.
func (c *stack) markOpen() {
	for {
		state := c.state.Load()
		if state&stackPhaseMask != stackOpening {
			return
		}
		if c.state.CompareAndSwap(state, state&^stackPhaseMask|stackOpen) {
			return
		}
	}
}

// markClosed publishes the stack closed, reporting whether this call
// published it: the socket's OnClose runs the callbacks once.
func (c *stack) markClosed() bool {
	for {
		state := c.state.Load()
		if state&stackPhaseMask == stackClosed {
			return false
		}
		if c.state.CompareAndSwap(state, state&^stackPhaseMask|stackClosed) {
			return true
		}
	}
}

// mayWriteDown reports whether a write entering the layer at index may enter
// the stack: anything while it is open, and while it is closing only the
// goodbyes of the layers below the close's initiator — never a Handler's
// write, and never through the initiator's own OnWrite.
func (c *stack) mayWriteDown(index int) bool {
	state := c.state.Load()
	switch state & stackPhaseMask {
	case stackClosing:
		return uint32(index+1) < state>>stackPhaseBits
	case stackClosed:
		return false
	}
	return true
}

// stackPhase is the stack's lifecycle; the low bits of stack.state.
const (
	stackOpening uint32 = iota // the layers open; the dial waits
	stackOpen                  // settled: turns, writes and goodbyes flow
	stackClosing               // a close began: no layer call, no new write
	stackClosed                // the socket's OnClose ran
)

const (
	stackPhaseBits = 8
	stackPhaseMask = 1<<stackPhaseBits - 1
)

// finishTurn ends a turn: the layers hear what was sent, then flush what
// they hold — what OnWireSent wrote too — unless the connection is closing, an
// abort dropping it and a graceful close having flushed already. A flush
// can send, and the socket report it right away, inside the flush: the loop
// takes each report on the turn it ended, so what the layers write for it
// is flushed too, and only a report that lands after the last look waits
// for a turn of its own.
func (c *stack) finishTurn() error {
	var err error
	if len(c.sends) == 0 && len(c.flushers) == 0 {
		// Nothing to deliver or flush: only settle's look, and that only
		// when an end, a receipt or a close waits for a turn.
		c.inTurn.Store(false)
		if c.shutsPending.Load() > 0 || c.receiptCount.Load() != 0 || c.over() {
			c.settle()
		}
		return nil
	}
	for !c.over() {
		c.deliverSent()
		if c.over() {
			break // a layer ended the connection: nothing more reaches any
		}
		c.openUpTo() // an Up().Open an OnWireSent made opens the layers above now
		if flushErr := c.flushHeld(); err == nil {
			err = flushErr
		}
		if c.sentPending.Load() == 0 || c.over() {
			break
		}
	}
	c.inTurn.Store(false)
	c.settle()
	return err
}

// settle does, as a turn ends, what waits for one: ends CloseWrite left
// waiting for output to go below. Send reports that came after the turn's own
// delivery — during its flush — get a turn of their own, which delivers them
// and then flushes what OnWireSent writes. It runs only where no layer is in
// a callback — the framework never calls a layer from inside another's — so a
// layer holding its own lock while it writes or flushes below is never called
// back. Once the connection is closing, nothing reaches the layers: the
// socket's own graceful close carries the queue from there.
func (c *stack) settle() {
	if c.closePlan.Load() != nil {
		// A close a layer began during this turn walks here, at its end, off
		// every callback.
		c.runClosePlan()
	}
	if c.over() {
		c.sentPending.Store(0)
		return
	}
	c.retireReceipts() // before the look: an OnSent write asks for its own turn
	if c.sentPending.Load() > 0 || c.upPending.Swap(false) {
		c.requestTurn()
	}
	c.finishShuts()
}

// requestTurn asks for a turn to settle what waits for one; a turn running
// now settles it as it ends. Requests made before the asked-for turn begins
// share it: that turn settles everything any of them waits for, so writes
// refused or send reports coming fast while turns are held up — a slow
// Observer, a busy loop — schedule one wake, not one each.
func (c *stack) requestTurn() {
	if !c.inTurn.Load() && !c.turnAsked.Load() && c.turnAsked.CompareAndSwap(false, true) {
		if c.socket.Wake() != nil {
			c.turnAsked.Store(false)
		}
	}
}

// deliverSent passes the layers that observe sends what the socket reported
// sent since they last heard.
func (c *stack) deliverSent() {
	if len(c.sends) == 0 {
		return
	}
	if n := c.sentPending.Swap(0); n > 0 {
		c.sent(int(n))
	}
}

// flushHeld flushes what the layers hold, top down, so what one flushes
// reaches the next: a turn ends with nothing held that a write in it made.
func (c *stack) flushHeld() error {
	for _, layer := range c.flushers {
		if !layer.opened.Load() {
			continue
		}
		if c.over() {
			return nil
		}
		if err := layer.ops.flush(layerCtx{layer}); err != nil {
			return err
		}
	}
	return nil
}

// run is a turn's work: the layers bottom up, then the Handler.
func (c *stack) run() error {
	due := c.takeDue()
	for _, layer := range c.layers {
		if !layer.opened.Load() {
			return nil
		}
		if due&(1<<uint(layer.index)) != 0 {
			if err := layer.conn.OnTimer(layerCtx{layer}); err != nil {
				return err
			}
			if c.over() {
				return nil
			}
		}
		if err := c.inbound(layer); err != nil {
			return err
		}
		if c.over() {
			return nil // a layer closed the connection: nothing above runs
		}
		c.openUpTo() // an Up().Open the layer made opens the layers above now
	}
	if !c.topOpen.Load() {
		return nil
	}
	if due&(1<<uint(len(c.layers))) != 0 {
		if err := c.top.OnTimer(c.topConn()); err != nil {
			return err
		}
		if c.over() {
			return nil
		}
	}
	top := c.layers[len(c.layers)-1]
	if err := c.top.OnRead(top); err != nil {
		return err
	}
	if top.ended && !c.halfOpen {
		return io.EOF // the end of the stream closes, as a socket's does
	}
	return nil
}

// inbound runs a layer's OnRead until it takes nothing more: the bytes
// below may sit in several chunks, and a layer reads one at a time, so it is
// called again while what waits below shrinks. It stops once nothing waits,
// the layer took nothing, or it stalled at its bound.
func (c *stack) inbound(layer *layerNode) error {
	for {
		// Only the bottom layer stops at the limit: it is the one taking new
		// bytes from the socket. The layers above go on moving what the stack
		// holds up to the Handler, which must be able to read it.
		if layer.index == 0 && c.inboundFull() {
			return nil
		}
		// A layer whose delivery to the layer above is full waits: it is not
		// called again until the layer above reads, and that read — roomMade
		// on this layer — asks for the turn. The layers above it go on
		// draining what it delivered.
		if layer.held() >= c.deliverBound() {
			layer.paced.Store(true)
			return nil
		}
		before := layer.waitingBelow()
		if err := layer.conn.OnRead(layerCtx{layer}); err != nil {
			return err
		}
		if c.over() || layer.stalled.Load() {
			return nil
		}
		// An Up().Open the layer made takes effect here, before the round
		// reads on into bytes that arrived behind its handshake.
		c.openUpTo()
		after := layer.waitingBelow()
		if after == 0 || after >= before {
			return nil
		}
	}
}

// deliverBound is how much one layer may deliver to the layer above:
// MaxInboundBuffered when set, DefaultDeliverBound otherwise.
func (c *stack) deliverBound() int {
	if c.inLimit > 0 {
		return c.inLimit
	}
	return DefaultDeliverBound
}

// inboundFull reports a stack whose layers hold MaxInboundBuffered or more
// that the Handler has not read: it takes nothing more from the socket, so
// the bytes wait there, and the socket's own hold stops its reads, until a
// read above makes room. It runs on the connection's turn.
func (c *stack) inboundFull() bool {
	if c.inLimit <= 0 {
		return false
	}
	held := 0
	for _, layer := range c.layers {
		held += layer.held()
	}
	if held < c.inLimit {
		return false
	}
	c.inHeld.Store(true)
	return true
}

// inboundRoom is a read above: a stack that stopped pulling takes another
// turn once its layers hold less than the limit. It runs on the turn.
func (c *stack) inboundRoom() {
	if c.inHeld.Load() && !c.inboundFullNow() && c.inHeld.CompareAndSwap(true, false) {
		_ = c.socket.Wake()
	}
}

func (c *stack) inboundFullNow() bool {
	held := 0
	for _, layer := range c.layers {
		held += layer.held()
	}
	return held >= c.inLimit
}

// openUpTo opens every layer that may open now, and the Handler past the
// top, iterating rather than recursing: a layer's Up().Open only records the
// request, and the framework opens the layers above after the callback
// returned — never from inside another callback. Turns run it after every
// callback that may ask, so a handshake that settles mid-round still opens
// the layers above on the same turn, and a round that finds bytes waiting
// below runs again once they are open. It runs on the connection's turn.
func (c *stack) openUpTo() {
	for !c.over() {
		if c.openedCount < len(c.layers) {
			i := c.openedCount
			if i > 0 && !c.layers[i-1].openUp() {
				return // the layer below has not opened the layers above it
			}
			layer := c.layers[i]
			c.openedCount = i + 1
			layer.opened.Store(true)
			c.reaim() // a timer it armed before it opened
			layer.conn.OnOpen(layerCtx{layer})
			continue // an Up().Open it made opens the next layer here, after it returned
		}
		if c.topOpen.Load() {
			return
		}
		if !c.layers[len(c.layers)-1].openUp() {
			return // the top layer has not opened the layers above it
		}
		if probe := c.probe.Load(); probe != nil && probe.claimed.Load() {
			// The dial gave up while a layer opened, and closes the
			// connection: the Handler never hears of it.
			return
		}
		// The Handler opens last, the stack turns open with it, and the dial
		// settles in the same step: DialContext returns a conn only after the
		// Handler's OnOpen returned, so the conn a dial holds is never one
		// the Handler has not heard of. A close that claimed the stack first
		// keeps it out — probe.settle finds the probe claimed and fails.
		c.topOpen.Store(true)
		c.reaim()
		c.top.OnOpen(c.topConn())
		if c.over() {
			// The Handler closed under its own OnOpen: the dial hears the
			// cause, not a settled connection already closing.
			if probe := c.probe.Swap(nil); probe != nil {
				cause := net.ErrClosed
				if held := c.closeCause.Load(); held != nil {
					cause = *held
				}
				probe.fail(cause)
			}
			return
		}
		c.markOpen()
		if probe := c.probe.Swap(nil); probe != nil {
			probe.settle(c.topConn())
		}
		return
	}
}

// closed is the socket's OnClose: the Handler hears it first, once it
// opened, then every layer, top down; a dial still waiting hears the cause.
func (c *stack) closed(err error) {
	if !c.markClosed() {
		return
	}
	c.stopTimers()
	c.sentPending.Store(0)
	c.closePlan.Store(nil)
	c.dropReceipts()
	if c.topOpen.Load() {
		c.top.OnClose(c.topConn(), err)
	}
	for i := len(c.layers) - 1; i >= 0; i-- {
		layer := c.layers[i]
		layer.conn.OnClose(layerCtx{layer}, err)
	}
	// The callbacks have read what they will; an app holding the closed Conn
	// must not keep the delivered buffers alive with it.
	for _, layer := range c.layers {
		layer.delivered = nil
		layer.at = 0
	}
	if probe := c.probe.Swap(nil); probe != nil {
		probe.fail(err)
	}
}

// topConn is the connection the Handler sees: the top layer's.
func (c *stack) topConn() Conn { return c.layers[len(c.layers)-1] }

// sent passes the socket's report of n bytes sent to the layers that
// observe sends. A layer that writes up in OnWireSent marks the write to
// wait for a turn: this one is running, and its pass has already delivered,
// so the turn asked for carries the bytes to the layer above.
func (c *stack) sent(n int) {
	for _, layer := range c.sends {
		if c.over() {
			return // a layer below ended the connection: the rest hear nothing
		}
		held := layer.held()
		layer.ops.wireSent(layerCtx{layer}, n)
		if layer.held() != held {
			c.upPending.Store(true)
		}
	}
}

// stackSent wraps the Observer's OnBytesWritten, if any, so a stack's send
// reports reach its layers.
//
// A send report can run inside the write that made it — a layer's OnWrite
// writing below while it holds a lock, crypto/tls its record lock — so it
// calls no layer: it counts the bytes, for WireSent at once and for the
// layers' OnWireSent on the next turn's end, and asks for that turn.
func stackSent(next func(Conn, int)) func(Conn, int) {
	return func(conn Conn, n int) {
		if l, ok := conn.(*layerNode); ok && !l.stack.closedStack() {
			c := l.stack
			c.wireSent.Add(uint64(n))
			if !c.over() && (len(c.sends) > 0 || c.receiptCount.Load() != 0) {
				c.sentPending.Add(int64(n))
				c.requestTurn()
			}
		}
		if next != nil {
			next(conn, n)
		}
	}
}

// gracefulCause reports a close that lets the queue drain first; see
// GracefulCloser.
func gracefulCause(cause error) bool {
	return cause == nil || errors.Is(cause, io.EOF) || errors.Is(cause, io.ErrUnexpectedEOF)
}

// queued is what the connection still has to send: the socket's queue and
// what the layers hold.
func (c *stack) queued() int {
	if c.holders {
		return c.topConn().OutboundBuffered()
	}
	return c.socket.OutboundBuffered()
}

// closeSocket is a close reaching the socket with no layer above to say a
// goodbye for: the stack moves to closing first, then the socket closes. A
// graceful one is the socket's own CloseGracefully — writes refused, queue
// drained, bounded by LingerTimeout without progress; see Conn.CloseGracefully.
// An abort closes at once, and cuts a graceful close's wait short.
func (c *stack) closeSocket(cause error) error {
	c.recordCloseCause(cause)
	return c.endAtSocket(cause, c.beginClosing(stackNoWriters), gracefulCause(cause))
}

// recordCloseCause keeps the first close's cause.
func (c *stack) recordCloseCause(cause error) {
	if cause != nil {
		c.closeCause.CompareAndSwap(nil, &cause)
	}
}

// socketClose ends the stack at the socket in the mode the walk carried, so a
// graceful close a layer began drains whatever its cause — the cause is
// reported, never re-read to choose the mode.
func (s socketLower) socketClose(cause error, graceful bool) error {
	s.stack.recordCloseCause(cause)
	return s.stack.endAtSocket(cause, s.stack.beginClosing(stackNoWriters), graceful)
}

// endAtSocket hands cause to the socket, graceful choosing between the
// socket's draining close and its one-shot one. first reports that this
// close moved the stack to closing: a socket found closed already is then
// the peer answering the close first, and the close reports itself done —
// reported by the close that did not move the stack as it is.
func (c *stack) endAtSocket(cause error, first, graceful bool) error {
	var err error
	if graceful {
		err = c.socket.CloseGracefully(cause)
	} else {
		err = c.socket.CloseWith(cause)
	}
	if first && errors.Is(err, net.ErrClosed) {
		return nil
	}
	return err
}

// reserve admits a write of n bytes from the Handler, or refuses it when the
// queue below has no room for it. It runs before the write enters the top
// layer: below it, a layer's output may already be bound to the wire — a TLS
// record carries a sequence number once encrypted — so the socket takes
// whatever the layers write. The room is reserved until release, once the
// write is queued below: a writer racing past the check while another is
// still inside a layer sees that one's bytes, so together they cannot pass
// the limit.
//
// It reserves first and checks after: the queue is read once the reservation
// is visible, so every other write is counted — still reserved, or already
// queued, since a write is queued before it releases. A compare-and-swap on
// the reservation alone could miss one that reserved, queued and released in
// between. Two writers that fit apart but not together may both be refused.
func (c *stack) reserve(n int) error {
	if c.limit <= 0 {
		return nil
	}
	reserved := c.reserved.Add(int64(n))
	if int64(c.queued())+reserved > int64(c.limit) {
		c.reserved.Add(-int64(n))
		return ErrOutboundOverflow
	}
	return nil
}

// release returns what reserve took, once the write it admitted is queued
// below, or failed.
func (c *stack) release(n int) {
	if c.limit > 0 {
		c.reserved.Add(-int64(n))
	}
}

// setTimer arms slot's timer, repeating every interval when it is positive,
// and aims the socket's wake slot at the earliest timer.
func (c *stack) setTimer(slot int, when time.Time, every time.Duration) error {
	c.timerMu.Lock()
	defer c.timerMu.Unlock()
	if c.closedStack() {
		return net.ErrClosed
	}
	if c.timers == nil {
		if when.IsZero() {
			return nil
		}
		c.timers = make([]timerSlot, len(c.layers)+1)
		c.hasTimers.Store(true)
	}
	c.timers[slot] = timerSlot{when: when, every: every}
	return c.aimLocked(time.Time{})
}

// aimLocked aims the socket's wake slot at the earliest timer. fired is the
// socket deadline that just brought a turn, if one did: the socket no
// longer holds it, so it must be armed again even when unchanged. It
// requires timerMu.
func (c *stack) aimLocked(fired time.Time) error {
	var earliest time.Time
	for i, slot := range c.timers {
		if !slot.when.IsZero() && c.slotOpen(i) && (earliest.IsZero() || slot.when.Before(earliest)) {
			earliest = slot.when
		}
	}
	if earliest.Equal(c.armed) && fired.IsZero() {
		return nil
	}
	c.armed = earliest
	return c.socket.SetWakeDeadline(earliest)
}

// reaim aims the socket's wake again once a layer opened, so a timer it
// armed before opening comes due.
func (c *stack) reaim() {
	if !c.hasTimers.Load() {
		return
	}
	c.timerMu.Lock()
	if c.timers != nil && !c.closedStack() {
		_ = c.aimLocked(time.Time{})
	}
	c.timerMu.Unlock()
}

// takeDue collects the timers due by now: one-shot ones are cleared,
// repeating ones move to their next period, and the socket's wake slot is
// re-aimed at what remains.
func (c *stack) takeDue() uint64 {
	if !c.hasTimers.Load() {
		return 0
	}
	c.timerMu.Lock()
	defer c.timerMu.Unlock()
	if c.timers == nil {
		return 0
	}
	now := time.Now()
	var due uint64
	for i := range c.timers {
		slot := &c.timers[i]
		if slot.when.IsZero() || slot.when.After(now) || !c.slotOpen(i) {
			continue
		}
		due |= 1 << uint(i)
		if slot.every > 0 {
			slot.when = slot.when.Add(slot.every)
			if !slot.when.After(now) {
				slot.when = now.Add(slot.every)
			}
		} else {
			slot.when = time.Time{}
		}
	}
	fired := time.Time{}
	if !c.armed.IsZero() && !c.armed.After(now) {
		fired = c.armed
	}
	if due != 0 || !fired.IsZero() {
		_ = c.aimLocked(fired)
	}
	return due
}

// slotOpen reports whether slot's owner opened: a timer armed before — from
// Attach — waits for it rather than firing into a layer that has not begun,
// and the socket's wake is not aimed at it until then.
func (c *stack) slotOpen(slot int) bool {
	if slot < len(c.layers) {
		return c.layers[slot].opened.Load()
	}
	return c.topOpen.Load()
}

// stopTimers drops a closed stack's timers; the socket's wake slot goes
// with the socket.
func (c *stack) stopTimers() {
	c.timerMu.Lock()
	c.timers = nil
	c.timerMu.Unlock()
}

// layerOps is the set of optional interfaces a LayerHandler implements,
// resolved once when the stack attaches it: the close and flush paths read
// the table instead of asserting at every call, and a layer without a
// capability costs one nil check.
type layerOps struct {
	graceful    func(LayerContext, error)       // GracefulCloser
	closeWriter func(LayerContext) error        // CloseWriter
	flush       func(LayerContext) error        // Flusher
	held        func(LayerContext) int          // OutboundHolder
	wireSent    func(LayerContext, int)         // WireCounterObserver
	sent        func(LayerContext, SendReceipt) // ReceiptObserver
}

// opsOf resolves a LayerHandler's optional interfaces.
func opsOf(h LayerHandler) layerOps {
	var ops layerOps
	if graceful, ok := h.(GracefulCloser); ok {
		ops.graceful = graceful.CloseGracefully
	}
	if closeWriter, ok := h.(CloseWriter); ok {
		ops.closeWriter = closeWriter.CloseWrite
	}
	if flusher, ok := h.(Flusher); ok {
		ops.flush = flusher.Flush
	}
	if holder, ok := h.(OutboundHolder); ok {
		ops.held = holder.OutboundHeld
	}
	if observer, ok := h.(WireCounterObserver); ok {
		ops.wireSent = observer.OnWireSent
	}
	if observer, ok := h.(ReceiptObserver); ok {
		ops.sent = observer.OnSent
	}
	return ops
}

// layerNode is one layer of one connection: the state the layer works
// on, and the Conn the layer above reads and writes through.
type layerNode struct {
	stack *stack
	index int
	lower Conn
	conn  LayerHandler // the layer's instance on this connection
	up    LayerUp      // the face the layer writes up, cached to allocate nothing

	ops    layerOps
	opened atomic.Bool // the layer's OnOpen ran; set on a turn
	ended  bool        // the layer ended its stream up; only turns touch it
	// gate packs the two facts a write is admitted by: openUp, the layer
	// opened the layers above it, and writeShut, its writing half ended. One
	// load on the write path admits or refuses.
	gate    atomic.Uint32
	closing atomic.Bool // this layer's close walk began; see CloseWith
	// shutPending marks a CloseWrite end not yet passed below, shutFlushed
	// its flush made, and writers counts the writes in flight into the layer —
	// the Handler's through the top, a layer's own through every layer it
	// writes below — which the end waits for; see shutDown.
	shutPending atomic.Bool
	shutFlushed atomic.Bool
	writers     atomic.Int32
	stalled     atomic.Bool // the layer stopped short; see LayerUp.Yield
	paced       atomic.Bool // a full delivery kept the layer from running; the read above wakes it

	// delivered is what the layer handed up that the layer above has not
	// read, from at on; only the connection's turns touch it.
	delivered []byte
	at        int
}

// The two facts a write into a layer is admitted by; see layerNode.gate.
const (
	gateOpenUp    uint32 = 1 << iota // the layer opened the layers above it
	gateWriteShut                    // the layer above ended its writing half
)

func (l *layerNode) openUp() bool { return l.gate.Load()&gateOpenUp != 0 }

func (l *layerNode) markOpenUp() { l.gate.Or(gateOpenUp) }

// markWriteShut ends the layer's writing half, reporting whether this call
// ended it: a second CloseWrite does nothing.
func (l *layerNode) markWriteShut() bool {
	for {
		gate := l.gate.Load()
		if gate&gateWriteShut != 0 {
			return false
		}
		if l.gate.CompareAndSwap(gate, gate|gateWriteShut) {
			return true
		}
	}
}

// layerCtx is the LayerContext a layer holds: the connection below as it
// reads it, the layer above as it writes to it, and the close verbs that
// begin the stack's close from here.
type layerCtx struct{ l *layerNode }

var (
	_ LayerContext = layerCtx{}
	_ LayerUp      = layerUp{}
	_ Conn         = (*layerNode)(nil)
	_ WireCounter  = (*layerNode)(nil)
)

func (lc layerCtx) Down() LayerDown { return lc.l.lower }

// Up returns the node's cached face, so writing up allocates nothing.
func (lc layerCtx) Up() LayerUp { return lc.l.up }

func (lc layerCtx) SetTimer(when time.Time) error {
	return lc.l.stack.setTimer(lc.l.index, when, 0)
}

func (lc layerCtx) Close(cause error) error { return lc.l.endClose(cause, true, false) }

func (lc layerCtx) Abort(cause error) error { return lc.l.endClose(cause, false, false) }

func (lc layerCtx) Wake() error { return lc.l.stack.socket.Wake() }

// layerUp is the layer above's input, as the layer below writes to it.
type layerUp struct{ l *layerNode }

func (u layerUp) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	l := u.l
	l.compact()
	l.delivered = append(l.delivered, p...)
	return len(p), nil
}

func (u layerUp) End() { u.l.ended = true }

// Open records that the layers above may open: their OnOpen runs on this
// turn once this callback returned — never inside it — so this layer's lock,
// if it holds one, stays its own.
func (u layerUp) Open() {
	u.l.markOpenUp()
}

func (u layerUp) Reserve(n int) []byte {
	l := u.l
	l.compact()
	if spare := cap(l.delivered) - len(l.delivered); spare < n {
		// Grow by doubling, as append does: a layer that commits a little at
		// a time copies what it holds a logarithmic number of times.
		grown := make([]byte, len(l.delivered), max(len(l.delivered)+n, 2*cap(l.delivered)))
		copy(grown, l.delivered)
		l.delivered = grown
	}
	return l.delivered[len(l.delivered):cap(l.delivered)]
}

func (u layerUp) Commit(n int) {
	l := u.l
	l.delivered = l.delivered[:len(l.delivered)+n]
}

func (u layerUp) Buffered() int { return len(u.l.delivered) - u.l.at }

func (u layerUp) Yield() { u.l.stalled.Store(true) }

// waitingBelow is what the layer can still read from the one below: the
// socket's buffered bytes for the first layer, the delivered bytes of the
// layer below for the others.
func (l *layerNode) waitingBelow() int {
	if below, ok := l.lower.(*layerNode); ok {
		return below.held()
	}
	return l.lower.InboundBuffered()
}

// maxRetainedDelivery is the most capacity a layer keeps for its next
// delivery once everything delivered was read; a larger buffer, grown for a
// burst, goes back to the heap.
const maxRetainedDelivery = 256 << 10

// reclaim drops a fully read delivery, so the next one reuses its capacity,
// or frees it when a burst grew it large.
func (l *layerNode) reclaim() {
	if l.at > 0 && l.at == len(l.delivered) {
		if cap(l.delivered) > maxRetainedDelivery {
			l.delivered, l.at = nil, 0
			return
		}
		l.delivered, l.at = l.delivered[:0], 0
	}
}

// compact moves what the layer above has not read to the front before the
// layer delivers more, so a reader that always leaves a little unread — a
// partial header, the start of a message — does not keep everything read
// before it alive. It runs as the layer delivers, on its OnRead or
// OnTimer, when no slice the layer above was lent is in use any more.
func (l *layerNode) compact() {
	if l.at == 0 {
		return
	}
	n := copy(l.delivered, l.delivered[l.at:])
	l.delivered, l.at = l.delivered[:n], 0
	if n == 0 && cap(l.delivered) > maxRetainedDelivery {
		l.delivered = nil
	}
}

// socketLower is the first layer's Lower: the socket, with the
// connection's userdata — the socket's own slot holds the stack — and the
// first layer's timer in place of the socket's single wake slot.
type socketLower struct {
	Conn
	stack *stack
}

func (s socketLower) Userdata() any {
	if held := s.stack.userdata.Load(); held != nil {
		return *held
	}
	return nil
}

func (s socketLower) SetUserdata(value any) { s.stack.userdata.Store(&value) }

func (s socketLower) CloseWith(cause error) error { return s.stack.closeSocket(cause) }

// Close and CloseGracefully follow the same road as CloseWith, so the first
// layer cannot end the stack under itself: the close a layer asks for runs
// the stack's goodbyes and waits, not the socket's own one-shot flush.
func (s socketLower) Close() error { return s.stack.closeSocket(io.ErrUnexpectedEOF) }

func (s socketLower) CloseGracefully(cause error) error { return s.stack.closeSocket(cause) }

// The writes that hand the socket bytes count them; see WireCounter.

func (s socketLower) Write(p []byte) (int, error) {
	n, err := s.Conn.Write(p)
	s.stack.wireQueued.Add(uint64(n))
	return n, err
}

func (s socketLower) WriteString(p string) (int, error) {
	n, err := s.Conn.WriteString(p)
	s.stack.wireQueued.Add(uint64(n))
	return n, err
}

func (s socketLower) WriteByte(b byte) error {
	err := s.Conn.WriteByte(b)
	if err == nil {
		s.stack.wireQueued.Add(1)
	}
	return err
}

func (s socketLower) Writev(vec [][]byte) (int, error) {
	n, err := s.Conn.Writev(vec)
	s.stack.wireQueued.Add(uint64(n))
	return n, err
}

func (s socketLower) WriteOwned(buffer *Buffer) (int, error) {
	n, err := s.Conn.WriteOwned(buffer)
	s.stack.wireQueued.Add(uint64(n))
	return n, err
}

func (s socketLower) ReserveOutbound(n int) ([]byte, error) {
	room, err := s.Conn.ReserveOutbound(n)
	if err == nil {
		s.stack.wireQueued.Add(uint64(n))
	}
	return room, err
}

var _ TrackedWriter = socketLower{}

// WriteTracked writes p to the socket and takes its wire position; the first
// layer hears the receipt once it leaves. See TrackedWriter.
func (s socketLower) WriteTracked(p []byte) (int, SendReceipt, error) {
	n, err := s.Write(p)
	if err != nil || n == 0 {
		return n, SendReceipt{}, err
	}
	r := SendReceipt{wire: s.stack.wireQueued.Load()}
	if r.wire == 0 || len(s.stack.layers) == 0 {
		return n, SendReceipt{}, nil
	}
	s.stack.addReceipt(pendingReceipt{conn: s, above: s.stack.layers[0], r: r})
	return n, r, nil
}

func (s socketLower) SetWakeDeadline(when time.Time) error { return s.stack.setTimer(0, when, 0) }

func (s socketLower) SetWakeInterval(interval time.Duration) error {
	if interval <= 0 {
		return s.stack.setTimer(0, time.Time{}, 0)
	}
	return s.stack.setTimer(0, time.Now().Add(interval), interval)
}

// The Conn the layer above holds. Its reads are what the layer delivered;
// its writes enter the layer; the rest is the connection's own.

func (l *layerNode) LocalAddr() net.Addr  { return l.lower.LocalAddr() }
func (l *layerNode) RemoteAddr() net.Addr { return l.lower.RemoteAddr() }

func (l *layerNode) Userdata() any {
	if held := l.stack.userdata.Load(); held != nil {
		return *held
	}
	return nil
}

func (l *layerNode) SetUserdata(value any) { l.stack.userdata.Store(&value) }

func (l *layerNode) SetLinger(secs int) error           { return l.lower.SetLinger(secs) }
func (l *layerNode) SetNoDelay(noDelay bool) error      { return l.lower.SetNoDelay(noDelay) }
func (l *layerNode) SetKeepAlive(keepAlive bool) error  { return l.lower.SetKeepAlive(keepAlive) }
func (l *layerNode) SetKeepAlivePeriod(secs int) error  { return l.lower.SetKeepAlivePeriod(secs) }
func (l *layerNode) Control(fn func(fd uintptr)) error  { return l.lower.Control(fn) }
func (l *layerNode) SetReadBuffer(size int) error       { return l.lower.SetReadBuffer(size) }
func (l *layerNode) SetWriteBuffer(size int) error      { return l.lower.SetWriteBuffer(size) }
func (l *layerNode) SetDeadline(t time.Time) error      { return l.lower.SetDeadline(t) }
func (l *layerNode) SetReadDeadline(t time.Time) error  { return l.lower.SetReadDeadline(t) }
func (l *layerNode) SetWriteDeadline(t time.Time) error { return l.lower.SetWriteDeadline(t) }

func (l *layerNode) Wake() error                          { return l.stack.socket.Wake() }
func (l *layerNode) YieldRead() error                     { return l.lower.YieldRead() }
func (l *layerNode) InboundBuffered() int                 { return l.held() }
func (l *layerNode) ReserveOutbound(int) ([]byte, error)  { return nil, ErrReserveUnsupported }
func (l *layerNode) Close() error                         { return l.CloseWith(io.ErrUnexpectedEOF) }
func (l *layerNode) held() int                            { return len(l.delivered) - l.at }
func (l *layerNode) slot() int                            { return l.index + 1 }
func (l *layerNode) Peek(b []byte) []byte                 { return b[:copy(b, l.delivered[l.at:])] }
func (l *layerNode) PeekChunk() []byte                    { return l.delivered[l.at:] }
func (l *layerNode) WriteByte(b byte) error               { _, err := l.Write([]byte{b}); return err }
func (l *layerNode) WriteString(s string) (int, error)    { return l.Write([]byte(s)) }
func (l *layerNode) SetWakeDeadline(when time.Time) error { return l.stack.setTimer(l.slot(), when, 0) }

// WireQueued is what the socket was handed, with what this layer and the
// ones below it hold — not the layers above, whose bytes have not reached
// this Conn. The layers' part is read first, from the layers themselves: a
// layer that hands bytes down between the two readings is counted twice,
// never missed.
func (l *layerNode) WireQueued() uint64 {
	held := 0
	if l.stack.holders && !l.stack.closedStack() {
		for _, below := range l.stack.layers[:l.index+1] {
			if below.ops.held != nil {
				held += below.ops.held(layerCtx{below})
			}
		}
	}
	return uint64(held) + l.stack.wireQueued.Load()
}

// WireSent is what the socket reported sent.
func (l *layerNode) WireSent() uint64 { return l.stack.wireSent.Load() }

// State reports where the connection stands; see ConnState.
func (l *layerNode) State() ConnState {
	var state ConnState
	if l.ended && l.held() == 0 {
		state |= StatePeerEnded
	}
	if l.gate.Load()&gateWriteShut != 0 {
		state |= StateWriteShut
	}
	switch l.stack.state.Load() & stackPhaseMask {
	case stackClosing:
		state |= StateClosing
	case stackClosed:
		state |= StateClosing | StateClosed
	}
	return state
}

// pendingReceipt is one tracked write the wire has not passed yet.
type pendingReceipt struct {
	conn  Conn       // the connection the write entered, for its queue
	above *layerNode // the layer above it, to hear; nil past the top
	r     SendReceipt
}

var _ TrackedWriter = (*layerNode)(nil)

// WriteTracked writes p into the layer and takes the position its output
// holds on the wire once the write returned; see TrackedWriter. The layer
// above this connection hears the receipt on a turn once it leaves.
func (l *layerNode) WriteTracked(p []byte) (int, SendReceipt, error) {
	n, err := l.Write(p)
	if err != nil || n == 0 {
		return n, SendReceipt{}, err
	}
	r := SendReceipt{wire: l.WireQueued()}
	if r.wire == 0 {
		return n, SendReceipt{}, nil
	}
	c := l.stack
	var above *layerNode
	if l.index+1 < len(c.layers) {
		above = c.layers[l.index+1]
	}
	c.addReceipt(pendingReceipt{conn: l, above: above, r: r})
	return n, r, nil
}

// addReceipt queues a tracked write, for the turn that sees the wire pass it.
func (c *stack) addReceipt(pr pendingReceipt) {
	c.receiptCount.Add(1)
	c.receiptMu.Lock()
	c.receipts = append(c.receipts, pr)
	c.receiptMu.Unlock()
	c.requestTurn()
}

// retireReceipts reports the tracked writes the wire passed, and keeps the
// rest. It runs as a turn ends: a receipt may not fire from inside the write
// that moved the wire — the layer holding its lock must not be called back.
func (c *stack) retireReceipts() {
	if c.receiptCount.Load() == 0 {
		return
	}
	c.receiptMu.Lock()
	out := c.receipts
	c.receipts = nil
	c.receiptMu.Unlock()
	sent := c.wireSent.Load()
	var keep []pendingReceipt
	for _, pr := range out {
		if c.over() {
			// A receipt reported began the close: the rest wait for OnClose's
			// notice, as any receipt outstanding at a close does.
			pr.r = SendReceipt{}
		}
		if pr.r.wire == 0 {
			c.receiptCount.Add(-1)
			continue
		}
		if pr.r.done(sent) || pr.conn.OutboundBuffered() == 0 {
			c.receiptCount.Add(-1)
			if pr.above != nil && pr.above.ops.sent != nil {
				held := pr.above.held()
				pr.above.ops.sent(layerCtx{pr.above}, pr.r)
				if pr.above.held() != held {
					// A layer that wrote up in OnSent gets a turn to carry it,
					// as one does in OnWireSent.
					c.upPending.Store(true)
				}
			}
			continue
		}
		keep = append(keep, pr)
	}
	if len(keep) > 0 {
		c.receiptMu.Lock()
		c.receipts = append(keep, c.receipts...) // kept writes entered first
		c.receiptMu.Unlock()
	}
}

// dropReceipts forgets the tracked writes the connection ended under: the
// layers hear OnClose instead.
func (c *stack) dropReceipts() {
	if c.receiptCount.Load() == 0 {
		return
	}
	c.receiptMu.Lock()
	c.receipts = nil
	c.receiptMu.Unlock()
	c.receiptCount.Store(0)
}

// SetWakeInterval arms the layer above's timer to repeat; see Conn.
func (l *layerNode) SetWakeInterval(interval time.Duration) error {
	if interval <= 0 {
		return l.stack.setTimer(l.slot(), time.Time{}, 0)
	}
	return l.stack.setTimer(l.slot(), time.Now().Add(interval), interval)
}

// ReserveInbound succeeds without copying: what a layer delivers is one
// chunk already. The limit is the connection's, as the socket's is.
func (l *layerNode) ReserveInbound(n int) error {
	if limit := l.stack.inLimit; limit > 0 && n > limit {
		return ErrInboundOverflow
	}
	return nil
}

// Discard drops the next n bytes delivered, all of them when n is negative
// or past what is held, as the socket's does.
func (l *layerNode) Discard(n int) (int, error) {
	if held := l.held(); n < 0 || n > held {
		n = held
	}
	l.at += n
	l.reclaim()
	l.roomMade()
	return n, nil
}

// Read is non-blocking, as the socket's is, and reports io.EOF once the
// layer ended its stream and everything it delivered was read.
func (l *layerNode) Read(b []byte) (int, error) {
	n := copy(b, l.delivered[l.at:])
	l.at += n
	l.reclaim()
	l.roomMade()
	if n == 0 && l.ended && l.held() == 0 {
		return 0, io.EOF
	}
	return n, nil
}

// OutboundBuffered is what waits below the layer above: what this layer
// holds, if it holds any, and what the layers below and the socket queue.
func (l *layerNode) OutboundBuffered() int {
	queued := l.lower.OutboundBuffered()
	if l.ops.held != nil && !l.stack.closedStack() {
		queued += l.ops.held(layerCtx{l})
	}
	return queued
}

// Flush sends what this layer holds, if it holds any, then passes below.
func (l *layerNode) Flush() error {
	if l.stack.over() {
		return net.ErrClosed
	}
	var err error
	if l.ops.flush != nil {
		err = l.ops.flush(layerCtx{l})
	}
	if err == nil && !l.stack.over() {
		err = l.lower.Flush()
	}
	// What the flush sent may be what an end waits for: a turn looks, since
	// this flush may run inside a layer above's.
	if c := l.stack; c.shutsPending.Load() > 0 {
		c.requestTurn()
	}
	return err
}

func (l *layerNode) WriteTo(w io.Writer) (int64, error) {
	n, err := w.Write(l.delivered[l.at:])
	l.at += n
	l.reclaim()
	l.roomMade()
	return int64(n), err
}

// roomMade runs another turn after the layer above read, when the layer
// stalled short of what it could deliver or the stack stopped pulling at its
// inbound limit.
func (l *layerNode) roomMade() {
	// Loads first: every read runs this, and a compare-and-swap costs a
	// locked instruction even when it fails.
	if l.stalled.Load() && l.stalled.CompareAndSwap(true, false) {
		_ = l.stack.socket.Wake()
	}
	if l.paced.Load() && l.paced.Swap(false) {
		// The layer feeding this one was paused on a full delivery; the read
		// that just ran makes room, so a turn calls it again.
		_ = l.stack.socket.Wake()
	}
	if l.stack.inHeld.Load() {
		l.stack.inboundRoom()
	}
}

// Write enters the layer, once it opened the layers above; the Handler's
// writes are admitted against MaxOutboundBuffered first.
func (l *layerNode) Write(p []byte) (int, error) {
	if err := l.enter(len(p)); err != nil {
		return 0, err
	}
	n, err := l.conn.OnWrite(layerCtx{l}, p)
	l.leave(len(p))
	if err == nil && n < len(p) {
		// A layer took part of the write: io.Writer reports that as a short
		// write, never as a complete one.
		err = io.ErrShortWrite
	}
	return n, err
}

func (l *layerNode) Writev(vec [][]byte) (int, error) {
	size := 0
	for _, segment := range vec {
		size += len(segment)
	}
	if err := l.enter(size); err != nil {
		return 0, err
	}
	var n int
	var err error
	if len(vec) == 1 {
		n, err = l.conn.OnWrite(layerCtx{l}, vec[0])
	} else {
		// One write whole, as the socket's Writev is: segments passed one by
		// one would interleave with another writer's.
		joined := AcquireBuffer(size)
		for _, segment := range vec {
			_, _ = joined.Write(segment)
		}
		n, err = l.conn.OnWrite(layerCtx{l}, joined.Bytes())
		ReleaseBuffer(joined)
	}
	l.leave(size)
	if err == nil && n < size {
		err = io.ErrShortWrite
	}
	return n, err
}

// enter checks that a write may enter the layer — it opened the layers above
// and is not closing — and, at the top of the stack, reserves its room; leave
// returns the room once the write is through.
//
// The write is counted at every layer, not only the top: a layer writing
// below from its OnRead passes through the layer below it, and a CloseWrite
// reaching that layer must not pass under a write still in flight — the top's
// count covers the Handler's writes only.
func (l *layerNode) enter(n int) error {
	if l.refuses() {
		return net.ErrClosed // refused before it counts: it asks for no turn
	}
	// Counted, then checked again, so a CloseWrite that has not seen this
	// write leaves its end to the write's leave.
	l.writers.Add(1)
	if l.refuses() {
		l.exit()
		return net.ErrClosed
	}
	if l.isTop() {
		if err := l.stack.reserve(n); err != nil {
			l.exit()
			return err
		}
	}
	return nil
}

// refuses reports a layer writes no longer reach: not open above yet, its
// writing half ended, or the stack closing — where only the goodbyes below
// the close's initiator still write down. The stack reads one state word, so
// a write racing the close is refused at once, not when the walk reaches its
// layer.
func (l *layerNode) refuses() bool {
	// One load: either the layer opened the layers above it and its writing
	// half is open, or the write is refused.
	return l.gate.Load() != gateOpenUp || !l.stack.mayWriteDown(l.index)
}

func (l *layerNode) leave(n int) {
	if l.isTop() {
		l.stack.release(n)
	}
	l.exit()
}

// exit ends a write into the layer; the last one out asks a turn to pass on
// an end that waited for it.
func (l *layerNode) exit() {
	if l.writers.Add(-1) == 0 && l.shutPending.Load() {
		l.stack.requestTurn()
	}
}

func (l *layerNode) isTop() bool { return l.index == len(l.stack.layers)-1 }

// WriteOwned keeps the Conn contract: a write refused whole with
// ErrOutboundOverflow leaves the buffer the caller's; any other outcome
// consumes it, and a partial one reports io.ErrShortWrite.
func (l *layerNode) WriteOwned(buffer *Buffer) (int, error) {
	n, err := l.Write(buffer.Bytes())
	if err == nil {
		n = buffer.Len()
	} else if errors.Is(err, ErrOutboundOverflow) {
		if n == 0 {
			return 0, err
		}
		err = io.ErrShortWrite
	}
	ReleaseBuffer(buffer)
	return n, err
}

// CloseWith closes the connection from above: the layer says its
// goodbye, if it has one, and the close passes down.
//
// A graceful close flushes the layer once it said its goodbye, so what it
// holds goes below ahead of the layers beneath saying theirs: the close
// passes down flushing each layer it passes, never one above the layer that
// began it, which may be inside a callback of its own.
func (l *layerNode) CloseWith(err error) error {
	return l.endClose(err, gracefulCause(err), true)
}

// endClose closes the connection from above, or from this layer through
// LayerContext.Close and Abort; graceful chooses the mode — the goodbyes are
// said either way, and a graceful close also flushes each layer and drains
// the socket's queue. fromAbove tells a close the layers above asked for
// (walked here) from one this layer began in its own callback (walked on the
// turn): that layer is not flushed on the way — it may hold a lock its flush
// would take — and its goodbye runs on the turn too.
func (l *layerNode) endClose(cause error, graceful, fromAbove bool) error {
	c := l.stack
	if c.closedStack() || !l.closing.CompareAndSwap(false, true) {
		return net.ErrClosed
	}
	c.recordCloseCause(cause)
	if !c.beginClosing(l.index) {
		// Closing already — a layer below closed it: no layer is called any
		// more, and the socket hears this cause, an abort cutting a
		// graceful close's wait short.
		return c.endAtSocket(cause, false, graceful)
	}
	if !fromAbove {
		// The stack is closing now — writes are refused at once — but the
		// walk runs on the turn: the caller is inside its own callback (or
		// the writer's), and UIO never calls a layer back there.
		c.closePlan.Store(&closePlan{cause: cause, graceful: graceful, from: int32(l.index)})
		c.requestTurn()
		return nil
	}
	// A close from above began past this layer, so it flushes; the walk runs
	// here, where no layer callback is under it.
	done := l.closeWalk(cause, graceful, true)
	if errors.Is(done, net.ErrClosed) {
		// The socket closed under this close, which began the stack's: the
		// peer answered the goodbye it sent before the socket's own close
		// came. The connection closed as asked.
		return nil
	}
	return done
}

// runClosePlan walks a close a layer began for itself, on the turn, once.
func (c *stack) runClosePlan() {
	plan := c.closePlan.Swap(nil)
	if plan == nil || c.closedStack() {
		return
	}
	c.recordCloseCause(plan.cause)
	switch {
	case plan.from == stackNoWriters:
		_ = c.endAtSocket(plan.cause, c.beginClosing(stackNoWriters), plan.graceful)
	case int(plan.from) < len(c.layers):
		// The layer's own goodbye says first; it is not flushed — it may
		// hold a lock its flush would take — it sends what it holds itself.
		layer := c.layers[plan.from]
		_ = layer.closeWalk(plan.cause, plan.graceful, false)
	default:
		// Above the top: every layer says its goodbye, then the socket.
		if len(c.layers) == 0 {
			_ = c.endAtSocket(plan.cause, c.beginClosing(stackNoWriters), plan.graceful)
			return
		}
		top := c.layers[len(c.layers)-1]
		top.closing.Store(true)
		_ = top.closeWalk(plan.cause, plan.graceful, true)
	}
}

// closeWalk says this layer's goodbye and passes the close to the layer
// below, one layer at a time, until it reaches the socket. The stack is
// closing throughout — no turn calls a layer — and the walk is what still
// writes down: its goodbyes enter the layers below the initiator, and
// nothing above them.
func (l *layerNode) closeWalk(cause error, graceful, flushSelf bool) error {
	c := l.stack
	if l.ops.graceful != nil && l.openUp() {
		l.ops.graceful(layerCtx{l}, cause)
	}
	if flushSelf && graceful && l.ops.flush != nil && l.opened.Load() && !c.closedStack() {
		_ = l.ops.flush(layerCtx{l})
	}
	below, ok := l.lower.(*layerNode)
	if !ok {
		if at, ok := l.lower.(socketLower); ok {
			return at.socketClose(cause, graceful) // the socket, in the walk's mode
		}
		return l.lower.CloseWith(cause)
	}
	if !below.closing.CompareAndSwap(false, true) {
		return net.ErrClosed // a close from below owns that layer
	}
	return below.closeWalk(cause, graceful, true)
}

// CloseGracefully closes the connection gracefully whatever its cause: the
// goodbyes are said and the close waits for what was queued.
func (l *layerNode) CloseGracefully(cause error) error {
	return l.endClose(cause, true, true)
}

// CloseWrite ends the layer above's writing half, through the layer's
// protocol, after what was written before it: writes into the layer are
// refused from here on, what the layer holds is flushed first — a flush's
// error is CloseWrite's — and what it holds still, a send window's worth,
// goes below before the end does. A layer with no way to end one direction
// does not support it. Calling it again does nothing.
func (l *layerNode) CloseWrite() error {
	if l.ops.closeWriter == nil {
		return ErrUnsupported
	}
	if l.stack.over() {
		return net.ErrClosed
	}
	if !l.markWriteShut() {
		return nil
	}
	l.stack.shutsPending.Add(1)
	l.shutPending.Store(true)
	_, err := l.shutDown()
	return err
}

// shutDown passes the end CloseWrite asked of l below once what was written
// before it is: no write in flight into the layer, its output flushed, and
// nothing held. It reports false while something is, leaving the end to a
// turn's end, which the last write out or a flush asks for. One caller at a
// time claims the end.
func (l *layerNode) shutDown() (bool, error) {
	for l.writers.Load() == 0 && l.shutPending.CompareAndSwap(true, false) {
		if l.stack.over() {
			l.stack.shutsPending.Add(-1)
			return true, net.ErrClosed
		}
		ctx := layerCtx{l}
		if l.ops.flush != nil && !l.shutFlushed.Swap(true) {
			if err := l.ops.flush(ctx); err != nil {
				l.stack.shutsPending.Add(-1)
				return true, err
			}
			if l.stack.over() {
				// The flush closed the connection: the end has nothing left
				// to end, and the layer is not called again.
				l.stack.shutsPending.Add(-1)
				return true, net.ErrClosed
			}
		}
		if l.ops.held != nil && l.ops.held(ctx) > 0 {
			// Held bytes the layer cannot send yet: published again
			// before the look that may see them gone, so a report of
			// their leaving finds the end either way.
			l.shutPending.Store(true)
			if l.ops.held(ctx) > 0 {
				return false, nil
			}
			continue
		}
		l.stack.shutsPending.Add(-1)
		return true, l.ops.closeWriter(ctx)
	}
	return false, nil
}

// finishShuts passes down the ends CloseWrite left waiting whose layers no
// longer hold anything back. It runs as a turn ends; an error closes the
// connection: nobody is left to return it to.
func (c *stack) finishShuts() {
	if c.shutsPending.Load() == 0 || c.closedStack() {
		return
	}
	for i := len(c.layers) - 1; i >= 0; i-- {
		l := c.layers[i]
		if !l.shutPending.Load() || c.over() {
			continue
		}
		if done, err := l.shutDown(); done && err != nil && !errors.Is(err, net.ErrClosed) {
			// Through closeSocket, so the stack is closing from here on and
			// no layer is called again while the socket's close comes up.
			_ = c.closeSocket(err)
		}
	}
}

// LayerOf returns the LayerHandler of the layer that built conn
// — the connection a layer hands up — so the layer above can ask it what it
// negotiated, a TLS layer's connection state:
//
//	if tls, ok := uio.LayerOf(conn).(interface{ State() tls.ConnectionState }); ok { … }
//
// It returns nil for a connection no layer built. FindLayer looks further
// down.
func LayerOf(conn LayerDown) LayerHandler {
	if l, ok := conn.(*layerNode); ok {
		return l.conn
	}
	return nil
}

// FindLayer returns the first LayerHandler of type T below conn, from the layer
// that built conn down to the socket — the TLS state under a WebSocket layer:
//
//	tls, ok := uio.FindLayer[interface{ State() tls.ConnectionState }](conn)
func FindLayer[T any](conn LayerDown) (T, bool) {
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
// unixpacket — on an Events with layers installed: a layer carries a byte
// stream, and a packet connection has message boundaries no layer would
// keep.
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

// refuseLayerPacket refuses a packet network on an endpoint that carries a
// chain.
func refuseLayerPacket(network string, layered bool) error {
	if layered && packetNetwork(network) {
		return ErrLayersNeedStream
	}
	return nil
}

// observed is the connection an Observer hears about: the socket counts the
// bytes, and an Observer is reported the connection the Handler holds — the
// top layer's, once layers run — so it reads the same Userdata, and the
// same identity, the Handler's callbacks see.
func (ev *Events) observed(socket Conn) Conn {
	if c, ok := socket.Userdata().(*stack); ok {
		return c.topConn()
	}
	return socket
}

// dialMarker replaces the dial's userdata with a probe when the connection's
// entry carries a chain, so the stack can report the dial once it opened. The
// second return is the userdata the dialed connection starts with.
func (ev *Events) dialMarker(userdata any, layered bool) (*dialProbe, any) {
	if !layered {
		return nil, userdata
	}
	probe := &dialProbe{user: userdata, done: make(chan struct{})}
	return probe, probe
}

// dialProbe is the marker a dial carries until its stack opens: the stack
// takes the dial's userdata from it, and settles or fails it — or the dial
// gives up on it first. Whichever claims it first decides.
type dialProbe struct {
	user    any
	claimed atomic.Bool
	done    chan struct{} // closed once, with conn or err in place
	conn    Conn
	err     error
}

// settle reports the opened stack, unless the dial gave up already.
func (p *dialProbe) settle(conn Conn) bool {
	if !p.claimed.CompareAndSwap(false, true) {
		return false
	}
	p.conn = conn
	close(p.done)
	return true
}

func (p *dialProbe) fail(err error) {
	if p.claimed.CompareAndSwap(false, true) {
		p.err = err
		close(p.done)
	}
}

// awaitDial waits for a dial's stack to open and reports what it became.
// raw is the socket, for a cancellation to close.
func (ev *Events) awaitDial(ctx context.Context, raw Conn, probe *dialProbe) (Conn, error) {
	select {
	case <-probe.done:
		if probe.err != nil {
			return nil, probe.err
		}
		return probe.conn, nil
	case <-ctx.Done():
		cause := context.Cause(ctx)
		if cause == nil {
			cause = ctx.Err()
		}
		if !probe.claimed.CompareAndSwap(false, true) {
			<-probe.done // settled or failed as the deadline arrived
			if probe.err != nil {
				return nil, probe.err
			}
			return probe.conn, nil
		}
		_ = raw.CloseWith(cause)
		return nil, cause
	}
}
