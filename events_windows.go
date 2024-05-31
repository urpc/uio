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
	"net"
	"net/url"
	"strings"
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
func (ev *Events) Dial(addr string, ctx interface{}) (Conn, error) {

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

	conn, err := net.Dial(u.Scheme, address)
	if nil != err {
		return nil, err
	}

	lAddr := conn.LocalAddr()
	rAddr := conn.RemoteAddr()

	fdc := &fdConn{}

	if udpConn, ok := conn.(*net.UDPConn); ok {
		fdc.udp = udpConn
	} else {
		fdc.conn = conn
	}

	fdc.ctx = ctx
	fdc.localAddr = lAddr
	fdc.remoteAddr = rAddr
	fdc.events = ev
	fdc.loop = ev.selectLoop(fdc.Fd())
	fdc.writeSig = make(chan struct{}, 1)

	if err = ev.addConn(fdc); nil != err {
		return nil, err
	}
	return fdc, nil
}
