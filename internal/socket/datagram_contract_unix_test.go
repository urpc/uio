//go:build linux || darwin || netbsd || freebsd || openbsd || dragonfly

package socket

import (
	"errors"
	"net"
	"net/netip"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func contractUDP(t *testing.T, family int) (int, syscall.Sockaddr) {
	t.Helper()
	fd, err := unix.Socket(family, unix.SOCK_DGRAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = unix.Close(fd) })
	var address unix.Sockaddr = &unix.SockaddrInet4{Addr: [4]byte{127, 0, 0, 1}}
	if family == unix.AF_INET6 {
		address = &unix.SockaddrInet6{Addr: [16]byte{15: 1}}
	}
	if err := unix.Bind(fd, address); err != nil {
		t.Fatal(err)
	}
	bound, err := unix.Getsockname(fd)
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.SetNonblock(fd, true); err != nil {
		t.Fatal(err)
	}
	switch a := bound.(type) {
	case *unix.SockaddrInet4:
		return fd, &syscall.SockaddrInet4{Port: a.Port, Addr: a.Addr}
	case *unix.SockaddrInet6:
		return fd, &syscall.SockaddrInet6{Port: a.Port, Addr: a.Addr, ZoneId: a.ZoneId}
	}
	t.Fatal("unexpected datagram family")
	return 0, nil
}

func awaitContractDatagram(t *testing.T, fd int) {
	t.Helper()
	fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
	if n, err := unix.Poll(fds, 1000); n != 1 || err != nil || fds[0].Revents&unix.POLLIN == 0 {
		t.Fatalf("datagram readiness = %d, %v, flags=%d", n, err, fds[0].Revents)
	}
}

func TestDatagramBatchPreservesPacketBoundariesAndConnectedAddress(t *testing.T) {
	for _, family := range []int{unix.AF_INET, unix.AF_INET6} {
		name := "IPv4"
		if family == unix.AF_INET6 {
			name = "IPv6"
		}
		t.Run(name, func(t *testing.T) {
			receiver, target := contractUDP(t, family)
			sender, _ := contractUDP(t, family)
			packets := [][]byte{nil, []byte("one"), {}, []byte("two")}
			if n, err := SendUDPBatch(sender, packets, target); n != 6 || err != nil {
				t.Fatalf("send=%d,%v", n, err)
			}
			buffers := [][]byte{make([]byte, 16), make([]byte, 16), make([]byte, 16)}
			receives := make([]UDPReceive, 3)
			got := 0
			deadline := time.Now().Add(time.Second)
			for got < 2 && time.Now().Before(deadline) {
				n, err := RecvUDPBatch(receiver, buffers[got:], receives[got:])
				if err != nil && !errors.Is(err, unix.EAGAIN) {
					t.Fatal(err)
				}
				got += n
				if n == 0 {
					time.Sleep(time.Millisecond)
				}
			}
			if got != 2 || string(buffers[0][:receives[0].Len]) != "one" || string(buffers[1][:receives[1].Len]) != "two" {
				t.Fatalf("received=%d packets=%q", got, buffers)
			}
			if receives[0].Addr.Port == 0 || receives[0].Addr != receives[1].Addr {
				t.Fatal("batch lost sender identity")
			}
			if n, err := RecvUDPBatch(receiver, buffers, receives); n != 0 || !errors.Is(err, unix.EAGAIN) {
				t.Fatalf("empty receive=%d,%v", n, err)
			}
			var connected unix.Sockaddr
			switch a := target.(type) {
			case *syscall.SockaddrInet4:
				connected = &unix.SockaddrInet4{Port: a.Port, Addr: a.Addr}
			case *syscall.SockaddrInet6:
				connected = &unix.SockaddrInet6{Port: a.Port, Addr: a.Addr, ZoneId: a.ZoneId}
			}
			if err := unix.Connect(sender, connected); err != nil {
				t.Fatal(err)
			}
			if n, err := SendUDPBatch(sender, [][]byte{[]byte("connected")}, nil); n != 9 || err != nil {
				t.Fatalf("connected send=%d,%v", n, err)
			}
			awaitContractDatagram(t, receiver)
			if n, err := RecvUDPBatch(receiver, buffers[:1], receives[:1]); n != 1 || err != nil || string(buffers[0][:receives[0].Len]) != "connected" {
				t.Fatalf("connected receive=%d,%v", n, err)
			}
			if f, err := SocketFamily(receiver); err != nil || (family == unix.AF_INET && f != 4) || (family == unix.AF_INET6 && f != 6) {
				t.Fatalf("socket family=%d,%v", f, err)
			}
		})
	}
	if n, err := SendUDPBatch(-1, nil, nil); n != 0 || err != nil {
		t.Fatalf("empty send=%d,%v", n, err)
	}
	if n, err := SendUDPBatch(-1, [][]byte{[]byte("x")}, nil); n != 0 || !errors.Is(err, unix.EBADF) {
		t.Fatalf("invalid send=%d,%v", n, err)
	}
	if n, err := SendUDPBatch(-1, [][]byte{[]byte("x")}, &syscall.SockaddrInet4{}); n != 0 || !errors.Is(err, unix.EBADF) {
		t.Fatalf("invalid addressed send=%d,%v", n, err)
	}
	if n, err := RecvUDPBatch(-1, nil, nil); n != 0 || err != nil {
		t.Fatalf("empty receive=%d,%v", n, err)
	}
	if _, err := RecvUDPBatch(-1, [][]byte{make([]byte, 1)}, make([]UDPReceive, 1)); !errors.Is(err, unix.EBADF) {
		t.Fatalf("invalid receive=%v", err)
	}
}

func TestDatagramReceiveZeroBufferAndNonIPPeer(t *testing.T) {
	receiver, target := contractUDP(t, unix.AF_INET)
	sender, _ := contractUDP(t, unix.AF_INET)
	if err := syscall.Sendto(sender, nil, 0, target); err != nil {
		t.Fatal(err)
	}
	awaitContractDatagram(t, receiver)
	var received UDPReceive
	if n, err := RecvUDP(receiver, nil, &received); n != 0 || err != nil || received.Addr.Family != 4 {
		t.Fatalf("empty datagram=%d,%v,%+v", n, err, received.Addr)
	}
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_DGRAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fds[0])
	defer unix.Close(fds[1])
	if _, err := unix.Write(fds[0], []byte("x")); err != nil {
		t.Fatal(err)
	}
	received = UDPReceive{}
	if _, err := RecvUDP(fds[1], make([]byte, 1), &received); !errors.Is(err, unix.EAFNOSUPPORT) {
		t.Fatalf("non-IP peer=%v", err)
	}
	if _, err := SocketFamily(fds[0]); !errors.Is(err, unix.EAFNOSUPPORT) {
		t.Fatalf("non-IP socket=%v", err)
	}
	if _, err := SocketFamily(-1); !errors.Is(err, unix.EBADF) {
		t.Fatalf("invalid socket=%v", err)
	}
	fds2, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fds2[0])
	defer unix.Close(fds2[1])
	if n, err := Send(fds2[0], []byte("stream")); n != 6 || err != nil {
		t.Fatalf("stream send=%d,%v", n, err)
	}
	buffer := make([]byte, 6)
	if n, err := Recv(fds2[1], buffer); n != 6 || err != nil || string(buffer) != "stream" {
		t.Fatalf("stream receive=%d,%v,%q", n, err, buffer)
	}
}

func TestUDPAddressConversionsPreserveZonesAndRejectMismatchedFamilies(t *testing.T) {
	interfaces, err := net.Interfaces()
	if err != nil || len(interfaces) == 0 {
		t.Fatalf("interfaces=%v,%v", interfaces, err)
	}
	iface := interfaces[0]
	for _, zone := range []string{iface.Name, "99999999"} {
		addr := netip.AddrPortFrom(netip.MustParseAddr("fe80::1").WithZone(zone), 53)
		key, err := UDPAddressFor(6, addr)
		if err != nil {
			t.Fatal(err)
		}
		if key.Family != 6 || key.Port != 53 {
			t.Fatal(key)
		}
		if zone == iface.Name && (key.Zone != uint32(iface.Index) || key.NetAddr().Zone != iface.Name) {
			t.Fatal("named zone did not round trip")
		}
		if zone != "99999999" {
			continue
		}
		if key.NetAddr().Zone != "" {
			t.Fatal("unknown interface acquired a zone name")
		}
	}
	for _, address := range []string{"127.0.0.1:53", "[::ffff:127.0.0.1]:53"} {
		key, err := UDPAddressFor(4, netip.MustParseAddrPort(address))
		if err != nil || key.Family != 4 || !key.NetAddr().IP.Equal(net.IPv4(127, 0, 0, 1)) {
			t.Fatalf("mapped v4=%+v,%v", key, err)
		}
	}
	for _, test := range []struct {
		family  uint8
		address netip.AddrPort
		want    error
	}{
		{4, netip.AddrPort{}, unix.EINVAL},
		{4, netip.MustParseAddrPort("[::1]:53"), unix.EAFNOSUPPORT},
		{9, netip.MustParseAddrPort("127.0.0.1:53"), unix.EAFNOSUPPORT},
	} {
		if _, err := UDPAddressFor(test.family, test.address); !errors.Is(err, test.want) {
			t.Fatalf("family=%d: %v", test.family, err)
		}
	}
	if _, err := UDPAddressFor(6, netip.AddrPortFrom(netip.MustParseAddr("fe80::1").WithZone("uio-no-such-interface"), 53)); err == nil {
		t.Fatal("unknown named zone accepted")
	}
	if (UDPAddress{}).Sockaddr() != nil {
		t.Fatal("invalid address produced a sockaddr")
	}
	v6 := &syscall.SockaddrInet6{Addr: [16]byte{15: 1}, Port: 53, ZoneId: uint32(iface.Index)}
	if a := SockaddrToAddrPort(v6); a.Addr().Zone() != iface.Name || a.Port() != 53 {
		t.Fatal(a)
	}
	if a := SockaddrToAddr(v6, true).(*net.UDPAddr); a.Zone != iface.Name || a.Port != 53 {
		t.Fatal(a)
	}
	v6.ZoneId = 99999999
	if a := SockaddrToAddrPort(v6); a.Addr().Zone() != "" {
		t.Fatal(a)
	}
	if SockaddrToAddrPort(&syscall.SockaddrUnix{Name: "local"}).IsValid() || SockaddrToAddrPort(nil).IsValid() {
		t.Fatal("non-IP address converted to IP peer")
	}
	if got := ClassifyAcceptError(errors.New("listener failure")); got != AcceptBackoff {
		t.Fatal(got)
	}
	if got := ClassifyAcceptError(syscall.EIO); got != AcceptBackoff {
		t.Fatal(got)
	}
	var received UDPReceive
	received.raw.Addr.Family = unix.AF_INET6
	if err := received.parse(unix.SizeofSockaddrInet6 - 1); !errors.Is(err, unix.EINVAL) {
		t.Fatal(err)
	}
}

func TestKeepAliveRejectsInvalidDescriptorsAndDatagramProtocol(t *testing.T) {
	if err := SetKeepAlivePeriod(-1, 10); !errors.Is(err, unix.EBADF) {
		t.Fatalf("keepalive on invalid descriptor = %v", err)
	}
	fd, _ := contractUDP(t, unix.AF_INET)
	if err := SetKeepAlivePeriod(fd, 10); err == nil {
		t.Fatal("datagram socket accepted TCP keepalive parameters")
	}
}

func TestWritevPartialProgressKeepsAnAccurateRetryBoundary(t *testing.T) {
	if _, _, _, err := AcceptPeer(-1, true); !errors.Is(err, unix.EBADF) {
		t.Fatalf("invalid accept peer = %v", err)
	}
	vec := make([][]byte, writevBatchLimit+1)
	for i := range vec {
		vec[i] = make([]byte, 1024)
	}
	if _, err := Writev(-1, vec); !errors.Is(err, unix.EBADF) {
		t.Fatalf("invalid vector send = %v", err)
	}
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fds[0])
	defer unix.Close(fds[1])
	if err := unix.SetNonblock(fds[0], true); err != nil {
		t.Fatal(err)
	}
	if err := unix.SetsockoptInt(fds[0], unix.SOL_SOCKET, unix.SO_SNDBUF, 4096); err != nil {
		t.Fatal(err)
	}
	n, err := Writev(fds[0], vec)
	if err != nil || n <= 0 || n >= len(vec)*1024 {
		t.Fatalf("partial vector = %d,%v", n, err)
	}
	got := make([]byte, n)
	for offset := 0; offset < len(got); {
		n, err := unix.Read(fds[1], got[offset:])
		if err != nil || n == 0 {
			t.Fatalf("read partial send=%d,%v", n, err)
		}
		offset += n
	}
}

func TestWritevRetainsEarlierBatchCountWhenLaterDatagramFails(t *testing.T) {
	receiver, target := contractUDP(t, unix.AF_INET)
	sender, _ := contractUDP(t, unix.AF_INET)
	a := target.(*syscall.SockaddrInet4)
	if err := unix.Connect(sender, &unix.SockaddrInet4{Port: a.Port, Addr: a.Addr}); err != nil {
		t.Fatal(err)
	}
	vec := make([][]byte, writevBatchLimit+1)
	for i := 0; i < writevBatchLimit; i++ {
		vec[i] = []byte{'x'}
	}
	vec[writevBatchLimit] = make([]byte, 65508) // exceeds an IPv4 datagram's payload limit
	if n, err := Writev(sender, vec); n != writevBatchLimit || err != nil {
		t.Fatalf("completed batch count=%d,%v", n, err)
	}
	awaitContractDatagram(t, receiver)
	packet := make([]byte, 2048)
	var received UDPReceive
	if n, err := RecvUDP(receiver, packet, &received); n != writevBatchLimit || err != nil {
		t.Fatalf("first datagram=%d,%v", n, err)
	}
	if _, err := RecvUDP(receiver, packet, &received); !errors.Is(err, unix.EAGAIN) {
		t.Fatalf("failed datagram was sent: %v", err)
	}
}
