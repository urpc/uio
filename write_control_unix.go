//go:build (linux || darwin || netbsd || freebsd || openbsd || dragonfly) && !stdio

package uio

import (
	"net"

	"github.com/urpc/uio/internal/bytebuf"
	"github.com/urpc/uio/internal/writecontrol"
)

func (conn *fdConn) currentWriteControl() *writecontrol.Control {
	if w := conn.wake.Load(); w != nil {
		return w.writeControl.Load()
	}
	return nil
}

func (conn *fdConn) ensureWriteControl() *writecontrol.Control {
	w := conn.wakeState()
	if c := w.writeControl.Load(); c != nil {
		return c
	}
	c := writecontrol.New(false)
	if w.writeControl.CompareAndSwap(nil, c) {
		return c
	}
	return w.writeControl.Load()
}

func (conn *fdConn) ReserveTurnWrite(n int) ([]byte, bool, error) {
	if n <= 0 || conn.isDatagram() || !conn.directOwner() || conn.scratch == nil || conn.turn&turnFlushing != 0 {
		return nil, false, nil
	}
	holder := conn.scratch
	if conn.outputClosed() {
		return nil, true, net.ErrClosed
	}
	took := !conn.turnHoldsWrite()
	if took && !conn.tryClaimWrite(writeTurnHeldFlag) {
		return nil, false, nil
	}
	reserved, err := conn.reservePendingAfterFlush(int64(n))
	if err == nil && !reserved {
		err = conn.errOutboundOverflow()
	}
	if err != nil {
		if took {
			conn.releaseWriteAndKick()
		}
		return nil, true, err
	}
	out := holder.out
	used := 0
	if out != nil {
		used = out.Len()
	}
	if conn.pending.Load() == int64(used+n) {
		if out == nil {
			out = bytebuf.AcquireBuffer(conn.coalesceBlockSize())
			holder.out = out
		}
		if out.Available() >= n {
			dst := out.AvailableBuffer()[:n:n]
			out.CommitWrite(n)
			return dst, true, nil
		}
		conn.handOverScratch()
	}
	conn.submitMu.Lock()
	dst := conn.outbound.Reserve(n, conn.coalesceBlockSize())
	conn.submitMu.Unlock()
	return dst, true, nil
}
