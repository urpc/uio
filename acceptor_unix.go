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
}

// acceptor owns the listeners. Stream listeners are spread over the event
// loops, whose waiters accept on them; a UDP listener is a connection of its
// own, whose turns read every peer's datagrams.
type acceptor struct {
	mux       sync.Mutex
	listeners map[int]*listener
	events    *Events
	next      int // the loop the next stream listener goes to
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
func (ld *acceptor) addListen(addr string) (err error) {
	ld.mux.Lock()
	defer ld.mux.Unlock()

	if nil == ld.listeners {
		ld.listeners = make(map[int]*listener)
	}

	var l *listener
	if l, err = ld.listen(addr, ld.events.ReusePort); nil != err {
		if nil != l {
			ld.closeListener(l)
		}
		return err
	}
	ld.listeners[l.fd] = l

	if l.udp != nil {
		l.udpSvr = &fdConn{}
		l.udpSvr.fd = l.fd
		// The logical UDP server takes ownership of the duplicated poller fd.
		l.udpSvr.udp = &unixUDPState{
			file:  l.file,
			peers: make(map[socket.UDPAddress]*fdConn),
		}
		l.file = nil
		l.udpSvr.loop = ld.events.selectLoop(l.fd)
		l.udpSvr.events = ld.events
		l.udpSvr.internal = true
		l.udpSvr.addr = l.pair
		return nil
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
			return err
		}
		ld.listeners[next.fd] = next
		ld.assignLoop(next)
	}
	return nil
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
	for _, l := range ld.listeners {
		if l.udpSvr != nil {
			if err := l.udpSvr.loop.registerListener(l.udpSvr); err != nil {
				return err
			}
			continue
		}
		if err := l.loop.addListener(l); err != nil {
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
