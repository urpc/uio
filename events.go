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
}

// Executor schedules native connection I/O rounds. Both methods must return
// promptly. SubmitBatch accepts a prefix and returns its length; every accepted
// task must run exactly once. Accepted tasks must run asynchronously after the
// submission method returns; inline execution would block the event loop and
// is unsupported. The batch slice is callback-scoped, so an executor must copy
// references it retains after SubmitBatch returns. Submit false means the task
// was not run; a short SubmitBatch result means the rejected suffix was not run.
// Rejection closes the affected connection. A native connection may have its
// connection turn and a write turn in flight at once; both arrive here.
type Executor interface {
	Submit(task IOTask) bool
	SubmitBatch(tasks []IOTask) int
}

// readBuffer boxes a slice pointer so sync.Pool Put does not allocate an
// interface copy of the slice header on every native read round.
type readBuffer struct {
	bytes []byte
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
	callbackWG     sync.WaitGroup // std I/O goroutines still able to enter callbacks
	done           chan struct{}  // closed after OnStop and all owned work exits
	doneOnce       sync.Once
	closeReason    atomic.Pointer[error]
	ioPool         *ioTaskPool
	readPool       sync.Pool
	readBufferSize int

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
	// creates a taskgo typed task queue with its built-in concurrency. When set,
	// the executor lifecycle remains the caller's responsibility.
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
	MaxOutboundBuffered int

	// MaxInboundBuffered limits payload left unread after a callback returns.
	// Exceeding it closes the connection with ErrInboundOverflow. Zero disables
	// the limit.
	MaxInboundBuffered int

	// OnOpen fires first for every connection, in the connection's first
	// turn. On Unix a stream connection's first turn reads whatever its peer
	// already sent right after OnOpen returns, in the same turn. Lifecycle and
	// data callbacks are serialized per connection, while callbacks for
	// different connections may run concurrently.
	OnOpen func(c Conn)

	// OnData fires when inbound data is available. Inbound access methods are
	// valid only for this callback invocation.
	OnData func(c Conn) error

	// OnClose is the final callback for a connection and never overlaps its
	// OnOpen or OnData callback. Close only requests the close: the
	// connection is released, and OnClose runs, once the callback that closed
	// it has returned.
	OnClose func(c Conn, err error)

	// OnInbound reports bytes read from the socket before OnData and shares its
	// inbound-access scope.
	OnInbound func(c Conn, readBytes int)

	// OnOutbound reports bytes successfully written to the socket. It may run on
	// a backend writer goroutine or a native write turn, concurrently with the
	// connection's other callbacks, and does not grant inbound-buffer access.
	// Calls for one connection never overlap each other: on the native backend
	// a Write made inside OnOutbound is queued and sent after it returns. A
	// native UDP connection's datagrams leave on the goroutines that write
	// them, and OnOutbound may report several of them, on any one of those
	// goroutines, after its send.
	OnOutbound func(c Conn, writeBytes int)

	// OnStart runs synchronously after initialization and before listeners
	// begin accepting connections.
	OnStart func(ev *Events)

	// OnStop runs once after Serve has stopped its loops and connection tasks.
	OnStop func(ev *Events)
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

	if ev.OnStart != nil {
		ev.OnStart(ev)
	}

	defer func() {
		if ev.OnStop != nil {
			ev.OnStop(ev)
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
		ev.readPool.New = func() any { return &readBuffer{bytes: make([]byte, size)} }
	}

	return nil
}

func (ev *Events) onData(fdc *fdConn) error {
	if nil != ev.OnData {
		return ev.OnData(fdc)
	}
	// discard all received bytes if not set OnData.
	//
	started := fdc.beginInboundCallback()
	_, _ = fdc.Discard(-1)
	fdc.endInboundCallback(started)
	return nil
}

func (ev *Events) onSocketBytesRead(fdc *fdConn, readBytes int) {
	if readBytes > 0 && ev.OnInbound != nil {
		started := fdc.beginInboundCallback()
		ev.OnInbound(fdc, readBytes)
		fdc.endInboundCallback(started)
	}
}

func (ev *Events) onSocketBytesWrite(fdc *fdConn, writeBytes int) {
	if writeBytes > 0 && ev.OnOutbound != nil {
		ev.OnOutbound(fdc, writeBytes)
	}
}
