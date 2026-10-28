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
	"net"
	"runtime"
	"sync"
	"sync/atomic"

	"github.com/urpc/uio/internal/bytebuf"
	"github.com/urpc/uio/internal/plocal"
)

// CompositeBuffer exposes UIO's pooled segmented buffer without introducing a
// second public buffer type.
type CompositeBuffer = bytebuf.CompositeBuffer

// IOTask is one native connection I/O round: either a connection turn, which
// reads and runs the connection's callbacks and is serialized per connection,
// or a write turn, which only sends output queued from outside those callbacks
// and may run beside the connection turn.
type IOTask interface {
	RunTask()
	// ShardKey is the same for every task of one connection, its turns and
	// its write turns alike, so an Executor that keeps a key's tasks on one
	// worker keeps the connection's work, and the kernel's state for its
	// socket, on one CPU. An Executor may ignore it.
	ShardKey() uint32
}

// Executor schedules native connection I/O rounds. Both methods must return
// promptly. SubmitBatch accepts a prefix and returns its length; every accepted
// task must run exactly once. Accepted tasks must run asynchronously after the
// submission method returns; inline execution would block the event loop and
// is unsupported. The batch slice is callback-scoped, so an executor must copy
// references it retains after SubmitBatch returns. Submit false means the task
// was not run; a short SubmitBatch result means the rejected suffix was not run.
// Rejection closes the affected connection, and what a refused turn still
// owes — OnClose, and OnBytesWritten for the close's final flush — runs on UIO's
// own goroutines, a few at a time, or inline on the rejecting goroutine when
// the executor also refuses that cleanup. A connection whose first turn is
// refused never got OnOpen, so it closes without OnClose too, and the Dial or
// Adopt that offered it reports ErrFirstTurnRefused. A native connection may have its
// connection turn and a write turn in flight at once; both arrive here.
type Executor interface {
	Submit(task IOTask) bool
	SubmitBatch(tasks []IOTask) int
}

// readBuffer boxes a slice pointer so sync.Pool Put does not allocate an
// interface copy of the slice header on every native read round.
type readBuffer struct {
	bytes []byte
	// out is the reply space of the read round that borrowed this buffer:
	// a native stream turn's reservations are laid out in it while it reads
	// (see fdConn.scratch). Unset until a round reserves.
	out *bytebuf.Buffer
}

// entry is what one endpoint — a Listen, a Dial, an Adopt — hands each of
// its connections: the callbacks resolved for its chain, and whether that
// chain has layers. Every connection carries its own, read-only for its life.
type entry struct {
	ev      *Events // the Events the endpoint dials or listens through
	cb      callbacks
	layered bool // the chain has layers: the socket takes no payload limit
}

// Events owns listeners, event loops, the native connection-task scheduler,
// shared read buffers, and application callbacks for one server/dialer
// lifecycle. Configure it before Serve; an Events value is not restartable.
type Events struct {
	loopState                 // the backend's event loops
	acceptor       *acceptor  // connection acceptor
	mux            sync.Mutex // serializes initialization and shutdown publication
	closing        atomic.Bool
	ready          atomic.Bool    // Dial is allowed only after full initialization
	callbackWG     sync.WaitGroup // std connections, I/O goroutines, and callbacks
	socketCloses   closeCounter   // native UDP sockets closing; Serve waits for them
	done           chan struct{}  // closed after OnStop and all owned work exits
	doneOnce       sync.Once
	closeReason    atomic.Pointer[error]
	ioPool         *ioTaskPool
	readPool       plocal.Cache[*readBuffer] // read buffers, one kept per P
	readBufferSize int
	cb             callbacks // Handler, resolved when Serve starts
	plain          *entry    // the entry of a connection that brings no chain
	plainOnce      sync.Once

	// Pollers is the number of event loops. On Unix each loop is one
	// epoll or kqueue instance that a few goroutines wait on: they collect
	// readiness for the connections and listeners registered with it and hand
	// runnable connections to the Executor, and never run a callback. The
	// default is one loop per four Ps, between one and eight, so a
	// connection's events stay within one cluster of cores. The count
	// follows GOMAXPROCS rather than NumCPU, so a process whose Ps are fewer
	// than the machine's CPUs — a prefork child or a cgroup-limited container
	// — sizes its loops to the Ps it runs on. On the stdio and Windows
	// backend the loops only order registration and close.
	Pollers int

	// Executor supplies the native connection-round scheduler. When nil, UIO
	// runs connection turns and write turns on sharded schedulers of its own
	// (see internal/shardpool), both keyed by connection: by its event loop
	// first, so the connections of one loop share a subset of the shards, and
	// by its descriptor within that subset (see IOTask.ShardKey).
	// When set, the executor lifecycle remains the caller's responsibility.
	Executor Executor

	// ReusePort indicates whether to set up the SO_REUSEPORT socket option.
	// The default value is false.
	ReusePort bool

	// LockOSThread is used to determine whether each I/O event-loop is associated to an OS thread.
	// The default value is false.
	LockOSThread bool

	// MaxBufferSize is the buffer size of each socket read. Native connection
	// tasks may perform multiple reads per readiness event. The default is 4 KiB.
	MaxBufferSize int

	// WriteBufferedThreshold makes native callback writes smaller than this value
	// join the connection's current outbound batch instead of attempting an
	// immediate syscall. A connection task always flushes its batch before it
	// finishes. Zero disables size-based buffering; a negative value also keeps
	// it disabled. The stdio/Windows backend already writes through its dedicated
	// asynchronous writer and does not use this threshold. The default is zero.
	WriteBufferedThreshold int

	// MaxOutboundBuffered limits accepted but unsent payload bytes per
	// connection. It is a pure write budget: a write that would push buffered
	// unsent data beyond the limit returns ErrOutboundOverflow, while reads
	// keep flowing so a full-duplex peer can always drain its side. Callers
	// treat overflow as overload — defer, drop, or close — and bound the
	// receive side with MaxInboundBuffered. Zero disables the limit.
	//
	// With layers installed it bounds what the Handler writes: a write is
	// refused before it enters the top layer when the queue below has no room
	// for it, and what a layer writes below is never refused for size — a TLS
	// record, once encrypted, must reach the wire. A write reserves its room
	// as it enters, so concurrent writers cannot pass the limit together. The
	// limit counts the Handler's bytes, so the queue can exceed it by what the
	// layers add — a record's or a frame's few bytes of framing — and by the
	// layers' own control output, a handshake or a ping.
	MaxOutboundBuffered int

	// MaxInboundBuffered bounds payload read but not yet consumed. Once
	// callbacks leave this much unconsumed, the connection stops reading its
	// socket, so TCP flow control slows the peer instead of the buffer
	// growing; a Wake whose OnRead consumes below the limit reads again. The
	// buffer exceeds the limit by at most one read. A ReserveInbound larger
	// than the limit returns ErrInboundOverflow. Zero disables the limit.
	//
	// With layers installed it bounds what the layers hold for the Handler
	// too: once they hold the limit unread, the stack takes nothing more from
	// the socket, whose own buffer then fills to the limit and stops its
	// reads. It is also each layer's delivery bound: a layer whose delivery
	// to the layer above holds the limit is not called again until that layer
	// reads, so a delivery passes it by one callback's write at most;
	// DefaultDeliverBound stands in when this is zero. A connection holds at
	// most twice the limit, and a layer's one delivery more.
	MaxInboundBuffered int

	// AllowHalfOpen keeps a stream connection open after its peer shuts its
	// writing half. OnRead then runs once more, Read reports io.EOF once the
	// buffered bytes are consumed, and the connection can still write; it
	// closes once its own writing half is shut too, by CloseWrite, and Read
	// reports io.EOF, or when it is closed. Bytes left unconsumed, say for a
	// later Wake, keep it open until a callback consumes them. A reset after the end of the stream closes it on the native
	// backend at once, and on the stdio backend at the next write. Without it,
	// the end of the peer's stream closes the connection with io.EOF.
	AllowHalfOpen bool

	// Handler receives every connection's callbacks. A nil Handler leaves
	// connections without callbacks, their input discarded.
	Handler Handler

	// Observer is told the bytes every connection reads and writes. Nil
	// reports nothing.
	Observer TrafficObserver

	// Hooks runs around Serve. Nil runs nothing.
	//
	// Handler, Observer and Hooks are read once, when Serve starts.
	Hooks ServeHooks
}

// Serve starts the event loops and listens on each supplied address. Calling
// Serve without an address starts a dial-only Events.
func (ev *Events) Serve(addrs ...string) (err error) {
	if ev.closing.Load() {
		ev.finishLifecycle(net.ErrClosed)
		return net.ErrClosed
	}

	// initialize events
	if err = ev.initEvents(addrs); nil != err {
		ev.finishLifecycle(err)
		return err
	}

	if ev.cb.onStart != nil {
		ev.cb.onStart(ev)
	}

	defer func() {
		if ev.cb.onStop != nil {
			ev.cb.onStop(ev)
		}
		ev.finishLifecycle(err)
	}()

	// Accepting starts only now: accepting is what leads to OnOpen, and that
	// callback must not run while the application is still initializing.
	// Bound listeners hold early connections in their backlogs until then.
	return ev.serveLoops()
}

// Close publishes shutdown and returns without waiting. Use Wait or the return
// of Serve as the lifecycle join point.
func (ev *Events) Close(err error) error {
	ev.initiateClose(err)
	return nil
}

// Wait blocks until Serve has stopped every loop and connection task, all
// stdio I/O goroutines have exited, and OnStop has returned. Wait may be called
// before Serve; Close before Serve completes it immediately. It must not be
// called from a callback that is itself part of the lifecycle being joined.
func (ev *Events) Wait() error {
	ev.mux.Lock()
	done := ev.ensureDoneLocked()
	ev.mux.Unlock()
	<-done
	if reason := ev.closeReason.Load(); reason != nil {
		return *reason
	}
	return nil
}

// initEvents builds configuration, loops, and listeners under one publication
// lock so Close cannot observe a partially initialized wait group.
func (ev *Events) initEvents(addrs []string) (err error) {

	ev.mux.Lock()
	defer ev.mux.Unlock()
	ev.ensureDoneLocked()
	if ev.closing.Load() {
		return net.ErrClosed
	}

	// init configs.
	if err = ev.initConfig(); nil != err {
		return err
	}

	// init event loops.
	if err = ev.initLoops(); nil != err {
		return err
	}

	// init listener.
	if err = ev.initListeners(addrs); nil != err {
		ev.rollbackInit(err)
		return err
	}
	ev.publishLoops()
	ev.ready.Store(true)

	return nil
}

// initiateClose publishes closing and asks the loops to stop. It never waits
// for callbacks or workers, so it is safe from every callback context.
func (ev *Events) initiateClose(err error) {
	ev.mux.Lock()
	done := ev.ensureDoneLocked()
	// Publish closing before stopping the loops so producers reject new work.
	if !ev.closing.CompareAndSwap(false, true) {
		ev.mux.Unlock()
		return
	}
	ev.recordCloseReason(err)
	ev.ready.Store(false)
	idle := !ev.stopLoopsLocked(err)
	ev.mux.Unlock()
	if idle {
		ev.doneOnce.Do(func() { close(done) })
	}
}

func (ev *Events) ensureDoneLocked() chan struct{} {
	if ev.done == nil {
		ev.done = make(chan struct{})
	}
	return ev.done
}

func (ev *Events) recordCloseReason(err error) {
	reason := err
	ev.closeReason.CompareAndSwap(nil, &reason)
}

func (ev *Events) finishLifecycle(err error) {
	ev.recordCloseReason(err)
	ev.mux.Lock()
	done := ev.ensureDoneLocked()
	ev.mux.Unlock()
	ev.doneOnce.Do(func() { close(done) })
}

func (ev *Events) initConfig() error {
	ev.cb = resolveCallbacks(ev.Handler, ev.Observer, ev.Hooks)
	// The plain entry is built on first use, when an endpoint takes it, just
	// as a layered one is built at its Listen or Dial.

	if ev.Pollers <= 0 {
		ev.Pollers = defaultPollers(runtime.GOMAXPROCS(0))
	}
	ev.Pollers = min(ev.Pollers, runtime.GOMAXPROCS(0))

	if ev.MaxBufferSize <= 0 {
		ev.MaxBufferSize = 1024 * 4
	}
	if ev.readBufferSize == 0 {
		ev.readBufferSize = ev.MaxBufferSize
		size := ev.readBufferSize
		ev.readPool.Init(func() *readBuffer { return &readBuffer{bytes: make([]byte, size)} })
	}

	return nil
}

func (ev *Events) onRead(fdc *fdConn) error {
	skipRead, err := fdc.fireWakeTimer()
	if err != nil {
		return err
	}
	if skipRead {
		return nil // the timer callback closed: no read callback follows
	}
	if onRead := fdc.callbacks().onRead; onRead != nil {
		return onRead(fdc)
	}
	// Without a data callback the received bytes are discarded.
	started := fdc.beginInboundCallback()
	_, _ = fdc.Discard(-1)
	fdc.endInboundCallback(started)
	return nil
}

func (ev *Events) onSocketBytesRead(fdc *fdConn, readBytes int) {
	if onInbound := fdc.callbacks().onBytesRead; readBytes > 0 && onInbound != nil {
		started := fdc.beginInboundCallback()
		onInbound(ev.observed(fdc), readBytes)
		fdc.endInboundCallback(started)
	}
}

// onSocketBytesWrite stays small enough to inline into every send path, so a
// connection nobody observes pays a nil check and no call.
func (ev *Events) onSocketBytesWrite(fdc *fdConn, writeBytes int) {
	if writeBytes > 0 && fdc.callbacks().onBytesWritten != nil {
		ev.reportBytesWritten(fdc, writeBytes)
	}
}

func (ev *Events) reportBytesWritten(fdc *fdConn, writeBytes int) {
	fdc.callbacks().onBytesWritten(ev.observed(fdc), writeBytes)
}

// closeCounter counts closes Serve waits for. Unlike a sync.WaitGroup, a close
// may begin while Serve is already waiting — a Listener.Close beside
// Events.Close — and the wait then covers it too.
type closeCounter struct {
	mu   sync.Mutex
	cond *sync.Cond
	n    int
}

func (c *closeCounter) add() {
	c.mu.Lock()
	c.n++
	c.mu.Unlock()
}

func (c *closeCounter) done() {
	c.mu.Lock()
	c.n--
	if c.n == 0 && c.cond != nil {
		c.cond.Broadcast()
	}
	c.mu.Unlock()
}

// wait returns once no counted close is running.
func (c *closeCounter) wait() {
	c.mu.Lock()
	for c.n > 0 {
		if c.cond == nil {
			c.cond = sync.NewCond(&c.mu)
		}
		c.cond.Wait()
	}
	c.mu.Unlock()
}
