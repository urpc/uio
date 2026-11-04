package umux

import (
	"context"
	"io"
	"net"
	"testing"
	"time"

	"github.com/urpc/uio"
)

// Regression tests for the read-control review of 2026-11-04: each one
// failed on the tree before its fix.

// A pull-mode session runs no read turns: a stream's Wake or YieldRead there
// queues nothing, so closed streams are not kept queued.
func TestPullModeWakesQueueNothing(t *testing.T) {
	session, _ := startPair(t, Config{KeepAlive: -1}, Config{KeepAlive: -1})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for range 50 {
		st, err := session.Open(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := st.Wake(); err != nil {
			t.Fatal(err)
		}
		if err := st.YieldRead(); err != nil {
			t.Fatal(err)
		}
		_ = st.CloseWith(context.Canceled)
	}
	session.mu.Lock()
	queued := len(session.wakeQ)
	session.mu.Unlock()
	if queued != 0 {
		t.Fatalf("a pull-mode session queued %d wakes", queued)
	}
}

// A relay session whose Handler is a Bridge, with no Observer configured,
// still tells the bridge when a stream's queue drains or its window reopens:
// an upload larger than the stream window completes.
func TestBridgeChainUploadWithoutObserver(t *testing.T) {
	const size = 8 << 20
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				time.Sleep(50 * time.Millisecond) // let the stream window fill
				if _, err := io.CopyN(io.Discard, c, size); err == nil {
					_, _ = c.Write([]byte("OK"))
				}
				time.Sleep(time.Second)
			}(c)
		}
	}()
	agent := &uio.Bridge{Upstream: "tcp://" + ln.Addr().String()}
	targets := agent.Events()
	stopped := make(chan struct{})
	serveWaitWithStop(targets, &stopped)
	t.Cleanup(func() { _ = targets.Close(nil); <-stopped })
	srvAddr := freeAddr(t)
	srv := NewServer(Config{Handler: agent})
	go func() { _ = srv.Serve("tcp://" + srvAddr) }()
	t.Cleanup(func() { _ = srv.Close(nil) })
	relay := &uio.Bridge{}
	cli := NewClient(Config{Handler: relay})
	t.Cleanup(func() { _ = cli.Close(nil) })
	var sess *Session
	waitFor(t, "relay session", func() bool {
		s, err := cli.Dial(context.Background(), "tcp://"+srvAddr)
		sess = s
		return err == nil
	})
	relay.Dial = func(ctx context.Context, _ uio.Conn, _ uio.DialFunc) (uio.Conn, error) { return sess.Open(ctx) }
	users := relay.Events()
	userAddr := freeAddr(t)
	ustop := make(chan struct{})
	serveWaitWithStop(users, &ustop, "tcp://"+userAddr)
	t.Cleanup(func() { _ = users.Close(nil); <-ustop })
	conn, err := dialRetry(userAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	go func() { _, _ = conn.Write(make([]byte, size)) }()
	_ = conn.SetReadDeadline(time.Now().Add(15 * time.Second))
	if _, err := io.ReadFull(conn, make([]byte, 2)); err != nil {
		t.Fatalf("upload stalled: %v", err)
	}
}
