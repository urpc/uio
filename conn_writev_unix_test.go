//go:build (linux || darwin || netbsd || freebsd || openbsd || dragonfly) && !stdio

package uio

import (
	"bytes"
	"io"
	"net"
	"testing"
	"time"
)

// Writev takes vectors longer than one writev or sendmsg allows, which is
// 1024 buffers on Linux and the BSDs, whether it sends them at once, as from
// OnOpen, or queues them behind a read round's other replies.
func TestWritevTakesVectorsLongerThanOneSyscall(t *testing.T) {
	for _, callback := range []string{"OnOpen", "OnData"} {
		t.Run(callback, func(t *testing.T) {
			vec := make([][]byte, 2000)
			var want []byte
			for index := range vec {
				vec[index] = []byte{byte('a' + index%26)}
				want = append(want, vec[index]...)
			}
			events := &Events{Pollers: 1}
			started := make(chan string, 1)
			result := make(chan error, 1)
			writev := func(conn Conn) {
				n, err := conn.Writev(vec)
				if err == nil && n != len(want) {
					err = io.ErrShortWrite
				}
				result <- err
			}
			events.OnStart = func(ev *Events) {
				for _, listener := range ev.acceptor.listeners {
					started <- listener.pair.local.String()
					return
				}
			}
			events.OnOpen = func(conn Conn) {
				if callback == "OnOpen" {
					writev(conn)
				}
			}
			events.OnData = func(conn Conn) error {
				_, _ = conn.Discard(conn.InboundBuffered())
				if callback == "OnData" {
					writev(conn)
				}
				return nil
			}
			serveDone := make(chan error, 1)
			go func() { serveDone <- events.Serve("tcp://127.0.0.1:0") }()
			t.Cleanup(func() {
				_ = events.Close(nil)
				<-serveDone
			})
			client, err := net.Dial("tcp", <-started)
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			if _, err = client.Write([]byte("x")); err != nil {
				t.Fatal(err)
			}
			if err = <-result; err != nil {
				t.Fatalf("Writev of %d buffers in %s: %v", len(vec), callback, err)
			}
			if err = client.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
				t.Fatal(err)
			}
			got := make([]byte, len(want))
			if _, err = io.ReadFull(client, got); err != nil {
				t.Fatalf("peer read: %v", err)
			}
			if !bytes.Equal(got, want) {
				t.Fatal("peer received different bytes")
			}
		})
	}
}

// A read round's first owned output leaves for the socket while the callback
// that wrote it is still running — nothing of it shows as buffered — and the
// writes after it coalesce into the round's final flush. This is the shape of
// a reply batch handed over as one owned buffer: the round it was assembled
// in is usually its only writer. A write the threshold would buffer keeps
// joining the round's batch in the first position, so the configuration's
// promise holds there as everywhere.
func TestRoundFirstWriteGoesOutBeforeTheCallbackReturns(t *testing.T) {
	tests := []struct {
		name       string
		threshold  int
		wantFirst  int // buffered bytes right after the round's first write
		wantSecond int
	}{
		{name: "threshold-disabled", threshold: 0, wantFirst: 0, wantSecond: 3},
		{name: "below-threshold-joins-the-batch", threshold: 64, wantFirst: 3, wantSecond: 6},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			type buffered struct{ first, second int }
			events := &Events{Pollers: 1, WriteBufferedThreshold: tt.threshold}
			started := make(chan string, 1)
			measured := make(chan buffered, 8)
			events.OnStart = func(ev *Events) {
				for _, listener := range ev.acceptor.listeners {
					started <- listener.pair.local.String()
					return
				}
			}
			ownedWrite := func(conn Conn, value string) error {
				owned := AcquireBuffer(len(value))
				copy(owned.AvailableBuffer()[:len(value)], value)
				owned.CommitWrite(len(value))
				_, err := conn.WriteOwned(owned)
				return err
			}
			events.OnData = func(conn Conn) error {
				_, _ = conn.Discard(conn.InboundBuffered())
				if err := ownedWrite(conn, "one"); err != nil {
					return err
				}
				first := conn.OutboundBuffered()
				if err := ownedWrite(conn, "two"); err != nil {
					return err
				}
				measured <- buffered{first: first, second: conn.OutboundBuffered()}
				return nil
			}
			serveDone := make(chan error, 1)
			go func() { serveDone <- events.Serve("tcp://127.0.0.1:0") }()
			t.Cleanup(func() {
				_ = events.Close(nil)
				<-serveDone
			})
			client, err := net.Dial("tcp", <-started)
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			if _, err = client.Write([]byte("x")); err != nil {
				t.Fatal(err)
			}
			var got buffered
			select {
			case got = <-measured:
			case <-time.After(3 * time.Second):
				t.Fatal("the round never ran")
			}
			if got.first != tt.wantFirst {
				t.Fatalf("buffered after the round's first write = %d, want %d", got.first, tt.wantFirst)
			}
			if got.second != tt.wantSecond {
				t.Fatalf("buffered after the round's second write = %d, want %d", got.second, tt.wantSecond)
			}
			// The round's final flush publishes what coalesced, behind the
			// first write, in order.
			if err = client.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
				t.Fatal(err)
			}
			wire := make([]byte, 6)
			if _, err = io.ReadFull(client, wire); err != nil {
				t.Fatalf("peer read: %v", err)
			}
			if string(wire) != "onetwo" {
				t.Fatalf("peer received %q, want %q", wire, "onetwo")
			}
		})
	}
}
