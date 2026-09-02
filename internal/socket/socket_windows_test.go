//go:build windows

package socket

import (
	"errors"
	"net"
	"syscall"
	"testing"

	"golang.org/x/sys/windows"
)

func TestWindowsDupNetConn(t *testing.T) {
	pipeA, pipeB := net.Pipe()
	defer pipeA.Close()
	defer pipeB.Close()
	if _, err := DupNetConn(pipeA); err == nil {
		t.Fatal("DupNetConn(net.Pipe()) succeeded")
	}
	if _, err := DupNetConn(windowsSyscallConn{err: errors.New("syscall conn error")}); err == nil {
		t.Fatal("DupNetConn did not report SyscallConn error")
	}
	if _, err := DupNetConn(windowsSyscallConn{raw: windowsRawConn{err: errors.New("control error")}}); err == nil {
		t.Fatal("DupNetConn did not report Control error")
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
	if err := windows.Close(windows.Handle(duplicate)); err != nil {
		t.Fatal(err)
	}
}

type windowsRawConn struct{ err error }

func (raw windowsRawConn) Control(func(uintptr)) error { return raw.err }
func (raw windowsRawConn) Read(func(uintptr) bool) error {
	return errors.New("unused")
}
func (raw windowsRawConn) Write(func(uintptr) bool) error {
	return errors.New("unused")
}

type windowsSyscallConn struct {
	net.Conn
	raw syscall.RawConn
	err error
}

func (conn windowsSyscallConn) SyscallConn() (syscall.RawConn, error) {
	return conn.raw, conn.err
}
