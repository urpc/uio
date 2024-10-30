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
	"net"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/libp2p/go-reuseport"
	"github.com/urpc/uio/internal/poller"
)

type listener struct {
	addr   string
	ln     net.Listener
	udp    net.PacketConn
	udpSvr *fdConn
}

type acceptor struct {
	mux       sync.Mutex
	listeners map[string]*listener
	loop      *eventLoop
	events    *Events
}

func (ld *acceptor) OnWrite(ep *poller.NetPoller, fd int) {
	panic("implement me")
}

func (ld *acceptor) OnRead(ep *poller.NetPoller, fd int) {
	panic("implement me")
}

func (ld *acceptor) OnClose(ep *poller.NetPoller, err error) {
	ld.close()
}

func (ld *acceptor) addListen(addr string) (err error) {
	ld.mux.Lock()
	defer ld.mux.Unlock()

	if nil == ld.listeners {
		ld.listeners = make(map[string]*listener)
	}

	var l *listener
	if l, err = ld.listen(addr, ld.events.ReusePort); nil != err {
		if nil != l {
			ld.closeListener(l)
		}
		return err
	}

	ld.listeners[addr] = l

	if l.udp != nil {
		l.udpSvr = &fdConn{}
		l.udpSvr.loop = ld.loop
		l.udpSvr.events = ld.events
		l.udpSvr.udp = l.udp.(*net.UDPConn)
		l.udpSvr.udpConns = make(map[string]*fdConn)

		go l.udpSvr.listenUDP()
		return nil
	}

	go func() {
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
			fdc.loop = ld.events.selectLoop(fdc.Fd())
			fdc.localAddr = conn.LocalAddr()
			fdc.remoteAddr = conn.RemoteAddr()
			fdc.writeSig = make(chan struct{}, 1)

			_ = ld.events.addConn(fdc)
		}
	}()

	return nil
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
		_ = l.udpSvr.Close()
	}

	if l.udp != nil {
		_ = l.udp.Close()
	}
}

func (ld *acceptor) close() {
	ld.mux.Lock()
	defer ld.mux.Unlock()

	for addr, l := range ld.listeners {
		delete(ld.listeners, addr)
		ld.closeListener(l)
	}
}
