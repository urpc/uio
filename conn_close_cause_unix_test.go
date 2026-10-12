//go:build (linux || darwin || netbsd || freebsd || openbsd || dragonfly) && !stdio

package uio

import (
	"errors"
	"io"
	"syscall"
	"testing"
)

// A close with one cause reports that cause itself, not a join wrapping it.
func TestJoinCloseErrorsKeepsALoneCause(t *testing.T) {
	cause := errors.New("cause")
	for _, errs := range [][3]error{{cause, nil, nil}, {nil, cause, nil}, {nil, nil, cause}} {
		if got := joinCloseErrors(errs[0], errs[1], errs[2]); got != cause {
			t.Fatalf("joinCloseErrors(%v) = %v, want the cause itself", errs, got)
		}
	}
	if joinCloseErrors(nil, nil, nil) != nil {
		t.Fatal("no cause joined into an error")
	}
	other := errors.New("other")
	joined := joinCloseErrors(cause, nil, other)
	if !errors.Is(joined, cause) || !errors.Is(joined, other) {
		t.Fatalf("joinCloseErrors with two causes = %v, want both", joined)
	}
}

// The shared requests for common causes are never modified: a later cause
// replaces the request instead.
func TestCloseRequestsForCommonCausesAreShared(t *testing.T) {
	for _, cause := range []error{nil, io.EOF, syscall.ECONNRESET} {
		if newCloseRequest(cause) != newCloseRequest(cause) {
			t.Fatalf("requests for %v are allocated per close", cause)
		}
	}
	var conn fdConn
	conn.setDeferredCloseLocked(nil)
	shared := conn.close.deferred
	cause := errors.New("later")
	conn.setDeferredCloseLocked(cause)
	if shared.cause != nil || conn.close.deferred == shared || conn.takeDeferredCloseCause() != cause {
		t.Fatal("recording a later cause modified the shared request")
	}
	conn.setDeferredCloseLocked(io.EOF)
	conn.setDeferredCloseLocked(errors.New("second"))
	if got := conn.takeDeferredCloseCause(); got != io.EOF || closeAtEOF.cause != io.EOF {
		t.Fatalf("the first cause was replaced: %v", got)
	}
}
