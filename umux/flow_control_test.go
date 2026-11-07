package umux

import (
	"bytes"
	"context"
	"errors"
	"github.com/urpc/uio"
	"github.com/urpc/uio/internal/readcontrol"
	"github.com/urpc/uio/ubridge"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

var streamEcho = uio.FuncHandler{Read: func(c uio.Conn) error {
	buf := make([]byte, 8<<10)
	for {
		n, err := c.Read(buf)
		if n > 0 {
			if _, werr := c.Write(buf[:n]); werr != nil {
				return werr
			}
			continue
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		break
	}
	if c.State()&uio.StateReadEnded != 0 {
		return c.CloseWrite()
	}
	return nil
}}

func serveWait(ev *uio.Events, addrs ...string) <-chan struct{} {
	started := make(chan struct{})
	hooks := ev.Hooks
	ev.Hooks = uio.FuncHandler{Start: func(e *uio.Events) {
		if hooks != nil {
			hooks.OnStart(e)
		}
		close(started)
	}, Stop: func(e *uio.Events) {
		if hooks != nil {
			hooks.OnStop(e)
		}
	}}
	go func() {
		_ = ev.Serve(addrs...)
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		panic("events did not start")
	}
	return started
}

func startTCPEcho(t *testing.T) string {
	t.Helper()
	ev := &uio.Events{Handler: streamEcho, AllowHalfOpen: true}
	addr := freeAddr(t)
	stopped := make(chan struct{})
	started := serveWaitWithStop(ev, &stopped, "tcp://"+addr)
	_ = started
	t.Cleanup(func() {
		_ = ev.Close(nil)
		<-stopped
	})
	return addr
}

func serveWaitWithStop(ev *uio.Events, stopped *chan struct{}, addrs ...string) <-chan struct{} {
	started := make(chan struct{})
	hooks := ev.Hooks
	ev.Hooks = uio.FuncHandler{Start: func(e *uio.Events) {
		if hooks != nil {
			hooks.OnStart(e)
		}
		close(started)
	}, Stop: func(e *uio.Events) {
		if hooks != nil {
			hooks.OnStop(e)
		}
	}}
	go func() {
		defer close(*stopped)
		_ = ev.Serve(addrs...)
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		panic("events did not start")
	}
	return started
}

func TestBridgeAgentHop(t *testing.T) {
	target := startTCPEcho(t)
	br := &ubridge.Bridge{Upstream: target}
	targets := br.Events()
	serveWait(targets)
	t.Cleanup(func() {
		_ = targets.Close(nil)
	})
	srvAddr := freeAddr(t)
	srv := NewServer(Config{Handler: br, Observer: br})
	go func() {
		_ = srv.Serve(srvAddr)
	}()
	t.Cleanup(func() {
		_ = srv.Close(nil)
	})
	cli := NewClient(Config{})
	t.Cleanup(func() {
		_ = cli.Close(nil)
	})
	var sess *Session
	waitFor(t, "client session", func() bool {
		s, err := cli.Dial(context.Background(), "tcp://"+srvAddr)
		sess = s
		return err == nil
	})
	st, err := sess.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("agent-hop")
	if _, err := st.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := st.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 64)
	var got []byte
	waitFor(t, "echo", func() bool {
		n, rerr := st.Read(buf)
		if n > 0 {
			got = append(got, buf[:n]...)
			return len(got) >= len(payload)
		}
		if rerr != nil && !errors.Is(rerr, io.EOF) {
			t.Fatalf("read: %v", rerr)
		}
		return false
	})
	if !bytes.Equal(got, payload) {
		t.Fatalf("echo = %q", got)
	}
}

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
	handler := uio.FuncHandler{Open: func(c uio.Conn) {
		st := c.(*Stream)
		var err error
		if first.CompareAndSwap(0, st.ID()) {
			err = st.PauseRead()
		}
		opened <- openResult{st, err}
	}, Read: func(c uio.Conn) error {
		if c.(*Stream).ID() == first.Load() {
			pausedReads.Add(1)
		}
		_, err := c.WriteTo(c)
		return err
	}, Timer: func(c uio.Conn) error {
		timers <- c.(*Stream).ID()
		return nil
	}, Close: func(c uio.Conn, _ error) {
		closed <- c.(*Stream).ID()
	}}
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
		if result.err != nil || result.stream.ReadPaused() {
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
	owner := readcontrol.NewPause(struct{ *Stream }{remote})
	if owner == nil {
		t.Fatal("wrapped stream lost its internal read boundary")
	}
	if err := owner.Pause(); err != nil {
		t.Fatal(err)
	}
	if err := remote.ResumeRead(); err != nil {
		t.Fatal(err)
	}
	if !remote.ReadPaused() {
		t.Fatal("application resume released a separate owner")
	}
	if err := owner.Resume(); err != nil {
		t.Fatal(err)
	}
	if got := string(readFull(t, a, 4)); got != "held" {
		t.Fatalf("retained echo = %q", got)
	}
	if err := remote.PauseRead(); err != nil {
		t.Fatal(err)
	}
	if err := remote.Abort(errors.New("test reset while paused")); err != nil {
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
	if err := remote.ResumeRead(); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("resume after close = %v", err)
	}
}

func TestStreamWakeProcessesRetainedInputEveryTime(t *testing.T) {
	opened := make(chan *Stream, 1)
	held := make(chan struct{}, 1)
	var consume atomic.Bool
	handler := uio.FuncHandler{Open: func(c uio.Conn) {
		opened <- c.(*Stream)
	}, Read: func(c uio.Conn) error {
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
	}}
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
		if remote.ReadPaused() {
			t.Fatal("fixture unexpectedly acquired a read pause")
		}
		consume.Store(true)
		if err := remote.ResumeRead(); err != nil {
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

func TestStreamEOFCallbackCanDeferItsOwnCompletion(t *testing.T) {
	s, st, _, h := productionStreamPair(t)
	reads := 0
	s.cfg.Handler = uio.FuncHandler{Read: func(c uio.Conn) error {
		if _, err := c.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
			t.Errorf("final read = %v, want EOF", err)
		}
		reads++
		if reads == 1 {
			return c.PauseRead()
		}
		return nil
	}, Close: h.OnClose}
	if err := st.PauseRead(); err != nil {
		t.Fatal(err)
	}
	if err := st.pushData(nil, true, false); err != nil {
		t.Fatal(err)
	}
	_ = st.CloseWrite()
	s.flush()
	if err := st.ResumeRead(); err != nil {
		t.Fatal(err)
	}
	s.runWakes()
	s.drainCloses()
	if reads != 1 || st.State()&uio.StateClosed != 0 || len(h.closed()) != 0 {
		t.Fatal("completion overtook new work requested inside the EOF callback")
	}
	if err := st.ResumeRead(); err != nil {
		t.Fatal(err)
	}
	s.runWakes()
	s.drainCloses()
	if reads != 2 || len(h.closed()) != 1 {
		t.Fatalf("final callbacks: reads=%d closes=%v", reads, h.closed())
	}
}

func TestStreamLastBytesCallbackKeepsItsCompletionDecision(t *testing.T) {
	for _, callback := range []string{"read", "timer"} {
		for _, consume := range []string{"read", "discard"} {
			for _, action := range []string{"pause", "return-error", "local-abort"} {
				t.Run(callback+"/"+consume+"/"+action, func(t *testing.T) {
					s, st, _, _ := productionStreamPair(t)
					cause := errors.New("consumer completion decision")
					var events []string
					var closeErr error
					progress := func(c uio.Conn) error {
						if consume == "read" {
							buffer := make([]byte, 4)
							if n, err := c.Read(buffer); n != 4 || err != nil || string(buffer) != "last" {
								t.Fatalf("last read=%d,%v,%q", n, err, buffer)
							}
						} else if n, err := c.Discard(-1); n != 4 || err != nil {
							t.Fatalf("last discard=%d,%v", n, err)
						}
						events = append(events, "data")
						if c.State()&uio.StateClosed != 0 {
							t.Fatal("draining the last bytes ended the callback's lifetime")
						}
						switch action {
						case "pause":
							return c.PauseRead()
						case "return-error":
							return cause
						default:
							return c.Abort(cause)
						}
					}
					s.cfg.Handler = uio.FuncHandler{Read: func(c uio.Conn) error {
						if callback == "read" && len(events) == 0 {
							return progress(c)
						}
						if n, err := c.Read(make([]byte, 1)); n != 0 || !errors.Is(err, io.EOF) {
							t.Fatalf("continuation EOF=%d,%v", n, err)
						}
						events = append(events, "EOF")
						return nil
					}, Timer: func(c uio.Conn) error {
						return progress(c)
					}, Close: func(_ uio.Conn, err error) {
						events = append(events, "close")
						closeErr = err
					}}
					if err := st.pushData([]byte("last"), true, false); err != nil {
						t.Fatal(err)
					}
					if err := st.CloseWrite(); err != nil {
						t.Fatal(err)
					}
					s.flush()
					if callback == "read" {
						s.dispatchRead(st)
					} else {
						if err := st.SetWakeDeadline(time.Now().Add(-time.Second)); err != nil {
							t.Fatal(err)
						}
						if err := s.onTimer(s.conn); err != nil {
							t.Fatal(err)
						}
					}
					s.drainCloses()
					if action == "return-error" || action == "local-abort" {
						if got := strings.Join(events, ","); got != "data,close" || !errors.Is(closeErr, cause) {
							t.Fatalf("callback abort lost to FIN: events=%s close=%v", got, closeErr)
						}
						s.dispatchRead(st)
						if got := strings.Join(events, ","); got != "data,close" {
							t.Fatalf("callback after close: %s", got)
						}
						return
					}
					if got := strings.Join(events, ","); got != "data" || st.State()&uio.StateClosed != 0 {
						t.Fatalf("deferred callback lost to FIN: events=%s state=%v", got, st.State())
					}
					if action == "pause" {
						if err := st.ResumeRead(); err != nil {
							t.Fatal(err)
						}
					}
					s.runWakes()
					s.drainCloses()
					if got := strings.Join(events, ","); got != "data,EOF,close" || !errors.Is(closeErr, io.EOF) {
						t.Fatalf("final callbacks=%s close=%v", got, closeErr)
					}
				})
			}
		}
	}
}

func TestWindowOverflowAndControlQueueFloodBounds(t *testing.T) {
	sw := newSendWindow(1 << 20)
	_ = sw.take(512 << 10)
	if err := sw.grant(int64(^uint64(0) >> 1)); !errors.Is(err, errWindowExceeded) {
		t.Fatalf("sendWindow.grant(MaxInt64) = %v, want errWindowExceeded", err)
	}
	rw := newRecvWindow(1 << 20)
	if err := rw.onData(512 << 10); err != nil {
		t.Fatal(err)
	}
	if err := rw.onData(int64(^uint64(0) >> 1)); !errors.Is(err, errWindowExceeded) {
		t.Fatalf("recvWindow.onData(MaxInt64) = %v, want errWindowExceeded", err)
	}

	// Control queue flood protection when outbound transport is stalled (ErrOutboundOverflow).
	s, _, _, _ := productionStreamPair(t)
	s.mu.Lock()
	limit := s.maxControlFrames()
	for i := range limit + 8 {
		s.sendControlLocked(typePing, flagACK, 0, uint32(i))
	}
	if len(s.control) != limit || !s.controlOverflow {
		s.mu.Unlock()
		t.Fatalf("control len=%d overflow=%v, want %d/true", len(s.control), s.controlOverflow, limit)
	}
	err := s.flushLocked()
	s.mu.Unlock()
	if !errors.Is(err, ErrProtocol) {
		t.Fatalf("flushLocked on control queue overflow = %v, want ErrProtocol", err)
	}
}
