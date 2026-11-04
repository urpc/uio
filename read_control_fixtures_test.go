package uio

import (
	"errors"
	"net"
	"testing"
	"time"
)

// Fixtures shared by the read-control, backpressure and layer tests: a
// server whose connections come back on a channel, quiet waits, and a
// client writer that stops when TCP backpressure stalls it.

func readControlAwait[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
		return *new(T)
	}
}

func readControlQuiet[T any](t *testing.T, ch <-chan T, what string) {
	t.Helper()
	select {
	case value := <-ch:
		t.Fatalf("%s while reads were paused: %v", what, value)
	case <-time.After(30 * time.Millisecond):
	}
}

// readControlServe uses the same public endpoints for plain and layered
// connections, so these contracts also run against the stdio backend.
func readControlServe(t *testing.T, ev *Events, layers ...Layer) (net.Conn, Conn) {
	t.Helper()
	opened := make(chan Conn, 1)
	handler, ok := asFuncs(ev.Handler)
	if !ok || handler == nil {
		t.Fatal("readControlServe requires a FuncHandler")
	}
	onOpen := handler.Open
	handler.Open = func(c Conn) {
		if onOpen != nil {
			onOpen(c)
		}
		opened <- c
	}
	ev.Handler = handler
	started := make(chan struct{})
	ev.Hooks = FuncHandler{Start: func(*Events) { close(started) }}
	done := make(chan error, 1)
	go func() { done <- ev.Serve() }()
	t.Cleanup(func() {
		_ = ev.Close(nil)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("Serve did not return")
		}
	})
	readControlAwait(t, started, "server initialization")
	var opts []ListenOption
	if len(layers) != 0 {
		opts = append(opts, WithLayers(layers...))
	}
	listener, err := ev.Listen("tcp://127.0.0.1:0", opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	client, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client, readControlAwait(t, opened, "OnOpen")
}

// writeUntilStalled writes from the client until the kernel takes nothing for
// a while, and reports how much it took. With the server holding reads, this
// is the socket buffers' worth and the rest of total stays unsent.
func writeUntilStalled(t *testing.T, client net.Conn, total int) int {
	t.Helper()
	chunk := make([]byte, 16<<10)
	sent := 0
	for sent < total {
		_ = client.SetWriteDeadline(time.Now().Add(300 * time.Millisecond))
		n, err := client.Write(chunk[:min(len(chunk), total-sent)])
		sent += n
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				break
			}
			if isPeerGoneWrite(err) {
				break // the server closed, as a deadline test may
			}
			t.Fatal(err)
		}
	}
	_ = client.SetWriteDeadline(time.Time{})
	return sent
}
