//go:build (linux || darwin || netbsd || freebsd || openbsd || dragonfly) && !stdio

package uio

import (
	"net"

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
	if n <= 0 || conn.isDatagram() || conn.ioOwner.Load() != currentGoroutineID() || conn.scratch == nil || conn.turn&turnFlushing != 0 {
		return nil, false, nil
	}
	if conn.outputClosed() {
		return nil, true, net.ErrClosed
	}
	if !conn.turnHoldsWrite() && !conn.tryClaimWrite(writeTurnHeldFlag) {
		return nil, false, nil
	}
	out, _, err := conn.prepareScratch(n)
	if err != nil {
		return nil, true, err
	}
	if out != nil {
		dst := out.AvailableBuffer()[:n:n]
		out.CommitWrite(n)
		return dst, true, nil
	}
	conn.submitMu.Lock()
	dst := conn.outbound.Reserve(n, conn.coalesceBlockSize())
	conn.submitMu.Unlock()
	return dst, true, nil
}
