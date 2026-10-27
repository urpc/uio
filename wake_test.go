//go:build linux || darwin || netbsd || freebsd || openbsd || dragonfly || stdio

package uio

import (
	"errors"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// wakeRecorder counts OnData turns and records when they ran.
type wakeRecorder struct {
	mu     sync.Mutex
	times  []time.Time
	opened atomic.Int32
	closed atomic.Int32
}

func (r *wakeRecorder) OnOpen(Conn) { r.opened.Add(1) }

func (r *wakeRecorder) OnData(Conn) error {
	r.mu.Lock()
	r.times = append(r.times, time.Now())
	r.mu.Unlock()
	return nil
}

func (r *wakeRecorder) OnClose(Conn, error) { r.closed.Add(1) }

func (r *wakeRecorder) fires() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.times)
}

func (r *wakeRecorder) firstAt() time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.times[0]
}

// dialWake dials a sink and returns its recorder with the connection: wake
// tests need one settled connection, not a server.
func dialWake(t *testing.T) (*wakeRecorder, Conn) {
	t.Helper()
	rec := &wakeRecorder{}
	started := make(chan struct{})
	events := &Events{Pollers: 1, Handler: rec, Hooks: FuncHandler{
		Start: func(*Events) { close(started) },
	}}
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
	return rec, conn
}

// waitFor polls cond until it holds or the timeout passes.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// A wake deadline fires one OnData near its time.
func TestWakeDeadlineFires(t *testing.T) {
	rec, conn := dialWake(t)
	start := time.Now()
	if err := conn.SetWakeDeadline(start.Add(60 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 3*time.Second, func() bool { return rec.fires() >= 1 })
	if rec.fires() != 1 {
		t.Fatalf("one-shot fired %d times", rec.fires())
	}
	if late := firstSince(t, rec, start.Add(60*time.Millisecond)); late > 100*time.Millisecond {
		t.Fatalf("wake fired %v late", late)
	} else if late < -20*time.Millisecond {
		t.Fatalf("wake fired %v early", -late)
	}
}

// A cancelled deadline does not fire.
func TestWakeDeadlineCancel(t *testing.T) {
	rec, conn := dialWake(t)
	if err := conn.SetWakeDeadline(time.Now().Add(80 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if err := conn.SetWakeDeadline(time.Time{}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(250 * time.Millisecond)
	if got := rec.fires(); got != 0 {
		t.Fatalf("a cancelled deadline fired %d times", got)
	}
}

// Re-arming replaces the deadline: the first never fires.
func TestWakeDeadlineRearm(t *testing.T) {
	rec, conn := dialWake(t)
	start := time.Now()
	if err := conn.SetWakeDeadline(start.Add(60 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if err := conn.SetWakeDeadline(start.Add(400 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 3*time.Second, func() bool { return rec.fires() >= 1 })
	if rec.fires() != 1 {
		t.Fatalf("re-armed one-shot fired %d times", rec.fires())
	}
	if late := firstSince(t, rec, start.Add(400*time.Millisecond)); late < 0 {
		t.Fatal("the replaced, earlier deadline fired")
	}
}

// An interval fires repeatedly until cleared.
func TestWakeIntervalFiresRepeatedly(t *testing.T) {
	rec, conn := dialWake(t)
	if err := conn.SetWakeInterval(40 * time.Millisecond); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 3*time.Second, func() bool { return rec.fires() >= 3 })
	if rec.fires() < 3 {
		t.Fatalf("interval fired %d times in 3 s, want at least 3", rec.fires())
	}
	if err := conn.SetWakeInterval(0); err != nil {
		t.Fatal(err)
	}
	fires := rec.fires()
	time.Sleep(200 * time.Millisecond)
	if got := rec.fires(); got != fires {
		t.Fatalf("cleared interval kept firing: %d then %d", fires, got)
	}
}

// A deadline after close reports closed and fires nothing.
func TestWakeAfterClose(t *testing.T) {
	rec, conn := dialWake(t)
	_ = conn.Close()
	if err := conn.SetWakeDeadline(time.Now().Add(50 * time.Millisecond)); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("SetWakeDeadline after Close = %v, want net.ErrClosed", err)
	}
	time.Sleep(120 * time.Millisecond)
	if got := rec.fires(); got != 0 {
		t.Fatalf("a deadline armed after close fired %d times", got)
	}
}

// The 50-way campaign: setters race close and wake turns from outside.
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
			// Close requests the close and returns: a turn queued before it
			// may still run, as on the stdio backend. Count from OnClose on.
			waitFor(t, 3*time.Second, func() bool { return rec.closed.Load() > 0 })
			fires := rec.fires()
			time.Sleep(50 * time.Millisecond)
			if got := rec.fires(); got > fires {
				t.Fatalf("a closed connection kept waking: %d then %d", fires, got)
			}
		}()
	}
}

// firstSince reports how long after when the first recorded turn ran,
// negative when it ran before.
func firstSince(t *testing.T, rec *wakeRecorder, when time.Time) time.Duration {
	t.Helper()
	waitFor(t, 3*time.Second, func() bool { return rec.fires() >= 1 })
	if rec.fires() == 0 {
		t.Fatal("no OnData fired")
	}
	return rec.firstAt().Sub(when)
}

// A deadline inside the scheduler's tick granularity still fires: the wheel
// reports entries parked in the current tick, and the armed kernel timer
// retries at a finer granularity rather than disarming.
func TestWakeSubTickDeadlineFires(t *testing.T) {
	rec, conn := dialWake(t)
	if err := conn.SetWakeDeadline(time.Now().Add(time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 2*time.Second, func() bool { return rec.fires() >= 1 })
	if got := rec.fires(); got < 1 {
		t.Fatal("a sub-tick deadline never fired")
	} else if got > 1 {
		t.Fatalf("a sub-tick one-shot fired %d times", got)
	}
}

// A sub-tick interval keeps firing: each expiry re-arms within the tick.
func TestWakeSubTickIntervalFires(t *testing.T) {
	rec, conn := dialWake(t)
	if err := conn.SetWakeInterval(time.Millisecond); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 2*time.Second, func() bool { return rec.fires() >= 5 })
	if got := rec.fires(); got < 5 {
		t.Fatalf("a 1ms interval fired %d times in 2s, want at least 5", got)
	}
}

// A periodic timer stops with the connection: after close, the fires settle.
func TestWakeIntervalStopsAtClose(t *testing.T) {
	rec, conn := dialWake(t)
	if err := conn.SetWakeInterval(5 * time.Millisecond); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 2*time.Second, func() bool { return rec.fires() >= 2 })
	_ = conn.Close()
	fires := rec.fires()
	time.Sleep(150 * time.Millisecond)
	if got := rec.fires(); got > fires {
		t.Fatalf("a closed connection's interval kept firing: %d then %d", fires, got)
	}
}

// A datagram server's child wake goes through the server's own turn, like
// every other wake: the child's OnData runs, and under -race concurrent
// delivery against the server's reads would be caught.
func TestWakeUDPChildFiresInServerTurn(t *testing.T) {
	type record struct {
		mu    sync.Mutex
		fires int
		opens int
	}
	rec := &record{}
	events := &Events{Pollers: 1, Handler: FuncHandler{
		Open:  func(Conn) { rec.mu.Lock(); rec.opens++; rec.mu.Unlock() },
		Data:  func(Conn) error { rec.mu.Lock(); rec.fires++; rec.mu.Unlock(); return nil },
		Close: func(Conn, error) {},
	}}
	started := make(chan struct{})
	events.Hooks = FuncHandler{Start: func(*Events) { close(started) }}
	done := make(chan error, 1)
	go func() { done <- events.Serve() }()
	t.Cleanup(func() { _ = events.Close(nil); <-done })
	<-started

	listener, err := events.Listen("udp://127.0.0.1:0", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	packet, ok := listener.(PacketListener)
	if !ok {
		t.Fatal("a udp listener is not a PacketListener")
	}
	// The peer needs a real remote address; a bound throwaway socket is one.
	far, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = far.Close() })
	raddr, err := netip.ParseAddrPort(far.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	peer, err := packet.DialPeer(raddr, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := peer.SetWakeInterval(5 * time.Millisecond); err != nil {
		t.Fatal(err)
	}
	// Datagrams from the far end race the wake interval: both deliver in the
	// server's turn, and -race hears any overlap.
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := far.WriteTo([]byte("tick"), listener.Addr()); err != nil {
				return
			}
			time.Sleep(3 * time.Millisecond)
		}
	}()
	defer close(stop)
	waitFor(t, 3*time.Second, func() bool {
		rec.mu.Lock()
		defer rec.mu.Unlock()
		return rec.fires >= 3
	})
	_ = peer.Close()
}

// A deadline in the past fires on the next turn, not never.
func TestWakePastDeadlineFires(t *testing.T) {
	rec, conn := dialWake(t)
	if err := conn.SetWakeDeadline(time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 2*time.Second, func() bool { return rec.fires() >= 1 })
	if got := rec.fires(); got < 1 {
		t.Fatal("a past deadline never fired")
	} else if got > 1 {
		t.Fatalf("a past one-shot fired %d times", got)
	}
}
