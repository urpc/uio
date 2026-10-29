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
	"syscall"
	"testing"
	"unsafe"

	"golang.org/x/sys/unix"
)

// The port in a raw sockaddr is network order whatever the host's byte
// order: a fixed byte swap was right on the little-endian architectures and
// sent every datagram to the wrong port on the big-endian ones.
func TestSockaddrPortNetworkOrder(t *testing.T) {
	var storage unix.RawSockaddrAny
	name, length, err := sockaddrName(&syscall.SockaddrInet4{Port: 0x1234, Addr: [4]byte{127, 0, 0, 1}}, &storage)
	if err != nil {
		t.Fatal(err)
	}
	if name != (*byte)(unsafe.Pointer(&storage)) || length != unix.SizeofSockaddrInet4 {
		t.Fatalf("the name is %p with length %d", name, length)
	}
	raw := (*unix.RawSockaddrInet4)(unsafe.Pointer(&storage))
	if raw.Family != unix.AF_INET || raw.Addr != [4]byte{127, 0, 0, 1} {
		t.Fatalf("the name is %d %v", raw.Family, raw.Addr)
	}
	port := unsafe.Slice((*byte)(unsafe.Pointer(&raw.Port)), 2)
	if port[0] != 0x12 || port[1] != 0x34 {
		t.Fatalf("the port went out as %02x%02x, want 1234", port[0], port[1])
	}

	var storage6 unix.RawSockaddrAny
	_, length6, err := sockaddrName(&syscall.SockaddrInet6{Port: 0xbeef, ZoneId: 3}, &storage6)
	if err != nil {
		t.Fatal(err)
	}
	if length6 != unix.SizeofSockaddrInet6 {
		t.Fatalf("the name has length %d", length6)
	}
	raw6 := (*unix.RawSockaddrInet6)(unsafe.Pointer(&storage6))
	if raw6.Family != unix.AF_INET6 || raw6.Scope_id != 3 {
		t.Fatalf("the name is %d %d", raw6.Family, raw6.Scope_id)
	}
	port6 := unsafe.Slice((*byte)(unsafe.Pointer(&raw6.Port)), 2)
	if port6[0] != 0xbe || port6[1] != 0xef {
		t.Fatalf("the port went out as %02x%02x, want beef", port6[0], port6[1])
	}
}

// The batched calls carry datagrams both ways: one sendmmsg writes the batch,
// one recvmmsg reads it back with each datagram's length and its sender.
func TestUDPBatchRoundTrip(t *testing.T) {
	server, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = unix.Close(server) })
	if err := unix.Bind(server, &unix.SockaddrInet4{Addr: [4]byte{127, 0, 0, 1}}); err != nil {
		t.Fatal(err)
	}
	// A datagram the batch loses never arrives, and a blocked recvmmsg would
	// hang the test: bound the wait.
	if err := unix.SetsockoptTimeval(server, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &unix.Timeval{Sec: 2}); err != nil {
		t.Fatal(err)
	}
	bound, err := unix.Getsockname(server)
	if err != nil {
		t.Fatal(err)
	}
	serverAddr := bound.(*unix.SockaddrInet4)

	client, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = unix.Close(client) })

	want := [][]byte{[]byte("one"), []byte("two"), []byte("three")}
	sent, err := SendUDPBatch(client, want, &syscall.SockaddrInet4{Addr: serverAddr.Addr, Port: serverAddr.Port})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if sent != 11 {
		t.Fatalf("sent %d bytes, want 11", sent)
	}
	// The unbound socket picks its port on the first send.
	local, err := unix.Getsockname(client)
	if err != nil {
		t.Fatal(err)
	}

	packets := make([][]byte, 4)
	receives := make([]UDPReceive, 4)
	for i := range packets {
		packets[i] = make([]byte, 32)
	}
	got, err := RecvUDPBatch(server, packets, receives)
	if err != nil {
		t.Fatalf("receive: %v", err)
	}
	if got != len(want) {
		t.Fatalf("received %d datagrams, want %d", got, len(want))
	}
	for i := range want {
		if string(packets[i][:receives[i].Len]) != string(want[i]) {
			t.Fatalf("datagram %d carries %q, want %q", i, packets[i][:receives[i].Len], want[i])
		}
		if receives[i].Addr.Port != uint16(local.(*unix.SockaddrInet4).Port) {
			t.Fatalf("datagram %d came from port %d, want %d", i, receives[i].Addr.Port, local.(*unix.SockaddrInet4).Port)
		}
	}
}

// A datagram longer than its buffer reports its original length, so a caller
// can tell a truncation from a short packet.
func TestUDPBatchReportsTruncation(t *testing.T) {
	server, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = unix.Close(server) })
	if err := unix.Bind(server, &unix.SockaddrInet4{Addr: [4]byte{127, 0, 0, 1}}); err != nil {
		t.Fatal(err)
	}
	if err := unix.SetsockoptTimeval(server, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &unix.Timeval{Sec: 2}); err != nil {
		t.Fatal(err)
	}
	bound, err := unix.Getsockname(server)
	if err != nil {
		t.Fatal(err)
	}
	serverAddr := bound.(*unix.SockaddrInet4)

	client, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = unix.Close(client) })
	if _, err := SendUDPBatch(client, [][]byte{[]byte("0123456789")}, &syscall.SockaddrInet4{Addr: serverAddr.Addr, Port: serverAddr.Port}); err != nil {
		t.Fatal(err)
	}

	packets := [][]byte{make([]byte, 4)}
	receives := make([]UDPReceive, 1)
	got, err := RecvUDPBatch(server, packets, receives)
	if err != nil {
		t.Fatal(err)
	}
	if got != 1 {
		t.Fatalf("received %d datagrams, want 1", got)
	}
	if receives[0].Len != 10 {
		t.Fatalf("the truncated datagram reports %d bytes, want its original 10", receives[0].Len)
	}
}
