//go:build linux && (amd64 || arm64 || riscv64 || loong64 || ppc64 || ppc64le || s390x || mips64 || mips64le)

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
	"encoding/binary"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// mmsghdr mirrors the kernel's struct mmsghdr: one message header and the
// bytes its datagram carried. Go pads the struct the way the C compiler does,
// so its size matches on every architecture.
type mmsghdr struct {
	Hdr unix.Msghdr
	Len uint32
}

const (
	// udpBatchBound is the most datagrams one batched syscall carries here; the
	// kernel takes up to UIO_MAXIOV, and this keeps the stack arrays modest.
	udpBatchBound = 64

	// udpGSOMaxPayload bounds one UDP_SEGMENT super-datagram below the IPv4/IPv6
	// 65535-byte total length limit minus maximum IP+UDP headers.
	udpGSOMaxPayload = 64512

	// udpGSOMaxSegments is the Linux kernel's UDP_MAX_SEGMENTS limit (64).
	udpGSOMaxSegments = 64

	// udpGSOMinSegment is the smallest datagram size coalesced into a GSO
	// super-datagram: smaller packets stay in plain sendmmsg so tiny frames
	// never pay skb_segment splitting overhead in the kernel.
	udpGSOMinSegment = 512
)

// udpGSOCmsg is the 24-byte stack control message carrying a uint16
// UDP_SEGMENT (SOL_UDP=17, UDP_SEGMENT=103) GSO segment size on 64-bit Linux.
type udpGSOCmsg struct {
	Hdr unix.Cmsghdr
	Seg uint16
	_   [6]byte
}

// SendUDPBatch sends each packet as its own datagram to remote — nil for a
// connected socket — with sendmmsg, coalescing runs of equal-sized datagrams
// (plus an optional shorter trailing datagram) into a single Linux UDP_SEGMENT
// (GSO) super-datagram so the kernel traverses the UDP/IP/routing stack once
// per run instead of once per MTU packet. If the socket or kernel does not
// support UDP_SEGMENT, it transparently falls back to one mmsghdr per datagram.
func SendUDPBatch(fd int, packets [][]byte, remote syscall.Sockaddr) (int, error) {
	var storage unix.RawSockaddrAny
	name, nameLen, err := sockaddrName(remote, &storage)
	if err != nil {
		return 0, err
	}
	if len(packets) == 1 {
		pkt := packets[0]
		if len(pkt) == 0 {
			return 0, nil
		}
		iov := unix.Iovec{Base: &pkt[0], Len: uint64(len(pkt))}
		hdr := [1]mmsghdr{{Hdr: unix.Msghdr{Name: name, Namelen: nameLen, Iov: &iov, Iovlen: 1}}}
		sent, err := sendmmsg(fd, hdr[:])
		if sent > 0 {
			return len(pkt), err
		}
		return 0, err
	}

	_, isUnix := remote.(*syscall.SockaddrUnix)
	useGSO := !isUnix

	var hdrs [udpBatchBound]mmsghdr
	var iovs [udpBatchBound]unix.Iovec
	var cmsgs [udpBatchBound]udpGSOCmsg
	var msgBytes [udpBatchBound]int

	sentBytes, cursor := 0, 0
	for cursor < len(packets) {
		roundStart := cursor
		batch, iovUsed, usedGSO := 0, 0, false
		for cursor < len(packets) && batch < udpBatchBound && iovUsed < udpBatchBound {
			pkt := packets[cursor]
			cursor++
			if len(pkt) == 0 {
				continue
			}
			gsoSize := len(pkt)
			iovStart := iovUsed
			iovs[iovUsed] = unix.Iovec{Base: &pkt[0], Len: uint64(gsoSize)}
			iovUsed++
			totalBytes := gsoSize
			segs := 1

			if useGSO && gsoSize >= udpGSOMinSegment && gsoSize+gsoSize <= udpGSOMaxPayload {
				for cursor < len(packets) && iovUsed < udpBatchBound && segs < udpGSOMaxSegments {
					next := packets[cursor]
					if len(next) == 0 {
						cursor++
						continue
					}
					if len(next) > gsoSize || totalBytes+len(next) > udpGSOMaxPayload {
						break
					}
					cursor++
					iovs[iovUsed] = unix.Iovec{Base: &next[0], Len: uint64(len(next))}
					iovUsed++
					totalBytes += len(next)
					segs++
					if len(next) < gsoSize {
						// A shorter final segment terminates a GSO super-datagram.
						break
					}
				}
			}

			hdr := unix.Msghdr{
				Name:    name,
				Namelen: nameLen,
				Iov:     &iovs[iovStart],
				Iovlen:  uint64(segs),
			}
			if segs > 1 {
				usedGSO = true
				cmsgs[batch] = udpGSOCmsg{
					Hdr: unix.Cmsghdr{
						Len:   uint64(unix.CmsgLen(2)),
						Level: unix.SOL_UDP,
						Type:  unix.UDP_SEGMENT,
					},
					Seg: uint16(gsoSize),
				}
				hdr.Control = (*byte)(unsafe.Pointer(&cmsgs[batch]))
				hdr.Controllen = uint64(unsafe.Sizeof(cmsgs[batch]))
			}
			hdrs[batch] = mmsghdr{Hdr: hdr}
			msgBytes[batch] = totalBytes
			batch++
		}
		if batch == 0 {
			break
		}
		sent, err := sendmmsg(fd, hdrs[:batch])
		if sent == 0 && usedGSO && isGSOUnsupported(err) {
			useGSO = false
			cursor = roundStart
			continue
		}
		for i := 0; i < sent; i++ {
			sentBytes += msgBytes[i]
		}
		if err != nil {
			return sentBytes, err
		}
		if sent < batch {
			// The kernel took what its queue had room for: the rest are lost,
			// as a datagram transport allows.
			return sentBytes, nil
		}
	}
	return sentBytes, nil
}

func isGSOUnsupported(err error) bool {
	return err == unix.EINVAL || err == unix.EIO || err == unix.ENOPROTOOPT || err == unix.EOPNOTSUPP
}

// RecvUDPBatch receives up to min(len(packets), len(receives), udpBatchBound)
// datagrams with recvmmsg. It reports how many arrived; each receive's Len is
// the datagram's original length, which may exceed its buffer when the
// datagram was truncated.
func RecvUDPBatch(fd int, packets [][]byte, receives []UDPReceive) (int, error) {
	count := min(len(packets), len(receives), udpBatchBound)
	if count == 0 {
		return 0, nil
	}
	var hdrs [udpBatchBound]mmsghdr
	var iovs [udpBatchBound]unix.Iovec
	for i := 0; i < count; i++ {
		iovs[i] = unix.Iovec{Base: &packets[i][0], Len: uint64(len(packets[i]))}
		receives[i].raw.Addr.Family = 0
		hdrs[i] = mmsghdr{Hdr: unix.Msghdr{
			Name:    (*byte)(unsafe.Pointer(&receives[i].raw)),
			Namelen: unix.SizeofSockaddrAny,
			Iov:     &iovs[i],
			Iovlen:  1,
		}}
	}
	got, err := recvmmsg(fd, hdrs[:count])
	if err != nil {
		return 0, err
	}
	for i := 0; i < got; i++ {
		receives[i].Len = int(hdrs[i].Len)
		if err := receives[i].parse(hdrs[i].Hdr.Namelen); err != nil {
			return i, err
		}
	}
	return got, nil
}

// sendmmsg and recvmmsg use the platform wrapper so large Linux hosts can
// keep non-blocking UDP batches on their current P, while race builds retain
// the runtime-accounted path.
func sendmmsg(fd int, messages []mmsghdr) (int, error) {
	return sendmmsgSyscall(fd, messages)
}

// recvmmsg receives without a timeout: the socket waits in the poller, so
// the call itself never blocks. MSG_TRUNC preserves the original datagram
// length for callers that need to detect truncation.
func recvmmsg(fd int, messages []mmsghdr) (int, error) {
	return recvmmsgSyscall(fd, messages)
}

// sockaddrName fills storage with remote's raw form and returns the name a
// message header carries: nil and no length for a connected socket.
func sockaddrName(remote syscall.Sockaddr, storage *unix.RawSockaddrAny) (*byte, uint32, error) {
	switch sa := remote.(type) {
	case nil:
		return nil, 0, nil
	case *syscall.SockaddrInet4:
		raw := (*unix.RawSockaddrInet4)(unsafe.Pointer(storage))
		*raw = unix.RawSockaddrInet4{Family: unix.AF_INET, Addr: sa.Addr}
		setPort(&raw.Port, uint16(sa.Port))
		return (*byte)(unsafe.Pointer(storage)), unix.SizeofSockaddrInet4, nil
	case *syscall.SockaddrInet6:
		raw := (*unix.RawSockaddrInet6)(unsafe.Pointer(storage))
		*raw = unix.RawSockaddrInet6{Family: unix.AF_INET6, Addr: sa.Addr, Scope_id: sa.ZoneId}
		setPort(&raw.Port, uint16(sa.Port))
		return (*byte)(unsafe.Pointer(storage)), unix.SizeofSockaddrInet6, nil
	case *syscall.SockaddrUnix:
		raw := (*unix.RawSockaddrUnix)(unsafe.Pointer(storage))
		if len(sa.Name) == 0 || len(sa.Name) >= len(raw.Path) {
			return nil, 0, unix.EINVAL
		}
		*raw = unix.RawSockaddrUnix{Family: unix.AF_UNIX}
		if sa.Name[0] == '@' {
			raw.Path[0] = 0
			for i := 1; i < len(sa.Name); i++ {
				raw.Path[i] = int8(sa.Name[i])
			}
			length := uint32(unsafe.Offsetof(unix.RawSockaddrUnix{}.Path)) + uint32(len(sa.Name))
			return (*byte)(unsafe.Pointer(storage)), length, nil
		}
		for i := 0; i < len(sa.Name); i++ {
			raw.Path[i] = int8(sa.Name[i])
		}
		length := uint32(unsafe.Offsetof(unix.RawSockaddrUnix{}.Path)) + uint32(len(sa.Name)) + 1
		return (*byte)(unsafe.Pointer(storage)), length, nil
	}
	return nil, 0, unix.EAFNOSUPPORT
}

// setPort lays a port out in network byte order in a raw sockaddr's port
// field. A sockaddr carries the port big-endian whatever the host's own byte
// order, so the bytes are written as such rather than through a native
// uint16 — a swap here was right on the little-endian architectures and sent
// every datagram to the wrong port on the big-endian ones.
func setPort(port *uint16, value uint16) {
	b := unsafe.Slice((*byte)(unsafe.Pointer(port)), 2)
	binary.BigEndian.PutUint16(b, value)
}
