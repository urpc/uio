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

// The stack engine: the driver the Events install when a chain is present,
// one connection's layer nodes, its turn state machine, inbound pacing, and
// per-layer timers.

package uio

import (
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/urpc/uio/internal/blockio"
	"github.com/urpc/uio/internal/writecontrol"
)

type driverSlot interface {
	driverData() any
	setDriverData(any)
}

func stackStateOf(socket Conn) any {
	if d, ok := socket.(driverSlot); ok {
		return d.driverData()
	}
	return socket.Userdata()
}

func setStackState(socket Conn, value any) {
	if d, ok := socket.(driverSlot); ok {
		d.setDriverData(value)
		return
	}
	socket.SetUserdata(value)
}

func stackOf(socket Conn) *stack {
	c, _ := stackStateOf(socket).(*stack)
	return c
}

// stackDriver is the socket's Handler when layers are installed: it builds
// each connection's stack on OnOpen and drives it from then on.
type stackDriver struct {
	layers        []Layer
	top           Handler
	limit         int  // MaxOutboundBuffered, which the stack's top enforces
	inLimit       int  // MaxInboundBuffered, which bounds what the layers hold too
	halfOpen      bool // AllowHalfOpen: the Handler may outlive its peer's end
	blockingWrite bool // BlockingWrite: top Write/Writev/WriteOwned suspend off-turn callers on overflow
}

func (h *stackDriver) OnOpen(socket Conn) {
	packetBottom := len(h.layers) > 0 && isPacketLayer(h.layers[0]) &&
		socket.LocalAddr() != nil && packetNetwork(socket.LocalAddr().Network())
	c := &stack{top: h.top, socket: socket, limit: h.limit, inLimit: h.inLimit, halfOpen: h.halfOpen, blockingWrite: h.blockingWrite, packetBottom: packetBottom}
	if probe, ok := stackStateOf(socket).(*dialProbe); ok {
		c.probe.Store(probe)
	}
	layers := h.layers
	c.layers = make([]*layerNode, len(layers))
	// Nodes share one allocation; the pointer slice keeps their indexed faces.
	nodes := make([]layerNode, len(layers))
	var below Conn = socketLower{Conn: socket, stack: c}
	for i := range layers {
		node := &nodes[i]
		node.stack = c
		node.index = i
		node.lower = below
		node.upper = layerUpper{node}
		c.layers[i] = node
		below = node
	}
	setStackState(socket, c)
	c.firstHeld = -1
	for i, layer := range layers {
		node := c.layers[i]
		node.conn = layer.Attach(layerCtx{node})
		node.ops = opsOf(node.conn)
		if node.ops.held != nil {
			c.holders = true
			if c.firstHeld < 0 {
				c.firstHeld = i
			}
		}
		if node.ops.turnEnd != nil {
			c.hasTurnEnd = true
		}
	}
	c.enterTurnOwner()
	defer c.leaveTurnOwner()
	c.openUpTo()
	c.finishTurn()
}

func (h *stackDriver) OnRead(socket Conn) error {
	if c := stackOf(socket); c != nil {
		return c.turn()
	}
	return nil
}

// OnTimer does nothing: with layers, the stack's own timer table drives the
// turn, and run dispatches the Handler's slot to the Handler itself. The
// socket-level mirror the stack aims at is therefore already accounted for.
func (h *stackDriver) OnTimer(socket Conn) error { return nil }

func (h *stackDriver) OnClose(socket Conn, err error) {
	switch held := stackStateOf(socket).(type) {
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
	top           Handler
	socket        Conn
	layers        []*layerNode
	probe         atomic.Pointer[dialProbe] // a dial waiting for the stack to open, until it does
	limit         int                       // MaxOutboundBuffered; see reserve
	reserved      atomic.Int64              // bytes admitted at the top, not yet queued below
	halfOpen      bool                      // AllowHalfOpen
	blockingWrite bool                      // Events.BlockingWrite
	hasTurnEnd    bool                      // at least one layer implements OnTurnEnd
	inLimit       int                       // MaxInboundBuffered; see inboundFull
	// writeWatched marks that at least one layer node has a write-progress
	// watcher; heldPeak is the most output the layers held since the last turn
	// ended, and writeRefused a write the stack refused for capacity since then.
	// See noteQueueRoom.
	writeWatchOnce sync.Once
	writeWatched   atomic.Bool
	heldPeak       atomic.Int64
	writeRefused   atomic.Bool
	reserveRefused atomic.Bool
	block          atomic.Pointer[blockio.State]
	pullRead       atomic.Bool
	localClosed    atomic.Bool

	// packetBottom marks a stack on a packet connection whose bottom layer is
	// a PacketLayer: its turns carry one datagram each, and the bottom layer
	// is never paced, because a datagram it does not take is gone.
	packetBottom bool
	// firstHeld is the index of the lowest layer that holds output, or -1.
	firstHeld int
	holders   bool // some layer holds output of its own

	// closeCause is the cause of the close that began the stack's, for the
	// dial a closing open stage fails; closePlan is a close a layer began for
	// itself, walked on the turn, never inside the callback that asked.
	closeCause atomic.Pointer[error]
	closePlan  atomic.Pointer[closePlan]

	// inHeld marks a stack that stopped pulling from below because what its
	// layers hold reached inLimit; a read above that makes room wakes it.
	inHeld atomic.Bool

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

	// inTurn is set while a turn runs; what waits for one — an end CloseWrite
	// left waiting, or a layer's close plan — is done as it ends rather than
	// asking for another. turnAsked marks a turn requestTurn asked for and no
	// turn has begun since: one is enough for any number of requests.
	inTurn    atomic.Bool
	turnOwner atomic.Int64
	turnAsked atomic.Bool

	// timers holds the timer of every layer, and in its last slot the one
	// the Handler armed with SetWakeDeadline; the socket's own wake slot
	// carries the earliest of them. hasTimers is cleared again when every
	// slot is stopped, so ordinary turns do not reacquire timerMu after a
	// one-shot timer has fired or a timer is cancelled.
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
	c.enterTurnOwner()
	defer c.leaveTurnOwner() // a panic an Executor recovers leaves none set
	c.noteHeldPeak()
	err := c.run()
	if err != nil {
		if errors.Is(err, io.EOF) {
			c.runTurnEnds()
		}
		c.leaveTurnOwner() // the failed callback is the last this turn runs
		return c.failTurn(err)
	}
	c.finishTurn()
	return nil
}

func (c *stack) enterTurnOwner() {
	c.turnOwner.Store(currentGoroutineID())
	c.inTurn.Store(true)
}

func (c *stack) leaveTurnOwner() {
	c.turnOwner.Store(0)
	c.inTurn.Store(false)
}

func (c *stack) inTurnOwner() bool {
	if !c.inTurn.Load() {
		return false
	}
	owner := c.turnOwner.Load()
	return owner != 0 && owner == currentGoroutineID()
}

// failTurn ends a connection whose turn failed — a layer's callback returned
// an error, or a layer's flush did as the turn ended, the open stage
// included. A stream's own end (io.EOF, io.ErrUnexpectedEOF) closes
// gracefully, so what was queued still leaves; anything else aborts, and the
// goodbyes are told the cause. Either way nothing reaches a layer after this
// point, and the walk runs here, off the failing callback. An abort reports
// the cause back — the core's close follows it, OnClose with it — while a
// graceful close is the socket's now: reporting its cause would close the
// socket at once and cut the drain short.
func (c *stack) failTurn(err error) error {
	graceful := errors.Is(err, io.EOF)
	if !c.over() {
		c.recordCloseCause(err)
		c.beginClosing(len(c.layers), graceful)
		c.closePlan.Store(&closePlan{cause: err, graceful: graceful, from: int32(len(c.layers))})
	}
	c.runClosePlan()
	if graceful {
		return nil
	}
	return err
}

// over reports a stack no layer is called on any more: one that ended, or
// one that is closing. A layer's callback can end it — a close from OnTimer or
// OnRead, or a protocol error — so every call into a layer is followed by this
// check before the next.
func (c *stack) over() bool { return c.state.Load()&stackPhaseMask >= stackDraining }

// closedStack reports the socket's OnClose run: no turn and no callback is
// owed any more.
func (c *stack) closedStack() bool { return c.state.Load()&stackPhaseMask == stackClosed }

// noLayerCalls reports a close that reached the socket: the walk that ran (or
// was cut short before it began) is over, and nothing may call a layer any
// more. A walk still running — the plan was taken before the cut — stops at
// its next layer; see closeWalk.
func (c *stack) noLayerCalls() bool { return c.state.Load()&stackPhaseMask >= stackEnding }

// moveToEnding publishes that the close reached the socket. The phase is the
// walk's one source of truth: a cut and a walk commit in one word, so no
// goodbye starts once the socket's close was committed.
func (c *stack) moveToEnding() {
	for {
		state := c.state.Load()
		if state&stackPhaseMask >= stackEnding {
			return
		}
		if c.state.CompareAndSwap(state, state&^stackPhaseMask|stackEnding) {
			return
		}
	}
}

// stackNoWriters is the close initiator of a close that came from no layer —
// the socket's directly: no layer, and no Handler write, may go down.
const stackNoWriters = -1

// beginClosing publishes the stack as closing on behalf of the layer at
// initiator, reporting whether this call published it.
func (c *stack) beginClosing(initiator int, graceful bool) bool {
	phase := stackAborting
	if graceful {
		phase = stackDraining
	}
	for {
		state := c.state.Load()
		if state&stackPhaseMask >= stackDraining {
			return false
		}
		next := uint32(initiator+1)<<stackPhaseBits | phase
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
	case stackDraining, stackAborting:
		return uint32(index+1) < state>>stackPhaseBits
	case stackEnding, stackClosed:
		return false
	}
	return true
}

// stackPhase is the stack's lifecycle; the low bits of stack.state.
const (
	stackOpening  uint32 = iota // the layers open; the dial waits
	stackOpen                   // settled: turns, writes and closes flow
	stackDraining               // graceful Close walk in progress
	stackAborting               // Abort walk in progress
	stackEnding                 // close committed to socket; no layer called from here
	stackClosed                 // the socket's OnClose ran
)

const (
	stackPhaseBits = 8
	stackPhaseMask = 1<<stackPhaseBits - 1
)

// finishTurn ends a turn: outbound room changes are reported and deferred
// close or CloseWrite work settles once no layer callback is on the stack.
func (c *stack) runTurnEnds() {
	c.turnOwner.Store(0)
	if c.hasTurnEnd && !c.over() {
		for i := len(c.layers) - 1; i >= 0; i-- {
			node := c.layers[i]
			if node.ops.turnEnd != nil && node.opened.Load() && !c.over() {
				node.ops.turnEnd(layerCtx{node})
			}
		}
	}
}

func (c *stack) finishTurn() {
	c.runTurnEnds()
	c.noteQueueRoom()
	c.leaveTurnOwner()
	if c.shutsPending.Load() > 0 || c.over() {
		c.settle()
	}
}

func (c *stack) morePacketsBelow() bool {
	fdc, ok := c.socket.(*fdConn)
	return c.packetBottom && ok && fdc.moreInUDPBatch()
}

func (c *stack) enableWriteWatch() {
	c.writeWatchOnce.Do(func() {
		c.writeWatched.Store(true)
		c.noteHeldPeak()
		writecontrol.Watch(c.socket, c.notifyWrite)
	})
}

func (c *stack) notifyWrite(sent int) {
	if c.closedStack() {
		return
	}
	for _, layer := range c.layers {
		if ctrl := layer.writeControl.Load(); ctrl != nil {
			ctrl.Notify(sent)
		}
	}
}

// noteQueueRoom notifies write-control watchers when the connection's queue
// may have room again where no socket write reports it: the output the layers
// hold shrank — a reliable transport's queue its peer acknowledged — or a
// write was refused for capacity since the last turn ended.
func (c *stack) noteQueueRoom() {
	if !c.writeWatched.Load() || c.over() || !c.topOpen.Load() {
		return
	}
	room := c.layers[len(c.layers)-1].writers.Load() > 0 ||
		c.writeRefused.Load() && c.writeRefused.Swap(false)
	if c.holders {
		held := int64(c.heldOutput())
		if held < c.heldPeak.Swap(held) {
			room = true
		}
	}
	if room {
		c.notifyWrite(0)
	}
}

// noteRefused records a write at the top refused for capacity, before the
// write leaves; see noteQueueRoom.
func (c *stack) noteRefused(err error) {
	if c.writeWatched.Load() && errors.Is(err, ErrOutboundOverflow) {
		c.writeRefused.Store(true)
	}
}

// noteHeldPeak records what the layers hold now, if it is the most since
// the last turn ended.
func (c *stack) noteHeldPeak() {
	if !c.writeWatched.Load() || !c.holders {
		return
	}
	held := int64(c.heldOutput())
	for {
		peak := c.heldPeak.Load()
		if held <= peak || c.heldPeak.CompareAndSwap(peak, held) {
			return
		}
	}
}

// heldOutput is the output the layers hold of their own.
func (c *stack) heldOutput() int {
	held := 0
	for _, layer := range c.layers[max(c.firstHeld, 0):] {
		if layer.ops.held != nil {
			held += layer.ops.held(layerCtx{layer})
		}
	}
	return held
}

// settle does, as a turn ends, what waits for one: a layer's close plan or
// ends CloseWrite left waiting for output to go below. It runs only where no
// layer is in a callback — the framework never calls a layer from inside
// another's — so a layer holding its own lock while it writes below is never
// called back.
func (c *stack) settle() {
	if c.closePlan.Load() != nil {
		// A close a layer began during this turn walks here, at its end, off
		// every callback.
		c.runClosePlan()
	}
	if c.over() {
		return
	}
	c.finishShuts()
}

// requestTurn asks for a turn to settle what waits for one; a turn running
// now settles it as it ends. Requests made before the asked-for turn begins
// share it: that turn settles everything any of them waits for, so writes
// refused while turns are held up schedule one wake, not one each.
func (c *stack) requestTurn() {
	if !c.inTurn.Load() && !c.turnAsked.Load() && c.turnAsked.CompareAndSwap(false, true) {
		if c.socket.Wake() != nil {
			c.turnAsked.Store(false)
		}
	}
}

// run is a turn's work: the layers bottom up, then the Handler.
func (c *stack) run() error {
	due := c.takeDue()
	for _, layer := range c.layers {
		if !layer.opened.Load() {
			return c.endOfStream(false)
		}
		if due&(1<<uint(layer.index)) != 0 {
			if err := layer.conn.OnTimer(layerCtx{layer}); err != nil {
				return err
			}
			if c.over() {
				return nil
			}
		}
		// A pause stops this boundary's consumer, never the whole turn:
		// timers and higher layers still run.
		if err := c.inbound(layer); err != nil {
			return err
		}
		if c.over() {
			return nil // a layer closed the connection: nothing above runs
		}
		c.openUpTo() // a ctx.Open() the layer made opens the layers above now
		if c.over() {
			return nil
		}
		if layer.index == 0 && due == 0 && c.morePacketsBelow() && c.topOpen.Load() &&
			!layer.ended.Load() && !layer.stalled.Load() && layer.held() < c.deliverBound() {
			return nil
		}
	}
	if !c.topOpen.Load() {
		return c.endOfStream(false)
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
	if control := top.readControl.Load(); !control.ReadPaused() {
		top.woken.Store(false)
		control.ConsumerRuns()
		if c.pullRead.Load() {
			if st := c.block.Load(); st != nil {
				if r := st.ActiveReader(); r != nil {
					r.Pump()
				}
			}
		} else if err := c.top.OnRead(top); err != nil {
			return err
		}
	}
	return c.endOfStream(true)
}

// endOfStream closes a stack without half-open once the peer's end reached
// the Handler, or a half-open stack once both halves ended and drained.
func (c *stack) endOfStream(handlerRan bool) error {
	if !c.ended() {
		return nil
	}
	top := c.layers[len(c.layers)-1]
	heldByConsumer := c.handlerHeld() || (top.held() > 0 && top.woken.Load())
	if c.halfOpen {
		if !c.writeSideEnded(top) || !c.drained() || c.socket.InboundBuffered() != 0 || heldByConsumer {
			return nil
		}
		return io.EOF
	}
	if handlerRan && heldByConsumer {
		return nil
	}
	if !c.topOpen.Load() {
		// The underlying stream ended before every layer opened: report truncation!
		return io.ErrUnexpectedEOF
	}
	if c.socket.InboundBuffered() != 0 || c.heldBelowTop() != 0 {
		// The end came behind a partial unit the layers could not hand up.
		return io.ErrUnexpectedEOF
	}
	return io.EOF
}

func (c *stack) writeSideEnded(top *layerNode) bool {
	return top.gate.Load()&gateWriteShut != 0 && c.shutsPending.Load() == 0
}

// heldBelowTop is the input the layers below the Handler's layer hold for
// the one above them.
func (c *stack) heldBelowTop() int {
	held := 0
	for _, layer := range c.layers[:len(c.layers)-1] {
		held += layer.held()
	}
	return held
}

// handlerHeld reports a Handler that holds the end of the stream back:
// paused, or owed the turn a released pause asked for.
func (c *stack) handlerHeld() bool {
	return len(c.layers) != 0 && c.layers[len(c.layers)-1].readControl.Load().Held()
}

// ended reports a stream whose end reached the Handler: the top layer ended
// it, or the socket ended with every layer able to move its input on — the
// turn ran each one unpaused and it took what it could. A layer that never
// calls Upper().End() still lets the connection close then. What a layer left
// below the Handler at the socket's end can never be completed — its peer
// sent a partial unit, or a delivery its consumer above declined to read —
// and endOfStream reports that as a truncation. Pacing is not a hold: at
// the socket's end no turn can come to unpace the layer, so its delivery is
// as declined as a smaller one. A paused or stalled layer may still move
// its input, and holds the end back, as a paused Handler does.
func (c *stack) ended() bool {
	last := len(c.layers) - 1
	if last < 0 {
		return false
	}
	if c.layers[last].ended.Load() {
		return true
	}
	if !socketEndedBehind(c.socket) {
		return false
	}
	for _, layer := range c.layers {
		if layer.stalled.Load() || layer.unpaced.Load() || layer.readControl.Load().Held() {
			return false
		}
	}
	return true
}

// socketEndedBehind reports a socket whose peer ended its writing half,
// bytes it still holds included. State alone reports the end only once its
// buffer is empty; custom transports offer nothing more.
func socketEndedBehind(socket Conn) bool {
	if s, ok := socket.(interface{ endedBehind() bool }); ok {
		return s.endedBehind()
	}
	return socket.State()&StateReadEnded != 0
}

// drained reports a stack that handed up everything the peer sent, the
// Handler's delivery read too: a half-open connection whose halves both
// ended may close.
func (c *stack) drained() bool {
	for _, layer := range c.layers {
		if layer.held() != 0 || layer.stalled.Load() {
			return false
		}
	}
	return true
}

// inbound runs a layer's OnRead until it takes nothing more: the bytes
// below may sit in several chunks, and a layer reads one at a time, so it is
// called again while what waits below shrinks. It stops once nothing waits,
// the layer took nothing, or it stalled at its bound. A packet connection
// has one datagram per turn instead: the bottom layer is called once and
// never paced — what it leaves of the datagram is dropped when the turn
// returns.
func (c *stack) inbound(layer *layerNode) error {
	packetBottom := c.packetBottom && layer.index == 0
	for {
		if layer.lower.ReadPaused() {
			return nil
		}
		// Only the bottom layer stops at the limit: it is the one taking new
		// bytes from the socket. The layers above go on moving what the stack
		// holds up to the Handler, which must be able to read it. A packet
		// connection's datagram waits for no one, so its bottom layer is not
		// stopped here.
		if layer.index == 0 && !packetBottom && c.inboundFull() {
			return nil
		}
		// A layer whose delivery to the layer above is full waits: it is not
		// called again until the layer above reads, and that read — roomMade
		// on this layer — asks for the turn. The layers above it go on
		// draining what it delivered. A packet bottom is never paced: a
		// datagram it does not take is gone.
		if !packetBottom && !layer.ops.selfPaced && layer.held() >= c.deliverBound() {
			layer.paced.Store(true)
			return nil
		}
		layer.unpaced.Store(false)
		below, _ := layer.lower.(*layerNode)
		if below != nil {
			below.woken.Store(false)
			below.readControl.Load().ConsumerRuns()
		}
		before := layer.waitingBelow()
		if err := layer.conn.OnRead(layerCtx{layer}); err != nil {
			return err
		}
		if c.over() || layer.stalled.Load() || (below != nil && below.woken.Load()) {
			return nil
		}
		// A ctx.Open() the layer made takes effect here, before the round
		// reads on into bytes that arrived behind its handshake.
		c.openUpTo()
		if c.over() {
			return nil
		}
		if packetBottom {
			// The turn carried one datagram. Reading on would hand the layer
			// whatever it left of it as if it were another packet; the
			// connection drops that tail when the turn returns.
			return nil
		}
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
		if len(c.layers) > 0 {
			c.layers[0].unpaced.Store(true)
		}
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
	c.closePlan.Store(nil)
	if st := c.block.Load(); st != nil {
		st.OnClose(c.localClosed.Load(), err)
	}
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
		cause := err
		if cause == nil {
			if held := c.closeCause.Load(); held != nil {
				cause = *held
			}
		}
		probe.fail(cause)
	}
}

// topConn is the connection the Handler sees: the top layer's.
func (c *stack) topConn() Conn { return c.layers[len(c.layers)-1] }

// queued is what the connection still has to send: the socket's queue and
// what the layers hold.
func (c *stack) queued() int {
	if c.holders {
		return c.topConn().OutboundBuffered()
	}
	return c.socket.OutboundBuffered()
}

// closeSocket is a close reaching the socket with no layer above to run
// OnClosing for: the stack moves to closing first, then the socket closes.
func (c *stack) closeSocket(cause error, graceful bool) error {
	c.recordCloseCause(cause)
	return c.endAtSocket(cause, c.beginClosing(stackNoWriters, graceful), graceful)
}

// recordCloseCause keeps the first close's cause.
func (c *stack) recordCloseCause(cause error) {
	if cause != nil {
		c.closeCause.CompareAndSwap(nil, &cause)
	}
}

// socketClose ends the stack at the socket in the mode the walk carried.
func (s socketLower) socketClose(cause error, graceful bool) error {
	s.stack.recordCloseCause(cause)
	return s.stack.endAtSocket(cause, s.stack.beginClosing(stackNoWriters, graceful), graceful)
}

// endAtSocket hands cause to the socket, graceful choosing between the
// socket's draining Close and its immediate Abort.
func (c *stack) endAtSocket(cause error, first, graceful bool) error {
	c.moveToEnding()
	var err error
	if graceful {
		if cg, ok := c.socket.(interface{ closeGracefully(error) error }); ok && cause != nil {
			err = cg.closeGracefully(cause)
		} else {
			err = c.socket.Close()
		}
	} else {
		err = c.socket.Abort(cause)
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
		if reserved > int64(n) {
			c.reserveRefused.Store(true)
		}
		return ErrOutboundOverflow
	}
	return nil
}

// release returns what reserve took, once the write it admitted is queued
// below, or failed. If a concurrent writer was refused by stack.reserve while
// this transient reservation was in flight, releasing it notifies write-control
// watchers without disturbing writeRefused set by a full lower layer.
func (c *stack) release(n int) {
	if c.limit > 0 {
		c.reserved.Add(-int64(n))
		if c.writeWatched.Load() && c.reserveRefused.Load() && c.reserveRefused.Swap(false) {
			c.notifyWrite(0)
		}
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
	}
	wasActive := !c.timers[slot].when.IsZero()
	c.timers[slot] = timerSlot{when: when, every: every}
	if when.IsZero() {
		c.refreshTimersActiveLocked()
	} else if !wasActive {
		c.hasTimers.Store(true)
	}
	return c.aimLocked(time.Time{})
}

// refreshTimersActiveLocked updates the lock-free guard used by takeDue and
// reaim. It is called only while timerMu is held, after a timer was cleared
// or consumed, so a zero value means no slot can wake the connection.
func (c *stack) refreshTimersActiveLocked() {
	for _, slot := range c.timers {
		if !slot.when.IsZero() {
			return // clearing one slot cannot deactivate another
		}
	}
	c.hasTimers.Store(false)
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
	cleared := false
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
			cleared = true
		}
	}
	if cleared {
		c.refreshTimersActiveLocked()
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
	c.hasTimers.Store(false)
	c.timerMu.Unlock()
}
