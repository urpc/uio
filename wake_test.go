//go:build linux || darwin || netbsd || freebsd || openbsd || dragonfly || stdio

package uio

import (
	"sync"
	"testing"
	"time"
)

func dialWake(t *testing.T) (*wakeRecorder, Conn) {
	t.Helper()
	rec := &wakeRecorder{}
	started := make(chan struct{})
	events := &Events{Pollers: 1, Handler: rec, Hooks: FuncHandler{Start: func(*Events) {
		close(started)
	}}}
	done := make(chan error, 1)
	go func() {
		done <- events.Serve()
	}()
	t.Cleanup(func() {
		_ = events.Close(nil)
		<-done
	})
	<-started
	sinkStarted := make(chan struct{})
	sink := &Events{Pollers: 1, Handler: FuncHandler{Open: func(Conn) {
	}, Read: func(Conn) error {
		return nil
	}, Close: func(Conn, error) {
	}}, Hooks: FuncHandler{Start: func(*Events) {
		close(sinkStarted)
	}}}
	sinkDone := make(chan error, 1)
	go func() {
		sinkDone <- sink.Serve()
	}()
	t.Cleanup(func() {
		_ = sink.Close(nil)
		<-sinkDone
	})
	<-sinkStarted
	listener, err := sink.Listen("tcp://127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = listener.Close()
	})
	conn, err := events.Dial("tcp://" + listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = conn.Close()
	})
	return rec, conn
}

func TestWakeRaceCloseAndTurns(t *testing.T) {
	for range 50 {
		func() {
			rec, conn := dialWake(t)
			stop := make(chan struct{})
			var wg sync.WaitGroup
			wg.Add(2)
			go func() {
				defer wg.Done()
				for {
					select {
					case <-stop:
						return
					default:
					}
					_ = conn.SetWakeDeadline(time.Now().Add(time.Millisecond))
					_ = conn.SetWakeInterval(2 * time.Millisecond)
					_ = conn.SetWakeDeadline(time.Time{})
					_ = conn.Wake()
				}
			}()
			go func() {
				defer wg.Done()
				time.Sleep(2 * time.Millisecond)
				_ = conn.Close()
				close(stop)
			}()
			wg.Wait()
			waitFor(t, 3*time.Second, func() bool {
				return rec.closed.Load() > 0
			})
			fires := rec.fires()
			time.Sleep(50 * time.Millisecond)
			if got := rec.fires(); got > fires {
				t.Fatalf("a closed connection kept waking: %d then %d", fires, got)
			}
		}()
	}
}

func TestWakeSubTickIntervalFires(t *testing.T) {
	rec, conn := dialWake(t)
	if err := conn.SetWakeInterval(time.Millisecond); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 2*time.Second, func() bool {
		return rec.fires() >= 5
	})
	if got := rec.fires(); got < 5 {
		t.Fatalf("a 1ms interval fired %d times in 2s, want at least 5", got)
	}
}

func TestWakePastDeadlineFires(t *testing.T) {
	rec, conn := dialWake(t)
	if err := conn.SetWakeDeadline(time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 2*time.Second, func() bool {
		return rec.fires() >= 1
	})
	if got := rec.fires(); got < 1 {
		t.Fatal("a past deadline never fired")
	} else if got > 1 {
		t.Fatalf("a past one-shot fired %d times", got)
	}
}
