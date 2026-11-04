package uio

import (
	"errors"
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
				destination.state.Store(uint32(StateClosed))
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
	source.state.Store(uint32(StatePeerEnded))
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
		source.state.Store(uint32(StatePeerEnded))
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
	source.state.Store(uint32(StateClosed))
	pair.a.waiting.Store(true)
	bridge.hold(&pair.a)
	if pair.a.pause.Paused() {
		t.Fatal("a hold was installed after the source's transport closed")
	}
	source.state.Store(0)
	// OnClose claims the waiting mark: a hold racing it must not stay.
	pair.a.waiting.Store(false)
	bridge.hold(&pair.a)
	if pair.a.pause.Paused() || source.ReadsPaused() {
		t.Fatal("a hold outlived the drain or close that claimed its wait")
	}
}

func TestBridgeReportsTrafficAndServeLifecycle(t *testing.T) {
	var notifications []string
	events := &Events{}
	bridge := &Bridge{
		Observer: FuncHandler{BytesRead: func(_ Conn, n int) {
			if n != 11 {
				t.Error(n)
			}
			notifications = append(notifications, "read")
		}, BytesWritten: func(_ Conn, n int) {
			if n != 13 {
				t.Error(n)
			}
			notifications = append(notifications, "written")
		}},
		Hooks: FuncHandler{Start: func(ev *Events) {
			if ev != events {
				t.Error("wrong Events")
			}
			notifications = append(notifications, "start")
		}, Stop: func(ev *Events) {
			if ev != events {
				t.Error("wrong Events")
			}
			notifications = append(notifications, "stop")
		}},
	}
	bridge.OnStart(events)
	if bridge.Events() != events {
		t.Fatal("OnStart did not publish its Events")
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

type contractRawConn struct {
	fd  uintptr
	err error
}

func (r contractRawConn) Control(fn func(uintptr)) error {
	if r.err != nil {
		return r.err
	}
	fn(r.fd)
	return nil
}
func (contractRawConn) Read(func(uintptr) bool) error  { return ErrUnsupported }
func (contractRawConn) Write(func(uintptr) bool) error { return ErrUnsupported }
