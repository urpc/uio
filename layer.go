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
// callbacks, its addressing and options, its closes. [Events.Use] stacks
// layers under the [Events.Handler].
//
// A layer carries one connection: one connection above for each one below.
// A protocol that opens several streams over one connection — HTTP/2, QUIC —
// runs as the Handler, over a connection the layer below it made.
//
// Attach runs once per connection and returns that connection's [LayerConn];
// the [LayerContext] it receives is the connection's place in the stack: the
// layer reads what arrived with LayerContext.Lower, hands the layer above
// what that became with LayerContext.Deliver, and writes down with
// LayerContext.Write. The layer above — the next layer or the Handler — gets
// a [Conn] UIO builds over this one: its reads are what this layer
// delivered, its writes reach this layer's OnOutbound, and everything else is
// the connection's own, passed through.
//
// What UIO guarantees every layer, so that none has to remember it:
//
//   - Every turn reaches the layers above, whether or not anything was
//     delivered, so their timers and wakes work through any stack.
//   - Each layer has a timer of its own, [LayerContext.SetTimer], and the
//     Handler's [Conn.SetWakeDeadline] is a slot of its own too: no layer
//     replaces another's.
//   - OnOpen, OnInbound, OnTimer, OnSent and OnClose run on the
//     connection's turns, which never overlap one another or the callbacks
//     of the layers above. The rest run on the goroutine that called for
//     them, and may overlap the turns and one another: OnOutbound on the
//     writer's, CloseGracefully and CloseWrite on the one closing the
//     connection, OutboundHeld on a writer's and Flush on a turn's or the
//     caller of Flush. State they share with the turns needs a lock.
//   - UIO calls a layer only on a turn or from above it — the Handler, or
//     the layer above through its LayerContext — never from below or from
//     inside another layer's callback: a layer may hold its lock while it
//     writes, flushes or closes below, and nothing calls it back meanwhile.
//     Send reports, which can come inside a write, reach the layers on a
//     turn; an end CloseWrite leaves waiting is passed down on one.
//   - Once the connection is closing — any layer, or the Handler, closed
//     it — or ended, UIO starts no call into a layer, checking again after
//     every callback, so a layer that closes from OnSent or Flush is the
//     last called: a write, flush or close that comes after returns
//     net.ErrClosed, and only OnClose follows. A call already under way on
//     another goroutine may still overlap.
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
//     calls LayerContext.End, and the layer above reads io.EOF once it has
//     read the rest.
type Layer interface {
	// Attach builds the layer's state for one connection. The LayerContext
	// stays valid until the connection's OnClose.
	Attach(ctx LayerContext) LayerConn
}

// LayerConn is a layer's instance on one connection: the callbacks UIO runs
// as the connection's bytes and events pass through the layer.
type LayerConn interface {
	// OnOpen runs once the layers below opened: the place to start a
	// handshake. The layers above open when LayerContext.Open is called, here
	// or on a later turn; a layer that negotiates nothing calls it here.
	OnOpen(ctx LayerContext)

	// OnInbound runs on a turn once the layer opened: bytes from below may
	// have arrived, or none. It reads them from LayerContext.Lower, consumes
	// them with Discard, and hands up what they became with
	// LayerContext.Deliver. It may take one chunk at a time: while it consumes
	// and bytes still wait below, the turn calls it again. The bottom layer is
	// not called at all while the stack holds MaxInboundBuffered unread, until
	// a read above makes room; the layers above it go on moving what the stack
	// already holds up.
	OnInbound(ctx LayerContext) error

	// OnOutbound runs for what the layer above writes, on the writer's
	// goroutine and possibly beside other writers: it transforms p, writes
	// the result with LayerContext.Write, and reports how much of p it took.
	// p is the writer's again once it returns; one call carries one write
	// whole, a Writev's segments joined. What it writes below is never
	// refused for the queue's size: the Handler's writes are checked against
	// MaxOutboundBuffered before they enter the stack. A layer that holds
	// output of its own — a send window — may refuse a write with
	// ErrOutboundOverflow, taking none of it. It runs only once the layer
	// opened the layers above.
	OnOutbound(ctx LayerContext, p []byte) (int, error)

	// OnTimer runs on the turn the layer's timer came due, before OnInbound.
	OnTimer(ctx LayerContext) error

	// OnClose runs once, as the connection ends, whichever end closed it.
	OnClose(ctx LayerContext, cause error)
}

// CloseWriter is implemented by a LayerConn that can end one direction of
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

// GracefulCloser is implemented by a LayerConn with a goodbye to send —
// a TLS close_notify, a WebSocket Close — when the layer above closes the
// connection. It runs before the close passes to the layer below, with the
// close's cause: nil, io.EOF or io.ErrUnexpectedEOF for a graceful close —
// Close's cause is io.ErrUnexpectedEOF, as the socket's own Close is — an
// error for an abort.
//
// A graceful close of a layered connection lets what was queued leave first,
// the goodbyes included: the socket closes once the queue — the socket's, and
// the bytes OutboundHolder layers at or below the one that began the close
// hold — is empty, or once it made no progress for LingerTimeout. Nothing
// reaches the layers or the Handler
// meanwhile, and what arrives is discarded. An abort closes at once. A layer
// that closes the connection itself, through its LayerContext, ends it for
// the layers above too: what they hold is dropped, and the close waits only
// for what lies below; the layer flushes its own output before it closes.
type GracefulCloser interface {
	CloseGracefully(ctx LayerContext, cause error)
}

// LingerTimeout bounds how long a graceful close of a layered connection
// waits for its queue to make progress; see GracefulCloser.
const LingerTimeout = 5 * time.Second

// lingerTimeout is LingerTimeout, which tests shorten.
var lingerTimeout = LingerTimeout

// SendObserver is implemented by a LayerConn that follows what the socket
// sends: a protocol whose state moves as its output leaves — a ping whose
// timeout starts once it is on the wire, a close that waits for its frame to
// go. OnSent reports n bytes the socket sent since the last call, counted on
// the wire: below every layer, so a layer under which others add or remove
// bytes cannot match n to its own. What it can rely on is the queue: once
// Lower's OutboundBuffered is zero, everything it wrote left. It runs on the
// connection's turn — the one running as the send finished, or one the
// report asks for — as the turn ends, possibly more than once before that
// turn ends, and may write: the socket may report a
// send inside the write that made it, where no layer can be called.
type SendObserver interface {
	OnSent(ctx LayerContext, n int)
}

// OutboundHolder is implemented by a LayerConn that holds output of its own
// before writing it below — a send window, a compressor's buffer. The bytes
// it holds count in the OutboundBuffered of the connection above it, in the
// MaxOutboundBuffered check, in what a graceful close waits for, and in what
// CloseWrite sends before the end: a byte counts until its write below has
// returned. OutboundHeld runs on writers' goroutines and in send reports,
// beside the turns, so it must not wait on a lock the layer holds while it
// writes below. A holder that sends what it held outside Flush and the turns
// — a send window opening on a goroutine of its own — calls
// LayerContext.Wake once the write returns, so what waits for the bytes to
// leave sees them gone.
type OutboundHolder interface {
	OutboundHeld(ctx LayerContext) int
}

// Flusher is implemented by a LayerConn that holds output it can send now.
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
// whatever WireSent says.
type WireCounter interface {
	WireQueued() uint64
	WireSent() uint64
}

// maxLayers is how many layers one stack takes: their timers, and the
// Handler's, share one bit mask.
const maxLayers = 63

// LayerContext is a layer's handle on its place in one connection's stack.
type LayerContext interface {
	// Lower is the connection below: the socket, for the first layer
	// installed; the one the layer below built, for the others. Its inbound
	// methods — Peek, PeekChunk, Discard, Read — are this layer's to call, in
	// OnInbound and OnTimer; its writes, options and addressing at any time.
	// Its Userdata is the connection's, and its SetWakeDeadline arms this
	// layer's own timer, as SetTimer does.
	Lower() Conn

	// Deliver hands p up to the layer above, after what it has not read
	// yet; it copies p. Call it in OnInbound or OnTimer.
	Deliver(p []byte)

	// Space returns at least n bytes of room past what is delivered, for the
	// layer to fill in place — decrypting straight into it — and Commit
	// delivers the first n of them. Call both in OnInbound or OnTimer.
	Space(n int) []byte
	Commit(n int)

	// HeldAbove is how many delivered bytes the layer above has not read.
	HeldAbove() int

	// End tells the layer above the stream ended: once it read what was
	// delivered, its Read reports io.EOF. A layer calls it when its peer ended
	// its stream — the socket's end, a TLS close_notify. At the top of the
	// stack, an Events without AllowHalfOpen closes the connection with
	// io.EOF once the Handler's OnData has run; with it, the Handler may go on
	// writing and end its own half with CloseWrite. Call it in OnInbound or
	// OnTimer.
	End()

	// Stall tells UIO the layer stopped short of what it could deliver — its
	// bound was reached, with bytes waiting below or in its own buffers — so
	// the next read above runs a turn to take the rest. A layer that delivers
	// everything it is given never needs it.
	Stall()

	// Write sends p down, through the layer below. MaxOutboundBuffered never
	// refuses it: the stack checks the Handler's writes before they reach
	// any layer.
	Write(p []byte) (int, error)

	// Open opens the layers above, once the layer's negotiation settled — at
	// once from OnOpen if it has none. A dial returns once the top layer
	// opened. Call it on the connection's turn.
	Open()

	// CloseWith closes the connection with cause, through the layer below.
	CloseWith(cause error) error

	// SetTimer arms this layer's own timer for when; a zero time stops it.
	// Lower's SetWakeDeadline and SetWakeInterval arm the same timer.
	SetTimer(when time.Time) error

	// Wake runs a turn of the connection.
	Wake() error
}

// Use stacks layers under the Handler, the first installed closest to the
// wire, so what arrives passes them in install order on its way up and what
// the Handler writes passes them in reverse on its way down:
//
//	events.Use(kcp, tls).Use(trace)
//
// reads bottom up — kcp next to the wire, tls over it, trace just under the
// Handler. Use before Serve: Serve reads the layers once, with the Handler,
// and a Use after it does nothing. It appends, and returns the Events, so
// calls chain. An empty Use does nothing.
//
// Every connection the Events accepts, adopts and dials runs the layers.
// DialContext returns only once the whole stack opened, with the top layer's
// connection; one that fails before that reports its error from
// DialContext, and the Handler hears nothing of the connection.
func (ev *Events) Use(layers ...Layer) *Events {
	ev.mux.Lock()
	defer ev.mux.Unlock()
	if ev.ready.Load() {
		return ev // Serve read the layers already
	}
	ev.use = append(ev.use, layers...)
	return ev
}

// LayerFunc is a layer written as the function its Attach runs.
type LayerFunc func(ctx LayerContext) LayerConn

// Attach calls f.
func (f LayerFunc) Attach(ctx LayerContext) LayerConn { return f(ctx) }

// PassThrough is a LayerConn that changes nothing: it opens at once,
// delivers what arrives, passes the end of the stream up, writes what is
// written and passes CloseWrite down. Embed it in a layer's LayerConn to
// write only what that layer transforms.
type PassThrough struct{}

var _ CloseWriter = PassThrough{}

// OnOpen opens the layers above at once.
func (PassThrough) OnOpen(ctx LayerContext) { ctx.Open() }

// OnInbound delivers what arrived as it is, and the end of the stream after
// it.
func (PassThrough) OnInbound(ctx LayerContext) error {
	lower := ctx.Lower()
	if chunk := lower.PeekChunk(); len(chunk) > 0 {
		ctx.Deliver(chunk)
		if _, err := lower.Discard(len(chunk)); err != nil {
			return err
		}
	}
	if lowerEnded(lower) {
		ctx.End()
	}
	return nil
}

// lowerEnded reports a stream below that ended and was read to its end, as
// lower.Read(nil) reporting io.EOF does — for a layer below, without the
// call's bookkeeping, which a read of nothing never needs.
func lowerEnded(lower Conn) bool {
	if below, ok := lower.(*layerNode); ok {
		return below.ended && below.held() == 0
	}
	_, err := lower.Read(nil)
	return err == io.EOF
}

// OnOutbound writes what is written as it is.
func (PassThrough) OnOutbound(ctx LayerContext, p []byte) (int, error) { return ctx.Write(p) }

// CloseWrite passes CloseWrite to the layer below.
func (PassThrough) CloseWrite(ctx LayerContext) error { return ctx.Lower().CloseWrite() }

// OnTimer does nothing.
func (PassThrough) OnTimer(LayerContext) error { return nil }

// OnClose does nothing.
func (PassThrough) OnClose(LayerContext, error) {}

// stackHandler puts the installed layers between the socket's callbacks and
// handler. It runs under ev.mux, in Serve's initialization.
func (ev *Events) stackHandler(handler Handler) Handler {
	if len(ev.use) == 0 {
		return handler
	}
	if fh, ok := asFuncs(handler); handler == nil || (ok && fh == nil) {
		// No Handler — nil, or a nil *FuncHandler, as resolveCallbacks reads
		// one: no callbacks and the input discarded, as without layers.
		// FuncHandler's zero value does exactly that.
		handler = FuncHandler{}
	}
	return &stackDriver{
		layers:   append([]Layer(nil), ev.use...),
		top:      handler,
		limit:    ev.MaxOutboundBuffered,
		inLimit:  ev.MaxInboundBuffered,
		halfOpen: ev.AllowHalfOpen,
	}
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
		c.probe, held = probe, probe.user
	}
	c.userdata.Store(&held)
	c.layers = make([]*layerNode, len(h.layers))
	var below Conn = socketLower{Conn: socket, stack: c}
	for i := range h.layers {
		c.layers[i] = &layerNode{stack: c, index: i, lower: below}
		below = c.layers[i]
	}
	socket.SetUserdata(c)
	for i, layer := range h.layers {
		c.layers[i].conn = layer.Attach(layerCtx{c.layers[i]})
		if _, ok := c.layers[i].conn.(SendObserver); ok {
			c.sends = append(c.sends, c.layers[i])
		}
		if _, ok := c.layers[i].conn.(OutboundHolder); ok {
			c.holders = true
		}
	}
	for i := len(c.layers) - 1; i >= 0; i-- {
		if _, ok := c.layers[i].conn.(Flusher); ok {
			c.flushers = append(c.flushers, c.layers[i])
		}
	}
	c.inTurn.Store(true)
	defer c.inTurn.Store(false)
	c.openLayer(0)
	if err := c.finishTurn(); err != nil {
		_ = socket.CloseWith(err)
	}
}

func (h *stackDriver) OnData(socket Conn) error {
	if c, ok := socket.Userdata().(*stack); ok {
		return c.turn()
	}
	return nil
}

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
	probe    *dialProbe   // a dial waiting for the stack to open, until it does
	limit    int          // MaxOutboundBuffered; see reserve
	reserved atomic.Int64 // bytes admitted at the top, not yet queued below
	halfOpen bool         // AllowHalfOpen
	inLimit  int          // MaxInboundBuffered; see inboundFull
	sends    []*layerNode // the layers that observe sends, bottom up
	holders  bool         // some layer holds output of its own
	flushers []*layerNode // the layers that hold output to flush, top down

	// inHeld marks a stack that stopped pulling from below because what its
	// layers hold reached inLimit; a read above that makes room wakes it.
	inHeld atomic.Bool

	userdata atomic.Pointer[any] // every layer's Userdata, the Handler's

	topOpen atomic.Bool // the Handler's OnOpen ran; set on a turn
	ended   atomic.Bool // the stack closed

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

	// closing is set as a close reaches the socket from the layers: a turn
	// that is running stops there, and flushes nothing more — a graceful close
	// flushed already, an abort drops what is held.
	closing atomic.Bool

	// wireQueued and wireSent count, on the wire, what the socket was handed
	// and what it reported sent; see WireCounter.
	wireQueued atomic.Uint64
	wireSent   atomic.Uint64

	// lingering holds a graceful close's cause while the socket waits for
	// its queue to empty, lingerTimer bounds the wait, and lingerDone marks
	// the close made.
	// lingerBelow is how many layers, from the bottom, a graceful close
	// waits for: those below the one that began it. A layer that closes the
	// connection itself ends it for the layers above, whose held output is
	// dropped, as an abort drops it.
	lingerBelow atomic.Int32
	lingering   atomic.Pointer[error]
	lingerFor   time.Duration // lingerTimeout as the close began; under lingerMu
	lingerTimer *time.Timer
	lingerMu    sync.Mutex
	lingerDone  atomic.Bool

	// timers holds the timer of every layer, and in its last slot the one
	// the Handler armed with SetWakeDeadline; the socket's own wake slot
	// carries the earliest of them. hasTimers spares a turn the lock while
	// none was ever armed.
	hasTimers atomic.Bool
	timerMu   sync.Mutex
	timers    []timerSlot
	armed     time.Time
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
	if c.ended.Load() {
		return nil
	}
	// Cleared before the turn looks at anything, so a request after this
	// point asks for a turn of its own.
	if c.turnAsked.Load() {
		c.turnAsked.Store(false)
	}
	if c.closing.Load() {
		// Closing — lingering, or aborted with the socket's close still to
		// come: what arrives is no longer anyone's, and kept it would grow
		// for as long as the queue takes to drain.
		_, _ = c.socket.Discard(c.socket.InboundBuffered())
		c.settle()
		return nil
	}
	c.inTurn.Store(true)
	defer c.inTurn.Store(false) // a panic an Executor recovers leaves none set
	err := c.run()
	if flushErr := c.finishTurn(); err == nil {
		err = flushErr
	}
	return err
}

// over reports a stack no layer is called on any more: one that ended, or
// one that is closing. A layer's callback can end it — a close from OnSent,
// a protocol error — so every call into a layer is followed by this check
// before the next.
func (c *stack) over() bool { return c.ended.Load() || c.closing.Load() }

// finishTurn ends a turn: the layers hear what was sent, then flush what
// they hold — what OnSent wrote too — unless the connection is closing, an
// abort dropping it and a graceful close having flushed already. A flush
// can send, and the socket report it right away, inside the flush: the loop
// takes each report on the turn it ended, so what the layers write for it
// is flushed too, and only a report that lands after the last look waits
// for a turn of its own.
func (c *stack) finishTurn() error {
	var err error
	if len(c.sends) == 0 && len(c.flushers) == 0 {
		// Nothing to deliver or flush: only settle's look, and that only
		// when an end or a linger waits for one.
		c.inTurn.Store(false)
		if c.shutsPending.Load() > 0 || c.closing.Load() {
			c.settle()
		}
		return nil
	}
	for !c.over() {
		c.deliverSent()
		if c.over() {
			break // a layer ended the connection: nothing more reaches any
		}
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
// waiting for output to go below, and a lingering close's look at the queue.
// Send reports that came after the turn's own delivery — during its flush —
// get a turn of their own, which delivers them and then flushes what OnSent
// writes. It runs only where no layer is in a callback — the framework never
// calls a layer from inside another's — so a layer holding its own lock
// while it writes or flushes below is never called back. Once the
// connection is closing, nothing reaches the layers: only the linger looks
// at the queue.
func (c *stack) settle() {
	if c.closing.Load() {
		c.sentPending.Store(0)
		if c.lingering.Load() != nil {
			c.finishLinger()
		}
		return
	}
	if c.sentPending.Load() > 0 {
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
		if err := layer.conn.(Flusher).Flush(layerCtx{layer}); err != nil {
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
			if c.closing.Load() {
				return nil
			}
		}
		if err := c.inbound(layer); err != nil {
			return err
		}
		if c.ended.Load() || c.closing.Load() {
			return nil // a layer closed the connection: nothing above runs
		}
	}
	if !c.topOpen.Load() {
		return nil
	}
	top := c.layers[len(c.layers)-1]
	if err := c.top.OnData(top); err != nil {
		return err
	}
	if top.ended && !c.halfOpen {
		return io.EOF // the end of the stream closes, as a socket's does
	}
	return nil
}

// inbound runs a layer's OnInbound until it takes nothing more: the bytes
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
		before := layer.waitingBelow()
		if err := layer.conn.OnInbound(layerCtx{layer}); err != nil {
			return err
		}
		if c.ended.Load() || c.closing.Load() || layer.stalled.Load() {
			return nil
		}
		after := layer.waitingBelow()
		if after == 0 || after >= before {
			return nil
		}
	}
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

// openLayer opens layer i, or the Handler past the top layer. It runs on
// the connection's turn.
func (c *stack) openLayer(i int) {
	if c.ended.Load() || c.closing.Load() {
		return
	}
	if i < len(c.layers) {
		layer := c.layers[i]
		if layer.opened.Load() {
			return
		}
		layer.opened.Store(true)
		c.reaim() // a timer it armed before it opened
		layer.conn.OnOpen(layerCtx{layer})
		return
	}
	if c.topOpen.Load() {
		return
	}
	if probe := c.probe; probe != nil {
		c.probe = nil
		if !probe.settle(c.topConn()) {
			return // the dial gave up and closes the connection
		}
	}
	c.topOpen.Store(true)
	c.top.OnOpen(c.topConn())
}

// closed is the socket's OnClose: the Handler hears it first, once it
// opened, then every layer, top down; a dial still waiting hears the cause.
func (c *stack) closed(err error) {
	if !c.ended.CompareAndSwap(false, true) {
		return
	}
	c.stopTimers()
	c.stopLinger()
	c.sentPending.Store(0)
	if c.topOpen.Load() {
		c.top.OnClose(c.topConn(), err)
	}
	for i := len(c.layers) - 1; i >= 0; i-- {
		layer := c.layers[i]
		layer.conn.OnClose(layerCtx{layer}, err)
	}
	if probe := c.probe; probe != nil {
		c.probe = nil
		probe.fail(err)
	}
}

// topConn is the connection the Handler sees: the top layer's.
func (c *stack) topConn() Conn { return c.layers[len(c.layers)-1] }

// sent passes the socket's report of n bytes sent to the layers that
// observe sends.
func (c *stack) sent(n int) {
	for _, layer := range c.sends {
		if c.over() {
			return // a layer below ended the connection: the rest hear nothing
		}
		layer.conn.(SendObserver).OnSent(layerCtx{layer}, n)
	}
}

// stackSent wraps the Observer's OnOutbound, if any, so a stack's send
// reports reach its layers, and a lingering close learns the queue moved.
//
// A send report can run inside the write that made it — a layer's OnOutbound
// writing below while it holds a lock, crypto/tls its record lock — so it
// calls no layer: it counts the bytes, for WireSent at once and for the
// layers' OnSent on the next turn's end, and asks for that turn. A lingering
// close needs no layer, only the queue's size.
func stackSent(next func(Conn, int)) func(Conn, int) {
	return func(conn Conn, n int) {
		if l, ok := conn.(*layerNode); ok && !l.stack.ended.Load() {
			c := l.stack
			c.wireSent.Add(uint64(n))
			if len(c.sends) > 0 && !c.closing.Load() {
				c.sentPending.Add(int64(n))
				c.requestTurn()
			}
			if c.lingering.Load() != nil {
				c.lingerProgress()
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

// closeSocket is a close reaching the socket from the layers. A graceful one
// waits for the queue to empty — the send reports tell it when — and gives up
// once the queue made no progress for lingerTimeout; it closes with its own
// cause either way. The layers it passed flushed their goodbyes already; see
// layerNode.CloseWith.
func (c *stack) closeSocket(cause error) error {
	wasClosing := c.closing.Swap(true)
	if !gracefulCause(cause) || c.lingerQueued() == 0 {
		err := c.socket.CloseWith(cause)
		if !wasClosing && errors.Is(err, net.ErrClosed) {
			// The socket closed under this close, the first: the peer
			// answered the goodbye it sent before the socket's own close
			// came. The connection closed as asked.
			return nil
		}
		return err
	}
	if !c.lingering.CompareAndSwap(nil, &cause) {
		return net.ErrClosed
	}
	c.lingerMu.Lock()
	if !c.ended.Load() {
		c.lingerFor = lingerTimeout
		c.lingerTimer = time.AfterFunc(c.lingerFor, c.lingerExpired)
	}
	c.lingerMu.Unlock()
	c.finishLinger() // the queue may have emptied before lingering was set
	return nil
}

// lingerProgress is a send report while lingering: the queue moved, so the
// bound starts again, and an empty queue closes.
func (c *stack) lingerProgress() {
	c.lingerMu.Lock()
	if c.lingerTimer != nil {
		c.lingerTimer.Reset(c.lingerFor)
	}
	c.lingerMu.Unlock()
	c.finishLinger()
}

// lingerExpired closes a lingering connection whose queue stopped moving.
func (c *stack) lingerExpired() { c.closeLingering() }

// finishLinger makes a lingering close once the queue is empty.
func (c *stack) finishLinger() {
	if c.lingerQueued() == 0 {
		c.closeLingering()
	}
}

// lingerQueued is what a graceful close waits for: the socket's queue and
// what the layers below the one that began the close hold.
func (c *stack) lingerQueued() int {
	queued := c.socket.OutboundBuffered()
	if !c.holders || c.ended.Load() {
		return queued
	}
	for _, layer := range c.layers[:c.lingerBelow.Load()] {
		if holder, ok := layer.conn.(OutboundHolder); ok {
			queued += holder.OutboundHeld(layerCtx{layer})
		}
	}
	return queued
}

func (c *stack) closeLingering() {
	cause := c.lingering.Load()
	if cause == nil || !c.lingerDone.CompareAndSwap(false, true) {
		return
	}
	c.stopLinger()
	_ = c.socket.CloseWith(*cause)
}

// stopLinger stops the linger's bound.
func (c *stack) stopLinger() {
	c.lingerMu.Lock()
	if c.lingerTimer != nil {
		c.lingerTimer.Stop()
		c.lingerTimer = nil
	}
	c.lingerMu.Unlock()
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
	if c.ended.Load() {
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
	if c.timers != nil && !c.ended.Load() {
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

// layerNode is one layer of one connection: the state the layer works
// on, and the Conn the layer above reads and writes through.
type layerNode struct {
	stack *stack
	index int
	lower Conn
	conn  LayerConn // the layer's instance on this connection

	opened  atomic.Bool // the layer's OnOpen ran; set on a turn
	ended   bool        // the layer ended its stream up; only turns touch it
	openUp  atomic.Bool // the layer opened the layers above it
	closing atomic.Bool // the layer above asked for the close
	// writeShut is set as the layer above ends its writing half: writes into
	// the layer are refused from then on. shutPending marks the end not yet
	// passed below, shutFlushed its flush made, and writers counts the
	// writes in flight into the top layer, which the end waits for; see
	// shutDown.
	writeShut   atomic.Bool
	shutPending atomic.Bool
	shutFlushed atomic.Bool
	writers     atomic.Int32
	stalled     atomic.Bool // the layer stopped short; see LayerContext.Stall

	// delivered is what the layer handed up that the layer above has not
	// read, from at on; only the connection's turns touch it.
	delivered []byte
	at        int
}

// layerCtx is the LayerContext a layer holds: layerNode's methods as seen
// from below, where Write sends down and CloseWith closes through the layer
// below — the other way round from the Conn the layer above holds.
type layerCtx struct{ l *layerNode }

var (
	_ LayerContext = layerCtx{}
	_ Conn         = (*layerNode)(nil)
	_ WireCounter  = (*layerNode)(nil)
)

func (lc layerCtx) Lower() Conn { return lc.l.lower }

func (lc layerCtx) Deliver(p []byte) {
	if len(p) == 0 {
		return
	}
	l := lc.l
	l.compact()
	l.delivered = append(l.delivered, p...)
}

func (lc layerCtx) Space(n int) []byte {
	l := lc.l
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

func (lc layerCtx) Commit(n int) {
	l := lc.l
	l.delivered = l.delivered[:len(l.delivered)+n]
}

func (lc layerCtx) HeldAbove() int { return len(lc.l.delivered) - lc.l.at }

func (lc layerCtx) Stall() { lc.l.stalled.Store(true) }

func (lc layerCtx) End() { lc.l.ended = true }

func (lc layerCtx) Write(p []byte) (int, error) { return lc.l.lower.Write(p) }

func (lc layerCtx) Open() {
	lc.l.openUp.Store(true)
	lc.l.stack.openLayer(lc.l.index + 1)
}

func (lc layerCtx) CloseWith(cause error) error { return lc.l.lower.CloseWith(cause) }

func (lc layerCtx) SetTimer(when time.Time) error {
	return lc.l.stack.setTimer(lc.l.index, when, 0)
}

func (lc layerCtx) Wake() error { return lc.l.stack.socket.Wake() }

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
// before it alive. It runs as the layer delivers, on its OnInbound or
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
	if l.stack.holders && !l.stack.ended.Load() {
		for _, below := range l.stack.layers[:l.index+1] {
			if holder, ok := below.conn.(OutboundHolder); ok {
				held += holder.OutboundHeld(layerCtx{below})
			}
		}
	}
	return uint64(held) + l.stack.wireQueued.Load()
}

// WireSent is what the socket reported sent.
func (l *layerNode) WireSent() uint64 { return l.stack.wireSent.Load() }

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
	if holder, ok := l.conn.(OutboundHolder); ok && !l.stack.ended.Load() {
		queued += holder.OutboundHeld(layerCtx{l})
	}
	return queued
}

// Flush sends what this layer holds, if it holds any, then passes below.
func (l *layerNode) Flush() error {
	if l.stack.over() {
		return net.ErrClosed
	}
	var err error
	if flusher, ok := l.conn.(Flusher); ok {
		err = flusher.Flush(layerCtx{l})
	}
	if err == nil && !l.stack.over() {
		err = l.lower.Flush()
	}
	// What the flush sent may be what an end or a close waits for: a turn
	// looks, since this flush may run inside a layer above's.
	if c := l.stack; c.shutsPending.Load() > 0 || c.lingering.Load() != nil {
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
	n, err := l.conn.OnOutbound(layerCtx{l}, p)
	l.leave(len(p))
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
		n, err = l.conn.OnOutbound(layerCtx{l}, vec[0])
	} else {
		// One write whole, as the socket's Writev is: segments passed one by
		// one would interleave with another writer's.
		joined := AcquireBuffer(size)
		for _, segment := range vec {
			_, _ = joined.Write(segment)
		}
		n, err = l.conn.OnOutbound(layerCtx{l}, joined.Bytes())
		ReleaseBuffer(joined)
	}
	l.leave(size)
	return n, err
}

// enter checks that a write may enter the layer — it opened the layers above
// and is not closing — and, at the top of the stack, reserves its room; leave
// returns the room once the write is through.
func (l *layerNode) enter(n int) error {
	if !l.isTop() {
		if l.refuses() {
			return net.ErrClosed
		}
		return nil
	}
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
	if err := l.stack.reserve(n); err != nil {
		l.exit()
		return err
	}
	return nil
}

// refuses reports a layer writes no longer reach: not open above yet,
// closing, its writing half ended, or the connection closing or over — a
// layer below may have closed it itself.
func (l *layerNode) refuses() bool {
	return !l.openUp.Load() || l.closing.Load() || l.writeShut.Load() || l.stack.over()
}

func (l *layerNode) leave(n int) {
	if l.isTop() {
		l.stack.release(n)
		l.exit()
	}
}

// exit ends a write into the top layer; the last one out asks a turn to
// pass on an end that waited for it.
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
	c := l.stack
	if c.ended.Load() || !l.closing.CompareAndSwap(false, true) {
		return net.ErrClosed
	}
	if c.closing.Load() {
		// Closing already — a layer below closed it: no layer is called any
		// more, and the socket hears this cause, an abort cutting a
		// graceful close's wait short.
		return c.closeSocket(err)
	}
	// The first layer the close reaches marks how much of the stack it
	// waits for: this layer and those below.
	c.lingerBelow.CompareAndSwap(0, int32(l.index+1))
	if graceful, ok := l.conn.(GracefulCloser); ok && l.openUp.Load() {
		graceful.CloseGracefully(layerCtx{l}, err)
	}
	if flusher, ok := l.conn.(Flusher); ok && gracefulCause(err) && l.opened.Load() && !c.over() {
		_ = flusher.Flush(layerCtx{l})
	}
	return l.lower.CloseWith(err)
}

// CloseWrite ends the layer above's writing half, through the layer's
// protocol, after what was written before it: writes into the layer are
// refused from here on, what the layer holds is flushed first — a flush's
// error is CloseWrite's — and what it holds still, a send window's worth,
// goes below before the end does. A layer with no way to end one direction
// does not support it. Calling it again does nothing.
func (l *layerNode) CloseWrite() error {
	if _, ok := l.conn.(CloseWriter); !ok {
		return errUnsupported
	}
	if l.stack.over() {
		return net.ErrClosed
	}
	if !l.writeShut.CompareAndSwap(false, true) {
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
		if flusher, ok := l.conn.(Flusher); ok && !l.shutFlushed.Swap(true) {
			if err := flusher.Flush(ctx); err != nil {
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
		if holder, ok := l.conn.(OutboundHolder); ok && holder.OutboundHeld(ctx) > 0 {
			// Held bytes the layer cannot send yet: published again
			// before the look that may see them gone, so a report of
			// their leaving finds the end either way.
			l.shutPending.Store(true)
			if holder.OutboundHeld(ctx) > 0 {
				return false, nil
			}
			continue
		}
		l.stack.shutsPending.Add(-1)
		return true, l.conn.(CloseWriter).CloseWrite(ctx)
	}
	return false, nil
}

// finishShuts passes down the ends CloseWrite left waiting whose layers no
// longer hold anything back. It runs as a turn ends; an error closes the
// connection: nobody is left to return it to.
func (c *stack) finishShuts() {
	if c.shutsPending.Load() == 0 || c.ended.Load() {
		return
	}
	for i := len(c.layers) - 1; i >= 0; i-- {
		l := c.layers[i]
		if !l.shutPending.Load() || c.over() {
			continue
		}
		if done, err := l.shutDown(); done && err != nil && !errors.Is(err, net.ErrClosed) {
			_ = c.socket.CloseWith(err)
		}
	}
}

// LayerOf returns the LayerConn of the layer that built conn
// — the connection a layer hands up — so the layer above can ask it what it
// negotiated, a TLS layer's connection state:
//
//	if tls, ok := uio.LayerOf(conn).(interface{ State() tls.ConnectionState }); ok { … }
//
// It returns nil for a connection no layer built. FindLayer looks further
// down.
func LayerOf(conn Conn) LayerConn {
	if l, ok := conn.(*layerNode); ok {
		return l.conn
	}
	return nil
}

// FindLayer returns the first LayerConn of type T below conn, from the layer
// that built conn down to the socket — the TLS state under a WebSocket layer:
//
//	tls, ok := uio.FindLayer[interface{ State() tls.ConnectionState }](conn)
func FindLayer[T any](conn Conn) (T, bool) {
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

// refusePacket refuses a packet network on an Events with layers.
func (ev *Events) refusePacket(network string) error {
	if len(ev.use) == 0 {
		return nil
	}
	switch {
	case strings.HasPrefix(network, "udp"), strings.HasPrefix(network, "ip"),
		network == "unixgram", network == "unixpacket":
		return ErrLayersNeedStream
	}
	return nil
}

// socketLimit is the MaxOutboundBuffered the socket enforces: none once
// layers run, where the stack's top enforces it instead; see stack.admit.
func (ev *Events) socketLimit() int {
	if len(ev.use) > 0 {
		return 0
	}
	return ev.MaxOutboundBuffered
}

// observed is the connection an Observer hears about: the socket counts the
// bytes, and an Observer is reported the connection the Handler holds — the
// top layer's, once layers run — so it reads the same Userdata, and the
// same identity, the Handler's callbacks see.
func (ev *Events) observed(socket Conn) Conn {
	if len(ev.use) == 0 {
		return socket // no stack: the socket is the Handler's connection
	}
	if c, ok := socket.Userdata().(*stack); ok {
		return c.topConn()
	}
	return socket
}

// dialMarker replaces the dial's userdata with a probe when layers are
// installed, so the stack can report the dial once it opened. The second
// return is the userdata the dialed connection starts with.
func (ev *Events) dialMarker(userdata any) (*dialProbe, any) {
	if len(ev.use) == 0 {
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
