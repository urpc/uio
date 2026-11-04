package uio

import (
	"errors"
	"io"
	"net"
	"reflect"
	"strings"
	"testing"
	"time"
)

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

func TestDialLocalAddressAndControlValidation(t *testing.T) {
	for _, network := range []string{"tcp", "udp"} {
		if _, err := netDialerFor(network, dialConfig{localAddr: "127.0.0.1:65536"}); err == nil {
			t.Fatalf("%s accepted an invalid local port", network)
		}
	}
	if _, err := netDialerFor("ip:icmp", dialConfig{localAddr: "127.0.0.1"}); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("unsupported local network = %v", err)
	}
	dialer, err := netDialerFor("unix", dialConfig{localAddr: "unix://local.sock"})
	if err != nil || dialer.LocalAddr.(*net.UnixAddr).Name != "local.sock" {
		t.Fatalf("unix local address = %v, %v", dialer, err)
	}
	cause := errors.New("application control denied")
	calls := 0
	dialer, err = netDialerFor("tcp", dialConfig{control: func(network, address string, fd uintptr) error {
		calls++
		if network != "tcp" || address != "target" || fd != 17 {
			t.Error("control metadata changed")
		}
		return cause
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := dialer.Control("tcp", "target", contractRawConn{fd: 17}); !errors.Is(err, cause) || calls != 1 {
		t.Fatal("dial control error/metadata was not preserved")
	}
	rawCause := errors.New("descriptor unavailable")
	if err := rawControl(func(string, string, uintptr) error { t.Error("called application without a descriptor"); return nil })("tcp", "target", contractRawConn{err: rawCause}); !errors.Is(err, rawCause) {
		t.Fatalf("raw descriptor error = %v", err)
	}
	config := netListenConfigFor(true, func(string, string, uintptr) error {
		t.Error("application control ran after reuseport failed")
		return nil
	})
	if err := config.Control("tcp", "target", contractRawConn{fd: ^uintptr(0)}); err == nil {
		t.Fatal("reuseport accepted an invalid socket")
	}
}

func TestAddressCopyOnWriteAndInboundReservationPreserveData(t *testing.T) {
	if (&commonConn{}).LocalAddr() != nil {
		t.Fatal("unset local address must be nil")
	}
	local := &net.UnixAddr{Name: "listener", Net: "unix"}
	oldRemote := &net.UnixAddr{Name: "old", Net: "unix"}
	shared := &addrPair{local: local, remote: oldRemote}
	conn := &commonConn{addr: shared}
	conn.setRemoteAddr(nil)
	if conn.addr != shared {
		t.Fatal("nil remote changed address ownership")
	}
	remote := &net.UnixAddr{Name: "new", Net: "unix"}
	conn.setRemoteAddr(remote)
	if conn.addr == shared || shared.remote != oldRemote || conn.addr.local != local || conn.addr.remote != remote {
		t.Fatal("remote update mutated a shared listener address pair")
	}
	fresh := &commonConn{}
	fresh.setRemoteAddr(remote)
	if fresh.addr == nil || fresh.addr.remote != remote {
		t.Fatal("non-IP address was lost")
	}
	_, _ = fresh.inbound.Write([]byte("prefix"))
	fresh.inboundTail = []byte("tail")
	chunk := fresh.PeekChunk()
	if err := fresh.reserveInbound(3, 16); err != nil {
		t.Fatal(err)
	}
	if next := fresh.PeekChunk(); len(next) == 0 || &next[0] != &chunk[0] {
		t.Fatal("reserving an existing prefix replaced its borrowed chunk")
	}
	var got strings.Builder
	if _, err := fresh.WriteTo(&got); err != nil || got.String() != "prefixtail" {
		t.Fatalf("reserved input = %q, %v", got.String(), err)
	}
}

func TestCloseCounterWaitsForEveryInFlightClose(t *testing.T) {
	var counter closeCounter
	counter.add()
	counter.add()
	done := make(chan struct{})
	go func() { counter.wait(); close(done) }()
	waitFor(t, time.Second, func() bool { counter.mu.Lock(); defer counter.mu.Unlock(); return counter.cond != nil })
	counter.mu.Lock()
	installed := counter.cond != nil
	counter.mu.Unlock()
	if !installed {
		t.Fatal("close waiter was not installed")
	}
	counter.done()
	readControlQuiet(t, done, "lifecycle join with a close still in flight")
	counter.done()
	readControlAwait(t, done, "all close references released")
	counter.wait() // completed joins are also immediately reusable
}

func TestSentObserverAdapterPreservesReceiptAndConnection(t *testing.T) {
	conn := &contractTransport{}
	receipt := SendReceipt{wire: 23}
	var calls int
	var observer SentObserver = FuncHandler{Sent: func(got Conn, r SendReceipt) {
		calls++
		if got != conn || r != receipt {
			t.Fatal("SentObserver adapter changed its connection or receipt")
		}
	}}
	observer.OnSent(conn, receipt)
	observer = FuncHandler{}
	observer.OnSent(conn, receipt)
	if calls != 1 {
		t.Fatal("unset receipt callback was not a no-op")
	}
}
