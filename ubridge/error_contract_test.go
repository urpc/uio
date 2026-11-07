package ubridge

import (
	"errors"
	"github.com/urpc/uio"
	"io"
	"net"
	"reflect"
	"testing"
)

// Bridge error-path contracts: retained input on failed reads, terminal
// peers, half-close failures, and pair ownership around a failed dial.

func TestBridgeReadFailureRetainsUnacceptedInput(t *testing.T) {
	failure := errors.New("destination write failed")
	for _, tc := range []struct {
		name       string
		writeError error
		closed     bool
		want       error
	}{
		{"live-destination-error", failure, false, failure},
		{"destination-close-return", net.ErrClosed, false, nil},
		{"destination-close-race", failure, true, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source, destination := newBridgeOwnedConn(nil), newBridgeOwnedConn(nil)
			bridge := &Bridge{}
			if err := bridge.Pair(source, destination); err != nil {
				t.Fatal(err)
			}
			source.setInbound("retained")
			destination.writeError = tc.writeError
			if tc.closed {
				destination.state.Store(uint32(uio.StateClosed))
			}
			if err := bridge.OnRead(source); !errors.Is(err, tc.want) {
				t.Fatalf("OnRead = %v, want %v", err, tc.want)
			}
			if source.InboundBuffered() != len("retained") || destination.writtenBytes.Load() != 0 {
				t.Fatal("a refused write consumed source bytes")
			}
		})
	}
}

func TestBridgeTerminalPeerDiscardsOnlyAfterSuccessfulConsumption(t *testing.T) {
	source, destination := newBridgeOwnedConn(nil), newBridgeOwnedConn(nil)
	bridge := &Bridge{}
	if err := bridge.Pair(source, destination); err != nil {
		t.Fatal(err)
	}
	bridge.OnClose(destination, io.EOF)
	source.setInbound("unforwardable")
	failure := errors.New("discard failed")
	source.discardError = failure
	if err := bridge.OnRead(source); !errors.Is(err, failure) || source.InboundBuffered() == 0 {
		t.Fatal("discard error was hidden or input lost")
	}
	source.discardError = nil
	source.state.Store(uint32(uio.StateReadEnded))
	if err := bridge.OnRead(source); err != nil {
		t.Fatal(err)
	}
	if source.InboundBuffered() != 0 || source.gracefulCloses.Load() != 1 {
		t.Fatal("orphan input/end did not complete gracefully")
	}
}

func TestBridgeHalfCloseFailureFollowsDestinationLifecycle(t *testing.T) {
	failure := errors.New("half-close failed")
	for _, closeError := range []error{failure, net.ErrClosed} {
		source, destination := newBridgeOwnedConn(nil), newBridgeOwnedConn(nil)
		bridge := &Bridge{}
		if err := bridge.Pair(source, destination); err != nil {
			t.Fatal(err)
		}
		source.state.Store(uint32(uio.StateReadEnded))
		destination.writeCloseError = closeError
		err := bridge.OnRead(source)
		if errors.Is(closeError, net.ErrClosed) {
			if err != nil {
				t.Fatal(err)
			}
		} else if !errors.Is(err, failure) {
			t.Fatalf("OnRead = %v", err)
		}
		if destination.writeCloses.Load() != 1 {
			t.Fatal("source end was not passed to its destination")
		}
	}
}

func TestBridgeEstablishedPairOpenAndLateHoldAreOwnerSafe(t *testing.T) {
	source, destination := newBridgeOwnedConn(nil), newBridgeOwnedConn(nil)
	bridge := &Bridge{InboundLimit: 8}
	if err := bridge.Pair(source, destination); err != nil {
		t.Fatal(err)
	}
	before := source.wakes.Load()
	bridge.OnOpen(source)
	if source.wakes.Load() != before+1 || source.defaultResumes.Load() != 0 {
		t.Fatal("paired OnOpen did not ask for its own turn")
	}
	pair := bridge.pairOf(source)
	source.state.Store(uint32(uio.StateClosed))
	pair.a.waiting.Store(true)
	bridge.hold(&pair.a)
	if pair.a.pause.Paused() {
		t.Fatal("a hold was installed after the source's transport closed")
	}
	source.state.Store(0)
	// OnClose claims the waiting mark: a hold racing it must not stay.
	pair.a.waiting.Store(false)
	bridge.hold(&pair.a)
	if pair.a.pause.Paused() || source.ReadPaused() {
		t.Fatal("a hold outlived the drain or close that claimed its wait")
	}
}

func TestBridgeReportsTrafficAndServeLifecycle(t *testing.T) {
	var notifications []string
	events := &uio.Events{}
	bridge := &Bridge{
		Observer: uio.FuncHandler{BytesRead: func(_ uio.Conn, n int) {
			if n != 11 {
				t.Error(n)
			}
			notifications = append(notifications, "read")
		}, BytesWritten: func(_ uio.Conn, n int) {
			if n != 13 {
				t.Error(n)
			}
			notifications = append(notifications, "written")
		}},
		Hooks: uio.FuncHandler{Start: func(ev *uio.Events) {
			if ev != events {
				t.Error("wrong uio.Events")
			}
			notifications = append(notifications, "start")
		}, Stop: func(ev *uio.Events) {
			if ev != events {
				t.Error("wrong uio.Events")
			}
			notifications = append(notifications, "stop")
		}},
	}
	bridge.OnStart(events)
	if bridge.Events() != events {
		t.Fatal("OnStart did not publish its uio.Events")
	}
	conn := newBridgeOwnedConn(nil)
	if err := bridge.OnTimer(conn); err != nil {
		t.Fatal(err)
	}
	bridge.OnBytesRead(conn, 11)
	bridge.OnBytesWritten(conn, 13)
	bridge.OnStop(events)
	if !reflect.DeepEqual(notifications, []string{"start", "read", "written", "stop"}) {
		t.Fatalf("notifications = %v", notifications)
	}
}

type bridgeDecorator struct {
	uio.Conn
}

func (d *bridgeDecorator) Unwrap() uio.Conn { return d.Conn }

func TestBridgePairUnwrapsDecorators(t *testing.T) {
	// Simple mocks that don't need deep integration
	a := newBridgeOwnedConn(nil)
	c := newBridgeOwnedConn(nil)
	da := &bridgeDecorator{Conn: a}
	dc := &bridgeDecorator{Conn: c}
	br := &Bridge{}
	if err := br.Pair(da, dc); err != nil {
		t.Fatal(err)
	}
	// Duplicate wrapped reference to an already-paired base connection is rejected.
	if err := br.Pair(&bridgeDecorator{Conn: a}, newBridgeOwnedConn(nil)); !errors.Is(err, ErrAlreadyPaired) {
		t.Fatalf("duplicate wrapped Pair = %v, want ErrAlreadyPaired", err)
	}
	br.OnClose(a, io.EOF)
	if br.pairOf(a) != nil {
		t.Fatal("closed leg stayed registered after OnClose")
	}
	br.OnClose(c, io.EOF)
	if br.pairOf(c) != nil {
		t.Fatal("partner leg stayed registered after OnClose")
	}
}

func TestBridgeStatsZeroAlloc(t *testing.T) {
	var nilBr *Bridge
	if nilBr.Stats() != (Stats{}) {
		t.Fatal("nil Bridge.Stats not zero")
	}
	a := newBridgeOwnedConn(nil)
	c := newBridgeOwnedConn(nil)
	br := &Bridge{}
	// Trigger DialFailures with missing upstream.
	br.OnOpen(newBridgeOwnedConn(nil))
	if err := br.Pair(a, c); err != nil {
		t.Fatal(err)
	}
	_ = br.Events()
	p := br.pairOf(a)
	_ = p.a.pause.Pause()
	_ = p.b.pause.Pause()
	st := br.Stats()
	if st.ActivePairs != 1 || st.ActiveLegs != 2 || st.PausedLegs != 2 || st.TotalPaired != 1 || st.DialFailures != 1 {
		t.Fatalf("Bridge.Stats = %+v", st)
	}
	_ = p.a.pause.Resume()
	_ = p.b.pause.Resume()
	if allocs := testing.AllocsPerRun(100, func() { _ = br.Stats() }); allocs != 0 {
		t.Fatalf("Bridge.Stats allocs = %v, want 0", allocs)
	}
	// Half-close one leg: ActiveLegs=1, ActivePairs=0 (closed=true).
	br.OnClose(a, io.EOF)
	if mid := br.Stats(); mid.ActiveLegs != 1 || mid.ActivePairs != 0 {
		t.Fatalf("half-closed Bridge.Stats = %+v", mid)
	}
	br.OnClose(c, io.EOF)
	if end := br.Stats(); end.ActiveLegs != 0 || end.ActivePairs != 0 || end.TotalPaired != 1 {
		t.Fatalf("closed Bridge.Stats = %+v", end)
	}
}
