package umux

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urpc/uio"
	"github.com/urpc/uio/internal/readcontrol"
)

func TestStreamBothFINWaitsForPausedConsumerAndResumeTurn(t *testing.T) {
	for _, drain := range []string{"empty", "read", "discard"} {
		t.Run(drain, func(t *testing.T) {
			s, st, _, _ := productionStreamPair(t)
			var events []string
			s.cfg.Handler = uio.FuncHandler{
				Read: func(c uio.Conn) error {
					if n, err := c.Read(make([]byte, 1)); n != 0 || !errors.Is(err, io.EOF) {
						t.Errorf("final read = %d, %v, want EOF", n, err)
					}
					events = append(events, "read EOF")
					return nil
				},
				Close: func(_ uio.Conn, err error) {
					if !errors.Is(err, io.EOF) {
						t.Errorf("close = %v, want EOF", err)
					}
					events = append(events, "close")
				},
			}
			if err := st.PauseReads(); err != nil {
				t.Fatal(err)
			}
			var payload []byte
			if drain != "empty" {
				payload = []byte("held")
			}
			if err := st.pushData(payload, true, false); err != nil {
				t.Fatal(err)
			}
			if err := st.CloseWrite(); err != nil {
				t.Fatal(err)
			}
			s.flush() // accepting the local FIN can also attempt completion
			switch drain {
			case "read":
				if n, err := st.Read(make([]byte, len(payload))); n != len(payload) || err != nil {
					t.Fatalf("drain = %d, %v", n, err)
				}
			case "discard":
				if _, err := st.Discard(-1); err != nil {
					t.Fatal(err)
				}
			}
			s.dispatchRead(st)
			s.tryFinish(st)
			s.drainCloses()
			if st.State()&uio.StateClosed != 0 || len(events) != 0 {
				t.Fatalf("both FIN bypassed paused input: state=%v events=%v", st.State(), events)
			}
			if err := st.ResumeReads(); err != nil {
				t.Fatal(err)
			}
			s.tryFinish(st) // an unpaused owner still has a pending consumer turn
			s.drainCloses()
			if st.State()&uio.StateClosed != 0 || len(events) != 0 {
				t.Fatalf("completion overtook the pending resume: events=%v", events)
			}
			s.runWakes()
			s.drainCloses()
			if got := strings.Join(events, ","); got != "read EOF,close" {
				t.Fatalf("final callbacks = %s", got)
			}
		})
	}
}

func TestStreamEOFCallbackCanDeferItsOwnCompletion(t *testing.T) {
	for _, deferWith := range []string{"pause", "yield"} {
		t.Run(deferWith, func(t *testing.T) {
			s, st, _, h := productionStreamPair(t)
			reads := 0
			s.cfg.Handler = uio.FuncHandler{Read: func(c uio.Conn) error {
				if _, err := c.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
					t.Errorf("final read = %v, want EOF", err)
				}
				reads++
				if reads == 1 {
					if deferWith == "pause" {
						return c.PauseReads()
					}
					return c.YieldRead()
				}
				return nil
			}, Close: h.OnClose}
			if err := st.PauseReads(); err != nil {
				t.Fatal(err)
			}
			if err := st.pushData(nil, true, false); err != nil {
				t.Fatal(err)
			}
			_ = st.CloseWrite()
			s.flush()
			if err := st.ResumeReads(); err != nil {
				t.Fatal(err)
			}
			s.runWakes()
			s.drainCloses()
			if reads != 1 || st.State()&uio.StateClosed != 0 || len(h.closed()) != 0 {
				t.Fatal("completion overtook new work requested inside the EOF callback")
			}
			if deferWith == "pause" {
				if err := st.ResumeReads(); err != nil {
					t.Fatal(err)
				}
			}
			s.runWakes()
			s.drainCloses()
			if reads != 2 || len(h.closed()) != 1 {
				t.Fatalf("final callbacks: reads=%d closes=%v", reads, h.closed())
			}
		})
	}
}

func TestStreamYieldKeepsBufferedFinalInputBeforeEOFAndClose(t *testing.T) {
	for _, consume := range []string{"read", "discard"} {
		t.Run(consume, func(t *testing.T) {
			s, st, _, _ := productionStreamPair(t)
			var events []string
			s.cfg.Handler = uio.FuncHandler{
				Read: func(c uio.Conn) error {
					if len(events) == 0 {
						// Publish the decoder's pending work before draining the
						// transport bytes that otherwise permit FIN completion.
						if err := c.YieldRead(); err != nil {
							t.Fatal(err)
						}
						if consume == "read" {
							buffer := make([]byte, 4)
							if n, err := c.Read(buffer); n != 4 || err != nil || string(buffer) != "last" {
								t.Fatalf("final data=%d,%v,%q", n, err, buffer)
							}
						} else if n, err := c.Discard(-1); n != 4 || err != nil {
							t.Fatalf("final discard=%d,%v", n, err)
						}
						events = append(events, "data")
						return nil
					}
					if n, err := c.Read(make([]byte, 1)); n != 0 || !errors.Is(err, io.EOF) {
						t.Fatalf("continuation EOF=%d,%v", n, err)
					}
					events = append(events, "EOF")
					return nil
				},
				Close: func(_ uio.Conn, err error) {
					if !errors.Is(err, io.EOF) {
						t.Errorf("final close=%v", err)
					}
					events = append(events, "close")
				},
			}
			if err := st.PauseReads(); err != nil {
				t.Fatal(err)
			}
			if err := st.pushData([]byte("last"), true, false); err != nil {
				t.Fatal(err)
			}
			if err := st.CloseWrite(); err != nil {
				t.Fatal(err)
			}
			s.flush()
			if err := st.ResumeReads(); err != nil {
				t.Fatal(err)
			}
			s.runWakes()
			s.drainCloses()
			if got := strings.Join(events, ","); got != "data" || st.State()&uio.StateClosed != 0 {
				t.Fatalf("buffer drain overtook yielded decoder work: events=%s state=%v", got, st.State())
			}
			if st.ReadsPaused() || !st.yieldOwed {
				t.Fatal("yield did not retain a continuation independent of consumer pauses")
			}
			s.runWakes()
			s.drainCloses()
			if got := strings.Join(events, ","); got != "data,EOF,close" {
				t.Fatalf("final callbacks=%s", got)
			}
		})
	}
}

func TestStreamYieldDoesNotReleaseForeignPauseAtFinalEOF(t *testing.T) {
	s, st, _, h := productionStreamPair(t)
	owner := readcontrol.NewPause(st)
	reads := 0
	s.cfg.Handler = uio.FuncHandler{Read: func(c uio.Conn) error {
		reads++
		if reads == 1 {
			if err := owner.Pause(); err != nil {
				return err
			}
			return c.YieldRead()
		}
		return nil
	}, Close: h.OnClose}
	if err := st.PauseReads(); err != nil {
		t.Fatal(err)
	}
	if err := st.pushData(nil, true, false); err != nil {
		t.Fatal(err)
	}
	if err := st.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	s.flush()
	if err := st.ResumeReads(); err != nil {
		t.Fatal(err)
	}
	s.runWakes()
	if !owner.Paused() || !st.ReadsPaused() {
		t.Fatal("yield released a foreign consumer pause")
	}
	s.runWakes() // the yielded turn remains suppressed by its independent owner
	s.drainCloses()
	if reads != 1 || st.State()&uio.StateClosed != 0 || len(h.closed()) != 0 {
		t.Fatal("a yielded turn bypassed a foreign consumer pause")
	}
	if err := owner.Resume(); err != nil {
		t.Fatal(err)
	}
	s.runWakes()
	s.drainCloses()
	if reads != 2 || len(h.closed()) != 1 {
		t.Fatalf("foreign owner resume did not deliver the final turn: reads=%d closes=%v", reads, h.closed())
	}
}

func TestStreamLastBytesCallbackKeepsItsCompletionDecision(t *testing.T) {
	for _, callback := range []string{"read", "timer"} {
		for _, consume := range []string{"read", "discard"} {
			for _, action := range []string{"yield", "pause", "return-error", "local-abort"} {
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
						case "yield":
							return c.YieldRead()
						case "pause":
							return c.PauseReads()
						case "return-error":
							return cause
						default:
							return c.CloseWith(cause)
						}
					}
					s.cfg.Handler = uio.FuncHandler{
						Read: func(c uio.Conn) error {
							if callback == "read" && len(events) == 0 {
								return progress(c)
							}
							if n, err := c.Read(make([]byte, 1)); n != 0 || !errors.Is(err, io.EOF) {
								t.Fatalf("continuation EOF=%d,%v", n, err)
							}
							events = append(events, "EOF")
							return nil
						},
						Timer: func(c uio.Conn) error { return progress(c) },
						Close: func(_ uio.Conn, err error) {
							events = append(events, "close")
							closeErr = err
						},
					}
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
						// A terminal stream cannot acquire a subsequent callback.
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
						if err := st.ResumeReads(); err != nil {
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

func TestStreamPausedBothFINKeepsEOFBeforeClose(t *testing.T) {
	for _, payload := range []string{"", "retained input"} {
		name := "empty"
		if payload != "" {
			name = "payload"
		}
		t.Run(name, func(t *testing.T) {
			type event struct {
				kind, payload string
				err           error
			}
			opened := make(chan *Stream, 2)
			callbacks := make(chan event, 4)
			timers := make(chan struct{}, 1)
			var target atomic.Uint32
			var received strings.Builder
			handler := uio.FuncHandler{
				Open: func(c uio.Conn) {
					st := c.(*Stream)
					if target.CompareAndSwap(0, st.ID()) {
						if err := st.PauseReads(); err != nil {
							t.Error(err)
						}
					}
					opened <- st
				},
				Read: func(c uio.Conn) error {
					if c.(*Stream).ID() != target.Load() {
						_, err := c.WriteTo(c)
						return err
					}
					buffer := make([]byte, 32)
					for {
						n, err := c.Read(buffer)
						received.Write(buffer[:n])
						if errors.Is(err, io.EOF) {
							callbacks <- event{kind: "EOF", payload: received.String()}
							return nil
						}
						if err != nil || n == 0 {
							return err
						}
					}
				},
				Timer: func(c uio.Conn) error {
					if c.(*Stream).ID() == target.Load() {
						timers <- struct{}{}
					}
					return nil
				},
				Close: func(c uio.Conn, err error) {
					if c.(*Stream).ID() == target.Load() {
						callbacks <- event{kind: "close", err: err}
					}
				},
			}
			session, _ := startPair(t, Config{Handler: handler, KeepAlive: -1}, Config{KeepAlive: -1})
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			local, err := session.Open(ctx)
			if err != nil {
				t.Fatal(err)
			}
			remote := <-opened
			if payload != "" {
				writeAll(t, local, []byte(payload))
			}
			if err := remote.CloseWrite(); err != nil {
				t.Fatal(err)
			}
			if err := local.CloseWrite(); err != nil {
				t.Fatal(err)
			}
			waitFor(t, "both FIN accepted while input is paused", func() bool {
				remote.sess.mu.Lock()
				remote.mu.Lock()
				ready := remote.closedLocal || (remote.peerFIN && remote.localFIN && remote.finState == finSent)
				remote.mu.Unlock()
				remote.sess.mu.Unlock()
				return ready
			})
			if remote.State()&uio.StateClosed != 0 {
				t.Fatal("both FIN closed the paused stream")
			}
			if err := remote.SetWakeDeadline(time.Now().Add(5 * time.Millisecond)); err != nil {
				t.Fatal(err)
			}
			select {
			case <-timers:
			case <-ctx.Done():
				t.Fatal("paused stream's timer stopped")
			}
			sibling, err := session.Open(ctx)
			if err != nil {
				t.Fatal(err)
			}
			<-opened
			writeAll(t, sibling, []byte("live"))
			if got := string(readFull(t, sibling, 4)); got != "live" {
				t.Fatalf("sibling echo = %q", got)
			}
			select {
			case early := <-callbacks:
				t.Fatalf("callback before ResumeReads: %+v", early)
			default:
			}
			if err := remote.ResumeReads(); err != nil {
				t.Fatal(err)
			}
			for _, want := range []string{"EOF", "close"} {
				select {
				case got := <-callbacks:
					if got.kind != want || (want == "EOF" && got.payload != payload) || (want == "close" && !errors.Is(got.err, io.EOF)) {
						t.Fatalf("callback = %+v, want %s after payload %q", got, want, payload)
					}
				case <-ctx.Done():
					t.Fatalf("missing %s callback after ResumeReads", want)
				}
			}
			select {
			case extra := <-callbacks:
				t.Fatalf("duplicate final callback: %+v", extra)
			default:
			}
		})
	}
}

func TestStreamResumeRacingFinalFINKeepsConsumerTurn(t *testing.T) {
	for range 50 {
		s, st, _, h := productionStreamPair(t)
		var reads int
		s.cfg.Handler = uio.FuncHandler{Read: func(c uio.Conn) error {
			if _, err := c.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
				t.Errorf("final read = %v, want EOF", err)
			}
			reads++
			return nil
		}, Close: h.OnClose}
		if err := st.PauseReads(); err != nil {
			t.Fatal(err)
		}
		if err := st.CloseWrite(); err != nil {
			t.Fatal(err)
		}
		s.flush()
		start := make(chan struct{})
		var done sync.WaitGroup
		done.Add(2)
		go func() {
			defer done.Done()
			<-start
			if err := st.ResumeReads(); err != nil {
				t.Error(err)
			}
		}()
		go func() {
			defer done.Done()
			<-start
			if err := st.pushData(nil, true, false); err != nil {
				t.Error(err)
			}
			s.tryFinish(st)
		}()
		close(start)
		done.Wait()
		if st.State()&uio.StateClosed != 0 {
			t.Fatal("final FIN overtook the consumer's resume turn")
		}
		s.runWakes()
		s.drainCloses()
		if reads != 1 || len(h.closed()) != 1 {
			t.Fatalf("final callbacks: reads=%d closes=%v", reads, h.closed())
		}
	}
}

func TestStreamPausedBothFINStillAllowsPullAndAbort(t *testing.T) {
	t.Run("pull", func(t *testing.T) {
		s, st, _, _ := productionStreamPair(t)
		s.cfg.Handler = nil
		if err := st.PauseReads(); err != nil {
			t.Fatal(err)
		}
		if err := st.pushData([]byte("x"), true, false); err != nil {
			t.Fatal(err)
		}
		_ = st.CloseWrite()
		s.flush()
		if n, err := st.Read(make([]byte, 1)); n != 1 || err != nil {
			t.Fatalf("pull read = %d, %v", n, err)
		}
		if st.State()&uio.StateClosed == 0 {
			t.Fatal("a pull reader waited for a nonexistent callback")
		}
	})
	for _, failure := range []string{"reset", "transport", "deadline"} {
		t.Run(failure, func(t *testing.T) {
			s, st, _, h := productionStreamPair(t)
			if err := st.PauseReads(); err != nil {
				t.Fatal(err)
			}
			if err := st.pushData(nil, true, false); err != nil {
				t.Fatal(err)
			}
			_ = st.CloseWrite()
			s.flush()
			cause := ErrStreamReset
			switch failure {
			case "reset":
				s.abortRemote(st, cause)
			case "transport":
				s.onTransportClose(cause)
			case "deadline":
				cause = os.ErrDeadlineExceeded
				if err := st.SetReadDeadline(time.Now().Add(-time.Second)); err != nil {
					t.Fatal(err)
				}
				_ = s.onTimer(s.conn)
			}
			s.drainCloses()
			if closed := h.closed(); len(closed) != 1 || !errors.Is(closed[0], cause) {
				t.Fatalf("paused failure close = %v, want %v", closed, cause)
			}
		})
	}
}
