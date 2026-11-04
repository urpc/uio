package uio

import (
	"bytes"
	"errors"
	"io"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urpc/uio/internal/readcontrol"
)

func readControlAwait[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
		return *new(T)
	}
}

func readControlQuiet[T any](t *testing.T, ch <-chan T, what string) {
	t.Helper()
	select {
	case value := <-ch:
		t.Fatalf("%s while reads were paused: %v", what, value)
	case <-time.After(30 * time.Millisecond):
	}
}

// readControlServe uses the same public endpoints for plain and layered
// connections, so these contracts also run against the stdio backend.
func readControlServe(t *testing.T, ev *Events, layers ...Layer) (net.Conn, Conn) {
	t.Helper()
	opened := make(chan Conn, 1)
	handler, ok := asFuncs(ev.Handler)
	if !ok || handler == nil {
		t.Fatal("readControlServe requires a FuncHandler")
	}
	onOpen := handler.Open
	handler.Open = func(c Conn) {
		if onOpen != nil {
			onOpen(c)
		}
		opened <- c
	}
	ev.Handler = handler
	started := make(chan struct{})
	ev.Hooks = FuncHandler{Start: func(*Events) { close(started) }}
	done := make(chan error, 1)
	go func() { done <- ev.Serve() }()
	t.Cleanup(func() {
		_ = ev.Close(nil)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("Serve did not return")
		}
	})
	readControlAwait(t, started, "server initialization")
	var opts []ListenOption
	if len(layers) != 0 {
		opts = append(opts, WithLayers(layers...))
	}
	listener, err := ev.Listen("tcp://127.0.0.1:0", opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	client, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client, readControlAwait(t, opened, "OnOpen")
}

func TestReadControlPauseInOnOpenKeepsTimersAndWritesAlive(t *testing.T) {
	paused := make(chan error, 1)
	reads := make(chan string, 8)
	timer := make(chan bool, 1)
	var readCalls atomic.Int64
	ev := &Events{Pollers: 1, Handler: FuncHandler{
		Open: func(c Conn) { paused <- c.PauseReads() },
		Read: func(c Conn) error {
			readCalls.Add(1)
			if n := c.InboundBuffered(); n != 0 {
				p := make([]byte, n)
				_, _ = c.Read(p)
				reads <- string(p)
			}
			return nil
		},
		Timer: func(c Conn) error {
			timer <- c.ReadsPaused()
			_, err := c.Write([]byte("timer"))
			return err
		},
	}}
	client, server := readControlServe(t, ev)
	if err := readControlAwait(t, paused, "OnOpen pause"); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Write([]byte("input")); err != nil {
		t.Fatal(err)
	}
	if err := server.Wake(); err != nil {
		t.Fatal(err)
	}
	if err := server.SetWakeDeadline(time.Now().Add(10 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if !readControlAwait(t, timer, "timer while paused") {
		t.Fatal("timer lost the application pause")
	}
	_ = client.SetReadDeadline(time.Now().Add(5 * time.Second))
	wire := make([]byte, len("timer"))
	if _, err := io.ReadFull(client, wire); err != nil || string(wire) != "timer" {
		t.Fatalf("write while paused = %q, %v", wire, err)
	}
	readControlQuiet(t, reads, "input delivery after Wake and timer")
	if got := readCalls.Load(); got != 0 {
		t.Fatalf("OnRead ran %d times while paused", got)
	}
	if err := server.ResumeReads(); err != nil {
		t.Fatal(err)
	}
	if got := readControlAwait(t, reads, "input after ResumeReads"); got != "input" {
		t.Fatalf("input = %q, want input", got)
	}
}

func TestReadControlOwnedPausesDoNotReleaseEachOther(t *testing.T) {
	reads := make(chan string, 8)
	ev := &Events{Pollers: 1, Handler: FuncHandler{Read: func(c Conn) error {
		if n := c.InboundBuffered(); n != 0 {
			p := make([]byte, n)
			_, _ = c.Read(p)
			reads <- string(p)
		}
		return nil
	}}}
	client, server := readControlServe(t, ev)
	a, b := readcontrol.NewPause(server), readcontrol.NewPause(server)
	if a == nil || b == nil || a.Paused() || b.Paused() {
		t.Fatal("new pause handles must exist and start unpaused")
	}
	for _, pause := range []func() error{server.PauseReads, server.PauseReads, a.Pause, a.Pause, b.Pause} {
		if err := pause(); err != nil {
			t.Fatal(err)
		}
	}
	if !server.ReadsPaused() || !a.Paused() || !b.Paused() {
		t.Fatal("the pause owners were not all recorded")
	}
	if _, err := client.Write([]byte("owned")); err != nil {
		t.Fatal(err)
	}
	for _, resume := range []func() error{server.ResumeReads, server.ResumeReads, a.Resume, a.Resume} {
		if err := resume(); err != nil {
			t.Fatal(err)
		}
	}
	if a.Paused() || !b.Paused() || !server.ReadsPaused() {
		t.Fatal("resuming the application or one owner released another owner")
	}
	if err := server.Wake(); err != nil {
		t.Fatal(err)
	}
	readControlQuiet(t, reads, "input delivery with an outstanding owner")
	if err := b.Resume(); err != nil {
		t.Fatal(err)
	}
	if server.ReadsPaused() || b.Paused() {
		t.Fatal("the last owner did not release the pause")
	}
	if got := readControlAwait(t, reads, "input after the final owner resumed"); got != "owned" {
		t.Fatalf("input = %q, want owned", got)
	}
}

// A capacity hold must stop its producer, not its consumer. In particular,
// ResumeReads must still invoke the consumer which can make that hold end.
func TestReadControlResumeDrainsAnInboundCapacityHold(t *testing.T) {
	const limit, total = 1024, 8 << 10
	var consume atomic.Bool
	var announced atomic.Bool
	var consumed atomic.Int64
	full := make(chan bool, 1)
	drained := make(chan []byte, 1)
	var received bytes.Buffer // used only by the serialized read callback
	var completed bool
	ev := &Events{Pollers: 1, MaxBufferSize: 512, MaxInboundBuffered: limit, Handler: FuncHandler{
		Read: func(c Conn) error {
			if !consume.Load() {
				if c.InboundBuffered() >= limit && announced.CompareAndSwap(false, true) {
					full <- c.ReadsPaused()
				}
				return nil
			}
			if _, err := c.WriteTo(&received); err != nil {
				return err
			}
			consumed.Store(int64(received.Len()))
			if received.Len() == total && !completed {
				completed = true
				drained <- append([]byte(nil), received.Bytes()...)
			}
			return nil
		},
	}}
	client, server := readControlServe(t, ev)
	want := bytes.Repeat([]byte("01234567"), total/8)
	if _, err := client.Write(want); err != nil {
		t.Fatal(err)
	}
	if readControlAwait(t, full, "the inbound capacity threshold") {
		t.Fatal("inbound capacity was reported as a consumer pause")
	}
	if err := server.PauseReads(); err != nil {
		t.Fatal(err)
	}
	consume.Store(true)
	if err := server.Wake(); err != nil {
		t.Fatal(err)
	}
	readControlQuiet(t, drained, "consumption before the manual pause resumed")
	if got := consumed.Load(); got != 0 {
		t.Fatalf("consumed %d bytes while manually paused", got)
	}
	if err := server.ResumeReads(); err != nil {
		t.Fatal(err)
	}
	if got := readControlAwait(t, drained, "capacity-held input after resume"); !bytes.Equal(got, want) {
		t.Fatalf("input changed: got %d bytes, want %d", len(got), len(want))
	}
}

// A consumer that left input unconsumed without pausing — held by capacity,
// including inside a layer stack — gets its next callback from Wake, which
// releases the producer's hold once it consumes; ResumeReads, with no pause
// to release, asks for none.
func TestReadControlWakeReevaluatesCapacityHeldInput(t *testing.T) {
	for _, layered := range []bool{false, true} {
		name := "plain"
		if layered {
			name = "layered"
		}
		t.Run(name, func(t *testing.T) {
			const limit, total = 128, 1024
			var consume atomic.Bool
			var announced atomic.Bool
			var received bytes.Buffer
			var complete bool
			full := make(chan bool, 1)
			drained := make(chan []byte, 1)
			events := &Events{Pollers: 1, MaxBufferSize: 64, MaxInboundBuffered: limit, Handler: FuncHandler{
				Read: func(c Conn) error {
					if !consume.Load() {
						if c.InboundBuffered() >= limit && announced.CompareAndSwap(false, true) {
							full <- c.ReadsPaused()
						}
						return nil
					}
					if _, err := c.WriteTo(&received); err != nil {
						return err
					}
					if received.Len() == total && !complete {
						complete = true
						drained <- append([]byte(nil), received.Bytes()...)
					}
					return nil
				},
			}}
			var layers []Layer
			if layered {
				layers = []Layer{PassThroughLayer}
			}
			peer, conn := readControlServe(t, events, layers...)
			want := bytes.Repeat([]byte("input"), total/5)
			want = append(want, bytes.Repeat([]byte("x"), total-len(want))...)
			if _, err := peer.Write(want); err != nil {
				t.Fatal(err)
			}
			if readControlAwait(t, full, "capacity hold without application pause") {
				t.Fatal("capacity hold was reported as an application pause")
			}
			consume.Store(true)
			if err := conn.ResumeReads(); err != nil {
				t.Fatal(err)
			}
			if err := conn.Wake(); err != nil {
				t.Fatal(err)
			}
			if got := readControlAwait(t, drained, "a wake after downstream drain"); !bytes.Equal(got, want) {
				t.Fatalf("received %d bytes, want %d unchanged", len(got), len(want))
			}
		})
	}
}

func TestReadControlPauseFromBytesReadPrecedesApplicationDelivery(t *testing.T) {
	paused := make(chan error, 1)
	reads := make(chan string, 8)
	var first atomic.Bool
	observer := FuncHandler{BytesRead: func(c Conn, n int) {
		if n > 0 && first.CompareAndSwap(false, true) {
			paused <- c.PauseReads()
		}
	}}
	ev := &Events{Pollers: 1, Observer: observer, Handler: FuncHandler{Read: func(c Conn) error {
		if n := c.InboundBuffered(); n != 0 {
			p := make([]byte, n)
			_, _ = c.Read(p)
			reads <- string(p)
		}
		return nil
	}}}
	client, server := readControlServe(t, ev)
	if _, err := client.Write([]byte("observed")); err != nil {
		t.Fatal(err)
	}
	if err := readControlAwait(t, paused, "the read observer's pause"); err != nil {
		t.Fatal(err)
	}
	if err := server.Wake(); err != nil {
		t.Fatal(err)
	}
	readControlQuiet(t, reads, "input delivery after the observer paused")
	if err := server.ResumeReads(); err != nil {
		t.Fatal(err)
	}
	if got := readControlAwait(t, reads, "observer-held input after resume"); got != "observed" {
		t.Fatalf("input = %q, want observed", got)
	}
}

func TestReadControlPausedConnectionsStillCloseAndExpire(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		name := "Close"
		if deadline {
			name = "ReadDeadline"
		}
		t.Run(name, func(t *testing.T) {
			paused := make(chan error, 1)
			closed := make(chan error, 1)
			ev := &Events{Pollers: 1, Handler: FuncHandler{
				Open:  func(c Conn) { paused <- c.PauseReads() },
				Close: func(_ Conn, err error) { closed <- err },
			}}
			_, server := readControlServe(t, ev)
			if err := readControlAwait(t, paused, "OnOpen pause"); err != nil {
				t.Fatal(err)
			}
			var err error
			if deadline {
				err = server.SetReadDeadline(time.Now().Add(30 * time.Millisecond))
			} else {
				err = server.Close()
			}
			if err != nil {
				t.Fatal(err)
			}
			err = readControlAwait(t, closed, "OnClose while paused")
			if deadline && !errors.Is(err, os.ErrDeadlineExceeded) {
				t.Fatalf("deadline close = %v, want deadline exceeded", err)
			}
			if err := server.ResumeReads(); !errors.Is(err, net.ErrClosed) {
				t.Fatalf("ResumeReads after OnClose = %v, want net.ErrClosed", err)
			}
		})
	}
}

// The last resume must not be lost if another owner pauses again or the
// reader enters its wait concurrently. No additional network event follows.
func TestReadControlConcurrentOwnersDoNotStrandInput(t *testing.T) {
	const total = 64 << 10
	want := bytes.Repeat([]byte("concurrency-test"), total/16)
	drained := make(chan []byte, 1)
	var received bytes.Buffer
	var completed bool
	ev := &Events{Pollers: 1, MaxBufferSize: 256, Handler: FuncHandler{Read: func(c Conn) error {
		if _, err := c.WriteTo(&received); err != nil {
			return err
		}
		if received.Len() == len(want) && !completed {
			completed = true
			drained <- append([]byte(nil), received.Bytes()...)
		}
		return nil
	}}}
	client, server := readControlServe(t, ev)
	if err := client.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	owners := []*readcontrol.Pause{readcontrol.NewPause(server), readcontrol.NewPause(server)}
	errs := make(chan error, len(owners)+1)
	var wg sync.WaitGroup
	for _, owner := range owners {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				if err := owner.Pause(); err != nil {
					errs <- err
					return
				}
				if err := owner.Resume(); err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, err := client.Write(want)
		errs <- err
	}()
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, owner := range owners {
		if err := owner.Resume(); err != nil {
			t.Fatal(err)
		}
	}
	if got := readControlAwait(t, drained, "input after concurrent owners resumed"); !bytes.Equal(got, want) {
		t.Fatalf("input changed: got %d bytes, want %d", len(got), len(want))
	}
}
