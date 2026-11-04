package uio

import (
	"errors"
	"net"
	"strings"
	"testing"
	"time"
)

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
