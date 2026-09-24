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
	"runtime"
	"sync"
	"sync/atomic"

	"github.com/urpc/uio/internal/bytebuf"
)

// CompositeBuffer exposes UIO's pooled segmented buffer without introducing a
// second public buffer type.
type CompositeBuffer = bytebuf.CompositeBuffer

// IOTask is one serialized native connection I/O round.
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
// Rejection closes the affected connection.
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
	master         *eventLoop     // serving listener
	workers        []*eventLoop   // serving connection
	acceptor       *acceptor      // connection acceptor
	waitGroup      sync.WaitGroup // wait for all eventLoop exit on shutdown
	mux            sync.Mutex     // serializes initialization and shutdown publication
	closing        atomic.Bool
	ready          atomic.Bool    // Dial is allowed only after full initialization
	callbackWG     sync.WaitGroup // std I/O goroutines still able to enter callbacks
	done           chan struct{}  // closed after OnStop and all owned work exits
	doneOnce       sync.Once
	closeReason    atomic.Pointer[error]
	ioPool         *ioTaskPool
	readPool       sync.Pool
	readBufferSize int

	// Pollers is the number of event-loop goroutines.
	// The default value is 4, capped by runtime.NumCPU().
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
	// connection. Native transports pause that connection's reads at 75% of the
	// limit and resume them at 50%. A write that would push buffered unsent data
	// beyond the limit returns ErrOutboundOverflow. Zero disables the limit.
	MaxOutboundBuffered int

	// MaxInboundBuffered limits payload left unread after a callback returns.
	// Exceeding it closes the connection with ErrInboundOverflow. Zero disables
	// the limit.
	MaxInboundBuffered int

	// OnOpen fires after registration. Lifecycle and data callbacks are
	// serialized per connection, while callbacks for different connections may
	// run concurrently.
	OnOpen func(c Conn)

	// OnData fires when inbound data is available. Inbound access methods are
	// valid only for this callback invocation.
	OnData func(c Conn) error

	// OnClose is the final callback for a connection and never overlaps its
	// OnOpen or OnData callback.
	OnClose func(c Conn, err error)

	// OnInbound reports bytes read from the socket before OnData and shares its
	// inbound-access scope.
	OnInbound func(c Conn, readBytes int)

	// OnOutbound reports bytes successfully written to the socket. It may run on
	// a backend writer goroutine and does not grant inbound-buffer access.
	OnOutbound func(c Conn, writeBytes int)

	// OnStart runs synchronously after initialization and before the master
	// listener loop begins polling.
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

	// Serve the listener loop on the caller goroutine.
	err = ev.master.Serve(ev.LockOSThread, ev.acceptor)
	ev.waitGroup.Done()
	ev.initiateClose(err)
	ev.waitGroup.Wait()
	if ev.ioPool != nil {
		ev.ioPool.stop()
	}
	ev.callbackWG.Wait()
	return err
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
	// Publish the master loop before initEvents unlocks so Close cannot observe
	// a successfully initialized Events with an incomplete wait group.
	ev.waitGroup.Add(1)
	ev.ready.Store(true)

	return nil
}

// initiateClose seals every loop queue after publishing closing. It never waits
// for callbacks or workers, so it is safe from every callback context.
func (ev *Events) initiateClose(err error) {
	ev.mux.Lock()
	done := ev.ensureDoneLocked()
	// Publish closing before sealing queues so producers reject new work.
	if !ev.closing.CompareAndSwap(false, true) {
		ev.mux.Unlock()
		return
	}
	ev.recordCloseReason(err)
	ev.ready.Store(false)
	if ev.master != nil {
		ev.master.beginStop(err)
	}
	for _, worker := range ev.workers {
		if worker != nil {
			worker.beginStop(err)
		}
	}
	idle := ev.master == nil && len(ev.workers) == 0
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

func (ev *Events) rollbackInit(err error) {
	// Workers may already be serving even though listener setup failed.
	ev.closing.Store(true)
	ev.ready.Store(false)
	if ev.acceptor != nil {
		ev.acceptor.close()
	}
	for _, worker := range ev.workers {
		if worker != nil {
			worker.beginStop(err)
		}
	}
	if ev.master != nil {
		_ = ev.master.poller.Close(err)
	}
	ev.waitGroup.Wait()
	if ev.ioPool != nil {
		ev.ioPool.stop()
	}
	ev.callbackWG.Wait()
}

func (ev *Events) initConfig() error {

	if ev.Pollers <= 0 {
		ev.Pollers = 4
	}
	ev.Pollers = min(ev.Pollers, runtime.NumCPU())

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

// initLoops creates one shared connection scheduler, a master listener loop,
// and Pollers worker loops. Startup rollback closes every successfully created
// poller before returning an error.
func (ev *Events) initLoops() (err error) {
	// Native Unix always uses connection tasks. An injected Executor owns
	// scheduling when present; otherwise UIO creates its default taskgo queue.
	ev.ioPool = newIOTaskPool(ev.Executor)

	// create main loop
	if ev.master, err = newEventLoop(ev); nil != err {
		return err
	}

	ev.workers = make([]*eventLoop, ev.Pollers)
	for idx := range ev.workers {
		if ev.workers[idx], err = newEventLoop(ev); nil != err {
			_ = ev.master.poller.Close(err)
			for _, worker := range ev.workers[:idx] {
				_ = worker.poller.Close(err)
			}
			return err
		}
	}

	for _, worker := range ev.workers {
		ev.waitGroup.Add(1)

		go func(worker *eventLoop) {
			serveErr := worker.Serve(ev.LockOSThread, nil)
			// rollbackInit may hold ev.mux while waiting for this worker.
			ev.waitGroup.Done()
			if serveErr != nil {
				ev.initiateClose(serveErr)
			}
		}(worker)
	}

	return nil
}

func (ev *Events) initListeners(addrs []string) (err error) {

	ev.acceptor = &acceptor{
		loop:   ev.master,
		events: ev,
	}

	for _, addr := range addrs {
		if addr == "" {
			continue
		}
		if err = ev.acceptor.addListen(addr); nil != err {
			return err
		}
	}

	return nil
}

func (ev *Events) selectLoop(fd int) *eventLoop {
	return ev.selectWorker(fd)
}

func (ev *Events) selectWorker(fd int) *eventLoop {
	if len(ev.workers) == 0 {
		return nil
	}
	return ev.workers[fd%len(ev.workers)]
}

func (ev *Events) addConn(fdc *fdConn) error {
	return ev.addConnContext(nil, fdc)
}

const (
	registerPending uint32 = iota
	registerCanceled
	registerCompleted
)

// registerRequest coordinates DialContext cancellation with loop registration.
// The CAS winner decides whether the new fd is returned or closed.
type registerRequest struct {
	ctx   context.Context
	state atomic.Uint32
}

func (request *registerRequest) cause() error {
	if cause := context.Cause(request.ctx); cause != nil {
		return cause
	}
	return context.Canceled
}

// addConnContext synchronously waits for loop registration while allowing the
// caller's context to cancel. Cancellation never returns ownership: a request
// already executing on the loop closes the connection when it observes the
// canceled state.
func (ev *Events) addConnContext(ctx context.Context, fdc *fdConn) error {
	if fdc.loop == nil || ev.closing.Load() {
		fdc.closeUnregistered()
		return net.ErrClosed
	}
	if ctx != nil {
		if cause := context.Cause(ctx); cause != nil {
			fdc.closeUnregistered()
			return cause
		}
	}
	if fdc.loop.inLoop() {
		return fdc.loop.registerConn(fdc)
	}
	// External Dial returns after registration. Native OnOpen runs in the
	// connection task; stdio invokes it synchronously during registration.
	t := acquireTask(registerTask, fdc)
	t.done = make(chan error, 1)
	done := t.done
	var request *registerRequest
	if ctx != nil {
		request = &registerRequest{ctx: ctx}
		t.registration = request
	}
	if !fdc.loop.submitTask(t) {
		releaseTask(t)
		fdc.closeUnregistered()
		if ctx != nil {
			if cause := context.Cause(ctx); cause != nil {
				return cause
			}
		}
		return net.ErrClosed
	}
	if request == nil {
		return <-done
	}
	select {
	case result := <-done:
		return result
	case <-ctx.Done():
		if request.state.CompareAndSwap(registerPending, registerCanceled) {
			return request.cause()
		}
		return <-done
	}
}

func (ev *Events) closeConn(fdc *fdConn, err error) {
	fdc.requestClose(err)
}

func (ev *Events) submitAccepted(fdc *fdConn, tcp bool) bool {
	if fdc.loop == nil || ev.closing.Load() {
		fdc.closeUnregistered()
		return false
	}
	// The listener loop never waits for a worker's OnOpen callback.
	t := acquireTask(registerTask, fdc)
	t.acceptedTCP = tcp
	if !fdc.loop.submitTask(t) {
		releaseTask(t)
		fdc.closeUnregistered()
		return false
	}
	return true
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
