//go:build linux && (amd64 || arm64 || riscv64 || loong64 || ppc64 || ppc64le || s390x || mips64 || mips64le)

package socket

import (
	"errors"
	"syscall"
	"testing"
	"unsafe"

	"golang.org/x/sys/unix"
)

func TestLinuxDatagramNameEncodesUnixAndRejectsUnknownAddress(t *testing.T) {
	var storage unix.RawSockaddrAny
	name, n, err := sockaddrName(&syscall.SockaddrUnix{Name: "local-peer"}, &storage)
	if err != nil || name == nil {
		t.Fatal(err)
	}
	raw := (*unix.RawSockaddrUnix)(unsafe.Pointer(&storage))
	if raw.Family != unix.AF_UNIX || n != uint32(unsafe.Offsetof(raw.Path))+uint32(len("local-peer"))+1 {
		t.Fatalf("unix name=%+v length=%d", raw, n)
	}
	for i, c := range []byte("local-peer") {
		if byte(raw.Path[i]) != c {
			t.Fatal("unix name changed")
		}
	}
	if _, _, err := sockaddrName(&syscall.SockaddrLinklayer{}, &storage); !errors.Is(err, unix.EAFNOSUPPORT) {
		t.Fatal(err)
	}
	if _, err := SendUDPBatch(-1, [][]byte{[]byte("x")}, &syscall.SockaddrLinklayer{}); !errors.Is(err, unix.EAFNOSUPPORT) {
		t.Fatal(err)
	}
	if n, err := SendUDPBatch(-1, [][]byte{nil, {}}, nil); n != 0 || err != nil {
		t.Fatalf("empty batch=%d,%v", n, err)
	}
	if a := SockaddrToAddrPort(&syscall.SockaddrInet4{Addr: [4]byte{127, 0, 0, 1}, Port: 53}); a.String() != "127.0.0.1:53" {
		t.Fatal(a)
	}
	if _, _, _, err := AcceptPeer(-1, false); !errors.Is(err, unix.EBADF) {
		t.Fatal(err)
	}
}
