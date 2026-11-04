//go:build linux && !race && (amd64 || arm64 || riscv64 || loong64 || ppc64 || ppc64le || s390x || mips64 || mips64le)

package socket

import (
	"errors"
	"testing"

	"golang.org/x/sys/unix"
)

// These are the two strategies selected at process initialization according
// to available Ps. They must agree on data, vector and datagram semantics.
func TestLinuxAccountedAndRawSocketStrategiesAgree(t *testing.T) {
	streamBefore, udpBefore := rawCallsEnabled, udpRawCallsEnabled
	defer func() { rawCallsEnabled, udpRawCallsEnabled = streamBefore, udpBefore }()
	for _, raw := range []bool{false, true} {
		name := "accounted"
		if raw {
			name = "raw"
		}
		t.Run(name, func(t *testing.T) {
			rawCallsEnabled, udpRawCallsEnabled = raw, raw
			fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_NONBLOCK, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer unix.Close(fds[0])
			defer unix.Close(fds[1])
			if n, err := Send(fds[0], []byte("abc")); n != 3 || err != nil {
				t.Fatalf("send=%d,%v", n, err)
			}
			if n, err := Writev(fds[0], [][]byte{[]byte("d"), []byte("e")}); n != 2 || err != nil {
				t.Fatalf("vector=%d,%v", n, err)
			}
			buffer := make([]byte, 5)
			for offset := 0; offset < len(buffer); {
				n, err := Recv(fds[1], buffer[offset:])
				if n <= 0 || err != nil {
					t.Fatalf("receive=%d,%v", n, err)
				}
				offset += n
			}
			if string(buffer) != "abcde" {
				t.Fatal(string(buffer))
			}
			receiver, target := contractUDP(t, unix.AF_INET)
			sender, _ := contractUDP(t, unix.AF_INET)
			if n, err := SendUDPBatch(sender, [][]byte{[]byte("one"), []byte("two")}, target); n != 6 || err != nil {
				t.Fatalf("datagram send=%d,%v", n, err)
			}
			for _, want := range []string{"one", "two"} {
				awaitContractDatagram(t, receiver)
				packets := [][]byte{make([]byte, 8)}
				receives := make([]UDPReceive, 1)
				if n, err := RecvUDPBatch(receiver, packets, receives); n != 1 || err != nil || string(packets[0][:receives[0].Len]) != want {
					t.Fatalf("datagram receive=%d,%v", n, err)
				}
			}
		})
	}
}

func TestLinuxDatagramBatchReportsPartialSuccessAndInvalidPeer(t *testing.T) {
	receiver, target := contractUDP(t, unix.AF_INET)
	sender, _ := contractUDP(t, unix.AF_INET)
	if n, err := SendUDPBatch(sender, [][]byte{[]byte("accepted"), make([]byte, 65508)}, target); n != 8 || err != nil {
		t.Fatalf("partial send=%d,%v", n, err)
	}
	awaitContractDatagram(t, receiver)
	var received UDPReceive
	packet := make([]byte, 16)
	if n, err := RecvUDP(receiver, packet, &received); n != 8 || err != nil {
		t.Fatalf("accepted packet=%d,%v", n, err)
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
	if n, err := RecvUDPBatch(fds[1], [][]byte{make([]byte, 1)}, make([]UDPReceive, 1)); n != 0 || !errors.Is(err, unix.EAFNOSUPPORT) {
		t.Fatalf("non-IP batch=%d,%v", n, err)
	}
}
