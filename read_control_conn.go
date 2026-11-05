package uio

import (
	"net"

	"github.com/urpc/uio/internal/readcontrol"
)

// inputReadControlGetter backs the package-private capability promoted by the
// zero-sized Access field. A private provider assertion also supports wrappers
// embedding a concrete connection without assuming source's dynamic type.
type inputReadControlGetter struct{}

type inputReadControlAccess = readcontrol.Access[inputReadControlGetter]

func (inputReadControlGetter) GetReadControl(source any) *readcontrol.Control {
	if boundary, ok := source.(interface{ inputReadControl() *readcontrol.Control }); ok {
		return boundary.inputReadControl()
	}
	return nil
}

// The socket's own control pauses its consumer: the Handler of a plain
// connection, the first layer of a stack. A layer node's control pauses the
// consumer of what that layer delivers — the layer above it, or the Handler
// at the top.

func (conn *fdConn) inputReadControl() *readcontrol.Control {
	if conn == nil || conn.isDatagram() {
		return nil
	}
	return conn.ensureReadControl()
}

func (l *layerNode) inputReadControl() *readcontrol.Control {
	if l == nil {
		return nil
	}
	return l.ensureReadControl()
}

func (s socketLower) inputReadControl() *readcontrol.Control {
	return readcontrol.ControlOf(s.Conn)
}

func (conn *fdConn) checkReadControl() error {
	if conn.isDatagram() {
		return ErrUnsupported
	}
	if conn.loop == nil || conn.isClosing() || conn.loop.stopping.Load() ||
		conn.entry == nil || conn.events().closing.Load() {
		return net.ErrClosed
	}
	return nil
}

func (conn *fdConn) PauseReads() error {
	if err := conn.checkReadControl(); err != nil {
		return err
	}
	return conn.ensureReadControl().PauseReads()
}

func (conn *fdConn) ResumeReads() error {
	if err := conn.checkReadControl(); err != nil {
		return err
	}
	if c := conn.currentReadControl(); c != nil {
		return c.ResumeReads()
	}
	return nil // never paused: nothing to release
}

// ReadsPaused reports a pause of the socket's consumer. Paused, a plain
// connection reads its socket no more and runs no OnRead; a stack reads on
// into its bounds while its first layer waits.
func (conn *fdConn) ReadsPaused() bool { return conn.currentReadControl().ReadsPaused() }

// readCloseReady reports whether a half-open connection whose two halves
// ended may close once the socket's buffer is empty: its consumer holds the
// end back while paused, owed a resume turn or a continuation it asked for,
// and a stack's layers must also have handed up everything they hold.
func (conn *fdConn) readCloseReady() bool {
	if conn.entry != nil && conn.entry.layered {
		if c, ok := conn.Userdata().(*stack); ok {
			return c.drained() && !c.handlerHeld() && !c.yieldOwed()
		}
	}
	return !conn.currentReadControl().Held() && !conn.yieldOwed()
}

// inputLimit is a producer capacity bound. An explicitly paused stack still
// needs a socket bound when its Events left the optional global limit off.
// Keep that default bound until a previous capacity hold has actually drained.
func (conn *fdConn) inputLimit(held bool) int {
	if limit := conn.events().MaxInboundBuffered; limit > 0 {
		return limit
	}
	if held {
		return DefaultDeliverBound
	}
	if conn.entry.layered {
		if c, ok := conn.Userdata().(*stack); ok && c.hasReadPause() {
			return DefaultDeliverBound
		}
	}
	return 0
}

func (l *layerNode) checkReadControl() error {
	if l.stack == nil || l.stack.over() {
		return net.ErrClosed
	}
	return nil
}

func (l *layerNode) ensureReadControl() *readcontrol.Control {
	if c := l.readControl.Load(); c != nil {
		return c
	}
	c := readcontrol.New(l.checkReadControl, l.Wake)
	if l.readControl.CompareAndSwap(nil, c) {
		return c
	}
	return l.readControl.Load()
}

func (l *layerNode) PauseReads() error {
	if err := l.checkReadControl(); err != nil {
		return err
	}
	return l.ensureReadControl().PauseReads()
}

func (l *layerNode) ResumeReads() error {
	if err := l.checkReadControl(); err != nil {
		return err
	}
	if c := l.readControl.Load(); c != nil {
		return c.ResumeReads()
	}
	return nil // never paused: nothing to release
}

func (l *layerNode) ReadsPaused() bool { return l.readControl.Load().ReadsPaused() }

func (c *stack) hasReadPause() bool {
	for _, layer := range c.layers {
		if layer.ReadsPaused() {
			return true
		}
	}
	return false
}
