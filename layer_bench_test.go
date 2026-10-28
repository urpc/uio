//go:build linux || darwin || netbsd || freebsd || openbsd || dragonfly || stdio

package uio

import (
	"context"
	"net"
	"testing"
	"time"
)

// BenchmarkLayerEcho measures a small request/response round trip — the
// shape where per-turn overhead shows, not bulk throughput — through 0, 1
// and 3 pass-through layers on the serving side, with and without an
// Observer installed.
func BenchmarkLayerEcho(b *testing.B) {
	for _, layers := range []int{0, 1, 3} {
		for _, observed := range []bool{false, true} {
			name := map[int]string{0: "layers=0", 1: "layers=1", 3: "layers=3"}[layers]
			if observed {
				name += "/observer"
			}
			b.Run(name, func(b *testing.B) { benchEcho(b, layers, observed) })
		}
	}
}

func benchEcho(b *testing.B, layers int, observed bool) {
	started := make(chan struct{})
	events := &Events{Pollers: 1, Handler: FuncHandler{
		Open: func(Conn) {},
		Read: func(c Conn) error {
			chunk := c.PeekChunk()
			if len(chunk) == 0 {
				return nil
			}
			_, err := c.Write(chunk)
			_, _ = c.Discard(len(chunk))
			return err
		},
		Close: func(Conn, error) {},
	}}
	events.Hooks = FuncHandler{Start: func(*Events) { close(started) }}
	if observed {
		events.Observer = FuncHandler{
			BytesRead:    func(Conn, int) {},
			BytesWritten: func(Conn, int) {},
		}
	}
	for range layers {
		events.Use(benchPassThrough{})
	}
	done := make(chan error, 1)
	go func() { done <- events.Serve() }()
	defer func() { _ = events.Close(nil); <-done }()
	<-started
	listener, err := events.Listen("tcp://127.0.0.1:0", nil)
	if err != nil {
		b.Fatal(err)
	}
	defer listener.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var dialer net.Dialer
	peer, err := dialer.DialContext(ctx, "tcp", listener.Addr().String())
	if err != nil {
		b.Fatal(err)
	}
	defer peer.Close()
	if tcp, ok := peer.(*net.TCPConn); ok {
		_ = tcp.SetNoDelay(true)
	}
	message := make([]byte, 64)
	reply := make([]byte, 64)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if _, err := peer.Write(message); err != nil {
			b.Fatal(err)
		}
		for got := 0; got < len(reply); {
			n, err := peer.Read(reply[got:])
			if err != nil {
				b.Fatal(err)
			}
			got += n
		}
	}
}

type benchPassThrough struct{}

func (benchPassThrough) Attach(LayerContext) LayerHandler { return PassThrough{} }

// BenchmarkLayerPipelined streams small messages through the server
// back to back — the CPU, not the round trip, bounds it, so per-turn and
// per-layer costs show.
func BenchmarkLayerPipelined(b *testing.B) {
	for _, layers := range []int{0, 1, 3} {
		b.Run(map[int]string{0: "layers=0", 1: "layers=1", 3: "layers=3"}[layers], func(b *testing.B) {
			benchPipelined(b, layers)
		})
	}
}

func benchPipelined(b *testing.B, layers int) {
	started := make(chan struct{})
	events := &Events{Pollers: 1, Handler: FuncHandler{
		Open: func(Conn) {},
		Read: func(c Conn) error {
			chunk := c.PeekChunk()
			if len(chunk) == 0 {
				return nil
			}
			_, err := c.Write(chunk)
			_, _ = c.Discard(len(chunk))
			return err
		},
		Close: func(Conn, error) {},
	}}
	events.Hooks = FuncHandler{Start: func(*Events) { close(started) }}
	for range layers {
		events.Use(benchPassThrough{})
	}
	done := make(chan error, 1)
	go func() { done <- events.Serve() }()
	defer func() { _ = events.Close(nil); <-done }()
	<-started
	listener, err := events.Listen("tcp://127.0.0.1:0", nil)
	if err != nil {
		b.Fatal(err)
	}
	defer listener.Close()
	peer, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		b.Fatal(err)
	}
	defer peer.Close()

	const message = 64
	const batch = 256 // messages per write: 16 KiB in flight
	out := make([]byte, message*batch)
	in := make([]byte, 64<<10)
	readDone := make(chan error, 1)
	total := 0
	b.SetBytes(int64(len(out)))
	b.ReportAllocs()
	b.ResetTimer()
	go func() {
		want := 0
		for range b.N {
			want += len(out)
		}
		for got := 0; got < want; {
			n, err := peer.Read(in)
			if err != nil {
				readDone <- err
				return
			}
			got += n
		}
		readDone <- nil
	}()
	for range b.N {
		if _, err := peer.Write(out); err != nil {
			b.Fatal(err)
		}
		total += len(out)
	}
	if err := <-readDone; err != nil {
		b.Fatal(err)
	}
}
