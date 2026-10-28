package uio

import (
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

// A FuncHandler contributes its fields themselves, so an unset one stays nil
// and the hot paths skip it; an unset Observer or Hooks contributes nothing.
func TestResolveCallbacksKeepsUnsetCallbacksNil(t *testing.T) {
	fh := FuncHandler{Read: func(Conn) error { return nil }, BytesRead: func(Conn, int) {}}
	cb := resolveCallbacks(fh, fh, nil)
	if cb.onRead == nil {
		t.Fatal("a set Inbound was not resolved")
	}
	if cb.onBytesRead == nil {
		t.Fatal("a set BytesRead was not resolved")
	}
	if cb.onOpen != nil || cb.onClose != nil || cb.onBytesWritten != nil {
		t.Fatal("unset FuncHandler fields resolved to callbacks")
	}
	if cb.onStart != nil || cb.onStop != nil {
		t.Fatal("hooks resolved without Hooks set")
	}
	// A pointer to a FuncHandler resolves like the value.
	if cb := resolveCallbacks(&fh, &fh, nil); cb.onRead == nil || cb.onOpen != nil || cb.onBytesRead == nil {
		t.Fatal("a pointer to a FuncHandler resolved differently from the value")
	}
	var nilFuncs *FuncHandler
	for _, cb := range []callbacks{resolveCallbacks(nil, nil, nil), resolveCallbacks(nilFuncs, nilFuncs, nilFuncs)} {
		if cb.onOpen != nil || cb.onRead != nil || cb.onClose != nil ||
			cb.onBytesRead != nil || cb.onBytesWritten != nil || cb.onStart != nil || cb.onStop != nil {
			t.Fatal("no configuration resolved to callbacks")
		}
	}
}

type echoHandler struct{ opened, closed atomic.Int32 }

func (h *echoHandler) OnOpen(Conn) { h.opened.Add(1) }
func (h *echoHandler) OnRead(c Conn) error {
	_, err := c.WriteTo(c)
	return err
}
func (h *echoHandler) OnTimer(Conn) error  { return nil }
func (h *echoHandler) OnClose(Conn, error) { h.closed.Add(1) }

type byteCounter struct{ in, out atomic.Int64 }

func (b *byteCounter) OnBytesRead(_ Conn, n int)    { b.in.Add(int64(n)) }
func (b *byteCounter) OnBytesWritten(_ Conn, n int) { b.out.Add(int64(n)) }

type startStop struct {
	addr    chan string
	stopped atomic.Bool
}

func (s *startStop) OnStart(ev *Events) {
	for _, l := range ev.acceptor.listeners {
		s.addr <- l.ln.Addr().String()
		return
	}
}
func (s *startStop) OnStop(*Events) { s.stopped.Store(true) }

// Handler, Observer and Hooks are separate values, each reaching only its own
// callbacks.
func TestSeparateHandlerObserverAndHooks(t *testing.T) {
	handler, counter, hooks := &echoHandler{}, &byteCounter{}, &startStop{addr: make(chan string, 1)}
	events := &Events{Pollers: 1, Handler: handler, Observer: counter, Hooks: hooks}
	done := make(chan error, 1)
	go func() { done <- events.Serve("tcp://127.0.0.1:0") }()
	var addr string
	select {
	case addr = <-hooks.addr:
	case err := <-done:
		t.Fatalf("Serve: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("OnStart did not run")
	}
	client, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("separate callbacks")
	if _, err := client.Write(payload); err != nil {
		t.Fatal(err)
	}
	_ = client.SetReadDeadline(time.Now().Add(5 * time.Second))
	got := make([]byte, len(payload))
	if _, err := io.ReadFull(client, got); err != nil {
		t.Fatal(err)
	}
	_ = client.Close()
	for deadline := time.Now().Add(5 * time.Second); handler.closed.Load() == 0; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("OnClose did not run")
		}
	}
	_ = events.Close(nil)
	<-done
	if handler.opened.Load() != 1 {
		t.Fatalf("OnOpen ran %d times", handler.opened.Load())
	}
	if counter.in.Load() != int64(len(payload)) || counter.out.Load() != int64(len(payload)) {
		t.Fatalf("observed %d bytes in and %d out, want %d each", counter.in.Load(), counter.out.Load(), len(payload))
	}
	if !hooks.stopped.Load() {
		t.Fatal("OnStop did not run after Serve")
	}
}
