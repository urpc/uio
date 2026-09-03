//go:build (linux || darwin || netbsd || freebsd || openbsd || dragonfly) && !stdio

package uio

import (
	"testing"
	"time"
	"unsafe"
)

func TestUnixConnectionColdStateIsLazy(t *testing.T) {
	if unsafe.Sizeof(uintptr(0)) == 8 {
		const maximum = uintptr(240)
		if size := unsafe.Sizeof(fdConn{}); size > maximum {
			t.Fatalf("fdConn size = %d bytes, want at most %d", size, maximum)
		}
	}

	conn := &fdConn{}
	if conn.udp != nil || conn.deadlines != nil || conn.isDatagram() {
		t.Fatal("new TCP connection allocated cold state")
	}

	var deadlineErr error
	allocations := testing.AllocsPerRun(1000, func() {
		deadlineErr = conn.applyDeadline(deadlineBoth, time.Time{})
	})
	if deadlineErr != nil {
		t.Fatal(deadlineErr)
	}
	if allocations != 0 || conn.deadlines != nil {
		t.Fatalf("clearing unused deadlines allocated %.2f objects", allocations)
	}

	if err := conn.applyDeadline(deadlineRead, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if conn.deadlines == nil || conn.deadlines.readTimer == nil {
		t.Fatal("nonzero deadline did not initialize deadline state")
	}
	conn.stopDeadlines()

	conn.udp = &unixUDPState{}
	if !conn.isDatagram() {
		t.Fatal("UDP state did not mark connection as datagram")
	}
}
