package umux

import (
	"context"
	"testing"
	"time"
)

func TestServerServeAndListenShareLifecycle(t *testing.T) {
	srv := NewServer(Config{KeepAlive: -1})
	served := make(chan error, 1)
	go func() { served <- srv.Serve() }()
	select {
	case <-srv.router.started:
	case <-time.After(5 * time.Second):
		t.Fatal("server did not start")
	}
	ln, err := srv.Listen("tcp://127.0.0.1:0")
	if err != nil {
		_ = srv.Close(err)
		_ = srv.Wait()
		t.Fatal(err)
	}
	cli := NewClient(Config{KeepAlive: -1})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sess, err := cli.Dial(ctx, "tcp://"+ln.Addr().String())
	if err != nil {
		_ = cli.Close(err)
		_ = cli.Wait()
		_ = srv.Close(err)
		_ = srv.Wait()
		t.Fatal(err)
	}
	_ = cli.Close(nil)
	_ = cli.Wait()
	select {
	case <-sess.Done():
	case <-ctx.Done():
		t.Fatal("client shutdown did not finish its session")
	}
	_ = srv.Close(nil)
	_ = srv.Wait()
	select {
	case <-served:
	case <-ctx.Done():
		t.Fatal("Serve did not return after shutdown")
	}
	// Buffered announcements may be drained, and then every consumer ends.
	for range srv.Sessions() {
	}
}

func TestIdleServerCloseEndsSessionConsumer(t *testing.T) {
	srv := NewServer(Config{})
	_ = srv.Close(nil)
	_ = srv.Wait()
	select {
	case _, ok := <-srv.Sessions():
		if ok {
			t.Fatal("idle server announced a session after Close")
		}
	case <-time.After(time.Second):
		t.Fatal("idle server left Sessions consumers blocked")
	}
	if _, err := srv.Listen("tcp://127.0.0.1:0"); err == nil {
		t.Fatal("closed server listened again")
	}
}

func TestSessionAnnouncementQueueRefusesUnreachableTransports(t *testing.T) {
	srv := NewServer(Config{KeepAlive: -1})
	t.Cleanup(func() { _ = srv.Close(nil) })
	ln, err := srv.Listen("tcp://127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cli := NewClient(Config{KeepAlive: -1})
	t.Cleanup(func() { _ = cli.Close(nil); _ = cli.Wait(); _ = srv.Close(nil); _ = srv.Wait() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for i := 0; i < cap(srv.session); i++ {
		if _, err := cli.Dial(ctx, "tcp://"+ln.Addr().String()); err != nil {
			t.Fatalf("dial %d: %v", i, err)
		}
	}
	sess, err := cli.Dial(ctx, "tcp://"+ln.Addr().String())
	if err == nil {
		select {
		case <-sess.Done():
		case <-ctx.Done():
			t.Fatal("full announcement queue left an unreachable transport open")
		}
	}
}
