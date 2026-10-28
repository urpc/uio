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
	"sync"
	"sync/atomic"
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
func (ev *Events) Dial(addr string, userdata any, opts ...DialOption) (Conn, error) {
	return ev.defaultDialer().Dial(addr, userdata, opts...)
}

// Adopt transfers ownership of an established stream connection to ev. The
// caller must not use conn after calling Adopt, including when Adopt returns an
// error. ev must already be serving. With layers installed it returns as
// DialContext does: once the whole stack opened, with the top layer's
// connection — the one the Handler holds — and a stack that fails before
// that reports its error; see AdoptContext to bound the wait.
func (ev *Events) Adopt(conn net.Conn, userdata any, opts ...AdoptOption) (Conn, error) {
	return ev.AdoptContext(context.Background(), conn, userdata, opts...)
}

// AdoptContext is Adopt with ctx bounding the wait for the layers to open; a
// stack still opening when ctx ends is closed, and AdoptContext reports the
// cause. Without layers there is nothing to wait for. With them, like
// DialContext, it cannot be called on an event loop goroutine.
func (ev *Events) AdoptContext(ctx context.Context, conn net.Conn, userdata any, opts ...AdoptOption) (Conn, error) {
	if conn == nil {
		return nil, ErrUnsupported
	}
	if !ev.ready.Load() || ev.closing.Load() {
		_ = conn.Close()
		return nil, net.ErrClosed
	}
	cfg := adoptConfigFor(opts)
	layered := cfg.hasLayers
	if layered && isEventLoopGoroutine() {
		_ = conn.Close()
		return nil, ErrDialOnEventLoop
	}
	if err := refuseLayerPacket(conn.LocalAddr().Network(), layered); err != nil {
		_ = conn.Close()
		return nil, err
	}
	entry, err := ev.entryFor(cfg.layers, cfg.hasLayers)
	if err != nil {
		_ = conn.Close()
		return nil, err
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
			addr: &addrPair{local: localAddr},
		},
		fd: int32(fd),
	}
	fdc.setRemoteAddr(remoteAddr)
	fdc.entry = entry
	probe, marked := ev.dialMarker(userdata, entry.layered)
	fdc.SetUserdata(marked)
	fdc.loop = ev.selectLoop()
	if err = ev.addConnContext(ctx, fdc); err != nil {
		return nil, err
	}
	if probe == nil {
		return fdc, nil
	}
	return ev.awaitDial(ctx, fdc, probe)
}

// DialContext connects synchronously and allows cancellation while resolving
// or establishing the connection. The connection is registered on the calling
// goroutine; with layers installed it returns once the whole stack opened,
// which waits for that connection's turns, so a call from a connection
// callback waits for the dial and its open stage — never call it from an
// event loop goroutine.
// A connection it returns gets OnOpen and, at its end, OnClose, even if it is
// closed first; after an error no callback runs for it.
func (ev *Events) DialContext(dialCtx context.Context, addr string, userdata any, opts ...DialOption) (Conn, error) {
	return ev.defaultDialer().DialContext(dialCtx, addr, userdata, opts...)
}

// Dialer dials through one Events with private defaults — the options it was
// built with, a chain among them — which each Dial's own options override: a
// Dial naming its chain (WithLayers) runs that chain for its connection alone,
// so one Dialer serves ws:// and wss:// dials alike. A Listener, by contrast,
// carries its one chain for its life. Events.Dial is the Events' default
// Dialer's.
type Dialer struct {
	ev  *Events
	cfg DialConfig

	entryOnce sync.Once
	entry     *entry
	entryErr  error
}

// Dialer returns a Dialer of ev with opts as its defaults: WithLayers names
// the chain every dial of it runs unless the dial names its own. It fails on
// a chain a connection cannot carry; a chain's layers are attached when a
// dial runs.
func (ev *Events) Dialer(opts ...DialOption) (*Dialer, error) {
	cfg := dialConfigFor(opts)
	if err := validateChain(cfg.layers, cfg.hasLayers); err != nil {
		return nil, err
	}
	return &Dialer{ev: ev, cfg: cfg}, nil
}

// defaultDialer is the Dialer the Events' own Dial and DialContext dial
// through: no defaults, every dial naming its own options.
func (ev *Events) defaultDialer() *Dialer { return &Dialer{ev: ev} }

// dialerEntry resolves the defaults' entry once: the Events' plain one, or
// the one the default chain builds. A dial that names its own chain builds
// its own.
func (d *Dialer) dialerEntry() (*entry, error) {
	if !d.cfg.hasLayers {
		return d.ev.plainEntry(), nil
	}
	d.entryOnce.Do(func() { d.entry, d.entryErr = d.ev.newEntry(d.cfg.layers) })
	return d.entry, d.entryErr
}

// Dial dials addr with this Dialer; see DialContext.
func (d *Dialer) Dial(addr string, userdata any, opts ...DialOption) (Conn, error) {
	return d.DialContext(context.Background(), addr, userdata, opts...)
}

// DialContext connects synchronously through this Dialer's Events; see
// Events.DialContext.
func (d *Dialer) DialContext(dialCtx context.Context, addr string, userdata any, opts ...DialOption) (Conn, error) {
	ev := d.ev
	config := d.cfg
	callLayers := false
	for _, opt := range opts {
		if opt == nil {
			continue
		}
		if _, ok := opt.(LayersOption); ok {
			callLayers = true
		}
		opt.applyDial(&config)
	}
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
	if err := refuseLayerPacket(u.Scheme, callLayers || d.cfg.hasLayers); err != nil {
		return nil, err
	}
	var entry *entry
	if callLayers {
		entry, err = ev.newEntry(config.layers)
	} else {
		entry, err = d.dialerEntry()
	}
	if err != nil {
		return nil, err
	}

	var address = u.Host
	if strings.HasPrefix(u.Scheme, "unix") {
		address = u.Path
	}

	dialer, err := netDialerFor(u.Scheme, config)
	if err != nil {
		return nil, err
	}
	conn, err := dialer.DialContext(dialCtx, u.Scheme, address)
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

	probe, marked := ev.dialMarker(userdata, entry.layered)
	fdc := &fdConn{}
	fdc.entry = entry
	fdc.fd = int32(nfd)
	fdc.SetUserdata(marked)
	fdc.setLocalAddr(lAddr)
	fdc.setRemoteAddr(rAddr)
	fdc.loop = ev.selectLoop()
	if strings.HasPrefix(u.Scheme, "udp") {
		fdc.udp = &unixUDPState{}
	}

	if err = ev.addConnContext(dialCtx, fdc); nil != err {
		return nil, err
	}
	if probe == nil {
		return fdc, nil
	}
	return ev.awaitDial(dialCtx, fdc, probe)
}

// loopState is this backend's share of Events: the event loops, each one
// poller and the goroutines that wait on it, and the signal that ends Serve.
type loopState struct {
	loops    []*eventLoop
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
