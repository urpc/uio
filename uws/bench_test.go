package uws

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"fmt"
	"github.com/urpc/uio"
	"github.com/urpc/uio/utls"
	"github.com/urpc/uio/uws/internal/frame"
	"math/rand/v2"
	"net"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"
)

type echoPathConn struct {
	probeReadAccess
	uio.Conn
	probeReadControl
	inbound  []byte
	out      []byte
	userdata any
	closes   atomic.Int32
} // Close/Abort calls, from the close timer among others

func (c *echoPathConn) PeekChunk() []byte {
	return c.inbound
}

func (c *echoPathConn) OutboundBuffered() int {
	return 0
}

func (c *echoPathConn) InboundBuffered() int {
	return len(c.inbound)
}

func (c *echoPathConn) Userdata() any {
	return c.userdata
}

func (c *echoPathConn) Flush() error {
	return nil
}

func (c *echoPathConn) Close() error {
	return c.Abort(nil)
}

func (c *echoPathConn) Abort(error) error {
	c.closes.Add(1)
	return nil
}

func (c *echoPathConn) Discard(n int) (int, error) {
	c.inbound = c.inbound[n:]
	return n, nil
}

func (c *echoPathConn) WriteOwned(buffer *uio.Buffer) (int, error) {
	n := buffer.Len()
	if cap(c.out)-len(c.out) >= n {
		c.out = append(c.out, buffer.Bytes()...)
	}
	uio.ReleaseBuffer(buffer)
	return n, nil
}

type echoPathHandler struct{}

func (echoPathHandler) OnOpen(*Conn) {
}

func (echoPathHandler) OnMessage(conn *Conn, message Message) {
	_ = conn.SendBinary(message.Bytes())
}

func (echoPathHandler) OnClose(*Conn, CloseEvent) {
}

func BenchmarkServerEchoPath(b *testing.B) {
	for _, frames := range []int{1, 100} {
		for _, size := range []int{128, 1024} {
			b.Run(fmt.Sprintf("frames=%d/payload=%d", frames, size), func(b *testing.B) {
				server := NewServer(Options{Handler: echoPathHandler{}})
				server.config = testServerConfig(server)
				raw := &echoPathConn{}
				conn := server.newConnection(raw)
				conn.opened.Store(true)
				raw.userdata = conn
				var wire []byte
				for range frames {
					wire = frame.Append(wire, frame.Frame{Fin: true, Opcode: frame.Binary, Masked: true, Payload: make([]byte, size)}, [4]byte{1, 2, 3, 4})
				}
				chunk := make([]byte, len(wire))
				raw.out = make([]byte, 0, frames*(size+14))
				b.SetBytes(int64(frames * size))
				b.ReportAllocs()
				b.ResetTimer()
				for range b.N {
					copy(chunk, wire)
					raw.inbound = chunk
					raw.out = raw.out[:0]
					if err := server.onData(raw); err != nil {
						b.Fatal(err)
					}
				}
				b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*frames), "ns/frame")
			})
		}
	}
}

func BenchmarkServerEchoPathSplitReads(b *testing.B) {
	const readSize = 16 << 10
	for _, size := range []int{64, 256, 1024} {
		b.Run(fmt.Sprintf("payload=%d", size), func(b *testing.B) {
			server := NewServer(Options{Handler: echoPathHandler{}})
			server.config = testServerConfig(server)
			raw := &echoPathConn{}
			conn := server.newConnection(raw)
			conn.opened.Store(true)
			raw.userdata = conn
			var wire []byte
			frames := 0
			for len(wire) < 4*readSize-size {
				wire = frame.Append(wire, frame.Frame{Fin: true, Opcode: frame.Binary, Masked: true, Payload: make([]byte, size)}, [4]byte{1, 2, 3, 4})
				frames++
			}
			chunk := make([]byte, len(wire))
			raw.out = make([]byte, 0, frames*(size+14))
			b.SetBytes(int64(frames * size))
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				copy(chunk, wire)
				raw.out = raw.out[:0]
				for read := chunk; len(read) > 0; {
					n := min(readSize, len(read))
					raw.inbound = read[:n]
					if err := server.onData(raw); err != nil {
						b.Fatal(err)
					}
					if len(raw.inbound) != 0 {
						b.Fatalf("%d bytes of a read left unconsumed", len(raw.inbound))
					}
					read = read[n:]
				}
			}
			b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*frames), "ns/frame")
		})
	}
}

type echoTextHandler struct{}

func (echoTextHandler) OnOpen(*Conn) {
}

func (echoTextHandler) OnMessage(conn *Conn, message Message) {
	_ = conn.SendText(message.Text())
}

func (echoTextHandler) OnClose(*Conn, CloseEvent) {
}

func BenchmarkServerEchoPathText(b *testing.B) {
	for _, multibyte := range []bool{false, true} {
		for _, size := range []int{128, 1024, 4000} {
			b.Run(fmt.Sprintf("multibyte=%v/payload=%d", multibyte, size), func(b *testing.B) {
				server := NewServer(Options{Handler: echoTextHandler{}})
				server.config = testServerConfig(server)
				raw := &echoPathConn{}
				conn := server.newConnection(raw)
				conn.opened.Store(true)
				raw.userdata = conn
				unit := []byte(`{"k":"v"}`)
				if multibyte {
					unit = []byte("文本帧")
				}
				payload := bytes.Repeat(unit, size/len(unit)+1)[:size/len(unit)*len(unit)]
				payload = append(payload, bytes.Repeat([]byte{'x'}, size-len(payload))...)
				if len(payload) != size || !utf8.Valid(payload) {
					b.Fatalf("payload of %d bytes (valid UTF-8 %v), want %d", len(payload), utf8.Valid(payload), size)
				}
				const frames = 8
				var wire []byte
				for range frames {
					wire = frame.Append(wire, frame.Frame{Fin: true, Opcode: frame.Text, Masked: true, Payload: payload}, [4]byte{1, 2, 3, 4})
				}
				chunk := make([]byte, len(wire))
				raw.out = make([]byte, 0, frames*(len(payload)+14))
				b.SetBytes(int64(frames * len(payload)))
				b.ReportAllocs()
				b.ResetTimer()
				for range b.N {
					copy(chunk, wire)
					raw.inbound = chunk
					raw.out = raw.out[:0]
					if err := server.onData(raw); err != nil {
						b.Fatal(err)
					}
				}
				b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*frames), "ns/frame")
			})
		}
	}
}

func BenchmarkClientMaskedFrameWrite(b *testing.B) {
	for _, size := range []int{1024, 1 << 20} {
		b.Run(fmt.Sprintf("%d", size), func(b *testing.B) {
			conn := &Conn{raw: &writeProbeConn{}, config: testDialerConfig(&Dialer{Options: Options{MaxFramePayload: uint64(size)}})}
			message := frame.Frame{Fin: true, Opcode: frame.Binary, Payload: make([]byte, size)}
			b.ReportAllocs()
			b.SetBytes(int64(size))
			b.ResetTimer()
			for range b.N {
				if err := conn.sendFrameLocked(message); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkServerUnmaskedFrameWrite(b *testing.B) {
	for _, size := range []int{1024, 1 << 20} {
		b.Run(fmt.Sprintf("%d", size), func(b *testing.B) {
			conn := &Conn{raw: &writeProbeConn{}, config: testServerConfig(&Server{Options: Options{MaxFramePayload: uint64(size)}})}
			message := frame.Frame{Fin: true, Opcode: frame.Binary, Payload: make([]byte, size)}
			b.ReportAllocs()
			b.SetBytes(int64(size))
			b.ResetTimer()
			for b.Loop() {
				if err := conn.sendFrameLocked(message); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkHeartbeatTickSteady(b *testing.B) {
	server := NewServer(Options{})
	server.HeartbeatInterval = time.Hour
	raw := newScriptedConn()
	conn := &Conn{raw: raw, config: testServerConfig(server), heartbeat: &heartbeatState{}}
	conn.opened.Store(true)
	conn.beginHeartbeat()
	now := time.Now()
	b.ReportAllocs()
	for b.Loop() {
		conn.heartbeatTick(now)
	}
}

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
				events := &uio.Events{Pollers: 1, Handler: uio.FuncHandler{Open: func(uio.Conn) {
				}, Read: func(c uio.Conn) error {
					if chunk := c.PeekChunk(); len(chunk) > 0 {
						if _, err := c.Write(chunk); err != nil {
							return err
						}
						_, _ = c.Discard(len(chunk))
					}
					return nil
				}, Close: func(uio.Conn, error) {
				}}}
				if mode != "layer" {
					events.WriteBufferedThreshold = defaultWriteBufferedThreshold
				}
				started := make(chan struct{})
				events.Hooks = uio.FuncHandler{Start: func(*uio.Events) {
					close(started)
				}}
				var chain []uio.Layer
				if secure {
					events.ReadBufferSize = 16 << 10
					chain = append(chain, utls.ServerLayer(utls.LayerOptions{TLSConfig: serverTLS}))
				}
				chain = append(chain, ServerLayer(LayerOptions{}))
				done := make(chan error, 1)
				go func() {
					done <- events.Serve()
				}()
				b.Cleanup(func() {
					_ = events.Close(nil)
					<-done
				})
				<-started
				listener, err := events.Listen("tcp://127.0.0.1:0", uio.WithLayers(chain...))
				if err != nil {
					b.Fatal(err)
				}
				b.Cleanup(func() {
					_ = listener.Close()
				})
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

func benchServe(b *testing.B, opts Options) string {
	b.Helper()
	opts.Handler, opts.Pollers = robustEchoHandler{}, 1
	for range 20 {
		addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(20000+rand.IntN(12000)))
		server := NewServer(opts)
		done := make(chan error, 1)
		go func() {
			done <- server.Serve(addr)
		}()
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
			b.Cleanup(func() {
				_ = server.Close(nil)
				<-done
			})
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
	request := "GET / HTTP/1.1\r\nHost: x\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n" + "Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\n\r\n"
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
