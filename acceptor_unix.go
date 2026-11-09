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
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/urpc/uio/internal/socket"
)

// acceptBatchSize bounds how many connections one listener event accepts.
const acceptBatchSize = 64

// listener keeps both the Go listener and the duplicated non-blocking fd used
// by the native poller. Closing it must account for which object owns that fd.
type listener struct {
	network    string         // network protocol
	fd         int            // fd
	addr       string         // address
	tcp        bool           // a TCP stream listener: accepted peers are IP addresses
	pair       *addrPair      // local listen address, shared by accepted connections
	ln         net.Listener   // tcp/unix listener
	file       *os.File       // file
	udp        net.PacketConn // udp endpoint
	groEnabled bool           // udp GRO status
	udpSvr     *fdConn        // udp server
	loop       *eventLoop     // the loop that accepts on a stream listener

	// userdata is every accepted connection's initial Userdata, and
	// userdataBox the boxed value the connections share, so accepting one
	// allocates no box of its own: a connection's SetUserdata stores a new box,
	// so the shared one is never written.
	userdata    any
	userdataBox *any
	// acceptLocal keeps what this listener accepts on its own loop: its
	// ReusePort group gives every loop a listener, the kernel spreads the
	// connections over them, and the connection is registered with the
	// poller of the loop whose runner accepted it. See addGroupLocked.
	acceptLocal bool
	// entry is what every connection this listener accepts runs: its
	// callbacks and chain.
	entry   *entry
	started bool // registered with its loop; guarded by acceptor.mux
	// The gate counts the accepts in progress on the listener. Its loop's
	// accept runner may have looked l up just before it left the loop, so
	// its descriptor is closed only after retire returns: closed earlier, the
	// number could be reused by another socket the runner would then accept
	// on.
	descriptorGate
	// acceptPaused marks a stream listener taken out of its poller after an
	// accept ran out of descriptors or memory, and acceptDelay is the pause
	// that took, zero again once an accept succeeds; see pauseAccept.
	acceptPaused atomic.Bool
	acceptDelay  atomic.Int64
	// acceptState is the listener's hand-over to its loop's acceptRunner:
	// acceptIdle, acceptRunning or acceptAgain.
	acceptState atomic.Int32
	// controlled records a SocketControl run on the listening socket, whose
	// options the accepted sockets inherit; they close in their own turns.
	controlled bool
}

// descriptorGate counts the goroutines using a descriptor, below a closed
// bit. retire closes it to new users and waits for those in progress, so the
// descriptor can close without its number being reused under one of them.
// Nothing a user does while counted may wait for the descriptor's closer.
type descriptorGate struct {
	use atomic.Int64
}

// gateClosed is the bit of descriptorGate.use that retire sets.
const gateClosed = 1 << 62

// enter reports whether the descriptor may still be used, and counts the
// caller until leave.
func (g *descriptorGate) enter() bool {
	for {
		use := g.use.Load()
		if use&gateClosed != 0 {
			return false
		}
		if g.use.CompareAndSwap(use, use+1) {
			return true
		}
	}
}

func (g *descriptorGate) leave() {
	if g != nil {
		g.use.Add(-1)
	}
}

// retire stops new users and waits for the ones in progress. They are
// syscalls, which end at once, or a Control's fn, which may take its time.
func (g *descriptorGate) retire() {
	g.markClosed()
	g.waitDrain(0)
}

// markClosed stops new users without waiting for the ones in progress.
func (g *descriptorGate) markClosed() { g.use.Or(gateClosed) }

// waitDrain waits for every user in progress to leave, at most as long as
// within; a zero within waits without bound. It reports whether they all
// left.
func (g *descriptorGate) waitDrain(within time.Duration) bool {
	deadline := time.Now().Add(within)
	for spins := 0; ; spins++ {
		if g.use.Load() == gateClosed {
			return true
		}
		if within > 0 && time.Now().After(deadline) {
			return false
		}
		if spins < 64 {
			runtime.Gosched()
		} else {
			time.Sleep(50 * time.Microsecond)
		}
	}
}

// acceptor owns the listeners. Stream listeners are spread over the event
// loops, whose accept runners accept on them; a UDP listener is a connection
// of its own, whose turns read every peer's datagrams.
type acceptor struct {
	mux       sync.Mutex
	listeners map[int]*listener
	events    *Events
	next      int  // the loop the next stream listener goes to
	accepting bool // startAccepting has run: a new listener starts at once
	closed    bool // close has run: Listen adds nothing more
}

// listenersOverride fixes the number of ReusePort listeners per address in
// tests; zero means automatic.
var listenersOverride int

// reusePortListeners returns how many listeners share a ReusePort address.
// The kernel spreads connections over the listeners of a ReusePort group, and
// each listener is accepted on by one loop, so several of them spread the
// accepting work — a few microseconds of accept and hand-off per connection —
// over more loops. The count grows with the machine, capped where a
// connection's own accept cost stops being the scarce resource and each
// listener's share would shrink its batches. A wide host gives every loop a
// listener: its loops' waiters each block on a poller of their own, and with
// four listeners over eight loops the accepting four held a connection-churn
// load 7-9% below one listener per loop.
func reusePortListeners(procs, loops int) int {
	if listenersOverride > 0 {
		return listenersOverride
	}
	if procs >= wideHostProcs && loops > 0 {
		return loops
	}
	return min(max(2, procs/12), 4)
}

// addListen creates the listeners for addr and assigns each stream listener
// to a loop. Nothing is accepted until startAccepting, which Serve calls once
// OnStart has returned; until then arriving connections wait in the
// listener backlogs.
func (ld *acceptor) addListen(addr string) error {
	group, err := ld.bindGroup(addr, nil)
	if err != nil {
		return err
	}
	ld.mux.Lock()
	defer ld.mux.Unlock()
	return ld.addGroupLocked(group, nil)
}

// bindGroup creates sockets without holding a registry lock: a control may
// close Events or another listener. Nothing is published until addGroupLocked.
func (ld *acceptor) bindGroup(addr string, control SocketControl) (group []*listener, err error) {
	defer func() {
		if err != nil {
			for _, l := range group {
				ld.closeListener(l)
			}
			group = nil
		}
	}()

	l, err := ld.listen(addr, ld.events.ReusePort, control)
	if err != nil {
		if l != nil {
			ld.closeListener(l)
		}
		return nil, err
	}
	group = append(group, l)

	count := 1
	if ld.events.ReusePort {
		if l.tcp {
			count = reusePortListeners(runtime.GOMAXPROCS(0), len(ld.events.loops))
		} else if listenersOverride > 0 {
			count = listenersOverride
		} else if reusePortBalances {
			count = max(1, runtime.GOMAXPROCS(0))
		}
	}
	// The others bind the concrete address l resolved, so a port of zero
	// still yields one shared port. The scheme goes along: without it the
	// address defaults to tcp, and a group requested as tcp4 or tcp6 would
	// bind dual-stack sockets instead.
	scheme := "tcp"
	if mark := strings.Index(addr, "://"); mark >= 0 {
		scheme = addr[:mark]
	}
	resolved := scheme + "://" + l.pair.local.String()
	for range count - 1 {
		next, err := ld.listen(resolved, true, control)
		if err != nil {
			// listen can fail after creating the listener (SetNonblock); it
			// is not in the registry yet, so release it here.
			if next != nil {
				ld.closeListener(next)
			}
			return group, err
		}
		group = append(group, next)
	}
	return group, nil
}

// addGroupLocked admits a complete group against shutdown, gives every UDP
// server a lifetime owner, and then publishes it. It requires ld.mux and
// releases the entire group on error, including sockets not yet admitted.
func (ld *acceptor) addGroupLocked(group []*listener, userdata any) (err error) {
	defer func() {
		if err != nil {
			for _, l := range group {
				delete(ld.listeners, l.fd)
				ld.closeListener(l)
			}
		}
	}()
	if ld.closed || ld.events.closing.Load() {
		return net.ErrClosed
	}
	if ld.listeners == nil {
		ld.listeners = make(map[int]*listener)
	}
	for i, l := range group {
		l.userdata = userdata
		if userdata != nil {
			box := userdata
			l.userdataBox = &box
		}
		if l.udp != nil {
			family, err := socket.SocketFamily(l.fd)
			if err != nil {
				return err
			}
			l.udpSvr = &fdConn{}
			// Its peers run the endpoint's own: the listener's entry, set
			// before the group was added.
			l.udpSvr.entry = l.entry
			if l.udpSvr.entry == nil {
				l.udpSvr.entry = ld.events.plainEntry()
			}
			l.udpSvr.fd = int32(l.fd)
			l.udpSvr.udp = &unixUDPState{
				peers:      make(map[socket.UDPAddress]*fdConn),
				family:     family,
				groEnabled: l.groEnabled,
				shardKey:   uint32(i),
			}
			l.udpSvr.loop = ld.events.selectLoop()
			l.udpSvr.internal = true
			l.udpSvr.addr = l.pair
			l.udpSvr.SetUserdata(userdata) // What its peers start with.
			// Ownership precedes publication and outlives listener removal,
			// even when the listener closes before accepting starts.
			if err := l.udpSvr.loop.trackListener(l.udpSvr); err != nil {
				return err
			}
		} else {
			ld.assignLoop(l)
		}
		ld.listeners[l.fd] = l
	}
	// A stream group with a listener per loop covers every loop once, since
	// assignLoop goes round them in turn. Registering an accepted connection
	// on another loop's poller contends with that loop's waiter for the
	// poller's lock; a connection-churn load at 4096 connections ran 10%
	// faster with each listener's connections kept on its own loop.
	if loops := len(ld.events.loops); reusePortBalances && loops > 1 && len(group) == loops && group[0].udp == nil {
		for _, l := range group {
			l.acceptLocal = true
		}
	}
	if len(group) > 1 && group[0].udp != nil {
		linkUDPGroup(group)
	}
	return nil
}

func linkUDPGroup(group []*listener) {
	servers := make([]*fdConn, len(group))
	for i, l := range group {
		servers[i] = l.udpSvr
	}
	for _, s := range servers {
		s.udp.group = servers
	}
}

// listenGroup adds addr while Serve runs: it records its listeners and starts
// accepting on them at once.
func (ld *acceptor) listenGroup(addr string, cfg listenConfig) (Listener, error) {
	group, err := ld.bindGroup(addr, cfg.control)
	if err != nil {
		return nil, err
	}
	entry, err := ld.events.entryFor(cfg.layers, cfg.hasLayers)
	if err != nil {
		for _, l := range group {
			ld.closeListener(l)
		}
		return nil, err
	}
	for _, l := range group {
		l.entry = entry
		if l.udpSvr != nil {
			l.udpSvr.entry = entry
		}
	}
	ld.mux.Lock()
	defer ld.mux.Unlock()
	if err = ld.addGroupLocked(group, cfg.userdata); err != nil {
		return nil, err
	}
	// Added during OnStart, the listeners start with the rest once it has
	// returned; see startAccepting.
	for i, l := range group {
		if !ld.accepting {
			break
		}
		if err = ld.startLocked(l); err != nil {
			for _, started := range group[:i] {
				ld.stopLocked(started)
			}
			for _, l := range group[i:] {
				delete(ld.listeners, l.fd)
				ld.closeListener(l)
			}
			return nil, err
		}
	}
	return (&listenerGroup{ld: ld, members: group, addr: group[0].pair.local}).asListener(), nil
}

// startLocked registers l with its loop; it requires ld.mux.
func (ld *acceptor) startLocked(l *listener) error {
	var err error
	if l.udpSvr != nil {
		err = l.udpSvr.loop.registerListener(l.udpSvr)
	} else {
		err = l.loop.addListener(l)
	}
	l.started = err == nil
	return err
}

// stopLocked takes l out of the registry and releases it while Serve runs;
// it requires ld.mux. A stream listener leaves its loop first, and its
// descriptor closes only once no accept is in progress on it any more. A UDP
// listener's server connection closes in its own turn, and its peers with it;
// its socket may close sooner, see closeGroup.
func (ld *acceptor) stopLocked(l *listener) {
	delete(ld.listeners, l.fd)
	if l.udpSvr != nil {
		// Started or not, the release travels as the server's own turn, in
		// line behind the opens DialPeer queued there: a peer's OnOpen runs
		// and returns before its release, and the cause is the one Close
		// promised.
		l.udpSvr.requestClose(net.ErrClosed)
		if l.udp != nil {
			_ = l.udp.Close()
		}
		return
	}
	if !l.started {
		// Nothing watches it yet; release it as shutdown would.
		ld.closeListener(l)
		return
	}
	l.loop.removeListener(l)
	l.retire()
	ld.closeListener(l)
}

// closeGroup releases group unless shutdown already did. A UDP listener's
// socket closes before it returns, so the address is free for the next bind,
// once ld.mux is released: closing waits for a Control in progress on the
// socket, whose fn may add or close listeners.
func (ld *acceptor) closeGroup(group []*listener) {
	ld.mux.Lock()
	var servers []*fdConn
	for _, l := range group {
		if ld.listeners[l.fd] == l {
			ld.stopLocked(l)
			if l.udpSvr != nil {
				servers = append(servers, l.udpSvr)
			}
		}
	}
	ld.mux.Unlock()
	for _, server := range servers {
		server.releaseSocketNow()
	}
}

// assignLoop gives stream listeners to the loops in turn.
func (ld *acceptor) assignLoop(l *listener) {
	loops := ld.events.loops
	l.loop = loops[ld.next%len(loops)]
	ld.next++
}

// startAccepting registers every listener with its loop. Serve calls it once
// OnStart has returned, so no connection is accepted — and no OnOpen can run —
// while the application is still initializing.
func (ld *acceptor) startAccepting() error {
	ld.mux.Lock()
	defer ld.mux.Unlock()
	if ld.closed || ld.events.closing.Load() {
		return nil
	}
	ld.accepting = true
	for _, l := range ld.listeners {
		if err := ld.startLocked(l); err != nil {
			return err
		}
	}
	return nil
}

func (ld *acceptor) closeListener(l *listener) {
	if l.udpSvr != nil {
		// The server remains owned by its loop until its own turn, or the
		// shutdown pass after all turns ended, finishes releasing it.
		l.udpSvr.requestClose(io.ErrUnexpectedEOF)
	}
	if l.file != nil && l.udpSvr == nil {
		_ = l.file.Close()
	} else if l.udp != nil && l.udpSvr == nil && l.fd > 0 {
		_ = syscall.Close(l.fd)
	}
	if l.ln != nil {
		if _, ok := l.ln.(*net.UnixListener); ok {
			_ = os.RemoveAll(l.addr)
		}

		_ = l.ln.Close()
	}

	if l.udp != nil {
		_ = l.udp.Close()
	}
}

// close releases every listener. Serve calls it after the loops' waiters and
// accept runners have returned, so nothing accepts on a listener it closes.
func (ld *acceptor) close() {
	ld.mux.Lock()
	// User OnClose callbacks run while closing UDP children, so release the
	// listener-map lock before closing resources.
	listeners := ld.listeners
	ld.listeners = nil
	ld.closed = true
	ld.mux.Unlock()
	for _, l := range listeners {
		ld.closeListener(l)
	}
}

// listen parses UIO's scheme-prefixed address and duplicates the resulting Go
// listener descriptor. The duplicate is switched to non-blocking mode and is
// thereafter owned by the poller-facing listener record.
func (ld *acceptor) listen(addr string, reusePort bool, control SocketControl) (*listener, error) {

	// default scheme is tcp protocol.
	if !strings.Contains(addr, "://") {
		addr = "tcp://" + addr
	}

	// parse url scheme.
	u, err := url.Parse(addr)
	if nil != err {
		return nil, err
	}

	var l listener
	if ld.events != nil {
		l.entry = ld.events.plainEntry()
	}
	l.controlled = control != nil
	lnConfig := netListenConfigFor(reusePort, control)

	switch u.Scheme {
	case "tcp", "tcp4", "tcp6":
		l.addr = u.Host
		l.ln, err = lnConfig.Listen(context.Background(), u.Scheme, u.Host)
	case "udp", "udp4", "udp6":
		l.addr = u.Host
		l.udp, err = lnConfig.ListenPacket(context.Background(), u.Scheme, u.Host)
	case "unix", "unixgram", "unixpacket":
		if err = os.RemoveAll(u.Path); nil == err || os.IsNotExist(err) {
			l.addr = u.Path
			l.ln, err = lnConfig.Listen(context.Background(), u.Scheme, u.Path)
		}
	default:
		return nil, fmt.Errorf("unsupported protocol: %s", u.Scheme)
	}

	if nil != err {
		return nil, err
	}

	// Arm the listening socket with the options every accepted stream
	// connection wants, on platforms that copy them at accept time; see
	// setListenerOptions. UDP and Unix listeners are left alone.
	if l.ln != nil {
		setListenerOptions(l.ln)
	}

	var laddr net.Addr
	if l.udp != nil {
		laddr = l.udp.LocalAddr()
	} else {
		laddr = l.ln.Addr()
	}
	l.pair = &addrPair{local: laddr}

	switch ln := l.ln.(type) {
	case nil:
		// udp listener: duplicate the raw descriptor via SyscallConn and close
		// the net.UDPConn wrapper immediately so the socket is removed from the
		// Go runtime's global netpoller instead of waking it on every datagram.
		switch pc := l.udp.(type) {
		case *net.UDPConn:
			l.fd, err = socket.DupNetConn(pc)
			_ = pc.Close()
		default:
			err = fmt.Errorf("unsupported udp connection type: %T", l.udp)
		}
	case *net.TCPListener:
		l.file, err = ln.File()
	case *net.UnixListener:
		l.file, err = ln.File()
	default:
		err = fmt.Errorf("unsupported listener type: %T", ln)
	}

	if nil != err {
		ld.closeListener(&l)
		return nil, err
	}

	if l.file != nil {
		l.fd = int(l.file.Fd())
	}
	if l.udp != nil && ld.events != nil && max(ld.events.readBufSize, ld.events.ReadBufferSize) >= 64<<10 {
		l.groEnabled = socket.EnableGRO(l.fd)
	}
	l.network = u.Scheme
	l.tcp = strings.HasPrefix(u.Scheme, "tcp")
	return &l, syscall.SetNonblock(l.fd, true)
}
