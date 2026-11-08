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
	"runtime"
	"strings"
	"sync/atomic"
	"syscall"

	"github.com/urpc/uio/internal/socket"
)

func detachNetConn(conn net.Conn) (fd int, local, remote net.Addr, err error) {
	local, remote = conn.LocalAddr(), conn.RemoteAddr()
	fd, err = socket.DupNetConn(conn)
	_ = conn.Close()
	if err != nil {
		return 0, nil, nil, err
	}
	if err = socket.SetNonblock(fd, true); err != nil {
		_ = syscall.Close(fd)
		return 0, nil, nil, err
	}
	return fd, local, remote, nil
}

func (ev *Events) newAdoptedConn(conn net.Conn) (*fdConn, error) {
	fd, local, remote, err := detachNetConn(conn)
	if err != nil {
		return nil, err
	}
	fdc := &fdConn{
		commonConn: commonConn{addr: &addrPair{local: local}},
		fd:         int32(fd),
	}
	fdc.setRemoteAddr(remote)
	return fdc, nil
}

func (ev *Events) newDialedConn(scheme string, conn net.Conn) (*fdConn, error) {
	fd, local, remote, err := detachNetConn(conn)
	if err != nil {
		return nil, err
	}
	fdc := &fdConn{fd: int32(fd)}
	fdc.setLocalAddr(local)
	fdc.setRemoteAddr(remote)
	if strings.HasPrefix(scheme, "udp") {
		fdc.udp = &unixUDPState{groEnabled: socket.EnableGRO(fd)}
	}
	return fdc, nil
}

// loopState is this backend's share of Events: the event loops, each one
// poller and the goroutines that wait on it, and the signal that ends Serve.
type loopState struct {
	loops    []*eventLoop
	ioPool   *ioTaskPool
	stopped  chan struct{} // closed once Close has been requested
	nextLoop atomic.Uint32 // picks the loops of connections not accepted by a waiter
}

// initLoops creates the shared connection scheduler and Pollers loops, and
// starts their waiters, so connections dialed during OnStart are already
// watched. The loops accept nothing until serveLoops registers the listeners.
// Startup rollback closes every successfully created poller before returning
// an error.
func (ev *Events) initLoops() (err error) {
	// An injected Executor owns scheduling when present; otherwise UIO creates
	// its own scheduler, shared by every loop. It keys a connection's turns by
	// descriptor, not by loop: the loops exist to spread event collection, and
	// a queue per loop measured slower than a shared one on a 64-core host.
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
	for idx, loop := range ev.loops {
		loop.index, loop.count = uint32(idx), uint32(len(ev.loops))
	}
	procs := runtime.GOMAXPROCS(0)
	waiters := loopWaiters(procs, len(ev.loops))
	mode := loopWaitMode(procs, len(ev.loops), waiters)
	for _, loop := range ev.loops {
		loop.start(waiters, mode, ev.LockOSThread)
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
	// A UDP socket closes off the loop, once the syscalls on it ended; Serve
	// returns with its address free. A Control still running on it holds
	// this until it returns, as it holds Listener.Close.
	ev.socketCloses.wait()
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

// selectLoop picks the loop a dialed, adopted or UDP connection registers
// with; see loopAt.
func (ev *Events) selectLoop() *eventLoop {
	if len(ev.loops) == 0 {
		return nil
	}
	return ev.loopAt(ev.nextLoop.Add(1))
}

// loopAt is the loop of the nth pick. Connections go round the loops in turn
// rather than by descriptor number: descriptors are handed out lowest first,
// so a server that opens a few of them per connection, as a proxy pairs each
// client with an upstream, would put every connection of one kind on the
// same few loops. A loop's connections share its poller, and with it the
// waiter that collects their readiness and the lock their registrations and
// closes take.
func (ev *Events) loopAt(n uint32) *eventLoop {
	return ev.loops[n%uint32(len(ev.loops))]
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

func (ev *Events) collectStats() Stats {
	var st Stats
	loops := ev.loops
	st.Pollers = len(loops)
	for _, loop := range loops {
		if loop == nil {
			continue
		}
		st.ActiveConnections += loop.stats.activeConns.Load()
		st.TotalConnections += loop.stats.totalConns.Load()
		st.OutboundOverflows += loop.stats.outboundOverflows.Load()
	}
	st.ActiveConnections = max(st.ActiveConnections, 0)
	if pool := ev.ioPool; pool != nil {
		if pool.turns != nil {
			ts := pool.turns.Stats()
			st.TurnQueue = ts.Queued
			st.ActiveTurnShards = ts.ActiveShards
			st.RunningWorkers += ts.RunningWorkers
			st.IdleWorkers += ts.IdleWorkers
		}
		if pool.writes != nil {
			ws := pool.writes.Stats()
			st.WriteQueue = ws.Queued
			st.RunningWorkers += ws.RunningWorkers
			st.IdleWorkers += ws.IdleWorkers
		}
	}
	return st
}
