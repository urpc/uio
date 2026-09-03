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
	"strings"
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
func (ev *Events) Dial(addr string, userdata any) (Conn, error) {
	return ev.DialContext(context.Background(), addr, userdata)
}

// Adopt transfers ownership of an established stream connection to ev. The
// caller must not use conn after calling Adopt, including when Adopt returns an
// error. ev must already be serving.
func (ev *Events) Adopt(conn net.Conn, userdata any) (Conn, error) {
	if conn == nil {
		return nil, errUnsupported
	}
	if !ev.ready.Load() || ev.closing.Load() {
		_ = conn.Close()
		return nil, net.ErrClosed
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
			events:     ev,
			localAddr:  localAddr,
			remoteAddr: remoteAddr,
			userdata:   userdata,
		},
		fd: fd,
	}
	fdc.loop = ev.selectLoop(fd)
	if err = ev.addConn(fdc); err != nil {
		return nil, err
	}
	return fdc, nil
}

// DialContext connects from outside event callbacks and allows cancellation
// while resolving or establishing the network connection.
func (ev *Events) DialContext(dialCtx context.Context, addr string, userdata any) (Conn, error) {
	if !ev.ready.Load() || ev.closing.Load() {
		return nil, net.ErrClosed
	}
	if ev.currentLoop() != nil {
		return nil, ErrDialOnEventLoop
	}

	if !strings.Contains(addr, "://") {
		addr = "tcp://" + addr
	}

	u, err := url.Parse(addr)
	if nil != err {
		return nil, err
	}

	var address = u.Host
	if strings.HasPrefix(u.Scheme, "unix") {
		address = u.Path
	}

	conn, err := (&net.Dialer{}).DialContext(dialCtx, u.Scheme, address)
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

	fdc := &fdConn{}
	fdc.fd = nfd
	fdc.userdata = userdata
	fdc.localAddr = lAddr
	fdc.remoteAddr = rAddr
	fdc.events = ev
	fdc.loop = ev.selectLoop(nfd)
	if strings.HasPrefix(u.Scheme, "udp") {
		fdc.udp = &unixUDPState{}
	}

	if err = ev.addConnContext(dialCtx, fdc); nil != err {
		return nil, err
	}
	return fdc, nil
}
