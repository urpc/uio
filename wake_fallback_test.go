//go:build (linux || darwin || netbsd || freebsd || openbsd || dragonfly) && !stdio

package uio

import (
	"errors"
	"testing"
	"time"
)

// The runtime fallback: a kernel timer that cannot be armed must not idle the
// loop's wake timers. With the kernel step forced to fail, an armed deadline
// still fires — through the loop's runtime timer — and a clear disarms it.
func TestWakeFallsBackWhenKernelTimerFails(t *testing.T) {
	rec := &wakeRecorder{}
	events := &Events{Pollers: 1, Handler: rec}
	started := make(chan struct{})
	events.Hooks = FuncHandler{Start: func(*Events) { close(started) }}
	done := make(chan error, 1)
	go func() { done <- events.Serve() }()
	t.Cleanup(func() {
		_ = events.Close(nil)
		<-done
	})
	<-started
	sinkStarted := make(chan struct{})
	sink := &Events{Pollers: 1, Handler: FuncHandler{
		Open: func(Conn) {}, Data: func(Conn) error { return nil }, Close: func(Conn, error) {},
	}, Hooks: FuncHandler{Start: func(*Events) { close(sinkStarted) }}}
	sinkDone := make(chan error, 1)
	go func() { sinkDone <- sink.Serve() }()
	t.Cleanup(func() {
		_ = sink.Close(nil)
		<-sinkDone
	})
	<-sinkStarted
	listener, err := sink.Listen("tcp://127.0.0.1:0", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	conn, err := events.Dial("tcp://"+listener.Addr().String(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	// From here on every kernel arm fails: the loop runs on its runtime timer.
	fail := func(*eventLoop, time.Duration) error { return errors.New("no kernel timer") }
	armKernelTimerOverride.Store(&fail)
	t.Cleanup(func() { armKernelTimerOverride.Store(nil) })

	if err := conn.SetWakeDeadline(time.Now().Add(20 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 3*time.Second, func() bool { return rec.fires() >= 1 })
	if rec.fires() != 1 {
		t.Fatalf("fallback fired %d times, want exactly 1", rec.fires())
	}

	// Disarm through the fallback stops it.
	if err := conn.SetWakeDeadline(time.Time{}); err != nil {
		t.Fatal(err)
	}
	fires := rec.fires()
	time.Sleep(150 * time.Millisecond)
	if got := rec.fires(); got != fires {
		t.Fatalf("a disarmed fallback kept firing: %d then %d", fires, got)
	}
}

// The fallback carries a repeating timer too: an interval keeps firing
// through the runtime timer, and stopping it ends the repeats.
func TestWakeIntervalFallsBackAndRepeats(t *testing.T) {
	rec := &wakeRecorder{}
	events := &Events{Pollers: 1, Handler: rec}
	started := make(chan struct{})
	events.Hooks = FuncHandler{Start: func(*Events) { close(started) }}
	done := make(chan error, 1)
	go func() { done <- events.Serve() }()
	t.Cleanup(func() {
		_ = events.Close(nil)
		<-done
	})
	<-started
	sinkStarted := make(chan struct{})
	sink := &Events{Pollers: 1, Handler: FuncHandler{
		Open: func(Conn) {}, Data: func(Conn) error { return nil }, Close: func(Conn, error) {},
	}, Hooks: FuncHandler{Start: func(*Events) { close(sinkStarted) }}}
	sinkDone := make(chan error, 1)
	go func() { sinkDone <- sink.Serve() }()
	t.Cleanup(func() {
		_ = sink.Close(nil)
		<-sinkDone
	})
	<-sinkStarted
	listener, err := sink.Listen("tcp://127.0.0.1:0", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	conn, err := events.Dial("tcp://"+listener.Addr().String(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	fail := func(*eventLoop, time.Duration) error { return errors.New("no kernel timer") }
	armKernelTimerOverride.Store(&fail)
	t.Cleanup(func() { armKernelTimerOverride.Store(nil) })

	if err := conn.SetWakeInterval(20 * time.Millisecond); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 3*time.Second, func() bool { return rec.fires() >= 3 })
	if rec.fires() < 3 {
		t.Fatalf("the fallback interval fired %d times, want at least 3", rec.fires())
	}
	if err := conn.SetWakeInterval(0); err != nil {
		t.Fatal(err)
	}
	fires := rec.fires()
	time.Sleep(150 * time.Millisecond)
	if got := rec.fires(); got != fires {
		t.Fatalf("a stopped fallback interval kept firing: %d then %d", fires, got)
	}
}
