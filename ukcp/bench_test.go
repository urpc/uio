package ukcp

import (
	"bytes"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urpc/uio"
	"github.com/urpc/uio/ukcp/internal/kcp"
)

func BenchmarkEngineStreamThroughput(b *testing.B) {
	c1, ctx1 := newBenchSession(b)
	c2, ctx2 := newBenchSession(b)
	c1.establish(42)
	c2.establish(42)

	chunk := bytes.Repeat([]byte("kcp-bench-payload-"), 1800) // ~32.4 KiB per write
	b.SetBytes(int64(len(chunk)))
	b.ReportAllocs()
	b.ResetTimer()

	var totalPackets, ackOnlyPackets int64
	for range b.N {
		ctx1.lower.wire = ctx1.lower.wire[:0]
		ctx2.lower.wire = ctx2.lower.wire[:0]
		ctx2.up.data = ctx2.up.data[:0]

		if _, err := c1.OnWrite(ctx1, chunk); err != nil {
			b.Fatal(err)
		}
		if f, ok := any(c1).(interface{ OnTurnEnd(uio.LayerContext) }); ok {
			f.OnTurnEnd(ctx1)
		}

		for _, pkt := range ctx1.lower.wire {
			totalPackets++
			ctx2.lower.inbound = pkt
			if err := c2.OnRead(ctx2); err != nil {
				b.Fatal(err)
			}
			if f, ok := any(c2).(interface{ OnTurnEnd(uio.LayerContext) }); ok {
				f.OnTurnEnd(ctx2)
			}
		}
		for _, ackPkt := range ctx2.lower.wire {
			totalPackets++
			if pushBytes(ackPkt) == 0 {
				ackOnlyPackets++
			}
			ctx1.lower.inbound = ackPkt
			if err := c1.OnRead(ctx1); err != nil {
				b.Fatal(err)
			}
			if f, ok := any(c1).(interface{ OnTurnEnd(uio.LayerContext) }); ok {
				f.OnTurnEnd(ctx1)
			}
		}
		if len(ctx2.up.data) != len(chunk) {
			b.Fatalf("delivered %d, want %d", len(ctx2.up.data), len(chunk))
		}
	}
	b.ReportMetric(float64(totalPackets)/float64(b.N), "pkts/op")
	b.ReportMetric(float64(ackOnlyPackets)/float64(b.N), "ack_pkts/op")
}

func newBenchSession(tb testing.TB) (*conn, *contractContext) {
	tb.Helper()
	ctx := &contractContext{lower: &contractLower{}, up: &contractUp{}}
	engine := newLayer(Options{KeepAlive: -1, SendWindow: 1024, RecvWindow: 1024}, false).Attach(ctx).(*conn)
	engine.OnOpen(ctx)
	return engine, ctx
}

func BenchmarkUDPBulkThroughput(b *testing.B) {
	const chunkSize = 32 << 10
	var srvRecv atomic.Int64
	var srvPktsIn, srvBytesOut atomic.Int64
	wake := make(chan struct{}, 1)

	srvEv := &uio.Events{
		Pollers:        1,
		ReadBufferSize: 4 << 10,
		BlockingWrite:  true,
		Handler: uio.FuncHandler{
			Open: func(c uio.Conn) {
				if raw, ok := uio.SocketOf(c); ok {
					_ = raw.SetReadBuffer(4 << 20)
					_ = raw.SetWriteBuffer(4 << 20)
				}
			},
			Read: func(c uio.Conn) error {
				n, _ := c.Discard(-1)
				if n > 0 {
					srvRecv.Add(int64(n))
					select {
					case wake <- struct{}{}:
					default:
					}
				}
				return nil
			},
		},
		Observer: uio.FuncHandler{
			BytesRead:    func(_ uio.Conn, _ int) { srvPktsIn.Add(1) },
			BytesWritten: func(_ uio.Conn, n int) { srvBytesOut.Add(int64(n)) },
		},
	}
	srvStarted := make(chan struct{})
	srvEv.Hooks = uio.FuncHandler{Start: func(*uio.Events) { close(srvStarted) }}
	srvDone := make(chan error, 1)
	go func() { srvDone <- srvEv.Serve() }()
	<-srvStarted
	defer func() {
		_ = srvEv.Close(nil)
		<-srvDone
	}()

	ln, err := srvEv.Listen("udp://127.0.0.1:0",
		uio.WithLayers(ServerLayer(Options{KeepAlive: -1, SendWindow: 2048, RecvWindow: 2048, MaxBuffered: 8 << 20})),
	)
	if err != nil {
		b.Fatal(err)
	}
	defer ln.Close()

	var cliPktsOut, cliPktsIn atomic.Int64
	cliEv := &uio.Events{
		Pollers:             1,
		ReadBufferSize:      4 << 10,
		BlockingWrite:       true,
		MaxOutboundBuffered: 2 << 20,
		Handler: uio.FuncHandler{
			Open: func(c uio.Conn) {
				if raw, ok := uio.SocketOf(c); ok {
					_ = raw.SetReadBuffer(4 << 20)
					_ = raw.SetWriteBuffer(4 << 20)
				}
			},
			Read: func(c uio.Conn) error { _, _ = c.Discard(-1); return nil },
		},
		Observer: uio.FuncHandler{
			BytesWritten: func(_ uio.Conn, _ int) { cliPktsOut.Add(1) },
			BytesRead:    func(_ uio.Conn, _ int) { cliPktsIn.Add(1) },
		},
	}
	cliStarted := make(chan struct{})
	cliEv.Hooks = uio.FuncHandler{Start: func(*uio.Events) { close(cliStarted) }}
	cliDone := make(chan error, 1)
	go func() { cliDone <- cliEv.Serve() }()
	<-cliStarted
	defer func() {
		_ = cliEv.Close(nil)
		<-cliDone
	}()

	cli, err := cliEv.Dial("udp://"+ln.Addr().String(),
		uio.WithLayers(ClientLayer(Options{KeepAlive: -1, SendWindow: 2048, RecvWindow: 2048, MaxBuffered: 8 << 20})),
	)
	if err != nil {
		b.Fatal(err)
	}
	defer cli.Close()

	payload := bytes.Repeat([]byte("U"), chunkSize)
	// Warmup 32 chunks (1 MiB) so RingBuffer and bufferPool reach steady-state capacity before ResetTimer.
	const warmChunks = 32
	for range warmChunks {
		if _, err := cli.Write(payload); err != nil {
			b.Fatal(err)
		}
	}
	for srvRecv.Load() < warmChunks*chunkSize {
		<-wake
	}
	srvRecv.Store(0)
	cliPktsOut.Store(0)
	cliPktsIn.Store(0)
	srvBytesOut.Store(0)

	b.SetBytes(chunkSize)
	b.ReportAllocs()
	b.ResetTimer()

	want := int64(b.N) * chunkSize
	writeDone := make(chan error, 1)
	go func() {
		for range b.N {
			if _, err := cli.Write(payload); err != nil {
				writeDone <- err
				return
			}
		}
		writeDone <- nil
	}()

	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	deadline := time.Now().Add(30 * time.Second)
	for srvRecv.Load() < want {
		select {
		case <-wake:
		case <-ticker.C:
			if time.Now().After(deadline) {
				b.Fatalf("timeout: got %d / %d", srvRecv.Load(), want)
			}
		}
	}
	if err := <-writeDone; err != nil {
		b.Fatal(err)
	}
	b.StopTimer()
	b.ReportMetric(float64(cliPktsIn.Load())/float64(b.N), "ack_reads/32KB")
	b.ReportMetric(float64(srvBytesOut.Load())/float64(b.N), "ack_bytes/32KB")
	_ = kcp.IKCP_CMD_ACK
}

func BenchmarkUDPEchoPingPong(b *testing.B) {
	replyCh := make(chan struct{}, 1)
	var srvBytesOut, cliBytesOut atomic.Int64

	srvEv := &uio.Events{
		Pollers:        1,
		ReadBufferSize: 4 << 10,
		Handler: uio.FuncHandler{
			Read: func(c uio.Conn) error {
				_, err := c.WriteTo(c)
				return err
			},
		},
		Observer: uio.FuncHandler{
			BytesWritten: func(_ uio.Conn, n int) { srvBytesOut.Add(int64(n)) },
		},
	}
	srvStarted := make(chan struct{})
	srvEv.Hooks = uio.FuncHandler{Start: func(*uio.Events) { close(srvStarted) }}
	srvDone := make(chan error, 1)
	go func() { srvDone <- srvEv.Serve() }()
	<-srvStarted
	defer func() {
		_ = srvEv.Close(nil)
		<-srvDone
	}()

	ln, err := srvEv.Listen("udp://127.0.0.1:0", uio.WithLayers(ServerLayer(Options{KeepAlive: -1, NoDelay: true, Interval: 10 * time.Millisecond})))
	if err != nil {
		b.Fatal(err)
	}
	defer ln.Close()

	var recvBytes atomic.Int64
	cliEv := &uio.Events{
		Pollers:        1,
		ReadBufferSize: 4 << 10,
		Handler: uio.FuncHandler{
			Read: func(c uio.Conn) error {
				n, _ := c.Discard(-1)
				if recvBytes.Add(int64(n)) >= 64 {
					recvBytes.Add(-64)
					select {
					case replyCh <- struct{}{}:
					default:
					}
				}
				return nil
			},
		},
		Observer: uio.FuncHandler{
			BytesWritten: func(_ uio.Conn, n int) { cliBytesOut.Add(int64(n)) },
		},
	}
	cliStarted := make(chan struct{})
	cliEv.Hooks = uio.FuncHandler{Start: func(*uio.Events) { close(cliStarted) }}
	cliDone := make(chan error, 1)
	go func() { cliDone <- cliEv.Serve() }()
	<-cliStarted
	defer func() {
		_ = cliEv.Close(nil)
		<-cliDone
	}()

	cli, err := cliEv.Dial("udp://"+ln.Addr().String(), uio.WithLayers(ClientLayer(Options{KeepAlive: -1, NoDelay: true, Interval: 10 * time.Millisecond})))
	if err != nil {
		b.Fatal(err)
	}
	defer cli.Close()

	msg := bytes.Repeat([]byte("e"), 64)
	_, _ = cli.Write(msg)
	<-replyCh
	srvBytesOut.Store(0)
	cliBytesOut.Store(0)

	b.SetBytes(64)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if _, err := cli.Write(msg); err != nil {
			b.Fatal(err)
		}
		<-replyCh
	}
	b.StopTimer()
	b.ReportMetric(float64(srvBytesOut.Load())/float64(b.N), "srv_wire_B/op")
	b.ReportMetric(float64(cliBytesOut.Load())/float64(b.N), "cli_wire_B/op")
}

func BenchmarkUDPPipelinedEcho(b *testing.B) {
	const batchMsgs = 32
	const msgSize = 64
	const roundBytes = batchMsgs * msgSize

	replyCh := make(chan struct{}, 1)
	var srvPktsOut, cliPktsOut atomic.Int64

	srvEv := &uio.Events{
		Pollers:        1,
		ReadBufferSize: 4 << 10,
		Handler: uio.FuncHandler{
			Read: func(c uio.Conn) error {
				var buf [msgSize]byte
				for {
					n, err := c.Read(buf[:])
					if n > 0 {
						if _, werr := c.Write(buf[:n]); werr != nil {
							return werr
						}
					}
					if err != nil || n == 0 {
						return nil
					}
				}
			},
		},
		Observer: uio.FuncHandler{
			BytesWritten: func(_ uio.Conn, _ int) { srvPktsOut.Add(1) },
		},
	}
	srvStarted := make(chan struct{})
	srvEv.Hooks = uio.FuncHandler{Start: func(*uio.Events) { close(srvStarted) }}
	srvDone := make(chan error, 1)
	go func() { srvDone <- srvEv.Serve() }()
	<-srvStarted
	defer func() {
		_ = srvEv.Close(nil)
		<-srvDone
	}()

	ln, err := srvEv.Listen("udp://127.0.0.1:0", uio.WithLayers(ServerLayer(Options{KeepAlive: -1, NoDelay: true, Interval: 10 * time.Millisecond})))
	if err != nil {
		b.Fatal(err)
	}
	defer ln.Close()

	var recvBytes atomic.Int64
	cliEv := &uio.Events{
		Pollers:        1,
		ReadBufferSize: 4 << 10,
		Handler: uio.FuncHandler{
			Read: func(c uio.Conn) error {
				n, _ := c.Discard(-1)
				if recvBytes.Add(int64(n)) >= roundBytes {
					recvBytes.Add(-roundBytes)
					select {
					case replyCh <- struct{}{}:
					default:
					}
				}
				return nil
			},
		},
		Observer: uio.FuncHandler{
			BytesWritten: func(_ uio.Conn, _ int) { cliPktsOut.Add(1) },
		},
	}
	cliStarted := make(chan struct{})
	cliEv.Hooks = uio.FuncHandler{Start: func(*uio.Events) { close(cliStarted) }}
	cliDone := make(chan error, 1)
	go func() { cliDone <- cliEv.Serve() }()
	<-cliStarted
	defer func() {
		_ = cliEv.Close(nil)
		<-cliDone
	}()

	cli, err := cliEv.Dial("udp://"+ln.Addr().String(), uio.WithLayers(ClientLayer(Options{KeepAlive: -1, NoDelay: true, Interval: 10 * time.Millisecond})))
	if err != nil {
		b.Fatal(err)
	}
	defer cli.Close()

	msg := bytes.Repeat([]byte("p"), msgSize)
	for range batchMsgs {
		_, _ = cli.Write(msg)
	}
	<-replyCh
	srvPktsOut.Store(0)
	cliPktsOut.Store(0)

	b.SetBytes(roundBytes)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		for range batchMsgs {
			if _, err := cli.Write(msg); err != nil {
				b.Fatal(err)
			}
		}
		<-replyCh
	}
	b.StopTimer()
	b.ReportMetric(float64(srvPktsOut.Load())/float64(b.N), "srv_writes/32msg")
}
