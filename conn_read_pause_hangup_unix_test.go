//go:build (linux || darwin || netbsd || freebsd || openbsd || dragonfly) && !stdio

package uio

import (
	"bytes"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

type pausedBottomInput struct {
	PassThrough
	down chan LayerDown
	err  chan error
}

func (h *pausedBottomInput) OnOpen(ctx LayerContext) {
	h.err <- ctx.Down().PauseReads()
	h.down <- ctx.Down()
	ctx.Up().Open()
}

// A reset is a transport failure, not an input-consumer decision. Neither an
// application pause nor an automatic capacity hold may wait for ResumeReads
// to report it. No server writes or deadlines are used to discover the reset.
func TestPausedNativeReadsReportResetWithoutResume(t *testing.T) {
	for _, mode := range []string{"pause-in-open", "pause-in-read", "capacity-hold", "bottom-layer"} {
		t.Run(mode, func(t *testing.T) {
			const limit = 64
			ready := make(chan error, 1)
			closed := make(chan error, 1)
			var once sync.Once
			releaseRead := make(chan struct{})
			var release sync.Once
			defer release.Do(func() { close(releaseRead) })
			handler := FuncHandler{
				Open: func(c Conn) {
					if mode == "pause-in-open" {
						ready <- c.PauseReads()
					}
				},
				Read: func(c Conn) error {
					if mode == "pause-in-read" && c.InboundBuffered() != 0 {
						once.Do(func() {
							ready <- c.PauseReads()
							<-releaseRead // the reset arrives during this callback
						})
					}
					if mode == "capacity-hold" && c.InboundBuffered() >= limit {
						once.Do(func() { ready <- nil })
					}
					return nil
				},
				Close: func(_ Conn, err error) { closed <- err },
			}
			var layers []Layer
			if mode == "bottom-layer" {
				bottom := &pausedBottomInput{down: make(chan LayerDown, 1), err: ready}
				layers = []Layer{LayerFunc(func(LayerContext) LayerHandler { return bottom })}
			}
			peer, server := readControlServe(t, &Events{
				Pollers: 1, MaxBufferSize: limit, MaxInboundBuffered: limit, Handler: handler,
			}, layers...)
			if mode == "pause-in-read" || mode == "capacity-hold" {
				count := limit
				if mode == "pause-in-read" {
					count = 8 // isolate application pause from the capacity hold
				}
				if _, err := peer.Write(bytes.Repeat([]byte("x"), count)); err != nil {
					t.Fatal(err)
				}
			}
			if err := readControlAwait(t, ready, "the read pause before reset"); err != nil {
				t.Fatal(err)
			}
			if mode == "capacity-hold" {
				conn := server.(*fdConn)
				waitFor(t, time.Second, conn.readHeld)
				if !conn.readHeld() {
					t.Fatal("the producer did not enter its capacity hold before reset")
				}
			}
			tcp := peer.(*net.TCPConn)
			if err := tcp.SetLinger(0); err != nil {
				t.Fatal(err)
			}
			if err := tcp.Close(); err != nil {
				t.Fatal(err)
			}
			if mode == "pause-in-read" {
				conn := server.(*fdConn)
				waitFor(t, time.Second, func() bool { return conn.taskState.Load()&ioEventHangup != 0 })
				if conn.taskState.Load()&ioEventHangup == 0 {
					t.Fatal("reset was not queued while the read callback was paused")
				}
				release.Do(func() { close(releaseRead) })
			}
			select {
			case err := <-closed:
				if !errors.Is(err, syscall.ECONNRESET) {
					t.Fatalf("OnClose = %v, want connection reset", err)
				}
			case <-time.After(time.Second):
				t.Fatal("reset remained hidden behind the read pause without ResumeReads")
			}
		})
	}
}

// The hangup notification for FIN must not probe by consuming paused bytes.
// Unlike a reset, an orderly end keeps both kernel-held and capacity-held
// input available until ordinary ResumeReads permits its delivery.
func TestPausedNativeReadsKeepFINAndInputUntilResume(t *testing.T) {
	for _, mode := range []string{"application-pause", "capacity-hold", "bottom-layer"} {
		t.Run(mode, func(t *testing.T) {
			const limit = 64
			want := bytes.Repeat([]byte("f"), limit)
			if mode != "capacity-hold" {
				want = want[:limit-1] // FIN must survive a short socket read, too
			}
			ready := make(chan error, 1)
			closed := make(chan error, 1)
			data := make(chan []byte, 1)
			var consume atomic.Bool
			var announced, delivered sync.Once
			var received bytes.Buffer
			handler := FuncHandler{
				Open: func(c Conn) {
					if mode == "application-pause" {
						ready <- c.PauseReads()
					}
				},
				Read: func(c Conn) error {
					if !consume.Load() {
						if mode == "capacity-hold" && c.InboundBuffered() >= limit {
							announced.Do(func() { ready <- nil })
						}
						return nil
					}
					if _, err := c.WriteTo(&received); err != nil {
						return err
					}
					if received.Len() == len(want) {
						delivered.Do(func() { data <- append([]byte(nil), received.Bytes()...) })
					}
					return nil
				},
				Close: func(_ Conn, err error) { closed <- err },
			}
			var layers []Layer
			var bottom *pausedBottomInput
			if mode == "bottom-layer" {
				bottom = &pausedBottomInput{down: make(chan LayerDown, 1), err: ready}
				layers = []Layer{LayerFunc(func(LayerContext) LayerHandler { return bottom })}
			}
			peer, server := readControlServe(t, &Events{
				Pollers: 1, MaxBufferSize: limit, MaxInboundBuffered: limit, Handler: handler,
			}, layers...)
			if _, err := peer.Write(want); err != nil {
				t.Fatal(err)
			}
			if err := readControlAwait(t, ready, "the read pause before FIN"); err != nil {
				t.Fatal(err)
			}
			if mode == "capacity-hold" {
				conn := server.(*fdConn)
				waitFor(t, time.Second, conn.readHeld)
				if !conn.readHeld() {
					t.Fatal("the producer did not enter its capacity hold before FIN")
				}
			}
			if err := peer.(*net.TCPConn).CloseWrite(); err != nil {
				t.Fatal(err)
			}
			readControlQuiet(t, closed, "orderly close before paused input resumed")
			consume.Store(true)
			switch {
			case bottom != nil:
				if err := readControlAwait(t, bottom.down, "the bottom input boundary").ResumeReads(); err != nil {
					t.Fatal(err)
				}
			case mode == "capacity-hold":
				// No pause to release: the held input's next callback is a Wake's.
				if err := server.Wake(); err != nil {
					t.Fatal(err)
				}
			default:
				if err := server.ResumeReads(); err != nil {
					t.Fatal(err)
				}
			}
			if got := readControlAwait(t, data, "paused input before orderly EOF"); !bytes.Equal(got, want) {
				t.Fatalf("received %d bytes, want %d unchanged", len(got), len(want))
			}
			if err := readControlAwait(t, closed, "orderly EOF after resumed input"); !errors.Is(err, io.EOF) {
				t.Fatalf("OnClose = %v, want EOF", err)
			}
		})
	}
}
