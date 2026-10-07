//go:build linux && !stdio

package poller

import (
	"net"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

// Urgent data raises EPOLLPRI, which wakes the reader but ends nothing:
// reporting it as a hangup would cost every later read round a syscall.
func TestNetPollerUrgentDataIsNotHangup(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, _ := listener.Accept()
		accepted <- conn
	}()
	client, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	server := <-accepted
	if server == nil {
		t.Fatal("accept failed")
	}
	defer server.Close()

	poller, err := NewNetPoller()
	if err != nil {
		t.Fatal(err)
	}
	defer poller.Close(nil)
	serverFD := connFD(t, server)
	if err = poller.Register(serverFD, Readable, true, 0); err != nil {
		t.Fatal(err)
	}
	if err = unix.Sendto(connFD(t, client), []byte{'!'}, unix.MSG_OOB, nil); err != nil {
		t.Fatal(err)
	}
	var events [4]Event
	n, err := poller.Wait(events[:], 1000)
	if n != 1 || err != nil {
		t.Fatalf("Wait = %#v, %v; want the urgent data's event", events[:n], err)
	}
	if got := events[0].Events; got&ReadEvents == 0 || got&HangupEvents != 0 {
		t.Fatalf("urgent data reported %b, want ReadEvents without HangupEvents", got)
	}
}

// connFD returns conn's descriptor, which stays valid while conn is open.
func connFD(t *testing.T, conn net.Conn) int {
	t.Helper()
	raw, err := conn.(syscall.Conn).SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	fd := -1
	if err = raw.Control(func(descriptor uintptr) { fd = int(descriptor) }); err != nil {
		t.Fatal(err)
	}
	return fd
}
