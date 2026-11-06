package uio

import "github.com/urpc/uio/internal/writecontrol"

// writeControlGetter backs the package-private capability promoted by the
// zero-sized writeControlAccess field.
type writeControlGetter struct{}

type writeControlAccess = writecontrol.Access[writeControlGetter]

func (writeControlGetter) GetWriteControl(source any) *writecontrol.Control {
	if boundary, ok := source.(interface{ connWriteControl() *writecontrol.Control }); ok {
		return boundary.connWriteControl()
	}
	return nil
}

func (conn *fdConn) connWriteControl() *writecontrol.Control {
	if conn == nil {
		return nil
	}
	return conn.ensureWriteControl()
}

func (conn *fdConn) hasWriteControl() bool {
	return conn.currentWriteControl() != nil
}

func (l *layerNode) connWriteControl() *writecontrol.Control {
	if l == nil {
		return nil
	}
	return l.ensureWriteControl()
}

func (l *layerNode) ensureWriteControl() *writecontrol.Control {
	if c := l.writeControl.Load(); c != nil {
		return c
	}
	inexact := l.stack.firstHeld >= 0 && l.stack.firstHeld <= l.index
	c := writecontrol.New(inexact)
	if !l.writeControl.CompareAndSwap(nil, c) {
		c = l.writeControl.Load()
	}
	l.stack.enableWriteWatch()
	return c
}

func (s socketLower) connWriteControl() *writecontrol.Control {
	return writecontrol.ControlOf(s.Conn)
}
