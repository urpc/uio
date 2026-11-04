//go:build (linux || darwin || netbsd || freebsd || openbsd || dragonfly) && !stdio

package uio

import (
	"io"

	"github.com/urpc/uio/internal/readcontrol"
)

// closeDeferredReadEOF ends a plain stream whose end of stream waited for
// its consumer — paused, or owed a continuation it asked for with YieldRead
// — once that consumer ran unpaused and asked for nothing more. Layered
// streams end through their stack instead; see stack.ended.
func (conn *fdConn) closeDeferredReadEOF() {
	if conn.readEOF() && !conn.events().AllowHalfOpen && !conn.entry.layered && !conn.readStalled() &&
		!conn.currentReadControl().Held() {
		conn.requestClose(io.EOF)
	}
}

// endedBehind reports the peer's end of the stream, bytes still buffered
// included; the stack ends through it, truncation included. See
// socketEndedBehind.
func (conn *fdConn) endedBehind() bool { return conn.readEOF() }

// yieldOwed reports a continuation the consumer asked for with YieldRead and
// has not had yet.
func (conn *fdConn) yieldOwed() bool { return conn.readStalled() }

// consumerStarts is the stdio backend's continuation bookkeeping; a native
// turn keeps it in readStalled and consumerMayRun.
func (*fdConn) consumerStarts() {}

// consumerMayRun reports whether OnRead runs: a stack's layers decide for
// themselves; a plain connection's Handler runs unpaused, and when it does,
// it is the turn a released pause owed. A connection that never paused has
// no wake state to look at: the inlined fast path.
func (conn *fdConn) consumerMayRun() bool {
	return conn.wake.Load() == nil || conn.consumerMayRunControlled()
}

//go:noinline
func (conn *fdConn) consumerMayRunControlled() bool {
	if conn.entry.layered {
		return true
	}
	c := conn.currentReadControl()
	if c.ReadsPaused() {
		return false
	}
	c.ConsumerRuns()
	return true
}

func (conn *fdConn) currentReadControl() *readcontrol.Control {
	if w := conn.wake.Load(); w != nil {
		return w.readControl.Load()
	}
	return nil
}

func (conn *fdConn) ensureReadControl() *readcontrol.Control {
	w := conn.wakeState()
	if c := w.readControl.Load(); c != nil {
		return c
	}
	c := readcontrol.New(conn.checkReadControl, func() error {
		if err := conn.checkReadControl(); err != nil {
			return err
		}
		// Edges were dropped while paused: the resume turn hands over what
		// was retained, then reads the socket.
		conn.scheduleIO(ioEventResume)
		return nil
	})
	if w.readControl.CompareAndSwap(nil, c) {
		return c
	}
	return w.readControl.Load()
}

// inputMark is how far the consumer has moved its input. A continuation that
// leaves it unchanged moved nothing: it waits for bytes, and the socket is
// read for them.
type inputMark struct {
	buffered int    // the socket's unconsumed bytes
	moved    uint64 // reads out of a stack's deliveries
}

func (conn *fdConn) inputMark() inputMark {
	m := inputMark{buffered: conn.inbound.Len()}
	if conn.entry.layered {
		if c, ok := conn.Userdata().(*stack); ok {
			m.moved = c.moved
		}
	}
	return m
}
