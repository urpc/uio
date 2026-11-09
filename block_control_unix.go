//go:build (linux || darwin || netbsd || freebsd || openbsd || dragonfly) && !stdio

package uio

import "github.com/urpc/uio/internal/blockio"

func (conn *fdConn) connBlockState() (*blockio.State, bool) {
	if conn == nil || conn.isDatagram() || conn.internal {
		return nil, false
	}
	w := conn.wakeState()
	if st := w.block.Load(); st != nil {
		return st, true
	}
	st := blockio.NewState(fdBlockSource{conn: conn})
	if !w.block.CompareAndSwap(nil, st) {
		st = w.block.Load()
	}
	if conn.close.phase.Load() >= closeDraining {
		st.OnClose(conn.flags.Load()&localCloseFlag != 0, conn.peekDeferredCloseCause())
	}
	return st, true
}

func (conn *fdConn) enablePullRead() (bool, error) {
	conn.wakeState().pullRead.Store(true)
	onConnTurn := conn.directOwner()
	if !onConnTurn && !conn.isClosing() && conn.loop != nil {
		conn.scheduleIO(ioEventWake)
	}
	return onConnTurn, nil
}

func (conn *fdConn) hasPullReader() bool {
	if w := conn.wake.Load(); w != nil {
		return w.pullRead.Load()
	}
	return false
}

func (conn *fdConn) writeSideClosed() bool {
	return conn.outputClosed()
}

func (conn *fdConn) closingOrDraining() bool {
	return conn.close.phase.Load() >= closeDraining
}

func (conn *fdConn) pumpBlockReader() {
	if w := conn.wake.Load(); w != nil {
		if st := w.block.Load(); st != nil {
			if r := st.ActiveReader(); r != nil {
				r.Pump()
			}
		}
	}
}

func (conn *fdConn) notifyBlockClose(err error) {
	if w := conn.wake.Load(); w != nil {
		if st := w.block.Load(); st != nil {
			local := conn.flags.Load()&localCloseFlag != 0
			st.OnClose(local, err)
		}
	}
}

func (conn *fdConn) notifyBlockWriteSpace() {
	if w := conn.wake.Load(); w != nil {
		if ctrl := w.writeControl.Load(); ctrl != nil {
			ctrl.Notify(0)
		}
	}
}

func (conn *fdConn) ensureBlockWriter() *blockio.Writer {
	st, _ := conn.connBlockState()
	return st.Writer(0)
}

func (conn *fdConn) hasBlockWriter() bool {
	if w := conn.wake.Load(); w != nil {
		if st := w.block.Load(); st != nil {
			return st.HasWriter()
		}
	}
	return false
}
