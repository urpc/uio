package uws

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/urpc/uio/uws/internal/compress"
	"github.com/urpc/uio/uws/internal/frame"
)

func assertPreparedPanics(t *testing.T, name string, fn func()) {
	t.Helper()
	defer func() {
		if recover() == nil {
			t.Fatalf("%s did not panic", name)
		}
	}()
	fn()
}

type broadcastHubHandler struct {
	mu    sync.Mutex
	conns []*Conn
	ready chan struct{}
	want  int
	onMsg func(*Conn, Message)
}

func (h *broadcastHubHandler) OnOpen(c *Conn) {
	h.mu.Lock()
	h.conns = append(h.conns, c)
	if h.want > 0 && len(h.conns) == h.want {
		select {
		case <-h.ready:
		default:
			close(h.ready)
		}
	}
	h.mu.Unlock()
}

func (h *broadcastHubHandler) OnMessage(c *Conn, m Message) {
	if h.onMsg != nil {
		h.onMsg(c, m)
	}
}

func (h *broadcastHubHandler) OnClose(*Conn, CloseEvent) {}

func startPreparedTestServer(t *testing.T, opts Options) (*Server, string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	srv := NewServer(opts)
	done := make(chan error, 1)
	go func() { done <- srv.Serve(addr) }()
	t.Cleanup(func() {
		_ = srv.Close(nil)
		<-done
	})
	select {
	case <-srv.Ready():
	case <-time.After(3 * time.Second):
		t.Fatal("server not ready")
	}
	return srv, addr
}

func TestPreparedMessageBroadcastMixedCompressionFallbackAndOrdering(t *testing.T) {
	// 1. Validation & nil/misuse guards.
	var nilPM *PreparedMessage
	if nilPM.Type() != 0 || nilPM.Len() != 0 || nilPM.Payload() != nil {
		t.Fatal("nil PreparedMessage accessors non-zero")
	}
	nilPM.Release()

	if _, err := NewPreparedMessage(MessageType(99), []byte("bad")); !errors.Is(err, frame.ErrProtocol) {
		t.Fatalf("invalid MessageType = %v, want ErrProtocol", err)
	}

	var unopened Conn
	var nilBC BlockingConn
	validPM, err := NewPreparedMessage(TextMessage, []byte("hi"))
	if err != nil {
		t.Fatal(err)
	}
	if err := unopened.SendPrepared(nil); !errors.Is(err, frame.ErrProtocol) {
		t.Fatalf("nil SendPrepared = %v, want ErrProtocol", err)
	}
	if err := nilBC.WritePrepared(context.Background(), nil); !errors.Is(err, frame.ErrProtocol) {
		t.Fatalf("nil WritePrepared = %v, want ErrProtocol", err)
	}
	if err := unopened.SendPrepared(validPM); !errors.Is(err, ErrNotReady) {
		t.Fatalf("unopened SendPrepared = %v, want ErrNotReady", err)
	}
	var closedConn Conn
	closedConn.opened.Store(true)
	closedConn.closed.Store(true)
	if err := closedConn.SendPrepared(validPM); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed SendPrepared = %v, want ErrClosed", err)
	}
	validPM.Release()
	assertPreparedPanics(t, "double Release", func() { validPM.Release() })
	assertPreparedPanics(t, "SendPrepared after Release", func() { _ = unopened.SendPrepared(validPM) })
	assertPreparedPanics(t, "WritePrepared after Release", func() { _ = nilBC.WritePrepared(context.Background(), validPM) })

	// 2. Stateless compression + mixed uncompressed/compressed clients + caller slice mutation + FIFO batch ordering!
	orderedPM, err := NewPreparedMessage(TextMessage, []byte("middle-prepared"))
	if err != nil {
		t.Fatal(err)
	}
	defer orderedPM.Release()

	hub := &broadcastHubHandler{
		ready: make(chan struct{}),
		want:  4,
		onMsg: func(c *Conn, m Message) {
			if string(m.Payload) == "trigger-order" {
				// Inside a multi-frame batching round, batch "first", then SendPrepared, then "third"!
				_ = c.SendText("first")
				_ = c.SendPrepared(orderedPM)
				_ = c.SendText("third")
			} else {
				_ = c.Send(m.Type, m.Payload)
			}
		},
	}
	_, addr := startPreparedTestServer(t, Options{
		Pollers:           2,
		EnableCompression: true,
		HeartbeatInterval: 200 * time.Millisecond,
		HeartbeatTimeout:  time.Second,
		Handler:           hub,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	compDialer := NewDialer(Options{EnableCompression: true})
	t.Cleanup(func() { _ = compDialer.Close(nil) })
	plainDialer := NewDialer(Options{EnableCompression: false})
	t.Cleanup(func() { _ = plainDialer.Close(nil) })

	clients := make([]*Client, 4)
	for i := range clients {
		d := compDialer
		if i%2 == 1 {
			d = plainDialer
		}
		cli, derr := d.DialClient(ctx, "ws://"+addr+"/")
		if derr != nil {
			t.Fatal(derr)
		}
		t.Cleanup(func() { _ = cli.Close(1000, "") })
		clients[i] = cli
	}
	<-hub.ready

	// Verify caller slice mutation immediately after NewPreparedMessage is isolated,
	// and test both compressible (4 KiB repetitive) and incompressible (3 B) payloads,
	// plus >64KiB 64-bit extended length payload!
	for _, rawPayload := range [][]byte{
		[]byte("tiny"),
		bytes.Repeat([]byte("broadcast- compressible-json-block-"), 120),
		bytes.Repeat([]byte("L"), 70000),
	} {
		callerBuf := append([]byte(nil), rawPayload...)
		pm, perr := NewPreparedMessage(BinaryMessage, callerBuf)
		if perr != nil {
			t.Fatal(perr)
		}
		if pm.Type() != BinaryMessage || pm.Len() != len(rawPayload) || !bytes.Equal(pm.Payload(), rawPayload) {
			t.Fatalf("PreparedMessage metadata = (%v, %d)", pm.Type(), pm.Len())
		}
		// Corrupt caller's buffer immediately!
		if len(callerBuf) > 0 {
			callerBuf[0] ^= 0xff
		}

		hub.mu.Lock()
		conns := append([]*Conn(nil), hub.conns...)
		hub.mu.Unlock()
		for _, sc := range conns {
			if serr := sc.SendPrepared(pm); serr != nil {
				t.Fatalf("SendPrepared = %v", serr)
			}
		}
		// Release immediately before clients read!
		pm.Release()

		for i, cli := range clients {
			msg, rerr := cli.ReadMessage(ctx)
			if rerr != nil {
				t.Fatalf("client %d ReadMessage = %v", i, rerr)
			}
			if msg.Type != BinaryMessage || !bytes.Equal(msg.Payload, rawPayload) {
				t.Fatalf("client %d payload mismatch (len=%d want=%d)", i, len(msg.Payload), len(rawPayload))
			}
		}
	}

	// Verify client-side fallback (masking) via BlockingConn.WritePrepared and Conn.SendPrepared,
	// AND multi-frame in-turn batch FIFO ordering!
	trigger1, _ := NewPreparedMessage(TextMessage, []byte("ping-1"))
	defer trigger1.Release()
	trigger2, _ := NewPreparedMessage(TextMessage, []byte("trigger-order"))
	defer trigger2.Release()

	// Send two frames back-to-back from client[1] (plain) so server enters roundBatching!
	if err := clients[1].Conn.SendPrepared(trigger1); err != nil {
		t.Fatal(err)
	}
	if err := clients[1].WritePrepared(ctx, trigger2); err != nil {
		t.Fatal(err)
	}
	var seq []string
	for range 4 {
		m, rerr := clients[1].ReadMessage(ctx)
		if rerr != nil {
			t.Fatal(rerr)
		}
		seq = append(seq, string(m.Payload))
	}
	wantSeq := []string{"ping-1", "first", "middle-prepared", "third"}
	for i := range wantSeq {
		if seq[i] != wantSeq[i] {
			t.Fatalf("batch FIFO order = %v, want %v", seq, wantSeq)
		}
	}
}

func TestPreparedMessageStatefulContextFragmentationAndLimits(t *testing.T) {
	// Server with stateful sliding-window context (KeepCompressionContext: true),
	// small MaxFramePayload (128B) requiring fragmentation, MaxMessageSize (1024B),
	// and tight MaxOutboundBuffered (600B).
	hub := &broadcastHubHandler{ready: make(chan struct{}), want: 1}
	srv, addr := startPreparedTestServer(t, Options{
		Pollers:                1,
		EnableCompression:      true,
		KeepCompressionContext: true,
		MaxFramePayload:        128,
		MaxMessageSize:         1024,
		MaxOutboundBuffered:    600,
		Handler:                hub,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	dialer := NewDialer(Options{
		EnableCompression:      true,
		KeepCompressionContext: true,
		MaxFramePayload:        128,
		MaxMessageSize:         1024,
	})
	t.Cleanup(func() { _ = dialer.Close(nil) })

	cli, err := dialer.DialClient(ctx, "ws://"+addr+"/")
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close(1000, "")
	<-hub.ready

	sc := hub.conns[0]

	// 1. Stateful context + fragmentation fallback across multiple messages (verifies dictionary stays synchronized!).
	body := bytes.Repeat([]byte("sliding-window-fragmented-payload-"), 12) // 408 B > 128 B MaxFramePayload
	pm, err := NewPreparedMessage(BinaryMessage, body)
	if err != nil {
		t.Fatal(err)
	}
	defer pm.Release()

	for round := range 3 {
		if err := sc.SendPrepared(pm); err != nil {
			t.Fatalf("round %d SendPrepared = %v", round, err)
		}
		if err := sc.SendBinary(body); err != nil {
			t.Fatalf("round %d SendBinary = %v", round, err)
		}
		for range 2 {
			got, rerr := cli.ReadMessage(ctx)
			if rerr != nil {
				t.Fatalf("round %d ReadMessage = %v", round, rerr)
			}
			if !bytes.Equal(got.Payload, body) {
				t.Fatalf("round %d dictionary/fragmentation mismatch", round)
			}
		}
	}

	// 2. Oversized message (> 1024B MaxMessageSize) returns ErrMessageTooBig.
	tooBig, err := NewPreparedMessage(BinaryMessage, make([]byte, 1500))
	if err != nil {
		t.Fatal(err)
	}
	defer tooBig.Release()
	if err := sc.SendPrepared(tooBig); !errors.Is(err, frame.ErrMessageTooBig) {
		t.Fatalf("oversized SendPrepared = %v, want ErrMessageTooBig", err)
	}

	// 3. Backpressure rejection increments BackpressureDrops on a plain tight-outbound server.
	tightHub := &broadcastHubHandler{ready: make(chan struct{}), want: 1}
	tightSrv, tightAddr := startPreparedTestServer(t, Options{
		Pollers:             1,
		MaxOutboundBuffered: 512,
		Handler:             tightHub,
	})
	plainDialer := NewDialer(Options{})
	t.Cleanup(func() { _ = plainDialer.Close(nil) })
	tightCli, err := plainDialer.DialClient(ctx, "ws://"+tightAddr+"/")
	if err != nil {
		t.Fatal(err)
	}
	defer tightCli.Close(1000, "")
	<-tightHub.ready

	overOutPM, _ := NewPreparedMessage(BinaryMessage, make([]byte, 800))
	defer overOutPM.Release()
	if err := tightHub.conns[0].SendPrepared(overOutPM); !errors.Is(err, ErrBackpressure) {
		t.Fatalf("SendPrepared past MaxOutboundBuffered = %v, want ErrBackpressure", err)
	}
	if tightSrv.Stats().BackpressureDrops == 0 {
		t.Fatalf("BackpressureDrops not recorded: %+v", tightSrv.Stats())
	}
	// 4. Plain and stateless-compressed MaxFramePayload fragmentation fallback.
	fragOpts, _ := (Options{MaxFramePayload: 64}).resolve()
	fragConn := &Conn{
		raw:    &echoPathConn{},
		config: newServerConnConfig(fragOpts, nil, nil),
	}
	fragConn.opened.Store(true)
	fragPM, _ := NewPreparedMessage(BinaryMessage, bytes.Repeat([]byte("F"), 200))
	defer fragPM.Release()
	if err := fragConn.SendPrepared(fragPM); err != nil {
		t.Fatalf("plain fragmented SendPrepared = %v", err)
	}

	compFragOpts, _ := (Options{EnableCompression: true, MaxFramePayload: 16}).resolve()
	compFragCfg := newServerConnConfig(compFragOpts, nil, nil)
	compFragConn := &Conn{
		raw:    &echoPathConn{},
		config: compFragCfg,
		compression: &compressionState{
			encoder: compress.NewEncoder(compFragCfg.compressionLevel, true),
			decoder: compress.NewDecoder(true),
		},
	}
	compFragConn.opened.Store(true)
	incompPM, _ := NewPreparedMessage(BinaryMessage, []byte("0123456789abcdefghij0123456789"))
	defer incompPM.Release()
	if err := compFragConn.SendPrepared(incompPM); err != nil {
		t.Fatalf("stateless-compressed fragmented SendPrepared = %v", err)
	}
	_ = srv
}

func TestSendPreparedZeroAllocOnServerConn(t *testing.T) {
	pm, err := NewPreparedMessage(BinaryMessage, bytes.Repeat([]byte("broadcast-payload-"), 128))
	if err != nil {
		t.Fatal(err)
	}
	defer pm.Release()

	opts, _ := (Options{}).resolve()
	plainConn := &Conn{
		raw:    &echoPathConn{},
		config: newServerConnConfig(opts, nil, nil),
	}
	plainConn.opened.Store(true)
	if err := plainConn.SendPrepared(pm); err != nil {
		t.Fatal(err)
	}
	if allocs := testing.AllocsPerRun(500, func() {
		if serr := plainConn.SendPrepared(pm); serr != nil {
			t.Fatal(serr)
		}
	}); allocs != 0 {
		t.Fatalf("plain Conn.SendPrepared allocs = %v, want 0", allocs)
	}

	compOpts, _ := (Options{EnableCompression: true}).resolve()
	compCfg := newServerConnConfig(compOpts, nil, nil)
	compConn := &Conn{
		raw:    &echoPathConn{},
		config: compCfg,
		compression: &compressionState{
			encoder: compress.NewEncoder(compCfg.compressionLevel, true),
			decoder: compress.NewDecoder(true),
		},
	}
	compConn.opened.Store(true)
	// First call triggers lazy compression once; subsequent sends must be 0 allocs!
	if err := compConn.SendPrepared(pm); err != nil {
		t.Fatal(err)
	}
	if allocs := testing.AllocsPerRun(500, func() {
		if serr := compConn.SendPrepared(pm); serr != nil {
			t.Fatal(serr)
		}
	}); allocs != 0 {
		t.Fatalf("stateless-compressed Conn.SendPrepared allocs = %v, want 0", allocs)
	}
}

func BenchmarkBroadcastPreparedVsSendBinary(b *testing.B) {
	const connsCount = 256
	for _, tc := range []struct {
		name     string
		size     int
		compress bool
	}{
		{name: "Plain_2KiB", size: 2 << 10, compress: false},
		{name: "Plain_16KiB", size: 16 << 10, compress: false},
		{name: "Compress_4KiB", size: 4 << 10, compress: true},
	} {
		payload := bytes.Repeat([]byte(`{"type":"BroadcastNotify","zone":1,"data":"reward-event"}`), tc.size/57+1)[:tc.size]
		opts, _ := (Options{EnableCompression: tc.compress}).resolve()
		cfg := newServerConnConfig(opts, nil, nil)
		conns := make([]*Conn, connsCount)
		for i := range conns {
			c := &Conn{raw: &echoPathConn{}, config: cfg}
			if tc.compress {
				c.compression = &compressionState{
					encoder: compress.NewEncoder(cfg.compressionLevel, true),
					decoder: compress.NewDecoder(true),
				}
			}
			c.opened.Store(true)
			conns[i] = c
		}

		b.Run(fmt.Sprintf("SendBinary_%s_%dConns", tc.name, connsCount), func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(tc.size * connsCount))
			b.ResetTimer()
			for range b.N {
				for _, c := range conns {
					_ = c.SendBinary(payload)
				}
			}
		})

		b.Run(fmt.Sprintf("SendPrepared_%s_%dConns", tc.name, connsCount), func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(tc.size * connsCount))
			b.ResetTimer()
			for range b.N {
				pm, _ := NewPreparedMessage(BinaryMessage, payload)
				for _, c := range conns {
					_ = c.SendPrepared(pm)
				}
				pm.Release()
			}
		})
	}
}
