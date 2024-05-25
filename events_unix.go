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
	"net"
	"syscall"

	"github.com/urpc/uio/internal/socket"
)

func (ev *Events) Dial(network, addr string) (Conn, error) {

	conn, err := net.Dial(network, addr)
	if nil != err {
		return nil, err
	}

	lAddr := conn.LocalAddr()
	rAddr := conn.RemoteAddr()

	nfd, err := socket.DupNetConn(conn)

	// close old fd
	_ = conn.Close()

	if nil != err {
		return nil, err // dup failed
	}

	if err = syscall.SetNonblock(nfd, true); nil != err {
		_ = syscall.Close(nfd)
		return nil, err
	}

	fdc := &fdConn{
		fd:         nfd,
		localAddr:  lAddr,
		remoteAddr: rAddr,
		loopIdx:    ev.selectLoop(nfd),
		events:     ev,
	}

	if err = ev.addConn(fdc); nil != err {
		return nil, err
	}
	return fdc, nil
}
