//go:build linux || darwin || netbsd || freebsd || openbsd || dragonfly

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
	"net"
	"net/url"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/libp2p/go-reuseport"
	"github.com/urpc/uio/internal/poller"
	"github.com/urpc/uio/internal/socket"
)

const defaultTCPKeepAlive = 15 * time.Second

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

type acceptor struct {
	mux       sync.Mutex
	listeners map[int]*listener
	loop      *eventLoop
	events    *Events
}

func (ld *acceptor) OnWrite(ep *poller.NetPoller, fd int) {
	panic("unreachable here")
}

func (ld *acceptor) OnRead(ep *poller.NetPoller, fd int) {
	ld.mux.Lock()
	l, ok := ld.listeners[fd]
	ld.mux.Unlock()

	if ok {
		_ = ld.accept(l)
	}
}

func (ld *acceptor) OnClose(ep *poller.NetPoller, err error) {
	ld.close()
}

func (ld *acceptor) accept(l *listener) error {

	// udp server incoming
	if l.udp != nil {
		return ld.onReadUDP(l)
	}

	for {
		nfd, sa, err := syscall.Accept(l.fd)
		if nil != err {
			if errors.Is(err, syscall.EAGAIN) || errors.Is(err, syscall.EWOULDBLOCK) {
				return nil
			}
			return err
		}

		if err = socket.SetNonblock(nfd, true); err != nil {
			_ = syscall.Close(nfd)
			return err
		}

		if strings.HasPrefix(l.network, "tcp") {
			_ = socket.SetNoDelay(nfd, true)
			_ = socket.SetKeepAlive(nfd, true)
			_ = socket.SetKeepAlivePeriod(nfd, int(defaultTCPKeepAlive/time.Second))
		}

		fdc := &fdConn{}
		fdc.fd = nfd
		fdc.events = ld.events
		fdc.loop = ld.events.selectLoop(nfd)
		fdc.localAddr = l.laddr
		fdc.remoteAddr = socket.SockaddrToAddr(sa, false)

		return ld.events.addConn(fdc)
	}
}

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
		l.udpSvr.loop = ld.loop
		l.udpSvr.events = ld.events
		l.udpSvr.isUdp = true
		l.udpSvr.udpConns = make(map[string]*fdConn)

		return ld.loop.addConn(l.udpSvr)
	}

	return ld.loop.listen(l.fd)
}

func (ld *acceptor) closeListener(l *listener) {
	if l.udpSvr != nil {
		_ = l.udpSvr.Close()
	}
	if l.fd > 0 {
		_ = syscall.Close(l.fd)
	}
	if l.file != nil {
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
	defer ld.mux.Unlock()

	for fd, l := range ld.listeners {
		delete(ld.listeners, fd)
		ld.closeListener(l)
	}
}

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
