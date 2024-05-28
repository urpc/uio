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
	"syscall"

	"github.com/libp2p/go-reuseport"
	"github.com/urpc/uio/internal/poller"
	"github.com/urpc/uio/internal/socket"
)

type listener struct {
	fd     int            // fd
	addr   string         // address
	laddr  net.Addr       // local listen address
	ln     net.Listener   // tcp/unix listener
	file   *os.File       // file
	udp    net.PacketConn // udp endpoint
	udpSvr *fdConn        // udp server
}

type acceptor struct {
	listeners map[int]*listener
	loop      *eventLoop
	events    *Events
}

func (ld *acceptor) OnWrite(ep *poller.NetPoller, fd int) error {
	panic("unreachable here")
}

func (ld *acceptor) OnRead(ep *poller.NetPoller, fd int) error {
	if l, ok := ld.listeners[fd]; ok {

		// udp server incoming
		if l.udp != nil {
			return ld.onReadUDP(l)
		}

		for {
			nfd, sa, err := syscall.Accept(fd)
			if nil != err {
				switch {
				case errors.Is(err, syscall.EINTR):
					continue
				case errors.Is(err, syscall.EAGAIN):
					return nil
				default:
					return err
				}
			}

			if err = syscall.SetNonblock(nfd, true); err != nil {
				fmt.Println("close fd:", nfd)
				_ = syscall.Close(nfd)
				return err
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

	panic("unknown fd")
}

func (ld *acceptor) addListen(addr string) (err error) {
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
		l.udpSvr = nil
	}
	if l.fd > 0 {
		_ = syscall.Close(l.fd)
		l.fd = -1
	}
	if l.file != nil {
		_ = l.file.Close()
		l.file = nil
	}
	if l.ln != nil {
		if _, ok := l.ln.(*net.UnixListener); ok {
			_ = os.RemoveAll(l.addr)
		}

		_ = l.ln.Close()
		l.ln = nil
	}

	if l.udp != nil {
		_ = l.udp.Close()
		l.udp = nil
	}
}

func (ld *acceptor) close() {
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
	return &l, syscall.SetNonblock(l.fd, true)
}

func (ld *acceptor) onReadUDP(l *listener) error {

	udpSvrConn := ld.loop.getConn(l.fd)
	if nil == udpSvrConn {
		return errors.New("no such udp server")
	}

	return udpSvrConn.fireReadEvent()
}
