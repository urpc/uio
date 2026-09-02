//go:build linux || darwin || freebsd || openbsd

package socket

import (
	"bytes"
	"errors"
	"net"
	"sync/atomic"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

func TestWritevSocketpair(t *testing.T) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fds[0])
	defer unix.Close(fds[1])

	if n, err := Writev(fds[0], nil); err != nil || n != 0 {
		t.Fatalf("Writev(nil) = %d, %v", n, err)
	}
	if n, err := Writev(fds[0], [][]byte{[]byte("single")}); err != nil || n != len("single") {
		t.Fatalf("Writev(single) = %d, %v", n, err)
	}
	single := make([]byte, len("single"))
	if _, err := ioReadFull(fds[1], single); err != nil || string(single) != "single" {
		t.Fatalf("single Writev payload = %q, %v", single, err)
	}
	want := []byte("first-second-third")
	vec := [][]byte{[]byte("first-"), nil, []byte("second-"), []byte("third")}
	n, err := Writev(fds[0], vec)
	if err != nil || n != len(want) {
		t.Fatalf("Writev() = %d, %v; want %d", n, err, len(want))
	}
	got := make([]byte, len(want))
	if _, err = ioReadFull(fds[1], got); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("Writev payload = %q, want %q", got, want)
	}
}

func TestSocketOptionsAndDup(t *testing.T) {
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)

	options := []struct {
		name string
		call func() error
	}{
		{"nodelay", func() error { return SetNoDelay(fd, true) }},
		{"nonblock", func() error { return SetNonblock(fd, true) }},
		{"recv-buffer", func() error { return SetRecvBuffer(fd, 4096) }},
		{"send-buffer", func() error { return SetSendBuffer(fd, 4096) }},
		{"reuse-addr", func() error { return SetReuseAddr(fd, 1) }},
		{"keepalive", func() error { return SetKeepAlive(fd, true) }},
		{"keepalive-period", func() error { return SetKeepAlivePeriod(fd, 1) }},
		{"linger", func() error { return SetLinger(fd, 0) }},
		{"linger-disabled", func() error { return SetLinger(fd, -1) }},
	}
	for _, option := range options {
		t.Run(option.name, func(t *testing.T) {
			if err := option.call(); err != nil {
				t.Fatal(err)
			}
		})
	}
	if err := SetNonblock(fd, false); err != nil {
		t.Fatal(err)
	}

	duplicate, err := Dup(fd)
	if err != nil {
		t.Fatal(err)
	}
	if duplicate == fd {
		t.Fatal("Dup returned the original descriptor")
	}
	if err := unix.Close(duplicate); err != nil {
		t.Fatal(err)
	}
	if _, err := Dup(-1); err == nil {
		t.Fatal("Dup(-1) succeeded")
	}

	ipv6, err := unix.Socket(unix.AF_INET6, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(ipv6)
	if err := SetIPv6Only(ipv6, 1); err != nil {
		t.Fatal(err)
	}
}

func TestDupFallback(t *testing.T) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fds[0])
	defer unix.Close(fds[1])

	old := atomic.LoadInt32(&tryDupCloexec)
	atomic.StoreInt32(&tryDupCloexec, 0)
	t.Cleanup(func() { atomic.StoreInt32(&tryDupCloexec, old) })
	duplicate, err := Dup(fds[0])
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(duplicate)
	if _, err := Dup(-1); err == nil {
		t.Fatal("fallback Dup(-1) succeeded")
	}
}

func TestDupNetConn(t *testing.T) {
	pipeA, pipeB := net.Pipe()
	defer pipeA.Close()
	defer pipeB.Close()
	if _, err := DupNetConn(pipeA); err == nil {
		t.Fatal("DupNetConn(net.Pipe()) succeeded")
	}
	if _, err := DupNetConn(syscallConnStub{err: errors.New("syscall conn error")}); err == nil {
		t.Fatal("DupNetConn did not report SyscallConn error")
	}
	if _, err := DupNetConn(syscallConnStub{raw: rawConnStub{err: errors.New("control error")}}); err == nil {
		t.Fatal("DupNetConn did not report Control error")
	}
	if _, err := DupNetConn(syscallConnStub{raw: rawConnStub{}}); err == nil {
		t.Fatal("DupNetConn did not report duplicate-handle error")
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	client, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	server, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	duplicate, err := DupNetConn(server)
	if err != nil {
		t.Fatal(err)
	}
	_ = unix.Close(duplicate)
}

type rawConnStub struct {
	err error
}

func (raw rawConnStub) Control(callback func(uintptr)) error {
	if raw.err != nil {
		return raw.err
	}
	callback(^uintptr(0))
	return nil
}
func (raw rawConnStub) Read(func(uintptr) bool) error  { return errors.New("unused") }
func (raw rawConnStub) Write(func(uintptr) bool) error { return errors.New("unused") }

type syscallConnStub struct {
	net.Conn
	raw syscall.RawConn
	err error
}

func (conn syscallConnStub) SyscallConn() (syscall.RawConn, error) { return conn.raw, conn.err }

func ioReadFull(fd int, dst []byte) (int, error) {
	read := 0
	for read < len(dst) {
		n, err := unix.Read(fd, dst[read:])
		read += n
		if err != nil {
			return read, err
		}
	}
	return read, nil
}
