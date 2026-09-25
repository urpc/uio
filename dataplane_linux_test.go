//go:build linux && !stdio

package uio

import (
	"bytes"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urpc/uio/internal/poller"
)

// startEchoEvents serves an echo handler and returns the listening address.
func startEchoEvents(t *testing.T, events *Events) string {
	t.Helper()
	started := make(chan string, 1)
	events.OnStart = func(events *Events) {
		for _, listener := range events.acceptor.listeners {
			started <- listener.ln.Addr().String()
			return
		}
	}
	if events.OnData == nil {
		events.OnData = func(conn Conn) error {
			_, err := conn.WriteTo(conn)
			return err
		}
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- events.Serve("tcp://127.0.0.1:0") }()
	t.Cleanup(func() {
		_ = events.Close(nil)
		select {
		case err := <-serveDone:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(5 * time.Second):
			t.Error("Serve did not stop")
		}
	})
	select {
	case addr := <-started:
		return addr
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not start")
		return ""
	}
}

// TestDataPollerEchoWithSeveralWaiters drives concurrent echo traffic through
// several goroutines sharing the stream poller, then checks that Serve joins
// all of them on shutdown.
func TestDataPollerEchoWithSeveralWaiters(t *testing.T) {
	previous := dataWaitersOverride
	dataWaitersOverride = 3
	t.Cleanup(func() { dataWaitersOverride = previous })

	events := &Events{Pollers: 4}
	addr := startEchoEvents(t, events)
	if events.data == nil {
		t.Fatal("stream connections are not using the shared data poller")
	}

	const clients, rounds = 16, 200
	var wg sync.WaitGroup
	errs := make(chan error, clients)
	for c := 0; c < clients; c++ {
		wg.Add(1)
		go func(c int) {
			defer wg.Done()
			conn, err := net.DialTimeout("tcp", addr, time.Second)
			if err != nil {
				errs <- err
				return
			}
			defer conn.Close()
			_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
			message := bytes.Repeat([]byte{byte('a' + c)}, 512)
			reply := make([]byte, len(message))
			for r := 0; r < rounds; r++ {
				if _, err := conn.Write(message); err != nil {
					errs <- err
					return
				}
				if _, err := readFull(conn, reply); err != nil {
					errs <- err
					return
				}
				if !bytes.Equal(reply, message) {
					errs <- net.UnknownNetworkError("echo mismatch")
					return
				}
			}
		}(c)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}

// TestDataPollerIgnoresStaleTags feeds the dispatcher an event whose tag
// belongs to an earlier registration of the same descriptor number. It must
// not schedule the current connection, whose open task may not have run yet.
func TestDataPollerIgnoresStaleTags(t *testing.T) {
	events := &Events{Pollers: 1}
	opened := make(chan *fdConn, 1)
	events.OnOpen = func(conn Conn) { opened <- conn.(*fdConn) }
	addr := startEchoEvents(t, events)

	client, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	var conn *fdConn
	select {
	case conn = <-opened:
	case <-time.After(5 * time.Second):
		t.Fatal("connection did not open")
	}
	deadline := time.Now().Add(5 * time.Second)
	for conn.scheduled.Load() {
		if time.Now().After(deadline) {
			t.Fatal("open task did not finish")
		}
		time.Sleep(time.Millisecond)
	}

	waiter := &dataWaiter{}
	stale := poller.Event{FD: conn.Fd(), Events: poller.ReadEvents, Tag: conn.pollTag + 1}
	events.data.dispatch(waiter, []poller.Event{stale})
	if len(waiter.ready) != 0 || conn.scheduled.Load() {
		t.Fatal("stale readiness scheduled the current connection")
	}

	current := stale
	current.Tag = conn.pollTag
	events.data.dispatch(waiter, []poller.Event{current})
	if len(waiter.ready) != 1 || waiter.ready[0] != conn {
		t.Fatal("current readiness did not schedule the connection")
	}
	events.data.submit(waiter)
}

// TestDataPollerRunsOpenBeforeRead holds each new stream after it joins the
// shared poller until the waiter has submitted the stream's task for the
// client's first bytes, before the loop schedules the open event. The task
// must still run OnOpen before OnData.
func TestDataPollerRunsOpenBeforeRead(t *testing.T) {
	var held, reads, early atomic.Int32
	registeredForTest = func(conn *fdConn) {
		// The task may start and finish between two polls of scheduled; a
		// read by this connection's task shows it ran as well.
		start := reads.Load()
		deadline := time.Now().Add(2 * time.Second)
		for !conn.scheduled.Load() && reads.Load() == start {
			if time.Now().After(deadline) {
				return
			}
			time.Sleep(50 * time.Microsecond)
		}
		held.Add(1)
	}
	t.Cleanup(func() { registeredForTest = nil })

	events := &Events{Pollers: 2}
	events.OnOpen = func(conn Conn) { conn.SetUserdata(true) }
	events.OnData = func(conn Conn) error {
		if conn.Userdata() == nil {
			early.Add(1)
		}
		reads.Add(1)
		_, err := conn.WriteTo(conn)
		return err
	}
	addr := startEchoEvents(t, events)

	const clients = 20
	for i := 0; i < clients; i++ {
		client, err := net.DialTimeout("tcp", addr, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		_ = client.SetDeadline(time.Now().Add(5 * time.Second))
		if _, err := client.Write([]byte("ping")); err != nil {
			t.Fatal(err)
		}
		if _, err := readFull(client, make([]byte, 4)); err != nil {
			t.Fatal(err)
		}
		_ = client.Close()
	}
	if held.Load() == 0 {
		t.Fatal("the data poller never submitted a stream before its open event")
	}
	if n := early.Load(); n != 0 {
		t.Fatalf("OnData ran before OnOpen on %d of %d connections", n, clients)
	}
}

func readFull(conn net.Conn, buf []byte) (int, error) {
	total := 0
	for total < len(buf) {
		n, err := conn.Read(buf[total:])
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}
