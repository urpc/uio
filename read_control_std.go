//go:build windows || stdio

package uio

import (
	"io"

	"github.com/urpc/uio/internal/readcontrol"
)

// closeDeferredReadEOF ends a plain stream whose end of stream waited for
// its consumer — paused, owed a resume turn, or woken to finish unconsumed
// bytes — once that consumer ran unpaused and asked for nothing more. It runs
// under callbackMu.
func (conn *fdConn) closeDeferredReadEOF() {
	if conn.readEOF.Load() && conn.autoCloseOnEOF() && !conn.currentReadControl().Held() &&
		((conn.inbound.Empty() && len(conn.inboundTail) == 0) || conn.wakesPending.Load() == 0) {
		conn.requestClose(io.EOF)
	}
}

// endedBehind reports the peer's end of the stream, bytes still buffered
// included.
func (conn *fdConn) endedBehind() bool { return conn.readEOF.Load() }

// consumerStarts marks the consumer's callback beginning unpaused, clearing
// any owed resume turn.
func (conn *fdConn) consumerStarts() {
	conn.currentReadControl().ConsumerRuns()
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
