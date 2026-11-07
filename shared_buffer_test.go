package uio

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"
)

type xorTransformLayer struct{}

func (xorTransformLayer) Attach(LayerContext) LayerHandler { return xorTransformHandler{} }

type xorTransformHandler struct{ PassThrough }

func (xorTransformHandler) OnRead(ctx LayerContext) error {
	chunk := ctx.Lower().PeekChunk()
	if len(chunk) == 0 {
		if ctx.Lower().State()&StateReadEnded != 0 {
			ctx.Upper().End()
		}
		return nil
	}
	dst := ctx.Upper().Reserve(len(chunk))
	for i, b := range chunk {
		dst[i] = b ^ 0x5A
	}
	ctx.Upper().Commit(len(chunk))
	_, _ = ctx.Lower().Discard(len(chunk))
	return nil
}

func (xorTransformHandler) OnWrite(ctx LayerContext, p []byte) (int, error) {
	tmp := make([]byte, len(p))
	for i, b := range p {
		tmp[i] = b ^ 0x5A
	}
	return ctx.Lower().Write(tmp)
}

func TestWriteSharedFanoutAcrossPlainLayeredOverflowAndBlocking(t *testing.T) {
	// Nil and empty no-ops.
	if n, err := WriteShared(nil, nil); n != 0 || err != nil {
		t.Fatalf("nil WriteShared = %d, %v", n, err)
	}
	emptySB := NewSharedBuffer(nil)
	defer emptySB.Release()
	if n, err := WriteShared(nil, emptySB); n != 0 || err != nil {
		t.Fatalf("empty WriteShared = %d, %v", n, err)
	}

	for _, mode := range []struct {
		name   string
		layers []Layer
	}{
		{name: "plain"},
		{name: "passthrough", layers: []Layer{PassThroughLayer}},
		{name: "transform", layers: []Layer{xorTransformLayer{}}},
	} {
		t.Run(mode.name, func(t *testing.T) {
			const fanout = 16
			msg := bytes.Repeat([]byte("broadcast-frame-"), 256) // 4 KiB
			var mu sync.Mutex
			srvConns := make([]Conn, 0, fanout)
			allOpened := make(chan struct{})

			var plainAddr string
			started := make(chan struct{})
			srvEv := &Events{
				Pollers:             2,
				MaxOutboundBuffered: 8 << 10,
				Handler: FuncHandler{
					Open: func(c Conn) {
						mu.Lock()
						srvConns = append(srvConns, c)
						if len(srvConns) == fanout {
							close(allOpened)
						}
						mu.Unlock()
					},
				},
				Hooks: FuncHandler{
					Start: func(ev *Events) {
						var opts []ListenOption
						if len(mode.layers) > 0 {
							opts = append(opts, WithLayers(mode.layers...))
						}
						ln, err := ev.Listen("tcp://127.0.0.1:0", opts...)
						if err != nil {
							t.Error(err)
							return
						}
						plainAddr = "tcp://" + ln.Addr().String()
						close(started)
					},
				},
			}
			srvDone := make(chan error, 1)
			go func() { srvDone <- srvEv.Serve() }()
			t.Cleanup(func() {
				_ = srvEv.Close(nil)
				<-srvDone
			})
			<-started

			cliStarted := make(chan struct{})
			cliEv := &Events{
				Pollers: 2,
				Hooks:   FuncHandler{Start: func(*Events) { close(cliStarted) }},
			}
			cliDone := make(chan error, 1)
			go func() { cliDone <- cliEv.Serve() }()
			t.Cleanup(func() {
				_ = cliEv.Close(nil)
				<-cliDone
			})
			<-cliStarted

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			bcs := make([]*BlockingConn, fanout)
			for i := range fanout {
				var dialOpts []DialOption
				if len(mode.layers) > 0 {
					dialOpts = append(dialOpts, WithLayers(mode.layers...))
				}
				c, err := cliEv.DialContext(ctx, plainAddr, dialOpts...)
				if err != nil {
					t.Fatal(err)
				}
				bc, err := NewBlockingConn(c)
				if err != nil {
					t.Fatal(err)
				}
				bcs[i] = bc
			}
			select {
			case <-allOpened:
			case <-ctx.Done():
				t.Fatal("timed out waiting for server connections")
			}

			// Broadcast 1 SharedBuffer across all 16 connections + release immediately while in-flight!
			buf := AcquireBuffer(len(msg))
			_, _ = buf.Write(msg)
			sb := FreezeBuffer(buf)
			for _, sc := range srvConns {
				n, err := WriteShared(sc, sb)
				if err != nil || n != len(msg) {
					t.Fatalf("WriteShared = %d, %v", n, err)
				}
			}
			// Release BEFORE clients read from sockets to prove in-flight refcount safety!
			sb.Release()

			// Also verify ErrOutboundOverflow handling on WriteShared (> 8 KiB MaxOutboundBuffered):
			overSB := NewSharedBuffer(make([]byte, 16<<10))
			if _, err := WriteShared(srvConns[0], overSB); !errors.Is(err, ErrOutboundOverflow) {
				t.Fatalf("oversized WriteShared = %v, want ErrOutboundOverflow", err)
			}
			overSB.Release()

			// Read broadcast on all 16 BlockingConns and echo back via BlockingConn.WriteShared / WriteSharedContext!
			replySB := NewSharedBuffer([]byte("ack-from-blocking-conn"))
			defer replySB.Release()
			for i, bc := range bcs {
				got := make([]byte, len(msg))
				if _, err := io.ReadFull(bc, got); err != nil {
					t.Fatalf("client %d ReadFull = %v", i, err)
				}
				if !bytes.Equal(got, msg) {
					t.Fatalf("client %d payload mismatch", i)
				}
				if i%2 == 0 {
					if _, err := bc.WriteShared(replySB); err != nil {
						t.Fatal(err)
					}
				} else {
					if _, err := bc.WriteSharedContext(ctx, replySB); err != nil {
						t.Fatal(err)
					}
				}
				if _, err := bc.WriteShared(nil); err != nil {
					t.Fatal(err)
				}
				if _, err := bc.WriteShared(emptySB); err != nil {
					t.Fatal(err)
				}
				_ = bc.Close()
			}
		})
	}
}
