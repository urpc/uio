//go:build (linux || darwin || netbsd || freebsd || openbsd || dragonfly) && !stdio

package uio

import (
	"io"
	"net"
	"syscall"
	"unsafe"

	"github.com/urpc/uio/internal/bytebuf"
	"github.com/urpc/uio/internal/poller"
	"github.com/urpc/uio/internal/socket"
)

func (conn *fdConn) WriteByte(value byte) error {
	var data [1]byte
	data[0] = value
	_, err := conn.Write(data[:])
	return err
}

func (conn *fdConn) WriteString(value string) (int, error) {
	data := unsafe.Slice(unsafe.StringData(value), len(value))
	return conn.Write(data)
}

func (conn *fdConn) Write(data []byte) (int, error) {
	if len(data) == 0 {
		return 0, nil
	}
	if conn.isClosing() {
		return 0, net.ErrClosed
	}
	// queuedWrites prevents a callback write from overtaking accepted external
	// writes that the loop has not consumed yet.
	if conn.loop.inLoop() && conn.queuedWrites.Load() == 0 {
		return conn.writeOnLoop(data)
	}
	// This fast rejection belongs after the direct path: data sent straight to
	// the kernel never counts against the user-space payload limit.
	if limit := conn.events.MaxOutboundBuffered; limit > 0 && len(data) > limit {
		return 0, ErrOutboundOverflow
	}
	if err := conn.precheckQueuedWrite(len(data)); err != nil {
		return 0, err
	}
	owned := bytebuf.CloneBuffer(data)
	return conn.queueOwnedWrite(owned, len(data))
}

func (conn *fdConn) Writev(vec [][]byte) (int, error) {
	total := 0
	for _, segment := range vec {
		if len(segment) > int(^uint(0)>>1)-total {
			return 0, ErrOutboundOverflow
		}
		total += len(segment)
	}
	if total == 0 {
		return 0, nil
	}
	if conn.isClosing() {
		return 0, net.ErrClosed
	}
	if conn.loop.inLoop() && conn.queuedWrites.Load() == 0 {
		return conn.writevOnLoop(vec, total)
	}
	if limit := conn.events.MaxOutboundBuffered; limit > 0 && total > limit {
		return 0, ErrOutboundOverflow
	}
	if err := conn.precheckQueuedWrite(total); err != nil {
		return 0, err
	}
	owned := bytebuf.CloneBuffers(vec, total)
	return conn.queueOwnedWrite(owned, total)
}

func (conn *fdConn) precheckQueuedWrite(size int) error {
	// queueOwnedWrite repeats these checks after cloning to close races.
	if limit := conn.events.MaxPendingWrites; limit > 0 && conn.queuedWrites.Load() >= int64(limit) {
		return ErrTaskQueueFull
	}
	if limit := int64(conn.events.MaxOutboundBuffered); limit > 0 {
		pending := conn.pending.Load()
		if int64(size) > limit-pending {
			return ErrOutboundOverflow
		}
	}
	return nil
}

func (conn *fdConn) queueOwnedWrite(owned *bytebuf.Buffer, size int) (int, error) {
	if limit := conn.events.MaxOutboundBuffered; limit > 0 && size > limit {
		bytebuf.ReleaseBuffer(owned)
		return 0, ErrOutboundOverflow
	}
	// Allocation and the only payload copy have already happened off-lock.
	t := acquireTask(writeTask, conn)
	t.buf = owned
	conn.submitMu.Lock()
	if conn.closing.Load() || conn.events.closing.Load() || conn.loop.stopping.Load() {
		conn.submitMu.Unlock()
		releaseTask(t)
		return 0, net.ErrClosed
	}
	if conn.queuedWrites.Load() >= int64(conn.events.MaxPendingWrites) {
		conn.submitMu.Unlock()
		releaseTask(t)
		return 0, ErrTaskQueueFull
	}
	if !conn.reservePending(int64(size)) {
		conn.submitMu.Unlock()
		releaseTask(t)
		return 0, ErrOutboundOverflow
	}
	conn.queuedWrites.Add(1)
	if !conn.loop.pushTask(t) {
		conn.queuedWrites.Add(-1)
		conn.pending.Add(-int64(size))
		conn.submitMu.Unlock()
		releaseTask(t)
		return 0, net.ErrClosed
	}
	conn.submitMu.Unlock()
	conn.loop.notify()
	return size, nil
}

func (conn *fdConn) reservePending(size int64) bool {
	// Both callback partial writes and external producers reserve this counter.
	for {
		old := conn.pending.Load()
		limit := int64(conn.events.MaxOutboundBuffered)
		if limit > 0 && size > limit-old {
			return false
		}
		if conn.pending.CompareAndSwap(old, old+size) {
			return true
		}
	}
}

func (conn *fdConn) writeOnLoop(data []byte) (int, error) {
	if conn.isUDP {
		return conn.sendUDPOnLoop(data)
	}
	threshold := conn.events.WriteBufferedThreshold
	if !conn.outbound.Empty() || (threshold > 0 && len(data) < threshold) {
		// Batching or an existing tail requires one copy into loop-owned storage.
		if !conn.reservePending(int64(len(data))) {
			return 0, ErrOutboundOverflow
		}
		_, _ = conn.outbound.Write(data)
		return len(data), nil
	}

	// The common callback path lends caller memory directly to the kernel.
	written, err := syscall.Write(conn.fd, data)
	if written < 0 {
		written = 0
	}
	if err != nil {
		if isWouldBlock(err) {
			written, err = 0, nil
		} else {
			conn.requestClose(err)
			return written, err
		}
	}
	conn.events.onSocketBytesWrite(conn, written)
	if written == len(data) {
		return written, nil
	}
	remaining := data[written:]
	if !conn.reservePending(int64(len(remaining))) {
		return written, ErrOutboundOverflow
	}
	// Only the unsent suffix must survive after Write returns.
	_, _ = conn.outbound.Write(remaining)
	conn.writeBlocked = written == 0
	return len(data), nil
}

func (conn *fdConn) writevOnLoop(vec [][]byte, total int) (int, error) {
	if conn.isUDP {
		return 0, errUnsupported
	}
	threshold := conn.events.WriteBufferedThreshold
	if !conn.outbound.Empty() || (threshold > 0 && total < threshold) {
		if !conn.reservePending(int64(total)) {
			return 0, ErrOutboundOverflow
		}
		_, _ = conn.outbound.Writev(vec)
		return total, nil
	}
	written, err := socket.Writev(conn.fd, vec)
	if written < 0 {
		written = 0
	}
	if err != nil {
		if isWouldBlock(err) {
			written, err = 0, nil
		} else {
			conn.requestClose(err)
			return written, err
		}
	}
	conn.events.onSocketBytesWrite(conn, written)
	if written == total {
		return written, nil
	}
	remaining := total - written
	if !conn.reservePending(int64(remaining)) {
		return written, ErrOutboundOverflow
	}
	owned := bytebuf.CloneBuffersFrom(vec, written, remaining)
	conn.outbound.AppendOwned(owned)
	conn.writeBlocked = written == 0
	return total, nil
}

func (conn *fdConn) sendUDPOnLoop(data []byte) (written int, err error) {
	if conn.rUDPAddr == nil {
		written, err = syscall.Write(conn.fd, data)
	} else {
		err = syscall.Sendto(conn.fd, data, 0, conn.rUDPAddr)
		if err == nil {
			written = len(data)
		}
	}
	if written < 0 {
		written = 0
	}
	conn.events.onSocketBytesWrite(conn, written)
	if err != nil {
		if isUDPSendBlocked(err) {
			return written, err
		}
		conn.requestClose(err)
		return written, err
	}
	if written != len(data) {
		// Datagram boundaries are atomic; never retry a partial suffix as a stream.
		err = io.ErrShortWrite
		conn.requestClose(err)
		return written, err
	}
	return written, nil
}

func isUDPSendBlocked(err error) bool {
	return isWouldBlock(err) || err == syscall.ENOBUFS
}

func (conn *fdConn) runWriteTask(t *task) {
	conn.queuedWrites.Add(-1)
	size := t.buf.Len()
	if conn.closed {
		conn.pending.Add(-int64(size))
		return
	}
	if conn.isUDP {
		data := t.buf.Bytes()
		written, err := conn.sendUDPOnLoop(data)
		conn.pending.Add(-int64(written))
		if err != nil {
			conn.requestClose(err)
		}
		return
	}
	// AppendOwned transfers the producer buffer without another payload copy.
	conn.outbound.AppendOwned(t.buf)
	t.buf = nil
	conn.loop.touch(conn)
}

func (conn *fdConn) Flush() error {
	if conn.isClosing() {
		return net.ErrClosed
	}
	if conn.loop.inLoop() && conn.queuedWrites.Load() == 0 {
		_, err := conn.flushOnLoop()
		if err == nil {
			err = conn.updateInterest()
		}
		return err
	}
	// Outside the loop, Flush is a FIFO barrier and returns after submission.
	t := acquireTask(flushTask, conn)
	conn.submitMu.Lock()
	if conn.closing.Load() || !conn.loop.pushTask(t) {
		conn.submitMu.Unlock()
		releaseTask(t)
		return net.ErrClosed
	}
	conn.submitMu.Unlock()
	conn.loop.notify()
	return nil
}

func (conn *fdConn) runFlushTask() error {
	if conn.closed {
		return net.ErrClosed
	}
	_, err := conn.flushOnLoop()
	if err == nil {
		err = conn.updateInterest()
	}
	return err
}

func (conn *fdConn) flushOnLoop() (int, error) {
	if conn.isUDP || conn.outbound.Empty() {
		return 0, nil
	}
	if conn.writeBlocked {
		// Once EAGAIN is observed, only a Writable event should retry the fd.
		return 0, nil
	}
	var vecStorage [8][]byte
	totalWritten := 0
	for calls := 0; calls < 16 && totalWritten < 1<<20 && !conn.outbound.Empty(); calls++ {
		vec, _ := conn.outbound.PeekVecN(vecStorage[:0], len(vecStorage))
		written, err := socket.Writev(conn.fd, vec)
		if err != nil {
			if isWouldBlock(err) {
				conn.writeBlocked = true
				return totalWritten, nil
			}
			return totalWritten, err
		}
		if written == 0 {
			conn.writeBlocked = true
			return totalWritten, nil
		}
		conn.outbound.Discard(written)
		conn.pending.Add(-int64(written))
		totalWritten += written
		conn.events.onSocketBytesWrite(conn, written)
	}
	if conn.outbound.Empty() {
		conn.writeBlocked = false
	}
	return totalWritten, nil
}

func (conn *fdConn) updateInterest() error {
	if conn.closed || conn.udpSvr != nil {
		return nil
	}
	want := conn.desiredInterest()
	if want == conn.interest {
		return nil
	}
	if err := conn.loop.poller.Watch(conn.fd, want); err != nil {
		return err
	}
	conn.interest = want
	return nil
}

func (conn *fdConn) desiredInterest() poller.Interest {
	if conn.isUDP {
		return poller.Readable
	}
	if limit := int64(conn.events.MaxOutboundBuffered); limit > 0 {
		// Hysteresis avoids toggling Readable around a single threshold.
		pending := conn.pending.Load()
		if !conn.throttled && pending >= limit-limit/4 {
			conn.throttled = true
		}
		if conn.throttled && pending <= limit/2 {
			conn.throttled = false
		}
	} else {
		conn.throttled = false
	}
	var want poller.Interest
	if !conn.throttled {
		want |= poller.Readable
	}
	if !conn.outbound.Empty() {
		want |= poller.Writable
	}
	if want == 0 {
		// pending may still live in queued tasks; suppress reads until consumed.
		want = poller.Writable
	}
	return want
}

func (conn *fdConn) OutboundBuffered() int { return int(conn.pending.Load()) }
