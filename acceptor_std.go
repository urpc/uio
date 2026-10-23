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
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/libp2p/go-reuseport"
	"github.com/urpc/uio/internal/poller"
)

// listener holds the blocking Go listener used by stdio and Windows builds.
type listener struct {
	addr     string
	ln       net.Listener
	udp      net.PacketConn
	udpSvr   *fdConn
	key      int // its key in acceptor.listeners
	userdata any // every accepted connection's initial Userdata
}

// acceptor owns blocking listener goroutines. The logical event loop is used
// only for registration and lifecycle serialization on this backend.
type acceptor struct {
	mux       sync.Mutex
	listeners map[int]*listener
	loop      *eventLoop
	events    *Events
	next      int  // the key of the next listener
	closed    bool // close has run: Listen adds nothing more
}

// OnEvent is unused because listener goroutines perform blocking accepts.
func (ld *acceptor) OnEvent(ep *poller.Poller, fd int, events poller.Events) {}

// OnClose releases every blocking listener and stops its accept goroutine.
func (ld *acceptor) OnClose(ep *poller.Poller, err error) {
	ld.close()
}

// startMultiAcceptors exists for the shared Serve path. This backend has no
// dedicated ReusePort acceptors: its blocking listener goroutines are created
// together with the listeners themselves.
func (ld *acceptor) startMultiAcceptors() {}

// addListen starts one blocking accept or UDP receive goroutine for addr.
// Accepted streams are registered before their dedicated read/write loops run.
func (ld *acceptor) addListen(addr string) error {
	ld.mux.Lock()
	defer ld.mux.Unlock()
	_, err := ld.addLocked(addr, nil)
	return err
}

// listenGroup adds addr while Serve runs.
func (ld *acceptor) listenGroup(addr string, userdata any) (Listener, error) {
	ld.mux.Lock()
	defer ld.mux.Unlock()
	if ld.closed {
		return nil, net.ErrClosed
	}
	l, err := ld.addLocked(addr, userdata)
	if err != nil {
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

// closeGroup releases group unless shutdown already did. Closing a stream
// listener ends its accept goroutine; a UDP listener's server connection
// closes its peers first.
func (ld *acceptor) closeGroup(group []*listener) {
	ld.mux.Lock()
	defer ld.mux.Unlock()
	for _, l := range group {
		if ld.listeners[l.key] != l {
			continue
		}
		delete(ld.listeners, l.key)
		if l.udpSvr != nil {
			// Synchronously, with this cause: the queued form would lose the
			// race with closeListener's own close and its default cause.
			l.udpSvr.closeOnLoop(net.ErrClosed)
		}
		ld.closeListener(l)
	}
}

// addLocked creates the listener for addr and starts its goroutine; it
// requires ld.mux.
func (ld *acceptor) addLocked(addr string, userdata any) (*listener, error) {
	if nil == ld.listeners {
		ld.listeners = make(map[int]*listener)
	}

	l, err := ld.listen(addr, ld.events.ReusePort)
	if nil != err {
		if nil != l {
			ld.closeListener(l)
		}
		return nil, err
	}
	l.userdata = userdata
	// The requested address may be repeated with port 0; each bind is distinct.
	l.key = ld.next
	ld.next++
	ld.listeners[l.key] = l

	if l.udp != nil {
		l.udpSvr = &fdConn{}
		l.udpSvr.loop = ld.loop
		l.udpSvr.events = ld.events
		l.udpSvr.internal = true
		l.udpSvr.userdata = userdata // what its peers start with
		l.udpSvr.udp = &stdUDPState{sock: l.udp.(*net.UDPConn), peers: make(map[string]*fdConn)}

		ld.events.callbackWG.Add(1)
		go l.udpSvr.listenUDP()
		return l, nil
	}

	ld.events.callbackWG.Add(1)
	go func() {
		defer ld.events.callbackWG.Done()
		var tempDelay time.Duration // how long to sleep on accept failure
		for {
			conn, err := l.ln.Accept()
			if nil != err {
				if ne, ok := err.(net.Error); ok && ne.Timeout() {
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
				return
			}

			fdc := &fdConn{}
			fdc.conn = conn
			fdc.events = ld.events
			fdc.loop = ld.events.selectWorker(fdc.Fd())
			fdc.userdata = l.userdata
			fdc.setLocalAddr(conn.LocalAddr())
			fdc.setRemoteAddr(conn.RemoteAddr())
			fdc.writeSig = make(chan struct{}, 1)
			fdc.readResume = make(chan struct{}, 1)

			// Registration runs on the worker loop; waiting for it here would
			// hold every later accept behind that loop's queue.
			ld.events.submitAccepted(fdc, true)
		}
	}()

	return l, nil
}

// listen parses UIO's address syntax and creates the corresponding blocking Go
// listener. Unlike the native backend, no descriptor is duplicated.
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

	return &l, err
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
