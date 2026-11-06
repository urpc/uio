package uio

import (
	"net"

	"github.com/urpc/uio/internal/readcontrol"
)

// readControlGetter backs the package-private capability promoted by the
// zero-sized readControlAccess field.
type readControlGetter struct{}

type readControlAccess = readcontrol.Access[readControlGetter]

func (readControlGetter) GetReadControl(source any) *readcontrol.Control {
	if boundary, ok := source.(interface{ connReadControl() *readcontrol.Control }); ok {
		return boundary.connReadControl()
	}
	return nil
}

// The socket's own control pauses its direct consumer: the Handler of a plain
// connection, the first layer of a stack. A layer node's control pauses the
// consumer above it — the next layer, or the Handler at the top.

func (conn *fdConn) connReadControl() *readcontrol.Control {
	if conn == nil || conn.isDatagram() {
		return nil
	}
	return conn.ensureReadControl()
}

func (l *layerNode) connReadControl() *readcontrol.Control {
	if l == nil {
		return nil
	}
	return l.ensureReadControl()
}

func (s socketLower) connReadControl() *readcontrol.Control {
	return readcontrol.ControlOf(s.Conn)
}

func (conn *fdConn) checkReadControl() error {
	if conn.isDatagram() {
		return ErrUnsupported
	}
	if conn.loop == nil || conn.closingOrDraining() || conn.loop.stopping.Load() ||
		conn.entry == nil || conn.events().closing.Load() {
		return net.ErrClosed
	}
	return nil
}

func (conn *fdConn) PauseRead() error {
	if err := conn.checkReadControl(); err != nil {
		return err
	}
	return conn.ensureReadControl().PauseRead()
}

func (conn *fdConn) ResumeRead() error {
	if err := conn.checkReadControl(); err != nil {
		return err
	}
	if c := conn.currentReadControl(); c != nil {
		return c.ResumeRead()
	}
	return nil // never paused: nothing to release
}

// ReadPaused reports a pause of the socket's consumer. Paused, a plain
// connection reads its socket no more and runs no OnRead; a stack reads on
// into its bounds while its first layer waits.
func (conn *fdConn) ReadPaused() bool { return conn.currentReadControl().ReadPaused() }

// readCloseReady reports whether a half-open connection whose two halves
// ended may close once the socket's buffer is empty: its consumer holds the
// end back while paused or owed a resume turn.
func (conn *fdConn) readCloseReady() bool {
	return !conn.currentReadControl().Held()
}

// inputLimit is a producer capacity bound. A non-direct consumer (such as a
// protocol stack) still bounds retained socket input at DefaultDeliverBound
// when its Events left the optional global limit off.
func (conn *fdConn) inputLimit(held bool) int {
	if limit := conn.events().MaxInboundBuffered; limit > 0 {
		return limit
	}
	if held || (conn.entry != nil && !conn.entry.directConsumer) {
		return DefaultDeliverBound
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

func (l *layerNode) PauseRead() error {
	if err := l.checkReadControl(); err != nil {
		return err
	}
	return l.ensureReadControl().PauseRead()
}

func (l *layerNode) ResumeRead() error {
	if err := l.checkReadControl(); err != nil {
		return err
	}
	if c := l.readControl.Load(); c != nil {
		return c.ResumeRead()
	}
	return nil // never paused: nothing to release
}

func (l *layerNode) ReadPaused() bool { return l.readControl.Load().ReadPaused() }
