package ukcp

import (
	"context"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/urpc/uio"
	"github.com/urpc/uio/ubridge"
)

// A KCP session's held output drains as its peer acknowledges it, with no
// socket write to report that. A Bridge forwarding an upload into it, with
// no keepalive traffic to stand in for that report, must still hear the
// drain and go on.
func TestBridgeUploadIntoKCPHearsAcknowledgedDrains(t *testing.T) {
	const size = 8 << 20
	var mu sync.Mutex
	got := 0
	sink := uio.FuncHandler{Read: func(c uio.Conn) error {
		n, err := c.Discard(-1)
		if err != nil {
			return err
		}
		mu.Lock()
		got += n
		done := got == size
		mu.Unlock()
		if done && n > 0 {
			_, err = c.Write([]byte("OK"))
		}
		return err
	}}
	opts := Options{KeepAlive: -1}
	server := newServer(t, sink, ServerLayer(opts))
	ln, err := server.Listen("udp://127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	up := ln.Addr().String()
	br := &ubridge.Bridge{Dial: func(ctx context.Context, _ uio.Conn, dial ubridge.DialFunc) (uio.Conn, error) {
		return dial(ctx, "udp://"+up, uio.WithLayers(ClientLayer(opts)))
	}}
	ev := br.Events()
	started := make(chan struct{})
	br.Hooks = uio.FuncHandler{Start: func(*uio.Events) { close(started) }}
	done := make(chan struct{})
	go func() { defer close(done); _ = ev.Serve() }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("bridge did not start")
	}
	t.Cleanup(func() { _ = ev.Close(nil); <-done })
	listener, err := ev.Listen("tcp://127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	conn, err := net.DialTimeout("tcp", listener.Addr().String(), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	go func() { _, _ = conn.Write(make([]byte, size)) }()
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.ReadFull(conn, make([]byte, 2)); err != nil {
		mu.Lock()
		defer mu.Unlock()
		t.Fatalf("upload stalled at %d of %d bytes: %v", got, size, err)
	}
}
