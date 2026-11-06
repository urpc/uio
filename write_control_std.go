//go:build windows || stdio

package uio

import "github.com/urpc/uio/internal/writecontrol"

func (conn *fdConn) currentWriteControl() *writecontrol.Control {
	return conn.writeControl.Load()
}

func (conn *fdConn) ensureWriteControl() *writecontrol.Control {
	if c := conn.writeControl.Load(); c != nil {
		return c
	}
	c := writecontrol.New(false)
	if conn.writeControl.CompareAndSwap(nil, c) {
		return c
	}
	return conn.writeControl.Load()
}
