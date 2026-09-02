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

type CompositeBuffer = bytebuf.CompositeBuffer

type Events struct {
	master        *eventLoop     // serving listener
	workers       []*eventLoop   // serving connection
	acceptor      *acceptor      // connection acceptor
	waitGroup     sync.WaitGroup // wait for all eventLoop exit on shutdown
	mux           sync.Mutex     // serializes initialization and shutdown publication
	closing       atomic.Bool
	ready         atomic.Bool  // Dial is allowed only after full initialization
	startGoid     atomic.Int64 // lets Close detect the synchronous OnStart path
	callbackGoids sync.Map     // std callback goroutines currently outside loops

	// Pollers is set up to start the given number of event-loop goroutine.
	// The default value is runtime.NumCPU().
	Pollers int

	// Addrs is the listening addr list for a server.
	Addrs []string

	// ReusePort indicates whether to set up the SO_REUSEPORT socket option.
	// The default value is false.
	ReusePort bool

	// LockOSThread is used to determine whether each I/O event-loop is associated to an OS thread.
	// The default value is false.
	LockOSThread bool

	// MaxBufferSize is the maximum number of bytes that can be read from the remote when the readable event comes.
	// The default value is 4KB.
	MaxBufferSize int

	// WriteBufferedThreshold enabled when value is greater than 0, writes will go into the outbound buffer instead of attempting to send them out immediately,
	// unless the outbound buffer reaches the threshold or the flush function is manually called.
	//
	// If you have multiple Write call requirements, opening it will improve write performance because it reduces the number of system calls by merging multiple write operations to improve performance.
	// The default value is 0.
	WriteBufferedThreshold int

	// MaxOutboundBuffered limits accepted but unsent payload bytes per
	// connection. Zero disables the byte limit.
	MaxOutboundBuffered int

	// MaxPendingWrites limits write tasks not yet consumed by a connection's
	// event loop. Values <= 0 use a default of 1024.
	MaxPendingWrites int

	// MaxInboundBuffered limits unread payload retained per connection. Zero
	// disables the limit.
	MaxInboundBuffered int

	// OnOpen fires when a new connection has been opened.
	OnOpen func(c Conn)

	// OnData fires when a socket receives data from the remote.
	OnData func(c Conn) error

	// OnClose fires when a connection has been closed.
	OnClose func(c Conn, err error)

	// OnInbound when any bytes read by a socket, it triggers the inbound event.
	OnInbound func(c Conn, readBytes int)

	// OnOutbound when any bytes write to a socket, it triggers the outbound event.
	OnOutbound func(c Conn, writeBytes int)

	// OnStart it triggers on the server initialized.
	OnStart func(ev *Events)

	// OnStop it triggers on the server closed.
	OnStop func(ev *Events)
}

func (ev *Events) Serve(addrs ...string) (err error) {
	if ev.closing.Load() {
		return net.ErrClosed
	}
	// append listen address.
	ev.Addrs = append(ev.Addrs, addrs...)

	// initialize events
	if err = ev.initEvents(); nil != err {
		return err
	}

	// OnStart runs before master.Serve, but Close must still avoid waiting on
	// the goroutine that will become the master loop.
	ev.startGoid.Store(currentGoroutineID())
	if ev.OnStart != nil {
		ev.OnStart(ev)
	}
	ev.startGoid.Store(0)

	defer func() {
		// trigger OnStop event.
		if ev.OnStop != nil {
			ev.OnStop(ev)
		}
	}()

	// Serve the listener loop on the caller goroutine.
	err = ev.master.Serve(ev.LockOSThread, ev.acceptor)
	ev.waitGroup.Done()
	ev.initiateClose(err)
	ev.waitGroup.Wait()
	return err
}

func (ev *Events) Close(err error) error {
	onLoop := ev.initiateClose(err)
	// Waiting from a callback would make that callback wait for itself.
	if !onLoop {
		ev.waitGroup.Wait()
	}
	return nil
}

func (ev *Events) initEvents() (err error) {

	ev.mux.Lock()
	defer ev.mux.Unlock()
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
	if err = ev.initListeners(); nil != err {
		ev.rollbackInit(err)
		return err
	}
	// Publish the master loop before initEvents unlocks so Close cannot observe
	// a successfully initialized Events with an incomplete wait group.
	ev.waitGroup.Add(1)
	ev.ready.Store(true)

	return nil
}

func (ev *Events) initiateClose(err error) bool {
	ev.mux.Lock()
	defer ev.mux.Unlock()
	callerGoid := currentGoroutineID()
	_, inExternalCallback := ev.callbackGoids.Load(callerGoid)
	onLoop := ev.currentLoop() != nil || ev.startGoid.Load() == callerGoid || inExternalCallback
	// Publish closing before sealing queues so producers reject new work.
	if !ev.closing.CompareAndSwap(false, true) {
		return onLoop
	}
	ev.ready.Store(false)
	if ev.master != nil {
		ev.master.beginStop(err)
	}
	for _, worker := range ev.workers {
		if worker != nil {
			worker.beginStop(err)
		}
	}
	return onLoop
}

func (ev *Events) enterExternalCallback() int64 {
	id := currentGoroutineID()
	ev.callbackGoids.Store(id, struct{}{})
	return id
}

func (ev *Events) leaveExternalCallback(id int64) {
	ev.callbackGoids.Delete(id)
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
}

func (ev *Events) initConfig() error {

	if ev.Pollers <= 0 || ev.Pollers > runtime.NumCPU() {
		ev.Pollers = runtime.NumCPU()
	}

	if ev.MaxBufferSize <= 0 {
		ev.MaxBufferSize = 1024 * 4
	}

	if ev.MaxPendingWrites <= 0 {
		ev.MaxPendingWrites = 1024
	}

	return nil
}

func (ev *Events) initLoops() (err error) {

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

func (ev *Events) initListeners() (err error) {

	ev.acceptor = &acceptor{
		loop:   ev.master,
		events: ev,
	}

	for _, addr := range ev.Addrs {
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
	// External Dial returns only after registration and OnOpen complete.
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

func (ev *Events) submitAccepted(fdc *fdConn) bool {
	if fdc.loop == nil || ev.closing.Load() {
		fdc.closeUnregistered()
		return false
	}
	// The listener loop never waits for a worker's OnOpen callback.
	t := acquireTask(registerTask, fdc)
	if !fdc.loop.submitTask(t) {
		releaseTask(t)
		fdc.closeUnregistered()
		return false
	}
	return true
}

func (ev *Events) currentLoop() *eventLoop {
	if ev.master != nil && ev.master.inLoop() {
		return ev.master
	}
	for _, worker := range ev.workers {
		if worker != nil && worker.inLoop() {
			return worker
		}
	}
	return nil
}

func (ev *Events) onData(fdc *fdConn) error {
	if nil != ev.OnData {
		return ev.OnData(fdc)
	}
	// discard all received bytes if not set OnData.
	//
	_, _ = fdc.Discard(-1)
	return nil
}

func (ev *Events) onSocketBytesRead(fdc *fdConn, readBytes int) {
	if readBytes > 0 && ev.OnInbound != nil {
		ev.OnInbound(fdc, readBytes)
	}
}

func (ev *Events) onSocketBytesWrite(fdc *fdConn, writeBytes int) {
	if writeBytes > 0 && ev.OnOutbound != nil {
		ev.OnOutbound(fdc, writeBytes)
	}
}
