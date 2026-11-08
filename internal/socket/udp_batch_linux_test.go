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
	"fmt"
	"syscall"
	"testing"
	"unsafe"

	"golang.org/x/sys/unix"
)

func BenchmarkUDPBatchRoundTrip(b *testing.B) {
	for _, batchSize := range []int{1, 8, 32, 64} {
		b.Run(fmt.Sprintf("batch=%d", batchSize), func(b *testing.B) {
			server, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM, 0)
			if err != nil {
				b.Fatal(err)
			}
			defer unix.Close(server)
			if err := unix.Bind(server, &unix.SockaddrInet4{Addr: [4]byte{127, 0, 0, 1}}); err != nil {
				b.Fatal(err)
			}
			bound, err := unix.Getsockname(server)
			if err != nil {
				b.Fatal(err)
			}
			serverAddr := bound.(*unix.SockaddrInet4)

			client, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM, 0)
			if err != nil {
				b.Fatal(err)
			}
			defer unix.Close(client)
			remote := &syscall.SockaddrInet4{Addr: serverAddr.Addr, Port: serverAddr.Port}
			packets := make([][]byte, batchSize)
			receives := make([]UDPReceive, batchSize)
			buffers := make([][]byte, batchSize)
			for i := range packets {
				packets[i] = make([]byte, 1200)
				buffers[i] = make([]byte, 1200)
			}

			b.SetBytes(int64(batchSize * 1200))
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				sent, err := SendUDPBatch(client, packets, remote)
				if err != nil || sent != batchSize*1200 {
					b.Fatalf("sendmmsg sent %d/%d: %v", sent, batchSize*1200, err)
				}
				got, err := RecvUDPBatch(server, buffers, receives)
				if err != nil || got != batchSize {
					b.Fatalf("recvmmsg got %d/%d: %v", got, batchSize, err)
				}
			}
		})
	}
}

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

func TestUDPBatchGSOCoalescingAndUnixgramFallback(t *testing.T) {
	// 1. UDPv4 GSO run: 8 equal 1200B segments + 1 trailing 500B segment + 1 larger 1300B segment.
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

	var sendPkts [][]byte
	wantTotal := 0
	for i := range 8 {
		b := make([]byte, 1200)
		b[0], b[len(b)-1] = byte(i+1), byte(i+100)
		sendPkts = append(sendPkts, b)
		wantTotal += len(b)
	}
	sendPkts = append(sendPkts, nil) // empty packet inside GSO run should be skipped
	tail := make([]byte, 500)
	tail[0], tail[len(tail)-1] = 0xAA, 0xBB
	sendPkts = append(sendPkts, tail)
	wantTotal += len(tail)

	after := make([]byte, 1300)
	after[0], after[len(after)-1] = 0xCC, 0xDD
	sendPkts = append(sendPkts, after)
	wantTotal += len(after)

	remote := &syscall.SockaddrInet4{Addr: serverAddr.Addr, Port: serverAddr.Port}
	sent, err := SendUDPBatch(client, sendPkts, remote)
	if err != nil || sent != wantTotal {
		t.Fatalf("SendUDPBatch GSO sent=%d, err=%v; want %d", sent, err, wantTotal)
	}

	recvBufs := make([][]byte, 10)
	recvs := make([]UDPReceive, 10)
	for i := range recvBufs {
		recvBufs[i] = make([]byte, 1500)
	}
	gotTotal := 0
	var gotLens []int
	for len(gotLens) < 10 {
		rem := 10 - len(gotLens)
		n, rerr := RecvUDPBatch(server, recvBufs[:rem], recvs[:rem])
		if rerr != nil {
			t.Fatalf("RecvUDPBatch after GSO: %v (got %d/10)", rerr, len(gotLens))
		}
		for i := range n {
			gotTotal += recvs[i].Len
			gotLens = append(gotLens, recvs[i].Len)
		}
	}
	if gotTotal != wantTotal {
		t.Fatalf("received %d bytes across %v, want %d", gotTotal, gotLens, wantTotal)
	}

	// 2. Real-kernel GSO fallback: when IP_PMTUDISC_DONT is set on an IPv4 UDP socket
	// (or on a socket that rejects UDP_SEGMENT with EINVAL/EIO), the kernel refuses
	// UDP_SEGMENT with EINVAL and SendUDPBatch must transparently retry without GSO!
	noGSOClient, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = unix.Close(noGSOClient) })
	_ = unix.SetsockoptInt(noGSOClient, unix.IPPROTO_IP, unix.IP_MTU_DISCOVER, unix.IP_PMTUDISC_DONT)

	if !isGSOUnsupported(unix.EINVAL) || !isGSOUnsupported(unix.EIO) || isGSOUnsupported(nil) {
		t.Fatal("isGSOUnsupported classification mismatch")
	}
	fbPkts := [][]byte{make([]byte, 600), make([]byte, 600)}
	fbSent, err := SendUDPBatch(noGSOClient, fbPkts, remote)
	if err != nil || fbSent != 1200 {
		t.Fatalf("IP_PMTUDISC_DONT fallback sent=%d, err=%v; want 1200, nil", fbSent, err)
	}
	for gotFB := 0; gotFB < 2; {
		n, rerr := RecvUDPBatch(server, recvBufs[:2-gotFB], recvs[:2-gotFB])
		if rerr != nil {
			t.Fatalf("RecvUDPBatch after fallback: %v", rerr)
		}
		gotFB += n
	}

	// Trigger EINVAL fallback branch on an AF_UNIX datagram socket given an IPv4 remote.
	ufds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_DGRAM, 0)
	if err == nil {
		defer unix.Close(ufds[0])
		defer unix.Close(ufds[1])
		_, _ = SendUDPBatch(ufds[0], fbPkts, remote)
	}
}
