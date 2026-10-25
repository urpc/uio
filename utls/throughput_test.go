package utls

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"os"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urpc/uio"
)

// TestThroughputCompare measures bulk-transfer throughput through the TLS
// stack against the standard library's, with plain TCP baselines on both
// sides: the same server-to-client download and client-to-server upload, one
// connection, one payload, every configuration in the same process. The
// handshake is over before each transfer is timed, on either side.
//
// It is skipped unless UTLS_BENCH is set, so the suite does not carry it:
//
//	UTLS_BENCH=1 go test -run TestThroughputCompare -v ./utls/
//
// The configurations:
//
//	std tcp    net.Listen and net.Dial, no TLS
//	std tls    crypto/tls.Listen and tls.Dial
//	uio        UIO over TCP, no wrappers installed
//	utls       UIO over TCP with a TLS wrapper installed, both sides ours
func TestThroughputCompare(t *testing.T) {
	if os.Getenv("UTLS_BENCH") == "" {
		t.Skip("skipping: set UTLS_BENCH=1 to measure throughput")
	}
	total := 64 << 20
	if v := os.Getenv("UTLS_BENCH_BYTES"); v != "" {
		n := 0
		for _, c := range v {
			if c < '0' || c > '9' {
				t.Fatalf("UTLS_BENCH_BYTES: %q", v)
			}
			n = n*10 + int(c-'0')
		}
		total = n
	}
	rounds := 3
	t.Logf("%s/%s, %d CPUs, %d MiB per transfer, best of %d rounds, handshake excluded",
		runtime.GOOS, runtime.GOARCH, runtime.NumCPU(), total>>20, rounds)

	type configuration struct {
		name string
		tls  bool
		ours bool
	}
	configs := []configuration{
		{name: "std tcp"},
		{name: "std tls", tls: true},
		{name: "uio", ours: true},
		{name: "utls", tls: true, ours: true},
	}
	serverConfig, clientConfig := testConfigs(t)

	report := map[string][2]float64{}
	for _, direction := range []struct {
		name     string
		download bool
	}{{"download", true}, {"upload", false}} {
		for _, config := range configs {
			best := 0.0
			for range rounds {
				var rate float64
				if config.ours {
					rate = uioRound(t, config.tls, direction.download, total, serverConfig, clientConfig)
				} else {
					rate = stdRound(t, config.tls, direction.download, total, serverConfig, clientConfig)
				}
				best = max(best, rate)
			}
			entry := report[config.name]
			if direction.download {
				entry[0] = best
			} else {
				entry[1] = best
			}
			report[config.name] = entry
		}
	}
	t.Log("configuration    download MB/s   upload MB/s")
	for _, config := range configs {
		entry := report[config.name]
		t.Logf("%-10s %15.1f %13.1f", config.name, entry[0], entry[1])
	}
}

// stdRound transfers total bytes one way over one fresh standard library
// connection and reports MB/s.
func stdRound(t *testing.T, useTLS, download bool, total int, serverConfig, clientConfig *tls.Config) float64 {
	t.Helper()
	payload := make([]byte, 64<<10)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if useTLS {
		listener = tls.NewListener(listener, serverConfig)
	}
	received := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			received <- err
			return
		}
		defer conn.Close()
		if download {
			received <- writeAll(conn, payload, total)
		} else {
			_, err := io.CopyN(io.Discard, conn, int64(total))
			received <- err
		}
	}()

	client, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	if useTLS {
		tlsClient := tls.Client(client, clientConfig)
		if err := tlsClient.Handshake(); err != nil {
			t.Fatal(err)
		}
		client = tlsClient
	}
	defer client.Close()

	started := time.Now()
	if download {
		if _, err := io.CopyN(io.Discard, client, int64(total)); err != nil {
			t.Fatal(err)
		}
	} else {
		if err := writeAll(client, payload, total); err != nil {
			t.Fatal(err)
		}
	}
	if err := <-received; err != nil {
		t.Fatal(err)
	}
	return mbps(total, time.Since(started))
}

// uioRound transfers total bytes one way over one fresh UIO stack — with a
// TLS layer when asked — and reports MB/s.
func uioRound(t *testing.T, useTLS, download bool, total int, serverConfig, clientConfig *tls.Config) float64 {
	t.Helper()
	payload := make([]byte, 64<<10)

	serverDone := make(chan struct{})
	var serverCount atomic.Int64
	var serverMiddleware []uio.Middleware
	if useTLS {
		serverMiddleware = append(serverMiddleware, Server(serverConfig))
	}
	serverConnOpened := make(chan uio.Conn, 1)
	server := uio.FuncHandler{
		Open: func(c uio.Conn) {
			select {
			case serverConnOpened <- c:
			default:
			}
		},
		Data: func(c uio.Conn) error {
			n := len(c.PeekChunk())
			_, _ = c.Discard(n)
			if serverCount.Add(int64(n)) >= int64(total) {
				select {
				case <-serverDone:
				default:
					close(serverDone)
				}
			}
			return nil
		},
		Close: func(uio.Conn, error) {},
	}
	serverEvents := &uio.Events{Pollers: 2, MaxBufferSize: benchReadBuffer(), WriteBufferedThreshold: benchWriteThreshold()}
	serverStarted := make(chan struct{})
	serverEvents.Hooks = uio.FuncHandler{Start: func(*uio.Events) { close(serverStarted) }}
	serverEvents.Handler = server
	serverEvents.Use(serverMiddleware...)
	serverServe := make(chan error, 1)
	go func() { serverServe <- serverEvents.Serve() }()
	defer func() { _ = serverEvents.Close(nil); <-serverServe }()
	select {
	case <-serverStarted:
	case <-time.After(testWait):
		t.Fatal("server Serve did not start")
	}
	listener, err := serverEvents.Listen("tcp://127.0.0.1:0", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()

	clientDone := make(chan struct{})
	var clientCount atomic.Int64
	var clientMiddleware []uio.Middleware
	if useTLS {
		clientMiddleware = append(clientMiddleware, Client(clientConfig))
	}
	client := uio.FuncHandler{
		Open: func(uio.Conn) {},
		Data: func(c uio.Conn) error {
			n := len(c.PeekChunk())
			_, _ = c.Discard(n)
			if clientCount.Add(int64(n)) >= int64(total) {
				select {
				case <-clientDone:
				default:
					close(clientDone)
				}
			}
			return nil
		},
		Close: func(uio.Conn, error) {},
	}
	clientEvents := &uio.Events{Pollers: 1, MaxBufferSize: benchReadBuffer(), WriteBufferedThreshold: benchWriteThreshold()}
	clientStarted := make(chan struct{})
	clientEvents.Hooks = uio.FuncHandler{Start: func(*uio.Events) { close(clientStarted) }}
	clientEvents.Handler = client
	clientEvents.Use(clientMiddleware...)
	clientServe := make(chan error, 1)
	go func() { clientServe <- clientEvents.Serve() }()
	defer func() { _ = clientEvents.Close(nil); <-clientServe }()
	select {
	case <-clientStarted:
	case <-time.After(testWait):
		t.Fatal("client Serve did not start")
	}
	ctx, cancel := context.WithTimeout(context.Background(), testWait)
	defer cancel()
	conn, err := clientEvents.DialContext(ctx, "tcp://"+listener.Addr().String(), nil)
	if err != nil {
		t.Fatal(err)
	}
	sender := <-serverConnOpened // the stack's handshake is over with the dial

	sendErr := make(chan error, 1)
	started := time.Now()
	if download {
		go func() { sendErr <- pump(sender, payload, total) }()
		select {
		case <-clientDone:
			rate := mbps(total, time.Since(started))
			if err := <-sendErr; err != nil {
				t.Fatal(err)
			}
			return rate
		case err := <-sendErr:
			if err != nil {
				t.Fatalf("server send failed: %v", err)
			}
			select { // sent: the last of it is still in flight
			case <-clientDone:
			case <-time.After(time.Minute):
				t.Fatal("download never finished after the send")
			}
			rate := mbps(total, time.Since(started))
			return rate
		case <-time.After(time.Minute):
			t.Fatal("download never finished")
		}
	}
	go func() { sendErr <- pump(conn, payload, total) }()
	select {
	case <-serverDone:
		rate := mbps(total, time.Since(started))
		if err := <-sendErr; err != nil {
			t.Fatal(err)
		}
		return rate
	case err := <-sendErr:
		if err != nil {
			t.Fatalf("client send failed: %v", err)
		}
		select { // sent: the last of it is still in flight
		case <-serverDone:
		case <-time.After(time.Minute):
			t.Fatal("upload never finished after the send")
		}
		return mbps(total, time.Since(started))
	case <-time.After(time.Minute):
		t.Fatal("upload never finished")
	}
	return 0
}

// writeAll writes payload until total bytes have gone out.
func writeAll(conn net.Conn, payload []byte, total int) error {
	sent := 0
	for sent < total {
		chunk := payload
		if remain := total - sent; remain < len(chunk) {
			chunk = chunk[:remain]
		}
		n, err := conn.Write(chunk)
		sent += n
		if err != nil {
			return err
		}
	}
	return nil
}

// pump writes payload until total bytes have gone out. The events run with
// no outbound limit, so the queue ahead of the socket is what a blocking
// write's kernel buffer would be; the wire is what paces it.
func pump(c uio.Conn, payload []byte, total int) error {
	sent := 0
	for sent < total {
		chunk := payload
		if remain := total - sent; remain < len(chunk) {
			chunk = chunk[:remain]
		}
		n, err := c.Write(chunk)
		sent += n
		if err != nil {
			return err
		}
	}
	return c.Flush()
}

func mbps(bytes int, elapsed time.Duration) float64 {
	return float64(bytes) / (1 << 20) / elapsed.Seconds()
}

// TestMemoryBaseline measures what one settled TLS connection holds over a
// raw UIO connection: N connections each way through the stack, the heap
// after a full garbage collection against the heap before, the same count
// through a stack of no layers, and the difference per connection per side.
//
// UTLS_BENCH=1 enables it, as the throughput comparison.
func TestMemoryBaseline(t *testing.T) {
	if os.Getenv("UTLS_BENCH") == "" {
		t.Skip("skipping: set UTLS_BENCH=1 to measure the memory baseline")
	}
	const conns = 300
	serverConfig, clientConfig := testConfigs(t)
	raw := memBaseline(t, conns, nil, nil)
	withTLS := memBaseline(t, conns, Server(serverConfig), Client(clientConfig))
	perConn := (withTLS - raw) / conns / 2
	t.Logf("%d connections: raw %d KiB, with TLS %d KiB, TLS adds ~%d KiB per connection per side",
		conns, raw>>10, withTLS>>10, perConn>>10)
}

// memBaseline returns the heap held by conns settled connections between two
// stacks, after the garbage collector has run twice.
func memBaseline(t *testing.T, conns int, serverMidd, clientMidd uio.Middleware) uint64 {
	t.Helper()
	opened := make(chan uio.Conn, conns)
	server := uio.FuncHandler{
		Open: func(c uio.Conn) {
			select {
			case opened <- c:
			default:
			}
		},
		Data:  func(uio.Conn) error { return nil },
		Close: func(uio.Conn, error) {},
	}
	var serverMiddleware []uio.Middleware
	if serverMidd != nil {
		serverMiddleware = append(serverMiddleware, serverMidd)
	}
	serverEvents := &uio.Events{Pollers: 2, MaxBufferSize: benchReadBuffer(), WriteBufferedThreshold: benchWriteThreshold()}
	started := make(chan struct{})
	serverEvents.Hooks = uio.FuncHandler{Start: func(*uio.Events) { close(started) }}
	serverEvents.Handler = server
	serverEvents.Use(serverMiddleware...)
	serverServe := make(chan error, 1)
	go func() { serverServe <- serverEvents.Serve() }()
	defer func() { _ = serverEvents.Close(nil); <-serverServe }()
	<-started
	listener, err := serverEvents.Listen("tcp://127.0.0.1:0", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()

	clientEvents := &uio.Events{Pollers: 2, MaxBufferSize: benchReadBuffer(), WriteBufferedThreshold: benchWriteThreshold()}
	clientStarted := make(chan struct{})
	clientEvents.Hooks = uio.FuncHandler{Start: func(*uio.Events) { close(clientStarted) }}
	var clientMiddleware []uio.Middleware
	if clientMidd != nil {
		clientMiddleware = append(clientMiddleware, clientMidd)
	}
	clientEvents.Handler = uio.FuncHandler{
		Open:  func(uio.Conn) {},
		Data:  func(uio.Conn) error { return nil },
		Close: func(uio.Conn, error) {},
	}
	clientEvents.Use(clientMiddleware...)
	clientServe := make(chan error, 1)
	go func() { clientServe <- clientEvents.Serve() }()
	defer func() { _ = clientEvents.Close(nil); <-clientServe }()
	<-clientStarted

	runtime.GC()
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)

	for range conns {
		if _, err := clientEvents.Dial("tcp://"+listener.Addr().String(), nil); err != nil {
			t.Fatal(err)
		}
	}
	for range conns {
		select {
		case <-opened:
		case <-time.After(time.Minute):
			t.Fatal("a connection never settled")
		}
	}
	time.Sleep(200 * time.Millisecond) // let the last rounds settle
	runtime.GC()
	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	return after.HeapInuse - before.HeapInuse
}

// benchReadBuffer is the socket read size the benchmark runs UIO with, from
// UTLS_BENCH_BUFFER: zero leaves UIO's own default (4 KiB) in place, so the
// same test measures both.
func benchReadBuffer() int {
	v := os.Getenv("UTLS_BENCH_BUFFER")
	n := 0
	for _, c := range v {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int(c-'0')
	}
	return n
}

// benchWriteThreshold is the write coalescing threshold from
// UTLS_BENCH_WRITEBUF; zero leaves UIO's default (no size-based buffering).
func benchWriteThreshold() int {
	v := os.Getenv("UTLS_BENCH_WRITEBUF")
	n := 0
	for _, c := range v {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int(c-'0')
	}
	return n
}
