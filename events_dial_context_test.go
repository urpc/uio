package uio

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestDialContextCancellationBeforeNetworkDial(t *testing.T) {
	started := make(chan struct{})
	events := &Events{Pollers: 1, OnStart: func(*Events) { close(started) }}
	serveDone := make(chan error, 1)
	go func() { serveDone <- events.Serve() }()
	defer func() {
		_ = events.Close(nil)
		select {
		case <-serveDone:
		case <-time.After(time.Second):
			t.Error("Events.Serve did not stop")
		}
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("Events.Serve did not start")
	}

	cause := errors.New("dial canceled")
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(cause)
	conn, err := events.DialContext(ctx, "tcp://127.0.0.1:1", nil)
	if conn != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("DialContext() = %v, %v; want nil, cancellation", conn, err)
	}
}
