package uio

import (
	"context"
	"errors"
	"net"
	"testing"

	"github.com/urpc/uio/internal/readcontrol"
)

func TestPublishedDialResultWinsConcurrentCancellation(t *testing.T) {
	for _, failure := range []error{nil, errors.New("handshake failed")} {
		for range 64 {
			ctx, cancel := context.WithCancel(context.Background())
			probe := &dialProbe{done: make(chan struct{})}
			transport := &contractTransport{}
			if failure == nil {
				probe.settle(transport)
			} else {
				probe.fail(failure)
			}
			cancel() // both select arms are ready; the published result owns the dial
			conn, err := (&Events{}).awaitDial(ctx, transport, probe)
			if !errors.Is(err, failure) || (failure == nil && conn != transport) {
				t.Fatalf("published dial = %v, %v, want %v", conn, err, failure)
			}
			if transport.state&StateClosed != 0 {
				t.Fatal("cancellation aborted an already published dial result")
			}
		}
	}
}

func TestCanceledDialAbortsTransportAndCannotPublishALateOpen(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	cause := errors.New("dial owner stopped waiting")
	cancel(cause)
	transport := &contractTransport{}
	probe := &dialProbe{done: make(chan struct{})}
	conn, err := (&Events{}).awaitDial(ctx, transport, probe)
	if conn != nil || !errors.Is(err, cause) || !errors.Is(transport.closeCause, cause) {
		t.Fatalf("canceled dial = %v, %v, transport cause %v", conn, err, transport.closeCause)
	}
	if probe.settle(transport) {
		t.Fatal("late handshake opened a dial already canceled by its owner")
	}
}

func TestLayerDefaultsAcceptTypedNilHandlerAndDiscardInput(t *testing.T) {
	var handler *FuncHandler
	events := &Events{Handler: handler}
	entry, err := events.newEntry([]Layer{PassThroughLayer})
	if err != nil {
		t.Fatal(err)
	}
	transport := &contractTransport{input: []byte("unhandled input")}
	entry.cb.onOpen(transport)
	if err := entry.cb.onRead(transport); err != nil {
		t.Fatal(err)
	}
	c := transport.userdata.(*stack)
	if !c.topOpen.Load() || len(transport.input) != 0 || c.topConn().InboundBuffered() != 0 {
		t.Fatal("typed nil Handler did not use the default consuming handler")
	}
	if sentNoticeOf(handler) != nil {
		t.Fatal("typed nil Handler registered a receipt callback")
	}
}

func TestDefaultReadHandlerConsumesEachBorrowedSocketChunk(t *testing.T) {
	observed := make(chan int, 2)
	events := &Events{
		Pollers: 1, MaxBufferSize: 1,
		Handler:  FuncHandler{},
		Observer: FuncHandler{BytesRead: func(c Conn, _ int) { observed <- c.InboundBuffered() }},
	}
	peer, _ := readControlServe(t, events)
	for _, payload := range []string{"a", "b"} {
		if _, err := peer.Write([]byte(payload)); err != nil {
			t.Fatal(err)
		}
		if buffered := readControlAwait(t, observed, "default handler input"); buffered != 1 {
			t.Fatalf("default handler retained an earlier chunk: %d bytes", buffered)
		}
	}
}

func TestPacketInputRejectsConsumerPauseWithoutInstallingAnOwner(t *testing.T) {
	started := make(chan struct{})
	events := &Events{Pollers: 1, Hooks: FuncHandler{Start: func(*Events) { close(started) }}}
	done := make(chan error, 1)
	go func() { done <- events.Serve() }()
	t.Cleanup(func() { _ = events.Close(nil); readControlAwait(t, done, "packet Events stopped") })
	readControlAwait(t, started, "packet Events started")
	conn, err := events.Dial("udp://127.0.0.1:9")
	if err != nil {
		t.Fatal(err)
	}
	if !errors.Is(conn.PauseReads(), ErrUnsupported) || !errors.Is(conn.ResumeReads(), ErrUnsupported) {
		t.Fatal("raw packet transport acquired stream-style pause semantics")
	}
	if conn.ReadsPaused() || readcontrol.ControlOf(conn) != nil || readcontrol.NewPause(conn) != nil {
		t.Fatal("unsupported packet input retained a consumer owner")
	}
}

func TestClosedInputRejectsANewApplicationPause(t *testing.T) {
	closed := make(chan struct{}, 1)
	events := &Events{Pollers: 1, Handler: FuncHandler{Close: func(Conn, error) { closed <- struct{}{} }}}
	_, conn := readControlServe(t, events)
	if err := conn.CloseWith(nil); err != nil {
		t.Fatal(err)
	}
	readControlAwait(t, closed, "input transport closed")
	if err := conn.PauseReads(); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("PauseReads after close = %v", err)
	}
	if conn.(*fdConn).currentReadControl() != nil {
		t.Fatal("rejected pause installed new control state on a closed transport")
	}
}
