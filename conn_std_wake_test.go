//go:build windows || stdio

package uio

import (
	"context"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

// TestStdWakeThenDialDoesNotStall pins that Wake callbacks are delivered
// outside the event loop: with one loop, a callback that wakes and then dials
// synchronously must not cycle with registration. When the loop ran wake
// callbacks itself, it waited on the connection's callback mutex — held by the
// running read callback — while that callback waited for the loop to register
// the dialed connection, and neither side could advance.
func TestStdWakeThenDialDoesNotStall(t *testing.T) {
	target, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	go func() {
		peer, err := target.Accept()
		if err == nil {
			defer peer.Close()
			var probe [1]byte
			_, _ = peer.Read(probe[:])
		}
	}()

	ev := &Events{Pollers: 1}
	started := make(chan struct{})
	result := make(chan error, 1)
	var calls atomic.Int32
	ev.OnStart = func(*Events) { close(started) }
	ev.OnData = func(conn Conn) error {
		if calls.Add(1) != 1 {
			return nil
		}
		_, _ = conn.Discard(-1)
		if err := conn.Wake(); err != nil {
			result <- err
			return err
		}
		// Give the loop time to take the wake before dialing: the loop must
		// not be waiting on this callback's mutex while Dial blocks on it.
		time.Sleep(50 * time.Millisecond)
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()
		dialed, err := ev.DialContext(ctx, target.Addr().String(), nil)
		if dialed != nil {
			_ = dialed.Close()
		}
		result <- err
		return nil
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- ev.Serve() }()
	<-started
	t.Cleanup(func() {
		_ = ev.Close(nil)
		select {
		case <-serveDone:
		case <-time.After(3 * time.Second):
			t.Error("Serve did not stop")
		}
	})

	client, server := tcpConnectionPair(t)
	defer client.Close()
	if _, err := ev.Adopt(server, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("DialContext from OnData stalled behind queued Wake: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("OnData failed to return")
	}
}
