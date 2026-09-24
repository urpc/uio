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
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"strings"
	"sync"
	"syscall"

	"github.com/libp2p/go-reuseport"
	"github.com/urpc/uio/internal/poller"
	"github.com/urpc/uio/internal/socket"
)

const acceptBatchSize = 64

// listener keeps both the Go listener and the duplicated non-blocking fd used
// by the native poller. Closing it must account for which object owns that fd.
type listener struct {
	network string         // network protocol
	fd      int            // fd
	addr    string         // address
	laddr   net.Addr       // local listen address
	ln      net.Listener   // tcp/unix listener
	file    *os.File       // file
	udp     net.PacketConn // udp endpoint
	udpSvr  *fdConn        // udp server
}

// acceptor runs on the master event loop. It accepts stream sockets and owns
// shared UDP listener state, then assigns stream connections to worker loops.
type acceptor struct {
	mux       sync.Mutex
	listeners map[int]*listener
	loop      *eventLoop
	events    *Events
}

// OnEvent drains listener readiness and escalates a non-retryable accept error
// to server shutdown.
func (ld *acceptor) OnEvent(ep *poller.NetPoller, fd int, events poller.Events) {

	if 0 != events&poller.ReadEvents {
		ld.mux.Lock()
		l, ok := ld.listeners[fd]
		ld.mux.Unlock()

		if ok {
			if err := ld.accept(l); err != nil {
				ld.events.initiateClose(err)
			}
		}
	}

	if 0 != events&poller.WriteEvents {
		return
	}
}

// OnClose releases every listener owned by the master loop.
func (ld *acceptor) OnClose(ep *poller.NetPoller, err error) {
	ld.close()
}

// accept handles a bounded portion of one listener readiness notification.
// The bound preserves fairness between listeners; level-triggered readiness
// is delivered again while the accept queue remains non-empty.
func (ld *acceptor) accept(l *listener) error {

	// udp server incoming
	if l.udp != nil {
		return ld.onReadUDP(l)
	}

	tcp := strings.HasPrefix(l.network, "tcp")
	// Bound one readiness dispatch so a busy listener cannot monopolize master.
	for accepted := 0; accepted < acceptBatchSize; accepted++ {
		nfd, sa, err := socket.Accept(l.fd)
		if nil != err {
			if isWouldBlock(err) {
				return nil
			}
			return err
		}

		fdc := &fdConn{}
		fdc.fd = nfd
		fdc.events = ld.events
		fdc.loop = ld.events.selectWorker(nfd)
		fdc.localAddr = l.laddr
		fdc.remoteAddr = socket.SockaddrToAddr(sa, false)

		ld.events.submitAccepted(fdc, tcp)
	}
	return nil
}

// addListen creates a listener and publishes its poller-visible endpoint while
// holding the listener registry lock. UDP registers a logical internal
// connection because all peers share the same socket.
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
		l.udpSvr.loop = ld.loop
		l.udpSvr.events = ld.events
		l.udpSvr.internal = true
		l.udpSvr.localAddr = l.laddr

		if err = ld.loop.fdMap.Put(l.fd, l.udpSvr); err != nil {
			l.udpSvr.closeUnregistered()
			return err
		}
		if err = ld.loop.poller.Add(l.fd, poller.Readable); err != nil {
			ld.loop.fdMap.Delete(l.fd)
			return err
		}
		l.udpSvr.interest = poller.Readable
		return nil
	}

	return ld.loop.listen(l.fd)
}

func (ld *acceptor) closeListener(l *listener) {
	if l.udpSvr != nil && !l.udpSvr.isClosedOnLoop() {
		l.udpSvr.closeOnLoop(io.ErrUnexpectedEOF)
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

	if l.udp != nil {
		l.laddr = l.udp.LocalAddr()
	} else {
		l.laddr = l.ln.Addr()
	}

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
	return &l, syscall.SetNonblock(l.fd, true)
}

func (ld *acceptor) onReadUDP(l *listener) error {

	udpSvrConn := ld.loop.getConn(l.fd)
	if nil == udpSvrConn {
		return errors.New("no such udp server")
	}

	return udpSvrConn.fireReadEvent()
}
