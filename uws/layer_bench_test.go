package uws

import (
	"bufio"
	"crypto/tls"
	"math/rand/v2"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/urpc/uio"
	"github.com/urpc/uio/utls"
)

// BenchmarkSocketEcho compares a Server with a ServerLayer stack over real
// sockets, in the clear and over TLS: a raw client pipelines a batch of small
// masked frames, and the server echoes their payloads. The layer's Handler
// echoes the stream, so the reply frames differ; the client counts payload
// bytes.
func BenchmarkSocketEcho(b *testing.B) {
	const frames, size = 64, 64
	serverTLS, clientTLS := wssConfigs(b)
	for _, mode := range []string{"server", "layer", "layer-coalesced", "wss-server", "wss-layer"} {
		b.Run(mode, func(b *testing.B) {
			var addr string
			secure := strings.HasPrefix(mode, "wss")
			switch mode {
			case "server":
				addr = benchServe(b, Options{})
			case "wss-server":
				addr = benchServe(b, Options{TLSConfig: serverTLS, ReadBufferSize: 16 << 10})
			default:
				events := &uio.Events{Pollers: 1, Handler: uio.FuncHandler{
					Open: func(uio.Conn) {},
					Read: func(c uio.Conn) error {
						if chunk := c.PeekChunk(); len(chunk) > 0 {
							if _, err := c.Write(chunk); err != nil {
								return err
							}
							_, _ = c.Discard(len(chunk))
						}
						return nil
					},
					Close: func(uio.Conn, error) {},
				}}
				if mode != "layer" {
					events.WriteBufferedThreshold = defaultWriteBufferedThreshold
				}
				started := make(chan struct{})
				events.Hooks = uio.FuncHandler{Start: func(*uio.Events) { close(started) }}
				var chain []uio.Layer
				if secure {
					events.MaxBufferSize = 16 << 10
					chain = append(chain, utls.Server(serverTLS))
				}
				chain = append(chain, ServerLayer(LayerOptions{}))
				done := make(chan error, 1)
				go func() { done <- events.Serve() }()
				b.Cleanup(func() { _ = events.Close(nil); <-done })
				<-started
				listener, err := events.Listen("tcp://127.0.0.1:0", uio.WithLayers(chain...))
				if err != nil {
					b.Fatal(err)
				}
				b.Cleanup(func() { _ = listener.Close() })
				addr = listener.Addr().String()
			}
			conn, reader := benchUpgrade(b, addr, secure, clientTLS)
			defer conn.Close()
			batch := make([]byte, 0, frames*(size+6))
			for range frames {
				batch = append(batch, 0x82, 0x80|size, 1, 2, 3, 4)
				for i := range size {
					batch = append(batch, byte(i)^[]byte{1, 2, 3, 4}[i%4])
				}
			}
			b.SetBytes(frames * size)
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				if _, err := conn.Write(batch); err != nil {
					b.Fatal(err)
				}
				for got := 0; got < frames*size; {
					n, err := readFramePayload(reader)
					if err != nil {
						b.Fatal(err)
					}
					got += n
				}
			}
		})
	}
}

// benchServe serves an echoing Server with opts on a free loopback port.
func benchServe(b *testing.B, opts Options) string {
	b.Helper()
	opts.Handler, opts.Pollers = robustEchoHandler{}, 1
	for range 20 {
		addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(20000+rand.IntN(12000)))
		server := NewServer(opts)
		done := make(chan error, 1)
		go func() { done <- server.Serve(addr) }()
		deadline := time.Now().Add(5 * time.Second)
		for !server.ready.Load() && time.Now().Before(deadline) {
			select {
			case <-done:
				deadline = time.Time{}
			default:
				time.Sleep(time.Millisecond)
			}
		}
		if server.ready.Load() {
			b.Cleanup(func() { _ = server.Close(nil); <-done })
			return addr
		}
	}
	b.Fatal("could not bind a loopback port")
	return ""
}

func benchUpgrade(b *testing.B, addr string, secure bool, config *tls.Config) (net.Conn, *bufio.Reader) {
	b.Helper()
	var conn net.Conn
	var err error
	if secure {
		conn, err = tls.Dial("tcp", addr, config)
	} else {
		conn, err = net.Dial("tcp", addr)
	}
	if err != nil {
		b.Fatal(err)
	}
	request := "GET / HTTP/1.1\r\nHost: x\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n" +
		"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\n\r\n"
	if _, err := conn.Write([]byte(request)); err != nil {
		b.Fatal(err)
	}
	reader := bufio.NewReaderSize(conn, 64<<10)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			b.Fatal(err)
		}
		if line == "\r\n" {
			return conn, reader
		}
	}
}

// readFramePayload skips one unmasked frame and reports its payload length.
// It reads byte by byte from the buffer, so the client allocates nothing.
func readFramePayload(r *bufio.Reader) (int, error) {
	if _, err := r.ReadByte(); err != nil {
		return 0, err
	}
	b, err := r.ReadByte()
	if err != nil {
		return 0, err
	}
	n, ext := int(b&0x7f), 0
	switch n {
	case 126:
		ext = 2
	case 127:
		ext = 8
	}
	if ext > 0 {
		n = 0
		for range ext {
			b, err := r.ReadByte()
			if err != nil {
				return 0, err
			}
			n = n<<8 | int(b)
		}
	}
	_, err = r.Discard(n)
	return n, err
}
