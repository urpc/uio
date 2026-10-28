package uio

import (
	"bytes"
	"net"
	"sync"
	"testing"
	"time"
)

// An OnTimer that closes keeps the same turn's OnRead from running.
func TestHandlerTimerCloseSkipsRead(t *testing.T) {
	var mu sync.Mutex
	var order []string
	note := func(s string) { mu.Lock(); order = append(order, s); mu.Unlock() }
	conn := serveAndDial(t, FuncHandler{
		Read: func(Conn) error { note("read"); return nil },
		Timer: func(c Conn) error {
			note("timer")
			_ = c.CloseWith(nil)
			return nil
		},
		Close: func(Conn, error) { note("close") },
	})
	if err := conn.SetWakeDeadline(time.Now().Add(20 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		got := append([]string(nil), order...)
		mu.Unlock()
		if len(got) > 0 && got[len(got)-1] == "close" {
			for _, what := range got {
				if what == "read" {
					t.Fatalf("OnRead ran in the turn whose OnTimer closed: %v", got)
				}
			}
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("no close within 2s: %v", order)
}

// A nanosecond interval backed up behind a stall must skip its missed
// periods arithmetically: looping them off stalls the turn.
func TestWakeIntervalCatchUpIsBounded(t *testing.T) {
	conn := serveAndDial(t, FuncHandler{
		Read:  func(Conn) error { return nil },
		Timer: func(Conn) error { return nil },
		Close: func(Conn, error) {},
	})
	if err := conn.SetWakeInterval(time.Nanosecond); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	before := time.Now()
	if err := conn.SetWakeInterval(0); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(before); elapsed > 500*time.Millisecond {
		t.Fatalf("stopping a backed-up 1ns interval took %v, want the catch-up skipped", elapsed)
	}
}

// A closed stack does not keep its delivered buffers alive through the Conn
// the app may still hold. Serve's return joins every connection task, so
// reading the stack after it orders against the close.
func TestCloseReleasesDeliveredBuffers(t *testing.T) {
	events := &Events{Pollers: 1, Handler: FuncHandler{
		Read: func(c Conn) error {
			_, _ = c.Discard(2) // leave bytes unread: they stay delivered
			return nil
		},
		Close: func(Conn, error) {},
	}}
	events.Use(LayerFunc(func(ctx LayerContext) LayerHandler {
		return PassThrough{}
	}))
	started := make(chan struct{})
	events.Hooks = FuncHandler{Start: func(*Events) { close(started) }}
	done := make(chan error, 1)
	go func() { done <- events.Serve() }()
	<-started
	listener, err := events.Listen("tcp://127.0.0.1:0", nil)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := events.Dial("tcp://"+listener.Addr().String(), nil)
	if err != nil {
		t.Fatal(err)
	}
	client, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Write(bytes.Repeat([]byte("x"), 64)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	_ = conn.CloseWith(nil)
	_ = client.Close()
	_ = listener.Close()
	_ = events.Close(nil) // joins the connection's work; orders what follows
	<-done
	node := conn.(*layerNode)
	for i, layer := range node.stack.layers {
		if layer.delivered != nil {
			t.Fatalf("layer %d keeps %d delivered bytes after close", i, len(layer.delivered))
		}
	}
}
