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
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
)

// Dial connects to the address on the named network.
//
// Known networks are "tcp", "tcp4" (IPv4-only), "tcp6" (IPv6-only),
// "udp", "udp4" (IPv4-only), "udp6" (IPv6-only), "ip", "ip4"
// (IPv4-only), "ip6" (IPv6-only), "unix", "unixgram" and
// "unixpacket".
//
// Examples:
//
//	Dial("tcp://golang.org:http")
//	Dial("tcp://192.0.2.1:http")
//	Dial("tcp://198.51.100.1:80")
//	Dial("udp://[2001:db8::1]:domain")
//	Dial("udp://[fe80::1%lo0]:53")
//	Dial("tcp://:80")
//	Dial("unix:///path/your/unix.sock")
func (ev *Events) Dial(addr string, userdata any) (Conn, error) {
	return ev.DialContext(context.Background(), addr, userdata)
}

// Adopt transfers ownership of an established stream connection to ev. The
// caller must not use conn after calling Adopt, including when Adopt returns an
// error. ev must already be serving.
func (ev *Events) Adopt(conn net.Conn, userdata any) (Conn, error) {
	if conn == nil {
		return nil, errUnsupported
	}
	if !ev.ready.Load() || ev.closing.Load() {
		_ = conn.Close()
		return nil, net.ErrClosed
	}
	if _, ok := conn.(syscall.Conn); !ok {
		_ = conn.Close()
		return nil, errUnsupported
	}

	fdc := &fdConn{
		commonConn: commonConn{
			events: ev,
			addr:   &addrPair{local: conn.LocalAddr()},
		},
		conn:     conn,
		writeSig: make(chan struct{}, 1),
	}
	fdc.setRemoteAddr(conn.RemoteAddr())
	fdc.SetUserdata(userdata)
	fd := fdc.Fd()
	if fd < 0 {
		fdc.closeUnregistered()
		return nil, errUnsupported
	}
	fdc.loop = ev.selectLoop(fd)
	if err := ev.addConn(fdc); err != nil {
		return nil, err
	}
	return fdc, nil
}

// DialContext connects synchronously and allows cancellation while resolving
// or establishing the network connection. Calling it from a connection
// callback blocks that callback until dialing completes.
func (ev *Events) DialContext(dialCtx context.Context, addr string, userdata any) (Conn, error) {
	if !ev.ready.Load() || ev.closing.Load() {
		return nil, net.ErrClosed
	}
	if isEventLoopGoroutine() {
		return nil, ErrDialOnEventLoop
	}

	if !strings.Contains(addr, "://") {
		addr = "tcp://" + addr
	}

	u, err := url.Parse(addr)
	if nil != err {
		return nil, err
	}

	var address = u.Host
	if strings.HasPrefix(u.Scheme, "unix") {
		address = u.Path
	}

	conn, err := (&net.Dialer{}).DialContext(dialCtx, u.Scheme, address)
	if nil != err {
		return nil, err
	}

	lAddr := conn.LocalAddr()
	rAddr := conn.RemoteAddr()

	fdc := &fdConn{}

	if udpConn, ok := conn.(*net.UDPConn); ok {
		fdc.udp = &stdUDPState{sock: udpConn}
	} else {
		fdc.conn = conn
	}

	fdc.SetUserdata(userdata)
	fdc.setLocalAddr(lAddr)
	fdc.setRemoteAddr(rAddr)
	fdc.events = ev
	fdc.loop = ev.selectLoop(fdc.Fd())
	fdc.writeSig = make(chan struct{}, 1)

	if err = ev.addConnContext(dialCtx, fdc); nil != err {
		return nil, err
	}
	return fdc, nil
}

// loopState is this backend's share of Events: a master loop that serves the
// listeners on Serve's goroutine and Pollers worker loops that own
// registration and close for their connections.
type loopState struct {
	master    *eventLoop     // serving listener
	workers   []*eventLoop   // serving connection
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
	err := ev.master.Serve(ev.LockOSThread, ev.acceptor)
	ev.waitGroup.Done()
	ev.initiateClose(err)
	ev.waitGroup.Wait()
	ev.stopIOPool()
	ev.callbackWG.Wait()
	return err
}

// stopIOPool joins every connection turn the loops counted, including close
// callbacks scheduled while they shut down, and then stops the scheduler.
func (ev *Events) stopIOPool() {
	if ev.ioPool == nil {
		return
	}
	if ev.master != nil {
		ev.master.waitIODrained()
	}
	for _, worker := range ev.workers {
		if worker != nil {
			worker.waitIODrained()
		}
	}
	ev.ioPool.stop()
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
	ev.stopIOPool()
	ev.callbackWG.Wait()
}

// initLoops creates one shared connection scheduler, a master listener loop,
// and Pollers worker loops. Startup rollback closes every successfully created
// poller before returning an error.
func (ev *Events) initLoops() (err error) {
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
