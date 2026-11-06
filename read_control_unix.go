//go:build (linux || darwin || netbsd || freebsd || openbsd || dragonfly) && !stdio

package uio

import (
	"io"

	"github.com/urpc/uio/internal/readcontrol"
)

// closeDeferredReadEOF ends a plain stream whose end of stream waited for
// its consumer — paused, owed a resume turn, or woken to finish unconsumed
// bytes — once that consumer ran unpaused and asked for nothing more.
func (conn *fdConn) closeDeferredReadEOF() {
	if conn.readEOF() && conn.autoCloseOnEOF() && !conn.currentReadControl().Held() &&
		(conn.readEnded() || conn.taskState.Load()&ioEventWake == 0) {
		conn.requestClose(io.EOF)
	}
}

// endedBehind reports the peer's end of the stream, bytes still buffered
// included.
func (conn *fdConn) endedBehind() bool { return conn.readEOF() }

// consumerStarts is the stdio backend's resume-turn bookkeeping; a native
// turn clears it in consumerMayRun.
func (*fdConn) consumerStarts() {}

// consumerMayRun reports whether OnRead runs: a non-direct driver decides for
// itself; a direct connection's Handler runs unpaused, and when it does,
// it is the turn a released pause owed. A connection that never paused has
// no wake state to look at: the inlined fast path.
func (conn *fdConn) consumerMayRun() bool {
	return conn.wake.Load() == nil || conn.consumerMayRunControlled()
}

//go:noinline
func (conn *fdConn) consumerMayRunControlled() bool {
	if !conn.entry.directConsumer {
		return true
	}
	c := conn.currentReadControl()
	if c.ReadPaused() {
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
