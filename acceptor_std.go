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
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/urpc/uio/internal/poller"
)

// listener holds the blocking Go listener used by stdio and Windows builds.
type listener struct {
	addr     string
	ln       net.Listener
	udp      net.PacketConn
	udpSvr   *fdConn
	key      int    // its key in acceptor.listeners
	started  bool   // its accept goroutine runs; guarded by acceptor.mux
	userdata any    // every accepted connection's initial Userdata
	entry    *entry // every accepted connection's callbacks and chain
}

// acceptor owns blocking listener goroutines. The logical event loop is used
// only for registration and lifecycle serialization on this backend.
type acceptor struct {
	mux       sync.Mutex
	listeners map[int]*listener
	loop      *eventLoop
	events    *Events
	next      int  // the key of the next listener
	accepting bool // startAccepting has run: a new listener starts at once
	closed    bool // close has run: Listen adds nothing more
}

// OnEvent is unused because listener goroutines perform blocking accepts.
func (ld *acceptor) OnEvent(ep *poller.Poller, fd int, events poller.Events) {}

// OnClose releases every blocking listener and stops its accept goroutine.
func (ld *acceptor) OnClose(ep *poller.Poller, err error) {
	ld.close()
}

// startMultiAcceptors exists for the shared Serve path. This backend has no
// dedicated ReusePort acceptors.
func (ld *acceptor) startMultiAcceptors() {}

// addListen binds addr for Serve. Nothing accepts on it until
// startAccepting, once OnStart has returned — a connection must not reach a
// callback while the application is still initializing.
func (ld *acceptor) addListen(addr string) error {
	l, err := ld.listen(addr, ld.events.ReusePort, nil)
	if err != nil {
		return err
	}
	ld.mux.Lock()
	defer ld.mux.Unlock()
	return ld.addLocked(l, nil)
}

// listenGroup adds addr while Serve runs.
func (ld *acceptor) listenGroup(addr string, cfg listenConfig) (Listener, error) {
	// A socket control may close Events or another listener. Create the
	// socket before taking the registry lock, then admit it against shutdown.
	l, err := ld.listen(addr, ld.events.ReusePort, cfg.control)
	if err != nil {
		return nil, err
	}
	entry, err := ld.events.entryFor(cfg.layers, cfg.hasLayers)
	if err != nil {
		ld.closeListener(l)
		return nil, err
	}
	l.entry = entry
	if l.udpSvr != nil {
		l.udpSvr.entry = entry
	}
	ld.mux.Lock()
	defer ld.mux.Unlock()
	if err = ld.addLocked(l, cfg.userdata); err != nil {
		return nil, err
	}
	var local net.Addr
	if l.udp != nil {
		local = l.udp.LocalAddr()
	} else {
		local = l.ln.Addr()
	}
	return (&listenerGroup{ld: ld, members: []*listener{l}, addr: local}).asListener(), nil
}

// startAccepting starts every listener Serve bound, once OnStart has
// returned; see addListen.
func (ld *acceptor) startAccepting() error {
	ld.mux.Lock()
	defer ld.mux.Unlock()
	if ld.closed || ld.events.closing.Load() {
		return nil
	}
	ld.accepting = true
	for _, l := range ld.listeners {
		ld.startLocked(l)
	}
	return nil
}

// closeGroup releases group unless shutdown already did. Closing a stream
// listener ends its accept goroutine; a UDP listener's server connection
// closes its peers first, and its socket before closeGroup returns — once
// ld.mux is released, since it waits for Controls in progress on the socket,
// whose fn may add or close listeners.
func (ld *acceptor) closeGroup(group []*listener) {
	ld.mux.Lock()
	var servers []*fdConn
	for _, l := range group {
		if ld.listeners[l.key] != l {
			continue
		}
		delete(ld.listeners, l.key)
		if l.udpSvr != nil {
			servers = append(servers, l.udpSvr)
			continue
		}
		ld.closeListener(l)
	}
	ld.mux.Unlock()
	for _, server := range servers {
		server.closeListenerSocket(net.ErrClosed)
	}
}

// addLocked admits and records a bound listener; it requires ld.mux. The same
// lock excludes shutdown until every I/O goroutine is counted.
// A listener added while Serve runs starts at once; one from Serve starts
// with the rest in startAccepting.
func (ld *acceptor) addLocked(l *listener, userdata any) error {
	if ld.closed || ld.events.closing.Load() {
		ld.closeListener(l)
		return net.ErrClosed
	}
	if nil == ld.listeners {
		ld.listeners = make(map[int]*listener)
	}
	l.userdata = userdata
	// The requested address may be repeated with port 0; each bind is distinct.
	l.key = ld.next
	ld.next++
	ld.listeners[l.key] = l

	if l.udp != nil {
		l.udpSvr = &fdConn{}
		// Its peers run the endpoint's own: the listener's entry, set before
		// the group was added.
		l.udpSvr.entry = l.entry
		if l.udpSvr.entry == nil {
			l.udpSvr.entry = ld.events.plainEntry()
		}
		l.udpSvr.loop = ld.loop
		l.udpSvr.internal = true
		l.udpSvr.setLocalAddr(l.udp.LocalAddr()) // what its peers report
		l.udpSvr.SetUserdata(userdata)           // what its peers start with
		l.udpSvr.udp = &stdUDPState{sock: l.udp.(*net.UDPConn), peers: make(map[netip.AddrPort]*fdConn)}
	}
	if ld.accepting {
		ld.startLocked(l)
	}
	return nil
}

// startLocked starts l accepting: one blocking accept goroutine, or the UDP
// receive goroutine of its server connection. It requires ld.mux and runs
// each listener once.
func (ld *acceptor) startLocked(l *listener) {
	if l.started {
		return
	}
	l.started = true
	if l.udpSvr != nil {
		ld.events.callbackWG.Add(1)
		go l.udpSvr.listenUDP()
		return
	}

	ld.events.callbackWG.Add(1)
	go func() {
		defer ld.events.callbackWG.Done()
		var tempDelay time.Duration // how long to sleep on accept failure
		for {
			conn, err := l.ln.Accept()
			if nil != err {
				// Only closing the listener ends accepting. Anything else —
				// out of descriptors or memory, a connection reset while it
				// waited — is retried after a pause that doubles while the
				// failures last.
				if errors.Is(err, net.ErrClosed) {
					return
				}
				if tempDelay == 0 {
					tempDelay = 5 * time.Millisecond
				} else {
					tempDelay *= 2
				}
				if maxDelay := 1 * time.Second; tempDelay > maxDelay {
					tempDelay = maxDelay
				}
				time.Sleep(tempDelay)
				continue
			}
			tempDelay = 0

			fdc := &fdConn{}
			fdc.entry = l.entry
			fdc.conn = conn

			fdc.loop = ld.events.selectWorker(fdc.Fd())
			fdc.SetUserdata(l.userdata)
			fdc.setLocalAddr(conn.LocalAddr())
			fdc.setRemoteAddr(conn.RemoteAddr())
			fdc.writeSig = make(chan struct{}, 1)
			fdc.readResume = make(chan struct{}, 1)

			// Registration runs on the worker loop; waiting for it here would
			// hold every later accept behind that loop's queue.
			ld.events.submitAccepted(fdc, true)
		}
	}()
}

// listen parses UIO's address syntax and creates the corresponding blocking Go
// listener. Unlike the native backend, no descriptor is duplicated.
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

	if err != nil {
		ld.closeListener(&l)
		return nil, err
	}
	return &l, nil
}

func (ld *acceptor) closeListener(l *listener) {
	if l.ln != nil {
		if _, ok := l.ln.(*net.UnixListener); ok {
			_ = os.RemoveAll(l.addr)
		}

		_ = l.ln.Close()
	}

	if l.udpSvr != nil {
		l.udpSvr.closeOnLoop(io.ErrUnexpectedEOF)
	}

	if l.udp != nil {
		_ = l.udp.Close()
	}
}

func (ld *acceptor) close() {
	ld.mux.Lock()
	listeners := ld.listeners
	ld.listeners = nil
	ld.closed = true
	ld.mux.Unlock()
	for _, l := range listeners {
		ld.closeListener(l)
	}
}
