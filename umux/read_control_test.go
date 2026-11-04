package umux

import (
	"context"
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urpc/uio"
	"github.com/urpc/uio/internal/readcontrol"
)

func TestStreamReadPauseKeepsSiblingAndControlWorkAlive(t *testing.T) {
	type openResult struct {
		stream *Stream
		err    error
	}
	opened := make(chan openResult, 4)
	timers := make(chan uint32, 4)
	closed := make(chan uint32, 4)
	var first atomic.Uint32
	var pausedReads atomic.Int64
	handler := uio.FuncHandler{
		Open: func(c uio.Conn) {
			st := c.(*Stream)
			var err error
			if first.CompareAndSwap(0, st.ID()) {
				err = st.PauseReads()
			}
			opened <- openResult{st, err}
		},
		Read: func(c uio.Conn) error {
			if c.(*Stream).ID() == first.Load() {
				pausedReads.Add(1)
			}
			_, err := c.WriteTo(c)
			return err
		},
		Timer: func(c uio.Conn) error { timers <- c.(*Stream).ID(); return nil },
		Close: func(c uio.Conn, _ error) { closed <- c.(*Stream).ID() },
	}
	session, _ := startPair(t, Config{Handler: handler, KeepAlive: -1}, Config{KeepAlive: -1})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	a, err := session.Open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var remote *Stream
	select {
	case result := <-opened:
		if result.err != nil {
			t.Fatal(result.err)
		}
		remote = result.stream
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	b, err := session.Open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-opened:
		if result.err != nil || result.stream.ReadsPaused() {
			t.Fatalf("sibling inherited pause: %v", result.err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	writeAll(t, a, []byte("held"))
	writeAll(t, b, []byte("live"))
	if got := string(readFull(t, b, 4)); got != "live" {
		t.Fatalf("sibling echo = %q", got)
	}
	for remote.InboundBuffered() != 4 {
		select {
		case <-ctx.Done():
			t.Fatal("paused stream did not retain its input")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	if err := remote.Wake(); err != nil {
		t.Fatal(err)
	}
	if err := remote.SetWakeDeadline(time.Now().Add(10 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	select {
	case id := <-timers:
		if id != remote.ID() {
			t.Fatalf("timer fired on stream %d", id)
		}
	case <-ctx.Done():
		t.Fatal("paused stream's timer did not run")
	}
	if got := pausedReads.Load(); got != 0 {
		t.Fatalf("paused stream received %d OnRead callbacks", got)
	}
	// A wrapper embedding a Stream must retain access to that same internal
	// boundary without adding an owner factory to the public Conn interface.
	owner := readcontrol.NewPause(struct{ *Stream }{remote})
	if owner == nil {
		t.Fatal("wrapped stream lost its internal read boundary")
	}
	if err := owner.Pause(); err != nil {
		t.Fatal(err)
	}
	if err := remote.ResumeReads(); err != nil {
		t.Fatal(err)
	}
	if !remote.ReadsPaused() {
		t.Fatal("application resume released a separate owner")
	}
	if err := owner.Resume(); err != nil {
		t.Fatal(err)
	}
	if got := string(readFull(t, a, 4)); got != "held" {
		t.Fatalf("retained echo = %q", got)
	}
	if err := remote.PauseReads(); err != nil {
		t.Fatal(err)
	}
	if err := remote.CloseWith(errors.New("test reset while paused")); err != nil {
		t.Fatal(err)
	}
	select {
	case id := <-closed:
		if id != remote.ID() {
			t.Fatalf("closed unexpected stream %d", id)
		}
	case <-ctx.Done():
		t.Fatal("paused stream did not close")
	}
	if err := remote.ResumeReads(); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("resume after close = %v", err)
	}
}

// Input a callback left unconsumed without pausing gets its next callback
// from Wake, every time; ResumeReads without a pause to release asks for none.
func TestStreamWakeProcessesRetainedInputEveryTime(t *testing.T) {
	opened := make(chan *Stream, 1)
	held := make(chan struct{}, 1)
	var consume atomic.Bool
	handler := uio.FuncHandler{
		Open: func(c uio.Conn) { opened <- c.(*Stream) },
		Read: func(c uio.Conn) error {
			if !consume.Load() {
				if c.InboundBuffered() != 0 {
					select {
					case held <- struct{}{}:
					default:
					}
				}
				return nil
			}
			_, err := c.WriteTo(c)
			return err
		},
	}
	session, _ := startPair(t, Config{Handler: handler, KeepAlive: -1}, Config{KeepAlive: -1})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	local, err := session.Open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var remote *Stream
	select {
	case remote = <-opened:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	for _, payload := range []string{"first", "second"} {
		consume.Store(false)
		writeAll(t, local, []byte(payload))
		select {
		case <-held:
		case <-ctx.Done():
			t.Fatal("the read callback did not retain its input")
		}
		if remote.ReadsPaused() {
			t.Fatal("fixture unexpectedly acquired a read pause")
		}
		consume.Store(true)
		if err := remote.ResumeReads(); err != nil {
			t.Fatal(err)
		}
		if err := remote.Wake(); err != nil {
			t.Fatal(err)
		}
		if got := string(readFull(t, local, len(payload))); got != payload {
			t.Fatalf("wake echoed %q, want %q", got, payload)
		}
	}
}
