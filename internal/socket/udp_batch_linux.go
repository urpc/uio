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

// udpBatchBound is the most datagrams one batched syscall carries here; the
// kernel takes up to UIO_MAXIOV, and this keeps the stack arrays modest.
const udpBatchBound = 64

// SendUDPBatch sends each packet as its own datagram to remote — nil for a
// connected socket — with sendmmsg, one syscall per batch of
// udpBatchBound. It reports the bytes of the datagrams the socket accepted; a
// batch the queue refused part-way reports what left before the refusal, and
// the datagrams after it are not attempted. Empty packets carry no datagram
// and are skipped.
func SendUDPBatch(fd int, packets [][]byte, remote syscall.Sockaddr) (int, error) {
	var storage unix.RawSockaddrAny
	name, nameLen, err := sockaddrName(remote, &storage)
	if err != nil {
		return 0, err
	}
	var hdrs [udpBatchBound]mmsghdr
	var iovs [udpBatchBound]unix.Iovec
	var idx [udpBatchBound]int
	sentBytes, cursor := 0, 0
	for cursor < len(packets) {
		batch := 0
		for cursor < len(packets) && batch < udpBatchBound {
			packet := packets[cursor]
			cursor++
			if len(packet) == 0 {
				continue
			}
			idx[batch] = cursor - 1
			iovs[batch] = unix.Iovec{Base: &packet[0], Len: uint64(len(packet))}
			hdrs[batch] = mmsghdr{Hdr: unix.Msghdr{Name: name, Namelen: nameLen, Iov: &iovs[batch], Iovlen: 1}}
			batch++
		}
		if batch == 0 {
			break
		}
		sent, err := sendmmsg(fd, hdrs[:batch])
		for i := 0; i < sent; i++ {
			sentBytes += len(packets[idx[i]])
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
		receives[i].raw = unix.RawSockaddrAny{}
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

// sendmmsg is the raw syscall: x/sys wraps neither sendmmsg nor recvmmsg.
func sendmmsg(fd int, messages []mmsghdr) (int, error) {
	sent, _, errno := unix.Syscall6(unix.SYS_SENDMMSG,
		uintptr(fd),
		uintptr(unsafe.Pointer(&messages[0])),
		uintptr(len(messages)),
		0, 0, 0)
	if errno != 0 {
		return int(sent), errno
	}
	return int(sent), nil
}

// recvmmsg is the raw syscall, without a timeout: the sockets wait in the
// poller, so the call itself never blocks. MSG_TRUNC makes each short buffer
// report its datagram's original length, where the caller can tell the
// truncation from a short packet.
func recvmmsg(fd int, messages []mmsghdr) (int, error) {
	got, _, errno := unix.Syscall6(unix.SYS_RECVMMSG,
		uintptr(fd),
		uintptr(unsafe.Pointer(&messages[0])),
		uintptr(len(messages)),
		uintptr(unix.MSG_TRUNC), 0, 0)
	if errno != 0 {
		return int(got), errno
	}
	return int(got), nil
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
		*raw = unix.RawSockaddrUnix{Family: unix.AF_UNIX}
		for i := 0; i < len(sa.Name) && i < len(raw.Path)-1; i++ {
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
