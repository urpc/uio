package uws

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestServerAndDialerStatsZeroAlloc(t *testing.T) {
	var nilSrv *Server
	var nilDialer *Dialer
	if nilSrv.Stats() != (Stats{}) || nilDialer.Stats() != (Stats{}) {
		t.Fatal("nil Server/Dialer Stats not zero")
	}

	var unstartedSrv Server
	var unstartedDialer Dialer
	if unstartedSrv.Stats() != (Stats{}) || unstartedDialer.Stats() != (Stats{}) {
		t.Fatal("unstarted Server/Dialer Stats not zero")
	}

	srv := NewServer(Options{
		Pollers:             1,
		MaxFramePayload:     256,
		MaxMessageSize:      1024,
		MaxOutboundBuffered: 512,
		Handler: &echoHandler{
			open:    make(chan struct{}),
			closed:  make(chan struct{}),
			message: make(chan Message, 1),
			conn:    make(chan *Conn, 1),
		},
	})
	srv.Authorize = func(r *http.Request) (any, error) {
		if r.URL.Query().Get("deny") == "1" {
			return nil, &RejectError{Status: http.StatusForbidden, Reason: "denied"}
		}
		return nil, nil
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	srvDone := make(chan error, 1)
	go func() { srvDone <- srv.Serve(addr) }()
	t.Cleanup(func() {
		_ = srv.Close(nil)
		<-srvDone
	})
	select {
	case <-srv.Ready():
	case <-time.After(3 * time.Second):
		t.Fatal("server not ready")
	}

	// 1. Rejected ServeHTTP upgrade increments HandshakeFailures.
	rec := httptest.NewRecorder()
	badReq := httptest.NewRequest(http.MethodGet, "http://"+addr+"/ws", nil)
	srv.ServeHTTP(rec, badReq)
	if srv.Stats().HandshakeFailures == 0 {
		t.Fatalf("ServeHTTP invalid upgrade did not record HandshakeFailures: %+v", srv.Stats())
	}

	// 2. Dial denied handshake via Dialer -> both Server and Dialer record HandshakeFailures.
	dialer := NewDialer(Options{Pollers: 1, MaxFramePayload: 256, MaxMessageSize: 1024, MaxOutboundBuffered: 512})
	t.Cleanup(func() { _ = dialer.Close(nil) })

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, derr := dialer.DialClient(ctx, "ws://"+addr+"/ws?deny=1"); derr == nil {
		t.Fatal("expected denied upgrade to fail")
	}
	for deadline := time.Now().Add(time.Second); (dialer.Stats().HandshakeFailures == 0 || srv.Stats().HandshakeFailures < 2) && time.Now().Before(deadline); {
		time.Sleep(time.Millisecond)
	}

	// 3. Successful upgrade + backpressure rejection + close.
	cli, err := dialer.DialClient(ctx, "ws://"+addr+"/ws")
	if err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(time.Second); srv.Stats().ActiveConnections < 1 && time.Now().Before(deadline); {
		time.Sleep(time.Millisecond)
	}
	if sSt, dSt := srv.Stats(), dialer.Stats(); sSt.TotalUpgrades != 1 || dSt.ActiveConnections != 1 || dSt.TotalUpgrades != 1 {
		t.Fatalf("upgraded stats srv=%+v dialer=%+v", sSt, dSt)
	}

	// Fragmented 768B message (> 512B MaxOutboundBuffered) triggers ErrBackpressure immediately.
	if serr := cli.Conn.SendBinary(make([]byte, 768)); !errors.Is(serr, ErrBackpressure) {
		t.Fatalf("oversized SendBinary = %v, want ErrBackpressure", serr)
	}
	if dialer.Stats().BackpressureDrops == 0 {
		t.Fatalf("BackpressureDrops = 0, want >= 1: %+v", dialer.Stats())
	}

	if allocs := testing.AllocsPerRun(100, func() {
		_ = srv.Stats()
		_ = dialer.Stats()
	}); allocs != 0 {
		t.Fatalf("uws Stats allocs = %v, want 0", allocs)
	}

	_ = cli.Close(1000, "")
	for deadline := time.Now().Add(time.Second); (srv.Stats().ActiveConnections != 0 || dialer.Stats().ActiveConnections != 0) && time.Now().Before(deadline); {
		time.Sleep(time.Millisecond)
	}
}
