//go:build darwin || netbsd || freebsd || openbsd || dragonfly || (linux && (386 || s390x))

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

package socket

import (
	"net/netip"
	"syscall"
)

// AcceptPeer accepts like Accept. For an IP listener it also returns the peer
// as a value; other listeners get the Sockaddr Accept returns.
func AcceptPeer(fd int, ip bool) (int, netip.AddrPort, syscall.Sockaddr, error) {
	nfd, sa, err := Accept(fd)
	if err != nil || !ip {
		return nfd, netip.AddrPort{}, sa, err
	}
	return nfd, SockaddrToAddrPort(sa), nil, nil
}
