package uio

import (
	"net"
	"sync/atomic"
	"testing"
	"time"
)

// Two uio endpoints that both write faster than they read must still deliver
// everything both ways. Each end's read rounds pause at their budget while its
// own output fills the socket; a paused read that waited for writability would
// wait for a peer waiting on it in turn, with both directions' buffers full.
func TestBidirectionalBulkTransferBetweenPeers(t *testing.T) {
	const (
		total = 64 << 20
		chunk = 64 << 10
	)
	var serverReceived, clientReceived atomic.Int64
	writeErrs := make(chan error, 2)
	payload := make([]byte, chunk)
	newEvents := func(received *atomic.Int64, started chan<- struct{}) *Events {
		return &Events{
			Pollers:       1,
			MaxBufferSize: chunk,
			OnStart:       func(*Events) { close(started) },
			OnOpen: func(conn Conn) {
				go func() {
					for sent := 0; sent < total; sent += chunk {
						if _, err := conn.Write(payload); err != nil {
							writeErrs <- err
							return
						}
					}
				}()
			},
			OnData: func(conn Conn) error {
				n, _ := conn.Discard(conn.InboundBuffered())
				received.Add(int64(n))
				return nil
			},
		}
	}
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := "tcp://" + probe.Addr().String()
	_ = probe.Close()

	serverStarted, clientStarted := make(chan struct{}), make(chan struct{})
	server := newEvents(&serverReceived, serverStarted)
	client := newEvents(&clientReceived, clientStarted)
	serverDone, clientDone := make(chan error, 1), make(chan error, 1)
	go func() { serverDone <- server.Serve(addr) }()
	go func() { clientDone <- client.Serve() }()
	t.Cleanup(func() {
		_ = client.Close(nil)
		_ = server.Close(nil)
		<-clientDone
		<-serverDone
	})
	for _, started := range []chan struct{}{serverStarted, clientStarted} {
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatal("event loops did not start")
		}
	}
	if _, err = client.Dial(addr, nil); err != nil {
		t.Fatal(err)
	}

	// A slow machine may move bytes slowly; only a transfer that stops moving
	// altogether fails.
	last, lastMoved := int64(-1), time.Now()
	for {
		select {
		case err = <-writeErrs:
			t.Fatalf("write failed: %v", err)
		default:
		}
		s, c := serverReceived.Load(), clientReceived.Load()
		if s == total && c == total {
			return
		}
		if s+c != last {
			last, lastMoved = s+c, time.Now()
		} else if time.Since(lastMoved) > 10*time.Second {
			t.Fatalf("transfer stopped: server received %d and client %d of %d bytes each", s, c, total)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
