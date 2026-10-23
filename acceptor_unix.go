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

	"github.com/libp2p/go-reuseport"
	"github.com/urpc/uio/internal/socket"
)

// acceptBatchSize bounds how many connections one listener event accepts.
const acceptBatchSize = 64

// listener keeps both the Go listener and the duplicated non-blocking fd used
// by the native poller. Closing it must account for which object owns that fd.
type listener struct {
	network string         // network protocol
	fd      int            // fd
	addr    string         // address
	tcp     bool           // a TCP stream listener: accepted peers are IP addresses
	pair    *addrPair      // local listen address, shared by accepted connections
	ln      net.Listener   // tcp/unix listener
	file    *os.File       // file
	udp     net.PacketConn // udp endpoint
	udpSvr  *fdConn        // udp server
	loop    *eventLoop     // the loop that accepts on a stream listener

	// userdata is every accepted connection's initial Userdata.
	userdata any
	started  bool // registered with its loop; guarded by acceptor.mux
	// use counts the waiters accepting on the listener, below its closed
	// bit; see enter and retire.
	use atomic.Int64
}

// listenerClosed is the bit of listener.use a closing listener sets.
const listenerClosed = 1 << 62

// enter reports whether a waiter may accept on l, and counts it until leave.
func (l *listener) enter() bool {
	for {
		use := l.use.Load()
		if use&listenerClosed != 0 {
			return false
		}
		if l.use.CompareAndSwap(use, use+1) {
			return true
		}
	}
}

func (l *listener) leave() { l.use.Add(-1) }

// retire stops new accepts on l and waits for the ones in progress. A waiter
// may have looked l up just before it left its loop, so its descriptor is
// closed only after this returns: closed earlier, the number could be reused
// by another socket that waiter would then accept on.
func (l *listener) retire() {
	l.use.Or(listenerClosed)
	for l.use.Load() != listenerClosed {
		runtime.Gosched()
	}
}

// acceptor owns the listeners. Stream listeners are spread over the event
// loops, whose waiters accept on them; a UDP listener is a connection of its
// own, whose turns read every peer's datagrams.
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
// listener's share would shrink its batches.
func reusePortListeners(procs int) int {
	if listenersOverride > 0 {
		return listenersOverride
	}
	return min(max(2, procs/12), 4)
}

// addListen creates the listeners for addr and assigns each stream listener
// to a loop. Nothing is accepted until startAccepting, which Serve calls once
// OnStart has returned; until then arriving connections wait in the
// listener backlogs.
func (ld *acceptor) addListen(addr string) error {
	ld.mux.Lock()
	defer ld.mux.Unlock()
	_, err := ld.addGroupLocked(addr, nil)
	return err
}

// addGroupLocked creates and records the listeners for addr: one, or a
// ReusePort group of them, every accepted connection starting with userdata.
// It requires ld.mux. On error nothing it created stays open.
func (ld *acceptor) addGroupLocked(addr string, userdata any) (group []*listener, err error) {
	if nil == ld.listeners {
		ld.listeners = make(map[int]*listener)
	}
	defer func() {
		if err != nil {
			for _, l := range group {
				delete(ld.listeners, l.fd)
				ld.closeListener(l)
			}
			group = nil
		}
	}()

	l, err := ld.listen(addr, ld.events.ReusePort)
	if err != nil {
		if l != nil {
			ld.closeListener(l)
		}
		return nil, err
	}
	l.userdata = userdata
	ld.listeners[l.fd] = l
	group = append(group, l)

	if l.udp != nil {
		l.udpSvr = &fdConn{}
		l.udpSvr.fd = int32(l.fd)
		// The logical UDP server takes ownership of the duplicated poller fd.
		l.udpSvr.udp = &unixUDPState{
			file:  l.file,
			peers: make(map[socket.UDPAddress]*fdConn),
		}
		l.file = nil
		l.udpSvr.loop = ld.events.selectLoop()
		l.udpSvr.events = ld.events
		l.udpSvr.internal = true
		l.udpSvr.addr = l.pair
		// The server is internal; its userdata is what its peers start with.
		l.udpSvr.userdata = userdata
		return group, nil
	}

	count := 1
	if ld.events.ReusePort && l.tcp {
		count = reusePortListeners(runtime.GOMAXPROCS(0))
	}
	ld.assignLoop(l)
	// The others bind the concrete address l resolved, so a port of zero
	// still yields one shared port.
	resolved := l.ln.Addr().String()
	for range count - 1 {
		next, err := ld.listen(resolved, true)
		if err != nil {
			// listen can fail after creating the listener (SetNonblock); it
			// is not in the registry yet, so release it here.
			if next != nil {
				ld.closeListener(next)
			}
			return group, err
		}
		next.userdata = userdata
		ld.listeners[next.fd] = next
		group = append(group, next)
		ld.assignLoop(next)
	}
	return group, nil
}

// listenGroup adds addr while Serve runs: it records its listeners and starts
// accepting on them at once.
func (ld *acceptor) listenGroup(addr string, userdata any) (*listenerGroup, error) {
	ld.mux.Lock()
	defer ld.mux.Unlock()
	if ld.closed {
		return nil, net.ErrClosed
	}
	group, err := ld.addGroupLocked(addr, userdata)
	if err != nil {
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
	return &listenerGroup{ld: ld, members: group, addr: group[0].pair.local}, nil
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
// descriptor closes only once no waiter accepts on it any more. A UDP
// listener's server connection closes in its own turn, and its peers with it.
func (ld *acceptor) stopLocked(l *listener) {
	delete(ld.listeners, l.fd)
	if !l.started {
		// Nothing watches it yet; release it as shutdown would.
		ld.closeListener(l)
		return
	}
	if l.udpSvr != nil {
		_ = l.udpSvr.CloseWith(net.ErrClosed)
		if l.udp != nil {
			_ = l.udp.Close()
		}
		return
	}
	l.loop.removeListener(l)
	l.retire()
	ld.closeListener(l)
}

// closeGroup releases group unless shutdown already did.
func (ld *acceptor) closeGroup(group []*listener) {
	ld.mux.Lock()
	defer ld.mux.Unlock()
	for _, l := range group {
		if ld.listeners[l.fd] == l {
			ld.stopLocked(l)
		}
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
	ld.accepting = true
	for _, l := range ld.listeners {
		if err := ld.startLocked(l); err != nil {
			return err
		}
	}
	return nil
}

func (ld *acceptor) closeListener(l *listener) {
	if l.udpSvr != nil && !l.udpSvr.isReleased() {
		if err, ok := l.udpSvr.teardown(io.ErrUnexpectedEOF); ok {
			l.udpSvr.scheduleCloseCallback(err)
		}
	}
	if l.file != nil && l.udpSvr == nil {
		_ = l.file.Close()
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

// close releases every listener. Serve calls it after the loops' waiters have
// returned, so nothing accepts on a listener it closes.
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
func (ld *acceptor) listen(addr string, reusePort bool) (*listener, error) {

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

	switch u.Scheme {
	case "tcp", "tcp4", "tcp6":
		l.addr = u.Host
		if reusePort {
			l.ln, err = reuseport.Listen(u.Scheme, u.Host)
		} else {
			l.ln, err = net.Listen(u.Scheme, u.Host)
		}
	case "udp", "udp4", "udp6":
		l.addr = u.Host
		if reusePort {
			l.udp, err = reuseport.ListenPacket(u.Scheme, u.Host)
		} else {
			l.udp, err = net.ListenPacket(u.Scheme, u.Host)
		}
	case "unix", "unixgram", "unixpacket":
		if err = os.RemoveAll(u.Path); nil == err || os.IsNotExist(err) {
			l.addr = u.Path
			l.ln, err = net.Listen(u.Scheme, u.Path)
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
		// udp listener
		switch pc := l.udp.(type) {
		case *net.UDPConn:
			l.file, err = pc.File()
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

	l.fd = int(l.file.Fd())
	l.network = u.Scheme
	l.tcp = strings.HasPrefix(u.Scheme, "tcp")
	return &l, syscall.SetNonblock(l.fd, true)
}
