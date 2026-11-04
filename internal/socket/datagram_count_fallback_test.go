//go:build darwin || netbsd || freebsd || openbsd || dragonfly || (linux && (386 || arm || mips || mipsle))

package socket

import (
	"errors"
	"testing"

	"golang.org/x/sys/unix"
)

// The syscall fallback reports a failure on the first packet that cannot be
// accepted. Its count includes earlier packets, never the syscall's -1.
func TestDatagramBatchCountExcludesFailedPacket(t *testing.T) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_DGRAM, 0)
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
	packets := [][]byte{[]byte("accepted"), make([]byte, 1<<20), []byte("not-attempted")}
	if n, err := SendUDPBatch(fds[0], packets, nil); n != len(packets[0]) || !errors.Is(err, unix.EMSGSIZE) {
		t.Fatalf("partial packet batch=%d,%v", n, err)
	}
	var packet [32]byte
	if n, err := unix.Read(fds[1], packet[:]); n != len(packets[0]) || err != nil || string(packet[:n]) != string(packets[0]) {
		t.Fatalf("accepted packet=%d,%v,%q", n, err, packet)
	}
	if err := unix.SetNonblock(fds[1], true); err != nil {
		t.Fatal(err)
	}
	if _, err := unix.Read(fds[1], packet[:]); !errors.Is(err, unix.EAGAIN) {
		t.Fatalf("failed packet batch continued after error: %v", err)
	}
}
