//go:build (linux || darwin || netbsd || freebsd || openbsd || dragonfly) && !stdio

package uio

import (
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/urpc/uio/internal/poller"
	"github.com/urpc/uio/internal/taskqueue"
	"golang.org/x/sys/unix"
)

// TestStopRaceKeepsClaimReserved pins the ordering inside noteIO: a turn's
// lifetime reservation is held before its scheduling claim becomes visible, so
// the stop barrier always joins every published claim. With the claim
// published first, a stop landing between the two would let shutdown hand the
// close to a claim that can never become a task, and Serve could return with
// the connection still open.
func TestStopRaceKeepsClaimReserved(t *testing.T) {
	ep, err := poller.NewNetPoller()
	if err != nil {
		t.Fatal(err)
	}
	defer ep.Close(nil)
	sockets, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(sockets[1])

	onClose := make(chan struct{})
	events := &Events{}
	events.OnClose = func(Conn, error) { close(onClose) }
	// The final close callback is delivered as a pooled turn, so the loop
	// needs a real pool and task queue.
	pool := newIOTaskPool(nil)
	defer pool.stop()
	loop := &eventLoop{
		events: events,
		poller: ep,
		fdMap:  newFdMap(),
		tasks:  taskqueue.New[*task](),
		ioPool: pool,
		ioIdle: make(chan struct{}),
	}
	conn := &fdConn{fd: sockets[0], commonConn: commonConn{events: events, loop: loop}}
	if err := loop.fdMap.Put(conn.fd, conn); err != nil {
		t.Fatal(err)
	}

	paused, resume := make(chan struct{}), make(chan struct{})
	testHookTaskClaimed = func(c *fdConn) {
		if c != conn {
			return
		}
		close(paused)
		<-resume
	}
	defer func() { testHookTaskClaimed = nil }()

	claimed := make(chan bool, 1)
	go func() { claimed <- conn.noteIO(ioEventRead) }()
	select {
	case <-paused:
	case <-time.After(time.Second):
		t.Fatal("admission probe did not pause")
	}
	// The claim is visible now; the stop barrier must be able to see this turn.
	if loop.ioState.Load()&^ioStopBit == 0 {
		close(resume)
		t.Fatal("scheduling claim was published without a turn reservation")
	}

	shutdownDone := make(chan struct{})
	go func() {
		loop.shutdown(io.EOF)
		close(shutdownDone)
	}()
	select {
	case <-shutdownDone:
		close(resume)
		t.Fatal("shutdown completed while a claimed turn was reserved")
	case <-time.After(50 * time.Millisecond):
	}
	close(resume)
	if !<-claimed {
		t.Fatal("noteIO did not claim the reserved turn")
	}
	// The pooled turn can never run after the stop; the refusal path ends its
	// turn the way a rejected submission does.
	conn.handleIOSubmitFailure(net.ErrClosed)
	select {
	case <-shutdownDone:
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown did not finish after the refused turn")
	}
	if !conn.close.isReleased() {
		t.Fatal("shutdown returned without releasing the connection")
	}
	if _, err := unix.FcntlInt(uintptr(conn.fd), unix.F_GETFD, 0); err != unix.EBADF {
		t.Fatalf("connection fd still open after shutdown: err = %v", err)
	}
	select {
	case <-onClose:
	case <-time.After(5 * time.Second):
		t.Fatal("OnClose was never delivered")
	}
}

// TestShutdownJoinsPendingWake drives the lifecycle end to end: an external
// Wake parked in noteIO's claim window while Events.Close runs. Serve must not
// return before the stop barrier has joined that turn: either the reservation
// holds the barrier until the producer resumes, or the barrier's close pass
// already completed the teardown itself.
func TestShutdownJoinsPendingWake(t *testing.T) {
	ev := &Events{Pollers: 1}
	started := make(chan struct{})
	opened := make(chan *fdConn, 1)
	closed := make(chan struct{})
	ev.OnStart = func(*Events) { close(started) }
	ev.OnOpen = func(conn Conn) { opened <- conn.(*fdConn) }
	ev.OnClose = func(Conn, error) { close(closed) }
	serveDone := make(chan error, 1)
	go func() { serveDone <- ev.Serve() }()
	<-started

	client, server := tcpConnectionPair(t)
	defer client.Close()
	if _, err := ev.Adopt(server, nil); err != nil {
		t.Fatal(err)
	}
	conn := <-opened
	for deadline := time.Now().Add(time.Second); conn.taskState.Load()&taskScheduledBit != 0; {
		if time.Now().After(deadline) {
			t.Fatal("OnOpen did not finish")
		}
		time.Sleep(time.Millisecond)
	}

	paused, resume := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(resume) }) }
	wakeDone := make(chan error, 1)
	testHookTaskClaimed = func(c *fdConn) {
		if c == conn {
			close(paused)
			<-resume
		}
	}
	defer func() {
		release()
		<-wakeDone
		testHookTaskClaimed = nil
	}()
	go func() { wakeDone <- conn.Wake() }()
	select {
	case <-paused:
	case <-time.After(time.Second):
		t.Fatal("wake admission did not pause")
	}
	if conn.loop.ioState.Load()&^ioStopBit == 0 {
		t.Fatal("claim visible without its turn reservation behind it")
	}

	ev.Close(nil)
	select {
	case err := <-serveDone:
		if err != nil && err != net.ErrClosed {
			t.Error(err)
		}
		select {
		case <-closed:
		default:
			t.Error("Serve returned before OnClose: pending Wake claim was not joined")
		}
		if !conn.close.isReleased() {
			t.Error("Serve returned with the connection fd still owned")
		}
		if _, err := unix.FcntlInt(uintptr(conn.Fd()), unix.F_GETFD, 0); err != unix.EBADF {
			t.Errorf("Serve returned with socket fd still open: F_GETFD error = %v", err)
		}
	case <-time.After(100 * time.Millisecond):
		// The barrier is waiting on the parked turn; releasing it must finish
		// the lifecycle.
		release()
		select {
		case <-serveDone:
		case <-time.After(3 * time.Second):
			t.Fatal("Serve did not finish after Wake admission resumed")
		}
		select {
		case <-closed:
		case <-time.After(3 * time.Second):
			t.Fatal("OnClose was never delivered")
		}
	}
}
