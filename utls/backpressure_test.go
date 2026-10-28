package utls

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/urpc/uio"
)

// A write refused for a full queue leaves the TLS connection whole: it was
// refused before crypto/tls encrypted it, so once the peer drains the queue
// the next write goes out, and the peer reads every accepted byte in order.
// Refused below, after encryption, the record's sequence number was spent
// and crypto/tls failed every write after it.
func TestTLSWritesAgainAfterTheQueueDrains(t *testing.T) {
	serverConfig, clientConfig := testConfigs(t)
	listener, err := tls.Listen("tcp", "127.0.0.1:0", serverConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	drain := make(chan struct{})
	received := make(chan []byte, 1)
	go func() {
		peer, err := listener.Accept()
		if err != nil {
			return
		}
		defer peer.Close()
		if err := peer.(*tls.Conn).Handshake(); err != nil {
			return
		}
		<-drain
		all, _ := io.ReadAll(peer)
		received <- all
	}()

	started := make(chan struct{})
	events := &uio.Events{
		Pollers:             1,
		Handler:             newRecord(),
		MaxOutboundBuffered: 64 << 10,
		Hooks:               uio.FuncHandler{Start: func(*uio.Events) { close(started) }},
	}
	done := make(chan error, 1)
	go func() { done <- events.Serve() }()
	t.Cleanup(func() { _ = events.Close(nil); <-done })
	<-started
	ctx, cancel := context.WithTimeout(context.Background(), testWait)
	defer cancel()
	conn, err := events.DialContext(ctx, "tcp://"+listener.Addr().String(), uio.WithLayers(Client(clientConfig)))
	if err != nil {
		t.Fatal(err)
	}

	var accepted []byte
	chunk := make([]byte, 16<<10)
	for i := 0; ; i++ {
		if i == 1<<12 {
			t.Fatal("the queue never filled against a peer that reads nothing")
		}
		for j := range chunk {
			chunk[j] = byte(i)
		}
		_, err := conn.Write(chunk)
		if errors.Is(err, uio.ErrOutboundOverflow) {
			break
		}
		if err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
		accepted = append(accepted, chunk...)
	}

	close(drain)
	waitDrained(t, conn)
	if _, err := conn.Write([]byte("after")); err != nil {
		t.Fatalf("write after the queue drained: %v", err)
	}
	accepted = append(accepted, "after"...)
	waitDrained(t, conn) // Close does not wait for what is queued
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	got := wait(t, received, "the peer's read")
	if len(got) != len(accepted) {
		t.Fatalf("the peer read %d bytes, want the %d accepted", len(got), len(accepted))
	}
	for i := range got {
		if got[i] != accepted[i] {
			t.Fatalf("the peer read byte %d as %#x, want %#x", i, got[i], accepted[i])
		}
	}
}

func waitDrained(t *testing.T, conn uio.Conn) {
	t.Helper()
	deadline := time.Now().Add(testWait)
	for conn.OutboundBuffered() > 0 {
		if time.Now().After(deadline) {
			t.Fatalf("the queue still holds %d bytes", conn.OutboundBuffered())
		}
		time.Sleep(time.Millisecond)
	}
}
