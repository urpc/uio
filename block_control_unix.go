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
	if w.block.CompareAndSwap(nil, st) {
		return st, true
	}
	return w.block.Load(), true
}

func (conn *fdConn) enablePullRead(onTurn bool) error {
	conn.wakeState().pullRead.Store(true)
	if !onTurn && !conn.isClosing() && conn.loop != nil {
		conn.scheduleIO(ioEventWake)
	}
	return nil
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

func (conn *fdConn) hasWaitingBlockWriter() bool {
	if w := conn.wake.Load(); w != nil {
		return w.block.Load().HasWaitingWriter()
	}
	return false
}

func (conn *fdConn) ensureBlockWriter() *blockio.Writer {
	st, _ := conn.connBlockState()
	return st.Writer(0)
}
