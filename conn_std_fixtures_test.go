//go:build windows || stdio

package uio

import (
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

type stdRegistrationConn struct {
	fd             uintptr
	closed         chan struct{}
	readData       chan []byte
	closeOnce      sync.Once
	openReturned   atomic.Bool
	orderViolation atomic.Bool
	readActive     atomic.Int32
	maxReadActive  atomic.Int32
	readCalls      atomic.Int32
	secondRead     chan struct{}
	secondReadOnce sync.Once
	writeCalls     atomic.Int32
	writeStarted   chan struct{}
	writeRelease   chan struct{}
	writeOnce      sync.Once
	writeFunc      func([]byte) (int, error)
	readFunc       func([]byte) (int, error)
}

func newStdRegistrationConn(fd uintptr) *stdRegistrationConn {
	return newStdRawRegistrationConn(syntheticFD(fd))
}

// newStdRawRegistrationConn keeps the descriptor as given: tests of the fd
// table's own bounds need descriptors it cannot hold.
func newStdRawRegistrationConn(fd uintptr) *stdRegistrationConn {
	return &stdRegistrationConn{
		fd: fd, closed: make(chan struct{}), readData: make(chan []byte),
	}
}

func (conn *stdRegistrationConn) Read(buffer []byte) (int, error) {
	if conn.readCalls.Add(1) == 2 && conn.secondRead != nil {
		conn.secondReadOnce.Do(func() { close(conn.secondRead) })
	}
	active := conn.readActive.Add(1)
	defer conn.readActive.Add(-1)
	for maximum := conn.maxReadActive.Load(); active > maximum; maximum = conn.maxReadActive.Load() {
		if conn.maxReadActive.CompareAndSwap(maximum, active) {
			break
		}
	}
	if !conn.openReturned.Load() {
		conn.orderViolation.Store(true)
	}
	if conn.readFunc != nil {
		return conn.readFunc(buffer)
	}
	select {
	case data := <-conn.readData:
		return copy(buffer, data), nil
	case <-conn.closed:
		return 0, net.ErrClosed
	}
}

func (conn *stdRegistrationConn) Write(buffer []byte) (int, error) {
	conn.writeCalls.Add(1)
	if !conn.openReturned.Load() {
		conn.orderViolation.Store(true)
	}
	if conn.writeStarted != nil {
		conn.writeOnce.Do(func() { close(conn.writeStarted) })
		<-conn.closed
		if conn.writeRelease != nil {
			<-conn.writeRelease
		}
		return 0, net.ErrClosed
	}
	if conn.writeFunc != nil {
		return conn.writeFunc(buffer)
	}
	return len(buffer), nil
}

func (conn *stdRegistrationConn) Close() error {
	conn.closeOnce.Do(func() { close(conn.closed) })
	return nil
}

func (*stdRegistrationConn) LocalAddr() net.Addr              { return &net.TCPAddr{} }
func (*stdRegistrationConn) RemoteAddr() net.Addr             { return &net.TCPAddr{} }
func (*stdRegistrationConn) SetDeadline(time.Time) error      { return nil }
func (*stdRegistrationConn) SetReadDeadline(time.Time) error  { return nil }
func (*stdRegistrationConn) SetWriteDeadline(time.Time) error { return nil }
func (conn *stdRegistrationConn) SyscallConn() (syscall.RawConn, error) {
	return stdRawConn(conn.fd), nil
}

type stdRawConn uintptr

func (fd stdRawConn) Control(callback func(uintptr)) error {
	callback(uintptr(fd))
	return nil
}

func (fd stdRawConn) Read(callback func(uintptr) bool) error {
	callback(uintptr(fd))
	return nil
}

func (fd stdRawConn) Write(callback func(uintptr) bool) error {
	callback(uintptr(fd))
	return nil
}

func startStdTestEvents(t *testing.T, events *Events) <-chan error {
	t.Helper()
	started := make(chan struct{})
	funcs(events).Start = func(*Events) { close(started) }
	serveDone := make(chan error, 1)
	go func() { serveDone <- events.Serve() }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("stdio events did not start")
	}
	return serveDone
}

func newStdFDConn(events *Events, raw *stdRegistrationConn) *fdConn {
	conn := &fdConn{
		conn: raw, writeSig: make(chan struct{}, 1),
	}
	conn.entry = events.plainEntry()
	conn.loop = events.workers[0]
	return conn
}

func stopStdTestEvents(t *testing.T, events *Events, serveDone <-chan error, closeErr error) {
	t.Helper()
	closeDone := make(chan error, 1)
	go func() { closeDone <- events.Close(closeErr) }()
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	for closeDone != nil || serveDone != nil {
		select {
		case err := <-closeDone:
			if err != nil {
				t.Fatalf("Events.Close error = %v", err)
			}
			closeDone = nil
		case err := <-serveDone:
			if !errors.Is(err, closeErr) {
				t.Fatalf("Events.Serve error = %v", err)
			}
			serveDone = nil
		case <-timer.C:
			t.Fatal("stdio events did not stop within one second")
		}
	}
}
