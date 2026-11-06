//go:build windows || stdio

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
	"sync"
	"sync/atomic"
)

func (ev *Events) onRead(fdc *fdConn) error {
	skipRead, err := fdc.fireWakeTimer()
	if err != nil {
		return err
	}
	if skipRead {
		return nil // the timer callback closed: no read callback follows
	}
	if fdc.entry.directConsumer && fdc.ReadPaused() {
		return nil
	}
	fdc.consumerStarts()
	if fdc.entry.directConsumer && fdc.hasPullReader() {
		started := fdc.beginInboundCallback()
		fdc.pumpBlockReader()
		fdc.endInboundCallback(started)
		return nil
	}
	if onRead := fdc.callbacks().onRead; onRead != nil {
		return onRead(fdc)
	}
	started := fdc.beginInboundCallback()
	_, _ = fdc.Discard(-1)
	fdc.endInboundCallback(started)
	return nil
}

func (ev *Events) newAdoptedConn(conn net.Conn) (*fdConn, error) {
	fdc := &fdConn{
		commonConn: commonConn{addr: &addrPair{local: conn.LocalAddr()}},
		conn:       conn,
		writeSig:   make(chan struct{}, 1),
		readResume: make(chan struct{}, 1),
	}
	fdc.setRemoteAddr(conn.RemoteAddr())
	return fdc, nil
}

func (ev *Events) newDialedConn(_ string, conn net.Conn) (*fdConn, error) {
	fdc := &fdConn{writeSig: make(chan struct{}, 1)}
	if udpConn, ok := conn.(*net.UDPConn); ok {
		fdc.udp = &stdUDPState{sock: udpConn}
	} else {
		fdc.conn = conn
		fdc.readResume = make(chan struct{}, 1)
	}
	fdc.setLocalAddr(conn.LocalAddr())
	fdc.setRemoteAddr(conn.RemoteAddr())
	return fdc, nil
}

// loopState is this backend's share of Events: a master loop that serves the
// listeners on Serve's goroutine and Pollers worker loops that own
// registration and close for their connections.
type loopState struct {
	master    *eventLoop     // serving listener
	workers   []*eventLoop   // serving connection
	nextLoop  atomic.Uint64  // round-robin worker selector
	waitGroup sync.WaitGroup // wait for all eventLoop exit on shutdown
}

// defaultPollers is one worker loop per four Ps, at least two.
func defaultPollers(procs int) int { return max(2, procs/4) }

// publishLoops counts the master loop before initEvents unlocks, so Close
// cannot observe a successfully initialized Events with an incomplete wait
// group.
func (ev *Events) publishLoops() { ev.waitGroup.Add(1) }

// stopLoopsLocked seals every loop queue. It reports whether any loop exists.
func (ev *Events) stopLoopsLocked(err error) bool {
	if ev.master != nil {
		ev.master.beginStop(err)
	}
	for _, worker := range ev.workers {
		if worker != nil {
			worker.beginStop(err)
		}
	}
	return ev.master != nil || len(ev.workers) != 0
}

// serveLoops serves the listener loop on Serve's goroutine and then joins
// every loop, connection task and I/O goroutine.
func (ev *Events) serveLoops() error {
	// Listeners were bound before OnStart; accepting starts only now, once
	// it has returned, so no callback runs while the application initializes.
	startErr := ev.acceptor.startAccepting()
	err := ev.master.Serve(ev.LockOSThread, func(error) { ev.acceptor.close() })
	ev.waitGroup.Done()
	ev.initiateClose(err)
	if startErr != nil {
		err = startErr
		ev.recordCloseReason(err)
	}
	ev.waitGroup.Wait()
	ev.callbackWG.Wait()
	return err
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
		ev.master.closeLoop(err)
	}
	ev.waitGroup.Wait()
	ev.callbackWG.Wait()
}

// initLoops creates a master listener loop and Pollers worker loops.
func (ev *Events) initLoops() (err error) {
	if ev.master, err = newEventLoop(ev); nil != err {
		return err
	}

	ev.workers = make([]*eventLoop, ev.Pollers)
	for idx := range ev.workers {
		if ev.workers[idx], err = newEventLoop(ev); nil != err {
			ev.master.closeLoop(err)
			for _, worker := range ev.workers[:idx] {
				worker.closeLoop(err)
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

func (ev *Events) selectLoop() *eventLoop {
	return ev.selectWorker()
}

func (ev *Events) selectWorker() *eventLoop {
	if len(ev.workers) == 0 {
		return nil
	}
	idx := ev.nextLoop.Add(1) - 1
	return ev.workers[idx%uint64(len(ev.workers))]
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
	// External Dial returns after registration. OnOpen runs afterwards: in
	// the connection task natively, first on the read goroutine here.
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

// submitAccepted hands an accepted connection to its worker loop for
// registration and returns at once. A task the loop accepted runs before the
// loop stops, so the connection is registered or closed either way.
func (ev *Events) submitAccepted(fdc *fdConn) bool {
	if fdc.loop == nil || ev.closing.Load() {
		fdc.closeUnregistered()
		return false
	}
	t := acquireTask(registerTask, fdc)
	if !fdc.loop.submitTask(t) {
		releaseTask(t)
		fdc.closeUnregistered()
		return false
	}
	return true
}
