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
