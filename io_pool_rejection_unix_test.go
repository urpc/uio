//go:build (linux || darwin || netbsd || freebsd || openbsd || dragonfly) && !stdio

package uio

import (
	"errors"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

func TestRejectedIOBatchHasBoundedCallbacksAndCompletesOnStop(t *testing.T) {
	const connections = 512
	var active, closed atomic.Int32
	var exceeded atomic.Bool
	entered := make(chan struct{}, connections)
	release := make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	events := &Events{OnClose: func(Conn, error) {
		if active.Add(1) > ioRejectWorkers {
			exceeded.Store(true)
		}
		entered <- struct{}{}
		<-release
		active.Add(-1)
		closed.Add(1)
	}}
	pool := newIOTaskPool(rejectingNativeExecutor{})
	tasks := make([]IOTask, connections)
	for index := range tasks {
		conn := &fdConn{commonConn: commonConn{events: events}}
		conn.close.phase.Store(closeResourcesReleased)
		conn.pendingEvents.Store(ioEventClose)
		conn.scheduled.Store(true)
		tasks[index] = conn
	}

	submitted := make(chan bool, 1)
	go func() { submitted <- pool.submitBatch(tasks) }()
	select {
	case ok := <-submitted:
		if !ok {
			t.Fatal("I/O pool rejected its own batch")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("rejected batch blocked the submitting goroutine")
	}
	for range ioRejectWorkers {
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("failure workers did not begin OnClose")
		}
	}
	if exceeded.Load() {
		t.Fatalf("more than %d OnClose callbacks ran concurrently", ioRejectWorkers)
	}

	stopped := make(chan struct{})
	go func() {
		pool.stop()
		close(stopped)
	}()
	select {
	case <-stopped:
		t.Fatal("pool stopped before blocked OnClose callbacks completed")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("pool did not wait for all rejected callbacks")
	}
	if exceeded.Load() {
		t.Fatalf("more than %d OnClose callbacks ran concurrently", ioRejectWorkers)
	}
	if got := closed.Load(); got != connections {
		t.Fatalf("OnClose calls = %d, want %d", got, connections)
	}
	for _, task := range tasks {
		if got := task.(*fdConn).close.phase.Load(); got != closeCallbackDelivered {
			t.Fatalf("close phase = %d, want %d", got, closeCallbackDelivered)
		}
	}
}

// closeOnLoop defers closure to a reserved turn, and that reservation can be a
// task the executor rejects. The rejection then ends the turn and must hand the
// closure back to the loop, or the connection never closes.
func TestRejectedTaskHandsBackDeferredClose(t *testing.T) {
	closed := make(chan error, 1)
	events := &Events{Pollers: 1, OnClose: func(_ Conn, err error) { closed <- err }}
	testConn := newTestConnection(t, events)
	conn := testConn.conn
	for deadline := time.Now().Add(2 * time.Second); conn.scheduled.Load() || conn.ioOwner.Load() != 0; {
		if time.Now().After(deadline) {
			t.Fatal("connection did not finish opening")
		}
		time.Sleep(time.Millisecond)
	}
	// A task the executor is about to reject holds the scheduling claim.
	if !conn.noteIO(ioEventRead) {
		t.Fatal("could not reserve a turn")
	}
	conn.requestClose(io.EOF)
	for deadline := time.Now().Add(2 * time.Second); ; {
		conn.submitMu.Lock()
		deferred := conn.close.deferred != nil
		conn.submitMu.Unlock()
		if deferred {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("closeOnLoop did not defer closure to the reserved turn")
		}
		time.Sleep(time.Millisecond)
	}
	// The rejection is handled after closeOnLoop deferred to it.
	conn.handleIOSubmitFailure(net.ErrClosed)
	select {
	case err := <-closed:
		if !errors.Is(err, io.EOF) {
			t.Fatalf("OnClose(%v), want the deferred cause", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the connection never closed: the rejected turn kept its deferred closure")
	}
}
