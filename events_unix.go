//go:build (linux || darwin || netbsd || freebsd || openbsd || dragonfly) && !stdio

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
	"runtime"
	"strings"
	"syscall"

	"github.com/urpc/uio/internal/socket"
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
	localAddr := conn.LocalAddr()
	remoteAddr := conn.RemoteAddr()

	// Detach the socket from net.Conn before giving its duplicate to the native
	// poller. DupNetConn marks the new descriptor close-on-exec.
	fd, err := socket.DupNetConn(conn)
	_ = conn.Close()
	if err != nil {
		return nil, err
	}
	if err = socket.SetNonblock(fd, true); err != nil {
		_ = syscall.Close(fd)
		return nil, err
	}

	fdc := &fdConn{
		commonConn: commonConn{
			events: ev,
			addr:   &addrPair{local: localAddr},
		},
		fd: fd,
	}
	fdc.setRemoteAddr(remoteAddr)
	fdc.SetUserdata(userdata)
	fdc.loop = ev.selectLoop(fd)
	if err = ev.addConn(fdc); err != nil {
		return nil, err
	}
	return fdc, nil
}

// DialContext connects synchronously and allows cancellation while resolving
// or establishing the connection. The connection is registered on the calling
// goroutine, so calling it from a connection callback waits for nothing but
// the dial itself.
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

	// Dup detaches the descriptor from net.Conn so the poller becomes its sole
	// I/O owner after the original connection is closed.
	nfd, err := socket.DupNetConn(conn)

	_ = conn.Close()

	if nil != err {
		return nil, err // dup failed
	}

	if err = syscall.SetNonblock(nfd, true); nil != err {
		_ = syscall.Close(nfd)
		return nil, err
	}

	fdc := &fdConn{}
	fdc.fd = nfd
	fdc.SetUserdata(userdata)
	fdc.setLocalAddr(lAddr)
	fdc.setRemoteAddr(rAddr)
	fdc.events = ev
	fdc.loop = ev.selectLoop(nfd)
	if strings.HasPrefix(u.Scheme, "udp") {
		fdc.udp = &unixUDPState{}
	}

	if err = ev.addConnContext(dialCtx, fdc); nil != err {
		return nil, err
	}
	return fdc, nil
}

// loopState is this backend's share of Events: the event loops, each one
// poller and the goroutines that wait on it, and the signal that ends Serve.
type loopState struct {
	loops   []*eventLoop
	stopped chan struct{} // closed once Close has been requested
}

// initLoops creates the shared connection scheduler and Pollers loops, and
// starts their waiters, so connections dialed during OnStart are already
// watched. The loops accept nothing until serveLoops registers the listeners.
// Startup rollback closes every successfully created poller before returning
// an error.
func (ev *Events) initLoops() (err error) {
	// An injected Executor owns scheduling when present; otherwise UIO creates
	// its default taskgo queue. The queue stays single: turns from every loop
	// share it, and measurements on a 64-core host showed a shared queue
	// beating one queue per loop by ~4% — the loops exist to spread event
	// collection, not to pin work to cores.
	ev.ioPool = newIOTaskPool(ev.Executor)
	ev.stopped = make(chan struct{})
	ev.loops = make([]*eventLoop, ev.Pollers)
	for idx := range ev.loops {
		if ev.loops[idx], err = newEventLoop(ev); err != nil {
			for _, loop := range ev.loops[:idx] {
				_ = loop.poller.Close(err)
			}
			ev.loops = nil
			ev.ioPool.stop()
			return err
		}
	}
	procs := runtime.GOMAXPROCS(0)
	waiters := loopWaiters(procs, len(ev.loops))
	parked := waiters*len(ev.loops) <= parkedWaiters
	for _, loop := range ev.loops {
		loop.start(waiters, parked, ev.LockOSThread)
	}
	return nil
}

func (ev *Events) initListeners(addrs []string) (err error) {
	ev.acceptor = &acceptor{events: ev}
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

func (ev *Events) publishLoops() {}

// stopLoopsLocked ends serveLoops. It reports whether any loop exists.
func (ev *Events) stopLoopsLocked(error) bool {
	if ev.stopped == nil {
		return false
	}
	select {
	case <-ev.stopped:
	default:
		close(ev.stopped)
	}
	return true
}

// serveLoops starts accepting, waits for Close, and then shuts down.
func (ev *Events) serveLoops() error {
	if err := ev.acceptor.startAccepting(); err != nil {
		ev.initiateClose(err)
	}
	<-ev.stopped
	var err error
	if reason := ev.closeReason.Load(); reason != nil {
		err = *reason
	}
	ev.shutdownLoops(err)
	return err
}

// shutdownLoops stops the loops in the order their users depend on: no loop
// admits a new turn, every waiter returns, so nothing is accepted or
// collected any more, then each loop releases its connections, the listeners
// close, and the scheduler stops once every remaining turn, close callbacks
// included, has returned.
func (ev *Events) shutdownLoops(err error) {
	for _, loop := range ev.loops {
		loop.stopping.Store(true)
	}
	for _, loop := range ev.loops {
		loop.closePoller(err)
	}
	for _, loop := range ev.loops {
		loop.shutdown(err)
	}
	if ev.acceptor != nil {
		ev.acceptor.close()
	}
	ev.stopIOPool()
}

func (ev *Events) rollbackInit(err error) {
	ev.closing.Store(true)
	ev.ready.Store(false)
	ev.shutdownLoops(err)
}

// stopIOPool joins every connection turn the loops counted, including close
// callbacks scheduled while they shut down, and then stops the scheduler.
func (ev *Events) stopIOPool() {
	if ev.ioPool == nil {
		return
	}
	for _, loop := range ev.loops {
		loop.waitIODrained()
	}
	ev.ioPool.stop()
}

func (ev *Events) selectLoop(fd int) *eventLoop {
	if len(ev.loops) == 0 {
		return nil
	}
	return ev.loops[fd%len(ev.loops)]
}

func (ev *Events) addConn(fdc *fdConn) error {
	return ev.addConnContext(nil, fdc)
}

// addConnContext registers fdc on the calling goroutine. A context that is
// already done refuses the connection; once registered, it is the caller's.
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
	return fdc.loop.registerConn(fdc)
}
