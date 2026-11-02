//go:build linux && !386 && !s390x

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
	"unsafe"

	"golang.org/x/sys/unix"
)

// AcceptPeer accepts like Accept. For an IP listener it returns the peer as a
// value read straight from the kernel's sockaddr, so accepting allocates
// nothing; other listeners get the Sockaddr Accept returns. The descriptor is
// non-blocking and close-on-exec, as Accept makes it. 386 and s390x reach
// accept4 through socketcall on the kernels Go supports, so they take the
// generic path in accept_peer_other.go.
func AcceptPeer(fd int, ip bool) (int, netip.AddrPort, syscall.Sockaddr, error) {
	if !ip {
		nfd, sa, err := Accept(fd)
		return nfd, netip.AddrPort{}, sa, err
	}
	var rsa syscall.RawSockaddrAny
	size := uint32(syscall.SizeofSockaddrAny)
	var r uintptr
	var errno syscall.Errno
	r, _, errno = unix.Syscall6(unix.SYS_ACCEPT4, uintptr(fd), uintptr(unsafe.Pointer(&rsa)),
		uintptr(unsafe.Pointer(&size)), syscall.SOCK_NONBLOCK|syscall.SOCK_CLOEXEC, 0, 0)
	if errno != 0 {
		return -1, netip.AddrPort{}, nil, errno
	}
	nfd := int(r)
	switch rsa.Addr.Family {
	case syscall.AF_INET:
		p := (*syscall.RawSockaddrInet4)(unsafe.Pointer(&rsa))
		return nfd, netip.AddrPortFrom(netip.AddrFrom4(p.Addr), networkPort(p.Port)), nil, nil
	case syscall.AF_INET6:
		p := (*syscall.RawSockaddrInet6)(unsafe.Pointer(&rsa))
		port := networkPort(p.Port)
		if p.Scope_id != 0 {
			// A zone needs its interface name; the rare link-local peer takes
			// the slow conversion.
			sa := &syscall.SockaddrInet6{Port: int(port), ZoneId: p.Scope_id, Addr: p.Addr}
			return nfd, SockaddrToAddrPort(sa), nil, nil
		}
		return nfd, netip.AddrPortFrom(netip.AddrFrom16(p.Addr), port), nil, nil
	}
	// An address family Accept would not convert either: it closes the
	// descriptor and reports the error, and so does this.
	_ = syscall.Close(nfd)
	return -1, netip.AddrPort{}, nil, syscall.EAFNOSUPPORT
}
