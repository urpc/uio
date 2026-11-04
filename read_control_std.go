//go:build windows || stdio

package uio

import (
	"io"

	"github.com/urpc/uio/internal/readcontrol"
)

// closeDeferredReadEOF ends a plain stream whose end of stream waited for
// its consumer — paused, or owed a continuation it asked for with YieldRead
// — once that consumer ran unpaused and asked for nothing more. It runs
// under callbackMu. Layered streams end through their stack instead.
func (conn *fdConn) closeDeferredReadEOF() {
	if conn.readEOF.Load() && !conn.events().AllowHalfOpen && !conn.entry.layered && !conn.yieldPending.Load() &&
		!conn.currentReadControl().Held() {
		conn.requestClose(io.EOF)
	}
}

// endedBehind reports the peer's end of the stream, bytes still buffered
// included; the stack ends through it, truncation included. See
// socketEndedBehind.
func (conn *fdConn) endedBehind() bool { return conn.readEOF.Load() }

// yieldOwed reports a continuation the consumer asked for with YieldRead and
// has not had yet.
func (conn *fdConn) yieldOwed() bool { return conn.yieldPending.Load() }

// consumerStarts marks the consumer's callback beginning unpaused — the
// Handler's, or the stack's that runs it: it is the continuation an earlier
// YieldRead asked for and the turn a released pause owed.
func (conn *fdConn) consumerStarts() {
	conn.yieldPending.Store(false)
	conn.currentReadControl().ConsumerRuns()
}

// inputMark is how far the consumer has moved its input; a continuation that
// leaves it unchanged moved nothing.
type stdInputMark struct {
	buffered int
	moved    uint64
}

func (conn *fdConn) inputMark() stdInputMark {
	m := stdInputMark{buffered: conn.inbound.Len()}
	if conn.entry.layered {
		if c, ok := conn.Userdata().(*stack); ok {
			m.moved = c.moved
		}
	}
	return m
}

func (conn *fdConn) currentReadControl() *readcontrol.Control { return conn.readControl.Load() }

func (conn *fdConn) ensureReadControl() *readcontrol.Control {
	if c := conn.readControl.Load(); c != nil {
		return c
	}
	c := readcontrol.New(conn.checkReadControl, conn.Wake)
	if conn.readControl.CompareAndSwap(nil, c) {
		return c
	}
	return conn.readControl.Load()
}

func (conn *fdConn) signalReadResume() {
	select {
	case conn.readResume <- struct{}{}:
	default:
	}
}
