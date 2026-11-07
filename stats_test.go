package uio

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestEventsStatsLifecycleOverflowAndZeroAlloc(t *testing.T) {
	var nilEv *Events
	if st := nilEv.Stats(); st != (Stats{}) {
		t.Fatalf("nil Events.Stats = %+v, want zero", st)
	}

	var idleEv Events
	if st := idleEv.Stats(); st != (Stats{}) {
		t.Fatalf("unstarted Events.Stats = %+v, want zero", st)
	}

	openedCh := make(chan Conn, 2)
	closedCh := make(chan struct{}, 2)
	var listenAddr string
	ready := make(chan struct{})
	ev := &Events{
		Pollers:             2,
		MaxOutboundBuffered: 16,
		Handler: FuncHandler{
			Open:  func(c Conn) { openedCh <- c },
			Close: func(Conn, error) { closedCh <- struct{}{} },
		},
		Hooks: FuncHandler{
			Start: func(e *Events) {
				ln, err := e.Listen("tcp://127.0.0.1:0")
				if err != nil {
					t.Error(err)
					return
				}
				listenAddr = "tcp://" + ln.Addr().String()
				close(ready)
			},
		},
	}
	done := make(chan error, 1)
	go func() { done <- ev.Serve() }()
	t.Cleanup(func() {
		_ = ev.Close(nil)
		<-done
	})
	<-ready

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cli, err := ev.DialContext(ctx, listenAddr)
	if err != nil {
		t.Fatal(err)
	}
	srv := <-openedCh
	<-openedCh // client OnOpen

	// Wait until both client and server connections are reflected in ActiveConnections.
	for deadline := time.Now().Add(time.Second); ev.Stats().ActiveConnections < 2 && time.Now().Before(deadline); {
		time.Sleep(time.Millisecond)
	}
	st := ev.Stats()
	if st.Pollers != 2 || st.ActiveConnections != 2 || st.TotalConnections != 2 {
		t.Fatalf("Stats with 2 open conns = %+v", st)
	}

	// Trigger ErrOutboundOverflow (> 16-byte MaxOutboundBuffered) and verify counter increments.
	if _, werr := cli.Write(make([]byte, 64)); !errors.Is(werr, ErrOutboundOverflow) {
		t.Fatalf("oversized Write = %v, want ErrOutboundOverflow", werr)
	}
	if got := ev.Stats().OutboundOverflows; got == 0 {
		t.Fatalf("OutboundOverflows = %d, want >= 1", got)
	}

	if allocs := testing.AllocsPerRun(100, func() { _ = ev.Stats() }); allocs != 0 {
		t.Fatalf("Events.Stats allocs = %v, want 0", allocs)
	}

	_ = cli.Close()
	_ = srv.Close()
	<-closedCh
	<-closedCh
	for deadline := time.Now().Add(time.Second); ev.Stats().ActiveConnections != 0 && time.Now().Before(deadline); {
		time.Sleep(time.Millisecond)
	}
	if end := ev.Stats(); end.ActiveConnections != 0 || end.TotalConnections != 2 {
		t.Fatalf("Stats after close = %+v", end)
	}
}
