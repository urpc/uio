package uio

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"runtime"
	"testing"
	"time"
)

func BenchmarkLayerEcho(b *testing.B) {
	for _, layers := range []int{0, 1, 3} {
		for _, observed := range []bool{false, true} {
			name := map[int]string{0: "layers=0", 1: "layers=1", 3: "layers=3"}[layers]
			if observed {
				name += "/observer"
			}
			b.Run(name, func(b *testing.B) {
				benchEcho(b, layers, observed)
			})
		}
	}
}

func benchEcho(b *testing.B, layers int, observed bool) {
	started := make(chan struct{})
	events := &Events{Pollers: 1, Handler: FuncHandler{Open: func(Conn) {
	}, Read: func(c Conn) error {
		chunk := c.PeekChunk()
		if len(chunk) == 0 {
			return nil
		}
		_, err := c.Write(chunk)
		_, _ = c.Discard(len(chunk))
		return err
	}, Close: func(Conn, error) {
	}}}
	events.Hooks = FuncHandler{Start: func(*Events) {
		close(started)
	}}
	if observed {
		events.Observer = FuncHandler{BytesRead: func(Conn, int) {
		}, BytesWritten: func(Conn, int) {
		}}
	}
	done := make(chan error, 1)
	go func() {
		done <- events.Serve()
	}()
	defer func() {
		_ = events.Close(nil)
		<-done
	}()
	<-started
	chain := make([]Layer, layers)
	for i := range chain {
		chain[i] = benchPassThrough{}
	}
	var listener Listener
	var err error
	if len(chain) > 0 {
		listener, err = events.Listen("tcp://127.0.0.1:0", WithLayers(chain...))
	} else {
		listener, err = events.Listen("tcp://127.0.0.1:0")
	}
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

func (benchPassThrough) Attach(LayerContext) LayerHandler {
	return PassThrough{}
}

func BenchmarkLayerPipelined(b *testing.B) {
	for _, layers := range []int{0, 1, 3} {
		b.Run(map[int]string{0: "layers=0", 1: "layers=1", 3: "layers=3"}[layers], func(b *testing.B) {
			benchPipelined(b, layers)
		})
	}
}

func benchPipelined(b *testing.B, layers int) {
	started := make(chan struct{})
	events := &Events{Pollers: 1, Handler: FuncHandler{Open: func(Conn) {
	}, Read: func(c Conn) error {
		chunk := c.PeekChunk()
		if len(chunk) == 0 {
			return nil
		}
		_, err := c.Write(chunk)
		_, _ = c.Discard(len(chunk))
		return err
	}, Close: func(Conn, error) {
	}}}
	events.Hooks = FuncHandler{Start: func(*Events) {
		close(started)
	}}
	done := make(chan error, 1)
	go func() {
		done <- events.Serve()
	}()
	defer func() {
		_ = events.Close(nil)
		<-done
	}()
	<-started
	chain := make([]Layer, layers)
	for i := range chain {
		chain[i] = benchPassThrough{}
	}
	var listener Listener
	var err error
	if len(chain) > 0 {
		listener, err = events.Listen("tcp://127.0.0.1:0", WithLayers(chain...))
	} else {
		listener, err = events.Listen("tcp://127.0.0.1:0")
	}
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
	const batch = 256
	out := make([]byte, // messages per write: 16 KiB in flight
		message*batch)
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

type layerBenchSocket struct {
	Conn
	user         any
	bytes, calls int
	outbound     int
}

func (s *layerBenchSocket) Userdata() any {
	return s.user
}

func (s *layerBenchSocket) SetUserdata(v any) {
	s.user = v
}

func (s *layerBenchSocket) LocalAddr() net.Addr {
	return nil
}

func (s *layerBenchSocket) State() ConnState {
	return 0
}

func (s *layerBenchSocket) SetWakeDeadline(time.Time) error {
	return nil
}

func (s *layerBenchSocket) Wake() error {
	return nil
}

func (s *layerBenchSocket) OutboundBuffered() int {
	return s.outbound
}

func (s *layerBenchSocket) Write(p []byte) (int, error) {
	s.bytes += len(p)
	s.calls++
	return len(p), nil
}

func (s *layerBenchSocket) Writev(v [][]byte) (int, error) {
	n := 0
	for _, p := range v {
		n += len(p)
	}
	s.bytes += n
	s.calls++
	return n, nil
}

type layerBenchCaps struct{ PassThrough }

func (layerBenchCaps) OutboundHeld(LayerContext) int {
	return 0
}

func (layerBenchCaps) OnClosing(LayerContext, error) {
}

var layerBenchCapsLayer Layer = LayerFunc(func(LayerContext) LayerHandler {
	return layerBenchCaps{}
})

func benchLayerChain(n int, kind string) []Layer {
	chain := make([]Layer, n)
	for i := range chain {
		switch kind {
		case "builtin":
			chain[i] = PassThroughLayer
		case "caps":
			chain[i] = layerBenchCapsLayer
		default:
			chain[i] = LayerFunc(func(LayerContext) LayerHandler {
				return PassThrough{}
			})
		}
	}
	return chain
}

func BenchmarkLayerAttach(b *testing.B) {
	for _, count := range []int{1, 3, 8, 63} {
		for _, kind := range []string{"builtin", "caps"} {
			b.Run(fmt.Sprintf("layers=%d/%s", count, kind), func(b *testing.B) {
				h := stackDriver{layers: benchLayerChain(count, kind), top: FuncHandler{}}
				socket := &layerBenchSocket{}
				b.ReportAllocs()
				b.ResetTimer()
				for b.Loop() {
					h.OnOpen(socket)
					h.OnClose(socket, nil)
					socket.user = nil
				}
				runtime.KeepAlive(socket)
			})
		}
	}
}

func BenchmarkLayerIdleTimer(b *testing.B) {
	for _, count := range []int{1, 3} {
		for _, state := range []string{"never", "cancelled", "fired", "active"} {
			b.Run(fmt.Sprintf("layers=%d/%s", count, state), func(b *testing.B) {
				socket := &layerBenchSocket{}
				h := stackDriver{layers: benchLayerChain(count, "builtin"), top: FuncHandler{}}
				h.OnOpen(socket)
				c := socket.user.(*stack)
				switch state {
				case "cancelled":
					if err := c.setTimer(0, time.Now().Add(time.Hour), 0); err != nil {
						b.Fatal(err)
					}
					if err := c.setTimer(0, time.Time{}, 0); err != nil {
						b.Fatal(err)
					}
				case "fired":
					if err := c.setTimer(0, time.Now().Add(-time.Second), 0); err != nil {
						b.Fatal(err)
					}
					if due := c.takeDue(); due != 1 {
						b.Fatalf("due=%x", due)
					}
				case "active":
					if err := c.setTimer(0, time.Now().Add(time.Hour), 0); err != nil {
						b.Fatal(err)
					}
				}
				b.ReportAllocs()
				b.ResetTimer()
				for b.Loop() {
					if due := c.takeDue(); due != 0 {
						b.Fatalf("unexpected due=%x", due)
					}
				}
				b.StopTimer()
				h.OnClose(socket, nil)
			})
		}
	}
}

func BenchmarkLayerWritev(b *testing.B) {
	for _, count := range []int{0, 1, 3} {
		for _, kind := range []string{"builtin", "fallback"} {
			if count == 0 && kind == "fallback" {
				continue
			}
			for _, size := range []int{64, 4096, 65536} {
				for _, segments := range []int{2, 8} {
					b.Run(fmt.Sprintf("layers=%d/%s/bytes=%d/segments=%d", count, kind, size, segments), func(b *testing.B) {
						socket := &layerBenchSocket{}
						var dst Conn = socket
						var h stackDriver
						if count > 0 {
							h = stackDriver{layers: benchLayerChain(count, kind), top: FuncHandler{}}
							h.OnOpen(socket)
							dst = socket.user.(*stack).topConn()
						}
						payload := make([]byte, size)
						for i := range payload {
							payload[i] = byte(i)
						}
						vec := make([][]byte, segments)
						for i := range vec {
							vec[i] = payload[i*size/segments : (i+1)*size/segments]
						}
						b.SetBytes(int64(size))
						b.ReportAllocs()
						b.ResetTimer()
						for b.Loop() {
							if n, err := dst.Writev(vec); err != nil || n != size {
								b.Fatalf("writev=(%d,%v)", n, err)
							}
						}
						b.StopTimer()
						if socket.bytes != b.N*size || socket.calls != b.N {
							b.Fatalf("socket received %d bytes in %d calls, want %d bytes in %d calls", socket.bytes, socket.calls, b.N*size, b.N)
						}
						runtime.KeepAlive(payload)
						runtime.KeepAlive(vec)
						if count > 0 {
							h.OnClose(socket, nil)
						}
					})
				}
			}
		}
	}
}

func BenchmarkLayerWritevTCP(b *testing.B) {
	for _, count := range []int{0, 1, 3} {
		for _, kind := range []string{"builtin", "fallback"} {
			if count == 0 && kind == "fallback" {
				continue
			}
			for _, shape := range []struct {
				name      string
				bytes     int
				streaming bool
			}{{"small", 64, false}, {"burst", 16384, true}, {"large", 65536, false}} {
				for _, segments := range []int{2, 8} {
					b.Run(fmt.Sprintf("layers=%d/%s/%s/segments=%d", count, kind, shape.name, segments), func(b *testing.B) {
						benchLayerWritevTCP(b, count, kind, shape.bytes, segments, shape.streaming)
					})
				}
			}
		}
	}
}

func benchLayerWritevTCP(b *testing.B, count int, kind string, size, segments int, streaming bool) {
	started := make(chan struct{})
	events := &Events{Pollers: 1, Handler: FuncHandler{Read: func(c Conn) error {
		chunk := c.PeekChunk()
		if len(chunk) == 0 {
			return nil
		}
		var local [8][]byte
		vec := local[:segments]
		for i := range vec {
			vec[i] = chunk[i*len(chunk)/segments : (i+1)*len(chunk)/segments]
		}
		n, err := c.Writev(vec)
		if err == nil && n != len(chunk) {
			return io.ErrShortWrite
		}
		if n > 0 {
			_, _ = c.Discard(n)
		}
		return err
	}}}
	events.Hooks = FuncHandler{Start: func(*Events) {
		close(started)
	}}
	done := make(chan error, 1)
	go func() {
		done <- events.Serve()
	}()
	defer func() {
		_ = events.Close(nil)
		<-done
	}()
	<-started
	var listener Listener
	var err error
	if count == 0 {
		listener, err = events.Listen("tcp://127.0.0.1:0")
	} else {
		listener, err = events.Listen("tcp://127.0.0.1:0", WithLayers(benchLayerChain(count, kind)...))
	}
	if err != nil {
		b.Fatal(err)
	}
	defer listener.Close()
	peer, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		b.Fatal(err)
	}
	defer peer.Close()
	if tcp, ok := peer.(*net.TCPConn); ok {
		if err = tcp.SetNoDelay(true); err != nil {
			b.Fatal(err)
		}
	}
	if err = peer.SetDeadline(time.Now().Add(30 * time.Second)); err != nil {
		b.Fatal(err)
	}
	out := make([]byte, size)
	for i := range out {
		out[i] = byte(i)
	}
	reply := make([]byte, size)
	if n, err := peer.Write(out); err != nil || n != len(out) {
		b.Fatalf("write=(%d,%v), want %d bytes", n, err, len(out))
	}
	if _, err = io.ReadFull(peer, reply); err != nil {
		b.Fatal(err)
	}
	if !bytes.Equal(out, reply) {
		b.Fatal("echo content mismatch")
	}
	b.SetBytes(int64(size))
	b.ReportAllocs()
	b.ResetTimer()
	if !streaming {
		for range b.N {
			if n, err := peer.Write(out); err != nil || n != len(out) {
				b.Fatalf("write=(%d,%v), want %d bytes", n, err, len(out))
			}
			if _, err = io.ReadFull(peer, reply); err != nil {
				b.Fatal(err)
			}
		}
	} else {
		readDone := make(chan error, 1)
		go func() {
			_, err := io.CopyN(io.Discard, peer, int64(b.N)*int64(size))
			readDone <- err
		}()
		for range b.N {
			if n, err := peer.Write(out); err != nil || n != len(out) {
				b.Fatalf("write=(%d,%v), want %d bytes", n, err, len(out))
			}
		}
		if err = <-readDone; err != nil {
			b.Fatal(err)
		}
	}
	runtime.KeepAlive(out)
	runtime.KeepAlive(reply)
}

type layerBenchHolder struct {
	PassThrough
	held int
}

func (h *layerBenchHolder) OutboundHeld(LayerContext) int {
	return h.held
}

func BenchmarkLayerQueueQueries(b *testing.B) {
	const socketBuffered, held = 13, 7
	for _, count := range []int{3, 8} {
		for _, position := range []string{"none", "bottom", "middle", "top"} {
			b.Run(fmt.Sprintf("layers=%d/%s/OutboundBuffered", count, position), func(b *testing.B) {
				chain := benchLayerChain(count, "builtin")
				holderIndex := -1
				switch position {
				case "bottom":
					holderIndex = 0
				case "middle":
					holderIndex = count / 2
				case "top":
					holderIndex = count - 1
				}
				if holderIndex >= 0 {
					chain[holderIndex] = LayerFunc(func(LayerContext) LayerHandler {
						return &layerBenchHolder{held: held}
					})
				}
				h := stackDriver{layers: chain, top: FuncHandler{}}
				socket := &layerBenchSocket{outbound: socketBuffered}
				h.OnOpen(socket)
				c := socket.user.(*stack)
				node := c.layers[len(c.layers)-1]
				want := uint64(socketBuffered)
				if holderIndex >= 0 {
					want += held
				}
				var total uint64
				b.ReportAllocs()
				b.ResetTimer()
				for b.Loop() {
					total += uint64(node.OutboundBuffered())
				}
				b.StopTimer()
				if total != uint64(b.N)*want {
					b.Fatalf("query total=%d, want %d iterations of %d", total, b.N, want)
				}
				runtime.KeepAlive(c)
				h.OnClose(socket, nil)
			})
		}
	}
}
