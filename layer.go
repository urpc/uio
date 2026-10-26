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
//   - Everything but OnOutbound runs on the connection's turns, which never
//     overlap one another or the callbacks of the layers above.
//   - Every layer hears OnClose once. The Handler's OnClose follows its
//     OnOpen: a connection whose stack never opened — a failed handshake —
//     ends without a Handler callback, and a dial reports its cause instead.
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

	// OnInbound runs on every turn once the layer opened: bytes from below
	// may have arrived, or none. It reads them from LayerContext.Lower,
	// consumes them with Discard, and hands up what they became with
	// LayerContext.Deliver. It may take one chunk at a time: while it consumes
	// and bytes still wait below, the turn calls it again.
	OnInbound(ctx LayerContext) error

	// OnOutbound runs for what the layer above writes, on the writer's
	// goroutine and possibly beside other writers: it transforms p, writes
	// the result with LayerContext.Write, and reports how much of p it took.
	// It returns ErrOutboundOverflow only for a write it took none of, as the
	// socket does: the writer above may send it again once the queue drains.
	// It runs only once the layer opened the layers above.
	OnOutbound(ctx LayerContext, p []byte) (int, error)

	// OnTimer runs on the turn the layer's timer came due, before OnInbound.
	OnTimer(ctx LayerContext) error

	// OnClose runs once, as the connection ends, whichever end closed it.
	OnClose(ctx LayerContext, cause error)
}

// CloseWriter is implemented by a LayerConn whose protocol ends one
// direction itself — a TLS close_notify sent before the socket's writing
// half shuts. It runs when the layer above calls CloseWrite; without it,
// CloseWrite passes to the layer below.
type CloseWriter interface {
	CloseWrite(ctx LayerContext) error
}

// GracefulCloser is implemented by a LayerConn with a goodbye to send —
// a TLS close_notify — when the layer above closes the connection. It runs
// before the close passes to the layer below.
type GracefulCloser interface {
	CloseGracefully(ctx LayerContext)
}

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

	// Stall tells UIO the layer stopped short of what it could deliver — its
	// bound was reached, with bytes waiting below or in its own buffers — so
	// the next read above runs a turn to take the rest. A layer that delivers
	// everything it is given never needs it.
	Stall()

	// Write sends p down, through the layer below.
	Write(p []byte) (int, error)

	// Open opens the layers above, once the layer's negotiation settled — at
	// once from OnOpen if it has none. A dial returns once the top layer
	// opened. Call it on the connection's turn.
	Open()

	// CloseWith closes the connection with cause, through the layer below.
	CloseWith(cause error) error

	// SetTimer arms this layer's own timer for when; a zero time stops it.
	SetTimer(when time.Time) error

	// Wake runs a turn of the connection.
	Wake() error
}

// Use collects layers for [Events.Use] and for uws's Options.Layers:
//
//	uws.Options{Layers: uio.Use(utls.Server(cfg), trace)}
//
// It returns a new slice of its arguments, so a list of layers can be built
// in a struct literal without spelling out the type.
func Use(layers ...Layer) []Layer {
	return layers
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
// delivers what arrives and writes what is written. Embed it in a layer's
// LayerConn to write only what that layer transforms.
type PassThrough struct{}

// OnOpen opens the layers above at once.
func (PassThrough) OnOpen(ctx LayerContext) { ctx.Open() }

// OnInbound delivers what arrived as it is.
func (PassThrough) OnInbound(ctx LayerContext) error {
	lower := ctx.Lower()
	chunk := lower.PeekChunk()
	if len(chunk) == 0 {
		return nil
	}
	ctx.Deliver(chunk)
	_, err := lower.Discard(len(chunk))
	return err
}

// OnOutbound writes what is written as it is.
func (PassThrough) OnOutbound(ctx LayerContext, p []byte) (int, error) { return ctx.Write(p) }

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
	return &stackDriver{layers: append([]Layer(nil), ev.use...), top: handler}
}

// stackDriver is the socket's Handler when layers are installed: it builds
// each connection's stack on OnOpen and drives it from then on. The stack
// lives in the socket's userdata slot; the userdata the connection came with
// moves to the stack, where every layer's Userdata reads it.
type stackDriver struct {
	layers []Layer
	top    Handler
}

func (h *stackDriver) OnOpen(socket Conn) {
	c := &stack{top: h.top, socket: socket}
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
	}
	c.openLayer(0)
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
	top    Handler
	socket Conn
	layers []*layerNode
	probe  *dialProbe // a dial waiting for the stack to open, until it does

	userdata atomic.Pointer[any] // every layer's Userdata, the Handler's

	topOpen bool        // the Handler's OnOpen ran; only turns touch it
	ended   atomic.Bool // the stack closed

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
	due := c.takeDue()
	for _, layer := range c.layers {
		if !layer.opened {
			return nil
		}
		if due&(1<<uint(layer.index)) != 0 {
			if err := layer.conn.OnTimer(layerCtx{layer}); err != nil {
				return err
			}
		}
		if err := c.inbound(layer); err != nil {
			return err
		}
		if c.ended.Load() {
			return nil
		}
	}
	if !c.topOpen {
		return nil
	}
	return c.top.OnData(c.topConn())
}

// inbound runs a layer's OnInbound until it takes nothing more: the bytes
// below may sit in several chunks, and a layer reads one at a time, so it is
// called again while what waits below shrinks. It stops once nothing waits,
// the layer took nothing, or it stalled at its bound.
func (c *stack) inbound(layer *layerNode) error {
	for {
		before := layer.waitingBelow()
		if err := layer.conn.OnInbound(layerCtx{layer}); err != nil {
			return err
		}
		if c.ended.Load() || layer.stalled.Load() {
			return nil
		}
		after := layer.waitingBelow()
		if after == 0 || after >= before {
			return nil
		}
	}
}

// openLayer opens layer i, or the Handler past the top layer. It runs on
// the connection's turn.
func (c *stack) openLayer(i int) {
	if c.ended.Load() {
		return
	}
	if i < len(c.layers) {
		layer := c.layers[i]
		if layer.opened {
			return
		}
		layer.opened = true
		layer.conn.OnOpen(layerCtx{layer})
		return
	}
	if c.topOpen {
		return
	}
	c.topOpen = true
	if probe := c.probe; probe != nil {
		c.probe = nil
		probe.settle(c.topConn())
	}
	c.top.OnOpen(c.topConn())
}

// closed is the socket's OnClose: every layer hears it, top down, and the
// Handler once it opened; a dial still waiting hears the cause instead.
func (c *stack) closed(err error) {
	if !c.ended.CompareAndSwap(false, true) {
		return
	}
	c.stopTimers()
	for i := len(c.layers) - 1; i >= 0; i-- {
		layer := c.layers[i]
		layer.conn.OnClose(layerCtx{layer}, err)
	}
	if probe := c.probe; probe != nil {
		c.probe = nil
		probe.fail(err)
		return
	}
	if c.topOpen {
		c.top.OnClose(c.topConn(), err)
	}
}

// topConn is the connection the Handler sees: the top layer's.
func (c *stack) topConn() Conn { return c.layers[len(c.layers)-1] }

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
	for _, slot := range c.timers {
		if !slot.when.IsZero() && (earliest.IsZero() || slot.when.Before(earliest)) {
			earliest = slot.when
		}
	}
	if earliest.Equal(c.armed) && fired.IsZero() {
		return nil
	}
	c.armed = earliest
	return c.socket.SetWakeDeadline(earliest)
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
		if slot.when.IsZero() || slot.when.After(now) {
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

	opened  bool        // the layer's OnOpen ran; only turns touch it
	openUp  atomic.Bool // the layer opened the layers above it
	closing atomic.Bool // the layer above asked for the close
	stalled atomic.Bool // the layer stopped short; see LayerContext.Stall

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
)

func (lc layerCtx) Lower() Conn { return lc.l.lower }

func (lc layerCtx) Deliver(p []byte) {
	if len(p) == 0 {
		return
	}
	l := lc.l
	l.reclaim()
	l.delivered = append(l.delivered, p...)
}

func (lc layerCtx) Space(n int) []byte {
	l := lc.l
	l.reclaim()
	if spare := cap(l.delivered) - len(l.delivered); spare < n {
		grown := make([]byte, len(l.delivered), len(l.delivered)+n)
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

// reclaim drops a fully read delivery, so the next one reuses its capacity.
func (l *layerNode) reclaim() {
	if l.at > 0 && l.at == len(l.delivered) {
		l.delivered, l.at = l.delivered[:0], 0
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

func (l *layerNode) SetLinger(secs int) error             { return l.lower.SetLinger(secs) }
func (l *layerNode) SetNoDelay(noDelay bool) error        { return l.lower.SetNoDelay(noDelay) }
func (l *layerNode) SetKeepAlive(keepAlive bool) error    { return l.lower.SetKeepAlive(keepAlive) }
func (l *layerNode) SetKeepAlivePeriod(secs int) error    { return l.lower.SetKeepAlivePeriod(secs) }
func (l *layerNode) Control(fn func(fd uintptr)) error    { return l.lower.Control(fn) }
func (l *layerNode) SetReadBuffer(size int) error         { return l.lower.SetReadBuffer(size) }
func (l *layerNode) SetWriteBuffer(size int) error        { return l.lower.SetWriteBuffer(size) }
func (l *layerNode) SetDeadline(t time.Time) error        { return l.lower.SetDeadline(t) }
func (l *layerNode) SetReadDeadline(t time.Time) error    { return l.lower.SetReadDeadline(t) }
func (l *layerNode) SetWriteDeadline(t time.Time) error   { return l.lower.SetWriteDeadline(t) }
func (l *layerNode) OutboundBuffered() int                { return l.lower.OutboundBuffered() }
func (l *layerNode) Flush() error                         { return l.lower.Flush() }
func (l *layerNode) Wake() error                          { return l.stack.socket.Wake() }
func (l *layerNode) YieldRead() error                     { return l.lower.YieldRead() }
func (l *layerNode) ReserveInbound(int) error             { return nil }
func (l *layerNode) ReserveOutbound(int) ([]byte, error)  { return nil, ErrReserveUnsupported }
func (l *layerNode) Close() error                         { return l.CloseWith(io.ErrUnexpectedEOF) }
func (l *layerNode) InboundBuffered() int                 { return l.held() + l.lower.InboundBuffered() }
func (l *layerNode) held() int                            { return len(l.delivered) - l.at }
func (l *layerNode) slot() int                            { return l.index + 1 }
func (l *layerNode) Peek(b []byte) []byte                 { return b[:copy(b, l.delivered[l.at:])] }
func (l *layerNode) PeekChunk() []byte                    { return l.delivered[l.at:] }
func (l *layerNode) WriteByte(b byte) error               { _, err := l.Write([]byte{b}); return err }
func (l *layerNode) WriteString(s string) (int, error)    { return l.Write([]byte(s)) }
func (l *layerNode) SetWakeDeadline(when time.Time) error { return l.stack.setTimer(l.slot(), when, 0) }

// SetWakeInterval arms the layer above's timer to repeat; see Conn.
func (l *layerNode) SetWakeInterval(interval time.Duration) error {
	if interval <= 0 {
		return l.stack.setTimer(l.slot(), time.Time{}, 0)
	}
	return l.stack.setTimer(l.slot(), time.Now().Add(interval), interval)
}

func (l *layerNode) Discard(n int) (int, error) {
	if held := l.held(); n > held {
		n = held
	}
	l.at += n
	l.reclaim()
	l.roomMade()
	return n, nil
}

func (l *layerNode) Read(b []byte) (int, error) {
	n := copy(b, l.delivered[l.at:])
	l.at += n
	l.reclaim()
	l.roomMade()
	return n, nil // non-blocking, as the socket's Read is
}

func (l *layerNode) WriteTo(w io.Writer) (int64, error) {
	n, err := w.Write(l.delivered[l.at:])
	l.at += n
	l.reclaim()
	l.roomMade()
	return int64(n), err
}

// roomMade runs another turn after the layer above read, when the
// layer stalled short of what it could deliver.
func (l *layerNode) roomMade() {
	if l.stalled.CompareAndSwap(true, false) {
		_ = l.stack.socket.Wake()
	}
}

// Write enters the layer, once it opened the layers above.
func (l *layerNode) Write(p []byte) (int, error) {
	if !l.openUp.Load() || l.closing.Load() {
		return 0, net.ErrClosed
	}
	return l.conn.OnOutbound(layerCtx{l}, p)
}

func (l *layerNode) Writev(vec [][]byte) (int, error) {
	total := 0
	for _, segment := range vec {
		n, err := l.Write(segment)
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

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
func (l *layerNode) CloseWith(err error) error {
	if !l.closing.CompareAndSwap(false, true) {
		return net.ErrClosed
	}
	if graceful, ok := l.conn.(GracefulCloser); ok && l.openUp.Load() {
		graceful.CloseGracefully(layerCtx{l})
	}
	return l.lower.CloseWith(err)
}

// CloseWrite ends the layer above's writing half: through the layer's
// protocol when it has a way, through the layer below otherwise.
func (l *layerNode) CloseWrite() error {
	if writer, ok := l.conn.(CloseWriter); ok {
		return writer.CloseWrite(layerCtx{l})
	}
	return l.lower.CloseWrite()
}

// LayerOf returns the LayerConn of the layer that built conn
// — the connection a layer hands up — so the layer above can ask it what it
// negotiated, a TLS layer's connection state:
//
//	if tls, ok := uio.LayerOf(conn).(interface{ State() tls.ConnectionState }); ok { … }
//
// It returns nil for a connection no layer built.
func LayerOf(conn Conn) LayerConn {
	if l, ok := conn.(*layerNode); ok {
		return l.conn
	}
	return nil
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
// takes the dial's userdata from it, and settles or fails it.
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
