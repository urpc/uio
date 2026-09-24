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

// nativeWriteVecLimit bounds stack use while allowing a read event's small
// owned writes to drain in one syscall.
const nativeWriteVecLimit = 64

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
	if conn.directOwner() {
		return conn.writeOnLoop(data)
	}
	if conn.isDatagram() {
		return conn.queueUDPWrite(bytebuf.CloneBuffer(data), len(data))
	}
	// This fast rejection belongs after the direct path: data sent straight to
	// the kernel never counts against the user-space payload limit.
	if limit := conn.events.MaxOutboundBuffered; limit > 0 && len(data) > limit {
		return 0, ErrOutboundOverflow
	}
	if err := conn.precheckOutbound(len(data)); err != nil {
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
	if conn.isDatagram() {
		return 0, errUnsupported
	}
	if conn.directOwner() {
		return conn.writevOnLoop(vec, total)
	}
	if limit := conn.events.MaxOutboundBuffered; limit > 0 && total > limit {
		return 0, ErrOutboundOverflow
	}
	if err := conn.precheckOutbound(total); err != nil {
		return 0, err
	}
	owned := bytebuf.CloneBuffers(vec, total)
	return conn.queueOwnedWrite(owned, total)
}

func (conn *fdConn) WriteOwned(owned *Buffer) (int, error) {
	if owned == nil {
		return 0, nil
	}
	size := owned.Len()
	if size == 0 {
		bytebuf.ReleaseBuffer(owned)
		return 0, nil
	}
	if conn.isClosing() {
		bytebuf.ReleaseBuffer(owned)
		return 0, net.ErrClosed
	}
	if conn.directOwner() {
		if conn.isDatagram() {
			defer bytebuf.ReleaseBuffer(owned)
			return conn.sendUDPOnLoop(owned.Bytes())
		}
		return conn.writeOwnedOnLoop(owned, size)
	}
	if conn.isDatagram() {
		return conn.queueUDPWrite(owned, size)
	}
	return conn.queueOwnedWrite(owned, size)
}

// queueUDPWrite transfers one datagram to the owning loop and waits for the
// nonblocking send result. The admission lock also orders it before a later
// CloseWith. A callback on another loop cannot wait without risking a cycle.
func (conn *fdConn) queueUDPWrite(owned *Buffer, size int) (int, error) {
	if isEventLoopGoroutine() {
		bytebuf.ReleaseBuffer(owned)
		return 0, ErrUDPWriteOnEventLoop
	}
	t := acquireTask(udpWriteTask, conn)
	t.udpPayload = owned
	t.udpDone = make(chan udpWriteResult, 1)
	done := t.udpDone
	conn.submitMu.Lock()
	if conn.loop == nil || conn.events == nil || conn.isClosing() || conn.events.closing.Load() || conn.loop.stopping.Load() {
		conn.submitMu.Unlock()
		bytebuf.ReleaseBuffer(owned)
		releaseTask(t)
		return 0, net.ErrClosed
	}
	if !conn.reservePending(int64(size)) {
		conn.submitMu.Unlock()
		bytebuf.ReleaseBuffer(owned)
		releaseTask(t)
		return 0, ErrOutboundOverflow
	}
	if !conn.loop.pushTask(t) {
		conn.pending.Add(-int64(size))
		conn.submitMu.Unlock()
		bytebuf.ReleaseBuffer(owned)
		releaseTask(t)
		return 0, net.ErrClosed
	}
	conn.submitMu.Unlock()
	conn.loop.notify()
	result := <-done
	return result.n, result.err
}

func (conn *fdConn) settleUDPWrite(size int64) {
	for {
		pending := conn.pending.Load()
		if pending == 0 {
			return // closeOnLoop already cleared the connection's accounting.
		}
		remaining := pending - size
		if remaining < 0 {
			remaining = 0
		}
		if conn.pending.CompareAndSwap(pending, remaining) {
			return
		}
	}
}

func (conn *fdConn) runUDPWriteTask(owned *Buffer) udpWriteResult {
	defer bytebuf.ReleaseBuffer(owned)
	defer conn.settleUDPWrite(int64(owned.Len()))
	if conn.isClosedOnLoop() || (conn.udp.server != nil && conn.udp.server.isClosedOnLoop()) {
		return udpWriteResult{err: net.ErrClosed}
	}
	n, err := conn.sendUDPOnLoop(owned.Bytes())
	return udpWriteResult{n: n, err: err}
}

func (conn *fdConn) precheckOutbound(size int) error {
	if limit := int64(conn.events.MaxOutboundBuffered); limit > 0 {
		pending := conn.pending.Load()
		if int64(size) > limit-pending {
			return ErrOutboundOverflow
		}
	}
	return nil
}

// queueOwnedWrite is the cross-goroutine write path. Ownership has already
// moved into owned, so submitMu only covers admission, accounting, and pointer
// insertion; payload allocation and copying never happen under the lock.
func (conn *fdConn) queueOwnedWrite(owned *bytebuf.Buffer, size int) (int, error) {
	if limit := conn.events.MaxOutboundBuffered; limit > 0 && size > limit {
		bytebuf.ReleaseBuffer(owned)
		return 0, ErrOutboundOverflow
	}
	// Allocation and the only payload copy have already happened off-lock.
	conn.submitMu.Lock()
	if conn.loop == nil || conn.isClosing() || conn.events.closing.Load() || conn.loop.stopping.Load() {
		conn.submitMu.Unlock()
		bytebuf.ReleaseBuffer(owned)
		return 0, net.ErrClosed
	}
	if !conn.reservePending(int64(size)) {
		conn.submitMu.Unlock()
		bytebuf.ReleaseBuffer(owned)
		return 0, ErrOutboundOverflow
	}
	conn.outbound.AppendOwned(owned)
	conn.submitMu.Unlock()
	conn.scheduleIO(ioEventWrite)
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

func (conn *fdConn) reservePendingAfterFlush(size int64) (bool, error) {
	if conn.reservePending(size) {
		return true, nil
	}
	if conn.outboundEmpty() || conn.writeBlocked() {
		return false, nil
	}
	if _, err := conn.flushOnLoop(); err != nil {
		return false, err
	}
	return conn.reservePending(size), nil
}

// writeOnLoop is the task-owner fast path. It lends caller memory directly to
// the non-blocking syscall when no batching is active and copies only an unsent
// suffix that must outlive the call.
func (conn *fdConn) writeOnLoop(data []byte) (int, error) {
	if conn.isDatagram() {
		return conn.sendUDPOnLoop(data)
	}
	threshold := conn.events.WriteBufferedThreshold
	if !conn.outboundEmpty() || conn.corked || (threshold > 0 && len(data) < threshold) {
		// Batching or an existing tail requires one copy into connection-owned storage.
		reserved, err := conn.reservePendingAfterFlush(int64(len(data)))
		if err != nil {
			return 0, err
		}
		if !reserved {
			return 0, ErrOutboundOverflow
		}
		conn.submitMu.Lock()
		_, _ = conn.outbound.Write(data)
		conn.submitMu.Unlock()
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
			conn.failDirectWrite(err)
			return written, err
		}
	}
	conn.events.onSocketBytesWrite(conn, written)
	if written == len(data) {
		return written, nil
	}
	remaining := data[written:]
	if !conn.reservePending(int64(len(remaining))) {
		if written > 0 {
			conn.failDirectWrite(ErrOutboundOverflow)
		}
		return written, ErrOutboundOverflow
	}
	// Only the unsent suffix must survive after Write returns.
	conn.submitMu.Lock()
	_, _ = conn.outbound.Write(remaining)
	conn.submitMu.Unlock()
	conn.setWriteBlocked(written == 0)
	return len(data), nil
}

// writevOnLoop mirrors writeOnLoop for scatter/gather input. A partial syscall
// is collapsed into one owned suffix so later retries do not retain caller
// slices or an unbounded vector list.
func (conn *fdConn) writevOnLoop(vec [][]byte, total int) (int, error) {
	if conn.isDatagram() {
		return 0, errUnsupported
	}
	threshold := conn.events.WriteBufferedThreshold
	if !conn.outboundEmpty() || conn.corked || (threshold > 0 && total < threshold) {
		reserved, err := conn.reservePendingAfterFlush(int64(total))
		if err != nil {
			return 0, err
		}
		if !reserved {
			return 0, ErrOutboundOverflow
		}
		conn.submitMu.Lock()
		_, _ = conn.outbound.Writev(vec)
		conn.submitMu.Unlock()
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
			conn.failDirectWrite(err)
			return written, err
		}
	}
	conn.events.onSocketBytesWrite(conn, written)
	if written == total {
		return written, nil
	}
	remaining := total - written
	if !conn.reservePending(int64(remaining)) {
		if written > 0 {
			conn.failDirectWrite(ErrOutboundOverflow)
		}
		return written, ErrOutboundOverflow
	}
	owned := bytebuf.CloneBuffersFrom(vec, written, remaining)
	conn.submitMu.Lock()
	conn.outbound.AppendOwned(owned)
	conn.submitMu.Unlock()
	conn.setWriteBlocked(written == 0)
	return total, nil
}

// writeOwnedOnLoop consumes owned on every return path. During a corked read
// round, the first small frame keeps zero-copy ownership and later frames are
// coalesced into pooled blocks to keep the final writev batch short.
func (conn *fdConn) writeOwnedOnLoop(owned *bytebuf.Buffer, size int) (int, error) {
	threshold := conn.events.WriteBufferedThreshold
	if !conn.outboundEmpty() || conn.corked || (threshold > 0 && size < threshold) {
		reserved, err := conn.reservePendingAfterFlush(int64(size))
		if err != nil {
			bytebuf.ReleaseBuffer(owned)
			return 0, err
		}
		if !reserved {
			bytebuf.ReleaseBuffer(owned)
			return 0, ErrOutboundOverflow
		}
		conn.submitMu.Lock()
		if conn.corked && !conn.outbound.Empty() && size < conn.events.readBufferSize {
			targetCapacity := min(64<<10, conn.events.readBufferSize*2)
			conn.outbound.AppendOwnedCoalesced(owned, targetCapacity)
		} else {
			conn.outbound.AppendOwned(owned)
		}
		conn.submitMu.Unlock()
		return size, nil
	}

	written, err := syscall.Write(conn.fd, owned.Bytes())
	if written < 0 {
		written = 0
	}
	if err != nil {
		if isWouldBlock(err) {
			written, err = 0, nil
		} else {
			bytebuf.ReleaseBuffer(owned)
			conn.failDirectWrite(err)
			return written, err
		}
	}
	conn.events.onSocketBytesWrite(conn, written)
	if written == size {
		bytebuf.ReleaseBuffer(owned)
		return written, nil
	}
	remaining := size - written
	if !conn.reservePending(int64(remaining)) {
		bytebuf.ReleaseBuffer(owned)
		if written > 0 {
			conn.failDirectWrite(ErrOutboundOverflow)
		}
		return written, ErrOutboundOverflow
	}
	if written > 0 {
		owned.Discard(written)
	}
	conn.submitMu.Lock()
	conn.outbound.AppendOwned(owned)
	conn.submitMu.Unlock()
	conn.setWriteBlocked(written == 0)
	return size, nil
}

// sendUDPOnLoop preserves datagram atomicity. A blocked datagram is reported to
// the caller rather than queued as a stream suffix, and a partial result is
// fatal because retrying it would create a different packet.
func (conn *fdConn) sendUDPOnLoop(data []byte) (written int, err error) {
	if conn.udp.remote == nil {
		written, err = syscall.Write(conn.fd, data)
	} else {
		err = syscall.Sendto(conn.fd, data, 0, conn.udp.remote)
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

func (conn *fdConn) failDirectWrite(err error) {
	// Once a syscall has made the byte stream unusable, queued writes must not
	// be appended ahead of the close task or flushed during close.
	conn.markWriteFailed()
	conn.requestClose(err)
}

func (conn *fdConn) Flush() error {
	if conn.isClosing() {
		return net.ErrClosed
	}
	if conn.loop == nil || conn.loop.stopping.Load() {
		return net.ErrClosed
	}
	if conn.isDatagram() {
		return nil
	}
	if conn.directOwner() {
		_, err := conn.flushOnLoop()
		return err
	}
	// Outside the task, Flush is a FIFO barrier and returns after scheduling
	// the connection's next I/O round.
	conn.scheduleIO(ioEventWrite)
	return nil
}

// flushOnLoop drains a bounded number of bytes and writev calls while holding
// submitMu, which prevents external producers from modifying the vector list.
// EAGAIN leaves the queue intact and sets writeBlocked until a writable edge.
func (conn *fdConn) flushOnLoop() (int, error) {
	conn.submitMu.Lock()
	if conn.isDatagram() || conn.outbound.Empty() {
		conn.submitMu.Unlock()
		return 0, nil
	}
	if conn.writeBlocked() {
		// Once EAGAIN is observed, only a Writable event should retry the fd.
		conn.submitMu.Unlock()
		return 0, nil
	}
	var vecStorage [nativeWriteVecLimit][]byte
	totalWritten := 0
	var writeErr error
	for calls := 0; calls < 16 && totalWritten < 1<<20 && !conn.outbound.Empty(); calls++ {
		vec, _ := conn.outbound.PeekVecN(vecStorage[:0], len(vecStorage))
		written, err := socket.Writev(conn.fd, vec)
		if err != nil {
			if isWouldBlock(err) {
				conn.setWriteBlocked(true)
				break
			}
			writeErr = err
			break
		}
		if written == 0 {
			conn.setWriteBlocked(true)
			break
		}
		conn.outbound.Discard(written)
		conn.pending.Add(-int64(written))
		totalWritten += written
	}
	if conn.outbound.Empty() {
		conn.setWriteBlocked(false)
	}
	conn.submitMu.Unlock()
	if totalWritten > 0 {
		conn.events.onSocketBytesWrite(conn, totalWritten)
	}
	if writeErr != nil {
		return totalWritten, writeErr
	}
	return totalWritten, nil
}

func (conn *fdConn) outboundEmpty() bool {
	conn.submitMu.Lock()
	empty := conn.outbound.Empty()
	conn.submitMu.Unlock()
	return empty
}

func (conn *fdConn) updateInterest() error {
	if conn.close.isReleased() || (conn.udp != nil && conn.udp.server != nil) {
		return nil
	}
	want := conn.desiredInterest()
	if want == conn.interest {
		return nil
	}
	if err := conn.loop.poller.Modify(conn.fd, conn.interest, want); err != nil {
		return err
	}
	conn.interest = want
	return nil
}

// desiredInterest applies per-connection read hysteresis and arms writable
// only while user-space output remains. It runs exclusively on the event loop.
func (conn *fdConn) desiredInterest() poller.Interest {
	if conn.isDatagram() {
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
	if !conn.outboundEmpty() {
		want |= poller.Writable
	}
	if want == 0 {
		// pending may still live in queued tasks; suppress reads until consumed.
		want = poller.Writable
	}
	return want
}

// readShouldStop reports whether this connection filled its outbound limit.
// The interest side applies the same limit through hysteresis, so the read
// round and poller registration cannot disagree.
func (conn *fdConn) readShouldStop() bool {
	if limit := int64(conn.events.MaxOutboundBuffered); limit > 0 {
		return conn.pending.Load() >= limit
	}
	return false
}

func (conn *fdConn) OutboundBuffered() int { return int(conn.pending.Load()) }
