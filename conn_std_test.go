//go:build windows || stdio

package uio

import (
	"bytes"
	"errors"
	"io"
	"net"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

type stdDeadlineFailureConn struct {
	*stdRegistrationConn
	cause error
	calls []string
}

func (c *stdDeadlineFailureConn) SetDeadline(time.Time) error {
	c.calls = append(c.calls, "both")
	return c.cause
}
func (c *stdDeadlineFailureConn) SetReadDeadline(time.Time) error {
	c.calls = append(c.calls, "read")
	return c.cause
}
func (c *stdDeadlineFailureConn) SetWriteDeadline(time.Time) error {
	c.calls = append(c.calls, "write")
	return c.cause
}

type stdControlFailureConn struct {
	*stdRegistrationConn
	cause error
}

func (c *stdControlFailureConn) SyscallConn() (syscall.RawConn, error) { return nil, c.cause }

func TestStdFailedDeadlineDoesNotPublishASuccessfulTimer(t *testing.T) {
	cause := errors.New("transport rejected deadline")
	raw := &stdDeadlineFailureConn{stdRegistrationConn: newStdRegistrationConn(0), cause: cause}
	conn := &fdConn{conn: raw, readResume: make(chan struct{}, 1)}
	when := time.Now().Add(time.Hour)
	for _, set := range []func(time.Time) error{conn.SetDeadline, conn.SetReadDeadline, conn.SetWriteDeadline} {
		if err := set(when); !errors.Is(err, cause) {
			t.Fatalf("deadline = %v", err)
		}
	}
	if len(raw.calls) != 3 || raw.calls[0] != "both" || raw.calls[1] != "read" || raw.calls[2] != "write" {
		t.Fatalf("transport deadline calls = %v", raw.calls)
	}
	if !conn.readDeadline.IsZero() || !conn.writeDeadline.IsZero() || conn.writeTimer != nil || len(conn.readResume) != 0 {
		t.Fatal("a rejected deadline armed a timer or changed a held-read deadline")
	}
	if err := (&fdConn{}).SetDeadline(when); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("missing transport deadline = %v", err)
	}
	conn.closing.Store(true)
	if err := conn.SetReadDeadline(when); !errors.Is(err, net.ErrClosed) || len(raw.calls) != 3 {
		t.Fatal("closed connection changed transport deadline state")
	}
}

func TestStdDescriptorControlDoesNotCallUserCodeWithoutADescriptor(t *testing.T) {
	first, second := net.Pipe()
	defer first.Close()
	defer second.Close()
	cause := errors.New("syscall descriptor unavailable")
	cases := []struct {
		name string
		conn *fdConn
		want error
	}{
		{"missing-stream", &fdConn{}, net.ErrClosed},
		{"missing-packet", &fdConn{udp: &stdUDPState{}}, net.ErrClosed},
		{"custom-stream", &fdConn{conn: first}, ErrUnsupported},
		{"failed-syscall-connection", &fdConn{conn: &stdControlFailureConn{stdRegistrationConn: newStdRegistrationConn(0), cause: cause}}, cause},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			if err := tc.conn.Control(func(uintptr) { called = true }); !errors.Is(err, tc.want) || called {
				t.Fatalf("Control = %v, callback = %v", err, called)
			}
		})
	}
}

func TestStdPeerDeadlineOwnershipAndTerminalCancellation(t *testing.T) {
	peer := &fdConn{udp: &stdUDPState{svr: &fdConn{}}}
	if err := peer.SetDeadline(time.Time{}); err != nil || peer.udp.deadlines != nil {
		t.Fatal("clearing unused peer deadlines allocated timer state")
	}
	when := time.Now().Add(time.Hour)
	if err := peer.SetDeadline(when); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(peer.stopChildDeadlines)
	state := peer.udp.deadlines
	if state == nil || state.read.timer == nil || state.write.timer == nil {
		t.Fatal("peer read/write deadlines were not independently armed")
	}
	if err := peer.SetReadDeadline(when.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if state.read.at.Equal(state.write.at) {
		t.Fatal("replacing one peer deadline changed the other direction")
	}
	if err := peer.SetDeadline(time.Time{}); err != nil || !state.read.at.IsZero() || !state.write.at.IsZero() {
		t.Fatal("peer deadline cancellation did not clear both directions")
	}
	peer.stopChildDeadlines()
	if err := peer.SetWriteDeadline(when); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("stopped peer accepted a new deadline: %v", err)
	}
}

func TestStdQueuedStringCopiesDataAndKeepsClosedAdmission(t *testing.T) {
	conn := queuedStdConnection(t, 64)
	if n, err := conn.WriteString("string"); n != 6 || err != nil {
		t.Fatalf("WriteString = %d, %v", n, err)
	}
	if n, err := conn.WriteString(""); n != 0 || err != nil {
		t.Fatal("empty string was not a no-op")
	}
	var got bytes.Buffer
	_, _ = conn.outbound.WriteTo(&got)
	if got.String() != "string" {
		t.Fatalf("queued string = %q", got.String())
	}
	conn.closing.Store(true)
	if err := conn.WriteByte('x'); !errors.Is(err, net.ErrClosed) || conn.OutboundBuffered() != 0 {
		t.Fatal("closed byte write changed the queue")
	}
}

func TestStdFinalCallbackSeesTerminalStateAndCannotRestartIO(t *testing.T) {
	states := make(chan ConnState, 1)
	events := &Events{Pollers: 1, Handler: FuncHandler{Close: func(c Conn, _ error) { states <- c.State() }}}
	_, conn := readControlServe(t, events)
	if err := conn.CloseWith(nil); err != nil {
		t.Fatal(err)
	}
	if state := readControlAwait(t, states, "the final callback's state"); state&(StateClosing|StateClosed) != StateClosing|StateClosed {
		t.Fatalf("final connection state = %b", state)
	}
	if err := conn.Flush(); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("closed flush = %v", err)
	}
	if err := conn.YieldRead(); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("closed continuation = %v", err)
	}
	if err := conn.ReserveInbound(0); err != nil {
		t.Fatalf("empty input reservation = %v", err)
	}
}

func TestStdLargeWriteBudgetRejectsWholeInputBeforeCopyingIntoQueue(t *testing.T) {
	conn := queuedStdConnection(t, stdOwnedWriteThreshold)
	if n, err := conn.Write(make([]byte, stdOwnedWriteThreshold+1)); n != 0 || !errors.Is(err, ErrOutboundOverflow) {
		t.Fatalf("oversized write = %d, %v", n, err)
	}
	if _, err := conn.Write([]byte("accepted")); err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte{'x'}, stdOwnedWriteThreshold)
	if n, err := conn.Write(payload); n != 0 || !errors.Is(err, ErrOutboundOverflow) {
		t.Fatalf("large write beyond remaining capacity = %d, %v", n, err)
	}
	var got bytes.Buffer
	_, _ = conn.outbound.WriteTo(&got)
	if got.String() != "accepted" || !bytes.Equal(payload, bytes.Repeat([]byte{'x'}, len(payload))) {
		t.Fatal("refused large input changed the accepted prefix or caller data")
	}
}

func TestStdTimerErrorClosesBeforeAnEmptyReadCallback(t *testing.T) {
	cause := errors.New("application timer failed")
	closed := make(chan error, 1)
	var reads atomic.Int64
	events := &Events{Pollers: 1, Handler: FuncHandler{
		Timer: func(Conn) error { return cause },
		Read:  func(Conn) error { reads.Add(1); return nil },
		Close: func(_ Conn, err error) { closed <- err },
	}}
	_, conn := readControlServe(t, events)
	if err := conn.SetWakeDeadline(time.Now().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := readControlAwait(t, closed, "a failing timer's close"); !errors.Is(err, cause) || reads.Load() != 0 {
		t.Fatalf("timer close = %v; later read callbacks = %d", err, reads.Load())
	}
}

func TestStdEOFConsumerErrorIsTheTerminalCause(t *testing.T) {
	cause := errors.New("consumer rejected orderly end")
	closed := make(chan error, 1)
	events := &Events{Pollers: 1, AllowHalfOpen: true, Handler: FuncHandler{
		Read: func(c Conn) error {
			if _, err := c.Read(nil); errors.Is(err, io.EOF) {
				return cause
			}
			return nil
		},
		Close: func(_ Conn, err error) { closed <- err },
	}}
	peer, _ := readControlServe(t, events)
	if err := peer.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	if err := readControlAwait(t, closed, "the EOF consumer's failure"); !errors.Is(err, cause) {
		t.Fatalf("EOF close = %v", err)
	}
}

type stdHalfCloseFailureConn struct {
	*stdRegistrationConn
	cause  error
	closes atomic.Int64
}

func (c *stdHalfCloseFailureConn) CloseWrite() error { c.closes.Add(1); return c.cause }

func TestStdHalfCloseSupportAndTransportFailureRemainDistinct(t *testing.T) {
	if err := (&fdConn{conn: newStdRegistrationConn(0)}).CloseWrite(); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("custom transport without half-close = %v", err)
	}
	cause := errors.New("transport half-close failed")
	raw := &stdHalfCloseFailureConn{stdRegistrationConn: newStdRegistrationConn(30114), cause: cause}
	closed := make(chan error, 1)
	events := &Events{Pollers: 1, Handler: FuncHandler{
		Open:  func(Conn) { raw.openReturned.Store(true) },
		Close: func(_ Conn, err error) { closed <- err },
	}}
	done := startStdTestEvents(t, events)
	t.Cleanup(func() { stopStdTestEvents(t, events, done, nil) })
	conn, err := events.Adopt(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	if err := readControlAwait(t, closed, "failed transport half-close"); !errors.Is(err, cause) || raw.closes.Load() != 1 {
		t.Fatalf("half-close cause = %v, calls = %d", err, raw.closes.Load())
	}
}
