//go:build windows || stdio

package uio

import "github.com/urpc/uio/internal/blockio"

func (conn *fdConn) connBlockState() (*blockio.State, bool) {
	if conn == nil || conn.isDatagram() || conn.internal {
		return nil, false
	}
	if st := conn.blockControl.Load(); st != nil {
		return st, true
	}
	st := blockio.NewState(fdBlockSource{conn: conn})
	if !conn.blockControl.CompareAndSwap(nil, st) {
		st = conn.blockControl.Load()
	}
	if conn.isClosing() || conn.draining.Load() {
		conn.mux.Lock()
		cause := conn.err
		if cause == nil {
			cause = conn.drainCause
		}
		conn.mux.Unlock()
		st.OnClose(conn.localClosed.Load(), cause)
	}
	return st, true
}

func (conn *fdConn) enablePullRead() (bool, error) {
	conn.pullRead.Store(true)
	onConnTurn := conn.directOwner()
	if !onConnTurn && !conn.isClosing() {
		_ = conn.Wake()
	}
	return onConnTurn, nil
}

func (conn *fdConn) hasPullReader() bool {
	return conn.pullRead.Load()
}

func (conn *fdConn) writeSideClosed() bool {
	if conn.isClosing() || conn.draining.Load() {
		return true
	}
	conn.mux.Lock()
	shut := conn.writeShut
	conn.mux.Unlock()
	return shut
}

func (conn *fdConn) closingOrDraining() bool {
	return conn.isClosing() || conn.draining.Load()
}

func (conn *fdConn) pumpBlockReader() {
	if st := conn.blockControl.Load(); st != nil {
		if r := st.ActiveReader(); r != nil {
			r.Pump()
		}
	}
}

func (conn *fdConn) notifyBlockClose(err error) {
	if st := conn.blockControl.Load(); st != nil {
		st.OnClose(conn.localClosed.Load(), err)
	}
}

func (conn *fdConn) notifyBlockWriteSpace() {
	if ctrl := conn.writeControl.Load(); ctrl != nil {
		ctrl.Notify(0)
	}
}

func (conn *fdConn) hasWaitingBlockWriter() bool {
	return conn.blockControl.Load().HasWaitingWriter()
}

func (conn *fdConn) ensureBlockWriter() *blockio.Writer {
	st, _ := conn.connBlockState()
	return st.Writer(0)
}
