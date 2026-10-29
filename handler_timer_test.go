package uio

import (
	"errors"
	"sync"
	"testing"
	"time"
)

// timerRecorder records the order callbacks ran, to assert OnTimer runs
// before OnRead in the turn a deadline brought.
type timerRecorder struct {
	mu    sync.Mutex
	order []string
}

func (r *timerRecorder) note(what string) {
	r.mu.Lock()
	r.order = append(r.order, what)
	r.mu.Unlock()
}

func (r *timerRecorder) sequence() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.order...)
}

func (r *timerRecorder) count(what string) int {
	n := 0
	for _, got := range r.sequence() {
		if got == what {
			n++
		}
	}
	return n
}

// dialTimer dials a sink and returns the recorder with the connection: timer
// tests need one settled connection, not a server, in the shape of the wake
// tests'.
func dialTimer(t *testing.T) (*timerRecorder, Conn) {
	t.Helper()
	rec := &timerRecorder{}
	started := make(chan struct{})
	events := &Events{Pollers: 1, Handler: FuncHandler{
		Read:  func(Conn) error { rec.note("read"); return nil },
		Timer: func(Conn) error { rec.note("timer"); return nil },
		Close: func(Conn, error) {},
	}, Hooks: FuncHandler{Start: func(*Events) { close(started) }}}
	done := make(chan error, 1)
	go func() { done <- events.Serve() }()
	t.Cleanup(func() {
		_ = events.Close(nil)
		<-done
	})
	<-started
	listener, err := events.Listen("tcp://127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	conn, err := events.Dial("tcp://" + listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return rec, conn
}

func waitForNote(t *testing.T, rec *timerRecorder, what string, want int, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		// Look before giving up: a fire landing just past the deadline must
		// not turn into a failure that reports the count it was waiting for.
		if got := rec.count(what); got >= want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s fired %d times within %v, want at least %d; ran %v",
				what, rec.count(what), within, want, rec.sequence())
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// SetWakeDeadline on a bare connection runs OnTimer once the deadline
// passes; a zero time cancels a fresh arm.
func TestHandlerWakeDeadlineFiresOnTimer(t *testing.T) {
	rec, conn := dialTimer(t)
	if err := conn.SetWakeDeadline(time.Now().Add(30 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	waitForNote(t, rec, "timer", 1, 2*time.Second)
	// One deadline, one fire: nothing more arrives.
	time.Sleep(60 * time.Millisecond)
	if n := rec.count("timer"); n != 1 {
		t.Fatalf("OnTimer ran %d times for a one-shot deadline, want 1", n)
	}
	// A zero time cancels a fresh arm.
	if err := conn.SetWakeDeadline(time.Now().Add(20 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if err := conn.SetWakeDeadline(time.Time{}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(60 * time.Millisecond)
	if n := rec.count("timer"); n != 1 {
		t.Fatalf("OnTimer ran %d more times after the cancel, want none", n-1)
	}
}

// SetWakeInterval fires OnTimer once per period, and a zero interval stops it.
func TestHandlerWakeIntervalFiresEachPeriod(t *testing.T) {
	rec, conn := dialTimer(t)
	if err := conn.SetWakeInterval(25 * time.Millisecond); err != nil {
		t.Fatal(err)
	}
	// The budget only has to tell a repeating timer from a one-shot: the
	// period itself is the wheel's business, and a loaded CI runner may take
	// it late.
	waitForNote(t, rec, "timer", 3, 2*time.Second)
	if err := conn.SetWakeInterval(0); err != nil {
		t.Fatal(err)
	}
	// A fire already on its way when the interval stopped still runs; give
	// it room to land before counting what comes after.
	time.Sleep(80 * time.Millisecond)
	stopped := rec.count("timer")
	time.Sleep(120 * time.Millisecond)
	if n := rec.count("timer"); n != stopped {
		t.Fatalf("OnTimer ran %d more times after the interval stopped", n-stopped)
	}
}

// An OnTimer error closes the connection, as an OnRead error does.
func TestHandlerTimerErrorCloses(t *testing.T) {
	wantErr := errors.New("timer boom")
	closed := make(chan struct{}, 1)
	started := make(chan struct{})
	events := &Events{Pollers: 1, Handler: FuncHandler{
		Read:  func(Conn) error { return nil },
		Timer: func(Conn) error { return wantErr },
		Close: func(Conn, error) {
			// A signal, not a hand-off: both ends of this Events close
			// together, and a close callback blocked on a full channel keeps
			// Serve from ever joining it.
			select {
			case closed <- struct{}{}:
			default:
			}
		},
	}, Hooks: FuncHandler{Start: func(*Events) { close(started) }}}
	done := make(chan error, 1)
	go func() { done <- events.Serve() }()
	t.Cleanup(func() {
		_ = events.Close(nil)
		<-done
	})
	<-started
	listener, err := events.Listen("tcp://127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	conn, err := events.Dial("tcp://" + listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if err := conn.SetWakeDeadline(time.Now().Add(20 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("OnClose did not run after the OnTimer error")
	}
}

// With layers installed, the Handler's own SetWakeDeadline still reaches its
// OnTimer: the stack's timer table carries a slot of the Handler's own.
func TestHandlerTimerThroughLayers(t *testing.T) {
	rec := &timerRecorder{}
	started := make(chan struct{})
	events := &Events{Pollers: 1, Handler: FuncHandler{
		Read:  func(Conn) error { rec.note("read"); return nil },
		Timer: func(Conn) error { rec.note("timer"); return nil },
		Close: func(Conn, error) {},
	}, Hooks: FuncHandler{Start: func(*Events) { close(started) }}}
	done := make(chan error, 1)
	go func() { done <- events.Serve() }()
	t.Cleanup(func() {
		_ = events.Close(nil)
		<-done
	})
	<-started
	listener, err := events.Listen("tcp://127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	conn, err := events.Dial("tcp://"+listener.Addr().String(), WithLayers(PassThroughLayer))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if err := conn.SetWakeDeadline(time.Now().Add(30 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	waitForNote(t, rec, "timer", 1, 2*time.Second)
	if n := rec.count("timer"); n != 1 {
		t.Fatalf("OnTimer ran %d times through a stack, want 1", n)
	}
}

// OnTimer shares the inbound-access scope: reading from it is legal on both
// backends. Before the scope covered it, the native backend's diagnostics
// panicked under -race.
func TestHandlerTimerHasInboundAccess(t *testing.T) {
	panicked := make(chan any, 1)
	conn := serveAndDial(t, FuncHandler{
		Read: func(Conn) error { return nil },
		Timer: func(c Conn) error {
			defer func() {
				if r := recover(); r != nil {
					panicked <- r
				}
			}()
			_ = c.InboundBuffered()
			_, _ = c.Read(nil)
			_ = c.Peek(nil)
			return nil
		},
		Close: func(Conn, error) {},
	})
	if err := conn.SetWakeDeadline(time.Now().Add(20 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-panicked:
		t.Fatalf("OnTimer panicked using inbound access: %v", r)
	case <-time.After(2 * time.Second):
	}
}

// Concurrent SetWakeDeadline and SetWakeInterval calls stay serialized: the
// wheel's arm and the turn-time mirror never name different deadlines.
func TestSetWakeDeadlineConcurrent(t *testing.T) {
	conn := serveAndDial(t, FuncHandler{
		Read:  func(Conn) error { return nil },
		Timer: func(Conn) error { return nil },
		Close: func(Conn, error) {},
	})
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				switch (g + i) % 3 {
				case 0:
					_ = conn.SetWakeDeadline(time.Now().Add(time.Duration(i) * time.Millisecond))
				case 1:
					_ = conn.SetWakeInterval(time.Duration(i) * time.Millisecond)
				default:
					_ = conn.SetWakeDeadline(time.Time{})
				}
			}
		}(g)
	}
	wg.Wait()
	_ = conn.SetWakeDeadline(time.Time{})
}

// serveAndDial serves one Events with handler and returns a dialed, settled
// connection to it.
func serveAndDial(t *testing.T, handler Handler) Conn {
	t.Helper()
	started := make(chan struct{})
	events := &Events{Pollers: 1, Handler: handler, Hooks: FuncHandler{
		Start: func(*Events) { close(started) },
	}}
	done := make(chan error, 1)
	go func() { done <- events.Serve() }()
	t.Cleanup(func() {
		_ = events.Close(nil)
		<-done
	})
	<-started
	listener, err := events.Listen("tcp://127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	conn, err := events.Dial("tcp://" + listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// A deadline at the Unix epoch is a legal past time, not the unset marker:
// it fires OnTimer once, promptly.
func TestHandlerEpochDeadlineFiresOnTimer(t *testing.T) {
	rec := &timerRecorder{}
	conn := serveAndDial(t, FuncHandler{
		Read:  func(Conn) error { rec.note("read"); return nil },
		Timer: func(Conn) error { rec.note("timer"); return nil },
		Close: func(Conn, error) {},
	})
	if err := conn.SetWakeDeadline(time.Unix(0, 0)); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if rec.count("timer") >= 1 {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("epoch deadline fired no OnTimer; ran %v", rec.sequence())
}

// Re-arming a live timer never consumes the interval as a one-shot: a
// deadline published beside a stale zero period once disarmed the mirror for
// good, and no OnTimer followed the successful re-arm.
func TestWakeReArmKeepsTheIntervalAlive(t *testing.T) {
	rec := &timerRecorder{}
	conn := serveAndDial(t, FuncHandler{
		Read:  func(Conn) error { return nil },
		Timer: func(Conn) error { rec.note("timer"); return nil },
		Close: func(Conn, error) {},
	})
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { // replace a future one-shot with a 1ms interval, over and over
		defer wg.Done()
		for i := 0; i < 30000; i++ {
			_ = conn.SetWakeDeadline(time.Now().Add(time.Minute))
			_ = conn.SetWakeInterval(time.Millisecond)
		}
	}()
	go func() { // push turns through while the arms race
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				_ = conn.Wake()
				time.Sleep(50 * time.Microsecond)
			}
		}
	}()
	_ = conn.SetWakeInterval(time.Millisecond)
	time.Sleep(300 * time.Millisecond)
	close(stop)
	wg.Wait()
	// Whatever raced, a settled interval must keep firing.
	before := rec.count("timer")
	time.Sleep(150 * time.Millisecond)
	if after := rec.count("timer"); after <= before {
		t.Fatalf("interval stopped: %d fires before the check, %d after", before, after)
	}
}
