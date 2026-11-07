package umux

import (
	"context"
	"testing"
	"time"
)

func TestSessionServerAndClientStatsZeroAlloc(t *testing.T) {
	var nilSess *Session
	var nilSrv *Server
	var nilCli *Client
	if nilSess.Stats() != (SessionStats{}) || nilSrv.Stats().Pollers != 0 || nilCli.Stats().Pollers != 0 {
		t.Fatal("nil receiver Stats not zero")
	}

	srv := NewServer(Config{KeepAlive: -1})
	t.Cleanup(func() { _ = srv.Close(nil) })
	ln, err := srv.Listen("tcp://127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	cli := NewClient(Config{KeepAlive: -1})
	t.Cleanup(func() { _ = cli.Close(nil) })

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cliSess, err := cli.Dial(ctx, "tcp://"+ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	srvSess := <-srv.Sessions()

	st, err := cliSess.Open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	acc, ok := <-srvSess.Accept()
	if !ok {
		t.Fatal("Accept closed early")
	}

	if s := cliSess.Stats(); s.ActiveStreams != 1 || s.Closed || s.GoAway {
		t.Fatalf("cliSess.Stats = %+v", s)
	}
	if srv.Stats().ActiveConnections < 1 || cli.Stats().ActiveConnections < 1 {
		t.Fatalf("transport Stats srv=%+v cli=%+v", srv.Stats(), cli.Stats())
	}

	if allocs := testing.AllocsPerRun(100, func() {
		_ = cliSess.Stats()
		_ = srv.Stats()
		_ = cli.Stats()
	}); allocs != 0 {
		t.Fatalf("umux Stats allocs = %v, want 0", allocs)
	}

	_ = st.Close()
	_ = acc.Stream.Close()
	_ = cliSess.Close(nil)
}
